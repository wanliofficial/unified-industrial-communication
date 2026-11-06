package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"time"

	mb "uic/internal/codec/modbus"
	"uic/internal/transport"
)

// mbread 是一个极小的 Modbus 主站，用来在没有第三方工具的情况下
// 验证从站（内置、真实设备或本进程的串口从站）。
//
// 存在的理由：现场最缺的不是「功能」，而是「不依赖任何外部安装即可自证
// 链路通」的手段。用 curl 验不了 Modbus，用商业工具又要求先装软件、再
// 手工点几十下；一个 30 行的子命令可以把这件事变成一行可复制的命令，
// 也让故障排查过程可以贴进工单。
//
// 边界：只做单次读写，不做轮询、不做重试、不解析设备型号。
func mbreadCmd(args []string) {
	fs := flag.NewFlagSet("mbread", flag.ExitOnError)
	tr := fs.String("transport", "tcp", "传输：tcp | rtu | ascii")
	addr := fs.String("addr", "127.0.0.1:1502", "从站地址 host:port（tcp）")
	port := fs.String("port", "", "串口设备路径，如 /dev/ttyUSB0（rtu/ascii）")
	baud := fs.Int("baud", 9600, "波特率（rtu/ascii）")
	dataBits := fs.Int("dataBits", 8, "数据位 5..8（rtu/ascii）")
	parity := fs.String("parity", "none", "校验：none | even | odd（rtu/ascii）")
	stopBits := fs.Int("stopBits", 1, "停止位 1|2（rtu/ascii）")
	unit := fs.Int("unit", 1, "从站单元号 0..247")
	area := fs.String("area", "holding", "区：holding | input | coil | discrete")
	offset := fs.Int("offset", 0, "起始地址")
	qty := fs.Int("qty", 1, "读取数量（寄存器或位）")
	write := fs.String("write", "", "写入值：寄存器区为十进制/0x 十六进制；线圈为 on|off|1|0")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	fs.Parse(args)

	if *unit < 0 || *unit > 247 {
		fmt.Fprintln(os.Stderr, "unit 必须在 0..247 之间")
		os.Exit(2)
	}

	bus, err := openBus(*tr, *addr, *port, transport.SerialConfig{
		Port: *port, Baud: *baud, DataBits: *dataBits,
		Parity: *parity, StopBits: *stopBits,
	}, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer bus.Close()

	if *write != "" {
		runWrite(bus, *area, *unit, *offset, *write)
		return
	}
	runRead(bus, *area, *unit, *offset, *qty)
}

// mbBus 抽象「一条能承载一次 Modbus 事务的链路」。
//
// 用接口而不是给每个函数加 if transport == ...：TCP 与串口的差别集中在
// **组帧与收帧**，读写解析、输出格式、错误文案应当完全一致——否则同一
// 条命令在两种传输下的输出会悄悄分叉，排障时最容易看错。
type mbBus interface {
	Do(unit int, pdu []byte) ([]byte, error)
	Close() error
}

func openBus(kind, addr, port string, serial transport.SerialConfig, timeout time.Duration) (mbBus, error) {
	switch strings.ToLower(kind) {
	case "tcp":
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
		return &tcpBus{conn: conn, timeout: timeout}, nil
	case "rtu", "ascii":
		if port == "" {
			return nil, fmt.Errorf("%s 传输需要 -port 指定串口设备", kind)
		}
		st, err := transport.OpenSerial(serial)
		if err != nil {
			return nil, fmt.Errorf("打开串口 %s 失败: %w", port, err)
		}
		b := &serialBus{st: st, framing: strings.ToLower(kind), timeout: timeout}
		if d, ok := st.(transport.DeadlineSetter); ok {
			_ = d.SetDeadline(time.Now().Add(timeout))
		}
		return b, nil
	}
	return nil, fmt.Errorf("未知传输 %q（tcp|rtu|ascii）", kind)
}

// ---------------------------------------------------------------------------
// TCP
// ---------------------------------------------------------------------------

type tcpBus struct {
	conn    net.Conn
	tx      uint16
	timeout time.Duration
}

func (b *tcpBus) Do(unit int, pdu []byte) ([]byte, error) {
	b.tx++
	tx := b.tx
	_ = b.conn.SetDeadline(time.Now().Add(b.timeout))
	frame := mb.BuildTCP(mb.Frame{Transaction: tx, Unit: uint8(unit), PDU: pdu})
	if _, err := b.conn.Write(frame); err != nil {
		return nil, fmt.Errorf("发送: %w", err)
	}
	head := make([]byte, 7)
	if _, err := io.ReadFull(b.conn, head); err != nil {
		return nil, fmt.Errorf("读取响应头: %w", err)
	}
	if binary.BigEndian.Uint16(head[0:2]) != tx {
		return nil, fmt.Errorf("事务号不匹配: 期望 %d，收到 %d",
			tx, binary.BigEndian.Uint16(head[0:2]))
	}
	n := int(binary.BigEndian.Uint16(head[4:6]))
	if n < 2 || n > 254 {
		return nil, fmt.Errorf("非法长度字段 %d", n)
	}
	body := make([]byte, n-1)
	if _, err := io.ReadFull(b.conn, body); err != nil {
		return nil, fmt.Errorf("读取响应体: %w", err)
	}
	if len(body) >= 2 && body[0]&0x80 != 0 {
		return body, fmt.Errorf("从站异常码 0x%02X（%s）", body[1], exceptionText(body[1]))
	}
	return body, nil
}

func (b *tcpBus) Close() error { return b.conn.Close() }

// ---------------------------------------------------------------------------
// 串口（RTU / ASCII）
// ---------------------------------------------------------------------------

type serialBus struct {
	st      transport.Stream
	framing string
	timeout time.Duration
}

func (b *serialBus) Close() error { return b.st.Close() }

func (b *serialBus) Do(unit int, pdu []byte) ([]byte, error) {
	var req []byte
	if b.framing == "ascii" {
		req = mb.BuildASCII(uint8(unit), pdu)
	} else {
		req = mb.BuildRTU(uint8(unit), pdu)
	}
	// 发请求前清掉接收缓冲：上一轮的超时响应、线路噪声都会在这里变成
	// 「功能码对不上」的伪故障。总线上多一台从站时尤其常见。
	if d, ok := b.st.(interface{ Drain() error }); ok {
		_ = d.Drain()
	}
	if d, ok := b.st.(transport.DeadlineSetter); ok {
		_ = d.SetDeadline(time.Now().Add(b.timeout))
	}
	if err := b.st.Write(req); err != nil {
		return nil, fmt.Errorf("发送: %w", err)
	}

	if b.framing == "ascii" {
		return b.readASCII(pdu)
	}
	return b.readRTU(pdu)
}

// readRTU 读满一条 RTU 响应。
//
// 先读 2 字节（地址 + 功能码）判断异常帧（定长 5 字节）还是正常帧
// （长度由请求推导），再一次性读满剩余部分。字节计数被写坏的从站会在
// 读完固定长度后被 CRC 拒绝，而不是把主站挂到超时。
func (b *serialBus) readRTU(reqPDU []byte) ([]byte, error) {
	head, err := b.st.ReadFull(2)
	if err != nil {
		return nil, fmt.Errorf("读取响应（超时或链路断开，检查波特率/接线/单元号）: %w", err)
	}
	if head[1]&0x80 != 0 {
		rest, err := b.st.ReadFull(3)
		if err != nil {
			return nil, fmt.Errorf("异常响应被截断: %w", err)
		}
		pdu, err := b.finishRTU(append(head, rest...))
		if err != nil {
			return nil, err
		}
		return pdu, fmt.Errorf("从站异常码 0x%02X（%s）", pdu[1], exceptionText(pdu[1]))
	}
	want, perr := mb.RTUExpectedResponseLen(reqPDU)
	if perr != nil {
		return nil, perr
	}
	if want < 2 || want > mb.RTUMaxFrame {
		return nil, fmt.Errorf("不可信的 RTU 响应长度 %d", want)
	}
	rest, err := b.st.ReadFull(want - 2)
	if err != nil {
		return nil, fmt.Errorf("RTU 帧被截断（已读 2/%d 字节）: %w", want, err)
	}
	return b.finishRTU(append(head, rest...))
}

func (b *serialBus) finishRTU(frame []byte) ([]byte, error) {
	_, pdu, err := mb.ParseRTU(frame)
	if err != nil {
		return nil, fmt.Errorf("RTU 校验失败（% x）: %w", frame, err)
	}
	return pdu, nil
}

// readASCII 读到 CRLF 为止。ASCII 帧自带分帧符，不需要预知长度；
// 长度上限只是防止一条持续有噪声的线路把缓冲区撑爆。
func (b *serialBus) readASCII(reqPDU []byte) ([]byte, error) {
	frame := make([]byte, 0, 32)
	for len(frame) < mb.ASCIIMaxFrame {
		c, err := b.st.ReadFull(1)
		if err != nil {
			return nil, fmt.Errorf("读取响应（超时或链路断开，检查波特率/接线/单元号）: %w", err)
		}
		frame = append(frame, c[0])
		if c[0] == '\n' {
			break
		}
	}
	_, pdu, err := mb.ParseASCII(frame)
	if err != nil {
		return nil, fmt.Errorf("ASCII 校验失败（%q）: %w", string(frame), err)
	}
	if len(pdu) >= 2 && pdu[0]&0x80 != 0 {
		return pdu, fmt.Errorf("从站异常码 0x%02X（%s）", pdu[1], exceptionText(pdu[1]))
	}
	return pdu, nil
}

// ---------------------------------------------------------------------------
// 命令
// ---------------------------------------------------------------------------

func runRead(bus mbBus, area string, unit, offset, qty int) {
	fc, bits, err := areaFunc(area, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if qty < 1 {
		fmt.Fprintln(os.Stderr, "qty 必须 ≥ 1")
		os.Exit(2)
	}
	if bits && qty > 2000 {
		qty = 2000
	}
	if !bits && qty > 125 {
		qty = 125
	}
	pdu := []byte{fc, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:3], uint16(offset))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))

	resp, err := bus.Do(unit, pdu)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if len(resp) < 2 || resp[1] != byte(len(resp)-2) {
		fmt.Fprintf(os.Stderr, "响应长度不一致: % x\n", resp)
		os.Exit(1)
	}
	fmt.Printf("%s unit=%d offset=%d qty=%d\n", area, unit, offset, qty)
	if bits {
		for i := 0; i < qty; i++ {
			on := resp[2+i/8]&(1<<uint(i%8)) != 0
			fmt.Printf("[%d] %v\n", offset+i, on)
		}
		return
	}
	for i := 0; i < qty; i++ {
		raw := resp[2+i*2:]
		u := binary.BigEndian.Uint16(raw)
		fmt.Printf("[%d] %5d  0x%04X  int16=%d\n", offset+i, u, u, int16(u))
	}
	// 相邻两寄存器按大端拼 float32：这是最常见的 32 位浮点排布，
	// 用来快速判断字节序是否配反（读出来是天文数字或 0 就是反了）。
	if qty >= 2 {
		fmt.Println("float32（ABCD，成对寄存器）:")
		for i := 0; i+1 < qty; i += 2 {
			raw := resp[2+i*2:]
			bits := binary.BigEndian.Uint32(raw[:4])
			fmt.Printf("  [%d..%d] %g\n", offset+i, offset+i+1, math.Float32frombits(bits))
		}
	}
}

