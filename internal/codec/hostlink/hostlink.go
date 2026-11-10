// Package hostlink 实现 Host Link FINS（上位链接 FINS，头码 `FA`）的纯函数编解码。
//
// 与 FINS/TCP 共用同一套 FINS 命令语义（内存区读 `0101` / 写 `0102`），差别
// 全在外面那层信封：FINS/TCP 是「二进制 + 定长头 + 节点号协商」，Host Link
// 是「可打印 ASCII + 异或校验 + CR 定界」。所以本包不做命令语义，只做
// 「FINS 帧 ↔ Host Link 信封」的转换与收帧。
//
// 一条请求帧（主机 → PLC）：
//
//	@ 00 FA 0 00 00 00 00 0101 820000000001 xx * CR
//	│ │  │  │ │  │  │  │  │    │            │  │ │
//	│ │  │  │ │  │  │  │  │    │            │  │ └ CR
//	│ │  │  │ │  │  │  │  │    │            │  └ 定界符 `*`
//	│ │  │  │ │  │  │  │  │    │            └ FCS（两位十六进制）
//	│ │  │  │ │  │  │  │  │    └ 命令正文（参数/数据，一字节两字符）
//	│ │  │  │ │  │  │  │  └ 命令码 MRC/SRC
//	│ │  │  │ │  │  │  └ SID（源端服务标识，重发计数用，通常 00）
//	│ │  │  │ │  │  └ SA2（源单元地址）
//	│ │  │  │ │  └ DA2（目标单元地址）
//	│ │  │  │ └ ICF（信息控制字段：直连 00，经网络中转 80）
//	│ │  │  └ 响应等待时间（1 位十六进制，单位 10ms，0..F）
//	│ │  └ 头码 FA（FINS 命令）；C 模式命令这里是 `RR`/`WD` 之类的两个字母
//	│ └ 单元号（两位十进制 ASCII，00..31）
//	└ 帧头
//
// 一条应答帧（PLC → 主机）在 `FA` 之后**多一个两位的结束码**：
//
//	@ 00 FA 00 40 00 00 00 0101 0000 1234 xx * CR
//	      └┬┘ └────── 精简 FINS 头 ──────┘ └┬┘ └──┘
//	       │                              │   └ FINS 结束码（四位十六进制）
//	       └ Host Link 结束码（两位十六进制，00 正常）
//
// 八个最容易写错的地方：
//
//  1. **收发格式不对称。** 应答比请求多一格结束码、少一格等待时间。按请求的
//     布局去解析应答，会把结束码当成 ICF、把后面每个字段整体错位两格，症状
//     是「SID 对不上」或「数据永远是垃圾」。反过来，把结束码写进请求，PLC
//     会回 14（格式错）或者干脆不理。
//
//  2. **FCS 的范围含 `@`。** 从帧头一直异或到正文最后一个字符，不含 FCS
//     自身、不含 `*`、不含 CR。少算 `@` 是最常见的写法错误，而且只会在真机
//     上暴露——本地回环里两边都少算，校验永远通过。
//
//  3. **正文是「一字节两字符」。** 区码 0x82 上线是字符 `'8'`、`'2'`
//     （0x38、0x32），不是裸字节 0x82。因此**整帧是可打印 ASCII**，7 位数据
//     位完全够用——这是串口参数一节的关键前提，也是「FINS 必须 8 位数据位」
//     这个流传很广的说法错在哪里。
//
//  4. **响应等待时间是 1 位，不是 2 位。** 取值 0..F，单位 10ms。写成两位会
//     让 PLC 把第二位当成 ICF 的高位，整帧错位。
//
//  5. **精简头只有 4 个字段。** ICF/DA2/SA2/SID。路由用的 RSV/GCT/DNA/DA1/
//     SNA/SA1 在「直连」格式里不存在——目标就是本机单元，路由由上一跳决定。
//     本包只实现直连短格式：跨网访问要用串行网关的长格式（那些字段各占两位
//     十六进制，整帧多 12 个字符），那是另一套布局，用「猜字段个数」的方式
//     去兼容两种格式只会让解析变成掷骰子。
//
//  6. **一帧只有一千多字符，且不能分段。** 手册明确「FINS 命令不能拆成多帧
//     发送」，所以读一次最多 269 字（应答数据 1076 字符 / 4）、写一次最多
//     267 字（命令正文 1080 字符减去 12 字符参数再 / 4）。照抄 FINS/TCP 的
//     999 字分块会稳定收到结束码 18（帧长错）。
//
//  7. **单元号是十进制。** 单元号 31 上线是字符 `'3'`、`'1'`，不是 `'1'`、
//     `'F'`。和 FCS/数据的十六进制混排，是这套协议第二个进制陷阱。
//
//  8. **ICF 在直连时是 00，不是 80。** FINS/TCP 的请求惯用 0x80，但 Host Link
//     直连要求 0x00（0x80 的含义是「要经网络中转」）。应答对应地回 0x40。
//     把 FINS/TCP 的头直接搬过来，PLC 会按「跨网」去解，然后找不到路由。
package hostlink

