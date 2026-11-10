package mewtocol

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件是编解码的另一半：**解析请求**与**构造应答**。
//
// 主站只组帧不拆请求，内置从站只拆请求不组帧，看起来可以各写一份；但
// 那样两份代码对「RD 的地址是 5 位、RCC 的是 4 位」这类细节会各自漂移，
// 而漂移的表现是「主站自检通过、对接真 PLC 全错」或者反过来的假绿。
// 放在同一个包里，至少让两边的字节序、地址宽度、接点编号规则只有一处
// 定义。

// Contact 是一个接点地址。
type Contact struct {
	Area Area
	Word int
	Bit  int
	// Value 与 HasValue 只对写命令有意义。读命令里 HasValue 恒为 false，
	// 这样零值 Contact 不会被误读成「写 0」。
	Value    bool
	HasValue bool
}

// Request 是一条已解析的请求帧。
type Request struct {
	Station Station
	Cmd     string
	// Area 是整段访问的区域。多点接点命令（RCP/WCP）里每个接点可以落在
	// 不同区域，此时以第一个接点的区域为准，逐个见 Contacts。
	Area Area
	// Start/End 是闭区间地址（字地址）。RD/WD/RCC/WCC/RS/WS/RK/WK 使用。
	// 索引寄存器（IX/IY/ID）没有编号，Start 恒为 0，End 由区码决定
	// （IX/IY 一个寄存器，ID 两个）。
	Start int
	End   int
	// Contacts 是接点命令（RCS/RCP/WCS/WCP）的逐点清单。
	Contacts []Contact
	// Data 是写命令的数据，已规范化成大端字节（每字 2 字节）。
	Data []byte
	Raw  []byte
}

// Words 返回这次请求涉及的字数。
func (r Request) Words() int { return r.End - r.Start + 1 }

// EchoCode 返回应答里回显的命令码：请求命令码的**前两个字符**。
//
// 手册的应答帧图里，`RCC`/`RCS`/`RCP` 一律回 `RC`，`WCC`/`WCS`/`WCP`
// 一律回 `WC`。第三位是「单点/多点/字单位」的变体位，只在请求里出现。
// 因此校验应答时比对前两位即可；按整条命令码比对会把每一条接点命令
// 都判成错帧——而且是永远错，不是偶发。
func EchoCode(cmd string) string {
	if len(cmd) < 2 {
		return cmd
	}
	return cmd[:2]
}

