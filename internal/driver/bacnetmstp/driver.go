// Package bacnetmstp 实现 BACnet MS/TP（ASHRAE 135 clause 9）主站驱动。
//
// 定位：**单一主站轮询从站设备**。一条 RS-485 总线上只有本机一个主站
// （MAC 0..127），其余节点都是从站（MAC 128..254）。主站用
// DataExpectingReply 向从站要数据，从站回 DataNotExpectingReply，令牌
// 不发生转移——从站不持有令牌，这正是「单一主站」能成立的原因。
//
// 刻意不做的能力，以及为什么：
//
//   - **不做多主站令牌环的建立与维护**。Tslot 抢占、NextStation 周期探活、
//     Tno_token/Tusage_timeout 这些定时器要求毫秒级的常驻状态机；而现场
//     常见的接法是「串口服务器把一条 RS-485 透传到以太网」，TCP 往返与
//     缓冲抖动会直接破坏时序。一个只在收到请求时才醒来的驱动，伪装成
//     令牌环成员只会把别人的环拖垮。
//   - **不做 Router（跨网段）**。NPDU 的路由字段能解析（真实设备可能经
//     路由器回包），但不生成。
//   - **不做 COBS 扩展帧**（MS/TP over IP 的规范）、不做波特率自适应、
//     不做 ReplyPostponed 的后续上送。ReplyPostponed 直接映射成协议错误：
//     与其猜「从站过一会儿会主动送哪一帧」，不如让上层重试。
//   - **轮询主站设备**（0..127）也不做。问主站要数据需要把令牌交给它、
//     等它用完再要回来，属于令牌环语义；本驱动只问从站。
//
// 被动配合：在等待从站应答的窗口里，如果收到发给本机的 Token，会记住并
// 在本次事务结束后交给下一个主站；收到 PollForMaster / TestRequest 会
// 立即应答。但**空闲时不监听总线**——这是 sole-master 拓扑的明确前提，
// 不是遗漏。若现场确实有第二个主站，请把本驱动换掉，而不是打开
// labels.nextStation 指望它维护令牌环。
package bacnetmstp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/driver"
	"uic/internal/driver/link"
	"uic/internal/model"
	"uic/internal/transport"
)

const (
	// defaultPort 是 TCP 承载（串口服务器/DTU 透传）省略端口时的默认值。
	// MS/TP 本身没有端口概念，4001 只是本平台与现场串口服务器的约定，
	// 与 IEC 101/DL/T 645 等串口协议保持一致。
	defaultPort = "4001"

	// defaultMasterMAC 是本机默认主站地址。0 是标准推荐的第一个主站地址。
	defaultMasterMAC = 0

	// presilence 是每次发送前的线路静默时间。
	//
	// RS-485 收发器从接收切到发送需要时间，主站与从站之间也要留出
	// Tturnaround。设得太短会在长线/多分支现场表现为「偶发首字节丢失」，
	// 而首字节恰好是 0x55，丢了之后整帧被当成噪声——症状是随机超时。
	presilence = 5 * time.Millisecond

	// pollTimeout 是寻找下一个主站时，单个 PollForMaster 的等待上限。
	// 它必须远小于设备超时：扫 127 个地址不能把一轮采集拖成几分钟。
	pollTimeout = 100 * time.Millisecond
)

// Dialer 打开底层链路。默认走 link.Open（串口共享 + 总线锁 / TCP 透传）。
type Dialer func(ctx context.Context, t model.Transport, timeout time.Duration, devID string) (link.Link, error)

// Driver 是 BACnet MS/TP 主站驱动。
type Driver struct {
	dial Dialer
	// retries 是一帧在超时后额外重试的次数。MS/TP 有 CRC 与 ACK 机制，
	// 但 RS-485 上的干扰通常是整帧丢失；重试 2 次足以覆盖偶发噪声，
	// 再多只会让「设备掉线」被拖成一轮采集迟迟不结束。
	retries int
}

// New 返回使用真实串口/TCP 的驱动。
func New() *Driver {
	return &Driver{
		dial: func(ctx context.Context, t model.Transport, timeout time.Duration, devID string) (link.Link, error) {
			return link.Open(ctx, link.Options{
				Proto:       "bacnet-mstp",
				DefaultPort: defaultPort,
			}, t, timeout, devID)
		},
		retries: 2,
	}
}

// NewWithDialer 返回注入链路的驱动（测试用）。
func NewWithDialer(d Dialer) *Driver {
	drv := New()
	drv.dial = d
	return drv
}

