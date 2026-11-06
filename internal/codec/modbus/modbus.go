// Package modbus 实现 Modbus 的纯函数编解码层。
//
// 该包不做任何 I/O、不读时钟、不持有可变状态，因此同一份代码被主站、
// 从站模拟器与金报文测试共同使用。
package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
)

// 功能码。
const (
	FCReadCoils           byte = 0x01
	FCReadDiscreteInputs  byte = 0x02
	FCReadHoldingRegs     byte = 0x03
	FCReadInputRegs       byte = 0x04
	FCWriteSingleCoil     byte = 0x05
	FCWriteSingleRegister byte = 0x06
	FCWriteMultipleCoils  byte = 0x0F
	FCWriteMultipleRegs   byte = 0x10

	// 诊断与识别类功能码（从站侧）。
	FC08 byte = 0x08 // Diagnostics（子功能：回显/清计数/各类计数）
	FC11 byte = 0x0B // Get Comm Event Counter
	FC17 byte = 0x11 // Report Server ID
	FC43 byte = 0x2B // Encapsulated Interface Transport（MEI 0x0E 读设备标识）
)

// 单次请求上限（协议规定）。
const (
	MaxReadRegisters = 125
	MaxReadBits      = 2000
)

// RTUMaxFrame 是 RTU 帧的字节上限：地址(1) + PDU(253) + CRC(2)。
//
// 从站按它给「长度无法推导、只能读到静默」的帧兜底：没有上限时，一条
// 持续有噪声的线路可以让缓冲区无限增长。
const RTUMaxFrame = 1 + 253 + 2

// Codec 是 Modbus 主站侧的编解码器。
type Codec struct {
	DeviceID string
}

// NewCodec 构造编解码器。
func NewCodec(deviceID string) *Codec { return &Codec{DeviceID: deviceID} }

func functionForArea(area string) (byte, bool, error) {
	switch strings.ToLower(area) {
	case "coil", "coils":
		return FCReadCoils, true, nil
	case "discrete", "discrete_input", "discreteinput":
		return FCReadDiscreteInputs, true, nil
	case "holding", "holding_register", "hr":
		return FCReadHoldingRegs, false, nil
	case "input", "input_register", "ir":
		return FCReadInputRegs, false, nil
	}
	return 0, false, fmt.Errorf("modbus: unknown area %q", area)
}

func spanWindow(span driver.ReadSpan) (area string, start, qty int, bits bool, err error) {
	if len(span.Points) == 0 {
		return "", 0, 0, false, errors.New("modbus: empty span")
	}
	area = span.Points[0].Address.Area
	fc, isBits, ferr := functionForArea(area)
	_ = fc
	if ferr != nil {
		return "", 0, 0, false, ferr
	}
	start = span.Points[0].Address.Offset
	end := start
	for _, p := range span.Points {
		if p.Address.Kind != model.AddressRegister {
			return "", 0, 0, false, fmt.Errorf("modbus: unsupported address kind %q", p.Address.Kind)
		}
		if !strings.EqualFold(p.Address.Area, area) {
			return "", 0, 0, false, errors.New("modbus: mixed areas in one span")
		}
		l := p.Length
		if isBits {
			l = 1
		}
		if l <= 0 {
			l = 1
		}
		if p.Address.Offset < start {
			start = p.Address.Offset
		}
		if e := p.Address.Offset + l; e > end {
			end = e
		}
	}
	qty = end - start
	if isBits && qty > MaxReadBits {
		return "", 0, 0, false, fmt.Errorf("modbus: %d bits exceeds limit %d", qty, MaxReadBits)
	}
	if !isBits && qty > MaxReadRegisters {
		return "", 0, 0, false, fmt.Errorf("modbus: %d registers exceeds limit %d", qty, MaxReadRegisters)
	}
	if qty <= 0 {
		return "", 0, 0, false, errors.New("modbus: empty window")
	}
	return area, start, qty, isBits, nil
}

// EncodeRead 生成一个读请求 PDU。
func (c *Codec) EncodeRead(span driver.ReadSpan) ([]byte, error) {
	area, start, qty, _, err := spanWindow(span)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "encode", c.DeviceID, err.Error(), false, err)
	}
	fc, _, _ := functionForArea(area)
	pdu := make([]byte, 5)
	pdu[0] = fc
	binary.BigEndian.PutUint16(pdu[1:3], uint16(start))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	return pdu, nil
}

