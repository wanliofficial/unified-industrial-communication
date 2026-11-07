// Package fins 实现欧姆龙 FINS 协议（TCP 承载）的主站驱动。
//
// 分层与 Modbus/S7/MC 相同：Meta（能力声明）→ conn（事务/错误分类）→
// codec/fins（纯函数编解码）→ transport（TCP 字节流）。
//
// 两个刻意的边界：
//  1. 只做 FINS/TCP（端口 9600）。FINS/UDP、Host Link、串口 FINS 不在
//     范围内；配置期就把其它 transport 挡掉，避免运行期降级。
//  2. 只做内存区读/写（0101/0102）。PLC 运行模式切换、时钟、强制置位
//     等写操作不放进采集驱动——它们不是「读点位」的一部分，误用代价远
//     高于收益。
//
// 地址约定：`offset` 是**字地址**，`bit` 是字内位号（0..15）。位区上的
// bool 点位必须给 `bit`；只有字访问码、没有位访问码的区域（当前 EM 区）
// 上的 bool 走「读字 → 改位 → 写字」。
package fins

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	finscodec "uic/internal/codec/fins"
	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 FINS/TCP 主站驱动。
type Driver struct {
	dial Dialer
}

// New 返回使用真实 TCP 的驱动。
func New() *Driver {
	return &Driver{dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
		return transport.DialTCP(ctx, addr, timeout)
	}}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver { return &Driver{dial: d} }

// Meta 声明 FINS 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "fins",
		Name:         "Omron FINS/TCP",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"tcp"},
		AddressKinds: []model.AddressKind{model.AddressRegister},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapSupported,
			BitAccess:   driver.CapSupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "地址", Type: "string", Required: true,
				Placeholder: "192.168.1.10:9600", Scope: "channel",
				Help: "只填主机名时默认端口 9600"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "FINS/TCP 是单连接一问一答；建议 1"},
			{Name: "device.unitId", Label: "目标节点号", Type: "int", Default: 0, Scope: "device",
				Help: "0 表示使用节点号协商返回的 PLC 节点"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "内存区", Type: "enum", Required: true,
				Enum: finscodec.Areas(), Scope: "point",
				Help: "cio/wr/hr/ar/dm/em/em0..em12；D/W/H/A 是别名"},
			{Name: "address.offset", Label: "字地址", Type: "int", Required: true, Scope: "point",
				Help: "从 0 开始；CIO0.3 的 offset 是 0、bit 是 3"},
			{Name: "address.length", Label: "字数", Type: "int", Scope: "point",
				Help: "多字类型按数据类型推导；字符串/字节用此宽度"},
			{Name: "address.bit", Label: "位号", Type: "int", Default: -1, Scope: "point",
				Help: "bool 点位必填：0..15（0 = LSB）"},
		},
		Description: "欧姆龙 FINS/TCP（端口 9600），支持 CIO/WR/HR/AR/DM/EM 内存区的字/位读写",
	}
}

// Open 建立 TCP 链路并完成节点号协商。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("fins requires tcp transport, got %q", t.Kind), false, nil)
	}
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host[:port])", false, nil)
	}
	if !strings.Contains(addr, ":") {
		addr += ":" + finscodec.DefaultPort
	}
	stream, err := d.dial(ctx, addr, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", addr), true, err)
	}
	f := &framing{stream: stream}
	if err := f.handshake(ctx, cfg.Device.UnitID); err != nil {
		_ = stream.Close()
		kind := driver.ErrProtocol
		retry := false
		var te *finscodec.TCPError
		if errors.As(err, &te) {
			kind = driver.ErrDisconnected
			retry = true
		} else if driver.KindOf(err) != "" {
			kind = driver.KindOf(err)
			retry = true
		}
		return nil, driver.E(kind, "handshake", cfg.Device.ID, "FINS node negotiation failed", retry, err)
	}
	return &conn{cfg: cfg, tr: f}, nil
}

type conn struct {
	cfg driver.OpenConfig
	tr  *framing
	// wmu 保护「读字 → 改位 → 写字」这类复合写入。按契约同一 Conn 不会
	// 被并发调用；加锁是为了在错误使用下宁可慢一点，也不要静默丢位。
	wmu sync.Mutex
}

func (c *conn) Probe(ctx context.Context) error {
	_, err := c.readWords(ctx, "dm", 0, 1)
	return err
}