import (
	"encoding/binary"
	"fmt"
	"strconv"
)

// 帧定界符与长度上限。
const (
	// Start 是帧头字符。
	Start = '@'
	// HeaderCode 是 FINS 命令的头码。C 模式命令（`RR`/`WD`/`RD`…）是另外
	// 一套字母头码，本实现不发也不认。
	HeaderCode = "FA"
	// Terminator 是定界符，紧接 FCS 之后。
	Terminator = '*'
	// CR 是帧结束符。
	CR = 0x0d

	// MaxUnit 是单元号上限（含）。手册写作 0..31。
	MaxUnit = 31
	// MaxWait 是响应等待时间上限（含），单位 10ms，即最长 150ms。
	MaxWait = 0x0f

	// MinRequest 是最短合法请求帧（无正文），22 字符：
	// `@`1 + 单元号2 + `FA`2 + 等待1 + 头8 + 命令码4 + FCS2 + `*`1 + CR1。
	MinRequest = 22
	// MinResponse 是最短合法应答帧（无数据），27 字符：
	// `@`1 + 单元号2 + `FA`2 + 结束码2 + 头8 + 命令码4 + FINS 结束码4
	// + FCS2 + `*`1 + CR1。
	MinResponse = 27
	// MaxFrame 是整帧上限（含 CR）。手册给的是应答 1115 字符，对应跨网长
	// 格式；直连短格式在同样数据量下只有 1103 字符。这里按手册的
	// 1115 取，给「PLC 回了长格式」这种意外留出接收余量（解析会另报错）。
	MaxFrame = 1115

	// MaxCommandChars / MaxResponseChars 是手册给出的整帧字符上限。
	// 它们是**跨网长格式**（多 12 字符路由字段）的数字；直连短格式对应
	// 1102 / 1103。上线时按正文预算切分才不会越界，见下面的 Max* 常量。
	MaxCommandChars  = 1114
	MaxResponseChars = 1115
	// MaxTextChars 是请求正文上限（1080 字符 = 540 字节）。
	MaxTextChars = 1080
	// MaxDataChars 是应答数据上限（1076 字符 = 538 字节），不含 FINS 结束码。
	MaxDataChars = 1076

	// routeChars 是跨网长格式多出来的 12 个字符（RSV/GCT/DNA/DA1/SNA/SA1）。
	routeChars = 12
	// memParamChars 是内存区读写命令正文里的固定参数：区码2 + 起始字4
	// + 位号2 + 数量4 = 12 字符。写命令的数据接在它后面。
	memParamChars = 12

	// cmdOverhead 是请求去掉正文后的固定开销（22 字符）。
	cmdOverhead = 22
	// respOverhead 是应答去掉数据与 FINS 结束码后的固定开销（23 字符）：
	// `@` + 单元号2 + `FA`2 + 结束码2 + 头8 + 命令码4 + FCS2 + `*` + CR。
	respOverhead = 23

	// 单次内存区访问的元素上限，由字符预算推导（见包注释第 6 条）。
	// 读受应答预算约束、写受命令预算约束，两者相差两字——不是一个可以
	// 随便统一成「都取 240」的数字。
	MaxReadWords  = MaxDataChars / 4                   // 269
	MaxWriteWords = (MaxTextChars - memParamChars) / 4 // 267
	MaxReadBits   = MaxDataChars / 2                   // 538
	MaxWriteBits  = (MaxTextChars - memParamChars) / 2 // 534
)

