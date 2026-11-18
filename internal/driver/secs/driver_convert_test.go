package secs

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	"uic/internal/driver"
	"uic/internal/model"
)

// 这一组测试不连仿真器：它钉的是「值怎么解释」和「握手与事务怎么失败」。
//
// 值转换是驱动里唯一「同一条线数据在点表里可以是任何类型」的地方，一个分支
// 走错，现象就变成「温度点常年 bad」；而握手/事务的失败路径决定调度器看到
// 的是 timeout、disconnected 还是 protocol——三者触发完全不同的退避与熔断
// 策略，混错会让一台好设备被拉黑，或者一台坏设备被无限重试。

// ---------------------------------------------------------------------------
// 值转换
// ---------------------------------------------------------------------------

func TestItemValueNumeric(t *testing.T) {
	cases := []struct {
		name string
		item *secscodec.Item
		dt   model.DataType
		want float64
	}{
		{"U1→uint16", secscodec.Int(secscodec.FormatU1, 200), model.TypeUint16, 200},
		{"U2→uint32", secscodec.Int(secscodec.FormatU2, 5000), model.TypeUint32, 5000},
		{"U4→uint32", secscodec.Int(secscodec.FormatU4, 25), model.TypeUint32, 25},
		{"U8→uint64", secscodec.Int(secscodec.FormatU8, 1<<40), model.TypeUint64, 1 << 40},
		{"I1→int16", secscodec.Int(secscodec.FormatI1, -5), model.TypeInt16, -5},
		{"I2→int32", secscodec.Int(secscodec.FormatI2, -300), model.TypeInt32, -300},
		{"I4→int32", secscodec.Int(secscodec.FormatI4, -70000), model.TypeInt32, -70000},
		{"I8→int64", secscodec.Int(secscodec.FormatI8, -1<<40), model.TypeInt64, -(1 << 40)},
		{"F4→float32", secscodec.Float(secscodec.FormatF4, 21.5), model.TypeFloat32, 21.5},
		{"F8→float64", secscodec.Float(secscodec.FormatF8, 0.1), model.TypeFloat64, 0.1},
		// 整数点接受浮点格式的「整数」值：很多设备把计数编成 F8。
		{"F8→uint32 整数", secscodec.Float(secscodec.FormatF8, 42), model.TypeUint32, 42},
		// 字符串里是数字也接受（有的设备把数值编成 A）。
		{"A→int32", secscodec.ASCII(" 42 "), model.TypeInt32, 42},
		{"A→float64", secscodec.ASCII("21.5"), model.TypeFloat64, 21.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := itemValue(tc.item, tc.dt, 0)
			if err != nil {
				t.Fatalf("itemValue: %v", err)
			}
			if v.Num != tc.want {
				t.Fatalf("值 = %v, want %v", v.Num, tc.want)
			}
			if v.Type != tc.dt {
				t.Fatalf("类型 = %s, want %s", v.Type, tc.dt)
			}
		})
	}
}

func TestItemValueErrors(t *testing.T) {
	cases := []struct {
		name string
		item *secscodec.Item
		dt   model.DataType
	}{
		// 小数不能塞进整数点：截断会让下游看到「21.5 变成 21」。
		{"F4 小数→uint32", secscodec.Float(secscodec.FormatF4, 21.5), model.TypeUint32},
		// 越界同样拒绝，不静默回绕。
		{"U4 超 uint16", secscodec.Int(secscodec.FormatU4, 70000), model.TypeUint16},
		{"I4 负值→uint16", secscodec.Int(secscodec.FormatI4, -1), model.TypeUint16},
		// 非数字字符串。
		{"A 非数字→int32", secscodec.ASCII("RUN"), model.TypeInt32},
		// 列表项不是标量。
		{"L→uint32", secscodec.List(secscodec.Int(secscodec.FormatU4, 1)), model.TypeUint32},
		// BOOLEAN 不隐式当 0/1：那会让「类型声明错了」这件事永远看不出来。
		{"BOOLEAN→uint32", secscodec.Bool(true), model.TypeUint32},
		// 字符串点拿到整数项。
		{"U4→string", secscodec.Int(secscodec.FormatU4, 1), model.TypeString},
		{"A→bool", secscodec.ASCII("true"), model.TypeBool},
		{"U4→bytes", secscodec.Int(secscodec.FormatU4, 1), model.TypeBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := itemValue(tc.item, tc.dt, 0); err == nil {
				t.Fatalf("%s 必须失败", tc.name)
			}
		})
	}
}

