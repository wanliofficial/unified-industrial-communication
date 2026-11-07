// Package s7 实现 S7comm（西门子 S7）的纯函数编解码。
//
// 分层：TPKT（RFC1006，带总长度）→ COTP（ISO 8073，连接与数据单元）→
// S7 PDU（10 字节头 + 参数区 + 数据区）。
//
// 与 Modbus 的根本差别：S7 一次请求可以携带**任意多项**（读最多约 20 项
// 受 PDU 长度约束），每项自带地址与长度，项与项之间不需要地址连续。因此
// 规划层的「按连续性合并」在这里收益不大，驱动改为「按项打包、按 PDU
// 预算切块」——这样跨区、跨 DB 的点位可以一次读完。
package s7

import (
	"encoding/binary"
	"errors"
	"fmt"

	"uic/internal/driver"
	"uic/internal/model"
)

// TPKT / COTP / S7 常量。
const (
	TPKTVersion = 0x03
	S7ProtoID   = 0x32

	ROSCTRJob      = 0x01 // 请求
	ROSCTRJobAck   = 0x02 // 确认（无数据）
	ROSCTRAckData  = 0x03 // 确认 + 数据
	ROSCTRUserData = 0x07 // 用户数据（块目录/SZL 等）

	COTPCR = 0xE0 // 连接请求
	COTPCC = 0xD0 // 连接确认
	COTPDT = 0xF0 // 数据

	FuncSetup = 0xF0
	FuncRead  = 0x04
	FuncWrite = 0x05

	AreaInput   = 0x81 // I / E
	AreaOutput  = 0x82 // Q / A
	AreaMerker  = 0x83 // M / 标志位
	AreaDB      = 0x84 // 数据块
	AreaCounter = 0x1C // C
	AreaTimer   = 0x1D // T

	// 请求项里的「数据类型」。
	TSBit  = 0x01
	TSByte = 0x02

	// 响应/写数据里的「传输尺寸」。
	RSBit       = 0x03
	RSByteWord  = 0x04 // BYTE/WORD/DWORD，长度以位计
	RSInteger   = 0x05
	RSDInteger  = 0x06
	RSReal      = 0x07
	RSDate      = 0x08
	RSTimeOfDay = 0x09
	RSTime      = 0x0A
	RSS5Time    = 0x0B
	RSDateTime  = 0x0C

	// 单项返回码。
	CodeOK               = 0xFF
	CodeHardware         = 0x01
	CodeAccessDenied     = 0x03
	CodeInvalidAddr      = 0x05
	CodeTypeNotSupport   = 0x06
	CodeTypeInconsistent = 0x07
	CodeObjectNotFound   = 0x0A
)

// Item 是一次读写中的一个变量。
//
// Bit < 0 表示按字节访问；否则按位访问（Bit 只能 0..7）。S7 的位地址是
// 「字节地址 × 8 + 位号」，这是协议规定，不是实现选择。
type Item struct {
	Area      byte
	DB        int
	ByteAddr  int
	Bit       int
	Bytes     int
	BitAccess bool
}

// TPKT 给载荷加上 4 字节 TPKT 头（版本 3 + 保留 + 总长度）。
func TPKT(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	out[0] = TPKTVersion
	out[1] = 0
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	copy(out[4:], payload)
	return out
}

// ParseTPKT 校验并剥离 TPKT 头。
func ParseTPKT(b []byte) ([]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("s7: tpkt too short (%d)", len(b))
	}
	if b[0] != TPKTVersion {
		return nil, fmt.Errorf("s7: tpkt version %d, want 3", b[0])
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n < 4 || n > 8192 {
		return nil, fmt.Errorf("s7: tpkt length %d out of range", n)
	}
	if len(b) < n {
		return nil, fmt.Errorf("s7: tpkt truncated: have %d, want %d", len(b), n)
	}
	return b[4:n], nil
}

// COTPConnect 返回建立 ISO 连接的 COTP CR（已含 TPKT）。
//
// 参数是行业里通行的最小集合：class 0、TPDU 256、TSAP 0x0100/0x0102。
// S7-300/400/1200/1500 都接受这组；改成别的值不会更通用，只会更难排查。
func COTPConnect() []byte { return COTPConnectWith(0, 2) }