const hexDigits = "0123456789ABCDEF"

// FCS 计算帧校验：从帧头 `@` 起逐字节异或到正文最后一个字符。
//
// 传入的应当是「不含 FCS、不含 `*` 与 CR」的那一段。返回值 0..255，上线时
// 写成两位大写十六进制。
func FCS(b []byte) byte {
	var x byte
	for _, c := range b {
		x ^= c
	}
	return x
}

// Request 是一次 Host Link FINS 请求的语义形式。
//
// Text 是二进制的命令正文（内存区读写的参数与数据）；上线时由 Build 转成
// 一字节两字符的十六进制 ASCII。把「二进制正文」与「ASCII 线格式」分开，
// 是为了让驱动/从站只关心 FINS 语义。
type Request struct {
	Unit byte // 单元号 0..31，两位十进制 ASCII
	Wait byte // 响应等待时间 0..15，一位十六进制，单位 10ms
	ICF  byte // 0x00 直连；0x80 经网络中转
	DA2  byte // 目标单元地址（Host Link 直连恒为 0，即 CPU 单元）
	SA2  byte // 源单元地址（Host Link 直连恒为 0）
	SID  byte // 源端服务标识，重发计数用
	MRC  byte // 主命令码
	SRC  byte // 子命令码
	Text []byte
}

// CharLen 返回该帧上线后的字符数（不含 FCS 与定界符）。
func (r Request) CharLen() int { return cmdOverhead + len(r.Text)*2 }

// Validate 校验取值范围。
func (r Request) Validate() error {
	if r.Unit > MaxUnit {
		return fmt.Errorf("hostlink: unit number %d out of range 0..%d", r.Unit, MaxUnit)
	}
	if r.Wait > MaxWait {
		return fmt.Errorf("hostlink: response wait time %d out of range 0..%d (single hex digit)", r.Wait, MaxWait)
	}
	if len(r.Text)*2 > MaxTextChars {
		return fmt.Errorf("hostlink: command text %d chars exceeds %d", len(r.Text)*2, MaxTextChars)
	}
	return nil
}

// Build 把请求组帧成纯 ASCII 字节（含 FCS、`*` 与 CR）。
func Build(r Request) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	out := make([]byte, 0, r.CharLen()+4)
	out = append(out, Start)
	out = append(out, byte('0'+r.Unit/10), byte('0'+r.Unit%10))
	out = append(out, HeaderCode...)
	out = append(out, hexDigits[r.Wait&0x0f])
	out = appendHex(out, r.ICF)
	out = appendHex(out, r.DA2)
	out = appendHex(out, r.SA2)
	out = appendHex(out, r.SID)
	out = appendHex(out, r.MRC)
	out = appendHex(out, r.SRC)
	out = appendHexBytes(out, r.Text)

	out = appendHex(out, FCS(out))
	out = append(out, Terminator, CR)
	return out, nil
}

// Response 是一次 Host Link FINS 应答的语义形式。
//
// 同一个结构既用于「解析收到的帧」也用于「构造要发出去的帧」：两者字段
// 完全一致，分成两个类型只会让调用方在两者之间来回搬字段。
type Response struct {
	Unit        byte
	EndCode     byte // Host Link 结束码，0x00 正常
	ICF         byte // 直连应答为 0x40
	DA2         byte // 回显请求的 SA2
	SA2         byte // 回显请求的 DA2
	SID         byte // 回显请求的 SID
	MRC         byte
	SRC         byte
	FinsEndCode uint16 // FINS 结束码，0x0000 正常
	Data        []byte
}

// CharLen 返回该帧上线后的字符数（不含 FCS 与定界符）。
func (r Response) CharLen() int { return respOverhead + 4 + len(r.Data)*2 }

