package enip

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ---------------------------------------------------------
// Class 3 显式报文连接
// ---------------------------------------------------------
//
// Read/Write Tag Fragmented（0x52/0x53）不是无连接服务：它们要求先对
// Connection Manager（class 0x06 / instance 0x01）做 Forward Open，之后
// 报文用封装命令 SendUnitData（0x0070）发出，CPF 里带连接 ID 与序列号。
//
// 本文件只编码/解码这一层，不持有连接状态；连接 ID、序列号自增与丢失后
// 重开由 driver 层负责。

// TransportClass3 是 Class 3（显式报文、应用对象触发）的传输类别字节。
const TransportClass3 byte = 0xA3

// ConnectionManagerPath 返回 Connection Manager 对象的逻辑路径。
func ConnectionManagerPath() ([]byte, error) { return LogicalPath(0x06, 0x01) }

// MessageRouterPath 返回 Message Router 对象的逻辑路径。Class 3 显式连接
// 的连接路径指向它，后续 SendUnitData 里的 CIP 报文才会被路由到标签服务。
func MessageRouterPath() ([]byte, error) { return LogicalPath(0x02, 0x01) }

// ForwardOpen 是 Forward Open（0x54）/ Large Forward Open（0x5B）请求参数。
//
// 所有字段都要显式给出：这些值会原样进入报文，设备在应答里回显，Forward
// Close 还必须带同一组序列号/厂商号，猜默认值只会让关闭请求对不上。
type ForwardOpen struct {
	// Extended 为 true 时用 Large Forward Open，连接尺寸可用 16 位。
	Extended bool
	// OTConnectionID 是本端建议的 O→T 连接 ID；设备可在应答里改写。
	OTConnectionID uint32
	// TOConnectionID 是本端建议的 T→O 连接 ID；设备可在应答里改写。
	TOConnectionID   uint32
	SerialNumber     uint16
	VendorID         uint16
	OriginatorSerial uint32
	OTRPI            uint32
	TORPI            uint32
	// ConnectionSize 是连接可承载的报文尺寸（字节）。
	ConnectionSize    uint16
	Priority          byte
	TimeoutTicks      byte
	TimeoutMultiplier byte
	// ConnectionPath 是连接路径（通常指向 Message Router），必须 16 位对齐。
	ConnectionPath []byte
}

// BuildForwardOpen 生成 Forward Open 请求。
func BuildForwardOpen(o ForwardOpen) ([]byte, error) {
	cm, err := ConnectionManagerPath()
	if err != nil {
		return nil, err
	}
	if len(o.ConnectionPath) == 0 {
		return nil, errors.New("enip: Forward Open needs a connection path")
	}
	if len(o.ConnectionPath)%2 != 0 {
		return nil, fmt.Errorf("enip: connection path must be 16-bit aligned, got %d bytes", len(o.ConnectionPath))
	}
	words := len(o.ConnectionPath) / 2
	if words > 255 {
		return nil, fmt.Errorf("enip: connection path too long: %d words", words)
	}
	if o.ConnectionSize == 0 {
		return nil, errors.New("enip: Forward Open connection size must be >= 1")
	}
	if !o.Extended && o.ConnectionSize > 511 {
		// 标准格式的网络连接参数只有 9 位给尺寸。硬塞 16 位会把高字节
		// 写进方向/类型位，设备读到的连接参数完全是另一个东西。
		return nil, fmt.Errorf("enip: connection size %d exceeds 511; use a large Forward Open", o.ConnectionSize)
	}

	data := make([]byte, 0, 40+len(o.ConnectionPath))
	data = append(data, o.Priority, o.TimeoutTicks)
	data = appendU32(data, o.OTConnectionID)
	data = appendU32(data, o.TOConnectionID)
	data = appendU16(data, o.SerialNumber)
	data = appendU16(data, o.VendorID)
	data = appendU32(data, o.OriginatorSerial)
	data = append(data, o.TimeoutMultiplier, 0, 0, 0) // 多播超时乘数 + 保留(3)
	data = appendU32(data, o.OTRPI)
	data = appendNetParams(data, o.ConnectionSize, o.Extended)
	data = appendU32(data, o.TORPI)
	data = appendNetParams(data, o.ConnectionSize, o.Extended)
	data = append(data, TransportClass3, byte(words))
	data = append(data, o.ConnectionPath...)

	service := ServiceForwardOpen
	if o.Extended {
		service = ServiceLargeForwardOpen
	}
	return BuildRequest(service, cm, data)
}

