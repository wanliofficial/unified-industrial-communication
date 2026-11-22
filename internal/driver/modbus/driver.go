// Package modbus 实现 Modbus 主站驱动，覆盖 TCP 与 RTU 两种链路。
//
// 分层：driver.Meta（能力声明）→ Conn（事务/错误分类）→
// codec/modbus（纯函数编解码）→ transport（字节流）。
//
// TCP 与 RTU 只在「分帧」这一层不同，其余（批读、写入、异常码分类、
// 部分失败保留好样本）完全共用同一份代码。这是刻意的：两份实现迟早
// 会在异常路径上分叉。
package modbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	mbcodec "uic/internal/codec/modbus"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
	"uic/internal/transport/serialport"
)

// Dialer 允许测试注入内存链路。
type Dialer func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error)

// SerialOpener 允许测试注入内存串口。
//
// 定义为 serialport.Opener 的别名而不是新类型：否则「共享串口」包与调用方
// 之间每次传递都要显式转换，而那些转换点恰好散落在最容易漏改的错误分支里。
type SerialOpener = serialport.Opener

// Driver 是 Modbus 主站驱动。
type Driver struct {
	dial   Dialer
	serial SerialOpener
}

// New 返回使用真实 TCP 与真实串口的驱动。
func New() *Driver {
	return &Driver{
		dial: func(ctx context.Context, addr string, timeout time.Duration) (transport.Stream, error) {
			return transport.DialTCP(ctx, addr, timeout)
		},
		serial: transport.OpenSerial,
	}
}

// NewWithDialer 返回使用自定义 TCP 链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	drv := New()
	drv.dial = d
	return drv
}

// NewWithSerial 返回使用自定义串口链路的驱动（测试用）。
func NewWithSerial(o SerialOpener) *Driver {
	drv := New()
	drv.serial = o
	return drv
}

// Meta 声明驱动能力。Modbus 不支持订阅、浏览与设备发现。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "modbus-tcp",
		Name:         "Modbus TCP",
		Version:      "0.3.0",
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
				Placeholder: "192.168.1.10:502", Scope: "channel"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 4, Scope: "channel",
				Help: "同一通道允许的最大在飞请求数"},
			{Name: "labels.options", Label: "扩展选项", Type: "string", Scope: "device",
				Placeholder: "maskwrite=auto",
				Help: "逗号分隔：maskwrite=auto|on|off 控制寄存器位写是否用 FC22 原子掩码写" +
					"（auto 先试、设备回 01 则退回读-改-写）"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.area", Label: "区", Type: "enum", Required: true,
				Enum: []string{"holding", "input", "coil", "discrete"}, Scope: "point"},
			{Name: "address.offset", Label: "偏移", Type: "int", Required: true, Scope: "point"},
			{Name: "address.bit", Label: "位", Type: "int", Default: -1, Scope: "point"},
		},
		Description: "Modbus TCP 主站，支持 FC1/2/3/4/5/6/15/16，寄存器位写优先走 FC22 掩码写",
	}
}

// Open 建立链路并按传输类型选择分帧方式。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	mode, err := parseOptions(cfg.Device)
	if err != nil {
		return nil, err
	}
	tr, err := d.openTransport(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &conn{
		cfg:      cfg,
		codec:    mbcodec.NewCodec(cfg.Device.ID),
		tr:       tr,
		maskMode: mode,
	}, nil
}

// maskMode 决定寄存器位写走哪条路径。
type maskMode int

const (
	// maskAuto：先试 FC22（设备侧原子掩码写），设备回「非法功能码」就退回
	// 读-改-写，并在本次连接内记住结论。默认值。
	maskAuto maskMode = iota
	// maskOn：只用 FC22。设备不支持时写入直接失败——现场若已确认设备支持，
	// 静默退回读-改-写会掩盖「设备被换成了另一台」。
	maskOn
	// maskOff：只用读-改-写。有些设备的协议栈遇到不认识的功能码会**断开
	// 连接**而不是回 01，自动探测在那类设备上表现为「第一次位写之后整台
	// 设备离线」，必须能从配置里关掉。
	maskOff
)