// Meta 声明 BACnet MS/TP 能力。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "bacnet-mstp",
		Name:         "BACnet MS/TP",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleMaster},
		Transports:   []string{"serial", "tcp"},
		AddressKinds: []model.AddressKind{model.AddressObject},
		Capabilities: driver.Capabilities{
			Read:        driver.CapSupported,
			Write:       driver.CapSupported,
			Subscribe:   driver.CapUnsupported,
			Browse:      driver.CapUnsupported,
			Discovery:   driver.CapUnsupported,
			MultiRead:   driver.CapUnsupported,
			BitAccess:   driver.CapUnsupported,
			StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "transport.address", Label: "串口/地址", Type: "string", Required: true,
				Placeholder: "/dev/ttyUSB0 或 192.168.1.30:4001", Scope: "channel",
				Help: "kind=serial 时是串口设备路径；kind=tcp 时是串口服务器地址，省略端口按 4001"},
			{Name: "labels.address", Label: "从站 MAC", Type: "int", Required: true, Scope: "device",
				Placeholder: "128", Help: "从站地址 128..254；0..127 是主站地址段，不能作为轮询目标"},
			{Name: "labels.master", Label: "本机主站 MAC", Type: "int", Default: 0, Scope: "device",
				Help: "本机在 MS/TP 总线上的主站地址 0..127；一条总线只能有一个主站"},
			{Name: "labels.nextStation", Label: "下一个主站", Type: "int", Scope: "device",
				Help: "可选。被动收到 Token 时要交还的下一站地址 0..127；留空时按 PollForMaster 现场探测"},
			{Name: "labels.presilence", Label: "发送前静默", Type: "duration", Default: "5ms", Scope: "device",
				Help: "每次发送前等待的线路静默时间；长线/慢速收发器可适当加大"},
			{Name: "concurrency", Label: "并发上限", Type: "int", Default: 1, Scope: "channel",
				Help: "MS/TP 是半双工总线，事务必须串行；必须为 1"},
		},
		PointFields: []driver.ConfigField{
			{Name: "address.objectType", Label: "对象类型", Type: "string", Required: true, Scope: "point",
				Placeholder: "analog-input",
				Help:        "analog-input / analog-output / analog-value / binary-input / binary-value / multi-state-value / device，也可写对象类型编号"},
			{Name: "address.instance", Label: "实例号", Type: "int", Required: true, Scope: "point",
				Placeholder: "1", Help: "对象实例号 0..4194302"},
			{Name: "address.property", Label: "属性", Type: "string", Required: true, Scope: "point",
				Placeholder: "present-value",
				Help:        "present-value / object-name / units / priority-array 等，也可写属性编号"},
			{Name: "address.propIndex", Label: "数组下标", Type: "int", Default: 0, Scope: "point",
				Help: "读取数组属性时的下标；0 表示不带下标（读整个属性）"},
		},
		Description: "BACnet MS/TP 主站：串行总线单主站轮询从站对象（ReadProperty / WriteProperty）",
	}
}

// Open 建立链路并校验寻址参数。
func (d *Driver) Open(ctx context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	t := cfg.Channel.Transport
	if t.Kind != "serial" && t.Kind != "tcp" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			fmt.Sprintf("bacnet-mstp requires serial or tcp transport, got %q", t.Kind), false, nil)
	}
	slave, err := slaveMAC(cfg.Device)
	if err != nil {
		return nil, err
	}
	master, err := masterMAC(cfg.Device)
	if err != nil {
		return nil, err
	}
	ns, hasNS, err := nextStation(cfg.Device)
	if err != nil {
		return nil, err
	}
	wait, err := silence(cfg.Device)
	if err != nil {
		return nil, err
	}
	l, err := d.dial(ctx, t, cfg.Channel.EffectiveTimeout(), cfg.Device.ID)
	if err != nil {
		return nil, err
	}
	return &connState{
		cfg:        cfg,
		bus:        l,
		master:     master,
		slave:      slave,
		next:       ns,
		hasNext:    hasNS,
		presilence: wait,
		retries:    d.retries,
	}, nil
}

// slaveMAC 取从站地址。优先 labels.address，其次 device.unitId。
//
// 只接受 128..254：0..127 是主站段，把一台从站配成 3 会被总线上真正的
// 3 号主站认领，症状是「读到的值属于另一台设备」。这个错误在配置期拦下，
// 比现场调试三天便宜得多。
func slaveMAC(dev model.Device) (byte, error) {
	raw := strings.TrimSpace(dev.Labels["address"])
	if raw == "" && dev.UnitID != 0 {
		raw = strconv.Itoa(dev.UnitID)
	}
	if raw == "" {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			"device.labels.address required (MS/TP 从站 MAC 128..254)", false, nil)
	}
	n, err := parseMAC(raw)
	if err != nil {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("invalid MS/TP slave address %q: %v", raw, err), false, err)
	}
	if !bacnetcodec.IsSlaveMAC(n) {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("MS/TP slave address %d is in master range 0..127; slaves must use 128..254", n), false, nil)
	}
	return n, nil
}