// COTPConnectWith 按机架/槽号生成 COTP CR。
//
// 目标 TSAP = 0x0100 + rack×32 + slot：这是 S7 协议把「CPU 在机架上的
// 物理位置」编码进 TSAP 的方式，不是厂商私有约定。同一台 PLC 换个槽位
// 就必须改这个值，否则连接请求会被 CPU 直接拒绝。
func COTPConnectWith(rack, slot int) []byte {
	if rack < 0 || rack > 7 {
		rack = 0
	}
	if slot < 0 || slot > 31 {
		slot = 2
	}
	dstTSAP := 0x0100 + rack*32 + slot
	return TPKT([]byte{
		0x11, COTPCR,
		0x00, 0x00, // dst ref
		0x00, 0x01, // src ref
		0x00,                   // class 0
		0xC1, 0x02, 0x01, 0x00, // TPDU size 256
		0xC2, 0x02, 0x01, 0x00, // src TSAP（PG/编程器）
		// 目的 TSAP = 0x0100 + rack×32 + slot，见官方 TSAP 结构说明。
		0xC0, 0x02, byte(dstTSAP >> 8), byte(dstTSAP),
	})
}

// ParseCOTPConnect 校验连接确认（CC）。
func ParseCOTPConnect(b []byte) error {
	p, err := ParseTPKT(b)
	if err != nil {
		return err
	}
	if len(p) < 2 {
		return errors.New("s7: cotp cc too short")
	}
	if p[1] != COTPCC {
		return fmt.Errorf("s7: cotp pdu type %#02x, want CC %#02x", p[1], COTPCC)
	}
	return nil
}

// HeaderLen 返回该类报文的 S7 头长度。
//
// 这是 S7 里最容易写错的一处：Job（请求）与 UserData 是 10 字节头，
// 而 ACK / ACK_DATA 在数据长度之后还多两个字节的 error class / error
// code，参数区从偏移 12 开始。把 ACK 当 10 字节解析会把这 2 个字节当成
// 参数区的前两个字节——单点读写的响应「看起来能解」，但参数长度一错，
// 后面的数据区就整体错位两字节。
func HeaderLen(rosctr byte) int {
	switch rosctr {
	case ROSCTRJobAck, ROSCTRAckData:
		return 12
	}
	return 10
}

// BuildPDU 生成 S7 头 + 参数区 + 数据区（不含 COTP/TPKT）。
//
// ACK / ACK_DATA 会自动带上两个零错误字节；要构造带错误的报文用
// BuildPDUError。
func BuildPDU(rosctr byte, ref uint16, param, data []byte) []byte {
	return buildPDU(rosctr, ref, 0, 0, param, data)
}

// BuildPDUError 生成带 error class / error code 的 ACK / ACK_DATA。
// 对 Job / UserData 调用时这两个参数没有位置，会被忽略。
func BuildPDUError(rosctr byte, ref uint16, class, code byte, param, data []byte) []byte {
	return buildPDU(rosctr, ref, class, code, param, data)
}

func buildPDU(rosctr byte, ref uint16, class, code byte, param, data []byte) []byte {
	hdr := HeaderLen(rosctr)
	out := make([]byte, hdr+len(param)+len(data))
	out[0] = S7ProtoID
	out[1] = rosctr
	// out[2:4] redundancy id = 0
	binary.BigEndian.PutUint16(out[4:6], ref)
	binary.BigEndian.PutUint16(out[6:8], uint16(len(param)))
	binary.BigEndian.PutUint16(out[8:10], uint16(len(data)))
	if hdr == 12 {
		out[10], out[11] = class, code
	}
	copy(out[hdr:], param)
	copy(out[hdr+len(param):], data)
	return out
}

// WrapDT 给 S7 PDU 加上 COTP DT 头与 TPKT 头，得到可直接写往 socket 的帧。
func WrapDT(pdu []byte) []byte {
	payload := make([]byte, 3+len(pdu))
	payload[0] = 0x02 // COTP 头长度
	payload[1] = COTPDT
	payload[2] = 0x80 // EOT
	copy(payload[3:], pdu)
	return TPKT(payload)
}

// PDU 是解析后的 S7 报文。
type PDU struct {
	ROSCTR   byte
	Ref      uint16
	ErrClass byte // 仅 ACK / ACK_DATA
	ErrCode  byte // 仅 ACK / ACK_DATA
	Param    []byte
	Data     []byte
}

// Error 返回报文头顶层的错误。nil 表示无错误。
//
// 顶层错误与「单项错误码」是两件事：顶层非零意味着整帧失败（例如功能
// 不支持、会话未建立），此时参数区与数据区的内容都不可信；单项返回码
// 在数据区里，只影响对应点位。
func (p PDU) Error() error {
	if p.ErrClass == 0 && p.ErrCode == 0 {
		return nil
	}
	return fmt.Errorf("s7: pdu error class=%#02x code=%#02x", p.ErrClass, p.ErrCode)
}

