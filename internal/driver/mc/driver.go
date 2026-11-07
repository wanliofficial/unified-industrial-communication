// Package mc 实现三菱 MC 协议（3E 帧，二进制）主站驱动。
//
// 分层与 Modbus/S7 相同：Meta（能力声明）→ conn（事务/错误分类）→
// codec/mc（纯函数编解码）→ transport（TCP 字节流）。
//
// 两个刻意的边界：
//  1. 只做 3E 二进制帧。ASCII 帧、4E 帧、SLMP 的 UDP 形态都不在范围内；
//     配置期就把不支持的能力挡掉，比运行期降级更容易解释。
//  2. 位软元件按位读写；字软元件上的 bool 点位要求显式给 `address.bit`
//     （0..15），驱动做「读整字 → 改目标位 → 写整字」。没有位号的 bool
//     在字软元件上没有意义，直接判地址错，而不是猜一个位。
package mc

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"

	mccodec "uic/internal/codec/mc"
	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// Driver 是 MC 3E 主站驱动。
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

// Meta 声明 MC 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "mc",
		Name:         "Mitsubishi MC 3E (binary)",
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
				Placeholder: "192.168.1.10:5000", Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 2, Scope: "channel",
				Help: "多数 PLC 的 3E 帧并发能力有限；建议 1..2"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "软元件", Type: "enum", Required: true,
				Enum: mccodec.Areas(), Scope: "point",
				Help: "D/W/R/ZR/SD/SW/TN/CN 是字软元件；M/X/Y/B/F/L/SM/SB/S 是位软元件"},
			{Name: "address.offset", Label: "起始编号", Type: "int", Required: true, Scope: "point",
				Help: "线上数值的十进制表示：X1F 填 31，D100 填 100"},
			{Name: "address.length", Label: "长度", Type: "int", Scope: "point",
				Help: "字软元件是字数；位软元件固定 1"},
			{Name: "address.bit", Label: "位号", Type: "int", Default: -1, Scope: "point",
				Help: "仅字软元件上的 bool 点位：0..15（0 = LSB）"},
		},
		Description: "三菱 MC 3E 帧（二进制，TCP 5000），支持 D/W/R/ZR/M/X/Y/B 等常用软元件",
	}
}

// Open 建立 TCP 链路。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("mc-3e requires tcp transport, got %q", t.Kind), false, nil)
	}
	if t.Address == "" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.address required (host:port)", false, nil)
	}
	stream, err := d.dial(ctx, t.Address, cfg.Channel.EffectiveTimeout())
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID,
			fmt.Sprintf("connect %s failed", t.Address), true, err)
	}
	return &conn{cfg: cfg, tr: &framing{stream: stream}}, nil
}

type conn struct {
	cfg driver.OpenConfig
	tr  *framing
	// wmu 只保护「读整字 → 改位 → 写整字」这类复合写入，避免两次写入
	// 交错后互相覆盖。按契约同一 Conn 不会被并发调用，这里仍加锁：错误
	// 使用方式下宁可慢一点，也不要静默丢位。
	wmu sync.Mutex
}

func (c *conn) Probe(ctx context.Context) error {
	// 读一个 D0：只要是合法的 3E 从站就一定会应答。
	_, err := c.read(ctx, "d", 0, 1, false)
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

func (c *conn) readSpan(ctx context.Context, span driver.ReadSpan) ([]driver.RawSample, error) {
	if len(span.Points) == 0 {
		return nil, nil
	}
	area := strings.ToLower(span.Points[0].Address.Area)
	dev, ok := mccodec.Lookup(area)
	if !ok {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("unknown MC device %q", area), false, nil)
	}
	for _, p := range span.Points {
		if strings.ToLower(p.Address.Area) != area {
			return nil, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID,
				"span mixes MC device areas", false, nil)
		}
	}

	min, max := spanRange(span, dev.Bit)
	if max-min <= 0 {
		return nil, nil
	}
	limit := 960
	if dev.Bit {
		limit = 7168
	}
	if max-min > limit {
		// 规划层通常已经切成小段；这里兜底，避免一个手工构造的 span
		// 被 PLC 以「点数无效」拒绝。
		return c.readChunked(ctx, span, dev, min, max, limit)
	}
	data, err := c.read(ctx, area, min, max-min, dev.Bit)
	if err != nil {
		return nil, err
	}
	return decodeSpan(span, dev, min, data)
}

func (c *conn) readChunked(ctx context.Context, span driver.ReadSpan, dev mccodec.Device, min, max, limit int) ([]driver.RawSample, error) {
	var out []driver.RawSample
	for start := min; start < max; start += limit {
		end := start + limit
		if end > max {
			end = max
		}
		data, err := c.read(ctx, strings.ToLower(span.Points[0].Address.Area), start, end-start, dev.Bit)
		if err != nil {
			return out, err
		}
		part := driver.ReadSpan{ID: span.ID}
		for _, p := range span.Points {
			if p.Address.Offset >= start && p.Address.Offset < end {
				part.Points = append(part.Points, p)
			}
		}
		samples, err := decodeSpan(part, dev, start, data)
		if err != nil {
			return out, err
		}
		out = append(out, samples...)
	}
	return out, nil
}

