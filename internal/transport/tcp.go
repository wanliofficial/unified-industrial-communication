// Package transport 提供字节流抽象与 TCP 实现。
//
// 协议分帧（MBAP、TPKT、RTU 长度等）属于协议层，不属于本包；本包只保证
// 「写满」「读满或超时」「关闭幂等」三件事。
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Stream 是一条有序字节流。ReadFull 必须读满 n 字节或返回错误。
type Stream interface {
	Write(p []byte) error
	ReadFull(n int) ([]byte, error)
	Close() error
}

type tcpStream struct {
	mu   sync.Mutex
	conn net.Conn
	// deadline 是协议层按设备超时设置的截止时间；零值表示用内置兜底。
	//
	// 必须自己记住它而不是只调一次 conn.SetDeadline：ReadFull/Write 每次
	// 都会重设 deadline，如果这里不保存，协议层设的值会被兜底值覆盖，
	// 「设备超时 300ms」就变成了「永远 5s」——现场表现为熔断迟迟不触发。
	deadline time.Time
}

// DialTCP 建立 TCP 连接。ctx 的截止时间同时作为连接与首次读写的超时。
func DialTCP(ctx context.Context, addr string, timeout time.Duration) (Stream, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &tcpStream{conn: conn}, nil
}

func (s *tcpStream) Write(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return errors.New("transport: closed")
	}
	_ = s.conn.SetWriteDeadline(s.effectiveDeadline())
	_, err := s.conn.Write(p)
	return err
}

func (s *tcpStream) ReadFull(n int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil, errors.New("transport: closed")
	}
	_ = s.conn.SetReadDeadline(s.effectiveDeadline())
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (s *tcpStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// SetDeadline 允许协议层按设备超时覆盖默认值。
type DeadlineSetter interface {
	SetDeadline(t time.Time) error
}

func (s *tcpStream) SetDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return errors.New("transport: closed")
	}
	s.deadline = t
	return s.conn.SetDeadline(t)
}

// effectiveDeadline 取「协议层设置值」与「5s 兜底」中更早的一个。
// 调用方必须持有 mu。
func (s *tcpStream) effectiveDeadline() time.Time {
	fallback := time.Now().Add(5 * time.Second)
	if s.deadline.IsZero() || s.deadline.After(fallback) {
		return fallback
	}
	// 已过期的 deadline 原样返回：调用方明确要求「现在就超时」，
	// 静默延长会掩盖上层已经取消的事实。
	return s.deadline
}