// DecodeRead 解析读响应 PDU，并按点位窗口回填。
func (c *Codec) DecodeRead(span driver.ReadSpan, resp []byte) ([]driver.RawSample, error) {
	area, start, qty, bits, err := spanWindow(span)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "decode", c.DeviceID, err.Error(), false, err)
	}
	fc, _, _ := functionForArea(area)
	if len(resp) == 0 {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID, "empty response", true, nil)
	}
	if resp[0]&0x80 != 0 {
		return nil, exceptionError(c.DeviceID, resp)
	}
	if resp[0] != fc {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("function echo %#x, want %#x", resp[0], fc), true, nil)
	}
	if len(resp) < 2 {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID, "missing byte count", true, nil)
	}
	bc := int(resp[1])
	if len(resp) < 2+bc {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("truncated payload: have %d bytes, need %d", len(resp)-2, bc), true, nil)
	}
	data := resp[2 : 2+bc]

	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		rel := p.Address.Offset - start
		if bits {
			bit := rel
			if bit < 0 || bit/8 >= len(data) {
				out = append(out, bad(p.ID, model.QualityAddressError, "bit offset out of range"))
				continue
			}
			v := data[bit/8]&(1<<uint(bit%8)) != 0
			out = append(out, driver.RawSample{PointID: p.ID, Value: model.BoolValue(v), Quality: model.QualityGood})
			continue
		}
		n := p.Length * 2
		off := rel * 2
		if off < 0 || off+n > len(data) {
			out = append(out, bad(p.ID, model.QualityAddressError, "register window out of range"))
			continue
		}
		// 寄存器内的位访问：bit >= 0 取对应位，bit == -1 时整字非零即真。
		// 这条路径之前是缺失的，而能力表却声明了 BitAccess=supported。
		if p.DataType == model.TypeBool {
			word := binary.BigEndian.Uint16(data[off : off+2])
			var v bool
			if p.Address.Bit >= 0 && p.Address.Bit <= 15 {
				v = word&(1<<uint(p.Address.Bit)) != 0
			} else {
				v = word != 0
			}
			out = append(out, driver.RawSample{PointID: p.ID, Value: model.BoolValue(v), Quality: model.QualityGood})
			continue
		}
		val, derr := decodeValue(data[off:off+n], p.DataType, p.ByteOrder)
		if derr != nil {
			out = append(out, bad(p.ID, model.QualityBad, derr.Error()))
			continue
		}
		out = append(out, driver.RawSample{PointID: p.ID, Value: val, Quality: model.QualityGood})
	}
	_ = qty
	return out, nil
}

// kindOfQuality 把质量码映射为错误分类。两者是不同维度：质量码描述「值可不可信」，
// 错误分类描述「上层该不该重试/熔断」。直接类型转换会得到 "address_error"
// 这类不存在的 ErrorKind，导致地址错误被误判为不可重试的内部错误。
func kindOfQuality(q model.Quality) driver.ErrorKind {
	switch q {
	case model.QualityTimeout:
		return driver.ErrTimeout
	case model.QualityCommError, model.QualityNotConnected:
		return driver.ErrDisconnected
	case model.QualityAddressError:
		return driver.ErrAddress
	case model.QualityDisabled:
		return driver.ErrUnsupported
	}
	return driver.ErrProtocol
}

func bad(point string, q model.Quality, msg string) driver.RawSample {
	return driver.RawSample{
		PointID: point, Quality: q,
		Err: driver.E(kindOfQuality(q), "decode", "", msg, false, nil),
	}
}

