// Package hostlink 实现 Host Link FINS（上位链接 FINS）主站驱动。
//
// 分层：Meta（能力声明）→ 本文件的 transactor（Host Link 信封：单元号、
// 等待时间、FCS、`*`+CR 定界）→ driver/finscore（FINS 命令语义与内存区
// 读写）→ codec/hostlink（纯函数编解码与收帧）。
//
// 为什么单独做一个驱动，而不是给 FINS/TCP 加一个「串口模式」开关：两者的
// 信封完全不同，连「一帧能装多少」都不同（TCP 999 字、Host Link 269 字）。
// 做成一个开关就会出现一组只在某一种承载下成立的默认值，而这些默认值恰好
// 又是最容易照抄错的东西。共用的是**命令语义**（finscore），不是封装。
//
// 四个刻意的边界：
//
//  1. **只做 FINS 帧（头码 `FA`），不做 C 模式。** C 模式（`RR`/`RD`/`WD`…）
//     是另一套命令集、另一套寻址写法：地址是「通道号 + 位号」的 BCD 文本，
//     没有区码表，也没有 FINS 结束码。两套命令共用一个驱动，每个字段都要
//     先判断「现在走哪套」，而调用方只想要「读 DM100」。
//
//  2. **只做直连短格式，不做跨网长格式。** 跨网格式在精简头之外还要
//     RSV/GCT/DNA/DA1/SNA/SA1 六个字段（多 12 个字符），而 Host Link 的
//     目标节点由单元号承载、路由由上一跳决定。真需要跨网时应当用串行网关
//     或以太网单元，不该让一个直连驱动去猜路由。-
//
//  3. **串口参数缺省 7/E/2，不是 8/E/1。** 整帧是可打印 ASCII（正文是
//     「一字节两字符」的十六进制），7 位数据位就够；出厂默认也正是
//     9600、偶校验、2 停止位。反过来，把 FINS/TCP 用户的习惯（8/N/1）
//     套过来会「发得出去、收不到东西」——而且不报错。这条与 Mewtocol 正好
//     相反（那边是 8 位、奇校验、1 停止位），是本平台两处最容易被抄错的
//     默认值，所以两边都在 Meta 里显式写出来。
//
//  4. **不自动重试。** 重试只能在「确定这条应答不是迟到的那一条」时才有意义。
//     Host Link 没有事务号可以排出歧义（SID 只有 8 位，且很多 PLC 固定回
//     00），盲目重发一次写可能把同一个字写两遍——对「加计数」这类点位这是
//     真实的数据错误。重连交给调度器，由它决定代价。
package hostlink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	finscodec "uic/internal/codec/fins"
	hlcodec "uic/internal/codec/hostlink"
	"uic/internal/driver"
	"uic/internal/driver/finscore"
	"uic/internal/driver/link"
	"uic/internal/model"
	"uic/internal/transport/serialport"
)

// 缺省值与上限。
const (
	// defaultTCPPort 是串口服务器/以太网单元的常见透传端口。
	//
	// 这里刻意不用 9600：9600 既是 FINS/TCP 的端口也是一个常见波特率，
	// 混在一起会让人以为「端口 9600 = 波特率 9600」。2000 是另一家透传
	// 设备的常用端口，但真正正确的做法是在配置里显式写端口。
	defaultTCPPort = "2000"
	// 出厂串口参数：9600、偶校验、2 停止位。
	defaultBaud     = 9600
	defaultDataBits = 7
	defaultParity   = "even"
	defaultStopBits = 2
	// defaultWait 是响应等待时间（单位 10ms）。0 表示「收到就答」。
	defaultWait = 0
	// defaultGap 是两次事务之间的静默。
	//
	// Host Link 在 RS-485 上是半双工：主机发完必须切回接收，PLC 才能驱动
	// 总线。适配器的方向切换延迟实测 1~20ms，取 5ms 覆盖多数 USB 转 485；
	// 太长会拖慢轮询，太短会让第一帧的应答被自己的发送残余吃掉。
	defaultGap = 5 * time.Millisecond
)