// masterMAC 取本机主站地址，默认 0。
func masterMAC(dev model.Device) (byte, error) {
	raw := strings.TrimSpace(dev.Labels["master"])
	if raw == "" {
		return defaultMasterMAC, nil
	}
	n, err := parseMAC(raw)
	if err != nil {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("invalid MS/TP master address %q: %v", raw, err), false, err)
	}
	if !bacnetcodec.IsMasterMAC(n) {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("MS/TP master address %d out of range 0..127", n), false, nil)
	}
	return n, nil
}

// nextStation 取可选的下一主站地址。
func nextStation(dev model.Device) (byte, bool, error) {
	raw := strings.TrimSpace(dev.Labels["nextStation"])
	if raw == "" {
		return 0, false, nil
	}
	n, err := parseMAC(raw)
	if err != nil {
		return 0, false, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("invalid MS/TP next station %q: %v", raw, err), false, err)
	}
	if !bacnetcodec.IsMasterMAC(n) {
		return 0, false, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("MS/TP next station %d out of range 0..127", n), false, nil)
	}
	return n, true, nil
}

// silence 解析发送前静默时间，默认 presilence。
func silence(dev model.Device) (time.Duration, error) {
	raw := strings.TrimSpace(dev.Labels["presilence"])
	if raw == "" {
		return presilence, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, driver.E(driver.ErrUnsupported, "open", dev.ID,
			fmt.Sprintf("invalid labels.presilence %q (want duration like 5ms)", raw), false, err)
	}
	return d, nil
}

func parseMAC(s string) (byte, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 0, 16)
	if err != nil {
		return 0, err
	}
	if n > 0xFF {
		return 0, fmt.Errorf("MAC %d exceeds 255", n)
	}
	return byte(n), nil
}

// connState 是一条设备连接。同一条串口上的多台从站各自持有一个 connState，
// 但通过 link/serialport 共享同一个 fd 与一把总线锁。
type connState struct {
	cfg driver.OpenConfig
	bus link.Link

	master byte
	slave  byte

	next    byte
	hasNext bool

	presilence time.Duration
	retries    int

	// mu 串行化本连接上的事务。总线锁已经跨设备串行，这里再锁一层是因为
	// invoke ID 是连接级状态；同一连接上交错两个事务会让应答错配。
	mu     sync.Mutex
	invoke byte
	// scan 是跨事务保留的字节扫描器：半帧（例如上一次超时后迟到的一帧）
	// 必须留在缓冲里继续凑，而不是每轮丢弃——丢弃会让「总线上有一帧正在
	// 路上」变成永远读不到。
	scan *frameScanner
}

func (c *connState) Close() error { return c.bus.Close() }

// nextInvoke 返回下一个非零 invoke ID（1..255 循环）。
func (c *connState) nextInvoke() byte {
	c.invoke++
	if c.invoke == 0 {
		c.invoke = 1
	}
	return c.invoke
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

// Probe 读 Device 对象的 object-name 验证「总线 + 从站 + BACnet 层」三段链路。
//
// 与 BACnet/IP 的探测一样，实例号猜 1：返回 ComplexACK 说明全通；返回
// Error/Reject/Abort 说明帧能到设备、只是没有 device:1，同样算链路可用。
// 只有超时与非法报文才算失败。
func (c *connState) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	invoke := c.nextInvoke()
	req, err := bacnetcodec.ReadPropertyRequest(invoke, bacnetcodec.ObjectDevice, 1,
		bacnetcodec.PropObjectName, 0)
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, invoke, "probe")
	if err != nil {
		return err
	}
	switch bacnetcodec.ClassifyAPDU(resp[0]) {
	case bacnetcodec.APDUKindComplexACK, bacnetcodec.APDUKindError,
		bacnetcodec.APDUKindReject, bacnetcodec.APDUKindAbort:
		return nil
	}
	return driver.E(driver.ErrProtocol, "probe", c.cfg.Device.ID,
		fmt.Sprintf("unexpected APDU 0x%02X for probe", resp[0]), false, nil)
}

// ---------------------------------------------------------------------------
// 分帧读取
// ---------------------------------------------------------------------------

// frameScanner 在字节流里切帧。
//
// 不能假设「下一次 ReadFull 正好从 preamble 开始」：RS-485 上电、总线冲突、
// 波特率不匹配、上一帧被截断都会留下半截字节。因此扫描器持有一个滚动
// 缓冲，逐字节寻找 0x55 0xFF；任何一层校验失败只丢弃**一个**字节再重扫，
// 而不是丢掉整段候选头——后者会把紧跟在假 preamble 后面的真帧一起吞掉，
// 症状是「偶发丢一帧」，在 1s 采集周期里看起来就是随机超时。
type frameScanner struct {
	st  transport.Stream
	buf []byte
}

func newFrameScanner(st transport.Stream) *frameScanner {
	return &frameScanner{st: st}
}