// Parse 剥离 COTP DT 与 S7 头，返回参数区与数据区。
func Parse(b []byte) (PDU, error) {
	payload, err := ParseTPKT(b)
	if err != nil {
		return PDU{}, err
	}
	// 连接确认（CC）与 DT 都出现在同一读循环里，调用方按需分流。
	if len(payload) < 2 {
		return PDU{}, errors.New("s7: cotp payload too short")
	}
	switch payload[1] {
	case COTPCC, COTPCR:
		return PDU{}, fmt.Errorf("s7: unexpected cotp pdu type %#02x", payload[1])
	case COTPDT:
		if len(payload) < 3 {
			return PDU{}, errors.New("s7: cotp dt too short")
		}
		payload = payload[3:]
	}
	if len(payload) < 10 {
		return PDU{}, fmt.Errorf("s7: s7 header too short (%d)", len(payload))
	}
	if payload[0] != S7ProtoID {
		return PDU{}, fmt.Errorf("s7: protocol id %#02x, want 0x32", payload[0])
	}
	hdr := HeaderLen(payload[1])
	if len(payload) < hdr {
		return PDU{}, fmt.Errorf("s7: s7 header too short for rosctr %#02x (%d)", payload[1], len(payload))
	}
	paramLen := int(binary.BigEndian.Uint16(payload[6:8]))
	dataLen := int(binary.BigEndian.Uint16(payload[8:10]))
	if len(payload) < hdr+paramLen+dataLen {
		return PDU{}, fmt.Errorf("s7: pdu truncated: header says %d, have %d",
			hdr+paramLen+dataLen, len(payload))
	}
	p := PDU{
		ROSCTR: payload[1],
		Ref:    binary.BigEndian.Uint16(payload[4:6]),
		Param:  payload[hdr : hdr+paramLen],
		Data:   payload[hdr+paramLen : hdr+paramLen+dataLen],
	}
	if hdr == 12 {
		p.ErrClass, p.ErrCode = payload[10], payload[11]
	}
	return p, nil
}

// SetupParams 生成「建立通信」参数区（请求最大 PDU 长度）。
func SetupParams(pduLen int) []byte {
	if pduLen <= 0 || pduLen > 960 {
		pduLen = 480
	}
	return []byte{FuncSetup, 0x00, 0x00, 0x01, 0x00, 0x01, byte(pduLen >> 8), byte(pduLen)}
}

// ParseSetupAck 返回对端协商出的 PDU 长度。
//
// 必须用协商值而不是自己请求的值：S7-300 常常只给 240，按 480 组包会
// 被直接拒绝（返回码 0x05 或整帧无响应），现场表现为「读多点就失败」。
func ParseSetupAck(pdu PDU) (int, error) {
	if pdu.ROSCTR != ROSCTRAckData && pdu.ROSCTR != ROSCTRJobAck {
		return 0, fmt.Errorf("s7: setup ack rosctr %#02x", pdu.ROSCTR)
	}
	if len(pdu.Param) < 8 || pdu.Param[0] != FuncSetup {
		return 0, fmt.Errorf("s7: setup ack param % x", pdu.Param)
	}
	n := int(binary.BigEndian.Uint16(pdu.Param[6:8]))
	if n < 64 {
		return 0, fmt.Errorf("s7: negotiated pdu length %d too small", n)
	}
	if n > 960 {
		n = 960
	}
	return n, nil
}

// itemSpec 生成一个 12 字节的 S7ANY 变量说明。
func itemSpec(it Item) []byte {
	ts := byte(TSByte)
	length := it.Bytes
	if it.BitAccess {
		ts = TSBit
		length = 1
	}
	addr := it.ByteAddr * 8
	if it.Bit > 0 {
		addr += it.Bit
	}
	out := make([]byte, 12)
	out[0] = 0x12 // variable specification
	out[1] = 0x0A // length of following address spec
	out[2] = 0x10 // syntax id: S7ANY
	out[3] = ts
	binary.BigEndian.PutUint16(out[4:6], uint16(length))
	binary.BigEndian.PutUint16(out[6:8], uint16(it.DB))
	out[8] = it.Area
	out[9] = byte(addr >> 16)
	out[10] = byte(addr >> 8)
	out[11] = byte(addr)
	return out
}

// BuildRead 生成读变量请求（不含 TPKT/COTP，由 WrapDT 包装）。
func BuildRead(ref uint16, items []Item) ([]byte, error) {
	if len(items) == 0 {
		return nil, errors.New("s7: read requires at least one item")
	}
	if len(items) > 20 {
		return nil, fmt.Errorf("s7: %d items exceeds per-request limit 20", len(items))
	}
	param := make([]byte, 0, 2+12*len(items))
	param = append(param, FuncRead, byte(len(items)))
	for _, it := range items {
		if err := validateItem(it); err != nil {
			return nil, err
		}
		param = append(param, itemSpec(it)...)
	}
	return BuildPDU(ROSCTRJob, ref, param, nil), nil
}

