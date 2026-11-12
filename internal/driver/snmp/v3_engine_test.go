package snmp

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
	snmpsim "uic/internal/sim/snmp"
)

// TestV3FixedEngineID 钉住「配置里写了 engineID」这条路。
//
// 固定 engineID 的常见误解是「既然身份已知，就不用再问代理任何东西了」。
// 这个误解的代价很高：engineBoots/engineTime 仍是 0/0，第一条真请求必然
// 吃一个 notInTimeWindow，重试一轮还是 0/0，最终报「代理时间窗口不同步」
// ——而真正的原因是驱动从没问过时间基准。
//
// 因此本用例断言两件事：
//  1. 读写都能成功（说明时间基准确实学到了）；
//  2. 整个会话里只出现一次 Report（说明没有跳过引擎身份，也没有重复
//     做一次空 engineID 的发现）。
func TestV3FixedEngineID(t *testing.T) {
	const (
		authPass = "authpass123"
		user     = "monitor"
	)
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			user: {Name: user, Auth: snmpcodec.AuthSHA256, AuthPassword: authPass},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	hexID := hex.EncodeToString(srv.EngineID())
	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["authPassword"] = authPass
		l["engineID"] = hexID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("probe with fixed engine ID: %v", err)
	}
	out, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out[0].Quality != model.QualityGood {
		t.Fatalf("read = %+v", out[0])
	}
	// 一次时间同步的 Report + 之后不再有 Report。多于一次说明每轮请求都
	// 在重新报时间窗口，那正是本用例要防的症状。
	if got := srv.Reports(); got != 1 {
		t.Fatalf("Reports = %d, want 1 (one time-sync report, no repeated discovery)", got)
	}
}

// TestV3FixedEngineIDMismatch：配置里的 engineID 与代理不一致必须明说。
//
// 静默改用代理报的 engineID 会让「配置写错了」变成「看起来一切正常」，
// 现场再也发现不了那份配置是错的；因此这里要求错误信息里同时出现两个
// engineID。
func TestV3FixedEngineIDMismatch(t *testing.T) {
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: "authpass123"},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	// 造一个长度合法、内容必错的 engineID（与内置代理的首字节不同）。
	wrong := "80001f8880000000ff"
	c := openV3(t, srv.Addr(), func(l map[string]string) {
		l["authPassword"] = "authpass123"
		l["engineID"] = wrong
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
	}}})
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("mismatched engine ID = %v (kind %v), want protocol", err, driver.KindOf(err))
	}
	msg := err.Error()
	if !strings.Contains(msg, wrong) || !strings.Contains(msg, hex.EncodeToString(srv.EngineID())) {
		t.Fatalf("error should name both engine IDs, got %q", msg)
	}
}

// TestV3EngineResetRecovers：代理换了 engineID 之后会话能自己恢复。
//
// 现场对应「代理重装/恢复出厂/换主控板」：我们缓存的身份失效，代理回
// unknownEngineIDs。此时必须丢掉旧身份重做发现，而不是拿旧身份反复算
// 密钥（那只会得到同一个 Report）。这里用 sim 的 EngineID 选项切换身份，
// 模拟同一个地址背后的新引擎。
func TestV3EngineResetRecovers(t *testing.T) {
	const authPass = "authpass123"
	srv, err := snmpsim.NewServerWithOptions("127.0.0.1:0", snmpsim.Options{
		V3: &snmpsim.USMConfig{Users: map[string]snmpsim.USMUser{
			"monitor": {Name: "monitor", Auth: snmpcodec.AuthSHA256, AuthPassword: authPass},
		}},
	})
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	defer srv.Close()

	c := openV3(t, srv.Addr(), func(l map[string]string) { l["authPassword"] = authPass })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	readDescr := func(stage string) {
		t.Helper()
		out, err := c.Read(ctx, []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
			point("descr", "1.3.6.1.2.1.1.1.0", model.TypeString),
		}}})
		if err != nil {
			t.Fatalf("%s read: %v", stage, err)
		}
		if out[0].Quality != model.QualityGood {
			t.Fatalf("%s read = %+v", stage, out[0])
		}
	}

	readDescr("before reset")
	// 换掉引擎身份：会话里缓存的 engineID 从此失效。
	srv.SetEngineID([]byte{0x80, 0x00, 0x1f, 0x88, 0x80, 0x00, 0x00, 0x00, 0x07})
	readDescr("after reset")
}

// TestV3FixedEngineIDNoAuth 覆盖「固定 engineID + 最弱安全级别」的组合：
// 没有密钥可派生，时间同步完全靠 Report 里的明文 security parameters。
func TestV3FixedEngineIDNoAuth(t *testing.T) {
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
		l["engineID"] = hex.EncodeToString(srv.EngineID())
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