func (c *conn) Close() error { return c.tr.Close() }

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	var out []driver.RawSample
	var firstErr error
	ok := 0
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return out, driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
		}
		samples, err := c.readSpan(ctx, span)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			out = append(out, failed(span, err)...)
			continue
		}
		ok++
		out = append(out, samples...)
	}
	if ok == 0 && firstErr != nil {
		return out, firstErr
	}
	return out, nil
}

// readSpan 处理一个 span。与 MC 不同，FINS 的同一个内存区可以同时有字
// 点位和位点位（例如 DM100 字 + DM100.3 位），所以这里按访问方式拆成
// 两次事务，再按原始顺序合并结果。
func (c *conn) readSpan(ctx context.Context, span driver.ReadSpan) ([]driver.RawSample, error) {
	if len(span.Points) == 0 {
		return nil, nil
	}
	area, ok := finscodec.Lookup(span.Points[0].Address.Area)
	if !ok {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("unknown FINS area %q", span.Points[0].Address.Area), false, nil)
	}
	for _, p := range span.Points {
		a, ok := finscodec.Lookup(p.Address.Area)
		if !ok || a.Key != area.Key {
			return nil, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID,
				"span mixes FINS memory areas", false, nil)
		}
	}

	out := make([]driver.RawSample, len(span.Points))
	wordIdx := make([]int, 0, len(span.Points))
	bitIdx := make([]int, 0, len(span.Points))
	bitAccess, bitErr := finscodec.BitAccess(area)

	for i, p := range span.Points {
		if p.DataType == model.TypeBool && bitErr == nil {
			if p.Address.Bit < 0 || p.Address.Bit > 15 {
				out[i] = errSample(p.ID, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
					fmt.Sprintf("bool point on %s needs address.bit 0..15", area.Key), false, nil))
				continue
			}
			bitIdx = append(bitIdx, i)
			continue
		}
		wordIdx = append(wordIdx, i)
	}

	if len(wordIdx) > 0 {
		if err := c.readWordPoints(ctx, span, area, wordIdx, out); err != nil {
			return nil, err
		}
	}
	if len(bitIdx) > 0 {
		if err := c.readBitPoints(ctx, span, area, bitAccess, bitIdx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *conn) readWordPoints(ctx context.Context, span driver.ReadSpan, area finscodec.Area, idx []int, out []driver.RawSample) error {
	min, max := wordBounds(span, idx)
	for start := min; start < max; {
		end := start + 480
		if end > max {
			end = max
		}
		data, err := c.readWords(ctx, area.Key, start, end-start)
		if err != nil {
			return err
		}
		for _, i := range idx {
			p := span.Points[i]
			off := p.Address.Offset
			if off < start || off >= end {
				continue
			}
			out[i] = decodeWordPoint(p, area, start, data)
		}
		start = end
	}
	return nil
}

func (c *conn) readBitPoints(ctx context.Context, span driver.ReadSpan, area finscodec.Area, acc finscodec.Access, idx []int, out []driver.RawSample) error {
	min, max := bitBounds(span, idx)
	// 一次读整字范围：请求的位从 min 字的 bit0 开始，数量是字数*16。
	// 这样不需要为每个点单独算起始位，也保证同一字上的多个点位共享
	// 一次事务。
	const wordsPerRead = 120 // 1920 位，低于 1998 上限
	for start := min; start < max; {
		end := start + wordsPerRead
		if end > max {
			end = max
		}
		count := (end - start) * 16
		data, err := c.readBits(ctx, acc, start, 0, count)
		if err != nil {
			return err
		}
		for _, i := range idx {
			p := span.Points[i]
			off := p.Address.Offset
			if off < start || off >= end {
				continue
			}
			bit := p.Address.Bit
			pos := (off-start)*16 + bit
			if pos < 0 || pos >= len(data) {
				out[i] = errSample(p.ID, driver.E(driver.ErrProtocol, "decode", c.cfg.Device.ID,
					"response shorter than requested bits", false, nil))
				continue
			}
			out[i] = driver.RawSample{
				PointID: p.ID,
				Value:   model.BoolValue(data[pos] != 0),
				Quality: model.QualityGood,
			}
		}
		start = end
	}
	return nil
}

func wordBounds(span driver.ReadSpan, idx []int) (int, int) {
	min, max := span.Points[idx[0]].Address.Offset, span.Points[idx[0]].Address.Offset
	for _, i := range idx {
		p := span.Points[i]
		w := p.Length
		if w <= 0 {
			w = registerWidth(p.DataType)
		}
		if p.DataType == model.TypeBool {
			w = 1
		}
		if p.Address.Offset < min {
			min = p.Address.Offset
		}
		if e := p.Address.Offset + w; e > max {
			max = e
		}
	}
	return min, max
}

func bitBounds(span driver.ReadSpan, idx []int) (int, int) {
	min, max := span.Points[idx[0]].Address.Offset, span.Points[idx[0]].Address.Offset
	for _, i := range idx {
		off := span.Points[i].Address.Offset
		if off < min {
			min = off
		}
		if off+1 > max {
			max = off + 1
		}
	}
	return min, max
}

func decodeWordPoint(p driver.PointRef, area finscodec.Area, start int, data []byte) driver.RawSample {
	idx := p.Address.Offset - start
	width := p.Length
	if p.DataType == model.TypeBool {
		width = 1
	}
	if width <= 0 {
		width = registerWidth(p.DataType)
	}
	byteOff := idx * 2
	if byteOff+width*2 > len(data) {
		return errSample(p.ID, driver.E(driver.ErrProtocol, "decode", "",
			"response shorter than requested words", false, nil))
	}
	raw := data[byteOff : byteOff+width*2]

	if p.DataType == model.TypeBool {
		// 走到这里说明该区没有直接的位访问码（当前 EM 区），只能从
		// 整字里取位。
		if p.Address.Bit < 0 || p.Address.Bit > 15 {
			return errSample(p.ID, driver.E(driver.ErrAddress, "decode", "",
				fmt.Sprintf("bool point on %s needs address.bit 0..15", area.Key), false, nil))
		}
		word := binary.BigEndian.Uint16(raw)
		return driver.RawSample{
			PointID: p.ID,
			Value:   model.BoolValue(word&(1<<uint(p.Address.Bit)) != 0),
			Quality: model.QualityGood,
		}
	}

	v, err := value.Decode(raw, p.DataType, orderFor(width*2, p.ByteOrder))
	if err != nil {
		return errSample(p.ID, driver.E(driver.ErrProtocol, "decode", p.ID, err.Error(), false, err))
	}
	return driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood}
}

