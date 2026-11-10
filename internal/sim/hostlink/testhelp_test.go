package hostlink

import (
	"io"
	"net"
	"time"
)

// dialRaw 建立一条裸 TCP 连接（不经过驱动），用于构造畸形帧。
func dialRaw(addr string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	return c, nil
}

// readOne 从裸连接上读一条以 `*`+CR 结尾的帧。
func readOne(c net.Conn) ([]byte, error) {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(c, one); err != nil {
			return nil, err
		}
		buf = append(buf, one[0])
		if one[0] == '\r' && len(buf) >= 2 && buf[len(buf)-2] == '*' {
			return buf, nil
		}
		if len(buf) > 2000 {
			return nil, io.ErrUnexpectedEOF
		}
	}
}