// parseOptions 解析 device.labels["options"]。
//
// 与 hostlink/mewtocol/ads 同一纪律：未知项、缺值都报错，不悄悄回默认值。
// 「maskwrite 拼错成 maskWrite」如果被忽略，现场看到的是「配置改了没效果」。
func parseOptions(dev model.Device) (maskMode, error) {
	mode := maskAuto
	raw := dev.Labels["options"]
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.ToLower(strings.TrimSpace(val))
		switch key {
		case "maskwrite":
			if !hasVal {
				return mode, driver.E(driver.ErrUnsupported, "open", dev.ID,
					"options.maskwrite 需要 =auto|on|off", false, nil)
			}
			switch val {
			case "auto":
				mode = maskAuto
			case "on", "true", "yes":
				mode = maskOn
			case "off", "false", "no":
				mode = maskOff
			default:
				return mode, driver.E(driver.ErrUnsupported, "open", dev.ID,
					fmt.Sprintf("options.maskwrite %q 必须是 auto|on|off", val), false, nil)
			}
		default:
			return mode, driver.E(driver.ErrUnsupported, "open", dev.ID,
				fmt.Sprintf("未知的 options 项 %q（支持 maskwrite）", key), false, nil)
		}
	}
	return mode, nil
}

func (d *Driver) openTransport(ctx context.Context, cfg driver.OpenConfig) (framed, error) {
	t := cfg.Channel.Transport
	switch t.Kind {
	case "tcp":
		stream, err := d.dial(ctx, t.Address, cfg.Channel.EffectiveTimeout())
		if err != nil {
			return nil, driver.E(driver.ErrDisconnected, "dial", cfg.Device.ID, "connect failed", true, err)
		}
		return &tcpFraming{stream: stream, unit: uint8(cfg.Device.UnitID)}, nil
	case "serial":
		// 串口走独立的 modbus-rtu 驱动（rtu.go）：它需要端口复用与总线
		// 串行化，这两件事在只处理 TCP 的驱动里没有位置。
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"serial transport requires driver modbus-rtu", false, nil)
	case "":
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"transport.kind required (tcp|serial)", false, nil)
	}
	return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
		fmt.Sprintf("modbus does not support transport %q", t.Kind), false, nil)
}

// framed 是一次 PDU 往返。TCP 与 RTU 各自实现，上层 conn 不感知差异。
type framed interface {
	RoundTrip(ctx context.Context, pdu []byte) ([]byte, error)
	Close() error
}

type conn struct {
	mu sync.Mutex
	// wmu 串行化写入。写本身已经是低频路径，而「读-改-写」要求同一寄存器的
	// 两次事务之间不被同一驱动的其他写入插进来——否则两个并发写入会互相
	// 覆盖对方改的位。读不加这把锁：读之间没有互斥需求。
	wmu   sync.Mutex
	cfg   driver.OpenConfig
	tr    framed
	codec *mbcodec.Codec
	// maskMode 是配置里的 FC22 策略；maskUsable 是运行期结论（设备是否真的
	// 认这个功能码）。两者分开：配置是「用户允许什么」，结论是「设备能什么」，
	// 混在一个字段里会让「用户改成 off 后仍然走 FC22」这种错难以发现。
	maskMode   maskMode
	maskUsable bool
	maskProbed bool
}

func (c *conn) Probe(ctx context.Context) error {
	// 读 1 个保持寄存器；即使返回异常码，也说明链路可达。
	pdu := []byte{mbcodec.FCReadHoldingRegs, 0, 0, 0, 1}
	_, err := c.tr.RoundTrip(ctx, pdu)
	return err
}

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
			out = append(out, failedSamples(span, err)...)
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
	pdu, err := c.codec.EncodeRead(span)
	if err != nil {
		return nil, err
	}
	resp, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return nil, err
	}
	return c.codec.DecodeRead(span, resp)
}

func (c *conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for i := 0; i < len(reqs); {
		req := reqs[i]
		if needsBitRMW(req) {
			res, err := c.writeBitRMW(ctx, req)
			results = append(results, res)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			i++
			continue
		}
		// 相邻的线圈整点写合并成一个 FC15 事务（Meta 声明的 FC15 落地处）：
		// 一批 N 个点从 N 帧变 1 帧，串口总线上是实打实的开销。寄存器写
		// 不合并——FC16 已承载单点多寄存器值，且合并会打乱逐点回显校验。
		if n := coilRun(reqs, i); n >= 2 {
			batch := c.writeCoilBatch(ctx, reqs[i:i+n])
			for _, res := range batch {
				if res.Err != nil && firstErr == nil {
					firstErr = res.Err
				}
			}
			results = append(results, batch...)
			i += n
			continue
		}
		pdu, err := c.codec.EncodeWrite(req)
		if err != nil {
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: model.QualityBad, Err: err})
			if firstErr == nil {
				firstErr = err
			}
			i++
			continue
		}
		resp, err := c.tr.RoundTrip(ctx, pdu)
		if err != nil {
			results = append(results, driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err})
			if firstErr == nil {
				firstErr = err
			}
			i++
			continue
		}
		res, err := c.codec.DecodeWrite(req, resp)
		if err != nil {
			res.Quality = model.QualityBad
			res.Err = err
			if firstErr == nil {
				firstErr = err
			}
		}
		results = append(results, res)
		i++
	}
	return results, firstErr
}

