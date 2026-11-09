package secs

import (
	"context"
	"net"
	"testing"
	"time"

	secscodec "uic/internal/codec/secs"
	"uic/internal/driver"
	"uic/internal/model"
)

// 写路径的分支覆盖：批量里有点被拒、事务超时、应答体畸形，以及「写一个
// 不在设备点表里的地址」（北向按地址写，不经过 Open 时的计划表）。

// 一次 S2F15 携带多个常数时，S2F16 只回一个 ACKC7：非 0 表示整批没有被接受。
// 驱动必须把这一批点全部标成失败，而不是「大概率都写进去了」。
func TestWriteBatchRejected(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv,
		ecidPoint("a", 10, model.TypeUint32, true),
		ecidPoint("b", 11, model.TypeString, true),
	)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{PointID: "a", Address: cfg.Device.Points[0].Address, DataType: model.TypeUint32,
			Value: model.NumValue(model.TypeUint32, 1)},
		// 编不出来的值：它不进 S2F15，但结果里必须有一行失败，不能消失。
		{PointID: "b", Address: cfg.Device.Points[1].Address, DataType: model.TypeString,
			Value: model.NumValue(model.TypeUint32, 1)},
	})
	if err == nil {
		t.Fatal("有失败项时应返回错误")
	}
	if len(res) != 2 {
		t.Fatalf("结果数 = %d, want 2（每个请求一行，成功的也要有）", len(res))
	}
	ok := 0
	for _, r := range res {
		if r.OK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("成功数 = %d, want 1: %+v", ok, res)
	}
	// 设备侧确实收到了那一批里唯一能编码的常数。
	if v, _ := srv.ValueOf(secscodec.ObjectECID, 10); v == nil {
		t.Fatal("ECID 10 应仍存在")
	} else if n, _ := v.U4(); n != 1 {
		t.Fatalf("ECID 10 = %d, want 1", n)
	}
}

// 设备不回应 S2F15：整批标 timeout，且错误分类是 timeout。
func TestWriteTimesOut(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
		_, _ = readFrame(t, c) // 读掉 S2F15，不回
		time.Sleep(2 * time.Second)
	})
	cfg := dialConfig(addr, ecidPoint("limit", 10, model.TypeUint32, true))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "limit", Address: cfg.Device.Points[0].Address,
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 5),
	}})
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("无应答应 timeout，得到 %v", err)
	}
	if len(res) != 1 || res[0].OK || res[0].Quality != model.QualityTimeout {
		t.Fatalf("结果 = %+v", res)
	}
}

// S2F16 的应答体不是 ACK：报 protocol，而不是「假定成功」。
func TestWriteMalformedAck(t *testing.T) {
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
		// 回一条 S2F16，但 body 是 ASCII 而不是二进制 ACKC7。
		writeMsg(c, secscodec.DataMessage{
			SystemBytes: req.SystemBytes, Stream: 2, Function: 16,
			Body: mustEncode(secscodec.EncodeItem(secscodec.ASCII("nope"))),
		}.ToMessage())
	})
	cfg := dialConfig(addr, ecidPoint("limit", 10, model.TypeUint32, true))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "limit", Address: cfg.Device.Points[0].Address,
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 5),
	}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("畸形 ACK 应 protocol，得到 %v", err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("畸形 ACK 不能算成功: %+v", res)
	}
}

// 写一个不在点表里的地址：必须被拒。
//
// writable 是设备配置里声明过的写权限；凭一个地址就允许写，等于让任何能
// 构造 API 请求的人绕过它。读没有这个约束（自检与按需读要按地址读）。
func TestWriteAddressNotInTable(t *testing.T) {
	srv := startSim(t)
	srv.SetECID(77, secscodec.Int(secscodec.FormatU4, 1))
	cfg := testConfig(srv) // 空点表
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID:  "ext",
		Address:  model.Address{Kind: model.AddressObject, ObjectType: "ecid", Instance: 77, Property: "value"},
		DataType: model.TypeUint32, Value: model.NumValue(model.TypeUint32, 42),
	}})
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("点表外的地址应 permission，得到 %v", err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("结果 = %+v", res)
	}
	v, ok := srv.ValueOf(secscodec.ObjectECID, 77)
	if !ok {
		t.Fatal("ECID 77 不存在")
	}
	if n, _ := v.U4(); n != 1 {
		t.Fatalf("ECID 77 = %d, want 1（不该被写到）", n)
	}
}

// 读一个不在点表里的地址，同样按地址解析（自检 CLI 与按需读都靠它）。
func TestReadAddressNotInTable(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ref := driver.PointRef{
		ID:       "ext",
		Address:  model.Address{Kind: model.AddressObject, ObjectType: "svid", Instance: 1, Property: "value"},
		DataType: model.TypeUint32,
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{ref}}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(samples) != 1 || samples[0].Value.Num != 25 {
		t.Fatalf("样本 = %+v（读到了别的编号说明按 byID 回查了）", samples)
	}
}

// 同一个编号在同一组里出现两次（同一地址两个点）：请求里保留两份对驱动无害，
// 关键是两个点都能拿到值，而不是后一个把前一个覆盖成空。
func TestReadDuplicateID(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv,
		svidPoint("p1", 1, model.TypeUint32),
		svidPoint("p2", 1, model.TypeUint32),
	)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	refs := make([]driver.PointRef, 0, 2)
	for _, p := range cfg.Device.Points {
		refs = append(refs, driver.PointRefOf(cfg.Device, p))
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: refs}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("样本数 = %d, want 2", len(samples))
	}
	for _, s := range samples {
		if s.Err != nil || s.Value.Num != 25 {
			t.Fatalf("%s = %+v", s.PointID, s)
		}
	}
}

// Probe 用 Linktest：设备不回时给 timeout，回错类型给 protocol。
func TestProbeFailureModes(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		addr := fakeLink(t, func(c net.Conn) {
			sel, err := readFrame(t, c)
			if err != nil {
				return
			}
			writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
			_, _ = readFrame(t, c)
			time.Sleep(2 * time.Second)
		})
		c, err := New().Open(context.Background(), dialConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrTimeout {
			t.Fatalf("Probe 应 timeout，得到 %v", err)
		}
	})

	t.Run("wrong-reply", func(t *testing.T) {
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
			writeMsg(c, secscodec.NewSelectRsp(req.SystemBytes, secscodec.StatusOK))
			time.Sleep(2 * time.Second)
		})
		c, err := New().Open(context.Background(), dialConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrProtocol {
			t.Fatalf("Probe 收到非 LinktestRsp 应 protocol，得到 %v", err)
		}
	})
}

// Close 会先发 Separate.req：假设备能收到它，链路就有一个干净的结束点。
func TestCloseSendsSeparate(t *testing.T) {
	got := make(chan uint8, 1)
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
		m, err := readFrame(t, c)
		if err != nil {
			return
		}
		got <- m.MsgType
	})
	c, err := New().Open(context.Background(), dialConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case mt := <-got:
		if mt != secscodec.MsgTypeSeparateReq {
			t.Fatalf("关闭前发的是 %s，want SeparateReq", secscodec.TypeName(mt))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("关闭时没有发出 Separate.req")
	}
}