// Driver 是 Host Link FINS 主站驱动。
type Driver struct {
	serial serialport.Opener
}

// New 返回使用真实串口的驱动。
func New() *Driver { return &Driver{} }

// NewWithSerial 返回注入串口实现的驱动（测试用）。
func NewWithSerial(o serialport.Opener) *Driver { return &Driver{serial: o} }

// Meta 声明能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "hostlink",
		Name:         "Omron Host Link FINS（上位链接）",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"serial", "tcp"},
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
			{Name: "transport.port", Label: "串口", Type: "string", Scope: "channel",
				Placeholder: "/dev/ttyUSB0", Help: "串口承载时必填；TCP 承载留空"},
			{Name: "transport.baud", Label: "波特率", Type: "int", Default: defaultBaud, Scope: "channel",
				Help: "出厂 9600；必须与 PLC 的「上位链接端口」设置一致"},
			{Name: "transport.dataBits", Label: "数据位", Type: "int", Default: defaultDataBits, Scope: "channel",
				Help: "7 位即可（整帧是可打印 ASCII）；出厂也是 7 位"},
			{Name: "transport.parity", Label: "校验", Type: "enum", Default: defaultParity, Scope: "channel",
				Enum: []string{"none", "even", "odd"},
				Help: "出厂是偶校验。与 Mewtocol 的奇校验相反，改错会「能发不能收」"},
			{Name: "transport.stopBits", Label: "停止位", Type: "int", Default: defaultStopBits, Scope: "channel",
				Help: "出厂是 2 位。与 Mewtocol 的 1 位相反"},
			{Name: "transport.address", Label: "TCP 地址", Type: "string", Scope: "channel",
				Placeholder: "192.168.1.10:2000",
				Help:        "串口服务器/以太网单元的 host:port；省略端口时用 2000"},
			{Name: "unitId", Label: "单元号", Type: "int", Default: 0, Scope: "device",
				Help: "PLC 的上位链接单元号，0..31（两位十进制）。同一根线上必须唯一"},
			{Name: "labels.options", Label: "扩展选项", Type: "string", Scope: "device",
				Placeholder: "gap=5ms,wait=1",
				Help:        "逗号分隔：gap=<时长> 帧间静默；wait=<0..15> 响应等待时间（单位 10ms）"},
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
		Description: "欧姆龙 CS/CJ/CP 系列的上位链接 FINS（头码 FA），串口出厂 9600-7-E-2。" +
			"支持 CIO/WR/HR/AR/DM/EM 内存区的字/位读写；单次读 269 字、写 267 字（整帧字符上限）。",
	}
}

// Options 是设备级可调参数。
type Options struct {
	// Gap 是两次事务之间的静默。
	Gap time.Duration
	// Wait 是响应等待时间，单位 10ms，取值 0..15。
	Wait byte
}

// Open 建链并校验单元号。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	devID := cfg.Device.ID
	unit, err := unitOf(cfg.Device)
	if err != nil {
		return nil, err
	}
	opt := parseOptions(cfg.Device)

	l, err := link.Open(ctx, link.Options{
		Proto:       "hostlink",
		DefaultPort: defaultTCPPort,
		Opener:      d.serial,
	}, serialDefaults(cfg.Channel.Transport), cfg.Channel.EffectiveTimeout(), devID)
	if err != nil {
		return nil, err
	}
	tr := &transactor{
		link:    l,
		unit:    unit,
		wait:    opt.Wait,
		gap:     opt.Gap,
		devID:   devID,
		timeout: cfg.Channel.EffectiveTimeout(),
	}
	return finscore.New(cfg, tr, hostLinkLimits()), nil
}

// hostLinkLimits 把 Host Link 的字符预算交给 finscore。
//
// 读受应答帧约束、写受命令帧约束，所以这四个数字不是同一个值（见
// codec/hostlink 的推导）。
func hostLinkLimits() finscore.Limits {
	return finscore.Limits{
		WordsPerRead:  hlcodec.MaxReadWords,
		WordsPerWrite: hlcodec.MaxWriteWords,
		BitsPerRead:   hlcodec.MaxReadBits,
		BitsPerWrite:  hlcodec.MaxWriteBits,
	}
}

