// Package fins 实现欧姆龙 FINS 协议（TCP 承载）的纯函数编解码。
//
// 与 MC/S7 的 codec 一样，这里只做「字节 ↔ 结构」：不建连接、不读时钟、
// 不持有状态。主站与内置从站共用同一张内存区码表，避免两边对同一个区名
// 的理解漂移。
//
// 三层封装，缺一不可：
//
//	FINS/TCP 头： "FINS" + 长度(4) + 命令(4) + 错误码(4) [+ 节点号(4/8)]
//	FINS 帧头：   ICF RSV GCT DNA DA1 DA2 SNA SA1 SA2 SID
//	FINS 命令：   MRC SRC + 参数/数据
//
// 三个最容易写错的细节：
//  1. FINS/TCP 的长度字段是 **大端**，且等于「命令+错误码+负载」的字节数
//     （即不含 magic 与长度字段自身）；
//  2. TCP 建链必须先做节点号协商（命令 0 → 1），后续每一帧都用协商出来的
//     节点号填 DA1/SA1，否则 PLC 会在帧层直接丢弃或回 0x0301；
//  3. 位访问与字访问用**不同的内存区码**（CIO 字 0xB0 / 位 0x30），位访问
//     的起始地址由「字地址(2) + 位号(1)」组成，响应是**一位一字节**，不是
//     像三菱那样按位打包。
package fins

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// FINS/TCP 封装常量。
const (
	TCPMagic = "FINS"

	TCPCommandNodeRequest  uint32 = 0 // 客户端请求分配节点号
	TCPCommandNodeResponse uint32 = 1 // 服务端返回节点号
	TCPCommandFrame        uint32 = 2 // 承载 FINS 帧

	DefaultPort = "9600"

	// MaxPayload 是 FINS/TCP 允许的最大负载（命令码之后的部分）。
	MaxPayload = 2012
	// MaxWords / MaxBits 是单条内存区读写的保守上限。规范允许更大
	// （字 999、位 1998），但不同机型差异明显；驱动按更小的块切分。
	MaxWords = 999
	MaxBits  = 1998
)

// 命令码（MRC/SRC）。
const (
	MRCRead  byte = 0x01
	SRCRead  byte = 0x01
	MRCWrite byte = 0x01
	SRCWrite byte = 0x02
)

// Header 是 10 字节的 FINS 帧头。
type Header struct {
	ICF byte // Information Control Field
	RSV byte
	GCT byte // Gateway Count
	DNA byte // Destination Network Address
	DA1 byte // Destination Node Address
	DA2 byte // Destination Unit Address
	SNA byte // Source Network Address
	SA1 byte // Source Node Address
	SA2 byte // Source Unit Address
	SID byte // Service ID
}

// CommandHeader 构造一个「请求」帧头：要求应答、走本网络、CPU 单元。
//
// da1 是目标节点（PLC），sa1 是源节点（本机，由握手协商得到）。GCT 固定
// 为 2 是 Omron 推荐值：跨两层网络仍可路由。
func CommandHeader(da1, sa1, sid byte) Header {
	return Header{ICF: 0x80, RSV: 0, GCT: 2, DNA: 0, DA1: da1, DA2: 0,
		SNA: 0, SA1: sa1, SA2: 0, SID: sid}
}

// Build 把帧头与命令体拼成完整 FINS 帧。
func (h Header) Build(body []byte) []byte {
	out := make([]byte, 10+len(body))
	out[0] = h.ICF
	out[1] = h.RSV
	out[2] = h.GCT
	out[3] = h.DNA
	out[4] = h.DA1
	out[5] = h.DA2
	out[6] = h.SNA
	out[7] = h.SA1
	out[8] = h.SA2
	out[9] = h.SID
	copy(out[10:], body)
	return out
}