// orderFor 返回多字数值的字节序。FINS 的原生布局是「每个字大端、低字在
// 前」：32 位值对应 CDAB，64 位值对应 DCBA（整体倒序）。用户显式配置时
// 以配置为准。
func orderFor(n int, order model.ByteOrder) model.ByteOrder {
	if order != "" {
		return order
	}
	if n >= 8 {
		return model.OrderDCBA
	}
	return model.OrderCDAB
}

func registerWidth(dt model.DataType) int {
	if n := dt.Registers(); n > 0 {
		return n
	}
	return 1
}

func (c *conn) readWords(ctx context.Context, areaKey string, word, count int) ([]byte, error) {
	area, ok := finscodec.Lookup(areaKey)
	if !ok {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("unknown FINS area %q", areaKey), false, nil)
	}
	frame, err := finscodec.BuildMemoryRead(c.header(), finscodec.WordAccess(area),
		uint16(word), 0, uint16(count))
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.tr.RoundTrip(ctx, frame)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) < count*2 {
		return nil, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("short read response: %d bytes for %d words", len(resp.Data), count), false, nil)
	}
	return resp.Data[:count*2], nil
}

func (c *conn) readBits(ctx context.Context, acc finscodec.Access, word int, bit byte, count int) ([]byte, error) {
	frame, err := finscodec.BuildMemoryRead(c.header(), acc, uint16(word), bit, uint16(count))
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.tr.RoundTrip(ctx, frame)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) < count {
		return nil, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("short bit response: %d bytes for %d bits", len(resp.Data), count), false, nil)
	}
	return resp.Data[:count], nil
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		for i := range results {
			if results[i].Err == nil {
				results[i].Err = firstErr
			}
		}
	}
	return results, firstErr
}

