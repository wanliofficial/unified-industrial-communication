// Package modbus 实现 Modbus 的纯函数编解码层。
//
// 该包不做任何 I/O、不读时钟、不持有可变状态，因此同一份代码被主站、
// 从站模拟器与金报文测试共同使用。
package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

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
)

// 单次请求上限（协议规定）。
const (
	MaxReadRegisters = 125
	MaxReadBits      = 2000
)

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
		if area != "coil" && area != "coils" {
			return nil, driver.E(driver.ErrUnsupported, "encode", c.DeviceID, "bool write requires coil area", false, nil)
		}
		pdu := make([]byte, 5)
		pdu[0] = FCWriteSingleCoil
		binary.BigEndian.PutUint16(pdu[1:3], uint16(req.Address.Offset))
		if req.Value.Bool {
			binary.BigEndian.PutUint16(pdu[3:5], 0xFF00)
		}
		return pdu, nil
	}
	if !req.DataType.IsNumeric() {
		return nil, driver.E(driver.ErrUnsupported, "encode", c.DeviceID, "non-numeric write unsupported", false, nil)
	}
	// FC6/FC16 只能写保持寄存器。input/discrete 是只读区：若放行，会把值
	// 写进 holding 的同一偏移，造成「写成功了但写错了地方」的静默错误。
	if area != "holding" && area != "holding_register" && area != "hr" {
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
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// ---------------------------------------------------------------------------
// 值编解码
// ---------------------------------------------------------------------------

// Reorder 按字序策略重排 16 位字与字节。该变换是对称的（编码与解码共用）。
func Reorder(b []byte, order model.ByteOrder) []byte {
	if len(b) < 2 {
		return b
	}
	out := make([]byte, len(b))
	copy(out, b)
	switch order {
	case model.OrderCDAB:
		// 字（16 位）顺序反转，字内字节保持大端。
		words := len(out) / 2
		for i := 0; i < words/2; i++ {
			j := words - 1 - i
			out[i*2], out[j*2] = out[j*2], out[i*2]
			out[i*2+1], out[j*2+1] = out[j*2+1], out[i*2+1]
		}
	case model.OrderDCBA:
		// 整值小端：全部字节反转（低字在前 + 字内小端）。
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	case model.OrderBADC:
		// 字内字节交换，字顺序不变。
		for i := 0; i+1 < len(out); i += 2 {
			out[i], out[i+1] = out[i+1], out[i]
		}
	}
	return out
}

func decodeValue(b []byte, t model.DataType, order model.ByteOrder) (model.Value, error) {
	switch t {
	case model.TypeString:
		return model.TextValue(t, strings.TrimRight(string(b), "\x00")), nil
	case model.TypeBytes:
		return model.BytesValue(b), nil
	}
	if len(b) < t.Bytes() {
		return model.Value{}, fmt.Errorf("modbus: need %d bytes for %s, got %d", t.Bytes(), t, len(b))
	}
	raw := Reorder(b[:t.Bytes()], order)
	switch t {
	case model.TypeInt16:
		return model.NumValue(t, float64(int16(binary.BigEndian.Uint16(raw)))), nil
	case model.TypeUint16:
		return model.NumValue(t, float64(binary.BigEndian.Uint16(raw))), nil
	case model.TypeInt32:
		return model.NumValue(t, float64(int32(binary.BigEndian.Uint32(raw)))), nil
	case model.TypeUint32:
		return model.NumValue(t, float64(binary.BigEndian.Uint32(raw))), nil
	case model.TypeInt64:
		return model.NumValue(t, float64(int64(binary.BigEndian.Uint64(raw)))), nil
	case model.TypeUint64:
		return model.NumValue(t, float64(binary.BigEndian.Uint64(raw))), nil
	case model.TypeFloat32:
		return model.NumValue(t, float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))), nil
	case model.TypeFloat64:
		return model.NumValue(t, math.Float64frombits(binary.BigEndian.Uint64(raw))), nil
	case model.TypeBCD16:
		v, err := decodeBCD(raw)
		return model.NumValue(t, v), err
	case model.TypeBCD32:
		v, err := decodeBCD(raw)
		return model.NumValue(t, v), err
	}
	return model.Value{}, fmt.Errorf("modbus: unsupported dataType %q", t)
}

func encodeValue(v model.Value, t model.DataType, order model.ByteOrder) ([]byte, error) {
	n := t.Bytes()
	if n <= 0 {
		return nil, fmt.Errorf("modbus: unsupported write type %q", t)
	}
	raw := make([]byte, n)
	switch t {
	case model.TypeInt16:
		binary.BigEndian.PutUint16(raw, uint16(int16(v.Num)))
	case model.TypeUint16:
		binary.BigEndian.PutUint16(raw, uint16(v.Num))
	case model.TypeInt32:
		binary.BigEndian.PutUint32(raw, uint32(int32(v.Num)))
	case model.TypeUint32:
		binary.BigEndian.PutUint32(raw, uint32(v.Num))
	case model.TypeInt64:
		binary.BigEndian.PutUint64(raw, uint64(int64(v.Num)))
	case model.TypeUint64:
		binary.BigEndian.PutUint64(raw, uint64(v.Num))
	case model.TypeFloat32:
		binary.BigEndian.PutUint32(raw, math.Float32bits(float32(v.Num)))
	case model.TypeFloat64:
		binary.BigEndian.PutUint64(raw, math.Float64bits(v.Num))
	case model.TypeBCD16, model.TypeBCD32:
		encodeBCD(raw, v.Num)
	default:
		return nil, fmt.Errorf("modbus: unsupported write type %q", t)
	}
	return Reorder(raw, order), nil
}

func decodeBCD(b []byte) (float64, error) {
	var out float64
	for _, x := range b {
		hi, lo := x>>4, x&0x0F
		if hi > 9 || lo > 9 {
			return 0, errors.New("modbus: invalid BCD digit")
		}
		out = out*100 + float64(hi)*10 + float64(lo)
	}
	return out, nil
}

func encodeBCD(b []byte, v float64) {
	n := int64(math.Round(v))
	for i := len(b) - 1; i >= 0; i-- {
		lo := byte(n % 10)
		n /= 10
		hi := byte(n % 10)
		n /= 10
		b[i] = hi<<4 | lo
	}
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