// ResponseHeader 由请求帧头构造响应帧头：源/目的地址互换，ICF 置「应答」
// 位（bit6），并保留 bit7。
func ResponseHeader(req Header) Header {
	return Header{
		ICF: req.ICF | 0xC0,
		RSV: 0,
		GCT: req.GCT,
		DNA: req.SNA,
		DA1: req.SA1,
		DA2: req.SA2,
		SNA: req.DNA,
		SA1: req.DA1,
		SA2: req.DA2,
		SID: req.SID,
	}
}

// Frame 是解析后的 FINS 帧。
type Frame struct {
	Header  Header
	MRC     byte
	SRC     byte
	EndCode uint16 // 仅响应帧有效
	Data    []byte // 命令参数或响应数据（响应已跳过结束码）
}

// IsResponse 报告该帧是否带「应答」位。
func (f Frame) IsResponse() bool { return f.Header.ICF&0x40 != 0 }

// BuildCommand 构造一个请求帧（不含结束码）。
func BuildCommand(h Header, mrc, src byte, data []byte) []byte {
	body := make([]byte, 2+len(data))
	body[0], body[1] = mrc, src
	copy(body[2:], data)
	return h.Build(body)
}

// BuildResponse 构造一个响应帧（含 2 字节结束码）。
func BuildResponse(req Frame, endCode uint16, data []byte) []byte {
	out := make([]byte, 14+len(data))
	h := ResponseHeader(req.Header)
	copy(out, h.Build(nil))
	out[10], out[11] = req.MRC, req.SRC
	binary.BigEndian.PutUint16(out[12:], endCode)
	copy(out[14:], data)
	return out
}

// Parse 解析一个完整 FINS 帧。响应帧从 Data 中剔除结束码；调用方可用
// EndCodeError 判断业务层是否成功。
func Parse(raw []byte) (Frame, error) {
	if len(raw) < 12 {
		return Frame{}, fmt.Errorf("fins: frame too short (%d bytes)", len(raw))
	}
	f := Frame{
		Header: Header{
			ICF: raw[0], RSV: raw[1], GCT: raw[2], DNA: raw[3],
			DA1: raw[4], DA2: raw[5], SNA: raw[6], SA1: raw[7],
			SA2: raw[8], SID: raw[9],
		},
		MRC: raw[10], SRC: raw[11],
	}
	if f.IsResponse() {
		if len(raw) < 14 {
			return Frame{}, fmt.Errorf("fins: response too short (%d bytes)", len(raw))
		}
		f.EndCode = binary.BigEndian.Uint16(raw[12:])
		f.Data = append([]byte(nil), raw[14:]...)
	} else {
		f.Data = append([]byte(nil), raw[12:]...)
	}
	return f, nil
}

// EndCodeError 表示 FINS 业务层结束码非 0。
type EndCodeError struct {
	Code uint16
}

func (e *EndCodeError) Error() string {
	return fmt.Sprintf("fins end code 0x%04X: %s", e.Code, EndCodeText(e.Code))
}

// Address 报告该结束码是否属于「地址/范围」类错误。
func (e *EndCodeError) Address() bool {
	switch e.Code {
	case 0x1101, 0x1102, 0x1103, 0x1104:
		return true
	}
	return false
}

// Link 报告该结束码是否属于「链路/路由」类错误（可重试）。
func (e *EndCodeError) Link() bool {
	switch e.Code {
	case 0x0201, 0x0202, 0x0203, 0x0204, 0x0205, 0x0301, 0x0302:
		return true
	}
	return false
}

// EndCodeText 返回常见结束码的文字说明。
func EndCodeText(code uint16) string {
	switch code {
	case 0x0000:
		return "normal completion"
	case 0x0101:
		return "service interrupted"
	case 0x0201:
		return "local node not in network"
	case 0x0202:
		return "token timeout"
	case 0x0203:
		return "maximum transmission retries exceeded"
	case 0x0205:
		return "routing table error"
	case 0x0301:
		return "destination node not in network"
	case 0x0302:
		return "unit missing"
	case 0x0401:
		return "undefined command"
	case 0x1001:
		return "command too long"
	case 0x1002:
		return "command too short"
	case 0x1101:
		return "area type mismatch"
	case 0x1102:
		return "address range exceeded"
	case 0x1103:
		return "address out of range"
	case 0x1104:
		return "address write-protected"
	}
	return "unknown end code"
}