// EncodeWrite 生成单点写请求 PDU。批量写由 Conn 逐项调用（写为低频路径）。
func (c *Codec) EncodeWrite(req driver.WriteRequest) ([]byte, error) {
	if req.Address.Kind != model.AddressRegister {
		return nil, driver.E(driver.ErrUnsupported, "encode", c.DeviceID, "only register addresses are writable", false, nil)
	}
	area := strings.ToLower(req.Address.Area)
	if req.DataType == model.TypeBool {
		switch {
		case isCoilArea(area):
			pdu := make([]byte, 5)
			pdu[0] = FCWriteSingleCoil
			binary.BigEndian.PutUint16(pdu[1:3], uint16(req.Address.Offset))
			if req.Value.Bool {
				binary.BigEndian.PutUint16(pdu[3:5], 0xFF00)
			}
			return pdu, nil
		case isHoldingArea(area):
			// 写寄存器里的某一位是「两帧事务」：先读回整字，改位，再写回。
			// 纯函数编解码层只负责单帧，所以这里明确拒绝；驱动在
			// writeBitRMW 里完成整个事务。如果放行单帧写，会把整字覆盖成
			// 0/1，把同一寄存器里的其他位全部清掉。
			return nil, driver.E(driver.ErrUnsupported, "encode", c.DeviceID,
				"bool write into a holding register requires address.bit and is done by the driver (read-modify-write)", false, nil)
		default:
			return nil, driver.E(driver.ErrPermission, "encode", c.DeviceID,
				fmt.Sprintf("area %q is read-only", req.Address.Area), false, nil)
		}
	}
	if !req.DataType.IsNumeric() {
		return nil, driver.E(driver.ErrUnsupported, "encode", c.DeviceID, "non-numeric write unsupported", false, nil)
	}
	// FC6/FC16 只能写保持寄存器。input/discrete 是只读区：若放行，会把值
	// 写进 holding 的同一偏移，造成「写成功了但写错了地方」的静默错误。
	if !isHoldingArea(area) {
		return nil, driver.E(driver.ErrPermission, "encode", c.DeviceID,
			fmt.Sprintf("area %q is read-only", req.Address.Area), false, nil)
	}
	payload, err := encodeValue(req.Value, req.DataType, req.ByteOrder)
	if err != nil {
		return nil, driver.E(driver.ErrInternal, "encode", c.DeviceID, err.Error(), false, err)
	}
	regs := len(payload) / 2
	if regs <= 1 {
		pdu := make([]byte, 5)
		pdu[0] = FCWriteSingleRegister
		binary.BigEndian.PutUint16(pdu[1:3], uint16(req.Address.Offset))
		copy(pdu[3:5], payload)
		return pdu, nil
	}
	pdu := make([]byte, 6+len(payload))
	pdu[0] = FCWriteMultipleRegs
	binary.BigEndian.PutUint16(pdu[1:3], uint16(req.Address.Offset))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(regs))
	pdu[5] = byte(len(payload))
	copy(pdu[6:], payload)
	return pdu, nil
}

// EncodeReadRegisters 生成 FC3 读保持寄存器 PDU。读-改-写用它先取回整字。
func (c *Codec) EncodeReadRegisters(offset, qty int) ([]byte, error) {
	if offset < 0 || offset > 0xFFFF {
		return nil, driver.E(driver.ErrAddress, "encode", c.DeviceID,
			fmt.Sprintf("register offset %d out of range 0..65535", offset), false, nil)
	}
	if qty <= 0 || qty > MaxReadRegisters {
		return nil, driver.E(driver.ErrInternal, "encode", c.DeviceID,
			fmt.Sprintf("register quantity %d out of range 1..%d", qty, MaxReadRegisters), false, nil)
	}
	pdu := make([]byte, 5)
	pdu[0] = FCReadHoldingRegs
	binary.BigEndian.PutUint16(pdu[1:3], uint16(offset))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	return pdu, nil
}

// DecodeRegisters 解析 FC3 响应为 16 位字（大端）。
//
// 它只做「字」这一层的解析，不绑定数据类型：读-改-写关心的是原始位，
// 用数据类型解码再编码会引入缩放与字节序两处不必要的失真。
func (c *Codec) DecodeRegisters(resp []byte, qty int) ([]uint16, error) {
	if len(resp) == 0 {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID, "empty response", true, nil)
	}
	if resp[0]&0x80 != 0 {
		return nil, exceptionError(c.DeviceID, resp)
	}
	if resp[0] != FCReadHoldingRegs {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("function echo %#x, want %#x", resp[0], FCReadHoldingRegs), true, nil)
	}
	if len(resp) < 2 {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID, "missing byte count", true, nil)
	}
	bc := int(resp[1])
	if bc != qty*2 {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("byte count %d, want %d", bc, qty*2), true, nil)
	}
	if len(resp) < 2+bc {
		return nil, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("truncated payload: have %d bytes, need %d", len(resp)-2, bc), true, nil)
	}
	out := make([]uint16, qty)
	for i := 0; i < qty; i++ {
		out[i] = binary.BigEndian.Uint16(resp[2+i*2:])
	}
	return out, nil
}