func validateItem(it Item) error {
	if it.Area != AreaInput && it.Area != AreaOutput && it.Area != AreaMerker &&
		it.Area != AreaDB && it.Area != AreaCounter && it.Area != AreaTimer {
		return fmt.Errorf("s7: unsupported area %#02x", it.Area)
	}
	if it.Area == AreaDB && it.DB < 1 {
		return errors.New("s7: db number required for DB area")
	}
	if it.Area != AreaDB && it.DB != 0 {
		return errors.New("s7: db number only valid for DB area")
	}
	if it.ByteAddr < 0 || it.ByteAddr > 0x7FFFF {
		return fmt.Errorf("s7: byte address %d out of range", it.ByteAddr)
	}
	if it.BitAccess {
		if it.Bit < 0 || it.Bit > 7 {
			return fmt.Errorf("s7: bit %d out of range 0..7", it.Bit)
		}
		return nil
	}
	if it.Bytes <= 0 {
		return fmt.Errorf("s7: byte length must be > 0, got %d", it.Bytes)
	}
	return nil
}

// BuildWrite 生成写变量请求。
//
// values[i] 必须与 items[i] 的宽度一致（位访问为 1 字节，其余为 Bytes）。
func BuildWrite(ref uint16, items []Item, values [][]byte) ([]byte, error) {
	if len(items) == 0 || len(items) != len(values) {
		return nil, fmt.Errorf("s7: write items/values length mismatch (%d/%d)", len(items), len(values))
	}
	param := make([]byte, 0, 2+12*len(items))
	param = append(param, FuncWrite, byte(len(items)))
	data := make([]byte, 0, 16*len(items))
	for i, it := range items {
		if err := validateItem(it); err != nil {
			return nil, err
		}
		param = append(param, itemSpec(it)...)

		want := it.Bytes
		ts := byte(RSByteWord)
		lengthBits := it.Bytes * 8
		if it.BitAccess {
			want = 1
			ts = RSBit
			lengthBits = 1
		}
		v := values[i]
		if len(v) != want {
			return nil, fmt.Errorf("s7: item %d value length %d, want %d", i, len(v), want)
		}
		data = append(data, 0x00, ts, byte(lengthBits>>8), byte(lengthBits))
		data = append(data, v...)
		if len(v)%2 == 1 {
			data = append(data, 0x00) // 数据区按偶数字节对齐
		}
	}
	return BuildPDU(ROSCTRJob, ref, param, data), nil
}

// ItemError 是单个变量的返回码。
type ItemError struct{ Code byte }

func (e ItemError) Error() string {
	return fmt.Sprintf("s7: item error %#02x (%s)", e.Code, e.CodeText())
}

func (e ItemError) CodeText() string {
	switch e.Code {
	case CodeOK:
		return "ok"
	case CodeHardware:
		return "hardware fault"
	case CodeAccessDenied:
		return "access denied"
	case CodeInvalidAddr:
		return "address out of range"
	case CodeTypeNotSupport:
		return "data type not supported"
	case CodeTypeInconsistent:
		return "data type inconsistent"
	case CodeObjectNotFound:
		return "object does not exist"
	}
	return "unknown"
}

// Kind 把 S7 返回码映射到统一错误分类。
//
// 地址不存在与越界必须映射成 ErrAddress：它们不会因为重试而变好，
// 也不应触发熔断（否则一个配错的点位会让整台设备反复熔断）。
func (e ItemError) Kind() driver.ErrorKind {
	switch e.Code {
	case CodeInvalidAddr, CodeObjectNotFound:
		return driver.ErrAddress
	case CodeAccessDenied:
		return driver.ErrPermission
	case CodeTypeNotSupport, CodeTypeInconsistent:
		return driver.ErrUnsupported
	case CodeHardware:
		return driver.ErrProtocol
	}
	return driver.ErrProtocol
}