// TCPError 表示 FINS/TCP 封装层错误码非 0。
type TCPError struct {
	Code uint32
}

func (e *TCPError) Error() string {
	return fmt.Sprintf("fins/tcp error 0x%08X: %s", e.Code, TCPErrorText(e.Code))
}

// TCPErrorText 返回 FINS/TCP 错误码说明。
func TCPErrorText(code uint32) string {
	switch code {
	case 0x00000000:
		return "normal"
	case 0x00000001:
		return "header is not FINS"
	case 0x00000002:
		return "data length too long"
	case 0x00000003:
		return "unsupported command"
	case 0x00000020:
		return "all connections in use"
	case 0x00000021:
		return "specified node already connected"
	case 0x00000022:
		return "protected node accessed from unspecified IP"
	case 0x00000023:
		return "client node address out of range"
	case 0x00000024:
		return "same node address used by client and server"
	case 0x00000025:
		return "no free node addresses"
	}
	return "unknown tcp error"
}

// ---------------------------------------------------------------------------
// FINS/TCP 封装
// ---------------------------------------------------------------------------

// BuildNodeRequest 构造节点号协商请求。clientNode 传 0 表示请 PLC 自动
// 分配（多数以太网单元支持；个别老机型要求填本机 IP 末位）。
func BuildNodeRequest(clientNode byte) []byte {
	out := make([]byte, 20)
	copy(out, TCPMagic)
	binary.BigEndian.PutUint32(out[4:], 12) // 命令 + 错误码 + 客户端节点
	binary.BigEndian.PutUint32(out[8:], TCPCommandNodeRequest)
	binary.BigEndian.PutUint32(out[12:], 0)
	binary.BigEndian.PutUint32(out[16:], uint32(clientNode))
	return out
}

// BuildNodeResponse 构造节点号协商响应（命令 1，总长 24 字节）。
//
// 长度字段是「命令+错误码+客户端节点+服务端节点」= 16，不含 magic 与
// 长度字段自身；写成 24 会让对端把后 8 字节当成下一帧的开头。
func BuildNodeResponse(clientNode, serverNode byte) []byte {
	out := make([]byte, 24)
	copy(out, TCPMagic)
	binary.BigEndian.PutUint32(out[4:], 16)
	binary.BigEndian.PutUint32(out[8:], TCPCommandNodeResponse)
	binary.BigEndian.PutUint32(out[12:], 0)
	binary.BigEndian.PutUint32(out[16:], uint32(clientNode))
	binary.BigEndian.PutUint32(out[20:], uint32(serverNode))
	return out
}

// BuildNodeError 构造只带错误码的节点号响应（长度 8，无节点号）。
func BuildNodeError(code uint32) []byte {
	out := make([]byte, 16)
	copy(out, TCPMagic)
	binary.BigEndian.PutUint32(out[4:], 8)
	binary.BigEndian.PutUint32(out[8:], TCPCommandNodeResponse)
	binary.BigEndian.PutUint32(out[12:], code)
	return out
}

// BuildTCPFrame 把完整 FINS 帧封装成 FINS/TCP 命令 2。
func BuildTCPFrame(frame []byte) ([]byte, error) {
	if len(frame)+8 > MaxPayload {
		return nil, fmt.Errorf("fins: frame too large for fins/tcp (%d bytes)", len(frame))
	}
	out := make([]byte, 16+len(frame))
	copy(out, TCPMagic)
	binary.BigEndian.PutUint32(out[4:], uint32(8+len(frame)))
	binary.BigEndian.PutUint32(out[8:], TCPCommandFrame)
	binary.BigEndian.PutUint32(out[12:], 0)
	copy(out[16:], frame)
	return out, nil
}

// TCPHeader 是解析后的 FINS/TCP 头。
type TCPHeader struct {
	Command uint32
	Error   uint32
	Length  int // 命令+错误码+负载的字节数
}