func (c *conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	area, ok := finscodec.Lookup(req.Address.Area)
	if !ok {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("unknown FINS area %q", req.Address.Area), false, nil)
		return writeFail(req, err), err
	}
	if req.DataType == model.TypeBool {
		if req.Address.Bit < 0 || req.Address.Bit > 15 {
			err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
				fmt.Sprintf("bool point on %s needs address.bit 0..15", area.Key), false, nil)
			return writeFail(req, err), err
		}
		if acc, err := finscodec.BitAccess(area); err == nil {
			return c.writeBit(ctx, req, acc)
		}
		return c.writeWordBit(ctx, req, area)
	}
	return c.writeWords(ctx, req, area)
}

func (c *conn) writeBit(ctx context.Context, req driver.WriteRequest, acc finscodec.Access) (driver.WriteResult, error) {
	data := []byte{0}
	if req.Value.Bool {
		data[0] = 1
	}
	return c.write(ctx, req, acc, req.Address.Offset, byte(req.Address.Bit), 1, data)
}

func (c *conn) writeWordBit(ctx context.Context, req driver.WriteRequest, area finscodec.Area) (driver.WriteResult, error) {
	// 读整字 → 只改目标位 → 写整字：邻位属于别的点位，不能被清零。
	data, err := c.readWords(ctx, area.Key, req.Address.Offset, 1)
	if err != nil {
		return writeFail(req, err), err
	}
	if len(data) < 2 {
		err := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, "short word read", false, nil)
		return writeFail(req, err), err
	}
	word := binary.BigEndian.Uint16(data)
	if req.Value.Bool {
		word |= 1 << uint(req.Address.Bit)
	} else {
		word &^= 1 << uint(req.Address.Bit)
	}
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, word)
	return c.write(ctx, req, finscodec.WordAccess(area), req.Address.Offset, 0, 1, payload)
}

func (c *conn) writeWords(ctx context.Context, req driver.WriteRequest, area finscodec.Area) (driver.WriteResult, error) {
	words := reqWords(req)
	if words <= 0 {
		err := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q has no width", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	var payload []byte
	if req.DataType == model.TypeString || req.DataType == model.TypeBytes {
		// 字符串/字节：按地址宽度补零或截断。FINS 每个字存两个字符，
		// 字内是大端（"AB" → 0x4142），不需要额外交换。
		raw := make([]byte, words*2)
		copy(raw, []byte(req.Value.Text))
		payload = raw
	} else {
		raw, err := value.Encode(req.Value, req.DataType, orderFor(words*2, req.ByteOrder))
		if err != nil {
			err = driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
			return writeFail(req, err), err
		}
		if len(raw) > words*2 {
			err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
				fmt.Sprintf("point %q needs %d bytes but address has %d", req.PointID, len(raw), words*2), false, nil)
			return writeFail(req, err), err
		}
		payload = append(raw, make([]byte, words*2-len(raw))...)
	}
	return c.write(ctx, req, finscodec.WordAccess(area), req.Address.Offset, 0, words, payload)
}

func (c *conn) write(ctx context.Context, req driver.WriteRequest, acc finscodec.Access, word int, bit byte, count int, data []byte) (driver.WriteResult, error) {
	frame, err := finscodec.BuildMemoryWrite(c.header(), acc, uint16(word), bit, uint16(count), data)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	if _, err := c.tr.RoundTrip(ctx, frame); err != nil {
		return writeFail(req, err), err
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// reqWords 计算一次写入占的字数。写请求不带点位定义，长度只能从地址与
// 数据类型自身推断：显式给了 address.length 就用它，否则按类型宽度。
func reqWords(req driver.WriteRequest) int {
	if req.Address.Length > 0 {
		return req.Address.Length
	}
	if n := req.DataType.Registers(); n > 0 {
		return n
	}
	return 1
}

func (c *conn) header() finscodec.Header {
	return finscodec.CommandHeader(c.tr.destNode, c.tr.clientNode, 0)
}

// ---------------------------------------------------------------------------
// 错误与工具
// ---------------------------------------------------------------------------

func failed(span driver.ReadSpan, err error) []driver.RawSample {
	q := qualityOf(err)
	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		out = append(out, driver.RawSample{PointID: p.ID, Quality: q, Err: err})
	}
	return out
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityOf(err), Err: err}
}

func qualityOf(err error) model.Quality {
	var ec *finscodec.EndCodeError
	if errors.As(err, &ec) {
		if ec.Address() {
			return model.QualityAddressError
		}
		if ec.Link() {
			return model.QualityCommError
		}
		return model.QualityBad
	}
	var te *finscodec.TCPError
	if errors.As(err, &te) {
		return model.QualityCommError
	}
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress, driver.ErrUnsupported:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}
}