// ParseRequest 校验并解析一条完整请求帧（含 BCC 与 CR）。
//
// 内置从站用它，因此它必须容忍主站侧的合法变化：BCC 可以写成 `**`、
// 站号可以是 `FF`（广播）。它不接受半帧——收帧的职责在 Reader 里。
func ParseRequest(raw []byte) (Request, error) {
	station, rest, err := stripFrame(raw, SymbolCmd)
	if err != nil {
		return Request{}, err
	}
	// 三字符命令先匹配：`RC` 是 `RCS`/`RCP`/`RCC` 的前缀，顺序反了
	// 就会把每一条接点命令都解析成两字符命令。
	cmd := ""
	for _, c := range []string{
		CmdReadContactOne, CmdReadContacts, CmdReadContactWord,
		CmdWriteContactOne, CmdWriteContacts, CmdWriteContactWord,
	} {
		if strings.HasPrefix(rest, c) {
			cmd = c
			break
		}
	}
	if cmd == "" {
		if len(rest) < 2 {
			return Request{}, fmt.Errorf("mewtocol: request body %q has no command code", rest)
		}
		cmd = rest[:2]
	}
	req := Request{Station: station, Cmd: cmd, Raw: raw}
	body := rest[len(cmd):]

	switch cmd {
	case CmdReadData: // RD：区码 + 起始(5) + 结束(5)
		area, tail, ok := takeWordArea(body)
		if !ok {
			return Request{}, fmt.Errorf("mewtocol: RD names an unknown data code in %q", body)
		}
		req.Area = area
		if area.Kind == KindIndex {
			if err := expectZeros(tail, 9, "index register address"); err != nil {
				return Request{}, err
			}
			req.Start, req.End = 0, 0
			if area.Code == "ID" {
				req.End = 1
			}
			return req, nil
		}
		if req.Start, tail, err = parseDec(tail, 5, "RD start address"); err != nil {
			return Request{}, err
		}
		if req.End, tail, err = parseDec(tail, 5, "RD end address"); err != nil {
			return Request{}, err
		}
		if tail != "" {
			return Request{}, fmt.Errorf("mewtocol: RD has %d unexpected trailing characters", len(tail))
		}
		return req, nil

	case CmdWriteData: // WD：区码 + 起始(5) + 结束(5) + 数据
		area, tail, ok := takeWordArea(body)
		if !ok {
			return Request{}, fmt.Errorf("mewtocol: WD names an unknown data code in %q", body)
		}
		req.Area = area
		if area.Kind == KindIndex {
			if err := expectZeros(tail, 9, "index register address"); err != nil {
				return Request{}, err
			}
			data, err := DecodeWords(tail[9:])
			if err != nil {
				return Request{}, err
			}
			req.Start, req.End = 0, len(data)/2-1
			req.Data = data
			return req, nil
		}
		if req.Start, tail, err = parseDec(tail, 5, "WD start address"); err != nil {
			return Request{}, err
		}
		if req.End, tail, err = parseDec(tail, 5, "WD end address"); err != nil {
			return Request{}, err
		}
		data, err := DecodeWords(tail)
		if err != nil {
			return Request{}, err
		}
		if len(data)/2 != req.End-req.Start+1 {
			return Request{}, fmt.Errorf("mewtocol: WD declares %d words (%d..%d) but carries %d",
				req.End-req.Start+1, req.Start, req.End, len(data)/2)
		}
		req.Data = data
		return req, nil

	case CmdReadContactWord, CmdWriteContactWord: // RCC / WCC：区码 + 起始(4) + 结束(4) [+ 数据]
		if len(body) < 1 {
			return Request{}, fmt.Errorf("mewtocol: %s is missing its contact code", cmd)
		}
		area, ok := contactArea(body[:1])
		if !ok {
			return Request{}, fmt.Errorf("mewtocol: %s names an unknown contact code %q", cmd, body[:1])
		}
		req.Area = area
		tail := body[1:]
		if req.Start, tail, err = parseDec(tail, 4, cmd+" start word"); err != nil {
			return Request{}, err
		}
		if req.End, tail, err = parseDec(tail, 4, cmd+" end word"); err != nil {
			return Request{}, err
		}
		if cmd == CmdReadContactWord {
			if tail != "" {
				return Request{}, fmt.Errorf("mewtocol: RCC has %d unexpected trailing characters", len(tail))
			}
			return req, nil
		}
		data, err := DecodeWords(tail)
		if err != nil {
			return Request{}, err
		}
		if len(data)/2 != req.End-req.Start+1 {
			return Request{}, fmt.Errorf("mewtocol: WCC declares %d words (%d..%d) but carries %d",
				req.End-req.Start+1, req.Start, req.End, len(data)/2)
		}
		req.Data = data
		return req, nil

	case CmdReadContactOne, CmdWriteContactOne: // 单接点：区码 + 接点号(4) [+ 数据(1)]
		c, tail, err := parseContact(body)
		if err != nil {
			return Request{}, err
		}
		if cmd == CmdWriteContactOne {
			if tail == "" {
				return Request{}, fmt.Errorf("mewtocol: WCS is missing the contact data digit")
			}
			v, err := parseBit(tail[:1])
			if err != nil {
				return Request{}, err
			}
			c.Value, c.HasValue = v, true
			tail = tail[1:]
		}
		if tail != "" {
			return Request{}, fmt.Errorf("mewtocol: %s has %d unexpected trailing characters", cmd, len(tail))
		}
		req.Area = c.Area
		req.Start, req.End = c.Word, c.Word
		req.Contacts = []Contact{c}
		return req, nil

	case CmdReadContacts, CmdWriteContacts: // 多接点：n(1) + n×(区码 + 接点号(4) [+ 数据(1)])
		if len(body) < 1 {
			return Request{}, fmt.Errorf("mewtocol: %s is missing the point count", cmd)
		}
		n, err := strconv.Atoi(body[:1])
		if err != nil || n < 1 || n > 8 {
			return Request{}, fmt.Errorf("mewtocol: %s point count %q must be 1..8", cmd, body[:1])
		}
		tail := body[1:]
		for i := 0; i < n; i++ {
			c, rest, err := parseContact(tail)
			if err != nil {
				return Request{}, fmt.Errorf("mewtocol: %s point %d: %w", cmd, i+1, err)
			}
			if cmd == CmdWriteContacts {
				if rest == "" {
					return Request{}, fmt.Errorf("mewtocol: WCP point %d is missing the contact data digit", i+1)
				}
				v, err := parseBit(rest[:1])
				if err != nil {
					return Request{}, fmt.Errorf("mewtocol: WCP point %d: %w", i+1, err)
				}
				c.Value, c.HasValue = v, true
				rest = rest[1:]
			}
			req.Contacts = append(req.Contacts, c)
			tail = rest
		}
		if tail != "" {
			return Request{}, fmt.Errorf("mewtocol: %s has %d unexpected trailing characters", cmd, len(tail))
		}
		req.Area = req.Contacts[0].Area
		req.Start, req.End = req.Contacts[0].Word, req.Contacts[len(req.Contacts)-1].Word
		return req, nil

	case CmdReadSetValue, CmdWriteSetValue, CmdReadElapsed, CmdWriteElapsed:
		// RS/WS/RK/WK：裸的 起始(4) + 结束(4) [+ 数据]，没有区域码。
		area, ok := Lookup("sv")
		if cmd == CmdReadElapsed || cmd == CmdWriteElapsed {
			area, ok = Lookup("ev")
		}
		if !ok {
			return Request{}, fmt.Errorf("mewtocol: internal area table is missing sv/ev")
		}
		req.Area = area
		if req.Start, body, err = parseDec(body, 4, cmd+" start address"); err != nil {
			return Request{}, err
		}
		if req.End, body, err = parseDec(body, 4, cmd+" end address"); err != nil {
			return Request{}, err
		}
		if cmd == CmdReadSetValue || cmd == CmdReadElapsed {
			if body != "" {
				return Request{}, fmt.Errorf("mewtocol: %s has %d unexpected trailing characters", cmd, len(body))
			}
			return req, nil
		}
		data, err := DecodeWords(body)
		if err != nil {
			return Request{}, err
		}
		if len(data)/2 != req.End-req.Start+1 {
			return Request{}, fmt.Errorf("mewtocol: %s declares %d words (%d..%d) but carries %d",
				cmd, req.End-req.Start+1, req.Start, req.End, len(data)/2)
		}
		req.Data = data
		return req, nil
	}
	return Request{}, fmt.Errorf("mewtocol: unsupported command %q", cmd)
}

