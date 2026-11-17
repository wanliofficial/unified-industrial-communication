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
	"encoding/binary"
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

	// Will 是本次 CONNECT 登记的遗嘱，nil 表示不带遗嘱。
	//
	// 用指针而不是四个平铺字段：遗嘱在 MQTT 里是「CONNECT 报文的一部分」，
	// 要么整条存在要么整条不存在。拆成 WillTopic/WillPayload/WillQoS/
	// WillRetain 之后，「有 topic 没 payload」这种半成品状态必须靠运行时
	// 校验挡，而它恰恰是最容易漏的一种（改了 topic 忘了改 payload）。
	Will *Will
}

// Message 是一条待发布消息。
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// Will 是 MQTT 遗嘱（CONNECT 里登记的 Will Message）。
//
// 单独成型是因为它必须在**每次** CONNECT 之前重新取值：Sparkplug 要求
// Will 载荷里的 bdSeq 比上一个 CONNECT 的 +1（255 后回绕 0），所以遗嘱
// 不是进程级常量。
type Will struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// ConnectInfo 是一条已解析的 CONNECT 报文。
//
// 生产代码不发 CONNECT（客户端只构造），这个类型的存在意义是让 Broker
// 侧（演示从站与测试）能断言客户端**实际**发了什么：CleanSession 是否为
// true、遗嘱的 QoS/retain/bdSeq 是否符合协议，都是只看本地状态无法确认
// 的契约。
type ConnectInfo struct {
	ClientID     string
	CleanSession bool
	KeepAlive    uint16
	Username     string
	HasPassword  bool
	Will         Will
	HasWill      bool
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
	if o.Will != nil {
		if err := o.Will.validate(); err != nil {
			return nil, err
		}
	}

	var flags byte
	if o.CleanSession {
		flags |= 0x02
	}
	if o.Will != nil {
		flags |= 0x04
		flags |= o.Will.QoS << 3
		if o.Will.Retain {
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
	if o.Will != nil {
		if body, err = appendString(body, o.Will.Topic); err != nil {
			return nil, err
		}
		body = append(body, byte(len(o.Will.Payload)>>8), byte(len(o.Will.Payload)))
		body = append(body, o.Will.Payload...)
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

// validate 检查遗嘱能否出现在 CONNECT 里。
//
// 空 topic 是硬错误：MQTT 的 Will Topic 是 UTF-8 字符串，空串会让 Broker
// 在掉线时代发一条「发布到空主题」的消息，多数 Broker 会直接断开新连接。
// 这种配错在本地完全看不出来，所以要在这里挡住。
func (w Will) validate() error {
	if w.Topic == "" {
		return errors.New("mqtt: will topic required")
	}
	if w.QoS > 2 {
		return fmt.Errorf("mqtt: will qos %d out of range", w.QoS)
	}
	if len(w.Payload) > 65535 {
		return fmt.Errorf("mqtt: will payload too long (%d bytes)", len(w.Payload))
	}
	return nil
}

// ParseConnect 解析 CONNECT 报文体（不含固定头）。
//
// 只解析 Broker 需要断言的字段：CleanSession、keepalive、遗嘱与凭据。
// 未知/多余字段直接忽略——CONNECT 没有可扩展区域，但宽容的解析器不会
// 比严格的解析器更危险：调用方看到的每个字段都是独立校验过的。
func ParseConnect(body []byte) (ConnectInfo, error) {
	var ci ConnectInfo
	rest := body
	str := func() (string, error) {
		if len(rest) < 2 {
			return "", errors.New("mqtt: connect payload truncated (length prefix)")
		}
		n := int(binary.BigEndian.Uint16(rest[:2]))
		rest = rest[2:]
		if len(rest) < n {
			return "", fmt.Errorf("mqtt: connect payload truncated (want %d bytes, have %d)", n, len(rest))
		}
		s := string(rest[:n])
		rest = rest[n:]
		return s, nil
	}

	proto, err := str()
	if err != nil {
		return ci, err
	}
	// 名字与版本一起校验：v3.1（"MQIsdp"/3）与 v5 的 CONNECT 结构不同，
	// 按 3.1.1 解析 v5 报文会把密码段读成垃圾。
	if proto != "MQTT" {
		return ci, fmt.Errorf("mqtt: unexpected protocol name %q", proto)
	}
	if len(rest) < 4 {
		return ci, errors.New("mqtt: connect payload truncated (level/flags/keepalive)")
	}
	level := rest[0]
	flags := rest[1]
	ci.KeepAlive = binary.BigEndian.Uint16(rest[2:4])
	rest = rest[4:]
	if level != ProtocolLevel {
		return ci, fmt.Errorf("mqtt: protocol level %d, only 4 (3.1.1) is supported", level)
	}
	if flags&0x01 != 0 {
		return ci, errors.New("mqtt: connect reserved flag set")
	}
	ci.CleanSession = flags&0x02 != 0

	if ci.ClientID, err = str(); err != nil {
		return ci, err
	}
	if flags&0x04 != 0 {
		willQoS := (flags >> 3) & 0x03
		if willQoS > 2 {
			return ci, fmt.Errorf("mqtt: will qos %d out of range", willQoS)
		}
		topic, err := str()
		if err != nil {
			return ci, err
		}
		payload, err := str()
		if err != nil {
			return ci, err
		}
		ci.Will = Will{Topic: topic, Payload: []byte(payload), QoS: willQoS, Retain: flags&0x20 != 0}
		ci.HasWill = true
	}
	if flags&0x80 != 0 {
		if ci.Username, err = str(); err != nil {
			return ci, err
		}
	}
	if flags&0x40 != 0 {
		// 密码是二进制而不是字符串，不能按 UTF-8 读——只是这里两者都按
		// 长度前缀取字节，用同一个辅助函数是安全的。
		if _, err := str(); err != nil {
			return ci, err
		}
		ci.HasPassword = true
	}
	return ci, nil
}

// PublishPacket 生成 PUBLISH 报文。pid 仅在 QoS>0 时使用且必须非零。
func PublishPacket(m Message, pid uint16) ([]byte, error) {
	if m.Topic == "" {
		return nil, errors.New("mqtt: topic required")
	}
	if m.QoS > 1 {
		// QoS2 需要 PUBREC/PUBREL/PUBCOMP 三条报文与两套中间状态，
		// 本实现不做。在这里拒绝，比发出去之后被 Broker 以协议错误断链
		// 更容易定位：调用方拿到的是一个明确的配置错误，而不是偶发断线。
		return nil, fmt.Errorf("mqtt: qos %d unsupported (only 0 and 1)", m.QoS)
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

// TopicFilter 是一条订阅项：主题过滤器 + 期望 QoS。
type TopicFilter struct {
	Filter string // 可含 + / # 通配符
	QoS    byte
}

// SubscribePacket 生成 SUBSCRIBE 报文。
//
// 固定头低 4 位必须是 0b0010（§3.8.1）；写错不会被本地发现，只会被
// Broker 以「协议错误」断链——这类错误在抓包里极难看出来，所以在这里
// 直接硬编码并用测试锁住。
func SubscribePacket(filters []TopicFilter, pid uint16) ([]byte, error) {
	if len(filters) == 0 {
		return nil, errors.New("mqtt: subscribe needs at least one topic filter")
	}
	if pid == 0 {
		return nil, errors.New("mqtt: subscribe packet id must be non-zero")
	}
	body := []byte{byte(pid >> 8), byte(pid)}
	for _, f := range filters {
		if f.Filter == "" {
			return nil, errors.New("mqtt: empty topic filter")
		}
		if f.QoS > 2 {
			return nil, fmt.Errorf("mqtt: qos %d out of range", f.QoS)
		}
		var err error
		if body, err = appendString(body, f.Filter); err != nil {
			return nil, err
		}
		body = append(body, f.QoS)
	}
	out := []byte{TypeSubscribe<<4 | 0x02}
	out = appendLength(out, len(body))
	return append(out, body...), nil
}

// SubackFailure 是 SUBACK 里表示「该过滤器被拒绝」的返回码。
const SubackFailure byte = 0x80

// ParseSuback 解析 SUBACK 报文体，返回 packet id 与逐条授予的 QoS。
//
// 授予值可以是 0/1/2，也可以是 0x80（拒绝）。**不能只看报文是否收到**：
// 通配符被 ACL 拒绝时 Broker 回的是 SUBACK+0x80，不是断链。把它当成
// 订阅成功，会得到一个永远收不到数据却显示「在线」的设备。
func ParseSuback(body []byte) (uint16, []byte, error) {
	if len(body) < 3 {
		return 0, nil, fmt.Errorf("mqtt: suback body too short (%d)", len(body))
	}
	pid := uint16(body[0])<<8 | uint16(body[1])
	return pid, body[2:], nil
}

// Publish 是一条收到的 PUBLISH。
type Publish struct {
	Topic     string
	Payload   []byte
	QoS       byte
	Retain    bool
	Duplicate bool
	PID       uint16 // 仅 QoS>0
}

// ParsePublish 解析一条 PUBLISH 报文。
func ParsePublish(pkt Packet) (Publish, error) {
	if pkt.Type != TypePublish {
		return Publish{}, fmt.Errorf("mqtt: not a publish (type %d)", pkt.Type)
	}
	body := pkt.Body
	if len(body) < 2 {
		return Publish{}, errors.New("mqtt: short publish")
	}
	tl := int(binary.BigEndian.Uint16(body[:2]))
	if len(body) < 2+tl {
		return Publish{}, errors.New("mqtt: short topic")
	}
	p := Publish{
		Topic:     string(body[2 : 2+tl]),
		QoS:       (pkt.Flags >> 1) & 0x03,
		Retain:    pkt.Flags&0x01 != 0,
		Duplicate: pkt.Flags&0x08 != 0,
	}
	rest := body[2+tl:]
	if p.QoS > 0 {
		if len(rest) < 2 {
			return Publish{}, errors.New("mqtt: short packet id")
		}
		p.PID = binary.BigEndian.Uint16(rest[:2])
		rest = rest[2:]
	}
	p.Payload = append([]byte(nil), rest...)
	return p, nil
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
