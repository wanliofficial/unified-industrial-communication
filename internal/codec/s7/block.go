package s7

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// S7 的块服务分两组，形态完全不同：
//
//   - USER_DATA（ROSCTR 0x07，功能组 0x03）：块目录与块属性。一次请求
//     一次应答，无会话状态；响应参数区是 12 字节，数据区是 `FF 09 + 长度`。
//   - Job 的 0x1D / 0x1E / 0x1F：块上传。三段式，Start 拿到会话 id 与块
//     长度，Upload 反复取分片，End 收尾。分片顺序与长度都由 PLC 决定，
//     主站只能按 `param[1]` 判断是不是最后一片。
//
// 这两组都不属于「周期采集」：它们发生在组态、备份、排障时，频率低、
// 数据量大，因此不进入调度器，而是给 CLI / 诊断包提供一次性会话。
const (
	// USER_DATA 功能组。0x43 = 请求(0x40) | 块信息组(0x03)。
	UserDataGroupBlocks = 0x03

	UserDataListAll          = 0x01
	UserDataListBlocksOfType = 0x02
	UserDataBlockInfo        = 0x03

	FuncStartUpload = 0x1D
	FuncUpload      = 0x1E
	FuncEndUpload   = 0x1F

	BlockOB  = 0x38 // 组织块
	BlockDB  = 0x41 // 数据块
	BlockSDB = 0x42 // 系统数据块
	BlockFC  = 0x43 // 功能
	BlockSFC = 0x44 // 系统功能
	BlockFB  = 0x45 // 功能块
	BlockSFB = 0x46 // 系统功能块

	// uploadMarker 是 Upload 响应数据区里的固定标记（FB 00）。
	uploadMarker = 0x00FB
)

// BlockTypeName 把块类型码转成两三个字母的短名。未知类型保留十六进制，
// 不猜——块类型码是协议常量，猜错会让「列 OB」变成「列出一堆别的东西」。
func BlockTypeName(t byte) string {
	switch t {
	case BlockOB:
		return "OB"
	case BlockDB:
		return "DB"
	case BlockSDB:
		return "SDB"
	case BlockFC:
		return "FC"
	case BlockSFC:
		return "SFC"
	case BlockFB:
		return "FB"
	case BlockSFB:
		return "SFB"
	}
	return fmt.Sprintf("0x%02X", t)
}

// BlockTypeOf 把配置/命令行里的块类型名转成协议码。
func BlockTypeOf(name string) (byte, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "OB":
		return BlockOB, nil
	case "DB":
		return BlockDB, nil
	case "SDB":
		return BlockSDB, nil
	case "FC":
		return BlockFC, nil
	case "SFC":
		return BlockSFC, nil
	case "FB":
		return BlockFB, nil
	case "SFB":
		return BlockSFB, nil
	}
	return 0, fmt.Errorf("s7: unknown block type %q (OB|DB|SDB|FC|SFC|FB|SFB)", name)
}