// serialDefaults 把「缺省串口参数」补进配置。
//
// 为什么要在这里补而不是靠 link 层：link 层是通用的，它看到 baud=0 只会
// 报「参数不合法」。而 Host Link 的出厂参数是协议事实（9600-7-E-2），
// 不写出来就意味着每个点表都得抄一遍这四个值。
//
// 只补「完全没配」的情况：只有 baud/dataBits/parity/stopBits **四项全空**
// 才补成 9600-7-E-2。只要用户填了任一项，其余项就保持零值，由
// transport.Normalize 补通用缺省（9600-8-N-1）——那是用户已经知道自己在
// 做什么的信号，替他把 7 位数据位、偶校验、2 停止位改回来反而更意外：
// 现场最常见的「只想改波特率」配置（例如 baud=19200）会被悄悄变成
// 19200-7-E-2，对一台 19200-8-N-1 的 PLC 表现为「发得出去、收不到东西」，
// 而且不报错——正是本驱动最要避免的那类静默故障。
func serialDefaults(t model.Transport) model.Transport {
	if t.Kind != "serial" {
		return t
	}
	if t.Baud != 0 || t.DataBits != 0 || t.Parity != "" || t.StopBits != 0 {
		return t
	}
	t.Baud = defaultBaud
	t.DataBits = defaultDataBits
	t.Parity = defaultParity
	t.StopBits = defaultStopBits
	return t
}

// unitOf 读取并校验单元号。
//
// 缺省 0：单机直连时出厂单元号就是 0，且 0 也是「CPU 单元」的单元地址。
// 与 Mewtocol 缺省 1 不同（那边 0 是广播，不能用）。
func unitOf(dev model.Device) (byte, error) {
	n := dev.UnitID
	if n < 0 {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("unit number %d is negative", n), false, nil)
	}
	if n > hlcodec.MaxUnit {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("unit number %d out of range 0..%d", n, hlcodec.MaxUnit), false, nil)
	}
	return byte(n), nil
}

func parseOptions(dev model.Device) Options {
	opt := Options{Gap: defaultGap, Wait: defaultWait}
	for _, part := range strings.Split(dev.Labels["options"], ",") {
		key, val, hasVal := strings.Cut(strings.TrimSpace(part), "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if !hasVal {
			continue
		}
		switch key {
		case "gap":
			if d, err := time.ParseDuration(val); err == nil && d >= 0 && d <= time.Second {
				opt.Gap = d
			}
		case "wait":
			var n int
			if _, err := fmt.Sscanf(val, "%d", &n); err == nil && n >= 0 && n <= hlcodec.MaxWait {
				opt.Wait = byte(n)
			}
		}
	}
	return opt
}

// ---------------------------------------------------------------------------
// Host Link 信封
// ---------------------------------------------------------------------------

// transactor 实现 finscore.Transactor：把 FINS 帧套上 Host Link 信封发出去。
type transactor struct {
	link    link.Link
	unit    byte
	wait    byte
	gap     time.Duration
	devID   string
	timeout time.Duration

	// mu 串行化事务。契约上同一 Conn 不会被并发调用，加锁是为了在错误
	// 使用下宁可慢一点，也不要让两条请求的字节在总线上交错。
	mu     sync.Mutex
	sid    byte
	closed bool
}

// Header 返回 FINS 请求帧头。
//
// 直连时节点地址全是 0：Host Link 没有节点号协商，目标由单元号表达。ICF
// 用 0x00（直连），不是 FINS/TCP 惯用的 0x80——0x80 的含义是「经网络
// 中转」，PLC 会去找路由表，而这是一条点对点串口线。
func (t *transactor) Header() finscodec.Header {
	return finscodec.Header{ICF: 0x00, GCT: 0x02, SID: 0}
}

func (t *transactor) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	return t.link.Close()
}