// next 返回下一帧。
func (s *frameScanner) next() (bacnetcodec.MSTPFrame, error) {
	for {
		if len(s.buf) >= bacnetcodec.MSTPHeaderLen &&
			s.buf[0] == bacnetcodec.MSTPPreamble1 && s.buf[1] == bacnetcodec.MSTPPreamble2 {
			length := int(s.buf[5])<<8 | int(s.buf[6])
			if length <= bacnetcodec.MSTPMaxData && bacnetcodec.CheckHeaderCRC(s.buf[2:bacnetcodec.MSTPHeaderLen]) {
				need := bacnetcodec.MSTPHeaderLen + length + 2
				if len(s.buf) < need {
					if err := s.more(need - len(s.buf)); err != nil {
						return bacnetcodec.MSTPFrame{}, err
					}
				}
				body := s.buf[bacnetcodec.MSTPHeaderLen:need]
				if bacnetcodec.CheckDataCRC(body) {
					frame := bacnetcodec.MSTPFrame{
						Type: s.buf[2], Dest: s.buf[3], Src: s.buf[4],
						Data: append([]byte(nil), body[:length]...),
					}
					s.drop(need)
					return frame, nil
				}
			}
		}
		if len(s.buf) >= bacnetcodec.MSTPHeaderLen {
			// 头不成立（preamble 对但 CRC 错、或长度超限）：丢一字节重扫。
			s.drop(1)
			continue
		}
		if err := s.more(1); err != nil {
			return bacnetcodec.MSTPFrame{}, err
		}
	}
}

// more 至少再读 n 字节。
func (s *frameScanner) more(n int) error {
	if n < 1 {
		n = 1
	}
	got, err := s.st.ReadFull(n)
	if err != nil {
		return err
	}
	s.buf = append(s.buf, got...)
	return nil
}

// drop 丢弃前 n 字节。用 copy 而不是 reslice：reslice 会让底层数组的偏移
// 不断增长，长时间运行时 append 会周期性重新分配并拷贝整个缓冲。
func (s *frameScanner) drop(n int) {
	if n >= len(s.buf) {
		s.buf = s.buf[:0]
		return
	}
	rest := copy(s.buf, s.buf[n:])
	s.buf = s.buf[:rest]
}

// readFrame 从总线读下一帧。
func (c *connState) readFrame() (bacnetcodec.MSTPFrame, error) {
	if c.scan == nil {
		c.scan = newFrameScanner(c.bus)
	}
	return c.scan.next()
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// exchange 发送一条 APDU 并等待 invoke ID 相同的应答。
func (c *connState) exchange(ctx context.Context, apdu []byte, invoke byte, op string) ([]byte, error) {
	npdu := bacnetcodec.BuildNPDU(1, bacnetcodec.NPDUControlExpectReply, apdu)
	wire, err := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FrameDataExpectingReply,
		Dest: c.slave, Src: c.master, Data: npdu,
	})
	if err != nil {
		return nil, driver.E(driver.ErrInternal, op, c.cfg.Device.ID, err.Error(), false, err)
	}

	// 总线锁覆盖「排空 → 发送 → 收完应答」整段：同一条 RS-485 上还有别的
	// 从站连接在使用同一个 fd，交错会让两边的字节互相污染。
	c.bus.Lock()
	defer c.bus.Unlock()

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, err)
		}
		sawToken, resp, err := c.attempt(ctx, wire, invoke, op)
		if err == nil {
			if sawToken {
				c.passToken(ctx)
			}
			return resp, nil
		}
		lastErr = err
		if sawToken {
			// 已经把令牌还回去了，就不再重试：在令牌环里没拿到令牌就发帧
			// 会撞上别人的发送窗口。
			c.passToken(ctx)
			return nil, err
		}
		if driver.KindOf(err) != driver.ErrTimeout || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt 是一轮「发一帧 + 收一帧」。返回值 sawToken 表示窗口内收到过