// ---------------------------------------------------------------------------
// TCP 分帧与节点号协商
// ---------------------------------------------------------------------------

type framing struct {
	stream transport.Stream

	mu         sync.Mutex
	sid        byte
	clientNode byte
	serverNode byte
	destNode   byte
}

// handshake 完成 FINS/TCP 节点号协商（命令 0 → 1）。
func (f *framing) handshake(ctx context.Context, destOverride int) error {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := f.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := f.stream.Write(finscodec.BuildNodeRequest(0)); err != nil {
		return classifyNetErr("handshake-write", err)
	}
	head, err := f.stream.ReadFull(16)
	if err != nil {
		return classifyNetErr("handshake-read", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		return driver.E(driver.ErrProtocol, "handshake", "", err.Error(), false, err)
	}
	if th.Command != finscodec.TCPCommandNodeResponse {
		return driver.E(driver.ErrProtocol, "handshake", "",
			fmt.Sprintf("unexpected FINS/TCP command %d", th.Command), false, nil)
	}
	if th.Error != 0 {
		return &finscodec.TCPError{Code: th.Error}
	}
	payloadLen := th.Length - 8
	if payloadLen < 8 {
		return driver.E(driver.ErrProtocol, "handshake", "",
			fmt.Sprintf("node response payload too short: %d", payloadLen), false, nil)
	}
	payload, err := f.stream.ReadFull(payloadLen)
	if err != nil {
		return classifyNetErr("handshake-payload", err)
	}
	f.clientNode = payload[3]
	f.serverNode = payload[7]
	if f.clientNode == 0 && f.serverNode == 0 {
		return driver.E(driver.ErrProtocol, "handshake", "", "PLC returned no node addresses", false, nil)
	}
	if destOverride > 0 && destOverride <= 254 {
		f.destNode = byte(destOverride)
	} else {
		f.destNode = f.serverNode
	}
	return nil
}

// RoundTrip 发送一个 FINS 帧并读回应答。SID 由这里统一分配并校验，调用
// 方构造帧时把第 10 字节留空即可。
func (f *framing) RoundTrip(ctx context.Context, req []byte) (finscodec.Frame, error) {
	if len(req) < 12 {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", "",
			"FINS frame too short", false, nil)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sid++
	sid := f.sid
	frame := append([]byte(nil), req...)
	frame[9] = sid

	pkt, err := finscodec.BuildTCPFrame(frame)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", "", err.Error(), false, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := f.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := f.stream.Write(pkt); err != nil {
		return finscodec.Frame{}, classifyNetErr("write", err)
	}
	head, err := f.stream.ReadFull(16)
	if err != nil {
		return finscodec.Frame{}, classifyNetErr("read-header", err)
	}
	th, err := finscodec.ParseTCPHeader(head)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "", err.Error(), false, err)
	}
	if th.Command != finscodec.TCPCommandFrame {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("unexpected FINS/TCP command %d", th.Command), false, nil)
	}
	if th.Error != 0 {
		// 封装层错误码大多是「连接数满 / 节点被占用 / 对端重启」这类
		// 瞬态问题，必须带上「可重试」的分类；否则调度器会当作内部错误
		// 直接熔断，把一次可以自愈的重连拖成需要人工介入。
		return finscodec.Frame{}, driver.E(driver.ErrDisconnected, "read", "",
			"FINS/TCP transport error", true, &finscodec.TCPError{Code: th.Error})
	}
	body, err := f.stream.ReadFull(th.Length - 8)
	if err != nil {
		return finscodec.Frame{}, classifyNetErr("read-body", err)
	}
	resp, err := finscodec.Parse(body)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "", err.Error(), false, err)
	}
	if !resp.IsResponse() {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "",
			"FINS frame is not a response", false, nil)
	}
	if resp.Header.SID != sid {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("SID mismatch: sent %d, got %d", sid, resp.Header.SID), false, nil)
	}
	if resp.EndCode != 0 {
		return resp, &finscodec.EndCodeError{Code: resp.EndCode}
	}
	return resp, nil
}

func (f *framing) Close() error { return f.stream.Close() }

func classifyNetErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, "", "i/o timeout", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, "", "connection lost", true, err)
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	if t, ok := err.(timeout); ok && t.Timeout() {
		return true
	}
	return false
}
