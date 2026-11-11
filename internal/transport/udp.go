package transport

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Datagram 是无连接报文通道。BACnet/IP 走 UDP：没有连接状态，重传与
// 超时都由协议层负责，因此这里不提供 ReadFull——一次 Read 就是一整条报文。
type Datagram interface {
	Write(p []byte) error
	Read() ([]byte, error)
	SetDeadline(t time.Time) error
	Close() error
}

type udpConn struct {
	conn *net.UDPConn
}

// DialUDP 建立「已连接」的 UDP 套接字。
//
// 用 connected socket 而不是 ListenUDP + 手工过滤：内核只把来自目标地址的
// 报文递上来，广播风暴与其它设备的 Who-Is 不会挤进我们的读循环。BACnet
// 现场同网段动辄几十台设备，这一条能省掉一整类「收到别人的包」的错解析。
func DialUDP(ctx context.Context, addr string, timeout time.Duration) (Datagram, error) {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udp: resolve %q: %w", addr, err)
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "udp", raddr.String())
	if err != nil {
		return nil, fmt.Errorf("udp: dial %q: %w", addr, err)
	}
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("udp: dial %q returned %T", addr, conn)
	}
	return &udpConn{conn: uc}, nil
}

func (c *udpConn) Write(p []byte) error {
	if _, err := c.conn.Write(p); err != nil {
		return fmt.Errorf("udp: write: %w", err)
	}
	return nil
}

// Read 读一整条报文。缓冲区按 UDP 最大载荷分配（65535），BACnet/IP 的
// 常规报文远小于此，但截断一条报文比多分配 64KB 危险得多。
func (c *udpConn) Read() ([]byte, error) {
	buf := make([]byte, 65535)
	n, err := c.conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("udp: read: %w", err)
	}
	return append([]byte(nil), buf[:n]...), nil
}

// LocalAddr 返回本端地址（IP:端口）。
//
// 只有 KNXnet/IP 用得上：它的 CONNECT_REQUEST 里要带 HPAI，明确告诉网关
// 「往哪个 UDP 端点回包」。已连接套接字经过 connect 之后内核已经选好了
// 源地址与端口，这里直接把它取出来即可——比在驱动里发 route-back（全零
// HPAI，让网关按报文的源地址回）更可靠：一部分网关会把全零地址当成
// 「客户端不会接收」而拒绝建立隧道。
func (c *udpConn) LocalAddr() string {
	if a := c.conn.LocalAddr(); a != nil {
		return a.String()
	}
	return ""
}

func (c *udpConn) SetDeadline(t time.Time) error {
	if err := c.conn.SetDeadline(t); err != nil {
		return fmt.Errorf("udp: set deadline: %w", err)
	}
	return nil
}

func (c *udpConn) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("udp: close: %w", err)
	}
	return nil
}