// EncodeWriteRegister 生成 FC6 写单个寄存器 PDU。
func (c *Codec) EncodeWriteRegister(offset int, word uint16) ([]byte, error) {
	if offset < 0 || offset > 0xFFFF {
		return nil, driver.E(driver.ErrAddress, "encode", c.DeviceID,
			fmt.Sprintf("register offset %d out of range 0..65535", offset), false, nil)
	}
	pdu := make([]byte, 5)
	pdu[0] = FCWriteSingleRegister
	binary.BigEndian.PutUint16(pdu[1:3], uint16(offset))
	binary.BigEndian.PutUint16(pdu[3:5], word)
	return pdu, nil
}

// WordSetBit 返回把 word 的第 bit 位置为 v 后的值。
//
// 读-改-写的全部正确性都压在这一行上：mask 只碰目标位，其余 15 位原样保留。
func WordSetBit(word uint16, bit int, v bool) uint16 {
	if bit < 0 || bit > 15 {
		return word
	}
	mask := uint16(1) << uint(bit)
	if v {
		return word | mask
	}
	return word &^ mask
}

func isCoilArea(area string) bool {
	return area == "coil" || area == "coils"
}

func isHoldingArea(area string) bool {
	return area == "holding" || area == "holding_register" || area == "hr"
}

// IsHoldingArea 供驱动判断某个区是否可写（FC6/FC16 只能写保持寄存器）。
func IsHoldingArea(area string) bool { return isHoldingArea(strings.ToLower(area)) }

// DecodeWrite 校验写响应。
func (c *Codec) DecodeWrite(req driver.WriteRequest, resp []byte) (driver.WriteResult, error) {
	if len(resp) == 0 {
		return driver.WriteResult{PointID: req.PointID}, driver.E(driver.ErrProtocol, "decode", c.DeviceID, "empty response", true, nil)
	}
	if resp[0]&0x80 != 0 {
		return driver.WriteResult{PointID: req.PointID}, exceptionError(c.DeviceID, resp)
	}
	if resp[0] != FCWriteSingleCoil && resp[0] != FCWriteSingleRegister && resp[0] != FCWriteMultipleRegs {
		return driver.WriteResult{PointID: req.PointID}, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("unexpected write echo %#x", resp[0]), true, nil)
	}
	if len(resp) < 5 {
		return driver.WriteResult{PointID: req.PointID}, driver.E(driver.ErrProtocol, "decode", c.DeviceID, "short write response", true, nil)
	}
	// 回显地址必须与请求一致：总线上串了回包（或从站把地址理解错）时，
	// 不回读确认的写入会「成功」得毫无声息。值本身不比较——有些设备会对
	// 线圈/寄存器做掩码，只回显地址与数量。
	if got, want := binary.BigEndian.Uint16(resp[1:3]), uint16(req.Address.Offset); got != want {
		return driver.WriteResult{PointID: req.PointID}, driver.E(driver.ErrProtocol, "decode", c.DeviceID,
			fmt.Sprintf("write echo address %d, want %d", got, want), true, nil)
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// ---------------------------------------------------------------------------
// 值编解码
// ---------------------------------------------------------------------------

// Reorder 按字序策略重排 16 位字与字节。该变换是对称的（编码与解码共用）。
// Reorder 委托给 codec/value：值语义必须在所有协议间唯一。
func Reorder(b []byte, order model.ByteOrder) []byte { return value.Reorder(b, order) }

// decodeValue 委托给 codec/value（见该包文档：为什么不做协议内私有实现）。
func decodeValue(b []byte, t model.DataType, order model.ByteOrder) (model.Value, error) {
	return value.Decode(b, t, order)
}

func encodeValue(v model.Value, t model.DataType, order model.ByteOrder) ([]byte, error) {
	return value.Encode(v, t, order)
}

// ---------------------------------------------------------------------------
// 帧层（MBAP）与异常
// ---------------------------------------------------------------------------

// Frame 是 Modbus TCP 的应用数据单元。
type Frame struct {
	Transaction uint16
	Unit        uint8
	PDU         []byte
}

// ParseTCP 解析 MBAP + PDU，校验长度一致性。
func ParseTCP(b []byte) (Frame, error) {
	if len(b) < 8 {
		return Frame{}, fmt.Errorf("modbus: frame too short (%d)", len(b))
	}
	if binary.BigEndian.Uint16(b[2:4]) != 0 {
		return Frame{}, errors.New("modbus: protocol id must be 0")
	}
	length := int(binary.BigEndian.Uint16(b[4:6]))
	if length < 2 || len(b) < 6+length {
		return Frame{}, fmt.Errorf("modbus: bad length %d for %d bytes", length, len(b))
	}
	return Frame{
		Transaction: binary.BigEndian.Uint16(b[0:2]),
		Unit:        b[6],
		PDU:         append([]byte(nil), b[7:6+length]...),
	}, nil
}

// BuildTCP 生成 MBAP + PDU。
func BuildTCP(f Frame) []byte {
	out := make([]byte, 7+len(f.PDU))
	binary.BigEndian.PutUint16(out[0:2], f.Transaction)
	binary.BigEndian.PutUint16(out[2:4], 0)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(f.PDU)+1))
	out[6] = f.Unit
	copy(out[7:], f.PDU)
	return out
}