// 发给本机的 Token（调用方负责在事务结束后交还）。
func (c *connState) attempt(ctx context.Context, wire []byte, invoke byte, op string) (bool, []byte, error) {
	// 半双工纪律：发之前清掉接收缓冲，避免把上一轮迟到的应答/令牌当成
	// 本轮的开头。串口服务器无法真正 flush，只能读掉已到达的字节；帧同步
	// 由 readFrame 的扫描兜底。
	//
	// 顺序很重要：Drain 会临时把 deadline 压到「立刻」，之后必须重新
	// SetDeadline，否则本次写会立刻超时（tcpLink.Drain 不恢复原值）。
	if f, ok := c.bus.(transport.Flusher); ok {
		_ = f.Drain()
	}
	if err := c.setDeadline(ctx); err != nil {
		return false, nil, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
			"set link deadline", true, err)
	}
	if err := c.wait(ctx); err != nil {
		return false, nil, err
	}
	if err := c.bus.Write(wire); err != nil {
		return false, nil, driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID,
			"bus write failed", true, err)
	}

	sawToken := false
	for {
		frame, err := c.readFrame()
		if err != nil {
			return sawToken, nil, c.readError(op, err, ctx)
		}
		switch frame.Type {
		case bacnetcodec.FrameToken:
			if frame.Dest == c.master {
				sawToken = true
			}
		case bacnetcodec.FramePollForMaster:
			if frame.Dest == c.master {
				_ = c.sendSimple(ctx, bacnetcodec.FrameReplyToPollForMaster, frame.Src)
			}
		case bacnetcodec.FrameTestRequest:
			if frame.Dest == c.master || frame.Dest == bacnetcodec.MSTPBroadcast {
				_ = c.sendSimple(ctx, bacnetcodec.FrameTestResponse, frame.Src)
			}
		case bacnetcodec.FrameTestResponse:
			// 别人对别人测试的应答，忽略。
		case bacnetcodec.FrameReplyPostponed:
			if frame.Src == c.slave && frame.Dest == c.master {
				return sawToken, nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"slave postponed its reply (ReplyPostponed is not consumed by this driver)", false, nil)
			}
		case bacnetcodec.FrameDataExpectingReply, bacnetcodec.FrameDataNotExpectingReply:
			if frame.Src != c.slave || frame.Dest != c.master {
				continue
			}
			resp, _, perr := bacnetcodec.ParseNPDU(frame.Data)
			if perr != nil {
				return sawToken, nil, driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
					"malformed MS/TP NPDU: "+perr.Error(), false, perr)
			}
			// 迟到/重发的应答 invoke ID 不匹配，丢掉继续等。
			if id, ok := bacnetcodec.AcknowledgeInvoke(resp); ok && id == invoke {
				return sawToken, resp, nil
			}
		}
	}
}

// sendSimple 发送一个不带数据的帧（TestResponse / ReplyToPollForMaster /
// Token）。
func (c *connState) sendSimple(ctx context.Context, typ, dest byte) error {
	wire, err := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: typ, Dest: dest, Src: c.master,
	})
	if err != nil {
		return err
	}
	if err := c.setDeadline(ctx); err != nil {
		return err
	}
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.bus.Write(wire)
}

// passToken 把令牌交给下一个主站。
//
// 令牌只在「被动收到」时才会出现。交还目标优先用 labels.nextStation；
// 没配就现场扫：从本机地址之后按环形顺序发 PollForMaster，第一个回
// ReplyToPollForMaster 的地址就是下一站。扫不到说明总线上只有本机一个
// 主站，此时保留令牌即可——这正是 sole-master 的稳态。
func (c *connState) passToken(ctx context.Context) {
	if c.hasNext {
		if c.next != c.master {
			_ = c.sendSimple(ctx, bacnetcodec.FrameToken, c.next)
		}
		return
	}
	for i := 1; i <= 127; i++ {
		mac := byte((int(c.master) + i) % 128)
		if mac == c.master {
			continue
		}
		if c.pollMaster(ctx, mac) {
			_ = c.sendSimple(ctx, bacnetcodec.FrameToken, mac)
			return
		}
	}
}

// pollMaster 问一个主站地址是否在线，并等它的 ReplyToPollForMaster。
// 每个地址的等待被限制在 pollTimeout，避免扫完 127 个地址花掉一整轮采集。
func (c *connState) pollMaster(ctx context.Context, mac byte) bool {
	wire, err := bacnetcodec.BuildMSTPFrame(bacnetcodec.MSTPFrame{
		Type: bacnetcodec.FramePollForMaster, Dest: mac, Src: c.master,
	})
	if err != nil {
		return false
	}
	dl := time.Now().Add(pollTimeout)
	if ctxDL, ok := ctx.Deadline(); ok && ctxDL.Before(dl) {
		dl = ctxDL
	}
	if err := c.bus.SetDeadline(dl); err != nil {
		return false
	}
	if err := c.wait(ctx); err != nil {
		return false
	}
	if err := c.bus.Write(wire); err != nil {
		return false
	}
	for {
		frame, err := c.readFrame()
		if err != nil {
			return false
		}
		if frame.Type == bacnetcodec.FrameReplyToPollForMaster && frame.Src == mac && frame.Dest == c.master {
			return true
		}
		if frame.Type == bacnetcodec.FrameTestRequest && frame.Dest == c.master {
			_ = c.sendSimple(ctx, bacnetcodec.FrameTestResponse, frame.Src)
		}
	}
}

// wait 做发送前静默。ctx 先到期时返回超时错误，而不是继续发帧。
func (c *connState) wait(ctx context.Context) error {
	if c.presilence <= 0 {
		return nil
	}
	t := time.NewTimer(c.presilence)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return driver.E(driver.ErrTimeout, "bus", c.cfg.Device.ID, "context done", true, ctx.Err())
	case <-t.C:
		return nil
	}
}

func (c *connState) setDeadline(ctx context.Context) error {
	return c.bus.SetDeadline(c.attemptDeadline(ctx))
}

func (c *connState) attemptDeadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.cfg.Channel.EffectiveTimeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		return dl
	}
	return d
}