func (c *conn) read(ctx context.Context, area string, head, points int, bit bool) ([]byte, error) {
	dev, ok := mccodec.Lookup(area)
	if !ok {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID,
			fmt.Sprintf("unknown MC device %q", area), false, nil)
	}
	req, err := mccodec.BuildRead(dev.Code, head, points, bit)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.tr.RoundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	return mccodec.ParseResponse(resp)
}

func spanRange(span driver.ReadSpan, bit bool) (int, int) {
	min := span.Points[0].Address.Offset
	max := min
	for _, p := range span.Points {
		width := 1
		if !bit {
			width = p.Length
			if width <= 0 {
				width = registerWidth(p.DataType)
			}
		}
		if p.Address.Offset < min {
			min = p.Address.Offset
		}
		if e := p.Address.Offset + width; e > max {
			max = e
		}
	}
	return min, max
}

func registerWidth(dt model.DataType) int {
	if n := dt.Registers(); n > 0 {
		return n
	}
	return 1
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

func decodeSpan(span driver.ReadSpan, dev mccodec.Device, min int, data []byte) ([]driver.RawSample, error) {
	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		idx := p.Address.Offset - min
		if dev.Bit {
			if p.DataType != model.TypeBool {
				out = append(out, errSample(p.ID, driver.E(driver.ErrAddress, "decode", "",
					fmt.Sprintf("bit device %s requires bool, got %s", p.Address.Area, p.DataType), false, nil)))
				continue
			}
			if idx/8 >= len(data) {
				out = append(out, errSample(p.ID, driver.E(driver.ErrProtocol, "decode", "",
					"response shorter than requested bits", false, nil)))
				continue
			}
			out = append(out, driver.RawSample{
				PointID: p.ID,
				Value:   model.BoolValue(data[idx/8]&(1<<uint(idx%8)) != 0),
				Quality: model.QualityGood,
			})
			continue
		}

		width := p.Length
		if width <= 0 {
			width = registerWidth(p.DataType)
		}
		byteOff := idx * 2
		if byteOff+width*2 > len(data) {
			out = append(out, errSample(p.ID, driver.E(driver.ErrProtocol, "decode", "",
				"response shorter than requested words", false, nil)))
			continue
		}
		raw := data[byteOff : byteOff+width*2]

		if p.DataType == model.TypeBool {
			if p.Address.Bit < 0 || p.Address.Bit > 15 {
				out = append(out, errSample(p.ID, driver.E(driver.ErrAddress, "decode", "",
					fmt.Sprintf("bool point on word device %s needs address.bit 0..15", p.Address.Area), false, nil)))
				continue
			}
			word := binary.LittleEndian.Uint16(raw)
			out = append(out, driver.RawSample{
				PointID: p.ID,
				Value:   model.BoolValue(word&(1<<uint(p.Address.Bit)) != 0),
				Quality: model.QualityGood,
			})
			continue
		}

		v, err := value.Decode(normalizeWords(raw), p.DataType, mcOrder(p.ByteOrder))
		if err != nil {
			out = append(out, errSample(p.ID, driver.E(driver.ErrProtocol, "decode", p.ID, err.Error(), false, err)))
			continue
		}
		out = append(out, driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood})
	}
	return out, nil
}

// mcOrder 返回该点位使用的字序。MC 的原生布局是「字内小端、低字在前」，
// 归一化字内字节后正好对应 CDAB；用户显式配置时以配置为准。
func mcOrder(order model.ByteOrder) model.ByteOrder {
	if order == "" {
		return model.OrderCDAB
	}
	return order
}

// normalizeWords 把每个 16 位字的两个字节对调，把 MC 的线上布局
// （字内小端）转成 value 包约定的「字内大端」。
func normalizeWords(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i := 0; i+1 < len(out); i += 2 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	return out
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
	area := strings.ToLower(req.Address.Area)
	dev, ok := mccodec.Lookup(area)
	if !ok {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("unknown MC device %q", area), false, nil)
		return writeFail(req, err), err
	}
	if dev.Bit {
		return c.writeBitDevice(ctx, req, dev)
	}
	if req.DataType == model.TypeBool {
		return c.writeWordBit(ctx, req, dev)
	}
	return c.writeWords(ctx, req, dev)
}

func (c *conn) writeBitDevice(ctx context.Context, req driver.WriteRequest, dev mccodec.Device) (driver.WriteResult, error) {
	if req.DataType != model.TypeBool {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("bit device %s requires bool, got %s", req.Address.Area, req.DataType), false, nil)
		return writeFail(req, err), err
	}
	data := mccodec.PackBits([]bool{req.Value.Bool})
	return c.write(ctx, dev, req.Address.Offset, 1, true, data, req)
}