func TestItemValueStringAndBytes(t *testing.T) {
	v, err := itemValue(secscodec.ASCII("RUN"), model.TypeString, 0)
	if err != nil || v.Text != "RUN" {
		t.Fatalf("字符串 = %+v err=%v", v, err)
	}
	// length 是上限而不是定长：短于上限要接受。
	if _, err := itemValue(secscodec.ASCII("RUN"), model.TypeString, 3); err != nil {
		t.Fatalf("等长应接受: %v", err)
	}
	if _, err := itemValue(secscodec.ASCII("RUNNING"), model.TypeString, 3); err == nil {
		t.Fatal("超长字符串必须失败")
	}

	b, err := itemValue(secscodec.Binary([]byte{0x01, 0x02}), model.TypeBytes, 0)
	if err != nil || b.Text != string([]byte{0x01, 0x02}) {
		t.Fatalf("字节 = %+v err=%v", b, err)
	}
	if _, err := itemValue(secscodec.Binary([]byte{1, 2, 3}), model.TypeBytes, 2); err == nil {
		t.Fatal("超长字节必须失败")
	}

	tv, err := itemValue(secscodec.Bool(true), model.TypeBool, 0)
	if err != nil || !tv.Bool {
		t.Fatalf("布尔 = %+v err=%v", tv, err)
	}
}

func TestValueItemAllTypes(t *testing.T) {
	cases := []struct {
		dt   model.DataType
		val  model.Value
		want secscodec.Format
	}{
		{model.TypeBool, model.BoolValue(true), secscodec.FormatBoolean},
		{model.TypeString, model.TextValue(model.TypeString, "AUTO"), secscodec.FormatASCII},
		{model.TypeBytes, model.BytesValue([]byte{0xAA}), secscodec.FormatBinary},
		{model.TypeInt16, model.NumValue(model.TypeInt16, -3), secscodec.FormatI2},
		{model.TypeUint16, model.NumValue(model.TypeUint16, 3), secscodec.FormatU2},
		{model.TypeInt32, model.NumValue(model.TypeInt32, -3), secscodec.FormatI4},
		{model.TypeUint32, model.NumValue(model.TypeUint32, 3), secscodec.FormatU4},
		{model.TypeInt64, model.NumValue(model.TypeInt64, -3), secscodec.FormatI8},
		{model.TypeUint64, model.NumValue(model.TypeUint64, 3), secscodec.FormatU8},
		{model.TypeFloat32, model.NumValue(model.TypeFloat32, 1.5), secscodec.FormatF4},
		{model.TypeFloat64, model.NumValue(model.TypeFloat64, 1.5), secscodec.FormatF8},
	}
	for _, tc := range cases {
		it, err := valueItem(tc.val, tc.dt)
		if err != nil {
			t.Fatalf("%s: %v", tc.dt, err)
		}
		if it.Format != tc.want {
			t.Fatalf("%s 编码成 %s, want %s", tc.dt, secscodec.FormatName(it.Format), secscodec.FormatName(tc.want))
		}
	}

	// 字符串值不能编成整数点：这正是「API 传错类型」时要给出的诚实失败。
	if _, err := valueItem(model.TextValue(model.TypeString, "x"), model.TypeUint32); err == nil {
		t.Fatal("字符串值编成 uint32 必须失败")
	}
	if _, err := valueItem(model.NumValue(model.TypeUint32, 1), model.DataType("bcd16")); err == nil {
		t.Fatal("未知数据类型必须失败")
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestIsTimeout(t *testing.T) {
	if !isTimeout(os.ErrDeadlineExceeded) {
		t.Fatal("os.ErrDeadlineExceeded 应算超时")
	}
	if !isTimeout(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded 应算超时")
	}
	if isTimeout(errors.New("boom")) {
		t.Fatal("普通错误不算超时")
	}
	// net.Error 的 Timeout() 分支：*net.OpError 包着 os.ErrDeadlineExceeded 时
	// errors.Is 已经能命中，这里构造一个只实现 Timeout() 的自定义错误。
	if !isTimeout(timeoutErr{}) {
		t.Fatal("实现 net.Error 且 Timeout()=true 的错误应算超时")
	}
	if isTimeout(wrapped{errors.New("x")}) {
		t.Fatal("被包装的普通错误不算超时")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type wrapped struct{ err error }

func (w wrapped) Error() string { return w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }

func TestQualityFor(t *testing.T) {
	cases := map[driver.ErrorKind]model.Quality{
		driver.ErrTimeout:      model.QualityTimeout,
		driver.ErrDisconnected: model.QualityCommError,
		driver.ErrAddress:      model.QualityAddressError,
		driver.ErrProtocol:     model.QualityBad,
		driver.ErrPermission:   model.QualityBad,
		driver.ErrInternal:     model.QualityBad,
	}
	for kind, want := range cases {
		if got := qualityFor(kind); got != want {
			t.Fatalf("qualityFor(%s) = %s, want %s", kind, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 握手与事务的失败路径（用一个只说一半 HSMS 的假设备）
// ---------------------------------------------------------------------------

// fakeLink 起一个只应答指定几步的 TCP 服务端，用来构造真仿真器不会产生的
// 异常应答：被拒的 Select、错误的应答类型、沉默的设备。
func fakeLink(t *testing.T, handle func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		handle(c)
	}()
	return ln.Addr().String()
}

// readFrame 从连接读一帧完整 HSMS 消息。
func readFrame(t *testing.T, c net.Conn) (secscodec.Message, error) {
	t.Helper()
	head := make([]byte, secscodec.FrameLen)
	if _, err := io.ReadFull(c, head); err != nil {
		return secscodec.Message{}, err
	}
	n, err := secscodec.FrameDataLen(head)
	if err != nil {
		return secscodec.Message{}, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		return secscodec.Message{}, err
	}
	return secscodec.Decode(append(head, body...))
}

func writeMsg(c net.Conn, m secscodec.Message) {
	raw, err := secscodec.Encode(m)
	if err != nil {
		return
	}
	_, _ = c.Write(raw)
}

func dialConfig(addr string, pts ...model.Point) driver.OpenConfig {
	return driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch", Protocol: "secs",
			Transport: model.Transport{Kind: "tcp", Address: addr},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
		Device: model.Device{
			ID: "dev", ChannelID: "ch",
			Timeout: model.Duration(300 * time.Millisecond),
			Labels:  map[string]string{"commTimeout": "300ms", "retries": "0"},
			Points:  pts,
		},
	}
}

// Select 被拒时 Open 必须失败：链路没选上就发 DATA，设备只会回 Reject，
// 而调用方会以为是「地址不对」。
func TestOpenFailsOnSelectRejected(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		m, err := readFrame(t, c)
		if err != nil || m.MsgType != secscodec.MsgTypeSelectReq {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(m.SystemBytes, secscodec.StatusTooMuch))
	})
	_, err := New().Open(context.Background(), dialConfig(addr))
	if err == nil {
		t.Fatal("Select 被拒时 Open 必须失败")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("Select 被拒应 disconnected（可重连），得到 %v", err)
	}
}

// 第一个应答不是 Select.rsp：说明对端说的不是 HSMS，属于配置错误而非链路抖动。
func TestOpenFailsOnWrongHandshakeReply(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		m, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewLinktestRsp(m.SystemBytes, secscodec.StatusOK))
	})
	_, err := New().Open(context.Background(), dialConfig(addr))
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("错误应答类型应 protocol，得到 %v", err)
	}
}