func (c *connState) readError(op string, err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID, "context done", true, ctxErr)
	}
	if isTimeout(err) {
		return driver.E(driver.ErrTimeout, op, c.cfg.Device.ID,
			"no response within timeout", true, err)
	}
	return driver.E(driver.ErrDisconnected, op, c.cfg.Device.ID, "bus read failed", true, err)
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

type pointPlan struct {
	ref      driver.PointRef
	objType  int
	instance int
	property int
	index    int
}

func planPoint(p driver.PointRef) (pointPlan, error) {
	if p.Address.Kind != model.AddressObject {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: bacnet-mstp requires object addressing, got %q", p.ID, p.Address.Kind), false, nil)
	}
	objType, ok := bacnetcodec.ObjectTypeFromName(p.Address.ObjectType)
	if !ok {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: unknown object type %q (examples: %s)", p.ID, p.Address.ObjectType,
				strings.Join(bacnetcodec.ObjectTypeNames(), ", ")), false, nil)
	}
	if p.Address.Instance < 0 || p.Address.Instance > 0x3FFFFF {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: instance %d out of range 0..4194302", p.ID, p.Address.Instance), false, nil)
	}
	property, ok := bacnetcodec.PropertyFromName(p.Address.Property)
	if !ok {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: unknown property %q (examples: %s)", p.ID, p.Address.Property,
				strings.Join(bacnetcodec.PropertyNames(), ", ")), false, nil)
	}
	if p.Address.PropIndex < 0 {
		return pointPlan{}, driver.E(driver.ErrAddress, "read", "",
			fmt.Sprintf("point %q: propIndex must be >= 0", p.ID), false, nil)
	}
	return pointPlan{
		ref: p, objType: objType, instance: p.Address.Instance,
		property: property, index: p.Address.PropIndex,
	}, nil
}

func (c *connState) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var points []driver.PointRef
	for _, span := range spans {
		points = append(points, span.Points...)
	}
	out := make([]driver.RawSample, 0, len(points))
	var firstErr error
	okSamples := 0
	for i, p := range points {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.cfg.Device.ID, "context done", true, err)
			for _, rest := range points[i:] {
				out = append(out, errSample(rest.ID, e))
			}
			// 上下文取消是链路级超时：立即返回，不走下面的 okSamples 闸门——
			// 一轮采集被周期超时打断本身就是「设备太慢」的信号，需要让调度器看到。
			return out, e
		}
		plan, err := planPoint(p)
		if err != nil {
			out = append(out, errSample(p.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		v, err := c.readOne(ctx, plan)
		if err != nil {
			out = append(out, errSample(p.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			// 链路级失败（超时/断连）时剩下的点不可能成功；立即返回，同样
			// 不走 okSamples 闸门。总线上重试留给下一轮调度。
			if driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected {
				for _, rest := range points[i+1:] {
					out = append(out, errSample(rest.ID, err))
				}
				return out, err
			}
			continue
		}
		okSamples++
		out = append(out, driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood})
	}
	// 部分失败语义（docs §3.7.4 / §3.9.5）：只要有任意一项成功，`Read` 就不
	// 向上返回错误（好样本必须保留）；全部点位失败才返回第一个错误，让设备
	// 进入故障/退避。链路级错误在上面已提前返回。
	if okSamples == 0 && firstErr != nil {
		return out, firstErr
	}
	return out, nil
}

func (c *connState) readOne(ctx context.Context, plan pointPlan) (model.Value, error) {
	invoke := c.nextInvoke()
	req, err := bacnetcodec.ReadPropertyRequest(invoke, plan.objType, plan.instance,
		plan.property, plan.index)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrInternal, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	resp, err := c.exchange(ctx, req, invoke, "read")
	if err != nil {
		return model.Value{}, err
	}
	if bacnetcodec.ClassifyAPDU(resp[0]) != bacnetcodec.APDUKindComplexACK {
		return model.Value{}, c.apduError("read", resp, plan)
	}
	kind, service, _, payload, err := bacnetcodec.ParseACK(resp)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID, err.Error(), false, err)
	}
	if kind != bacnetcodec.APDUKindComplexACK || service != bacnetcodec.ServiceReadProperty {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			fmt.Sprintf("unexpected ACK service 0x%02X", service), false, nil)
	}
	appValue, has, err := bacnetcodec.ReadPropertyACK(payload)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"malformed ReadProperty-ACK: "+err.Error(), false, err)
	}
	if !has {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"ReadProperty-ACK carries no property value", false, nil)
	}
	tag, u, f, b, bl, err := bacnetcodec.DecodeApplicationValue(appValue)
	if err != nil {
		return model.Value{}, driver.E(driver.ErrProtocol, "read", c.cfg.Device.ID,
			"decode property value: "+err.Error(), false, err)
	}
	return toValue(plan, tag, u, f, b, bl)
}