// Build 把应答组帧成纯 ASCII 字节。
func BuildResponse(r Response) ([]byte, error) {
	if r.Unit > MaxUnit {
		return nil, fmt.Errorf("hostlink: unit number %d out of range 0..%d", r.Unit, MaxUnit)
	}
	if len(r.Data)*2 > MaxDataChars {
		return nil, fmt.Errorf("hostlink: response data %d chars exceeds %d", len(r.Data)*2, MaxDataChars)
	}
	out := make([]byte, 0, r.CharLen()+4)
	out = append(out, Start)
	out = append(out, byte('0'+r.Unit/10), byte('0'+r.Unit%10))
	out = append(out, HeaderCode...)
	out = appendHex(out, r.EndCode)
	out = appendHex(out, r.ICF)
	out = appendHex(out, r.DA2)
	out = appendHex(out, r.SA2)
	out = appendHex(out, r.SID)
	out = appendHex(out, r.MRC)
	out = appendHex(out, r.SRC)
	out = appendHex(out, byte(r.FinsEndCode>>8))
	out = appendHex(out, byte(r.FinsEndCode))
	out = appendHexBytes(out, r.Data)

	out = appendHex(out, FCS(out))
	out = append(out, Terminator, CR)
	return out, nil
}

// Reply 按请求回显字段构造一条应答。
//
// 三个必须回显的字段：单元号、SID、MRC/SRC（PLC 自己的应答也这么做，主站
// 靠它们把应答配到请求上）。ICF 置「这是应答」位（0x40）并保留请求的
// 「有无网络中转」位：直连请求 0x00 → 应答 0x40，跨网请求 0x80 → 0xC0。
func Reply(req Request, finsEndCode uint16, data []byte) Response {
	return Response{
		Unit:        req.Unit,
		EndCode:     0,
		ICF:         (req.ICF & 0x80) | 0x40,
		DA2:         req.SA2,
		SA2:         req.DA2,
		SID:         req.SID,
		MRC:         req.MRC,
		SRC:         req.SRC,
		FinsEndCode: finsEndCode,
		Data:        data,
	}
}

// FINS 把应答还原成标准 FINS 帧（10 字节头 + 结束码 + 数据），交给上层
// 复用 FINS 的解析与结束码分类。
//
// 被 Host Link 丢掉的 6 个路由字段填 0：直连格式里它们本来就不存在，填 0
// 表示「本网络、本节点」，与直接连接的语义一致。
func (r Response) FINS() []byte {
	out := make([]byte, 14+len(r.Data))
	out[0] = r.ICF
	out[5] = r.DA2
	out[8] = r.SA2
	out[9] = r.SID
	out[10], out[11] = r.MRC, r.SRC
	binary.BigEndian.PutUint16(out[12:], r.FinsEndCode)
	copy(out[14:], r.Data)
	return out
}

// FromFINS 把一帧标准 FINS 请求压进 Host Link 信封。
//
// 这一步是**有损**的：RSV/GCT/DNA/DA1/SNA/SA1 六个字节在直连格式里没有
// 对应字段（目标由单元号指定，路由由上一跳决定），会被丢弃。ICF 也由调用
// 方显式给出——FINS/TCP 惯用 0x80，而 Host Link 直连必须是 0x00。
func FromFINS(unit, wait, icf byte, frame []byte) (Request, error) {
	if len(frame) < 12 {
		return Request{}, fmt.Errorf("hostlink: FINS frame too short (%d bytes)", len(frame))
	}
	return Request{
		Unit: unit,
		Wait: wait,
		ICF:  icf,
		DA2:  frame[5],
		SA2:  frame[8],
		SID:  frame[9],
		MRC:  frame[10],
		SRC:  frame[11],
		Text: append([]byte(nil), frame[12:]...),
	}, nil
}

// ParseRequest 解析一条请求帧（内置从站用）。
func ParseRequest(raw []byte) (Request, error) {
	f, err := split(raw, MinRequest)
	if err != nil {
		return Request{}, err
	}
	unit, err := parseUnit(f)
	if err != nil {
		return Request{}, err
	}
	wait, err := parseHex1(f[5])
	if err != nil {
		return Request{}, fmt.Errorf("hostlink: bad response wait time: %w", err)
	}
	if wait > MaxWait {
		return Request{}, fmt.Errorf("hostlink: response wait time %02X out of range 0..%X", wait, MaxWait)
	}
	// 请求在等待时间（1 字符）之后就是精简头：f[0]=`@`、f[1:3]=单元号、
	// f[3:5]=`FA`、f[5]=等待时间，所以头从 f[6] 开始。
	head, text, err := parseBody(f[6:])
	if err != nil {
		return Request{}, err
	}
	body, err := decodeHexAll(text)
	if err != nil {
		return Request{}, fmt.Errorf("hostlink: bad command text: %w", err)
	}
	return Request{
		Unit: unit, Wait: wait,
		ICF: head.icf, DA2: head.da2, SA2: head.sa2, SID: head.sid,
		MRC: head.mrc, SRC: head.src, Text: body,
	}, nil
}