// RoundTrip 发送一帧 FINS 请求并读回已校验的应答。
func (t *transactor) RoundTrip(ctx context.Context, req []byte) (finscodec.Frame, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		// 关闭后仍被调用是调度器的正常重连路径（先 Close 再 Open），
		// 必须标记为可重试，否则会被当成协议错而熔断。
		return finscodec.Frame{}, driver.E(driver.ErrDisconnected, "request", t.devID,
			"connection closed", true, nil)
	}

	t.sid++
	sid := t.sid
	// 帧头里的 ICF/DA2/SA2/SID 由本层决定：上层构造的帧只保证 MRC/SRC 与
	// 正文正确，头里这几个字段在 Host Link 下有别的取值。
	frame := append([]byte(nil), req...)
	frame[0] = 0x00
	frame[9] = sid

	hr, err := hlcodec.FromFINS(t.unit, t.wait, 0x00, frame)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", t.devID, err.Error(), false, err)
	}
	out, err := hlcodec.Build(hr)
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrInternal, "request", t.devID, err.Error(), false, err)
	}

	// 顺序很关键：Drain 会把读截止时间压到「现在 + 1ms」，必须在它之后
	// 再设本次事务的截止时间。反过来会让截止时间被覆盖成过去时刻，症状
	// 是「连上了，但每次请求都 timeout」。
	t.link.Lock()
	defer t.link.Unlock()
	if t.gap > 0 {
		select {
		case <-ctx.Done():
			return finscodec.Frame{}, driver.E(driver.ErrTimeout, "request", t.devID, "context done", true, ctx.Err())
		case <-time.After(t.gap):
		}
	}
	_ = t.link.Drain()
	t.setDeadline(ctx)

	if err := t.link.Write(out); err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrDisconnected, "write", t.devID, "write failed", true, err)
	}

	rd := hlcodec.NewReader(t.link)
	resp, err := rd.ReadFrame(t.unit, sid)
	if err != nil {
		return finscodec.Frame{}, t.classifyRead(err, ctx)
	}
	if resp.MRC != frame[10] || resp.SRC != frame[11] {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", t.devID,
			fmt.Sprintf("response echoes command %02X%02X, request was %02X%02X "+
				"(bus noise or another master on the line)",
				resp.MRC, resp.SRC, frame[10], frame[11]), true, nil)
	}
	if resp.FinsEndCode != 0 {
		// 结束码必须按语义分类，**不能一律当协议错**。
		//
		// 早先这里写的是 driver.E(driver.ErrProtocol, …, temporary=true)：
		// 每一类结束码都被标成「可重试」，后果是 0x1103（地址越界）这种点表错误
		// 被重发一遍（结果相同），并且——更糟——被 driver.CountsTowardBreaker
		// 计入熔断：一个填错地址的点，会把整台 PLC 标成坏设备。而 0x1101..0x1104
		// 按 FINS 手册是地址类，本就不该重试也不该熔断；链路类（节点不在网、
		// 令牌超时、路由表错）才是「重发一次可能就通了」的那一类。
		//
		// FINS/TCP 信封对 FINS 结束码的做法是不判定、原样返回；这里的 Host
		// Link 信封不同：它没有 FINS/TCP 那层分类，只能自己判定。分类依据是
		// 同一张结束码表（finscodec.EndCodeError.Address/Link），两个驱动因此
		// 对同一个码给出同一个 Kind 与同一个重试决策。
		return finscodec.Frame{}, hostLinkEndCodeError(resp.FinsEndCode, t.devID)
	}
	parsed, err := finscodec.Parse(resp.FINS())
	if err != nil {
		return finscodec.Frame{}, driver.E(driver.ErrProtocol, "read", t.devID, err.Error(), false, err)
	}
	return parsed, nil
}

func (t *transactor) setDeadline(ctx context.Context) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(t.timeout)
	}
	if t.timeout <= 0 && !ok {
		dl = time.Now().Add(finscore.DefaultTimeout())
	}
	_ = t.link.SetDeadline(dl)
}

