package modbus

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Modbus ASCII 分帧。
//
// 与 RTU 的差别不只是「多了两个字符」：
//
//   - RTU 用 CRC16 与静默间隔分帧，ASCII 用 LRC 与固定终止符 CRLF 分帧；
//   - RTU 的帧是二进制，ASCII 的帧是十六进制文本，每个字节膨胀成两个字符；
//   - RTU 的帧间静默是 3.5 字符时间，ASCII 的帧间允许长间隔（规范给 1s）。
//
// 现场仍有大量仪表只支持 ASCII（尤其是老式流量计、温控表），而它们的
// 配置页往往只写「Modbus」，不写是哪种帧。把 ASCII 做成独立驱动、由配置
// 显式选择，比「自动探测」可靠得多：探测失败会把故障推迟到第一次采样。

const (
	// ASCIIMaxFrame 是一帧的字节上限：':' + 2×(地址1+PDU253+LRC1) + CRLF。
	ASCIIMaxFrame = 1 + 2*(1+253+1) + 2
	// 最大十六进制字符数：地址(1) + PDU(253) + LRC(1) 各两字符。
	asciiMaxHex = 2 * (1 + 253 + 1)
)

// LRC 计算 Modbus ASCII 的纵向冗余校验：所有字节之和取二进制补码。
//
// 与 CRC 不同，它不能发现字节换位，但实现只有一行，且对串口的偶发
// 位错足够。校验强度低是 ASCII 帧格式本身的设计，不是这里的取舍。
func LRC(b []byte) byte {
	var sum byte
	for _, x := range b {
		sum += x
	}
	return byte(0 - sum)
}

// BuildASCII 生成 ASCII 帧：':' + 大写十六进制(地址+PDU+LRC) + CRLF。
//
// 规范允许小写十六进制，但统一输出大写：现场用串口助手排障时，大小写
// 混排会让「肉眼比对从站回包」这件事变得无谓地困难。
func BuildASCII(unit byte, pdu []byte) []byte {
	raw := make([]byte, 0, len(pdu)+2)
	raw = append(raw, unit)
	raw = append(raw, pdu...)
	raw = append(raw, LRC(raw))

	out := make([]byte, 0, 1+len(raw)*2+2)
	out = append(out, ':')
	const digits = "0123456789ABCDEF"
	for _, b := range raw {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return append(out, '\r', '\n')
}

// ParseASCII 校验并剥离 ASCII 帧的地址与 LRC，返回地址与 PDU。
//
// 容错边界刻意收紧：必须以 ':' 开头，必须是偶数个十六进制字符，LRC
// 必须吻合。缺任何一条都直接报错，而不是「尽力解析」——半条帧被当成
// 完整帧解析出来的值，比读失败危险得多。
func ParseASCII(b []byte) (unit byte, pdu []byte, err error) {
	if len(b) < 7 {
		return 0, nil, fmt.Errorf("modbus: ascii frame too short (%d)", len(b))
	}
	if b[0] != ':' {
		return 0, nil, fmt.Errorf("modbus: ascii frame missing ':' start, got %q", b[0])
	}
	body := b[1:]
	// 终止符允许 CRLF 或仅 LF：部分网关/串口服务器会吃掉 CR。
	switch {
	case len(body) >= 2 && body[len(body)-2] == '\r' && body[len(body)-1] == '\n':
		body = body[:len(body)-2]
	case len(body) >= 1 && body[len(body)-1] == '\n':
		body = body[:len(body)-1]
	default:
		return 0, nil, errors.New("modbus: ascii frame missing CRLF terminator")
	}

	if len(body) < 4 {
		return 0, nil, fmt.Errorf("modbus: ascii payload too short (%d hex chars)", len(body))
	}
	if len(body)%2 != 0 {
		return 0, nil, fmt.Errorf("modbus: ascii payload has odd hex length (%d)", len(body))
	}
	if len(body) > asciiMaxHex {
		return 0, nil, fmt.Errorf("modbus: ascii payload too long (%d hex chars)", len(body))
	}

	raw, err := hex.DecodeString(strings.ToUpper(string(body)))
	if err != nil {
		return 0, nil, fmt.Errorf("modbus: ascii non-hex payload: %w", err)
	}
	if got := LRC(raw[:len(raw)-1]); got != raw[len(raw)-1] {
		return 0, nil, fmt.Errorf("modbus: ascii lrc mismatch want %#02x got %#02x",
			got, raw[len(raw)-1])
	}
	return raw[0], append([]byte(nil), raw[1:len(raw)-1]...), nil
}

// ASCIIExpectedResponseLen 由请求 PDU 推导**正常**响应的帧总字符数。
//
// ASCII 帧本身以 CRLF 分帧，理论上不需要预知长度；但有了它就能在读完
// 预期字符数后立刻校验终止符，把一个「从站把字节计数写坏、一直不发
// CRLF」的故障从「等到超时」提前到「立刻判协议错误」。
//
// 异常响应（FC|0x80，PDU 恒 2 字节）不走本函数。
func ASCIIExpectedResponseLen(reqPDU []byte) (int, error) {
	rtuLen, err := RTUExpectedResponseLen(reqPDU)
	if err != nil {
		return 0, err
	}
	// RTU 帧长 = 地址1 + PDU + CRC2；ASCII 用 LRC1 替代 CRC2。
	body := rtuLen - 1 // 减掉 CRC 的 2 字节、加上 LRC 的 1 字节
	return 1 + body*2 + 2, nil
}