// ParseResponse 解析一条应答帧（主站用）。
//
// 结束码非 0 不在这里报错：结束码是业务信息（0x13 FCS 错、0x14 格式错、
// 0x18 帧长错…），由调用方决定怎么分类。真正属于「这帧读不成」的问题
// （缺定界符、FCS 不符、字面量非法）才返回错误。
func ParseResponse(raw []byte) (Response, error) {
	f, err := split(raw, MinResponse)
	if err != nil {
		return Response{}, err
	}
	unit, err := parseUnit(f)
	if err != nil {
		return Response{}, err
	}
	end, err := parseHex2(f[5:7])
	if err != nil {
		return Response{}, fmt.Errorf("hostlink: bad end code: %w", err)
	}
	head, rest, err := parseBody(f[7:])
	if err != nil {
		return Response{}, err
	}
	if len(rest) < 2 {
		return Response{}, fmt.Errorf("hostlink: response is missing the FINS end code")
	}
	hi, err := parseHex2(rest[0:2])
	if err != nil {
		return Response{}, fmt.Errorf("hostlink: bad FINS end code: %w", err)
	}
	lo, err := parseHex2(rest[2:4])
	if err != nil {
		return Response{}, fmt.Errorf("hostlink: bad FINS end code: %w", err)
	}
	data, err := decodeHexAll(rest[4:])
	if err != nil {
		return Response{}, fmt.Errorf("hostlink: bad data field: %w", err)
	}
	return Response{
		Unit: unit, EndCode: end,
		ICF: head.icf, DA2: head.da2, SA2: head.sa2, SID: head.sid,
		MRC: head.mrc, SRC: head.src,
		FinsEndCode: uint16(hi)<<8 | uint16(lo), Data: data,
	}, nil
}

// split 做三件所有帧都要做的事：长度、定界符、FCS。
//
// 返回的是「`@` 到正文末字符」这一段（不含 FCS、`*`、CR）。
func split(raw []byte, minLen int) ([]byte, error) {
	n := len(raw)
	if n < minLen {
		return nil, fmt.Errorf("hostlink: frame too short (%d bytes, want >= %d)", n, minLen)
	}
	if raw[0] != Start {
		return nil, fmt.Errorf("hostlink: frame does not start with %q", Start)
	}
	if raw[n-1] != CR || raw[n-2] != Terminator {
		return nil, fmt.Errorf("hostlink: frame is missing the %q CR terminator", Terminator)
	}
	if string(raw[3:5]) != HeaderCode {
		return nil, fmt.Errorf("hostlink: header code %q is not %q (this is a C-mode or vendor frame, "+
			"not a FINS command)", raw[3:5], HeaderCode)
	}
	fcsAt := n - 4 // FCS 占两位，之后是 `*` 与 CR
	want, err := parseHex2(raw[fcsAt : fcsAt+2])
	if err != nil {
		return nil, fmt.Errorf("hostlink: bad FCS field: %w", err)
	}
	body := raw[:fcsAt]
	if got := FCS(body); got != want {
		return nil, &FCSMismatch{Got: got, Want: want, Raw: append([]byte(nil), raw...)}
	}
	return body, nil
}

// headFields 是精简 FINS 头的解码结果。
type headFields struct {
	icf, da2, sa2, sid, mrc, src byte
}