// stripFrame 校验外层结构（头、CR、BCC、站号、命令符号）并返回正文。
func stripFrame(raw []byte, symbol byte) (Station, string, error) {
	if len(raw) < MinFrame {
		return 0, "", fmt.Errorf("mewtocol: frame too short (%d bytes)", len(raw))
	}
	if raw[len(raw)-1] != Terminator {
		return 0, "", fmt.Errorf("mewtocol: frame does not end with CR")
	}
	if raw[0] != Header && raw[0] != HeaderLong {
		return 0, "", fmt.Errorf("mewtocol: bad header %q", raw[0])
	}
	body := raw[:len(raw)-1]
	if len(body) < 7 {
		return 0, "", fmt.Errorf("mewtocol: frame body too short (%d bytes)", len(body))
	}
	bccAt := len(body) - 2
	if body[bccAt] != BCCWildcard || body[bccAt+1] != BCCWildcard {
		want, err := parseHexByte(string(body[bccAt:]))
		if err != nil {
			return 0, "", fmt.Errorf("mewtocol: bad BCC field %q: %w", body[bccAt:], err)
		}
		if got := BCC(body[:bccAt]); got != want {
			return 0, "", &ChecksumError{Got: got, Want: want}
		}
	}
	station, err := ParseStation(string(body[1:3]))
	if err != nil {
		return 0, "", err
	}
	if body[3] != symbol {
		return 0, "", fmt.Errorf("mewtocol: expected symbol %q, got %q", symbol, body[3])
	}
	return station, string(body[4:bccAt]), nil
}

// parseDec 取出 n 位十进制字段。
//
// 单独成一个函数是为了统一错误信息：地址字段写成十六进制是这套协议最
// 常见的组帧错误，报「第几个字段、期望几位十进制」比报「strconv 失败」
// 有用得多。
func parseDec(rest string, n int, what string) (int, string, error) {
	if len(rest) < n {
		return 0, "", fmt.Errorf("mewtocol: %s needs %d digits, frame ends after %q", what, n, rest)
	}
	field := rest[:n]
	for i := 0; i < n; i++ {
		if field[i] < '0' || field[i] > '9' {
			return 0, "", fmt.Errorf("mewtocol: %s %q is not %d decimal digits", what, field, n)
		}
	}
	v, err := strconv.Atoi(field)
	if err != nil {
		return 0, "", fmt.Errorf("mewtocol: %s %q is not a number", what, field)
	}
	return v, rest[n:], nil
}