// ExceptionPDU 生成异常响应 PDU。
func ExceptionPDU(fc, code byte) []byte { return []byte{fc | 0x80, code} }

// ParseRequestPDU 解析读/写请求，供从站模拟器使用。
func ParseRequestPDU(pdu []byte) (fc byte, addr, qty int, values []byte, err error) {
	if len(pdu) < 1 {
		return 0, 0, 0, nil, errors.New("modbus: empty pdu")
	}
	fc = pdu[0]
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegs, FCReadInputRegs:
		if len(pdu) < 5 {
			return 0, 0, 0, nil, errors.New("modbus: short read pdu")
		}
		return fc, int(binary.BigEndian.Uint16(pdu[1:3])), int(binary.BigEndian.Uint16(pdu[3:5])), nil, nil
	case FCWriteSingleCoil, FCWriteSingleRegister:
		if len(pdu) < 5 {
			return 0, 0, 0, nil, errors.New("modbus: short write pdu")
		}
		return fc, int(binary.BigEndian.Uint16(pdu[1:3])), 1, pdu[3:5], nil
	case FCWriteMultipleCoils, FCWriteMultipleRegs:
		if len(pdu) < 6 {
			return 0, 0, 0, nil, errors.New("modbus: short multi-write pdu")
		}
		n := int(binary.BigEndian.Uint16(pdu[3:5]))
		bc := int(pdu[5])
		if len(pdu) < 6+bc {
			return 0, 0, 0, nil, errors.New("modbus: truncated multi-write pdu")
		}
		return fc, int(binary.BigEndian.Uint16(pdu[1:3])), n, pdu[6 : 6+bc], nil
	}
	return fc, 0, 0, nil, fmt.Errorf("modbus: unsupported function %#x", fc)
}

func exceptionError(device string, resp []byte) *driver.OpError {
	if len(resp) < 2 {
		return driver.E(driver.ErrProtocol, "decode", device, "short exception response", true, nil)
	}
	code := resp[1]
	switch code {
	case 1:
		return driver.E(driver.ErrProtocol, "decode", device, "illegal function (01)", false, nil)
	case 2:
		return driver.E(driver.ErrAddress, "decode", device, "illegal data address (02)", false, nil)
	case 3:
		return driver.E(driver.ErrProtocol, "decode", device, "illegal data value (03)", false, nil)
	case 4:
		return driver.E(driver.ErrProtocol, "decode", device, "server device failure (04)", true, nil)
	case 6:
		return driver.E(driver.ErrProtocol, "decode", device, "server device busy (06)", true, nil)
	}
	return driver.E(driver.ErrProtocol, "decode", device, fmt.Sprintf("exception code %#x", code), false, nil)
}

// ---------------------------------------------------------------------------
// CRC16（RTU）
// ---------------------------------------------------------------------------