// parseBody 解 `@`+单元号+`FA`+[结束码/等待时间] 之后的公共部分：
// 8 字符精简头 + 4 字符命令码，其余按「一字节两字符」解成二进制。
//
// 请求与应答在这一段之后才开始分岔（请求的其余部分就是正文，应答的
// 前 4 字符是 FINS 结束码），所以公共部分只解到这里。
func parseBody(b []byte) (headFields, []byte, error) {
	if len(b) < 12 {
		return headFields{}, nil, fmt.Errorf("hostlink: frame body too short (%d chars)", len(b))
	}
	var h headFields
	var err error
	if h.icf, err = parseHex2(b[0:2]); err != nil {
		return h, nil, fmt.Errorf("hostlink: bad ICF: %w", err)
	}
	if h.da2, err = parseHex2(b[2:4]); err != nil {
		return h, nil, fmt.Errorf("hostlink: bad DA2: %w", err)
	}
	if h.sa2, err = parseHex2(b[4:6]); err != nil {
		return h, nil, fmt.Errorf("hostlink: bad SA2: %w", err)
	}
	if h.sid, err = parseHex2(b[6:8]); err != nil {
		return h, nil, fmt.Errorf("hostlink: bad SID: %w", err)
	}
	if h.mrc, err = parseHex2(b[8:10]); err != nil {
		return h, nil, fmt.Errorf("hostlink: bad command code: %w", err)
	}
	if h.src, err = parseHex2(b[10:12]); err != nil {
		return h, nil, fmt.Errorf("hostlink: bad command code: %w", err)
	}
	// 返回的是**十六进制字符**而不是解码后的字节：应答的头 4 个字符是
	// FINS 结束码，请求的同一位置已经是正文第一字节。分界只有调用方知道，
	// 在这里解码就得再切一次已经丢掉原始字符的字节流。
	rest := b[12:]
	if len(rest)%2 != 0 {
		return h, nil, fmt.Errorf("hostlink: text field has an odd number of hex chars (%d)", len(rest))
	}
	return h, rest, nil
}

// parseUnit 解两位十进制单元号并校验范围。
func parseUnit(f []byte) (byte, error) {
	u, err := parseDec2(f[1:3])
	if err != nil {
		return 0, fmt.Errorf("hostlink: bad unit number: %w", err)
	}
	if u > MaxUnit {
		return 0, fmt.Errorf("hostlink: unit number %d out of range 0..%d", u, MaxUnit)
	}
	return u, nil
}

// FCSMismatch 表示校验和不符。带上两边的值与原始帧，现场排查时可以
// 直接把帧贴进手册的算例里比对。
type FCSMismatch struct {
	Got  byte // 实际算出的
	Want byte // 帧里携带的
	Raw  []byte
}

func (e *FCSMismatch) Error() string {
	return fmt.Sprintf("hostlink: FCS mismatch (computed %02X, frame carries %02X)", e.Got, e.Want)
}

// Comm 标记这是线路层问题（噪声、位错、参数不匹配），不是点位配置问题。
// 正确的质量是「通信错误」——记成「坏值」会让运维去查点表而不是查线。
func (e *FCSMismatch) Comm() bool { return true }

// EndCodeError 表示 Host Link 结束码非 0（PLC 在信封层就拒了这条命令）。
type EndCodeError struct {
	Code byte
}

func (e *EndCodeError) Error() string {
	return fmt.Sprintf("hostlink end code 0x%02X: %s", e.Code, EndCodeText(e.Code))
}

// Format 报告该结束码是否说明「帧本身写错了」。
//
// 这三类错误修一次就永久消失（改帧格式/长度），不该参与重试或熔断统计；
// 把它们和「PLC 忙」混在一起，会让一个写错的实现看起来像链路不稳。
func (e *EndCodeError) Format() bool {
	switch e.Code {
	case 0x13, 0x14, 0x15, 0x18:
		return true
	}
	return false
}

// EndCodeText 返回常见 Host Link 结束码的文字说明。
func EndCodeText(code byte) string {
	switch code {
	case 0x00:
		return "normal completion"
	case 0x01:
		return "not executable in RUN mode"
	case 0x02:
		return "not executable in MONITOR mode"
	case 0x03:
		return "user memory write-protected"
	case 0x04:
		return "address overflow"
	case 0x0b:
		return "not executable in PROGRAM mode"
	case 0x13:
		return "FCS error"
	case 0x14:
		return "format error"
	case 0x15:
		return "entry number data error"
	case 0x16:
		return "command not supported"
	case 0x18:
		return "frame length error"
	case 0x19:
		return "not executable"
	case 0x20:
		return "could not create I/O table"
	case 0x21:
		return "not executable due to CPU Unit error"
	case 0x23:
		return "user memory protected"
	}
	return "unknown end code"
}