// ParseReadAck 解析读响应，返回每项的原始字节与每一项的错误（nil 表示成功）。
func ParseReadAck(pdu PDU, items []Item) ([][]byte, []error, error) {
	if pdu.ROSCTR != ROSCTRAckData {
		return nil, nil, fmt.Errorf("s7: read ack rosctr %#02x", pdu.ROSCTR)
	}
	if len(pdu.Param) < 2 || pdu.Param[0] != FuncRead {
		return nil, nil, fmt.Errorf("s7: read ack param % x", pdu.Param)
	}
	count := int(pdu.Param[1])
	if count != len(items) {
		return nil, nil, fmt.Errorf("s7: read ack returned %d items, requested %d", count, len(items))
	}
	out := make([][]byte, count)
	errs := make([]error, count)
	off := 0
	for i := 0; i < count; i++ {
		if off+4 > len(pdu.Data) {
			return nil, nil, fmt.Errorf("s7: read ack data truncated at item %d", i)
		}
		code := pdu.Data[off]
		ts := pdu.Data[off+1]
		length := int(binary.BigEndian.Uint16(pdu.Data[off+2 : off+4]))
		off += 4
		if code != CodeOK {
			errs[i] = ItemError{Code: code}
			continue
		}
		payload := payloadBytes(ts, length)
		if off+payload > len(pdu.Data) {
			return nil, nil, fmt.Errorf("s7: read ack payload truncated at item %d (need %d)", i, payload)
		}
		out[i] = append([]byte(nil), pdu.Data[off:off+payload]...)
		off += payload
		if payload%2 == 1 {
			off++ // 偶数字节对齐
		}
	}
	return out, errs, nil
}

// payloadBytes 把响应里的「长度」换算成字节数。
//
// S7 的长度单位不一致是协议里最容易踩的坑：BIT 与 BYTE/WORD/DWORD 用
// 位，INT/DINT/REAL 等用字节。按错一种就会读出半个值或越过数据区。
func payloadBytes(ts byte, length int) int {
	switch ts {
	case RSBit, RSByteWord:
		return (length + 7) / 8
	}
	return length
}

// ParseWriteAck 解析写响应，返回每项的错误（nil 表示成功）。
func ParseWriteAck(pdu PDU, count int) ([]error, error) {
	if pdu.ROSCTR != ROSCTRAckData && pdu.ROSCTR != ROSCTRJobAck {
		return nil, fmt.Errorf("s7: write ack rosctr %#02x", pdu.ROSCTR)
	}
	if len(pdu.Param) < 2 || pdu.Param[0] != FuncWrite {
		return nil, fmt.Errorf("s7: write ack param % x", pdu.Param)
	}
	if int(pdu.Param[1]) != count {
		return nil, fmt.Errorf("s7: write ack count %d, requested %d", pdu.Param[1], count)
	}
	errs := make([]error, count)
	for i := 0; i < count && i < len(pdu.Data); i++ {
		if pdu.Data[i] != CodeOK {
			errs[i] = ItemError{Code: pdu.Data[i]}
		}
	}
	return errs, nil
}

// AreaOf 把配置里的区名映射为 S7 区域码。
func AreaOf(name string) (byte, error) {
	switch name {
	case "input", "i", "e":
		return AreaInput, nil
	case "output", "q", "a":
		return AreaOutput, nil
	case "merker", "memory", "m", "flag":
		return AreaMerker, nil
	case "db":
		return AreaDB, nil
	case "counter", "c":
		return AreaCounter, nil
	case "timer", "t":
		return AreaTimer, nil
	}
	return 0, fmt.Errorf("s7: unknown area %q", name)
}

// ItemOf 把一个点位转换成 S7 变量说明。
func ItemOf(p driver.PointRef) (Item, error) {
	area, err := AreaOf(p.Address.Area)
	if err != nil {
		return Item{}, err
	}
	it := Item{Area: area, DB: p.Address.DB, ByteAddr: p.Address.Offset, Bit: -1}
	if p.DataType == model.TypeBool {
		if p.Address.Bit < 0 || p.Address.Bit > 7 {
			return Item{}, fmt.Errorf("s7: bool point %q needs address.bit 0..7, got %d", p.ID, p.Address.Bit)
		}
		it.Bit = p.Address.Bit
		it.BitAccess = true
		return it, nil
	}
	// 非 bool 点位的 bit 没有意义。允许 0 是因为 JSON 省略 bit 时零值就是 0，
	// 而「显式写 bit: 3 但类型是 float32」几乎一定是配错了，必须报出来。
	if p.Address.Bit > 0 {
		return Item{}, fmt.Errorf("s7: non-bool point %q must not set address.bit (%d)", p.ID, p.Address.Bit)
	}
	width := p.DataType.Bytes()
	if width <= 0 {
		// string / bytes：宽度由点位显式长度决定。
		if p.Length <= 0 {
			return Item{}, fmt.Errorf("s7: point %q needs explicit length for %s", p.ID, p.DataType)
		}
		width = p.Length
	}
	it.Bytes = width
	return it, nil
}