// coilRun 计算从 i 开始可以合并成一个 FC15 的请求数：都是线圈区的整点
// bool 写、偏移严格连续。任何不满足的请求终止该串（包括重复偏移——同一
// 线圈写两次合并成一次会丢掉其中一次的语义）。
func coilRun(reqs []driver.WriteRequest, i int) int {
	first := reqs[i]
	if first.DataType != model.TypeBool || first.Address.Kind != model.AddressRegister ||
		!mbcodec.IsCoilArea(first.Address.Area) {
		return 0
	}
	n := 1
	for j := i + 1; j < len(reqs); j++ {
		next := reqs[j]
		if next.DataType != model.TypeBool || next.Address.Kind != model.AddressRegister ||
			!mbcodec.IsCoilArea(next.Address.Area) ||
			next.Address.Offset != first.Address.Offset+(j-i) {
			break
		}
		n++
	}
	return n
}

// writeCoilBatch 把一段连续线圈写合成为一次 FC15 事务，结果按请求顺序
// 逐点返回。任何一步失败，整串标同样的错误——FC15 的回显只确认
// 「起始地址 + 数量」，不确认每个位，无法只标坏其中一部分。
func (c *conn) writeCoilBatch(ctx context.Context, reqs []driver.WriteRequest) []driver.WriteResult {
	start := reqs[0].Address.Offset
	bits := make([]bool, len(reqs))
	for i, r := range reqs {
		bits[i] = r.Value.Bool
	}
	out := make([]driver.WriteResult, len(reqs))
	for i := range out {
		out[i] = driver.WriteResult{PointID: reqs[i].PointID}
	}
	fail := func(err error, quality model.Quality) []driver.WriteResult {
		for i := range out {
			out[i].Err = err
			out[i].Quality = quality
		}
		return out
	}
	pdu, err := c.codec.EncodeWriteMultipleCoils(start, bits)
	if err != nil {
		return fail(err, model.QualityBad)
	}
	resp, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return fail(err, model.QualityCommError)
	}
	if err := c.codec.DecodeWriteMultipleCoils(start, len(reqs), resp); err != nil {
		return fail(err, model.QualityBad)
	}
	for i := range out {
		out[i].OK = true
		out[i].Quality = model.QualityGood
	}
	return out
}

// needsBitRMW 判断一次写入是否需要「读-改-写」：布尔点落在保持寄存器里，
// 且指明了具体位号。线圈是整位区（FC5 直接写），不在此列。
func needsBitRMW(req driver.WriteRequest) bool {
	return req.DataType == model.TypeBool &&
		req.Address.Kind == model.AddressRegister &&
		mbcodec.IsHoldingArea(req.Address.Area) &&
		req.Address.Bit >= 0
}

