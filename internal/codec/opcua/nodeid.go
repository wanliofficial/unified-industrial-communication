package opcua

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// NodeIDKind 是 NodeId 的标识符类型。数值与字符串覆盖工业现场 99% 的
// 点位；GUID 与 ByteString 保留是为了能原样透传（浏览结果里可能出现）。
type NodeIDKind byte

const (
	NodeIDNull       NodeIDKind = 0
	NodeIDNumeric    NodeIDKind = 2
	NodeIDString     NodeIDKind = 3
	NodeIDGUID       NodeIDKind = 4
	NodeIDByteString NodeIDKind = 5
)

// NodeID 是 OPC UA 的节点标识。
type NodeID struct {
	Kind      NodeIDKind
	Namespace uint16
	ID        uint32 // numeric
	Text      string // string / base64 bytestring
	GUID      [16]byte
}

// NullNodeID 是空节点（全零）。
var NullNodeID = NodeID{}

func (n NodeID) IsNull() bool { return n.Kind == NodeIDNull }

// Numeric 返回数值型 NodeId。
func Numeric(ns uint16, id uint32) NodeID {
	return NodeID{Kind: NodeIDNumeric, Namespace: ns, ID: id}
}

// StringNode 返回字符串型 NodeId。
func StringNode(ns uint16, id string) NodeID {
	return NodeID{Kind: NodeIDString, Namespace: ns, Text: id}
}

// String 输出 `ns=N;s=...` 形式；数值型输出 `ns=N;i=...`。
// 这是配置里写地址的格式，也是日志与错误信息里唯一可读的表示。
func (n NodeID) String() string {
	switch n.Kind {
	case NodeIDNumeric:
		return fmt.Sprintf("ns=%d;i=%d", n.Namespace, n.ID)
	case NodeIDString:
		return fmt.Sprintf("ns=%d;s=%s", n.Namespace, n.Text)
	case NodeIDGUID:
		g := n.GUID
		return fmt.Sprintf("ns=%d;g=%08x-%04x-%04x-%04x-%012x", n.Namespace,
			uint32(g[0])<<24|uint32(g[1])<<16|uint32(g[2])<<8|uint32(g[3]),
			uint16(g[4])<<8|uint16(g[5]), uint16(g[6])<<8|uint16(g[7]),
			uint16(g[8])<<8|uint16(g[9]),
			uint64(g[10])<<40|uint64(g[11])<<32|uint64(g[12])<<24|uint64(g[13])<<16|uint64(g[14])<<8|uint64(g[15]))
	case NodeIDByteString:
		return fmt.Sprintf("ns=%d;b=%s", n.Namespace, n.Text)
	}
	return "ns=0;i=0"
}

// ParseNodeID 解析配置里的 NodeId。
//
// 支持：
//
//	ns=2;s=Line1.Temperature
//	ns=2;i=1001
//	i=2258                 （ns 默认 0）
//	s=Temperature          （ns 默认 0）
//	Temperature            （等价于 s=Temperature）
//	ns=2;b=<base64>
//	ns=2;g=<uuid>
//
// 不接受 `nsu=`：它需要先读取 NamespaceArray 才能换算，是连接后的动作，
// 不能出现在静态配置里。
func ParseNodeID(s string) (NodeID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return NullNodeID, nil
	}
	// `ns=` 开头但没有 `;` 是明显的配置笔误（漏写了标识符部分）。
	// 走「裸字符串」分支会把它当成一个名为 "ns=2" 的字符串 NodeId，
	// 服务端只会回「节点不存在」——错误信息完全指不到配置上。
	if !strings.Contains(s, ";") && (strings.HasPrefix(s, "ns=") || strings.HasPrefix(s, "nsu=")) {
		return NodeID{}, fmt.Errorf("opcua: node id %q is missing the identifier part (e.g. ns=2;s=Name)", s)
	}
	ns := uint16(0)
	idPart := s
	if i := strings.IndexByte(s, ';'); i >= 0 {
		head, tail := s[:i], s[i+1:]
		switch {
		case strings.HasPrefix(head, "ns="):
			n, err := strconv.ParseUint(head[3:], 10, 16)
			if err != nil {
				return NodeID{}, fmt.Errorf("opcua: invalid namespace %q", head)
			}
			ns = uint16(n)
		case strings.HasPrefix(head, "nsu="):
			return NodeID{}, fmt.Errorf("opcua: namespace URIs (nsu=) are not supported; use ns=<index>")
		default:
			return NodeID{}, fmt.Errorf("opcua: invalid node id %q", s)
		}
		idPart = tail
	}

	switch {
	case strings.HasPrefix(idPart, "i="):
		v, err := strconv.ParseUint(idPart[2:], 10, 32)
		if err != nil {
			return NodeID{}, fmt.Errorf("opcua: invalid numeric id %q", idPart)
		}
		if ns == 0 && v == 0 {
			return NullNodeID, nil
		}
		return Numeric(ns, uint32(v)), nil
	case strings.HasPrefix(idPart, "s="):
		return StringNode(ns, idPart[2:]), nil
	case strings.HasPrefix(idPart, "b="):
		if _, err := base64.StdEncoding.DecodeString(idPart[2:]); err != nil {
			return NodeID{}, fmt.Errorf("opcua: invalid base64 bytestring id: %w", err)
		}
		return NodeID{Kind: NodeIDByteString, Namespace: ns, Text: idPart[2:]}, nil
	case strings.HasPrefix(idPart, "g="):
		g, err := parseGUID(idPart[2:])
		if err != nil {
			return NodeID{}, err
		}
		return NodeID{Kind: NodeIDGUID, Namespace: ns, GUID: g}, nil
	default:
		// `x=1` 这种「像标识符但不是受支持那种」的写法必须报错：
		// 把它当裸字符串会得到一个永远查不到的 NodeId，而错误信息
		// 只有「节点不存在」，指不回配置。
		if len(idPart) >= 2 && idPart[1] == '=' {
			return NodeID{}, fmt.Errorf("opcua: unknown node id identifier %q (want i=, s=, g= or b=)", idPart)
		}
		// 裸字符串：现场最常见的写法，等价于 ns=0;s=...
		return StringNode(ns, idPart), nil
	}
}