// 设备不说话：Open 在 commTimeout 内放弃，分类是 timeout（可重试）。
func TestOpenTimesOutOnSilentPeer(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		// 读掉 Select 后什么也不回，等客户端超时。
		_, _ = readFrame(t, c)
		time.Sleep(2 * time.Second)
	})
	start := time.Now()
	_, err := New().Open(context.Background(), dialConfig(addr))
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("沉默设备应 timeout，得到 %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("Open 耗时 %s，commTimeout 没生效", d)
	}
}

// DATA 没有应答：Read 返回 timeout，且每个点都被标 timeout 质量。
// 这是「设备在线但忙」与「网络断」的区分点，调度器据此决定重试而不是熔断。
func TestReadTimesOutWithoutSecondary(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		m, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(m.SystemBytes, secscodec.StatusOK))
		// 之后收到的 DATA 一律不回。
		_, _ = readFrame(t, c)
		time.Sleep(2 * time.Second)
	})
	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("无应答应 timeout，得到 %v", err)
	}
	if len(samples) != 1 || samples[0].Err == nil {
		t.Fatalf("样本 = %+v", samples)
	}
	if samples[0].Quality != model.QualityTimeout {
		t.Fatalf("质量 = %s, want timeout", samples[0].Quality)
	}
}

// 应答的系统字节对不上（上一次迟到的应答）：必须继续等，而不是把旧值当成
// 这次的结果——那会让所有点永久滞后一个周期。
func TestExchangeIgnoresStaleSecondary(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))

		req, err := readFrame(t, c)
		if err != nil {
			return
		}
		// 先回一条系统字节错、值也错的应答。
		writeMsg(c, secscodec.DataMessage{
			SystemBytes: req.SystemBytes + 1, Stream: 1, Function: 4,
			Body: mustEncode(secscodec.EncodeValueReply([]*secscodec.Item{
				secscodec.Int(secscodec.FormatU4, 999),
			})),
		}.ToMessage())
		// 再回正确的应答。
		writeMsg(c, secscodec.DataMessage{
			SystemBytes: req.SystemBytes, Stream: 1, Function: 4,
			Body: mustEncode(secscodec.EncodeValueReply([]*secscodec.Item{
				secscodec.Int(secscodec.FormatU4, 25),
			})),
		}.ToMessage())
	})
	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(samples) != 1 || samples[0].Value.Num != 25 {
		t.Fatalf("取到了迟到的旧应答: %+v", samples)
	}
}

// 畸形帧头（数据长度字段被翻成天文数字）：必须报 protocol 而不是无限等。
func TestReadMalformedHeader(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
		if _, err := readFrame(t, c); err != nil {
			return
		}
		// 12 字节头，长度字段（偏移 6..9）= 0xFFFFFFFF。
		head := make([]byte, secscodec.FrameLen)
		head[0] = 0x81 // Stream 1 + W
		head[1] = 0x03
		head[2], head[3], head[4], head[5] = 0x00, 0x00, 0x00, 0x01
		head[6], head[7], head[8], head[9] = 0xFF, 0xFF, 0xFF, 0xFF
		head[10] = secscodec.MsgTypeDataMsg
		head[11] = 1
		_, _ = c.Write(head)
		time.Sleep(2 * time.Second)
	})
	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("畸形帧头应 protocol，得到 %v", err)
	}
}

func mustEncode(body []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return body
}