// ParseTCPHeader 解析 16 字节的 FINS/TCP 头（magic + 长度 + 命令 + 错误码）。
func ParseTCPHeader(b []byte) (TCPHeader, error) {
	if len(b) < 16 {
		return TCPHeader{}, fmt.Errorf("fins/tcp: header too short (%d bytes)", len(b))
	}
	if string(b[:4]) != TCPMagic {
		return TCPHeader{}, fmt.Errorf("fins/tcp: bad magic % x", b[:4])
	}
	length := int(binary.BigEndian.Uint32(b[4:]))
	if length < 8 || length > MaxPayload {
		return TCPHeader{}, fmt.Errorf("fins/tcp: invalid length %d", length)
	}
	return TCPHeader{
		Command: binary.BigEndian.Uint32(b[8:]),
		Error:   binary.BigEndian.Uint32(b[12:]),
		Length:  length,
	}, nil
}

// ---------------------------------------------------------------------------
// 内存区
// ---------------------------------------------------------------------------

// Area 描述一个 FINS 内存区。
type Area struct {
	Key  string // 配置里使用的小写区名
	Name string // 展示名
	// Word 是字访问码；Bit 是位访问码，0 表示该区不支持位访问（或
	// 位访问码不确定，驱动会退化为「读字 → 改位 → 写字」）。
	Word byte
	Bit  byte
	Bank int // EM 区号；0 表示非 EM 或当前 EM 区
}

// BitCapable 报告该区是否可以直接按位访问。
func (a Area) BitCapable() bool { return a.Bit != 0 }

var areas = func() map[string]Area {
	m := map[string]Area{
		"cio": {Key: "cio", Name: "CIO 继电器", Word: 0xB0, Bit: 0x30},
		"wr":  {Key: "wr", Name: "工作区 WR", Word: 0xB1, Bit: 0x31},
		"hr":  {Key: "hr", Name: "保持继电器 HR", Word: 0xB2, Bit: 0x32},
		"ar":  {Key: "ar", Name: "辅助继电器 AR", Word: 0xB3, Bit: 0x33},
		"dm":  {Key: "dm", Name: "数据存储器 DM", Word: 0x82, Bit: 0x02},
		// 当前 EM 区只有字访问码（0x98）是明确的；位访问码在不同手册
		// 里表述不一，这里不猜：bool 点位走字读取 + 位提取。
		"em": {Key: "em", Name: "当前 EM 区", Word: 0x98},
	}
	// EM bank 0..12：字 0xA0..0xAC，位 0x20..0x2C。
	for i := 0; i <= 12; i++ {
		key := fmt.Sprintf("em%d", i)
		m[key] = Area{Key: key, Name: fmt.Sprintf("EM 区 bank %d", i),
			Word: byte(0xA0 + i), Bit: byte(0x20 + i), Bank: i}
	}
	return m
}()

var areaAliases = map[string]string{
	"d": "dm", "w": "wr", "h": "hr", "a": "ar",
}

// Lookup 按区名查内存区定义。大小写不敏感，并接受 D/W/H/A 简写。
func Lookup(area string) (Area, bool) {
	key := strings.ToLower(strings.TrimSpace(area))
	if alias, ok := areaAliases[key]; ok {
		key = alias
	}
	a, ok := areas[key]
	return a, ok
}

// Areas 返回支持的区名（固定顺序，便于表单与文档）。
func Areas() []string {
	out := []string{"cio", "wr", "hr", "ar", "dm", "em"}
	for i := 0; i <= 12; i++ {
		out = append(out, fmt.Sprintf("em%d", i))
	}
	return out
}

// ByWordCode 按字访问码反查内存区（从站解析请求时用）。
func ByWordCode(code byte) (Area, bool) {
	for _, a := range areas {
		if a.Word == code {
			return a, true
		}
	}
	return Area{}, false
}

