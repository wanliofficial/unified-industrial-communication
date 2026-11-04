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
)

// mbread 是一个极小的 Modbus TCP 主站，用来在没有第三方工具的情况下
// 验证从站（内置或真实设备）。
//
// 存在的理由：现场最缺的不是「功能」，而是「不依赖任何外部安装即可自证
// 链路通」的手段。用 curl 验不了 Modbus，用商业工具又要求先装软件、再
// 手工点几十下；一个 30 行的子命令可以把这件事变成一行可复制的命令，
// 也让故障排查过程可以贴进工单。
//
// 边界：只做单次读写，不做轮询、不做重试、不解析设备型号。
func mbreadCmd(args []string) {
	fs := flag.NewFlagSet("mbread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:1502", "从站地址 host:port")
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

	conn, err := net.DialTimeout("tcp", *addr, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接 %s 失败: %v\n", *addr, err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(*timeout))

	if *write != "" {
		runWrite(conn, *area, *unit, *offset, *write)
		return
	}
	runRead(conn, *area, *unit, *offset, *qty)
}

func mbRoundTrip(conn net.Conn, tx uint16, unit int, pdu []byte) ([]byte, error) {
	frame := mb.BuildTCP(mb.Frame{Transaction: tx, Unit: uint8(unit), PDU: pdu})
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("发送: %w", err)
	}
	head := make([]byte, 7)
	if _, err := io.ReadFull(conn, head); err != nil {
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
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, fmt.Errorf("读取响应体: %w", err)
	}
	if len(body) >= 2 && body[0]&0x80 != 0 {
		return body, fmt.Errorf("从站异常码 0x%02X（%s）", body[1], exceptionText(body[1]))
	}
	return body, nil
}

func runRead(conn net.Conn, area string, unit, offset, qty int) {
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

	resp, err := mbRoundTrip(conn, 1, unit, pdu)
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

func runWrite(conn net.Conn, area string, unit, offset int, value string) {
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
		resp, err := mbRoundTrip(conn, 1, unit, pdu)
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
	resp, err := mbRoundTrip(conn, 1, unit, pdu)
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