func runWrite(bus mbBus, area string, unit, offset int, value string) {
	fc, bits, err := areaFunc(area, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if bits {
		on := parseBool(value)
		var v uint16
		if on {
			v = 0xFF00
		}
		pdu := []byte{mb.FCWriteSingleCoil, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(pdu[1:3], uint16(offset))
		binary.BigEndian.PutUint16(pdu[3:5], v)
		resp, err := bus.Do(unit, pdu)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Printf("写入 coil[%d]=%v 成功（回显 % x）\n", offset, on, resp)
		return
	}
	v, err := parseUint16(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入值 %q: %v\n", value, err)
		os.Exit(2)
	}
	pdu := []byte{fc, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:3], uint16(offset))
	binary.BigEndian.PutUint16(pdu[3:5], v)
	resp, err := bus.Do(unit, pdu)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("写入 %s[%d]=%d (0x%04X) 成功（回显 % x）\n", area, offset, v, v, resp)
}

func areaFunc(area string, write bool) (fc byte, bits bool, err error) {
	switch strings.ToLower(area) {
	case "holding":
		if write {
			return mb.FCWriteSingleRegister, false, nil
		}
		return mb.FCReadHoldingRegs, false, nil
	case "input":
		if write {
			return 0, false, fmt.Errorf("input 区只读")
		}
		return mb.FCReadInputRegs, false, nil
	case "coil":
		if write {
			return mb.FCWriteSingleCoil, true, nil
		}
		return mb.FCReadCoils, true, nil
	case "discrete":
		if write {
			return 0, false, fmt.Errorf("discrete 区只读")
		}
		return mb.FCReadDiscreteInputs, true, nil
	}
	return 0, false, fmt.Errorf("未知区 %q（holding|input|coil|discrete）", area)
}

func parseUint16(s string) (uint16, error) {
	var v uint64
	var err error
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		_, err = fmt.Sscanf(s[2:], "%x", &v)
	} else {
		_, err = fmt.Sscanf(s, "%d", &v)
	}
	if err != nil {
		return 0, err
	}
	if v > 0xFFFF {
		return 0, fmt.Errorf("超出 uint16 范围")
	}
	return uint16(v), nil
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

func exceptionText(code byte) string {
	switch code {
	case 0x01:
		return "非法功能码"
	case 0x02:
		return "非法数据地址"
	case 0x03:
		return "非法数据值"
	case 0x04:
		return "从站设备故障"
	case 0x05:
		return "已确认（处理中）"
	case 0x06:
		return "从站设备忙"
	case 0x08:
		return "存储奇偶校验错"
	case 0x0A:
		return "网关路径不可用"
	case 0x0B:
		return "网关目标无响应"
	}
	return "未知"
}

// 编译期断言：两个实现都满足接口。写在这里是为了让「新增一种传输却忘了
// 实现 Do」在编译期就失败，而不是在运行时得到一个空指针。
var (
	_ mbBus = (*tcpBus)(nil)
	_ mbBus = (*serialBus)(nil)
)