// ByBitCode 按位访问码反查内存区。
func ByBitCode(code byte) (Area, bool) {
	for _, a := range areas {
		if a.Bit != 0 && a.Bit == code {
			return a, true
		}
	}
	return Area{}, false
}

// BitToWordCode 把位访问码映射到同一块内存的字访问码。内置从站用一张
// 字内存表同时承载字/位视图，保证「写 CIO0.0 后读 CIO0 字」能看到变化。
func BitToWordCode(code byte) (byte, bool) {
	if a, ok := ByBitCode(code); ok {
		return a.Word, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 内存区读写命令
// ---------------------------------------------------------------------------

// Access 描述一次内存区访问使用的区码与访问方式。位访问与字访问共用
// 同一套参数布局，只有区码不同；把「方式」单独拎出来，避免用 bit==0
// 判断字/位（CIO0.0 的位号就是 0）。
type Access struct {
	Code byte
	Bit  bool
}

// WordAccess 返回字访问。
func WordAccess(a Area) Access { return Access{Code: a.Word} }

// BitAccess 返回位访问；该区不支持位访问时报错。
func BitAccess(a Area) (Access, error) {
	if !a.BitCapable() {
		return Access{}, fmt.Errorf("fins: area %q does not support direct bit access", a.Key)
	}
	return Access{Code: a.Bit, Bit: true}, nil
}

// BuildMemoryRead 构造内存区读命令。
//
// 字访问：bit 传 0，count 是字数；位访问：bit 传 0..15，count 是位数。
func BuildMemoryRead(h Header, acc Access, word uint16, bit byte, count uint16) ([]byte, error) {
	if count == 0 {
		return nil, fmt.Errorf("fins: read count must be > 0")
	}
	if bit > 15 {
		return nil, fmt.Errorf("fins: bit position %d out of range 0..15", bit)
	}
	if acc.Bit {
		if count > MaxBits {
			return nil, fmt.Errorf("fins: bit read count %d exceeds %d", count, MaxBits)
		}
		if int(word)+(int(bit)+int(count)-1)/16 > 0xFFFF {
			return nil, fmt.Errorf("fins: bit read range exceeds address space")
		}
	} else {
		if count > MaxWords {
			return nil, fmt.Errorf("fins: word read count %d exceeds %d", count, MaxWords)
		}
		if int(word)+int(count)-1 > 0xFFFF {
			return nil, fmt.Errorf("fins: word read range exceeds address space")
		}
	}
	data := []byte{
		acc.Code, byte(word >> 8), byte(word), bit,
		byte(count >> 8), byte(count),
	}
	return BuildCommand(h, MRCRead, SRCRead, data), nil
}

// BuildMemoryWrite 构造内存区写命令。data 的宽度必须与访问方式一致：
// 字访问为 count*2 字节，位访问为 count 字节（每字节 0/1）。
func BuildMemoryWrite(h Header, acc Access, word uint16, bit byte, count uint16, data []byte) ([]byte, error) {
	if count == 0 {
		return nil, fmt.Errorf("fins: write count must be > 0")
	}
	if bit > 15 {
		return nil, fmt.Errorf("fins: bit position %d out of range 0..15", bit)
	}
	want := int(count) * 2
	if acc.Bit {
		want = int(count)
		if count > MaxBits {
			return nil, fmt.Errorf("fins: bit write count %d exceeds %d", count, MaxBits)
		}
	} else if count > MaxWords {
		return nil, fmt.Errorf("fins: word write count %d exceeds %d", count, MaxWords)
	}
	if len(data) != want {
		return nil, fmt.Errorf("fins: write data length %d, want %d", len(data), want)
	}
	body := make([]byte, 6+len(data))
	body[0] = acc.Code
	body[1], body[2], body[3] = byte(word>>8), byte(word), bit
	body[4], body[5] = byte(count>>8), byte(count)
	copy(body[6:], data)
	if len(body)+10 > MaxPayload {
		return nil, fmt.Errorf("fins: write command too large (%d bytes)", len(body))
	}
	return BuildCommand(h, MRCWrite, SRCWrite, body), nil
}
