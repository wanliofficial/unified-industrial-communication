package secs

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	"uic/internal/driver"
	"uic/internal/model"
)

// 剩余的失败路径：连不上、对端中途挂断、关闭后调用自检扩展、编号回绕、
// 以及「地址不带端口」时补默认端口。

func TestOpenConnectionRefused(t *testing.T) {
	// 先占一个端口再关掉：之后连它必然是 refused。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := dialConfig(addr)
	start := time.Now()
	_, err = New().Open(context.Background(), cfg)
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("连不上应 disconnected，得到 %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("refused 应立即失败，用了 %s", d)
	}
}

// 地址不带端口时补 5000：现场常只写 IP。
func TestOpenAppendsDefaultPort(t *testing.T) {
	cfg := dialConfig("127.0.0.1") // 没有端口，监听在 5000 之外，必然 refused
	_, err := New().Open(context.Background(), cfg)
	if err == nil {
		t.Skip("本机 5000 端口上恰好有服务；跳过")
	}
	if want := "127.0.0.1:5000"; !strings.Contains(err.Error(), want) {
		t.Fatalf("错误里应出现 %s，得到 %v", want, err)
	}
}

// 对端在事务中途关闭连接：分类是 disconnected（可重连），不是 timeout。
func TestReadDisconnectedMidTransaction(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
		_, _ = readFrame(t, c) // 读掉 S1F3 后直接断开
	})
	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("对端挂断应 disconnected，得到 %v", err)
	}
	if len(samples) != 1 || samples[0].Quality != model.QualityCommError {
		t.Fatalf("样本 = %+v", samples)
	}
}

// 帧头本身就残缺（不足 12 字节就断开）：同样是 disconnected，而不是卡住。
func TestReadTruncatedHeader(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
		if _, err := readFrame(t, c); err != nil {
			return
		}
		_, _ = c.Write([]byte{0x81, 0x03, 0x00})
	})
	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("半帧后断开应 disconnected，得到 %v", err)
	}
}

// 关闭后自检扩展也要拒绝，而不是用 nil 连接读下去。
func TestInspectAfterClose(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	in := inspector(t, c)
	if _, err := in.Describe(context.Background(), []uint32{1}); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("关闭后 Describe 应 disconnected，得到 %v", err)
	}
	if _, err := in.RawRead(context.Background(), "svid", 1); driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("关闭后 RawRead 应 disconnected，得到 %v", err)
	}
}

// RawRead 对 ecid 走 S2F13：两条路径的 Stream 不同，不能只测 svid。
func TestRawReadECID(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	it, err := inspector(t, c).RawRead(context.Background(), secscodec.ObjectECID, 10)
	if err != nil {
		t.Fatalf("RawRead ecid: %v", err)
	}
	if n, _ := it.U4(); n != 300 {
		t.Fatalf("ECID 10 = %s", it.Text())
	}
}

// 系统字节是全 32 位回绕的：到 0 要跳回 1，永远不发 0（少数设备把 0 当非法）。
func TestNextSysWrapsAroundZero(t *testing.T) {
	// 0xFFFFFFFF + 1 回绕成 0，此时跳过 0 直接给 1。
	c := &conn{sysSeq: 0xFFFFFFFE}
	if got := c.nextSys(); got != 0xFFFFFFFF {
		t.Fatalf("第一个 = %#x", got)
	}
	if got := c.nextSys(); got != 1 {
		t.Fatalf("跨零后 = %d, want 1（0 被跳过）", got)
	}
}

// 禁用的点不进入计划表：它们既不该被读，也不该被写。
func TestDisabledPointSkipped(t *testing.T) {
	srv := startSim(t)
	p := svidPoint("off", 1, model.TypeUint32)
	p.Enabled = false
	cfg := testConfig(srv, p)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cc := c.(*conn)
	if len(cc.points) != 0 || len(cc.byID) != 0 {
		t.Fatalf("禁用点不应进入计划表: %d plans", len(cc.points))
	}
}

// length 为负是配置错误，Open 阶段就该拦住。
func TestOpenNegativeLength(t *testing.T) {
	srv := startSim(t)
	p := svidPoint("s", 1, model.TypeString)
	p.Address.Length = -1
	if _, err := New().Open(context.Background(), testConfig(srv, p)); driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("负 length 应 ErrAddress，得到 %v", err)
	}
}
