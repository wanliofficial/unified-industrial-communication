// Package mqtt 实现一个零依赖的 MQTT 3.1.1 客户端（仅发布侧）。
//
// 为什么自己写而不是引第三方库：本项目的第一条约束是「零第三方依赖、
// 单二进制交叉编译」。MQTT 3.1.1 的发布侧只需要 CONNECT / PUBLISH(QoS0,1)
// / PUBACK / PINGREQ / PINGRESP / DISCONNECT 六种报文，规范正文不到 80 页，
// 实现规模小于引入一个依赖带来的供应链与版本管理成本。
//
// 本包只做客户端发布，不做订阅、不做 QoS2、不做会话持久化——这些不在本项目
// 的北向需求里（北向是「网关往外发」，订阅语义属于南向）。缺失的部分在
// 能力声明里显式标注，而不是假装支持。
package mqtt

import (
	"errors"
	"fmt"
	"io"
)

// 报文类型（MQTT 3.1.1 §2.2.1）。
const (
	TypeConnect    byte = 1
	TypeConnack    byte = 2
	TypePublish    byte = 3
	TypePuback     byte = 4
	TypeSubscribe  byte = 8
	TypeSuback     byte = 9
	TypePingreq    byte = 12
	TypePingresp   byte = 13
	TypeDisconnect byte = 14
)

// CONNACK 返回码（§3.2.2.3）。
const (
	ConnAccepted           byte = 0
	ConnRefusedProtocol    byte = 1
	ConnRefusedIdentifier  byte = 2
	ConnRefusedUnavailable byte = 3
	ConnRefusedBadAuth     byte = 4
	ConnRefusedNotAuth     byte = 5
)

// MaxRemainingLength 是 MQTT 剩余长度的协议上限（268435455 字节）。
const MaxRemainingLength = 268435455

// ProtocolLevel 是本实现支持的协议版本（3.1.1）。
const ProtocolLevel = 4

// ErrPacketTooLarge 表示报文超过本地上限。必须拒绝而不是截断：
// 半个报文会让整条连接失去帧同步，后续所有报文都会解析错位。
var ErrPacketTooLarge = errors.New("mqtt: packet exceeds local limit")

// ConnectOptions 是 CONNECT 报文的可配项。
type ConnectOptions struct {
	ClientID     string
	Username     string
	Password     string
	KeepAlive    uint16 // 秒；0 表示关闭 keepalive
	CleanSession bool

	// Will（遗嘱）用于让 Broker 在客户端异常掉线时代发一条消息。
	// 北向用它维护 $death，让消费方不必依赖超时猜测网关是否在线。
	WillTopic   string
	WillPayload []byte
	WillQoS     byte
	WillRetain  bool
}

// Message 是一条待发布消息。
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// ---------------------------------------------------------------------------
// 编码
// ---------------------------------------------------------------------------

// appendLength 追加 MQTT 变长整数（每字节 7 位，最高位为续位）。
func appendLength(dst []byte, n int) []byte {
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if n == 0 {
			return dst
		}
	}
}

// appendString 追加 UTF-8 字符串（2 字节大端长度前缀）。
func appendString(dst []byte, s string) ([]byte, error) {
	if len(s) > 65535 {
		return nil, fmt.Errorf("mqtt: string too long (%d bytes)", len(s))
	}
	dst = append(dst, byte(len(s)>>8), byte(len(s)))
	return append(dst, s...), nil
}

// ConnectPacket 生成 CONNECT 报文。
func ConnectPacket(o ConnectOptions) ([]byte, error) {
	if o.ClientID == "" {
		return nil, errors.New("mqtt: clientId required (empty client id needs a broker-assigned id, unsupported)")
	}
	if o.Password != "" && o.Username == "" {
		// 3.1.1 规定 password flag 只能在 username flag 置位时使用。
		return nil, errors.New("mqtt: password requires username")
	}
	if o.WillTopic != "" {
		if o.WillQoS > 2 {
			return nil, fmt.Errorf("mqtt: will qos %d out of range", o.WillQoS)
		}
	} else if len(o.WillPayload) > 0 || o.WillQoS != 0 || o.WillRetain {
		return nil, errors.New("mqtt: will payload/qos/retain without will topic")
	}

	var flags byte
	if o.CleanSession {
		flags |= 0x02
	}
	if o.WillTopic != "" {
		flags |= 0x04
		flags |= o.WillQoS << 3
		if o.WillRetain {
			flags |= 0x20
		}
	}
	if o.Username != "" {
		flags |= 0x80
	}
	if o.Password != "" {
		flags |= 0x40
	}

	var body []byte
	body, _ = appendString(body, "MQTT")
	body = append(body, ProtocolLevel, flags, byte(o.KeepAlive>>8), byte(o.KeepAlive))

	var err error
	if body, err = appendString(body, o.ClientID); err != nil {
		return nil, err
	}
	if o.WillTopic != "" {
		if body, err = appendString(body, o.WillTopic); err != nil {
			return nil, err
		}
		if len(o.WillPayload) > 65535 {
			return nil, fmt.Errorf("mqtt: will payload too long (%d bytes)", len(o.WillPayload))
		}
		body = append(body, byte(len(o.WillPayload)>>8), byte(len(o.WillPayload)))
		body = append(body, o.WillPayload...)
	}
	if o.Username != "" {
		if body, err = appendString(body, o.Username); err != nil {
			return nil, err
		}
	}
	if o.Password != "" {
		if body, err = appendString(body, o.Password); err != nil {
			return nil, err
		}
	}

	out := []byte{TypeConnect << 4}
	out = appendLength(out, len(body))
	return append(out, body...), nil
}