// CRC16 计算 Modbus RTU 的 CRC-16/MODBUS。
func CRC16(b []byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// BuildRTU 生成 RTU 帧（地址 + PDU + CRC 小端）。
func BuildRTU(unit byte, pdu []byte) []byte {
	out := make([]byte, 0, len(pdu)+3)
	out = append(out, unit)
	out = append(out, pdu...)
	crc := CRC16(out)
	return append(out, byte(crc), byte(crc>>8))
}

// RTURequestLen 由已收到的帧头推导**请求**整帧长度（地址 + PDU + CRC）。
//
// head 至少要包含地址与功能码（2 字节）。返回 ok=false 表示无法推导：
// 功能码未知，或（FC15/16）字节数字段还没收到。调用方应当退回「读到
// 链路静默」的分帧方式——绝不能猜：猜短了会把下一帧的开头当成这一帧
// 的尾巴，猜长了会一直等一段永不到来的数据。
//
// 这里的长度表是**从站**解析请求用的；主站侧对应的表是
// RTUExpectedResponseLen，两者不要合并：请求与响应的 PDU 形状不同，
// 合成一张表会制造「读写都能过、但长度源不同」的隐式耦合。
func RTURequestLen(head []byte) (int, bool) {
	if len(head) < 2 {
		return 0, false
	}
	switch head[1] {
	// 读请求（地址 + FC + 起始地址 + 数量 + CRC）与单写请求
	// （地址 + FC + 地址 + 值 + CRC）都是 8 字节；FC8 的诊断请求
	// （子功能 + 数据各 2 字节）也是 8 字节。
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegs, FCReadInputRegs,
		FCWriteSingleCoil, FCWriteSingleRegister, FC08:
		return 8, true
	// 没有数据字段的短请求。
	case FC11, FC17:
		return 4, true
	case FCWriteMultipleCoils, FCWriteMultipleRegs:
		// 字节数字段在帧内偏移 6（地址 1 + FC 1 + 起始 2 + 数量 2）。
		if len(head) < 7 {
			return 0, false
		}
		return 9 + int(head[6]), true
	case FC43:
		// 只实现 MEI 0x0E（读设备标识），请求固定 4 字节 PDU；
		// 其他 MEI 是变长封装，交给静默分帧。
		if len(head) < 3 {
			return 0, false
		}
		if head[2] == 0x0E {
			return 7, true
		}
		return 0, false
	}
	return 0, false
}

// RTUExpectedResponseLen 由请求 PDU 推导**正常**响应的帧总长（地址 + PDU + CRC）。
//
// 相比「逐字节读到长度可判定」，这条路径快一个数量级：一次 2 字节探测
// 之后就能整段读满，不必每字节一次系统调用。更重要的是它把「字节计数
// 被写坏」变成了立即可判的错误——否则主站会一直等到设备超时才失败，
// 现场看到的是「偶发 3 秒延迟」而不是「从站返回了非法字节数」。
//
// 异常响应（FC|0x80）恒为 5 字节，不在本函数职责内。
func RTUExpectedResponseLen(reqPDU []byte) (int, error) {
	if len(reqPDU) < 1 {
		return 0, errors.New("modbus: empty request pdu")
	}
	switch fc := reqPDU[0]; fc {
	case FCReadCoils, FCReadDiscreteInputs:
		if len(reqPDU) < 5 {
			return 0, fmt.Errorf("modbus: short FC%#x request pdu (%d bytes)", fc, len(reqPDU))
		}
		qty := int(binary.BigEndian.Uint16(reqPDU[3:5]))
		if qty <= 0 || qty > MaxReadBits {
			return 0, fmt.Errorf("modbus: FC%#x quantity %d out of range", fc, qty)
		}
		return 5 + (qty+7)/8, nil
	case FCReadHoldingRegs, FCReadInputRegs:
		if len(reqPDU) < 5 {
			return 0, fmt.Errorf("modbus: short FC%#x request pdu (%d bytes)", fc, len(reqPDU))
		}
		qty := int(binary.BigEndian.Uint16(reqPDU[3:5]))
		if qty <= 0 || qty > MaxReadRegisters {
			return 0, fmt.Errorf("modbus: FC%#x quantity %d out of range", fc, qty)
		}
		return 5 + qty*2, nil
	case FCWriteSingleCoil, FCWriteSingleRegister, FCWriteMultipleCoils, FCWriteMultipleRegs:
		// 写响应是回显：地址 + FC + 2 字节地址 + 2 字节值/数量 + CRC。
		return 8, nil
	}
	return 0, fmt.Errorf("modbus: cannot predict RTU response length for fc %#x", reqPDU[0])
}

// ParseRTU 校验并剥离 RTU 帧的地址与 CRC。
func ParseRTU(b []byte) (unit byte, pdu []byte, err error) {
	if len(b) < 4 {
		return 0, nil, fmt.Errorf("modbus: rtu frame too short (%d)", len(b))
	}
	body := b[:len(b)-2]
	want := CRC16(body)
	got := uint16(b[len(b)-2]) | uint16(b[len(b)-1])<<8
	if want != got {
		return 0, nil, fmt.Errorf("modbus: crc mismatch want %#04x got %#04x", want, got)
	}
	return b[0], append([]byte(nil), b[1:len(b)-2]...), nil
}
