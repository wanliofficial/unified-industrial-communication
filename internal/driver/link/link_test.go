package link

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
)

// 这一层没有协议知识，却决定了两个协议能不能在「串口服务器」这种最常见的
// 接法下工作。用例围绕四件容易回归的事：
//
//  1. TCP 与串口给出同一种能力（Write/ReadFull/SetDeadline/Drain/Lock）；
//  2. `host` 省略端口时补默认端口（现场把 DTU 配成 4001 是常态）；
//  3. Drain 必须真的把残留字节读掉，且不能吃掉下一帧的应答；
//  4. 错误分类：参数问题是 ErrUnsupported（重试无用），连不上是
//     ErrDisconnected（可重试）。

// echoServer 起一个只服务一条连接的 TCP 服务端，把收到的每个字节原样回显。
// 返回端口与退出信号。
func echoServer(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = ln.Close() }()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 64)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if _, werr := c.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	return ln.Addr().String(), done
}

func tcpTransport(addr string) model.Transport {
	return model.Transport{Kind: "tcp", Address: addr}
}

func TestOpenTCPRoundTrip(t *testing.T) {
	addr, _ := echoServer(t)
	l, err := Open(context.Background(), Options{Proto: "test"}, tcpTransport(addr), time.Second, "dev1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = l.Close() }()

	if err := l.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if err := l.Write([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := l.ReadFull(3)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("echo = % x", got)
	}
}

// 只给主机名时必须补 DefaultPort：串口服务器/DTU 的配置页里最常见的写法
// 就是「IP + 端口分两栏」，模型里拼起来往往只剩 IP。
func TestOpenTCPUsesDefaultPort(t *testing.T) {
	addr, _ := echoServer(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(context.Background(), Options{Proto: "test", DefaultPort: port},
		tcpTransport("127.0.0.1"), time.Second, "dev1")
	if err != nil {
		t.Fatalf("open with default port: %v", err)
	}
	defer func() { _ = l.Close() }()
	if err := l.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := l.Write([]byte{0xAA}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := l.ReadFull(1); err != nil || got[0] != 0xAA {
		t.Fatalf("echo = % x, %v", got, err)
	}
}

// 参数错误与「连不上」必须分开：前者重试一万次也不会好，后者会。
func TestOpenErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		tr   model.Transport
		kind driver.ErrorKind
	}{
		{"unsupported kind", model.Transport{Kind: "udp", Address: "127.0.0.1:1"}, driver.ErrUnsupported},
		{"missing address", model.Transport{Kind: "tcp"}, driver.ErrUnsupported},
		{"bad serial params", model.Transport{Kind: "serial", Port: "/dev/null", Baud: 12345}, driver.ErrUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(context.Background(), Options{Proto: "test"}, tc.tr, time.Second, "dev1")
			if err == nil {
				t.Fatal("must fail")
			}
			if got := driver.KindOf(err); got != tc.kind {
				t.Fatalf("kind = %v, want %v (%v)", got, tc.kind, err)
			}
		})
	}

	// 先占一个端口再关掉，保证它确实没人监听。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	_, err = Open(context.Background(), Options{Proto: "test"}, tcpTransport(dead), 300*time.Millisecond, "dev1")
	if err == nil {
		t.Fatal("dial to closed port must fail")
	}
	if got := driver.KindOf(err); got != driver.ErrDisconnected {
		t.Fatalf("dial failure kind = %v, want disconnected (%v)", got, err)
	}
}

// staleServer 在连接建立后先吐一段残留字节，然后等服务端收到请求，回一个
// 固定应答。用来验证 Drain 的语义。
func staleServer(t *testing.T, stale, reply []byte) (string, <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	got := make(chan []byte, 1)
	go func() {
		defer func() { _ = ln.Close() }()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write(stale)
		buf := make([]byte, len(reply))
		if _, err := io.ReadFull(c, buf); err != nil {
			close(got)
			return
		}
		got <- append([]byte(nil), buf...)
		_, _ = c.Write(reply)
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), got
}

// Drain 之后读到的必须是本轮应答，而不是上一轮的残留。
func TestDrainDiscardsStaleBytes(t *testing.T) {
	addr, got := staleServer(t, []byte("STALE"), []byte("PONG"))
	l, err := Open(context.Background(), Options{Proto: "test"}, tcpTransport(addr), time.Second, "dev1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = l.Close() }()

	// 让服务端的残留字节先到达。TCP 回环上这个间隔远小于 20ms。
	time.Sleep(20 * time.Millisecond)
	// 顺序必须是「先 Drain，再 SetDeadline」。反序会让 Drain 把本次事务的
	// 截止时间压到现在，下一次 Write 直接 i/o timeout（见 Drain 的注释）。
	if err := l.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := l.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if err := l.Write([]byte("PING")); err != nil {
		t.Fatalf("write after drain: %v", err)
	}
	select {
	case req := <-got:
		if !bytes.Equal(req, []byte("PING")) {
			t.Fatalf("server saw %q", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive the request")
	}
	resp, err := l.ReadFull(4)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(resp) != "PONG" {
		t.Fatalf("response = %q, want PONG (stale bytes were not drained)", resp)
	}
}

// fakeSerial 是注入给共享串口层的假串口。
type fakeSerial struct {
	mu        sync.Mutex
	in        bytes.Buffer
	written   bytes.Buffer
	drains    int
	deadlines []time.Time
	closed    bool
}

func (f *fakeSerial) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written.Write(p)
	return nil
}

func (f *fakeSerial) ReadFull(n int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.in.Len() < n {
		return nil, io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	_, _ = io.ReadFull(&f.in, buf)
	return buf, nil
}

func (f *fakeSerial) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSerial) SetDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines = append(f.deadlines, t)
	return nil
}

func (f *fakeSerial) Drain() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
	f.in.Reset()
	return nil
}

// 同一路径 + 同一参数复用同一个物理端口；参数或分帧方式不同则显式报错，
// 而不是悄悄再开一个 fd。锁与 Drain 必须透到底层。
func TestOpenSerialSharesPortAndLocks(t *testing.T) {
	path := "/dev/uic-link-test-" + t.Name()
	fake := &fakeSerial{}
	fake.in.WriteString("stale")
	opened := 0
	opener := func(cfg transport.SerialConfig) (transport.Stream, error) {
		opened++
		if cfg.Port != path {
			return nil, errors.New("wrong path " + cfg.Port)
		}
		return fake, nil
	}
	tr := model.Transport{Kind: "serial", Port: path, Baud: 2400, DataBits: 8, Parity: "even", StopBits: 1}
	opt := Options{Proto: "cjt188", Opener: opener}

	l1, err := Open(context.Background(), opt, tr, time.Second, "dev1")
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	l2, err := Open(context.Background(), opt, tr, time.Second, "dev2")
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	if opened != 1 {
		t.Fatalf("port opened %d times, want 1 (shared per physical path)", opened)
	}

	// 总线锁：两个句柄互斥。先拿锁再在另一个 goroutine 里抢，确认它被挡住。
	l1.Lock()
	acquired := make(chan struct{})
	go func() {
		l2.Lock()
		close(acquired)
		l2.Unlock()
	}()
	select {
	case <-acquired:
		t.Fatal("second handle acquired the bus lock while the first held it")
	case <-time.After(30 * time.Millisecond):
	}
	l1.Unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("bus lock was not released")
	}

	// Drain 必须透到底层（串口要 tcflush，TCP 退化成读掉残留）。
	if err := l1.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fake.drains != 1 {
		t.Fatalf("underlying drain calls = %d, want 1", fake.drains)
	}
	if err := l1.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if len(fake.deadlines) != 1 {
		t.Fatalf("underlying deadline calls = %d, want 1", len(fake.deadlines))
	}

	// 参数不同：拒绝，且不能把正在工作的第一个使用者踢掉。
	bad := tr
	bad.Baud = 9600
	if _, err := Open(context.Background(), opt, bad, time.Second, "dev3"); err == nil {
		t.Fatal("different baud on the same path must fail")
	}
	// 分帧方式不同：同一根线不可能同时跑两种分帧。
	if _, err := Open(context.Background(), Options{Proto: "dlt645", Opener: opener}, tr, time.Second, "dev4"); err == nil {
		t.Fatal("different framing on the same path must fail")
	}

	// 引用计数：第一个 Close 不能关掉正在被第二个使用的端口。
	if err := l1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if fake.closed {
		t.Fatal("port closed while a second handle still uses it")
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if !fake.closed {
		t.Fatal("last Close did not close the port")
	}
}