// hostLinkEndCodeError 把一个 FINS 结束码（Host Link 应答正文里的 4 位
// 十六进制）翻译成带分类的驱动错误。
//
// 为什么信封层要自己判定，而 FINS/TCP 不用：FINS/TCP 把 FINS 结束码原样返回
// 给调用方，由 finscore 判定；Host Link 没有那层二进制定长封套，结束码是解析
// 应答时顺手拿到的，本函数就是这里的等价物。两者必须使用**同一张表**
// （finscodec.EndCodeError.Address/Link），否则同一个地址错在 TCP 上显示
// address_error、在串口上只是 bad——而这两个驱动共用的正是 finscore 的命令
// 语义。
//
// 三类的依据是「重发一次会不会好」：
//
//   - 地址类（0x1101..0x1104 区码不认识 / 范围超限 / 地址越界 / 写保护）：
//     ErrAddress，temporary=false。重发一万次结果相同，而且 ErrAddress 走
//     driver.CountsTowardBreaker 时不计入熔断——设备是好的，是这个点写错了。
//   - 链路类（0x0201..0x0205、0x0301/0x0302 节点不在网 / 令牌超时 / 路由表
//     错 / 单元缺失）：ErrProtocol + temporary=true。重发通常正是它等的动作。
//   - 其余（模式不允许、权限、命令未定义、帧长错……）：ErrProtocol，不重试。
func hostLinkEndCodeError(code uint16, devID string) error {
	ec := &finscodec.EndCodeError{Code: code}
	switch {
	case ec.Address():
		return driver.E(driver.ErrAddress, "read", devID, ec.Error(), false, ec)
	case ec.Link():
		return driver.E(driver.ErrProtocol, "read", devID, ec.Error(), true, ec)
	default:
		return driver.E(driver.ErrProtocol, "read", devID, ec.Error(), false, ec)
	}
}

// classifyRead 把「收帧/解析」阶段的错误映射成驱动错误分类。
//
// 三类要分开，因为处置方式完全不同：
//   - 超时：先查单元号、波特率/校验位、PLC 是否处于上位链接模式；
//   - 帧格式/FCS 错：线路噪声或参数错位，可重试但更需要查线；
//   - 单元号或 SID 不匹配：总线上有另一台 PLC 或上一轮的应答迟到，属
//     「通信错误」，但不能当作点位地址错。
func (t *transactor) classifyRead(err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, "read", t.devID, "context done", true, ctxErr)
	}
	var ec *hlcodec.EndCodeError
	if errors.As(err, &ec) {
		// 信封层结束码：0x13/0x14/0x15/0x18 是帧本身写错了，重发无用；
		// 其余（模式不允许、写保护…）是 PLC 状态，值得重试一次。
		kind := driver.ErrProtocol
		if ec.Code == 0x03 || ec.Code == 0x23 {
			kind = driver.ErrPermission
		}
		return driver.E(kind, "read", t.devID, ec.Error(), !ec.Format(), ec)
	}
	var ce finscore.CommError
	if errors.As(err, &ce) {
		return driver.E(driver.ErrProtocol, "read", t.devID, err.Error(), true, err)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, "read", t.devID,
			"no response; check unit number, baud rate/parity/stop bits (9600-7-E-2 default), "+
				"and whether the port is set to Host Link mode", true, err)
	}
	if errors.Is(err, errNoFrame) {
		return driver.E(driver.ErrProtocol, "read", t.devID, err.Error(), true, err)
	}
	// 还没开始读就断链，和读到一半断链，处置一致：重连。
	if isConnErr(err) {
		return driver.E(driver.ErrDisconnected, "read", t.devID, "connection lost", true, err)
	}
	return driver.E(driver.ErrProtocol, "read", t.devID, err.Error(), true, err)
}

// errNoFrame 保留给「收到字节但认不出帧」这类情况；目前由 codec 直接返回
// 具体错误，这里只作为分类兜底。
var errNoFrame = errors.New("hostlink: no frame")

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeout interface{ Timeout() bool }
	if t, ok := err.(timeout); ok && t.Timeout() {
		return true
	}
	// 串口读超时在不同平台上是不同的错误值；按措辞兜底。
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func isConnErr(err error) bool {
	// 各层的关闭错误没有统一哨兵值（TCP 流、串口、poller 各不相同），
	// 按措辞兜底；这里只需要「是不是断链」这一个判断。
	return errors.Is(err, net.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "closed")
}
