package transport

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// echoUDP 是一个最小 UDP 回声端，用来验证 DialUDP 的连通性/超时/关闭语义。
type echoUDP struct {
	conn *net.UDPConn
	port string

	wg     sync.WaitGroup
	closed chan struct{}
}

func startEcho(t *testing.T) *echoUDP {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	e := &echoUDP{conn: pc, port: pc.LocalAddr().String(), closed: make(chan struct{})}
	e.wg.Add(1)
	go e.run()
	return e
}

func (e *echoUDP) run() {
	defer e.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := e.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if _, err := e.conn.WriteToUDP(buf[:n], addr); err != nil {
			select {
			case <-e.closed:
			default:
			}
			return
		}
	}
}

func (e *echoUDP) stop() {
	close(e.closed)
	_ = e.conn.Close()
	e.wg.Wait()
}

// send 从回声端口主动发一条报文，用来模拟同网段其它 BACnet 设备的 Who-Is。
func (e *echoUDP) send(payload string) error {
	// 向自身发送：内容不重要，只要它经过网络栈即可。
	addr, err := net.ResolveUDPAddr("udp", e.port)
	if err != nil {
		return err
	}
	_, err = e.conn.WriteToUDP([]byte(payload), addr)
	return err
}

func TestUDPEchoRoundTrip(t *testing.T) {
	echo := startEcho(t)
	defer echo.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := DialUDP(ctx, echo.port, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.Write([]byte("hello bacnet")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	got, err := c.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello bacnet" {
		t.Fatalf("echo = %q", got)
	}
}

// TestUDPConnectedSocketFiltersForeign 验证已连接套接字只收到目标地址的
// 报文：这是用 DialUDP 而不是 ListenUDP 的核心理由。
func TestUDPConnectedSocketFiltersForeign(t *testing.T) {
	echo := startEcho(t)
	defer echo.stop()

	// 另一个会发报文的端点，模拟同网段其它 BACnet 设备的 Who-Is。
	other := startEcho(t)
	defer other.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := DialUDP(ctx, echo.port, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 先让「别人」发一条报文；它不会被我们的已连接套接字收到。
	if err := other.send("foreign"); err != nil {
		t.Fatalf("foreign send: %v", err)
	}
	if err := c.Write([]byte("mine")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	got, err := c.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "mine" {
		t.Fatalf("received %q, want our own echo (foreign packet leaked?)", got)
	}
}

func TestUDPReadTimeout(t *testing.T) {
	echo := startEcho(t)
	defer echo.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := DialUDP(ctx, echo.port, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := c.Read(); err == nil {
		t.Fatal("read with no incoming packet succeeded")
	}
}

func TestUDPCloseUnblocksRead(t *testing.T) {
	echo := startEcho(t)
	defer echo.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := DialUDP(ctx, echo.port, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Read()
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read after close returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock read")
	}
}

func TestUDPBadAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := DialUDP(ctx, "not-an-addr", time.Second); err == nil {
		t.Fatal("invalid address accepted")
	}
	if _, err := DialUDP(ctx, "256.256.256.256:1", time.Second); err == nil {
		t.Fatal("unroutable address accepted")
	}
}

func TestUDPDialContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialUDP(ctx, "127.0.0.1:1", time.Second); err == nil {
		t.Fatal("cancelled context accepted")
	}
}