func (c *conn) writeWordBit(ctx context.Context, req driver.WriteRequest, dev mccodec.Device) (driver.WriteResult, error) {
	if req.Address.Bit < 0 || req.Address.Bit > 15 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			fmt.Sprintf("bool point on word device %s needs address.bit 0..15", req.Address.Area), false, nil)
		return writeFail(req, err), err
	}
	// 读整字 → 只改目标位 → 写整字：邻位属于别的点位，不能被清零。
	data, err := c.read(ctx, strings.ToLower(req.Address.Area), req.Address.Offset, 1, false)
	if err != nil {
		return writeFail(req, err), err
	}
	if len(data) < 2 {
		err := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, "short word read", false, nil)
		return writeFail(req, err), err
	}
	word := binary.LittleEndian.Uint16(data)
	if req.Value.Bool {
		word |= 1 << uint(req.Address.Bit)
	} else {
		word &^= 1 << uint(req.Address.Bit)
	}
	payload := make([]byte, 2)
	binary.LittleEndian.PutUint16(payload, word)
	return c.write(ctx, dev, req.Address.Offset, 1, false, payload, req)
}

func (c *conn) writeWords(ctx context.Context, req driver.WriteRequest, dev mccodec.Device) (driver.WriteResult, error) {
	words := reqWords(req)
	if words <= 0 {
		err := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("point %q has no width", req.PointID), false, nil)
		return writeFail(req, err), err
	}
	var payload []byte
	if req.DataType == model.TypeString || req.DataType == model.TypeBytes {
		// 字符串/字节：按地址宽度补零或截断，再转成线上字内小端。
		raw := make([]byte, words*2)
		if req.DataType == model.TypeString {
			copy(raw, req.Value.Text)
		} else {
			// Bytes 类型在 model.Value 里是 []byte 的 Text 承载（见 Value.Bytes 注释），
			// 这里直接用原始字节；若 Text 为空则写一个零字。
			copy(raw, []byte(req.Value.Text))
		}
		payload = swapWords(raw)
	} else {
		raw, err := value.Encode(req.Value, req.DataType, mcOrder(req.ByteOrder))
		if err != nil {
			err = driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
			return writeFail(req, err), err
		}
		if len(raw) > words*2 {
			err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
				fmt.Sprintf("point %q needs %d bytes but address has %d", req.PointID, len(raw), words*2), false, nil)
			return writeFail(req, err), err
		}
		raw = append(raw, make([]byte, words*2-len(raw))...)
		payload = swapWords(raw)
	}
	return c.write(ctx, dev, req.Address.Offset, words, false, payload, req)
}

func (c *conn) write(ctx context.Context, dev mccodec.Device, head, points int, bit bool, data []byte, req driver.WriteRequest) (driver.WriteResult, error) {
	frame, err := mccodec.BuildWrite(dev.Code, head, points, bit, data)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, err), err
	}
	resp, err := c.tr.RoundTrip(ctx, frame)
	if err != nil {
		return writeFail(req, err), err
	}
	if _, err := mccodec.ParseResponse(resp); err != nil {
		return writeFail(req, err), err
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

func swapWords(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i := 0; i+1 < len(out); i += 2 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	return out
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
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress, driver.ErrUnsupported:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	var ec *mccodec.EndCodeError
	if ok := asEndCode(err, &ec); ok {
		switch ec.Code {
		case 0xC052, 0xC053, 0xC054, 0xC056:
			return model.QualityAddressError
		}
	}
	return model.QualityBad
}

func asEndCode(err error, target **mccodec.EndCodeError) bool {
	for err != nil {
		if e, ok := err.(*mccodec.EndCodeError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}
}

// ---------------------------------------------------------------------------
// TCP 分帧
// ---------------------------------------------------------------------------

type framing struct {
	stream transport.Stream
}

// RoundTrip 写一个 3E 请求，按响应头里的数据长度读满整帧。
func (f *framing) RoundTrip(ctx context.Context, req []byte) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		if s, ok := f.stream.(transport.DeadlineSetter); ok {
			_ = s.SetDeadline(dl)
		}
	}
	if err := f.stream.Write(req); err != nil {
		return nil, classifyNetErr("write", err)
	}
	head, err := f.stream.ReadFull(9)
	if err != nil {
		return nil, classifyNetErr("read-header", err)
	}
	if binary.LittleEndian.Uint16(head[0:]) != mccodec.SubheaderResponse {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("bad MC subheader % x", head[0:2]), false, nil)
	}
	dataLen := int(binary.LittleEndian.Uint16(head[7:]))
	if dataLen < 2 || dataLen > 4096 {
		return nil, driver.E(driver.ErrProtocol, "read", "",
			fmt.Sprintf("invalid MC data length %d", dataLen), false, nil)
	}
	body, err := f.stream.ReadFull(dataLen)
	if err != nil {
		return nil, classifyNetErr("read-body", err)
	}
	return append(head, body...), nil
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