// toValue 把 BACnet 应用值转成平台值。规则与 BACnet/IP 驱动一致：
// 数值类型宽松（REAL/UNSIGNED/SIGNED 都收），bool/string 严格。
func toValue(plan pointPlan, tag byte, u uint64, f float64, b []byte, bl bool) (model.Value, error) {
	switch plan.ref.DataType {
	case model.TypeBool:
		switch tag {
		case bacnetcodec.AppBoolean:
			return model.BoolValue(bl), nil
		case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated, bacnetcodec.AppSigned:
			return model.BoolValue(u != 0), nil
		}
		return model.Value{}, typeError(plan, tag, "bool")
	case model.TypeString:
		if tag != bacnetcodec.AppCharacterStr {
			return model.Value{}, typeError(plan, tag, "string")
		}
		return model.TextValue(model.TypeString, string(b)), nil
	case model.TypeBytes:
		switch tag {
		case bacnetcodec.AppOctetString, bacnetcodec.AppBitString, bacnetcodec.AppCharacterStr:
			return model.BytesValue(b), nil
		}
		return model.Value{}, typeError(plan, tag, "bytes")
	}

	switch tag {
	case bacnetcodec.AppReal, bacnetcodec.AppDouble:
		return model.NumValue(plan.ref.DataType, f), nil
	case bacnetcodec.AppUnsigned, bacnetcodec.AppEnumerated:
		return model.NumValue(plan.ref.DataType, float64(u)), nil
	case bacnetcodec.AppSigned:
		return model.NumValue(plan.ref.DataType, float64(int64(u))), nil
	}
	return model.Value{}, typeError(plan, tag, string(plan.ref.DataType))
}

func typeError(plan pointPlan, tag byte, want string) error {
	return driver.E(driver.ErrAddress, "decode", "",
		fmt.Sprintf("point %q (%s): device returned application tag %d, configured as %s",
			plan.ref.ID, bacnetcodec.FormatAddress(plan.objType, plan.instance, plan.property, plan.index),
			tag, want), false, nil)
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

func (c *connState) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for _, req := range reqs {
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if err != nil && (driver.KindOf(err) == driver.ErrTimeout || driver.KindOf(err) == driver.ErrDisconnected) {
			for _, rest := range reqs[len(results):] {
				results = append(results, writeFail(rest, err))
			}
			break
		}
	}
	return results, firstErr
}

func (c *connState) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	plan, err := planPoint(driver.PointRef{
		ID: req.PointID, Address: req.Address, DataType: req.DataType, ByteOrder: req.ByteOrder,
	})
	if err != nil {
		return writeFail(req, err), err
	}
	appValue, err := encodeWriteValue(plan, req)
	if err != nil {
		return writeFail(req, err), err
	}
	invoke := c.nextInvoke()
	apdu, err := bacnetcodec.WritePropertyRequest(invoke, plan.objType, plan.instance,
		plan.property, plan.index, 0, appValue)
	if err != nil {
		e := driver.E(driver.ErrInternal, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, e), e
	}
	resp, err := c.exchange(ctx, apdu, invoke, "write")
	if err != nil {
		return writeFail(req, err), err
	}
	if bacnetcodec.ClassifyAPDU(resp[0]) != bacnetcodec.APDUKindSimpleACK {
		e := c.apduError("write", resp, plan)
		return writeFail(req, e), e
	}
	kind, service, _, _, err := bacnetcodec.ParseACK(resp)
	if err != nil {
		e := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID, err.Error(), false, err)
		return writeFail(req, e), e
	}
	if kind != bacnetcodec.APDUKindSimpleACK || service != bacnetcodec.ServiceWriteProperty {
		e := driver.E(driver.ErrProtocol, "write", c.cfg.Device.ID,
			fmt.Sprintf("unexpected ACK service 0x%02X", service), false, nil)
		return writeFail(req, e), e
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// encodeWriteValue 按对象语义选择应用值编码，规则与 BACnet/IP 驱动相同：
// present-value 的类型由对象决定（模拟量 REAL、二进制量 ENUMERATED），
// 其它属性才按平台 dataType 推断。
func encodeWriteValue(plan pointPlan, req driver.WriteRequest) ([]byte, error) {
	kind := bacnetcodec.KindOfObject(plan.objType)
	var tag byte = bacnetcodec.AppReal
	if plan.property == bacnetcodec.PropPresentValue {
		switch kind {
		case bacnetcodec.KindAnalog:
			tag = bacnetcodec.AppReal
		case bacnetcodec.KindBinary:
			tag = bacnetcodec.AppEnumerated
		case bacnetcodec.KindMultiState:
			tag = bacnetcodec.AppUnsigned
		case bacnetcodec.KindString:
			tag = bacnetcodec.AppCharacterStr
		}
	} else {
		switch req.DataType {
		case model.TypeBool:
			tag = bacnetcodec.AppBoolean
		case model.TypeString:
			tag = bacnetcodec.AppCharacterStr
		case model.TypeBytes:
			tag = bacnetcodec.AppOctetString
		case model.TypeFloat64:
			tag = bacnetcodec.AppDouble
		}
	}

	var (
		u   uint64
		f   float64
		b   []byte
		bl  bool
		err error
	)
	switch tag {
	case bacnetcodec.AppBoolean:
		if req.DataType != model.TypeBool {
			err = fmt.Errorf("point %q: boolean property requires bool value", req.PointID)
		}
		bl = req.Value.Bool
	case bacnetcodec.AppCharacterStr:
		if req.DataType != model.TypeString && req.DataType != model.TypeBytes {
			err = fmt.Errorf("point %q: character string property requires string value", req.PointID)
		}
		b = []byte(req.Value.Text)
	case bacnetcodec.AppOctetString:
		if req.DataType != model.TypeBytes && req.DataType != model.TypeString {
			err = fmt.Errorf("point %q: octet string property requires bytes value", req.PointID)
		}
		b = []byte(req.Value.Text)
	case bacnetcodec.AppEnumerated, bacnetcodec.AppUnsigned:
		n, ok := req.Value.AsFloat()
		if !ok || n < 0 || n > float64(^uint32(0)) {
			err = fmt.Errorf("point %q: enumerated property requires a non-negative integer, got %s", req.PointID, req.Value.String())
		} else {
			u = uint64(n)
		}
	case bacnetcodec.AppDouble:
		n, ok := req.Value.AsFloat()
		if !ok {
			err = fmt.Errorf("point %q: numeric property requires a number, got %s", req.PointID, req.Value.String())
		} else {
			f = n
		}
	default: // AppReal
		n, ok := req.Value.AsFloat()
		if !ok {
			err = fmt.Errorf("point %q: numeric property requires a number, got %s", req.PointID, req.Value.String())
		} else {
			f = n
		}
	}
	if err != nil {
		e := driver.E(driver.ErrAddress, "write", "", err.Error(), false, nil)
		return nil, e
	}
	out, err := bacnetcodec.EncodeApplicationValue(tag, u, f, b, bl)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "write", "",
			fmt.Sprintf("point %q: %v", req.PointID, err), false, err)
	}
	return out, nil
}

func writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: qualityOf(err), Err: err}
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func (c *connState) apduError(op string, apdu []byte, plan pointPlan) error {
	switch bacnetcodec.ClassifyAPDU(apdu[0]) {
	case bacnetcodec.APDUKindError:
		_, class, code, err := bacnetcodec.ParseError(apdu)
		if err != nil {
			return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
				"malformed Error APDU: "+err.Error(), false, err)
		}
		return mapBacnetError(op, c.cfg.Device.ID, plan, class, code)
	case bacnetcodec.APDUKindReject:
		_, reason, _ := bacnetcodec.ParseReject(apdu)
		return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
			fmt.Sprintf("device rejected request (reason 0x%02X)", reason), false, nil)
	case bacnetcodec.APDUKindAbort:
		_, reason, _ := bacnetcodec.ParseAbort(apdu)
		return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
			fmt.Sprintf("device aborted transaction (reason 0x%02X)", reason), false, nil)
	}
	return driver.E(driver.ErrProtocol, op, c.cfg.Device.ID,
		fmt.Sprintf("unexpected APDU 0x%02X", apdu[0]), false, nil)
}

func mapBacnetError(op, device string, plan pointPlan, class, code byte) error {
	addr := bacnetcodec.FormatAddress(plan.objType, plan.instance, plan.property, plan.index)
	msg := fmt.Sprintf("%s: device error class %d code %d", addr, class, code)
	switch class {
	case bacnetcodec.ErrorClassObject:
		return driver.E(driver.ErrAddress, op, device, msg, false, nil)
	case bacnetcodec.ErrorClassProperty:
		switch code {
		case bacnetcodec.ErrorCodeUnknownProperty, bacnetcodec.ErrorCodeInvalidArrayIndex:
			return driver.E(driver.ErrAddress, op, device, msg, false, nil)
		case bacnetcodec.ErrorCodeWriteAccessDenied, bacnetcodec.ErrorCodeReadAccessDenied:
			return driver.E(driver.ErrPermission, op, device, msg, false, nil)
		case bacnetcodec.ErrorCodeValueOutOfRange, bacnetcodec.ErrorCodeInvalidDataType:
			return driver.E(driver.ErrAddress, op, device, msg, false, nil)
		}
	case bacnetcodec.ErrorClassResources:
		return driver.E(driver.ErrDisconnected, op, device, msg, true, nil)
	case bacnetcodec.ErrorClassSecurity:
		return driver.E(driver.ErrPermission, op, device, msg, false, nil)
	}
	return driver.E(driver.ErrProtocol, op, device, msg, false, nil)
}

func errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: qualityOf(err), Err: err}
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}
