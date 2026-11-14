package toledo

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	toledocodec "uic/internal/codec/toledo"
)

// 这些用例验证「衡器写进去的、驱动解出来的」一致：衡器用 Format 组帧，测试用
// 独立分帧读取后交给 codec.Parse，断言往返不漂移。用真实 TCP 回环而非直接调
// Format，是为了覆盖「TCP 字节流 → 分帧 → 解析」这条完整链路。

func startSim(t *testing.T, opts ...Option) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// dial 连上衡器并返回一个按行读取的 reader。
func dial(t *testing.T, addr string) *bufio.Reader {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return bufio.NewReader(conn)
}

// readLine 读一行（容忍 CR/LF）。
func readLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// TestServerPushesStableWeight 衡器应持续推稳定重量帧，能被解析回设定值。
func TestServerPushesStableWeight(t *testing.T) {
	srv := startSim(t, WithWeight(149.0, "kg", 1))
	br := dial(t, srv.Addr())

	line := readLine(t, br)
	r, err := toledocodec.Parse([]byte(line))
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	if r.Status != toledocodec.StatusStable {
		t.Errorf("status = %q, want ST", r.Status)
	}
	if r.Weight != 149.0 {
		t.Errorf("weight = %v, want 149.0", r.Weight)
	}
	if r.Unit != "kg" {
		t.Errorf("unit = %q, want kg", r.Unit)
	}
}

// TestServerReflectsStateChange 改读数后，衡器后续帧应反映新值。
func TestServerReflectsStateChange(t *testing.T) {
	srv := startSim(t, WithWeight(10.0, "kg", 1))
	br := dial(t, srv.Addr())

	// 先读到初始值。
	r, _ := toledocodec.Parse([]byte(readLine(t, br)))
	if r.Weight != 10.0 {
		t.Fatalf("initial weight = %v, want 10.0", r.Weight)
	}

	// 改成运动中、负净重，单位 lb。
	srv.SetStatus(toledocodec.StatusMotion)
	srv.SetMode(toledocodec.ModeNet)
	srv.SetWeight(-3.5)

	// 推帧是持续的，几帧内必读到新值。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, err := toledocodec.Parse([]byte(readLine(t, br)))
		if err != nil {
			continue
		}
		if r.Weight == -3.5 && r.Status == toledocodec.StatusMotion {
			return
		}
	}
	t.Fatal("never observed updated reading (weight=-3.5, status=US)")
}

// TestServerSilentPushesNothing 静默衡器连着但不推帧。
func TestServerSilentPushesNothing(t *testing.T) {
	srv := startSim(t, WithSilent())
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// 静默衡器不应主动吐任何字节。设一个短读超时确认「没数据」。
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 || err == nil {
		t.Fatalf("silent server should push nothing, got n=%d err=%v", n, err)
	}
}
