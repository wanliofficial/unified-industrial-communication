package main

import (
	"context"
	"testing"
	"time"

	"uic/internal/driver"
	toledodrv "uic/internal/driver/toledo"
	"uic/internal/model"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：字段名必须在发出任何帧
// 之前就拦住，并且报错要说清错在哪一层——Toledo 的失败在串口上全都表现成「没有
// 输出」，如果这里把字段名错报成超时，人会先去改波特率。

func TestToledoRefs(t *testing.T) {
	got := toledoRefs("  Weight , STATUS ,, Unit ")
	want := []string{"weight", "status", "unit"}
	if len(got) != len(want) {
		t.Fatalf("got %v，应为 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v，应为 %v", got, want)
		}
	}
}

func TestToledoDefaultType(t *testing.T) {
	// 默认类型来自驱动 Symbols() 这张唯一事实源：自检命令不再自维护一份。
	cases := map[string]string{
		"weight":   "float64",
		"status":   "string",
		"unit":     "string",
		"mode":     "string",
		"stable":   "bool",
		"motion":   "bool",
		"decimals": "int64",
	}
	for field, want := range cases {
		if got := string(toledodrv.SymbolType(field)); got != want {
			t.Errorf("SymbolType(%q) = %s，应为 %s", field, got, want)
		}
	}
}

func TestToledoFieldValidation(t *testing.T) {
	// 合法字段名（大小写不敏感，CLI 会先小写）都应被 KnownSymbol 认出。
	for _, good := range []string{"weight", "status", "unit", "mode", "stable", "motion", "decimals"} {
		if !toledodrv.KnownSymbol(good) {
			t.Fatalf("KnownSymbol(%q) 应为 true", good)
		}
	}
	// 非法字段名：把 status 写成中文名、把 weight 写成寄存器语法，都应在连秤前被拒。
	for _, bad := range []string{"温度", "0", "w", "gross_weight", "净重"} {
		if toledodrv.KnownSymbol(bad) {
			t.Fatalf("KnownSymbol(%q) 应为 false", bad)
		}
	}
}

func TestToledoFieldDescCoversAll(t *testing.T) {
	// 每个合法字段都应有一句话说明（自检命令的可读性依赖它）。
	//
	// 名单从驱动的 SymbolsOf() 派生：新增字段若忘了补说明，这里会红，而不是悄悄
	// 在自检输出里留空。
	for _, f := range toledodrv.SymbolsOf() {
		if toledoFieldDesc(f) == "" {
			t.Errorf("字段 %q 缺少说明（在 toledoFieldDesc 里补一句）", f)
		}
	}
}

// ---------------------------------------------------------------------------
// 收帧判据与统计输出
// ---------------------------------------------------------------------------

// hasFreshFrame 是「等首帧」循环的判据。
//
// 不能用「样本数 > 0」：驱动在还没收到帧时也会为每个点位返回一条 comm_error
// 样本（否则采集层会把点位当响应缺项）。用样本数判会让这段循环只读一次，
// 于是「连上但仪表还没开始推帧」直接显示成 comm_error，看起来像设备坏了。
func TestHasFreshFrame(t *testing.T) {
	commErr := []driver.RawSample{
		{PointID: "weight", Quality: model.QualityCommError},
		{PointID: "status", Quality: model.QualityCommError},
	}
	if hasFreshFrame(commErr) {
		t.Fatal("全是 comm_error 时不算拿到帧")
	}
	if hasFreshFrame(nil) {
		t.Fatal("空样本不算拿到帧")
	}
	// 只要有一个点位不是 comm_error（stale/uncertain/bad 都算收到过帧）。
	for _, q := range []model.Quality{
		model.QualityGood, model.QualityStale, model.QualityUncertain, model.QualityBad,
	} {
		samples := append(append([]driver.RawSample(nil), commErr...),
			driver.RawSample{PointID: "unit", Quality: q})
		if !hasFreshFrame(samples) {
			t.Fatalf("存在 %s 样本时应认为已收到帧", q)
		}
	}
}

// statsOf 通过可选接口取收帧统计；驱动不提供时安全返回 false。
func TestStatsOf(t *testing.T) {
	// 不实现 Stats 的连接：自检命令不应因此崩掉或输出错位。
	// 用独立的桩类型（而不是给 stubConn 加开关）：类型要么实现接口、要么不实现，
	// 否则这条断言测不到任何事。
	if _, ok := statsOf(noStatsConn{}); ok {
		t.Fatal("不提供 Stats 的连接不应报告成功")
	}

	// 提供 Stats 的连接：字段要原样映射（FrameAge 转毫秒）。
	got, ok := statsOf(statsConn{})
	if !ok {
		t.Fatal("提供 Stats 的连接应报告成功")
	}
	want := toledoStats{
		BytesSeen: 1234, Dropped: 7, BadFrames: 2,
		HasFrame: true, FrameAge: 1500,
		LastError: "boom", Raw: "ST,GS,+00149.0kg",
	}
	if got != want {
		t.Fatalf("statsOf = %+v，期望 %+v", got, want)
	}
}

// noStatsConn 是最小的 driver.Conn 实现，**不**暴露 Stats。
type noStatsConn struct{}

func (noStatsConn) Probe(context.Context) error { return nil }
func (noStatsConn) Read(context.Context, []driver.ReadSpan) ([]driver.RawSample, error) {
	return nil, nil
}
func (noStatsConn) Write(context.Context, []driver.WriteRequest) ([]driver.WriteResult, error) {
	return nil, nil
}
func (noStatsConn) Close() error { return nil }

// statsConn 在最小 Conn 之外额外暴露 Stats，验证接口断言与字段映射。
type statsConn struct{}

func (statsConn) Probe(context.Context) error { return nil }
func (statsConn) Read(context.Context, []driver.ReadSpan) ([]driver.RawSample, error) {
	return nil, nil
}
func (statsConn) Write(context.Context, []driver.WriteRequest) ([]driver.WriteResult, error) {
	return nil, nil
}
func (statsConn) Close() error { return nil }

func (statsConn) Stats() toledodrv.Stats {
	return toledodrv.Stats{
		BytesSeen: 1234, Dropped: 7, BadFrames: 2,
		HasFrame: true, FrameAge: 1500 * time.Millisecond,
		LastError: "boom", Raw: "ST,GS,+00149.0kg",
	}
}
