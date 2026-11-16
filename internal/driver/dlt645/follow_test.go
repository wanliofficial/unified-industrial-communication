package dlt645

import (
	"context"
	"net"
	"testing"
	"time"

	"uic/internal/codec/dlt645"
	"uic/internal/driver"
	"uic/internal/model"
)

// TestFollowFrameCountBound 钉住「后续帧上限按**帧数**而不是只按字节数」。
//
// 早先只有 `len(data) > MaxData*maxFollow`（约 12.8KB）这一道字节上限。
// 一台「每帧只回 1 字节、且永远置后续位」的坏表（或一条把 D5 咬住的噪声）
// 能把这个循环拖到上万次事务；按每次 200ms 超时算，一轮采集直接变成几十分钟，
// 而字节上限永远碰不到。帧数上限必须先把它拦住。
//
// 这里起一台「永远有后续帧、每帧只回 1 字节」的假表。
func TestFollowFrameCountBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go stuckSlave(ln)

	dev := model.Device{
		ID: "meter", ChannelID: "ch", Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(2 * time.Second),
		Labels:   map[string]string{"address": "000000000042", "options": "gap=1ms,gap=1ms"},
	}
	c, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "dlt645",
			Transport: model.Transport{Kind: "tcp", Address: ln.Addr().String()},
			Timeout:   model.Duration(2 * time.Second)},
		Device: dev,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = c.Close() }()

	done := make(chan []driver.RawSample, 1)
	go func() {
		got, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{{
			ID: "blk", DataType: model.TypeBytes,
			Address: model.Address{Kind: model.AddressRegister, Area: "0201FF00", Bit: -1, Length: 8},
		}}}})
		done <- got
	}()
	select {
	case got := <-done:
		if len(got) == 1 && got[0].Err == nil {
			t.Fatalf("「永远有后续帧」的从站被当成读成功：%+v", got[0])
		}
	case <-time.After(30 * time.Second):
		t.Fatal("驱动在「永远有后续帧」的从站上没有停下——帧数上限缺失")
	}
}

// stuckSlave 接受一条连接，对任何请求都回「1 字节数据 + D5 后续位」。
func stuckSlave(ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	addr := [dlt645.AddrLen]byte{0x42}
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 256)
	for {
		n, rerr := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				fr, consumed, perr := dlt645.Parse(buf)
				if perr != nil {
					if consumed > 0 && consumed <= len(buf) {
						buf = buf[consumed:]
					}
					break
				}
				buf = buf[consumed:]
				var reply []byte
				if fr.Func() == dlt645.CtrlRead {
					// 首帧：DI 回显 + 1 字节数据，置后续位。
					reply, _ = dlt645.Build(addr, fr.Ctrl|dlt645.BitReply|dlt645.BitFollow,
						append(fr.Data[:4], 0x00))
				} else {
					// 读后续：DI 回显 + 1 字节数据 + 序号，仍然置后续位。
					seq := fr.Data[4]
					reply, _ = dlt645.Build(addr, fr.Ctrl|dlt645.BitReply|dlt645.BitFollow,
						append(append(fr.Data[:4], 0x00), seq))
				}
				if reply != nil {
					_, _ = conn.Write(reply)
				}
			}
		}
		if rerr != nil {
			return
		}
	}
}