// appendNetParams 追加网络连接参数。
//
// 标准格式是 16 位：0x4200（点对点、可变长度、低优先级）+ 9 位尺寸；
// Large 格式是 32 位：同样的 0x4200 放在高 16 位，尺寸用低 16 位。
func appendNetParams(b []byte, size uint16, extended bool) []byte {
	const base uint32 = 0x4200
	if extended {
		return appendU32(b, base<<16|uint32(size))
	}
	return appendU16(b, uint16(base)|size&0x01FF)
}

// ForwardOpenReply 是 Forward Open 应答里本端需要保留的字段。
type ForwardOpenReply struct {
	OTConnectionID   uint32
	TOConnectionID   uint32
	SerialNumber     uint16
	VendorID         uint16
	OriginatorSerial uint32
}

// ParseForwardOpenReply 解析 Forward Open 应答。
//
// SendUnitData 的地址条目要用应答里的 **O→T 连接 ID**（应答第一个字段），
// 不是请求里本端建议的值：设备有权改写它，两家主流实现都取应答值。
func ParseForwardOpenReply(reply Reply) (ForwardOpenReply, error) {
	svc := reply.Service &^ ServiceReplyMask
	if svc != ServiceForwardOpen && svc != ServiceLargeForwardOpen {
		return ForwardOpenReply{}, fmt.Errorf("enip: Forward Open reply has service 0x%02X", reply.Service)
	}
	if reply.Status != 0 {
		return ForwardOpenReply{}, &StatusError{Service: svc, Code: reply.Status, Extra: reply.Extra}
	}
	if len(reply.Data) < 26 {
		return ForwardOpenReply{}, fmt.Errorf("enip: Forward Open reply needs 26 bytes, got %d", len(reply.Data))
	}
	var out ForwardOpenReply
	out.OTConnectionID = binary.LittleEndian.Uint32(reply.Data[0:])
	out.TOConnectionID = binary.LittleEndian.Uint32(reply.Data[4:])
	out.SerialNumber = binary.LittleEndian.Uint16(reply.Data[8:])
	out.VendorID = binary.LittleEndian.Uint16(reply.Data[10:])
	out.OriginatorSerial = binary.LittleEndian.Uint32(reply.Data[12:])
	return out, nil
}

// BuildForwardClose 生成 Forward Close（0x4E）请求。
//
// 序列号/厂商号/发起方序列号必须与 Forward Open 一致，否则设备找不到要关闭
// 的连接，会一直占着连接资源直到超时。
func BuildForwardClose(priority, ticks byte, serialNumber, vendorID uint16, originatorSerial uint32) ([]byte, error) {
	cm, err := ConnectionManagerPath()
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, 10)
	data = append(data, priority, ticks)
	data = appendU16(data, serialNumber)
	data = appendU16(data, vendorID)
	data = appendU32(data, originatorSerial)
	return BuildRequest(ServiceForwardClose, cm, data)
}

// ParseForwardCloseReply 校验 Forward Close 应答。
func ParseForwardCloseReply(reply Reply) error {
	if reply.Status != 0 {
		return &StatusError{Service: ServiceForwardClose, Code: reply.Status, Extra: reply.Extra}
	}
	return nil
}

// ---------------------------------------------------------
// SendUnitData
// ---------------------------------------------------------

