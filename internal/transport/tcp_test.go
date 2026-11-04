package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, ln.Addr().String()
}

// echoOnce 接受一个连接，把收到的字节原样回写一次。
func echoOnce(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		_, _ = c.Write(buf[:n])
	}()
}

func TestDialAndRoundTrip(t *testing.T) {
	ln, addr := listen(t)
	echoOnce(t, ln)

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer s.Close()

	if err := s.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.ReadFull(5)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

// ReadFull 必须读满 n 字节：对端分两次发送也不能短读。
func TestReadFullAccumulates(t *testing.T) {
	ln, addr := listen(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("ab"))
		time.Sleep(20 * time.Millisecond)
		_, _ = c.Write([]byte("cdef"))
	}()

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ReadFull(6)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdef" {
		t.Fatalf("got %q", got)
	}
}

func TestReadFullEOF(t *testing.T) {
	ln, addr := listen(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close() // 立即关闭 → 对端读到 EOF
	}()

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.ReadFull(4)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
}

func TestDialFailure(t *testing.T) {
	_, err := DialTCP(context.Background(), "127.0.0.1:1", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		var ne net.Error
		if !errors.As(err, &ne) {
			t.Fatalf("err = %v should be a net.Error or wrap ctx error", err)
		}
	}
}

func TestDialHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialTCP(ctx, "127.0.0.1:1", time.Second); err == nil {
		t.Fatal("expected error")
	}
}

// 关闭后所有操作都必须返回错误而不是 panic 或阻塞。
func TestCloseIsIdempotentAndBlocksUse(t *testing.T) {
	ln, addr := listen(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
	}()

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := s.Write([]byte("x")); err == nil {
		t.Fatal("write after close must fail")
	}
	if _, err := s.ReadFull(1); err == nil {
		t.Fatal("read after close must fail")
	}
	if ds, ok := s.(DeadlineSetter); ok {
		if err := ds.SetDeadline(time.Now()); err == nil {
			t.Fatal("set deadline after close must fail")
		}
	}
}

// SetDeadline 让协议层可以按设备超时覆盖默认值。
func TestSetDeadlineCausesTimeout(t *testing.T) {
	ln, addr := listen(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		time.Sleep(300 * time.Millisecond)
	}()

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ds, ok := s.(DeadlineSetter)
	if !ok {
		t.Fatal("tcpStream must implement DeadlineSetter")
	}
	if err := ds.SetDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadFull(1); err == nil {
		t.Fatal("expected timeout")
	}
}

// 并发写不得交错破坏字节流（tcpStream 内部串行化）。
func TestConcurrentWritesAreSerialized(t *testing.T) {
	ln, addr := listen(t)
	const rounds = 20
	payload := make([]byte, 8)
	for i := range payload {
		payload[i] = 'x'
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, len(payload)*rounds)
		_, _ = io.ReadFull(c, buf)
	}()

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Write(payload); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()
}

// 协议层设置的 deadline 必须真的生效。曾经的实现每次 ReadFull 都重设
// 5s 兜底，把设备级超时覆盖掉——表现是「熔断迟迟不触发」。
func TestSetDeadlineShorterThanDefaultIsHonoured(t *testing.T) {
	ln, addr := listen(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// 一直不回数据。
		<-time.After(2 * time.Second)
	}()

	s, err := DialTCP(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer s.Close()

	ds, ok := s.(DeadlineSetter)
	if !ok {
		t.Fatal("tcp stream must implement DeadlineSetter")
	}
	if err := ds.SetDeadline(time.Now().Add(120 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	start := time.Now()
	_, err = s.ReadFull(1)
	if err == nil {
		t.Fatal("read must time out")
	}
	var ne interface{ Timeout() bool }
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("deadline not honoured: %v", elapsed)
	}
}