func parseGUID(s string) ([16]byte, error) {
	var out [16]byte
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return out, fmt.Errorf("opcua: invalid guid %q", s)
	}
	hex := strings.Join(parts, "")
	if len(hex) != 32 {
		return out, fmt.Errorf("opcua: invalid guid %q", s)
	}
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return out, fmt.Errorf("opcua: invalid guid %q", s)
		}
		out[i] = byte(v)
	}
	return out, nil
}

// Encode 写 NodeId。数值型固定用 0x02（ns+uint32），字符串型用 0x03。
// 不使用两字节/四字节紧凑形式：省下的几个字节不值得增加解码分支，
// 而所有服务端都必须接受完整的数值编码。
func (n NodeID) Encode(e *encoder) {
	switch n.Kind {
	case NodeIDNumeric:
		if n.Namespace == 0 && n.ID <= math.MaxUint8 {
			e.Byte(0x00)
			e.Byte(byte(n.ID))
			return
		}
		if n.Namespace <= math.MaxUint8 && n.ID <= math.MaxUint16 {
			e.Byte(0x01)
			e.Byte(byte(n.Namespace))
			e.U16(uint16(n.ID))
			return
		}
		e.Byte(0x02)
		e.U16(n.Namespace)
		e.U32(n.ID)
	case NodeIDString:
		e.Byte(0x03)
		e.U16(n.Namespace)
		e.String(n.Text)
	case NodeIDGUID:
		e.Byte(0x04)
		e.U16(n.Namespace)
		e.b = append(e.b, n.GUID[:]...)
	case NodeIDByteString:
		e.Byte(0x05)
		e.U16(n.Namespace)
		raw, _ := base64.StdEncoding.DecodeString(n.Text)
		e.ByteString(raw)
	default:
		// Null：两字节形式，ns=0、id=0。
		e.Byte(0x00)
		e.Byte(0)
	}
}

// Decode 读 NodeId。扩展标志（NamespaceURI / ServerIndex）由
// DecodeExpanded 处理；普通 NodeId 不允许带这两个标志。
func (d *decoder) NodeID() NodeID {
	enc := d.Byte()
	kind := enc & 0x3F
	switch kind {
	case 0x00:
		return Numeric(0, uint32(d.Byte()))
	case 0x01:
		ns := uint16(d.Byte())
		return Numeric(ns, uint32(d.U16()))
	case 0x02:
		ns := d.U16()
		return Numeric(ns, d.U32())
	case 0x03:
		ns := d.U16()
		return StringNode(ns, d.String())
	case 0x04:
		ns := d.U16()
		var g [16]byte
		if b := d.take(16); b != nil {
			copy(g[:], b)
		}
		return NodeID{Kind: NodeIDGUID, Namespace: ns, GUID: g}
	case 0x05:
		ns := d.U16()
		raw := d.ByteString()
		return NodeID{Kind: NodeIDByteString, Namespace: ns,
			Text: base64.StdEncoding.EncodeToString(raw)}
	default:
		d.fail(fmt.Errorf("opcua: unsupported node id encoding 0x%02x", enc))
		return NullNodeID
	}
}

// ExpandedNodeID 是带 NamespaceURI/ServerIndex 的 NodeId，仅用于浏览结果。
type ExpandedNodeID struct {
	NodeID       NodeID
	NamespaceURI string
	ServerIndex  uint32
}

func (n ExpandedNodeID) Encode(e *encoder) {
	flags := byte(0)
	if n.NamespaceURI != "" {
		flags |= 0x80
	}
	if n.ServerIndex != 0 {
		flags |= 0x40
	}
	// 先写普通 NodeId 的编码字节，再补标志位。
	start := len(e.b)
	n.NodeID.Encode(e)
	e.b[start] |= flags
	if n.NamespaceURI != "" {
		e.String(n.NamespaceURI)
	}
	if n.ServerIndex != 0 {
		e.U32(n.ServerIndex)
	}
}

func (d *decoder) ExpandedNodeID() ExpandedNodeID {
	// 读编码字节，解析出标志后再复用普通 NodeId 的解析逻辑。
	if d.err != nil {
		return ExpandedNodeID{}
	}
	if d.Remaining() < 1 {
		d.fail(ErrShort)
		return ExpandedNodeID{}
	}
	enc := d.b[d.pos]
	hasURI := enc&0x80 != 0
	hasIndex := enc&0x40 != 0
	n := d.NodeID()
	out := ExpandedNodeID{NodeID: n}
	if hasURI {
		out.NamespaceURI = d.String()
	}
	if hasIndex {
		out.ServerIndex = d.U32()
	}
	return out
}
