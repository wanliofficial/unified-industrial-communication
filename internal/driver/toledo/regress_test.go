package toledo

import (
	"context"
	"net"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

// 验证推送型驱动 Read 在「一帧都没收到」时不得把帧级问题（ErrProtocol /
// ErrTimeout）作为顶层错误上抛：collector 对这两类会 Close 重连 + consecutive
// 熔断，而衡器此刻链路是好的（在推字节、或只是暂时安静）。应逐点降级为
// comm_error 并返回 nil，排查信息走 Stats()。
func TestReadNoFrameReturnsNotTopLevelError(t *testing.T) {
	// 只吐二进制帧（STX 开头）：衡器在推、但一帧都解不出 → ErrProtocol 语义。
	binary := startBinaryScale(t)
	c := openConnAt(t, binary, pt("w", "weight", model.TypeFloat64))
	waitBadFrames(t, c)
	out, err := c.Read(context.Background(), spans(pt("w", "weight", model.TypeFloat64)))
	if err != nil {
		t.Fatalf("Read 不应把帧级问题当顶层错误上抛（会触发 collector 重连+熔断）：%v", err)
	}
	s, ok := sampleByID(out, "w")
	if !ok {
		t.Fatal("缺少 weight 样本")
	}
	if s.Quality != model.QualityCommError {
		t.Errorf("从未成帧时质量码应为 comm_error，得到 %s", s.Quality)
	}
}

// 静默衡器：连着但一个字节都没有 → ErrTimeout 语义（衡器暂时安静，不是断链）。
func TestReadSilentScaleReturnsNotTopLevelError(t *testing.T) {
	srv := startSilentScale(t)
	c := openConnAt(t, srv, pt("w", "weight", model.TypeFloat64))
	out, err := c.Read(context.Background(), spans(pt("w", "weight", model.TypeFloat64)))
	if err != nil {
		t.Fatalf("静默衡器不应把 ErrTimeout 当顶层错误上抛（推送型链路安静≠断链）：%v", err)
	}
	if s, _ := sampleByID(out, "w"); s.Quality != model.QualityCommError {
		t.Errorf("静默衡器质量码应为 comm_error，得到 %s", s.Quality)
	}
}

// startSilentScale 起一台连上但不推任何字节的衡器。
func startSilentScale(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// waitBadFrames 轮询直到收帧统计的坏帧计数 > 0（pump 已读到并拒绝二进制帧）。
func waitBadFrames(t *testing.T, c driver.Conn) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.(*conn).Stats(); st.BadFrames > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("2s 内未观察到坏帧（假秤应持续吐二进制帧）")
}