// BuildSendUnitData 生成 SendUnitData 的 CPF 负载（封装命令 0x0070）。
//
// CPF 是两个条目，不是三个：
//
//	0x00A1 长度 4：连接 ID
//	0x00B1 长度 2+N：序列号(2) + CIP 报文
//
// 0x8002（Sequenced Address）属于 Class 1 隐式 I/O，显式连接用了它设备会
// 把报文当成 I/O 数据，CIP 服务根本不会被解析。
func BuildSendUnitData(connectionID uint32, sequence uint16, cip []byte) ([]byte, error) {
	addr := make([]byte, 4)
	binary.LittleEndian.PutUint32(addr, connectionID)
	data := make([]byte, 2, 2+len(cip))
	binary.LittleEndian.PutUint16(data, sequence)
	data = append(data, cip...)
	return BuildCPF([]Item{
		{Type: ItemConnectedAddress, Data: addr},
		{Type: ItemConnectedData, Data: data},
	})
}

// ParseSendUnitData 从 SendUnitData 的 CPF 负载里取出连接 ID、序列号与 CIP 报文。
//
// 地址条目按规范必须存在；数据条目必须恰好一个。二者缺一都说明报文结构
// 不是我们理解的样子，继续解析只会把别的字节当 CIP 服务。
func ParseSendUnitData(payload []byte) (connectionID uint32, sequence uint16, cip []byte, err error) {
	items, err := ParseCPF(payload)
	if err != nil {
		return 0, 0, nil, err
	}
	var addr, data []byte
	for _, it := range items {
		switch it.Type {
		case ItemConnectedAddress:
			if addr != nil {
				return 0, 0, nil, errors.New("enip: multiple connected address items")
			}
			addr = it.Data
		case ItemConnectedData:
			if data != nil {
				return 0, 0, nil, errors.New("enip: multiple connected data items")
			}
			data = it.Data
		}
	}
	if addr == nil {
		return 0, 0, nil, errors.New("enip: SendUnitData has no connected address item")
	}
	if len(addr) != 4 {
		return 0, 0, nil, fmt.Errorf("enip: connected address item length %d, want 4", len(addr))
	}
	if data == nil {
		return 0, 0, nil, errors.New("enip: SendUnitData has no connected data item")
	}
	if len(data) < 2 {
		return 0, 0, nil, errors.New("enip: connected data item shorter than sequence count")
	}
	return binary.LittleEndian.Uint32(addr), binary.LittleEndian.Uint16(data), data[2:], nil
}

// ---------------------------------------------------------
// 分片读写
// ---------------------------------------------------------

// BuildReadTagFrag 生成 Read Tag Fragmented 请求（服务 0x52）。
//
// count 是**整个标签要读的元素总数**（不是剩余数）；offset 是已经收到的
// **数据字节数**（不含类型头）。两者单位不同是协议本身的规定。
func BuildReadTagFrag(tag string, count uint16, offset uint32) ([]byte, error) {
	if count == 0 {
		return nil, errors.New("enip: read element count must be >= 1")
	}
	path, err := SymbolicPath(tag)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 6)
	binary.LittleEndian.PutUint16(data[0:], count)
	binary.LittleEndian.PutUint32(data[2:], offset)
	return BuildRequest(ServiceReadFrag, path, data)
}

// BuildWriteTagFrag 生成 Write Tag Fragmented 请求（服务 0x53）。
//
// count 同样是整个标签的总元素数；data 只带本片字节。
func BuildWriteTagFrag(tag string, dataType byte, count uint16, offset uint32, data []byte) ([]byte, error) {
	if count == 0 {
		return nil, errors.New("enip: write element count must be >= 1")
	}
	if len(data) == 0 {
		return nil, errors.New("enip: write fragment needs at least one byte")
	}
	path, err := SymbolicPath(tag)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 8, 8+len(data))
	binary.LittleEndian.PutUint16(payload[0:], uint16(dataType))
	binary.LittleEndian.PutUint16(payload[2:], count)
	binary.LittleEndian.PutUint32(payload[4:], offset)
	payload = append(payload, data...)
	return BuildRequest(ServiceWriteFrag, path, payload)
}

func appendU16(b []byte, v uint16) []byte {
	var x [2]byte
	binary.LittleEndian.PutUint16(x[:], v)
	return append(b, x[:]...)
}

func appendU32(b []byte, v uint32) []byte {
	var x [4]byte
	binary.LittleEndian.PutUint32(x[:], v)
	return append(b, x[:]...)
}