// PublishPacket 生成 PUBLISH 报文。pid 仅在 QoS>0 时使用且必须非零。
func PublishPacket(m Message, pid uint16) ([]byte, error) {
	if m.Topic == "" {
		return nil, errors.New("mqtt: topic required")
	}
	if m.QoS > 2 {
		return nil, fmt.Errorf("mqtt: qos %d out of range", m.QoS)
	}
	if m.QoS > 0 && pid == 0 {
		return nil, errors.New("mqtt: packet id must be non-zero for qos>0")
	}

	first := TypePublish << 4
	first |= m.QoS << 1
	if m.Retain {
		first |= 0x01
	}

	var body []byte
	var err error
	if body, err = appendString(body, m.Topic); err != nil {
		return nil, err
	}
	if m.QoS > 0 {
		body = append(body, byte(pid>>8), byte(pid))
	}
	body = append(body, m.Payload...)

	out := []byte{first}
	out = appendLength(out, len(body))
	return append(out, body...), nil
}

// PubackPacket 生成 PUBACK 报文。
func PubackPacket(pid uint16) []byte {
	return []byte{TypePuback << 4, 2, byte(pid >> 8), byte(pid)}
}

// PingreqPacket 生成 PINGREQ 报文。
func PingreqPacket() []byte { return []byte{TypePingreq << 4, 0} }

// DisconnectPacket 生成 DISCONNECT 报文。
func DisconnectPacket() []byte { return []byte{TypeDisconnect << 4, 0} }

// ---------------------------------------------------------------------------
// 解码
// ---------------------------------------------------------------------------

// Packet 是一条已解出的报文（固定头拆分完毕）。
type Packet struct {
	Type  byte
	Flags byte
	Body  []byte
}

// ReadPacket 从流中读取一条报文。
//
// limit 是本地报文大小上限：Broker 若声称一条超大报文，直接拒绝比
// 按声明长度分配内存安全（否则一个坏掉的对端就能 OOM 掉网关）。
func ReadPacket(r io.Reader, limit int) (Packet, error) {
	var head [1]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Packet{}, err
	}
	length, err := readLength(r)
	if err != nil {
		return Packet{}, err
	}
	if limit > 0 && length > limit {
		return Packet{}, fmt.Errorf("%w: %d > %d", ErrPacketTooLarge, length, limit)
	}
	body := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, body); err != nil {
			return Packet{}, err
		}
	}
	return Packet{Type: head[0] >> 4, Flags: head[0] & 0x0F, Body: body}, nil
}

// readLength 读取变长整数，最多 4 字节。
func readLength(r io.Reader) (int, error) {
	mult, value := 1, 0
	for i := 0; i < 4; i++ {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		value += int(b[0]&0x7F) * mult
		if b[0]&0x80 == 0 {
			return value, nil
		}
		mult *= 128
	}
	return 0, errors.New("mqtt: malformed remaining length (>4 bytes)")
}

// ConnackCode 描述 CONNACK 返回码，用于给出可操作的错误信息。
func ConnackCode(code byte) string {
	switch code {
	case ConnAccepted:
		return "accepted"
	case ConnRefusedProtocol:
		return "unacceptable protocol version (broker may only speak MQTT 3.1 / 5.0)"
	case ConnRefusedIdentifier:
		return "client id rejected (empty or too long, or duplicate session)"
	case ConnRefusedUnavailable:
		return "broker unavailable"
	case ConnRefusedBadAuth:
		return "bad username or password"
	case ConnRefusedNotAuth:
		return "not authorized (check broker ACL for this client id / topic)"
	}
	return fmt.Sprintf("unknown return code %d", code)
}

// ParseConnack 解析 CONNACK 报文体。
func ParseConnack(body []byte) (sessionPresent bool, code byte, err error) {
	if len(body) != 2 {
		return false, 0, fmt.Errorf("mqtt: connack body length %d, want 2", len(body))
	}
	if body[0]&0xFE != 0 {
		return false, 0, fmt.Errorf("mqtt: connack reserved bits set (%#x)", body[0])
	}
	return body[0]&0x01 != 0, body[1], nil
}

// ParsePuback 从 PUBACK 报文体中取出 packet id。
func ParsePuback(body []byte) (uint16, error) {
	if len(body) != 2 {
		return 0, fmt.Errorf("mqtt: puback body length %d, want 2", len(body))
	}
	return uint16(body[0])<<8 | uint16(body[1]), nil
}