// UnitMismatch 表示应答的单元号与本次请求不一致。
//
// 一条 RS-485 上挂多台 PLC 时，单元号重复会让两台的应答都发出来；不校验
// 就会把 A 的数据记到 B 头上，而且从数据上完全看不出来。
type UnitMismatch struct {
	Got  byte
	Want byte
}

func (e *UnitMismatch) Error() string {
	return fmt.Sprintf("hostlink: response from unit %02d, request addressed unit %02d "+
		"(another PLC on the line probably shares the same unit number)", e.Got, e.Want)
}

// Comm 标记这是「总线上有别的 PLC 在回话」，属线路问题。
func (e *UnitMismatch) Comm() bool { return true }

// ---------------------------------------------------------------------------
// 字面量
// ---------------------------------------------------------------------------

func appendHex(dst []byte, v byte) []byte {
	return append(dst, hexDigits[v>>4], hexDigits[v&0x0f])
}

func appendHexBytes(dst, src []byte) []byte {
	for _, v := range src {
		dst = append(dst, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return dst
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

func parseHex1(c byte) (byte, error) {
	v, ok := hexVal(c)
	if !ok {
		return 0, fmt.Errorf("%q is not a hex digit", c)
	}
	return v, nil
}

func parseHex2(s []byte) (byte, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("want 2 hex digits, got %d", len(s))
	}
	hi, ok := hexVal(s[0])
	if !ok {
		return 0, fmt.Errorf("%q is not a hex digit", s[0])
	}
	lo, ok := hexVal(s[1])
	if !ok {
		return 0, fmt.Errorf("%q is not a hex digit", s[1])
	}
	return hi<<4 | lo, nil
}

func parseDec2(s []byte) (byte, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("want 2 decimal digits, got %d", len(s))
	}
	n, err := strconv.ParseUint(string(s), 10, 8)
	if err != nil {
		return 0, err
	}
	return byte(n), nil
}

func decodeHexAll(src []byte) ([]byte, error) {
	if len(src)%2 != 0 {
		return nil, fmt.Errorf("odd number of hex chars (%d)", len(src))
	}
	if len(src) == 0 {
		return nil, nil
	}
	dst := make([]byte, len(src)/2)
	for i := range dst {
		v, err := parseHex2(src[i*2 : i*2+2])
		if err != nil {
			return nil, err
		}
		dst[i] = v
	}
	return dst, nil
}

// FrameLen 判断缓冲区开头这一帧是否已完整，返回帧长（含 CR）。
//
// 帧尾判据是 `*`+CR 而不是「第一个 CR」：只有定界符才算结束。手册说 FINS
// 帧不能分段，但把「裸 CR」排除掉仍然值得——噪声、别的主站的 C 模式残留、
// 以及某些设备在 `*CR` 后补的 CRLF 都会带来裸 CR，按第一个 CR 收尾就会把
// 一帧切成两半，FCS 永远算不对。
func FrameLen(buf []byte) (int, bool) {
	if len(buf) > 0 && buf[0] != Start {
		return 0, false
	}
	for i, c := range buf {
		if c == CR {
			if i == 0 || buf[i-1] != Terminator {
				continue // 裸 CR：分段定界符或噪声，继续等
			}
			return i + 1, true
		}
	}
	return 0, false
}

// WordsFor 返回一次内存区访问可用的最大元素数。
func WordsFor(bitAccess, write bool) int {
	switch {
	case bitAccess && write:
		return MaxWriteBits
	case bitAccess:
		return MaxReadBits
	case write:
		return MaxWriteWords
	default:
		return MaxReadWords
	}
}

// PeekUnit 尽力从一帧里取出单元号，用于「帧本身解析失败但仍想回一条带
// 结束码的应答」的场景（内置从站）。
//
// 只看 `@` 后面两位十进制字符：这是全帧唯一不依赖 FCS 就能独立定位的字段，
// 也正好是判断「这帧是不是发给我的」所需的全部信息。
func PeekUnit(raw []byte) (byte, error) {
	if len(raw) < 3 || raw[0] != Start {
		return 0, fmt.Errorf("hostlink: frame too short to locate the unit number")
	}
	return parseUnit(raw)
}