func validBlockType(t byte) bool {
	switch t {
	case BlockOB, BlockDB, BlockSDB, BlockFC, BlockSFC, BlockFB, BlockSFB:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// USER_DATA：块目录与块属性
// ---------------------------------------------------------------------------

// userDataParam 生成 USER_DATA 请求的 8 字节参数区。
func userDataParam(group, subfunc byte) []byte {
	return []byte{
		0x00,                  // reserved
		0x01,                  // parameter count
		0x12,                  // type/length header
		0x04,                  // length of following data
		0x11,                  // method: request
		0x40 | (group & 0x0F), // type 4 (request) | group
		subfunc,               // subfunction
		0x00,                  // data reference
	}
}

// BuildUserData 生成一条带 `FF 09 + 长度` 数据头的 USER_DATA 请求。
func BuildUserData(ref uint16, group, subfunc byte, payload []byte) []byte {
	data := make([]byte, 4+len(payload))
	data[0] = 0xFF
	data[1] = 0x09
	binary.BigEndian.PutUint16(data[2:4], uint16(len(payload)))
	copy(data[4:], payload)
	return BuildPDU(ROSCTRUserData, ref, userDataParam(group, subfunc), data)
}

// BuildListBlocks 请求全部七类块的数量。
//
// 它的数据区是个例外：不是 `FF 09`，而是「返回码 0x0A + 传输尺寸 0 +
// 长度 0」。照抄其它查询的格式会得到一条被 PLC 静默忽略的请求。
func BuildListBlocks(ref uint16) []byte {
	return BuildPDU(ROSCTRUserData, ref, userDataParam(UserDataGroupBlocks, UserDataListAll),
		[]byte{0x0A, 0x00, 0x00, 0x00})
}

// BuildListBlocksOfType 请求某一类型的所有块号。
func BuildListBlocksOfType(ref uint16, blockType byte) []byte {
	return BuildUserData(ref, UserDataGroupBlocks, UserDataListBlocksOfType,
		[]byte{0x30, blockType})
}

// BuildBlockInfo 请求单个块的属性。
//
// 块地址是 ASCII 的 `_0TNNNNNA` 形式：`0x30 + 类型 + 5 位块号 + 'A'`。
// 块号必须补足 5 位，`DB1` 要写成 `00001`——补零不是排版，是协议格式。
func BuildBlockInfo(ref uint16, blockType byte, num int) ([]byte, error) {
	if !validBlockType(blockType) {
		return nil, fmt.Errorf("s7: invalid block type %#02x", blockType)
	}
	if num < 0 || num > 99999 {
		return nil, fmt.Errorf("s7: block number %d out of range 0..99999", num)
	}
	payload := make([]byte, 0, 8)
	payload = append(payload, 0x30, blockType)
	payload = append(payload, fmt.Sprintf("%05d", num)...)
	payload = append(payload, 'A')
	return BuildUserData(ref, UserDataGroupBlocks, UserDataBlockInfo, payload), nil
}

// BuildUserDataResponse 生成 USER_DATA 响应（主站侧只在模拟从站与测试里用）。
func BuildUserDataResponse(ref uint16, group, subfunc byte, more bool, payload []byte) []byte {
	param := make([]byte, 12)
	param[1] = 0x01
	param[2] = 0x12
	param[3] = 0x08
	param[4] = 0x12 // method: response
	param[5] = 0x40 | (group & 0x0F)
	param[6] = subfunc
	if more {
		param[9] = 0x01
	}
	data := make([]byte, 4+len(payload))
	data[0] = 0xFF
	data[1] = 0x09
	binary.BigEndian.PutUint16(data[2:4], uint16(len(payload)))
	copy(data[4:], payload)
	return BuildPDU(ROSCTRUserData, ref, param, data)
}

// UserData 是解析后的 USER_DATA 响应。
type UserData struct {
	Group   byte
	Subfunc byte
	Seq     byte
	More    bool
	Err     uint16
	Payload []byte
}

// ParseUserData 解析 USER_DATA 响应。
//
// 参数区必须是 12 字节且方法字节为 0x12（response）。长度不吻合时直接
// 报错而不是尽量解析：USER_DATA 的负载通常是块目录，错位两字节会得到
// 一串看似合法、实际属于别的块的编号。
func ParseUserData(pdu PDU) (UserData, error) {
	if pdu.ROSCTR != ROSCTRUserData {
		return UserData{}, fmt.Errorf("s7: userdata rosctr %#02x", pdu.ROSCTR)
	}
	p := pdu.Param
	if len(p) != 12 {
		return UserData{}, fmt.Errorf("s7: userdata param length %d, want 12", len(p))
	}
	if p[0] != 0x00 || p[2] != 0x12 || p[4] != 0x12 {
		return UserData{}, fmt.Errorf("s7: unexpected userdata param % x", p)
	}
	ud := UserData{
		Group:   p[5] & 0x0F,
		Subfunc: p[6],
		Seq:     p[7],
		More:    p[9] == 0x01,
		Err:     binary.BigEndian.Uint16(p[10:12]),
	}
	if ud.Err != 0 {
		return ud, fmt.Errorf("s7: userdata error %#04x", ud.Err)
	}
	if len(pdu.Data) == 0 {
		return ud, nil
	}
	if len(pdu.Data) < 4 {
		return ud, fmt.Errorf("s7: userdata data too short (%d)", len(pdu.Data))
	}
	if pdu.Data[0] != 0xFF || pdu.Data[1] != 0x09 {
		return ud, fmt.Errorf("s7: unexpected userdata data header % x", pdu.Data[:4])
	}
	n := int(binary.BigEndian.Uint16(pdu.Data[2:4]))
	if 4+n > len(pdu.Data) {
		return ud, fmt.Errorf("s7: userdata payload says %d, have %d", n, len(pdu.Data)-4)
	}
	ud.Payload = pdu.Data[4 : 4+n]
	return ud, nil
}

// BlockCount 是「某类型块有多少个」。
type BlockCount struct {
	Type  byte
	Count int
}

// ParseListBlocks 解析块数量表。负载固定 28 字节 = 7 × (0x30, 类型, 数量)。
func ParseListBlocks(payload []byte) ([]BlockCount, error) {
	if len(payload) != 28 {
		return nil, fmt.Errorf("s7: block count payload is %d bytes, want 28", len(payload))
	}
	out := make([]BlockCount, 0, 7)
	seen := make(map[byte]bool, 7)
	for off := 0; off < 28; off += 4 {
		if payload[off] != 0x30 {
			return nil, fmt.Errorf("s7: block count entry %d has marker %#02x", off/4, payload[off])
		}
		t := payload[off+1]
		if !validBlockType(t) {
			return nil, fmt.Errorf("s7: block count entry %d has unknown type %#02x", off/4, t)
		}
		if seen[t] {
			return nil, fmt.Errorf("s7: duplicate block type %s", BlockTypeName(t))
		}
		seen[t] = true
		out = append(out, BlockCount{Type: t, Count: int(binary.BigEndian.Uint16(payload[off+2 : off+4]))})
	}
	return out, nil
}

// ParseListBlocksOfType 解析「某类型的块号列表」。
// 每项 4 字节，前 2 字节是块号，后 2 字节是语言/保留位。
func ParseListBlocksOfType(payload []byte) ([]int, error) {
	if len(payload)%4 != 0 {
		return nil, fmt.Errorf("s7: block list payload is %d bytes, not a multiple of 4", len(payload))
	}
	out := make([]int, 0, len(payload)/4)
	for off := 0; off < len(payload); off += 4 {
		out = append(out, int(binary.BigEndian.Uint16(payload[off:off+2])))
	}
	return out, nil
}

// BlockInfo 是单个块的属性。字段偏移来自 S7 的 78 字节块属性布局，
// 不是「按语义猜」的顺序——尤其是两个日期块夹在长度字段之间。
type BlockInfo struct {
	Type      byte
	Number    int
	Flags     byte
	Lang      byte
	LoadSize  int
	SBBLength int
	LocalData int
	MC7Size   int
	Version   byte
	Checksum  int
	Author    string
	Family    string
	Header    string
	CodeDate  []byte // 6 字节：毫秒(2) + 天数(2) + 保留(2)
	IntfDate  []byte
}

// ParseBlockInfo 解析 78 字节的块属性负载。
func ParseBlockInfo(payload []byte) (BlockInfo, error) {
	if len(payload) != 78 {
		return BlockInfo{}, fmt.Errorf("s7: block info payload is %d bytes, want 78", len(payload))
	}
	return BlockInfo{
		Type:      payload[1],
		Flags:     payload[9],
		Lang:      payload[10],
		Number:    int(binary.BigEndian.Uint16(payload[12:14])),
		LoadSize:  int(binary.BigEndian.Uint32(payload[14:18])),
		CodeDate:  append([]byte(nil), payload[22:28]...),
		IntfDate:  append([]byte(nil), payload[28:34]...),
		SBBLength: int(binary.BigEndian.Uint16(payload[34:36])),
		LocalData: int(binary.BigEndian.Uint16(payload[38:40])),
		MC7Size:   int(binary.BigEndian.Uint16(payload[40:42])),
		Author:    trimBlockField(payload[42:50]),
		Family:    trimBlockField(payload[50:58]),
		Header:    trimBlockField(payload[58:66]),
		Version:   payload[66],
		Checksum:  int(binary.BigEndian.Uint16(payload[68:70])),
	}, nil
}

// trimBlockField 去掉块属性里文本字段的 NUL 与尾部空格。
func trimBlockField(b []byte) string {
	return strings.TrimRight(string(b), "\x00 ")
}

// ---------------------------------------------------------------------------
// 块上传：Start → Upload × N → End
// ---------------------------------------------------------------------------

// blockAddress 生成块地址 `_0TNNNNNA`（9 字节）。
func blockAddress(blockType byte, num int, suffix byte) []byte {
	out := make([]byte, 0, 9)
	out = append(out, '_', '0', blockType)
	out = append(out, fmt.Sprintf("%05d", num)...)
	return append(out, suffix)
}

// BuildStartUpload 生成开始上传请求。
//
// 参数区是 18 字节：功能码 + 7 个保留字节 + 地址长度(9) + 块地址。
// 地址末尾的 'A' 表示「从 CPU 上传」，下载请求用 'P'——写错会被拒绝，
// 但错误码不会告诉你「本该是 A」。
func BuildStartUpload(ref uint16, blockType byte, num int) ([]byte, error) {
	if !validBlockType(blockType) {
		return nil, fmt.Errorf("s7: invalid block type %#02x", blockType)
	}
	if num < 0 || num > 99999 {
		return nil, fmt.Errorf("s7: block number %d out of range 0..99999", num)
	}
	addr := blockAddress(blockType, num, 'A')
	param := make([]byte, 0, 9+len(addr))
	param = append(param, FuncStartUpload)
	param = append(param, make([]byte, 7)...)
	param = append(param, byte(len(addr)))
	param = append(param, addr...)
	return BuildPDU(ROSCTRJob, ref, param, nil), nil
}

// BuildUpload 生成取下一分片的请求。
func BuildUpload(ref uint16, uploadID byte) []byte {
	param := make([]byte, 8)
	param[0] = FuncUpload
	param[7] = uploadID
	return BuildPDU(ROSCTRJob, ref, param, nil)
}

// BuildEndUpload 生成结束上传请求（释放 PLC 侧的会话）。
func BuildEndUpload(ref uint16, uploadID byte) []byte {
	param := make([]byte, 8)
	param[0] = FuncEndUpload
	param[7] = uploadID
	return BuildPDU(ROSCTRJob, ref, param, nil)
}

// UploadStart 是 Start Upload 的应答。
type UploadStart struct {
	ID     byte
	Length int // 整个块的字节数（含 PLC 侧的块头）
}

// ParseStartUpload 解析开始上传应答。
//
// 参数区 16 字节：功能码 + 6 保留 + 会话 id + 3 保留 + 5 位 ASCII 长度。
// 长度是十进制字符串而不是二进制——这是 S7 块服务里另一个「照直觉写
// 就会错」的地方。
func ParseStartUpload(pdu PDU) (UploadStart, error) {
	if pdu.ROSCTR != ROSCTRAckData && pdu.ROSCTR != ROSCTRJobAck {
		return UploadStart{}, fmt.Errorf("s7: start upload rosctr %#02x", pdu.ROSCTR)
	}
	if len(pdu.Param) != 16 || pdu.Param[0] != FuncStartUpload {
		return UploadStart{}, fmt.Errorf("s7: start upload param % x", pdu.Param)
	}
	text := pdu.Param[11:16]
	for _, c := range text {
		if c < '0' || c > '9' {
			return UploadStart{}, fmt.Errorf("s7: start upload length %q is not decimal", text)
		}
	}
	n, err := strconv.Atoi(string(text))
	if err != nil {
		return UploadStart{}, fmt.Errorf("s7: start upload length %q: %w", text, err)
	}
	return UploadStart{ID: pdu.Param[7], Length: n}, nil
}

// UploadFragment 是一片块数据。
type UploadFragment struct {
	Data []byte
	Last bool
}

// ParseUpload 解析一个上传分片。
//
// 数据区是 `长度(2) + FB 00 + 负载`，长度只覆盖负载本身。`param[1]` 是
// 「后面还有」标志：0 表示这是最后一片。把 1 当成最后一片会静默截断
// 块文件——这类错误不会报错，只会让备份少半截。
func ParseUpload(pdu PDU) (UploadFragment, error) {
	if pdu.ROSCTR != ROSCTRAckData && pdu.ROSCTR != ROSCTRJobAck {
		return UploadFragment{}, fmt.Errorf("s7: upload rosctr %#02x", pdu.ROSCTR)
	}
	if len(pdu.Param) != 2 || pdu.Param[0] != FuncUpload || pdu.Param[1] > 1 {
		return UploadFragment{}, fmt.Errorf("s7: upload param % x", pdu.Param)
	}
	if len(pdu.Data) < 4 {
		return UploadFragment{}, fmt.Errorf("s7: upload data too short (%d)", len(pdu.Data))
	}
	n := int(binary.BigEndian.Uint16(pdu.Data[0:2]))
	if marker := binary.BigEndian.Uint16(pdu.Data[2:4]); marker != uploadMarker {
		return UploadFragment{}, fmt.Errorf("s7: upload marker %#04x, want %#04x", marker, uploadMarker)
	}
	if n != len(pdu.Data)-4 {
		return UploadFragment{}, fmt.Errorf("s7: upload payload says %d, have %d", n, len(pdu.Data)-4)
	}
	return UploadFragment{Data: pdu.Data[4:], Last: pdu.Param[1] == 0}, nil
}

// ParseEndUpload 校验结束上传应答：参数区只有功能码，且没有数据区。
func ParseEndUpload(pdu PDU) error {
	if pdu.ROSCTR != ROSCTRAckData && pdu.ROSCTR != ROSCTRJobAck {
		return fmt.Errorf("s7: end upload rosctr %#02x", pdu.ROSCTR)
	}
	if len(pdu.Param) != 1 || pdu.Param[0] != FuncEndUpload {
		return fmt.Errorf("s7: end upload param % x", pdu.Param)
	}
	if len(pdu.Data) != 0 {
		return fmt.Errorf("s7: end upload must not carry data (%d bytes)", len(pdu.Data))
	}
	return nil
}
