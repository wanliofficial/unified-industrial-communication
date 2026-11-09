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

// 重发与取消：这两条路径决定「偶发丢包」与「上层主动放弃」时的行为，
// 一个走错就会让一台偶发丢包的设备被当成永久故障，或者让取消请求卡住不返回。

// retries=1 时，第一次 S1F3 无应答，重发后成功：Read 必须给出正常值。
// 这条路径是「设备偶尔忙」的真实写照，也正是重发参数存在的理由。
func TestReadRetriesAfterDrop(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))

		_, _ = readFrame(t, c) // 第一次 S1F3：丢弃
		req, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.DataMessage{
			SystemBytes: req.SystemBytes, Stream: 1, Function: 4,
			Body: mustEncode(secscodec.EncodeStatusReply([]secscodec.StatusValue{
				{ID: 1, Value: secscodec.Int(secscodec.FormatU4, 25)},
			})),
		}.ToMessage())
	})

	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	// 第一次等待要短，否则整个测试要等满两次 300ms；这里不用默认值。
	cfg.Device.Labels = map[string]string{"commTimeout": "300ms", "retries": "1"}
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if err != nil {
		t.Fatalf("重发后应成功: %v", err)
	}
	if len(samples) != 1 || samples[0].Value.Num != 25 {
		t.Fatalf("样本 = %+v", samples)
	}
}

// 上层取消（ctx 到期）时 Read 必须尽快返回 timeout，而不是等满设备超时
// 再重发一遍。调度器的关闭预算只有 10 秒，卡住会拖垮整个关闭流程。
func TestReadHonorsContextCancel(t *testing.T) {
	addr := fakeLink(t, func(c net.Conn) {
		sel, err := readFrame(t, c)
		if err != nil {
			return
		}
		writeMsg(c, secscodec.NewSelectRsp(sel.SystemBytes, secscodec.StatusOK))
		_, _ = readFrame(t, c)
		time.Sleep(3 * time.Second)
	})
	// 设备超时很长（5s），但 ctx 只给 150ms：必须由 ctx 决定。
	cfg := dialConfig(addr, svidPoint("p", 1, model.TypeUint32))
	cfg.Device.Timeout = model.Duration(5 * time.Second)
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		driver.PointRefOf(cfg.Device, cfg.Device.Points[0]),
	}}})
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("ctx 到期应 timeout，得到 %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("ctx 取消后 %s 才返回，太慢", d)
	}
}

// Read 里遇到解析不了的点位地址：该点标 address_error，其余点照常读。
// 批量读的价值就是「一个坏地址不拖垮整组」。
func TestReadBadAddressAlongsideGood(t *testing.T) {
	srv := startSim(t)
	cfg := testConfig(srv, svidPoint("good", 1, model.TypeUint32))
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	good := driver.PointRefOf(cfg.Device, cfg.Device.Points[0])
	bad := driver.PointRef{
		ID:       "bad",
		Address:  model.Address{Kind: model.AddressObject, ObjectType: "nope", Instance: 1, Property: "value"},
		DataType: model.TypeUint32,
	}
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{bad, good}}})
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("应返回 address 错误，得到 %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("样本数 = %d, want 2", len(samples))
	}
	byID := map[string]driver.RawSample{}
	for _, s := range samples {
		byID[s.PointID] = s
	}
	if byID["bad"].Quality != model.QualityAddressError {
		t.Fatalf("bad = %+v", byID["bad"])
	}
	if byID["good"].Err != nil || byID["good"].Value.Num != 25 {
		t.Fatalf("好点不该被连累: %+v", byID["good"])
	}
}

// Probe 只接受 Linktest.rsp：其它控制消息（这里是 Reject）不算「还活着」。
func TestProbeRejectsNonLinktest(t *testing.T) {
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
		writeMsg(c, secscodec.Message{
			SystemBytes: req.SystemBytes, MsgType: secscodec.MsgTypeRejectReq,
			Payload: []byte{0x00, 0x02, 0x00, 0x00},
		})
	})
	c, err := New().Open(context.Background(), dialConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Probe(context.Background()); driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("Reject 不算探活成功，应 protocol，得到 %v", err)
	}
}
