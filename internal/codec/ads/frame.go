package ads

import (
	"encoding/binary"
	"fmt"
)

// Encode 把 Frame 编码成完整的 AMS/TCP 报文（含 6 字节 TCP 头）。
//
// 长度字段有两层且**必须一致**：TCP 头的 length 是 AMS 头 + 载荷，
// AMS 头的 length 只是载荷。少写一层会让对端把载荷开头当成下一个
// 报文的长度。
func (f Frame) Encode() ([]byte, error) {
	if f.Command == CmdInvalid {
		return nil, fmt.Errorf("ads: cannot encode command 0")
	}
	if len(f.Data) > MaxDatagram {
		return nil, fmt.Errorf("ads: payload %d bytes exceeds %d", len(f.Data), MaxDatagram)
	}
	amsLen := HeaderLen + len(f.Data)
	out := make([]byte, TCPHeaderLen+amsLen)

	// TCP 头：reserved=0，length=AMS 头+载荷。
	binary.LittleEndian.PutUint16(out[0:2], 0)
	binary.LittleEndian.PutUint32(out[2:6], uint32(amsLen))

	b := out[TCPHeaderLen:]
	copy(b[0:6], f.Target.NetID[:])
	binary.LittleEndian.PutUint16(b[6:8], f.Target.Port)
	copy(b[8:14], f.Source.NetID[:])
	binary.LittleEndian.PutUint16(b[14:16], f.Source.Port)
	binary.LittleEndian.PutUint16(b[16:18], uint16(f.Command))
	binary.LittleEndian.PutUint16(b[18:20], uint16(f.Flags))
	binary.LittleEndian.PutUint32(b[20:24], uint32(len(f.Data)))
	binary.LittleEndian.PutUint32(b[24:28], uint32(f.Error))
	binary.LittleEndian.PutUint32(b[28:32], f.Invoke)
	copy(b[HeaderLen:], f.Data)
	return out, nil
}

// Decode 解析一条完整的 AMS/TCP 报文。
//
// 严格校验三层长度与 TCP reserved 字段：这三个地方是最常见的“帧边界
// 算错”信号，宽容处理只会把错误推迟到载荷解析，变成更难懂的报错。
func Decode(b []byte) (Frame, error) {
	var f Frame
	if len(b) < TCPHeaderLen+HeaderLen {
		return f, fmt.Errorf("ads: datagram %d bytes shorter than minimal frame %d",
			len(b), TCPHeaderLen+HeaderLen)
	}
	if rsv := binary.LittleEndian.Uint16(b[0:2]); rsv != 0 {
		return f, fmt.Errorf("ads: tcp header reserved field is 0x%04X, want 0", rsv)
	}
	amsLen := int(binary.LittleEndian.Uint32(b[2:6]))
	if amsLen < HeaderLen {
		return f, fmt.Errorf("ads: ams length %d shorter than header %d", amsLen, HeaderLen)
	}
	if amsLen > MaxDatagram {
		return f, fmt.Errorf("ads: ams length %d exceeds %d", amsLen, MaxDatagram)
	}
	if got := len(b) - TCPHeaderLen; got != amsLen {
		return f, fmt.Errorf("ads: tcp length says %d ams bytes, datagram has %d", amsLen, got)
	}

	h := b[TCPHeaderLen:]
	copy(f.Target.NetID[:], h[0:6])
	f.Target.Port = binary.LittleEndian.Uint16(h[6:8])
	copy(f.Source.NetID[:], h[8:14])
	f.Source.Port = binary.LittleEndian.Uint16(h[14:16])
	f.Command = Command(binary.LittleEndian.Uint16(h[16:18]))
	f.Flags = Flags(binary.LittleEndian.Uint16(h[18:20]))
	payloadLen := int(binary.LittleEndian.Uint32(h[20:24]))
	f.Error = Code(binary.LittleEndian.Uint32(h[24:28]))
	f.Invoke = binary.LittleEndian.Uint32(h[28:32])

	if payloadLen < 0 || payloadLen != amsLen-HeaderLen {
		return Frame{}, fmt.Errorf("ads: ams payload length %d, header+payload says %d",
			payloadLen, amsLen-HeaderLen)
	}
	// 拷贝一份：调用方通常复用读缓冲，Frame 的生命周期比它长。
	f.Data = append([]byte(nil), h[HeaderLen:HeaderLen+payloadLen]...)
	return f, nil
}

// ReadFull 是 Decode 的流式版本所需的长度计算：先读 6 字节头，再读 rest。
// 返回本帧总长度（含 6 字节头）。
func FrameLen(tcpHeader []byte) (int, error) {
	if len(tcpHeader) < TCPHeaderLen {
		return 0, fmt.Errorf("ads: need %d bytes of tcp header, got %d", TCPHeaderLen, len(tcpHeader))
	}
	if rsv := binary.LittleEndian.Uint16(tcpHeader[0:2]); rsv != 0 {
		return 0, fmt.Errorf("ads: tcp header reserved field is 0x%04X, want 0", rsv)
	}
	n := int(binary.LittleEndian.Uint32(tcpHeader[2:6]))
	if n < HeaderLen || n > MaxDatagram {
		return 0, fmt.Errorf("ads: ams length %d out of range %d..%d", n, HeaderLen, MaxDatagram)
	}
	return TCPHeaderLen + n, nil
}
