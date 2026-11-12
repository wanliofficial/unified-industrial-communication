package snmp

import (
	"context"
	"testing"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
	snmpsim "uic/internal/sim/snmp"
	"uic/internal/transport"
)

// TestV3EndToEnd 是最重要的一条：用内置代理跑完整的 USM 流程
// （发现 → 认证 → 加密 → 读 → 写）。
//
// 这条测试必须在 sim 里真的做密钥派生与 HMAC：如果两侧各写一份不
// 一致的实现，「自己骗自己」的绿是可能的；只有让它们互为对端，才
// 能证明派生流程符合协议。
func TestV3EndToEnd(t *testing.T) {
	const (
		authPass = "authpass123"
		privPass = "privpass123"
		user     = "monitor"
	)
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{
			Users: map[string]snmpsim.USMUser{
				user: {
					Name:         user,
					Auth:         snmpcodec.AuthSHA256,
					AuthPassword: authPass,
					Priv:         snmpcodec.PrivAES,
					PrivPassword: privPass,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	d := NewWithDialer(func(ctx context.Context, _ string, timeout time.Duration) (transport.Datagram, error) {
		return transport.DialUDP(ctx, srv.Addr(), timeout)
	})
	cfg := driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch1", Protocol: "snmp",
			Transport: model.Transport{Kind: "udp", Address: srv.Addr()},
			Timeout:   model.Duration(500 * time.Millisecond),
		},
		Device: model.Device{ID: "dev1", ChannelID: "ch1", Labels: map[string]string{
			"version":      "3",
			"username":     user,
			"authProtocol": "SHA-256",
			"authPassword": authPass,
			"privProtocol": "AES",
			"privPassword": privPass,
			"options":      "retries=1",
		}},
	}
	conn, err := d.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if srv.Reports() == 0 {
		t.Fatal("expected at least one Report during discovery")
	}

	out, err := conn.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
		point("contact", "1.3.6.1.2.1.1.4.0", model.TypeString),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, s := range out {
		if s.Quality != model.QualityGood {
			t.Fatalf("read %s = %+v", s.PointID, s)
		}
	}

	res, err := conn.Write(ctx, []driver.WriteRequest{{
		PointID:  "contact",
		DataType: model.TypeString,
		Address:  model.Address{Kind: model.AddressSymbol, Path: "1.3.6.1.2.1.1.4.0"},
		Value:    model.TextValue(model.TypeString, "noc@example.invalid"),
	}})
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("write = %+v, %v", res, err)
	}
	if v, ok := srv.Get("1.3.6.1.2.1.1.4.0"); !ok || string(v.Bytes) != "noc@example.invalid" {
		t.Fatalf("sim value = %+v", v)
	}
}

// TestV3WrongPassword 钉住「口令错 → 权限错误」，而不是「超时」。
//
// 这是 USM 排障里最重要的一条：把认证失败报成超时，运维会去查
// 网络与 ACL，永远找不到真正的原因。
func TestV3WrongPassword(t *testing.T) {
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "correct-pass"},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["authPassword"] = "wrong-pass"
	})
	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("wrong password = %v (kind %v), want permission", err, driver.KindOf(err))
	}
}

// TestV3UnknownUser 钉住「用户不存在」也走权限错误。
func TestV3UnknownUser(t *testing.T) {
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"someone": {Name: "someone", Auth: snmpcodec.AuthSHA1, AuthPassword: "correct-pass"},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["username"] = "ghost"
	})
	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("unknown user = %v", err)
	}
}

// TestV3SecurityLevelMismatch：驱动配 authPriv、代理只允许 authNoPriv。
func TestV3SecurityLevelMismatch(t *testing.T) {
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: "authpass123"},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["authPassword"] = "authpass123"
		l["privProtocol"] = "AES"
		l["privPassword"] = "privpass123"
	})
	_, err = c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("security level mismatch = %v", err)
	}
}

// TestV3AuthNoPriv 覆盖不加密的 v3：认证但明文传输。
// 这是现场很常见的一档（性能考虑），代码路径与 authPriv 不同。
func TestV3AuthNoPriv(t *testing.T) {
	const authPass = "authpass123"
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthMD5, AuthPassword: authPass},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["authProtocol"] = "MD5"
		l["authPassword"] = authPass
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityGood {
		t.Fatalf("read = %+v", out[0])
	}
}

// TestV3NoAuthNoPriv 覆盖「发现即用」的最弱一档。
func TestV3NoAuthNoPriv(t *testing.T) {
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"monitor": {Name: "monitor"},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["authProtocol"] = "none"
		delete(l, "authPassword")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityGood {
		t.Fatalf("read = %+v", out[0])
	}
}

// TestV3TimeWindowFailure：代理始终报时间窗口错，最终必须是协议错误。
//
// 如果驱动在这里无限重试（每轮都重新发现），一个时钟配置错误的站点
// 会让采集线程永远转下去。
func TestV3TimeWindowFailure(t *testing.T) {
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{
			Users: map[string]snmpsim.USMUser{
				"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA1, AuthPassword: "authpass123"},
			},
			Faults: snmpsim.USMFaults{NotInTimeWindow: true},
		},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) { l["authPassword"] = "authpass123" })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err = c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("time-window failure = %v (kind %v)", err, driver.KindOf(err))
	}
}

// openV3 用内置代理开一条 v3 连接，mutate 用于调整标签。
func openV3(t *testing.T, addr string, mutate func(map[string]string)) *connState {
	t.Helper()
	labels := map[string]string{
		"version":      "3",
		"username":     "monitor",
		"authProtocol": "SHA-256",
		"authPassword": "authpass123",
		"options":      "retries=1",
	}
	mutate(labels)
	d := NewWithDialer(func(ctx context.Context, _ string, timeout time.Duration) (transport.Datagram, error) {
		return transport.DialUDP(ctx, addr, timeout)
	})
	cfg := driver.OpenConfig{
		Channel: model.Channel{
			ID: "ch1", Protocol: "snmp",
			Transport: model.Transport{Kind: "udp", Address: addr},
			Timeout:   model.Duration(300 * time.Millisecond),
		},
		Device: model.Device{ID: "dev1", ChannelID: "ch1", Labels: labels},
	}
	conn, err := d.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*connState)
}