// writeBitRMW 以「读整字 → 改一位 → 写整字」完成寄存器内位写。
//
// 为什么不能直接写 FC6：写 0/1 会把同一寄存器里的其他 15 位一并覆盖。
// 这类错误在调试时表现为「改了 A 点，B 点跟着变了」，而配置与代码都看不出
// 问题——必须从协议层就避免。
//
// 优先走 FC22（设备侧掩码写）：它把「读-改-写」的两帧压成一次原子操作。
// 两帧版本的空窗期里，PLC 自己的程序或另一个主站改掉同一寄存器的其它位，
// 会被本驱动带着旧值写回覆盖——这种丢更新在现场表现为「偶尔有个位自己弹回去」，
// 日志里一切正常。设备不支持 FC22 时退回两帧版本，并在本次连接内记住结论。
//
// 原子性边界（诚实说明）：
//   - 同一驱动的并发写入由 wmu 串行化，两个位写不会互相吞掉；
//   - 走 FC22 时设备侧原子，窗口消失；
//   - 退回读-改-写时，同一串口总线上的其他设备若恰好写同一个 unit 的同一个
//     寄存器，仍可能插在这两帧之间。那是配置冲突（一个寄存器只能有一个所有者），
//     不是驱动能替用户解决的；文档里已写明。
//   - 另一个主站（SCADA 直连同一从站）不受本驱动控制，同样需要现场约束。
func (c *conn) writeBitRMW(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	bit := req.Address.Bit
	if bit < 0 || bit > 15 {
		err := driver.E(driver.ErrAddress, "write", c.cfg.Device.ID,
			"register bit write requires address.bit 0..15", false, nil)
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}

	if c.wantMaskWrite() {
		res, err := c.writeBitMask(ctx, req, bit)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, mbcodec.ErrIllegalFunction) {
			return res, err
		}
		// 设备不认识 FC22。maskAuto 下永久退回读-改-写；maskOn 下这是配置
		// 与设备不符，必须让人看见，不能悄悄换条路走。
		c.maskProbed, c.maskUsable = true, false
		if c.maskMode == maskOn {
			err := driver.E(driver.ErrUnsupported, "write", c.cfg.Device.ID,
				"device rejects FC22 mask write (illegal function) but options.maskwrite=on", false, err)
			return driver.WriteResult{PointID: req.PointID, Quality: model.QualityBad, Err: err}, err
		}
		c.logf("设备不支持 FC22 掩码写，寄存器位写退回读-改-写",
			"device", c.cfg.Device.ID, "register", req.Address.Offset)
	} else {
		c.maskProbed, c.maskUsable = true, false
	}

	pdu, err := c.codec.EncodeReadRegisters(req.Address.Offset, 1)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}
	resp, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err}, err
	}
	words, err := c.codec.DecodeRegisters(resp, 1)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}, err
	}

	next := mbcodec.WordSetBit(words[0], bit, req.Value.Bool)
	wpdu, err := c.codec.EncodeWriteRegister(req.Address.Offset, next)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}
	resp, err = c.tr.RoundTrip(ctx, wpdu)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err}, err
	}
	res, err := c.codec.DecodeWrite(req, resp)
	if err != nil {
		res.Quality = model.QualityBad
		res.Err = err
	}
	return res, err
}

// wantMaskWrite 决定这次位写是否先试 FC22。已探测出不支持就不再试——
// 每轮先失败一次再退回会让串口总线上的写慢一倍，而且错误计数里全是噪声。
func (c *conn) wantMaskWrite() bool {
	switch c.maskMode {
	case maskOff:
		return false
	case maskOn:
		return true
	}
	return !c.maskProbed || c.maskUsable
}

// writeBitMask 用 FC22 完成一次原子位写。
//
// 成功时锁存 maskUsable=true：这是运行期学会的事实，不需要配置提前声明
// 「这台设备支持 FC22」——那类开关最终都是「默认值对厂商 A 有效、对厂商 B 报错」。
func (c *conn) writeBitMask(ctx context.Context, req driver.WriteRequest, bit int) (driver.WriteResult, error) {
	and, or, err := mbcodec.MaskWriteWord(bit, req.Value.Bool)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.cfg.Device.ID, err.Error(), false, err)
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}
	pdu, err := c.codec.EncodeMaskWriteRegister(req.Address.Offset, and, or)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityAddressError, Err: err}, err
	}
	resp, err := c.tr.RoundTrip(ctx, pdu)
	if err != nil {
		return driver.WriteResult{PointID: req.PointID, Quality: model.QualityCommError, Err: err}, err
	}
	if err := c.codec.DecodeMaskWriteRegister(req.Address.Offset, and, or, resp); err != nil {
		// 异常码 01（非法功能）已在 codec 层映射成 ErrIllegalFunction，
		// 这里原样上抛，由 writeBitRMW 决定要不要退回读-改-写。
		return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}, err
	}
	c.maskProbed, c.maskUsable = true, true
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// logf 是可选日志：驱动在测试里常常没有注入 logger。
func (c *conn) logf(msg string, args ...any) {
	if c.cfg.Log != nil {
		c.cfg.Log.Info(msg, args...)
	}
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tr == nil {
		return nil
	}
	err := c.tr.Close()
	c.tr = nil
	return err
}

func failedSamples(span driver.ReadSpan, err error) []driver.RawSample {
	q := qualityOf(err)
	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		out = append(out, driver.RawSample{PointID: p.ID, Quality: q, Err: err})
	}
	return out
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrUnsupported, driver.ErrPermission:
		return model.QualityBad
	}
	return model.QualityBad
}

func classifyNetErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return driver.E(driver.ErrTimeout, op, "", "network timeout", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return driver.E(driver.ErrTimeout, op, "", "deadline exceeded", true, err)
	}
	if errors.Is(err, errClosedStream) {
		return driver.E(driver.ErrDisconnected, op, "", "connection closed", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, "", "network error", true, err)
}

var errClosedStream = errors.New("transport: closed")