// expectZeros 校验一段「必须全是 0」的占位字段。
func expectZeros(rest string, n int, what string) error {
	if len(rest) < n {
		return fmt.Errorf("mewtocol: %s needs %d zero characters, frame ends after %q", what, n, rest)
	}
	for i := 0; i < n; i++ {
		if rest[i] != '0' {
			return fmt.Errorf("mewtocol: %s must be %d zeros, got %q", what, n, rest[:n])
		}
	}
	return nil
}

// parseContact 解析「区码 + 4 位接点号」。
func parseContact(s string) (Contact, string, error) {
	if len(s) < 5 {
		return Contact{}, "", fmt.Errorf("mewtocol: contact specification %q is shorter than 5 characters", s)
	}
	area, ok := contactArea(s[:1])
	if !ok {
		return Contact{}, "", fmt.Errorf("mewtocol: unknown contact code %q", s[:1])
	}
	word, bit, err := ParseContactNumber(area, s[1:5])
	if err != nil {
		return Contact{}, "", err
	}
	return Contact{Area: area, Word: word, Bit: bit}, s[5:], nil
}

func parseBit(s string) (bool, error) {
	switch s {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("mewtocol: contact data %q must be \"0\" or \"1\"", s)
}

// contactArea 按区码查接点区。
func contactArea(code string) (Area, bool) {
	for _, a := range areas {
		if a.Kind == KindContact && a.Code == code {
			return a, true
		}
	}
	return Area{}, false
}

// takeWordArea 从正文开头取出区码（1 或 2 个字符）并返回剩余正文。
//
// 先试 2 字符再试 1 字符：索引寄存器 IX/IY/ID 是两字符区码，而字区
// D/L/F 是一字符。反过来的顺序会把 "IX" 里的 "I" 当成未知区码。
func takeWordArea(rest string) (Area, string, bool) {
	if len(rest) >= 2 {
		for _, a := range areas {
			if (a.Kind == KindWord || a.Kind == KindIndex) && a.Code == rest[:2] {
				return a, rest[2:], true
			}
		}
	}
	if len(rest) >= 1 {
		for _, a := range areas {
			if a.Kind == KindWord && a.Code == rest[:1] {
				return a, rest[1:], true
			}
		}
	}
	return Area{}, "", false
}

// ---------------------------------------------------------------------------
// 应答构造（内置从站用）
// ---------------------------------------------------------------------------

// BuildResponse 构造正常应答：`%` + 站号 + `$` + 回显码(2) + 数据 + BCC + CR。
//
// 回显码是请求命令码的前两位，由 EchoCode 统一处理——从站自己截一次
// 字符串，迟早会漏掉某个三字符命令。
func BuildResponse(st Station, cmd, data string) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	frame := make([]byte, 0, 9+len(data))
	frame = append(frame, Header)
	frame = append(frame, st.Format()...)
	frame = append(frame, SymbolOK)
	frame = append(frame, EchoCode(cmd)...)
	frame = append(frame, data...)
	out := AppendBCC(frame)
	if len(out) > MaxFramePct {
		return nil, fmt.Errorf("mewtocol: response is %d characters, over the %d-character limit",
			len(out), MaxFramePct)
	}
	return out, nil
}

// BuildErrorResponse 构造异常应答：`%` + 站号 + `!` + 错误码(2) + BCC + CR。
func BuildErrorResponse(st Station, code byte) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	frame := make([]byte, 0, 8)
	frame = append(frame, Header)
	frame = append(frame, st.Format()...)
	frame = append(frame, SymbolErr, hexDigits[code>>4], hexDigits[code&0x0f])
	return AppendBCC(frame), nil
}

// EncodeContacts 把布尔序列编码成接点读应答的数据段（每点一个字符）。
func EncodeContacts(vals []bool) string {
	var sb strings.Builder
	sb.Grow(len(vals))
	for _, v := range vals {
		if v {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	}
	return sb.String()
}

// DecodeContacts 把接点读应答的数据段解成布尔序列。
func DecodeContacts(data string) ([]bool, error) {
	out := make([]bool, 0, len(data))
	for i := 0; i < len(data); i++ {
		v, err := parseBit(data[i : i+1])
		if err != nil {
			return nil, fmt.Errorf("mewtocol: contact data at offset %d: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}
