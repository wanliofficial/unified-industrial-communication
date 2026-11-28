package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/alarm"
	"uic/internal/driver"
	"uic/internal/driver/computed"
	"uic/internal/driver/modbus"
	"uic/internal/driver/virtual"
	"uic/internal/model"
)

func registerDrivers(t *testing.T) {
	t.Helper()
	// 逐个检查而不是「virtual 在就整体跳过」：后者会让后加进列表的驱动
	// 在同一个测试进程里永远注册不上（前面的用例已经注册过 virtual）。
	for _, d := range []driver.Driver{virtual.New(), modbus.New(), modbus.NewRTU(), computed.New()} {
		if _, ok := driver.Get(d.Meta().ID); ok {
			continue
		}
		if err := driver.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.Meta().ID, err)
		}
	}
}

func baseFile() *File {
	return &File{
		SchemaVersion: SchemaVersion,
		Site:          "plant-01",
		Server:        Server{Listen: ":8080"},
		Channels: []model.Channel{{
			ID: "ch1", Protocol: "virtual",
			Transport: model.Transport{Kind: "memory"},
		}},
		Devices: []model.Device{{
			ID: "dev1", ChannelID: "ch1", Interval: model.Duration(1e9), Enabled: true,
			Points: []model.Point{{
				ID: "p1", DataType: model.TypeFloat32, Enabled: true,
				Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 2, Bit: -1},
			}},
		}},
		Storage: Storage{Dir: "./data"},
	}
}

func TestValidateAcceptsMinimalConfig(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	if err := f.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if f.Server.Listen != ":8080" || f.Storage.OutboxTTL <= 0 || f.Logging.Level != "info" {
		t.Fatalf("defaults not applied: %+v", f)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	f.Validate()
	first, err := f.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	v1 := VersionOf(first)
	f.Normalize()
	f.Normalize()
	second, _ := f.Canonical()
	if VersionOf(second) != v1 {
		t.Fatal("Normalize must be idempotent, or version hashes drift")
	}
}

func TestValidateRejects(t *testing.T) {
	registerDrivers(t)
	cases := map[string]func(*File){
		"bad site":          func(f *File) { f.Site = "Plant_01" },
		"no channels":       func(f *File) { f.Channels = nil; f.Devices = nil },
		"duplicate channel": func(f *File) { f.Channels = append(f.Channels, f.Channels[0]) },
		"unknown protocol":  func(f *File) { f.Channels[0].Protocol = "nope" },
		"missing address":   func(f *File) { f.Channels[0].Transport.Kind = "tcp" },
		"channel not found": func(f *File) { f.Devices[0].ChannelID = "ghost" },
		"duplicate device":  func(f *File) { f.Devices = append(f.Devices, f.Devices[0]) },
		"schema mismatch":   func(f *File) { f.SchemaVersion = 99 },
		"bad point id":      func(f *File) { f.Devices[0].Points[0].ID = "Bad ID" },
		"bit on non-bool":   func(f *File) { f.Devices[0].Points[0].Address.Bit = 3 },
		// TLS fail-closed：enabled 却无证书文件也无 auto（旧的静默降级，现配置期拒绝）。
		"tls enabled no cert": func(f *File) {
			f.Server.TLS = TLS{Enabled: true}
		},
		// TLS：certFile/keyFile 必须成对，半配（只给证书）在配置期拒绝。
		"tls cert without key": func(f *File) {
			f.Server.TLS = TLS{Enabled: true, CertFile: "cert.pem"}
		},
	}
	for name, mut := range cases {
		f := baseFile()
		mut(f)
		if err := f.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

// 空通道列表不能 panic（Normalize 曾经直接取 Channels[0]）。
// TestValidateServerTLS 守护 TLS 配置校验的三种合法形态都通过、两种非法形态被拒。
// 这是等保「传输保密性」与 NFR「配置期失败而非静默降级」的收口：过去 enabled
// 无证书会静默降级明文，现在配置期直接拒绝。
func TestValidateServerTLS(t *testing.T) {
	registerDrivers(t)

	ok := map[string]func(*File){
		"disabled (明文)": func(f *File) {
			f.Server.TLS = TLS{Enabled: false}
		},
		"cert+key 成对": func(f *File) {
			f.Server.TLS = TLS{Enabled: true, CertFile: "c.pem", KeyFile: "k.pem"}
		},
		"auto 自签": func(f *File) {
			f.Server.TLS = TLS{Enabled: true, Auto: true}
		},
	}
	for name, mut := range ok {
		f := baseFile()
		mut(f)
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid, got %v", name, err)
		}
	}

	bad := map[string]func(*File){
		"enabled 无证书无 auto（旧的静默降级）": func(f *File) {
			f.Server.TLS = TLS{Enabled: true}
		},
		"cert 有 key 无（半配）": func(f *File) {
			f.Server.TLS = TLS{Enabled: true, CertFile: "c.pem"}
		},
		"key 有 cert 无（半配）": func(f *File) {
			f.Server.TLS = TLS{Enabled: true, KeyFile: "k.pem"}
		},
	}
	for name, mut := range bad {
		f := baseFile()
		mut(f)
		if err := f.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

// 空通道列表不能 panic（Normalize 曾经直接取 Channels[0]）。
func TestValidateEmptyChannelsDoesNotPanic(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	f.Channels = nil
	f.Devices = nil
	if err := f.Validate(); err == nil {
		t.Fatal("expected error")
	}
}

// 多通道、各通道超时不同的配置里，挂在慢通道上的设备若被第一个（可能很快）
// 通道的超时提前掐断，会表现为「明明配了 30s 却 3s 就超时」，且没有任何报错。
func TestDeviceTimeoutInheritsOwnChannel(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	f.Channels = []model.Channel{
		{ID: "fast", Protocol: "virtual", Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:1"}, Timeout: model.Duration(3 * time.Second)},
		{ID: "slow", Protocol: "virtual", Transport: model.Transport{Kind: "tcp", Address: "127.0.0.1:2"}, Timeout: model.Duration(30 * time.Second)},
	}
	f.Devices = []model.Device{{
		ID: "d-on-slow", ChannelID: "slow", Interval: model.Duration(time.Second), Enabled: true,
		Points: []model.Point{{
			ID: "p1", DataType: model.TypeFloat32, Enabled: true,
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 2, Bit: -1},
		}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := f.Devices[0].TimeoutOrDefault(); got != 30*time.Second {
		t.Fatalf("device timeout = %v, want 30s (must inherit its own 'slow' channel, not Channels[0])", got)
	}

	// 显式配了 timeout 的设备不被 channel 默认值覆盖。
	f.Devices[0].Timeout = model.Duration(7 * time.Second)
	f.Normalize()
	if got := f.Devices[0].TimeoutOrDefault(); got != 7*time.Second {
		t.Fatalf("explicit device timeout = %v, want 7s", got)
	}
}

func openManager(t *testing.T) (*Manager, string) {
	t.Helper()
	registerDrivers(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	f := baseFile()
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, _ := f.Canonical()
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return m, dir
}

func TestApplyIsIdempotent(t *testing.T) {
	m, _ := openManager(t)
	before := m.Version()
	f := m.Current()
	res, err := m.Apply(f, "api")
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("same content must not be applied twice")
	}
	if m.Version() != before {
		t.Fatal("version must not change")
	}
}

func TestApplyCreatesVersionAndSnapshot(t *testing.T) {
	m, dir := openManager(t)
	before := m.Version()

	f := baseFile()
	f.Devices[0].Interval = model.Duration(2e9)
	f.Devices[0].Points = append(f.Devices[0].Points, model.Point{
		ID: "p2", DataType: model.TypeUint16, Enabled: true,
		Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 10, Length: 1, Bit: -1},
	})
	res, err := m.Apply(f, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Previous != before || res.Version == before {
		t.Fatalf("apply result = %+v", res)
	}
	if res.Changed.DevicesModified != 1 || res.Changed.PointsAdded != 1 {
		t.Fatalf("summary = %+v", res.Changed)
	}

	// 文件内容与版本一致，且旧版本快照仍可读。
	raw, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	if VersionOf(raw) != res.Version {
		t.Fatal("on-disk config does not match reported version")
	}
	old, err := m.VersionConfig(before)
	if err != nil {
		t.Fatalf("old version: %v", err)
	}
	if old.Devices[0].Interval.Duration().Seconds() != 1 {
		t.Fatalf("old snapshot mutated: %v", old.Devices[0].Interval)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots")); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackRestoresContent(t *testing.T) {
	m, _ := openManager(t)
	original := m.Version()
	origConfig, _ := m.Current().Canonical()

	f := baseFile()
	f.Site = "plant-02"
	res, err := m.Apply(f, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatal("expected apply")
	}

	rb, err := m.Rollback(original)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !rb.Applied {
		t.Fatalf("rollback result = %+v", rb)
	}
	now, _ := m.Current().Canonical()
	if string(now) != string(origConfig) {
		t.Fatal("rollback did not restore original content")
	}
	st := m.State()
	if st.Action != "rollback" || st.RollbackCount != 1 || st.Status != "rolled_back" {
		t.Fatalf("state = %+v", st)
	}
}

func TestRollbackUnknownVersionFails(t *testing.T) {
	m, _ := openManager(t)
	if _, err := m.Rollback("sha256:deadbeef"); err == nil {
		t.Fatal("expected error for unknown version")
	}
}

// 快照内容被篡改时必须拒绝加载，而不是悄悄用上被改过的配置。
func TestVersionConfigDetectsTampering(t *testing.T) {
	m, dir := openManager(t)
	ver := m.Version()
	path := filepath.Join(dir, "snapshots", "v-"+strings.TrimPrefix(ver, "sha256:")+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), "plant-01", "plant-99", 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.VersionConfig(ver); err == nil {
		t.Fatal("expected hash mismatch error")
	}
}

func TestDiffAndVersions(t *testing.T) {
	m, _ := openManager(t)
	v1 := m.Version()

	f := baseFile()
	f.Channels = append(f.Channels, model.Channel{
		ID: "ch2", Protocol: "virtual", Transport: model.Transport{Kind: "memory"},
	})
	f.Devices[0].Points[0].Deadband = 0.5
	res, err := m.Apply(f, "api")
	if err != nil {
		t.Fatal(err)
	}
	v2 := res.Version

	d, err := m.Diff(v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ChannelsAdded) != 1 || d.ChannelsAdded[0] != "ch2" {
		t.Fatalf("diff = %+v", d)
	}
	if d.PointsModified != 1 {
		t.Fatalf("pointsModified = %d", d.PointsModified)
	}
	// 空参数表示「当前」。
	d2, err := m.Diff("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.ChannelsAdded) != 0 || len(d2.DevicesModified) != 0 {
		t.Fatalf("self diff must be empty: %+v", d2)
	}
	if _, err := m.Diff("sha256:nope", ""); err == nil {
		t.Fatal("expected error for unknown from version")
	}

	vers := m.Versions()
	if len(vers) < 2 {
		t.Fatalf("versions = %d", len(vers))
	}
	found := false
	for _, v := range vers {
		if v.Version == v2 && v.Current {
			found = true
		}
	}
	if !found {
		t.Fatalf("current version not marked: %+v", vers)
	}
}

// 重启后当前版本必须来自磁盘，且历史快照可回滚。
func TestReopenPreservesState(t *testing.T) {
	m, dir := openManager(t)
	v1 := m.Version()

	f := baseFile()
	f.Site = "plant-02"
	res, err := m.Apply(f, "api")
	if err != nil {
		t.Fatal(err)
	}
	v2 := res.Version

	m2, err := Open(filepath.Join(dir, "config.json"), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if m2.Version() != v2 {
		t.Fatalf("version after reopen = %s, want %s", m2.Version(), v2)
	}
	if m2.Current().Site != "plant-02" {
		t.Fatalf("site = %s", m2.Current().Site)
	}
	if _, err := m2.Rollback(v1); err != nil {
		t.Fatalf("rollback after reopen: %v", err)
	}
	if m2.Current().Site != "plant-01" {
		t.Fatalf("site after rollback = %s", m2.Current().Site)
	}
}

// 非法配置绝不能落盘覆盖当前配置。
func TestInvalidApplyLeavesFileIntact(t *testing.T) {
	m, _ := openManager(t)
	before, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	bad := baseFile()
	bad.Site = "INVALID"
	if _, err := m.Apply(bad, "api"); err == nil {
		t.Fatal("expected validation error")
	}
	after, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("invalid apply must not modify the config file")
	}
	if m.Version() != VersionOf(before) {
		t.Fatal("version must not change")
	}
}

func TestStateFileWritten(t *testing.T) {
	m, dir := openManager(t)
	b, err := os.ReadFile(filepath.Join(dir, "config.state.json"))
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("state json: %v", err)
	}
	if st.CurrentVersion != m.Version() || st.Status != "applied" {
		t.Fatalf("state = %+v", st)
	}
}

// 损坏的状态文件不应阻止启动（配置本身仍然可信）。
func TestCorruptStateFileIsRecovered(t *testing.T) {
	registerDrivers(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	f := baseFile()
	f.Validate()
	raw, _ := f.Canonical()
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, dir); err != nil {
		t.Fatalf("open with corrupt state: %v", err)
	}
}

// Validate 是「只校验不应用」，绝不能让当前配置发生变化。
func TestManagerValidateDoesNotMutate(t *testing.T) {
	m, _ := openManager(t)
	before := m.Version()
	f := m.Current()
	bad := *f
	bad.Site = "BAD"
	if err := m.Validate(&bad); err == nil {
		t.Fatal("expected validation error")
	}
	if err := m.Validate(f); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if m.Version() != before {
		t.Fatal("Validate must not change the version")
	}
}

// dataDir 为空时快照写进配置文件所在目录，避免「配置在这、数据在那」的隐性耦合。
func TestOpenDefaultsDataDirToConfigDir(t *testing.T) {
	registerDrivers(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	f := baseFile()
	f.Validate()
	raw, _ := f.Canonical()
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots")); err != nil {
		t.Fatalf("snapshots must live next to the config: %v", err)
	}
	if m.Path() != path {
		t.Fatalf("path = %q", m.Path())
	}
}

// 快照保留策略：超过上限时删最旧的，但当前版本永远保留。
func TestPruneKeepsCurrentAndNewest(t *testing.T) {
	m, dir := openManager(t)
	cur := "v-" + strings.TrimPrefix(m.Version(), "sha256:") + ".json"

	// 造 6 个更旧的快照。
	for i := 0; i < 6; i++ {
		name := filepath.Join(dir, "snapshots", "v-old"+string(rune('a'+i))+".json")
		if err := os.WriteFile(name, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 显式设置不同 mtime，保证排序确定。
		mt := time.Now().Add(-time.Duration(i+1) * time.Hour)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.prune(3); err != nil {
		t.Fatalf("prune: %v", err)
	}
	ents, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range ents {
		names[e.Name()] = true
	}
	if !names[cur] {
		t.Fatal("current snapshot must never be pruned")
	}
	if len(names) != 3 {
		t.Fatalf("kept %d snapshots: %v", len(names), names)
	}
	// keep <= 0 使用默认值 50，不删除任何东西。
	if err := m.prune(0); err != nil {
		t.Fatal(err)
	}
	ents, _ = os.ReadDir(filepath.Join(dir, "snapshots"))
	if len(ents) != 3 {
		t.Fatalf("prune(0) removed files: %d", len(ents))
	}
}

// 原子写覆盖已有文件时必须读到完整的新内容（而不是前半截旧、后半截新）。
func TestAtomicWriteReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	if err := atomicWrite(p, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(p, []byte("second-longer-content")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "second-longer-content" {
		t.Fatalf("content = %q", b)
	}
	// 目标目录不存在时应自动创建（快照目录首次使用时就是这种状态）。
	nested := filepath.Join(dir, "missing", "y.json")
	if err := atomicWrite(nested, []byte("x")); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	if b, err := os.ReadFile(nested); err != nil || string(b) != "x" {
		t.Fatalf("nested content = %q %v", b, err)
	}
	// 不留临时文件。
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// 北向 MQTT 的静态错误必须在装配期暴露：配了 enabled 却没有 broker、
// 用了本实现不支持的 scheme、或者模板缺占位符，都不可能通过重试变好。
func TestValidateMQTTNorthbound(t *testing.T) {
	registerDrivers(t)
	cases := []struct {
		name string
		mod  func(*File)
		want string
	}{
		{"no broker", func(f *File) { f.Northbound.MQTT.Enabled = true }, "broker"},
		{"qos2", func(f *File) {
			q := 2
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "127.0.0.1:1883", QoS: &q}
		}, "qos 2"},
		{"tls", func(f *File) {
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "ssl://broker:8883"}
		}, "not supported"},
		{"topic", func(f *File) {
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", Topic: "static/topic"}
		}, "{device}"},
	}
	for _, c := range cases {
		f := baseFile()
		c.mod(f)
		err := f.Validate()
		if err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}
}

// 北向 OPC UA 只做 None/None + 匿名。任何「看起来更安全」的配置都必须
// 显式失败——静默降级成无加密是最危险的一种兼容。
func TestValidateOPCUANorthbound(t *testing.T) {
	registerDrivers(t)
	cases := []struct {
		name string
		mod  func(*File)
		want string
	}{
		{"port conflict", func(f *File) {
			f.Northbound.OPCUA = OPCUA{Enabled: true, Listen: f.Server.Listen, Policy: "None", AllowAnon: true}
		}, "conflicts"},
		{"anonymous required", func(f *File) {
			f.Northbound.OPCUA = OPCUA{Enabled: true, Listen: ":4840", Policy: "None"}
		}, "allowAnonymous"},
		{"policy", func(f *File) {
			f.Northbound.OPCUA = OPCUA{Enabled: true, Listen: ":4840", Policy: "Basic256Sha256", AllowAnon: true}
		}, "Basic256Sha256"},
	}
	for _, c := range cases {
		f := baseFile()
		c.mod(f)
		err := f.Validate()
		if err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}

	f := baseFile()
	f.Northbound.OPCUA = OPCUA{Enabled: true, AllowAnon: true}
	if err := f.Validate(); err != nil {
		t.Fatalf("defaults must make it valid: %v", err)
	}
	// 默认加密策略必须是本实现真正支持的那一个。
	if f.Northbound.OPCUA.Policy != "None" {
		t.Fatalf("default policy = %q", f.Northbound.OPCUA.Policy)
	}
	if f.Northbound.OPCUA.Listen != ":4840" {
		t.Fatalf("default listen = %q", f.Northbound.OPCUA.Listen)
	}
}

// QoS 用指针是为了区分「没写」与「写了 0」：两者的投递语义不同。
func TestMQTTQoSTriState(t *testing.T) {
	registerDrivers(t)

	f := baseFile()
	f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
	if err := f.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if q := f.Northbound.MQTT.EffectiveQoS(); q != 1 {
		t.Fatalf("default qos = %d, want 1", q)
	}

	zero := 0
	f = baseFile()
	f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", QoS: &zero}
	if err := f.Validate(); err != nil {
		t.Fatalf("explicit qos0 must be accepted: %v", err)
	}
	if q := f.Northbound.MQTT.EffectiveQoS(); q != 0 {
		t.Fatalf("explicit qos0 became %d", q)
	}
}

// 契约版本：0（未配置）按 1、1、2 合法，其余显式拒绝；2 与 sparkplug-b 互斥；
// TopicV2 作为模板必须自带占位符。这些都是「配了就永远不可能投递成功」或
// 「自相矛盾」的静态错误，必须在装配期暴露。
func TestValidateContractVersion(t *testing.T) {
	registerDrivers(t)
	cases := []struct {
		name string
		mod  func(*File)
		want string
	}{
		{"cv3 rejected", func(f *File) { f.Northbound.ContractVersion = 3 }, "contractVersion"},
		{"cv negative rejected", func(f *File) { f.Northbound.ContractVersion = -1 }, "contractVersion"},
		{"cv2 with sparkplug rejected", func(f *File) {
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB}
			f.Northbound.ContractVersion = 2
		}, "sparkplug"},
		{"topicV2 missing placeholder", func(f *File) {
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", TopicV2: "static/v2"}
			f.Northbound.ContractVersion = 2
		}, "topicV2"},
		{"topicV2 with sparkplug rejected", func(f *File) {
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB, TopicV2: "x/{device}/{point}"}
			f.Northbound.ContractVersion = 1
		}, "topicV2"},
	}
	for _, c := range cases {
		f := baseFile()
		c.mod(f)
		err := f.Validate()
		if err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}
}

// 0/1/2 都是合法契约版本：0（未配置）与 1 等价，2 是双发过渡。
func TestValidateContractVersionAccepts(t *testing.T) {
	registerDrivers(t)
	for _, cv := range []int{0, 1, 2} {
		f := baseFile()
		f.Northbound.ContractVersion = cv
		if cv == 2 {
			f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("cv=%d must be accepted: %v", cv, err)
		}
	}
}

// EffectiveContractVersion 把未配置（<=0）解释为 1，且读取时不写字段——
// 否则存量配置的规范化内容哈希会漂移。
func TestEffectiveContractVersion(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, 1}, {-5, 1}, {1, 1}, {2, 2},
	} {
		nb := Northbound{ContractVersion: tc.in}
		if got := nb.EffectiveContractVersion(); got != tc.want {
			t.Fatalf("EffectiveContractVersion(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Normalize 只在 cv==2 且 json 时派生 TopicV2 = Topic + "/v2"；cv 为 0/1
// 时绝不碰这个字段（存量配置哈希不漂移）。
func TestNormalizeDerivesTopicV2(t *testing.T) {
	registerDrivers(t)

	// cv==2：派生默认 v2 主题。
	f := baseFile()
	f.Northbound.ContractVersion = 2
	f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
	f.Validate()
	if got := f.Northbound.MQTT.TopicV2; got != "uic/{site}/{device}/{point}/value/v2" {
		t.Fatalf("cv=2 derived topicV2 = %q", got)
	}

	// cv==2 且用户显式填了 topicV2：保留，不覆盖。
	f = baseFile()
	f.Northbound.ContractVersion = 2
	f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", TopicV2: "custom/{device}/{point}"}
	f.Validate()
	if got := f.Northbound.MQTT.TopicV2; got != "custom/{device}/{point}" {
		t.Fatalf("explicit topicV2 was overwritten: %q", got)
	}

	// cv==0/1：绝不派生，保持空串（哈希不漂移）。
	for _, cv := range []int{0, 1} {
		f = baseFile()
		f.Northbound.ContractVersion = cv
		f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
		f.Validate()
		if got := f.Northbound.MQTT.TopicV2; got != "" {
			t.Fatalf("cv=%d must not derive topicV2, got %q", cv, got)
		}
	}
}

// 契约版本字段必须是 omitempty + 条件规范化：未配置（cv==0）时规范化结果里
// 既不该出现 contractVersion 也不该出现 topicV2——这正是「存量配置升级到本版本
// 后内容哈希不变」的保证。cv==2 则两者都该出现，且 topicV2 取派生默认值。
func TestCanonicalContractFieldsGating(t *testing.T) {
	registerDrivers(t)

	// cv 未配置：规范化后两个字段都不出现在 canonical JSON 里。
	base := baseFile()
	base.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
	base.Validate()
	cb, err := base.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cb), "contractVersion") {
		t.Fatalf("unset contractVersion must be omitted from canonical: %s", cb)
	}
	if strings.Contains(string(cb), "topicV2") {
		t.Fatalf("topicV2 must be omitted when cv!=2: %s", cb)
	}

	// cv==2：两个字段都出现，且 topicV2 为派生默认值。
	dual := baseFile()
	dual.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
	dual.Northbound.ContractVersion = 2
	dual.Validate()
	cd, err := dual.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cd), "\"contractVersion\": 2") {
		t.Fatalf("cv=2 must surface contractVersion: %s", cd)
	}
	if !strings.Contains(string(cd), "\"topicV2\": \"uic/{site}/{device}/{point}/value/v2\"") {
		t.Fatalf("cv=2 must surface derived topicV2: %s", cd)
	}
}

// 从站映射的校验必须落到点位元数据上：宽度来自类型，可写性来自点位，
// 这两件事分开定义迟早会出现「从站说可写、点位说只读」的矛盾配置。
func TestValidateSlaveMappings(t *testing.T) {
	registerDrivers(t)
	good := func() *File {
		f := baseFile()
		f.Slaves = []model.Slave{{
			ID: "scada", Listen: "127.0.0.1:1502",
			Units: []model.SlaveUnit{{
				UnitID: 1,
				Mappings: []model.SlaveMapping{
					{Point: "dev1/p1", Area: model.MappingHolding, Offset: 0},
				},
			}},
		}}
		return f
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("valid slave: %v", err)
	}

	cases := map[string]func(*File){
		"unknown point": func(f *File) {
			f.Slaves[0].Units[0].Mappings[0].Point = "dev1/ghost"
		},
		"bad area": func(f *File) {
			f.Slaves[0].Units[0].Mappings[0].Area = "registers"
		},
		"overlap": func(f *File) {
			// p1 是 float32，占 2 个寄存器；offset 1 与它重叠。
			f.Slaves[0].Units[0].Mappings = append(f.Slaves[0].Units[0].Mappings,
				model.SlaveMapping{Point: "dev1/p1", Area: model.MappingHolding, Offset: 1})
		},
		"write to read-only area": func(f *File) {
			f.Slaves[0].Units[0].Mappings[0].Area = model.MappingInput
			f.Slaves[0].Units[0].Mappings[0].Writable = true
		},
		"write to read-only point": func(f *File) {
			f.Slaves[0].Units[0].Mappings[0].Writable = true
		},
		"unit out of range": func(f *File) { f.Slaves[0].Units[0].UnitID = 248 },
		"duplicate unit": func(f *File) {
			f.Slaves[0].Units = append(f.Slaves[0].Units, f.Slaves[0].Units[0])
		},
		"duplicate slave": func(f *File) { f.Slaves = append(f.Slaves, f.Slaves[0]) },
		"port conflict":   func(f *File) { f.Slaves[0].Listen = ":8080" },
		"empty units":     func(f *File) { f.Slaves[0].Units = nil },
	}
	for name, mut := range cases {
		f := good()
		mut(f)
		if err := f.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

// 端口 0 是「随机分配」，永远不该被判成冲突：测试与容器部署都靠它。
func TestListenConflicts(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:0", "127.0.0.1:0", false},
		{":0", ":8080", false},
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"0.0.0.0:8080", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "127.0.0.1:8081", false},
		{"[::]:8080", "127.0.0.1:8080", true},
	}
	for _, c := range cases {
		if got := listenConflicts(c.a, c.b); got != c.want {
			t.Fatalf("listenConflicts(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// 计算点的配置校验：能编译、引用存在、类型自洽、没有环。
// 这四条都是静态错误，必须在配置期拒绝。
func TestValidateComputed(t *testing.T) {
	registerDrivers(t)

	// baseComputed 构造「一台虚拟源设备 + 一台计算设备」的最小配置。
	baseComputed := func() *File {
		f := baseFile()
		f.Channels = []model.Channel{
			{ID: "ch-src", Protocol: "virtual", Transport: model.Transport{Kind: "memory"}},
			{ID: "ch-calc", Protocol: "computed", Transport: model.Transport{Kind: "memory"}},
		}
		f.Devices = []model.Device{
			{
				ID: "src", ChannelID: "ch-src", Interval: model.Duration(1e9), Enabled: true,
				Points: []model.Point{
					{ID: "a", DataType: model.TypeFloat64, Enabled: true,
						Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 2, Bit: -1}},
					{ID: "flag", DataType: model.TypeBool, Enabled: true,
						Address: model.Address{Kind: model.AddressRegister, Area: "coil", Offset: 0, Length: 1, Bit: -1}},
				},
			},
			{
				ID: "kpi", ChannelID: "ch-calc", Interval: model.Duration(1e9), Enabled: true,
				Points: []model.Point{
					{ID: "sum", DataType: model.TypeFloat64, Enabled: true,
						Address: model.Address{Kind: model.AddressComputed, Expr: "src:a + src:a"}},
					{ID: "chained", DataType: model.TypeFloat64, Enabled: true,
						Address: model.Address{Kind: model.AddressComputed, Expr: "kpi:sum * 2"}},
					{ID: "high", DataType: model.TypeBool, Enabled: true,
						Address: model.Address{Kind: model.AddressComputed, Expr: "src:a > 100"}},
				},
			},
		}
		return f
	}

	t.Run("accepts valid", func(t *testing.T) {
		if err := baseComputed().Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	// 引用可以指向停用点位：那是运行态问题，不是配置错误。
	t.Run("reference to disabled point is allowed", func(t *testing.T) {
		f := baseComputed()
		f.Devices[0].Points[0].Enabled = false
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	cases := []struct {
		name string
		mod  func(*File)
		want string
	}{
		{"missing reference", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = "nobody:x + 1"
		}, "does not exist"},
		{"self cycle", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = "kpi:sum + 1"
		}, "cycle"},
		{"two point cycle", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = "kpi:chained + 1"
		}, "cycle"},
		{"bool point with number expression", func(f *File) {
			f.Devices[1].Points[2].Address.Expr = "src:a + 1"
		}, "expression is number"},
		{"number point with bool expression", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = "src:a > 1"
		}, "expression is bool"},
		{"writable computed", func(f *File) {
			f.Devices[1].Points[0].Writable = true
		}, "read-only"},
		{"physical field on computed", func(f *File) {
			f.Devices[1].Points[0].Address.Offset = 4
		}, "mixed fields"},
		{"missing expr", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = ""
		}, "required for computed"},
		{"syntax error", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = "src:a +"
		}, "unexpected"},
		{"dot instead of colon", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = "src.a + 1"
		}, "冒号"},
		{"string data type", func(f *File) {
			f.Devices[1].Points[0].DataType = model.TypeString
		}, "numeric or bool"},
		{"too long expression", func(f *File) {
			f.Devices[1].Points[0].Address.Expr = strings.Repeat("1", 1025)
		}, "1024"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := baseComputed()
			c.mod(f)
			err := f.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want mention of %q", err, c.want)
			}
		})
	}
}

// 跨设备的环同样要拒绝：两边各拿对方上一轮的值，表现为「值永远慢一拍」，
// 而每台设备看起来都完全正常——最不该留给现场去发现的一类配置。
func TestValidateComputedCrossDeviceCycle(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	f.Channels = []model.Channel{
		{ID: "ch-calc", Protocol: "computed", Transport: model.Transport{Kind: "memory"}},
	}
	f.Devices = []model.Device{
		{ID: "d1", ChannelID: "ch-calc", Interval: model.Duration(1e9), Enabled: true,
			Points: []model.Point{{ID: "p", DataType: model.TypeFloat64, Enabled: true,
				Address: model.Address{Kind: model.AddressComputed, Expr: "d2:p + 1"}}}},
		{ID: "d2", ChannelID: "ch-calc", Interval: model.Duration(1e9), Enabled: true,
			Points: []model.Point{{ID: "p", DataType: model.TypeFloat64, Enabled: true,
				Address: model.Address{Kind: model.AddressComputed, Expr: "d1:p + 1"}}}},
	}
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cross-device cycle must be rejected, got %v", err)
	}
}

func TestValidateAlarms(t *testing.T) {
	registerDrivers(t)

	withAlarm := func(r alarm.Rule) *File {
		f := baseFile()
		f.Alarms = []alarm.Rule{r}
		return f
	}
	high := func(v float64) *float64 { return &v }

	t.Run("accepts threshold rule", func(t *testing.T) {
		f := withAlarm(alarm.Rule{ID: "over-heat", Point: "dev1:p1", High: high(80)})
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		// Normalize 补默认级别。
		if f.Alarms[0].Severity != alarm.SeverityWarning {
			t.Fatalf("severity = %q", f.Alarms[0].Severity)
		}
	})

	t.Run("accepts expression rule", func(t *testing.T) {
		f := withAlarm(alarm.Rule{ID: "ratio", Expr: "dev1:p1 / 2 > 10", Severity: alarm.SeverityInfo})
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	t.Run("accepts rule with delays", func(t *testing.T) {
		f := withAlarm(alarm.Rule{
			ID: "delayed", Point: "dev1:p1", High: high(10), Hysteresis: 1,
			OnDelay: model.Duration(3 * time.Second), OffDelay: model.Duration(5 * time.Second),
		})
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	// 停用的规则也要校验：它随时可能被重新启用，而没人会重新检查一遍。
	t.Run("disabled rule is still validated", func(t *testing.T) {
		f := withAlarm(alarm.Rule{ID: "off", Point: "nobody:x", High: high(1), Disabled: true})
		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("err = %v", err)
		}
	})

	cases := []struct {
		name  string
		rules []alarm.Rule
		want  string
	}{
		{"missing point", []alarm.Rule{{ID: "a", Point: "dev1:missing", High: high(1)}}, "does not exist"},
		{"missing device", []alarm.Rule{{ID: "a", Point: "ghost:p1", High: high(1)}}, "does not exist"},
		{"malformed point", []alarm.Rule{{ID: "a", Point: "p1", High: high(1)}}, "device:point"},
		{"threshold on text point", []alarm.Rule{{ID: "a", Point: "dev1:label", High: high(1)}}, "cannot be compared"},
		{"threshold on bool point", []alarm.Rule{{ID: "a", Point: "dev1:flag", High: high(1)}}, "cannot be compared"},
		{"expr references bool point", []alarm.Rule{{ID: "a", Expr: "dev1:flag > 0"}}, "cannot be compared"},
		{"expr references missing point", []alarm.Rule{{ID: "a", Expr: "dev1:p1 > nobody:x"}}, "does not exist"},
		{"duplicate id", []alarm.Rule{
			{ID: "dup", Point: "dev1:p1", High: high(1)},
			{ID: "dup", Point: "dev1:p1", Low: high(1)},
		}, "duplicate id"},
		{"rule level error", []alarm.Rule{{ID: "a"}}, "one of"},
		{"bad severity", []alarm.Rule{{ID: "a", Point: "dev1:p1", High: high(1), Severity: "fatal"}}, "severity"},
		{"high equals low", []alarm.Rule{{ID: "a", Point: "dev1:p1", High: high(5), Low: high(5)}}, "greater than low"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := baseFile()
			// 文本 / 布尔点位是「不可比较」的对照组。
			f.Devices[0].Points = append(f.Devices[0].Points,
				model.Point{ID: "label", DataType: model.TypeString, Enabled: true,
					Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 4, Length: 1, Bit: -1}},
				model.Point{ID: "flag", DataType: model.TypeBool, Enabled: true,
					Address: model.Address{Kind: model.AddressRegister, Area: "coil", Offset: 0, Length: 1, Bit: -1}},
			)
			f.Alarms = c.rules
			err := f.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want mention of %q", err, c.want)
			}
		})
	}

	// 报警规则参与版本哈希：只改一条规则的阈值也必须产生新版本。
	t.Run("alarms affect config version", func(t *testing.T) {
		f1 := withAlarm(alarm.Rule{ID: "a", Point: "dev1:p1", High: high(1)})
		f2 := withAlarm(alarm.Rule{ID: "a", Point: "dev1:p1", High: high(2)})
		if err := f1.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := f2.Validate(); err != nil {
			t.Fatal(err)
		}
		c1, _ := f1.Canonical()
		c2, _ := f2.Canonical()
		if VersionOf(c1) == VersionOf(c2) {
			t.Fatal("changing a threshold must change the config version")
		}
	})
}

func TestValidateHistory(t *testing.T) {
	registerDrivers(t)

	t.Run("disabled by default", func(t *testing.T) {
		f := baseFile()
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		if f.Storage.History.Enabled {
			t.Fatal("history must be opt-in")
		}
		if f.Storage.History.Dir != "" || f.Storage.History.MaxBytes != 0 {
			t.Fatalf("disabled history must not be normalized: %+v", f.Storage.History)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		f := baseFile()
		f.Storage.Dir = "/var/lib/uic"
		f.Storage.History.Enabled = true
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		h := f.Storage.History
		// 目录默认挂在 storage.dir 下：历史是这台网关的本地数据，
		// 不应该默认写到进程的当前工作目录。
		if h.Dir != "/var/lib/uic/history" {
			t.Fatalf("dir = %q", h.Dir)
		}
		if h.MaxBytes != 256<<20 || h.SegmentBytes != 16<<20 {
			t.Fatalf("defaults = %+v", h)
		}
		if h.MaxAge.Duration() != 7*24*time.Hour {
			t.Fatalf("maxAge = %s", h.MaxAge)
		}
		if h.MinInterval.Duration() != time.Second {
			t.Fatalf("minInterval = %s", h.MinInterval)
		}
		if h.Heartbeat == nil || h.Heartbeat.Duration() != 5*time.Minute {
			t.Fatalf("heartbeat = %v (a stable value must still get a point every 5m)", h.Heartbeat)
		}
	})

	// 负值走「回到默认」而不是报错：与 outboxTtl 等字段保持一致，
	// 配置文件里一个漏写的减号不应该让整台网关起不来。
	t.Run("negatives fall back to defaults", func(t *testing.T) {
		f := baseFile()
		f.Storage.History.Enabled = true
		f.Storage.History.MaxBytes = -1
		f.Storage.History.MaxAge = -1
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		if f.Storage.History.MaxBytes != 256<<20 || f.Storage.History.MaxAge.Duration() != 7*24*time.Hour {
			t.Fatalf("negatives were not normalized: %+v", f.Storage.History)
		}
		if f.Storage.History.SegmentBytes > f.Storage.History.MaxBytes {
			t.Fatalf("segment must be clamped to maxBytes: %+v", f.Storage.History)
		}
	})

	t.Run("segment clamped to maxBytes", func(t *testing.T) {
		f := baseFile()
		f.Storage.History.Enabled = true
		f.Storage.History.MaxBytes = 1 << 20
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		// 只调小总量不应该要求同时改段大小。
		if f.Storage.History.SegmentBytes != 1<<20 {
			t.Fatalf("segmentBytes = %d, want clamp to maxBytes", f.Storage.History.SegmentBytes)
		}
	})

	bad := []struct {
		name  string
		apply func(*History)
		want  string
	}{
		{"empty dir", func(h *History) { h.Dir = " " }, "must not be empty"},
		{"tiny maxBytes", func(h *History) { h.MaxBytes = 512 }, "too small"},
		{"tiny segment", func(h *History) { h.MaxBytes = 4096; h.SegmentBytes = 100 }, "too small"},
		{"maxAge over a year", func(h *History) { h.MaxAge = model.Duration(400 * 24 * time.Hour) }, "365-day"},
		{"minInterval too long", func(h *History) { h.MinInterval = model.Duration(2 * time.Hour) }, "minInterval"},
		{"negative heartbeat", func(h *History) { d := model.Duration(-1); h.Heartbeat = &d }, "heartbeat"},
		{"heartbeat too long", func(h *History) { d := model.Duration(48 * time.Hour); h.Heartbeat = &d }, "24h"},
		{"non-finite deadband", func(h *History) { h.Deadband = math.Inf(1) }, "deadband"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			f := baseFile()
			f.Storage.History.Enabled = true
			f.Normalize() // 先拿到默认值，再破坏其中一项
			c.apply(&f.Storage.History)
			err := f.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want mention of %q", err, c.want)
			}
		})
	}

	t.Run("history affects config version", func(t *testing.T) {
		mk := func(maxBytes int64) *File {
			f := baseFile()
			f.Storage.History.Enabled = true
			f.Storage.History.MaxBytes = maxBytes
			if err := f.Validate(); err != nil {
				t.Fatal(err)
			}
			return f
		}
		_ = mk
		c1, _ := mk(1 << 20).Canonical()
		c2, _ := mk(2 << 20).Canonical()
		if VersionOf(c1) == VersionOf(c2) {
			t.Fatal("changing the retention policy must change the config version")
		}
	})
}

// 串口是独占资源：同一物理端口不能既做主机又做从站，也不能被两个从站
// 同时打开。半双工总线上两个角色会互相抢答，症状是「偶发 CRC 错」。
//
// 比较的是字面路径；符号链接/`by-id` 别名指向同一设备时这里看不出来，
// 这一点写在驱动规范里，而不是假装能自动识别。
func TestValidateSlaveSerialPorts(t *testing.T) {
	registerDrivers(t)

	serialSlave := func(id, port string) model.Slave {
		return model.Slave{
			ID:     id,
			Serial: &model.SlaveSerial{Port: port, Framing: "rtu", Baud: 19200},
			Units: []model.SlaveUnit{{
				UnitID: 1,
				Mappings: []model.SlaveMapping{
					{Point: "dev1/p1", Area: model.MappingHolding, Offset: 0},
				},
			}},
		}
	}
	withMaster := func(f *File, port string) {
		f.Channels = []model.Channel{{
			ID: "ch-rtu", Protocol: "modbus-rtu",
			Transport: model.Transport{Kind: "serial", Port: port, Baud: 19200},
		}}
		f.Devices[0].ChannelID = "ch-rtu"
	}

	// 从站独占一个没被主站使用的端口：合法。
	f := baseFile()
	withMaster(f, "/dev/ttyUSB0")
	f.Slaves = []model.Slave{serialSlave("scada", "/dev/ttyUSB1")}
	if err := f.Validate(); err != nil {
		t.Fatalf("distinct ports must be accepted: %v", err)
	}

	// 同一个端口既做主站又做从站：拒绝。
	f = baseFile()
	withMaster(f, "/dev/ttyUSB0")
	f.Slaves = []model.Slave{serialSlave("scada", "/dev/ttyUSB0")}
	if err := f.Validate(); err == nil {
		t.Fatal("slave must not reuse a channel's serial port")
	} else if !strings.Contains(err.Error(), "channel ch-rtu") {
		t.Fatalf("error should name the conflicting owner: %v", err)
	}

	// 两个从站抢同一根串口：拒绝。
	f = baseFile()
	withMaster(f, "/dev/ttyUSB0")
	f.Slaves = []model.Slave{serialSlave("scada", "/dev/ttyUSB1"), serialSlave("scada2", "/dev/ttyUSB1")}
	if err := f.Validate(); err == nil {
		t.Fatal("two slaves must not share one serial port")
	}

	// 字面路径不同即视为不同端口（有意保守：宁可漏报，也不要误伤
	// 两条真正独立的链路）。
	f = baseFile()
	withMaster(f, "/dev/ttyUSB0")
	f.Slaves = []model.Slave{serialSlave("scada", "/dev/serial/by-id/usb-1")}
	if err := f.Validate(); err != nil {
		t.Fatalf("different literal paths must be accepted: %v", err)
	}
}

// 两个 TCP 从站监听同一地址同样是配置错误：真正的失败会推迟到第二个
// bind，那时第一个从站已经起来了，启动失败的日志更难读。
func TestValidateSlaveListenConflicts(t *testing.T) {
	registerDrivers(t)
	slave := func(id, listen string) model.Slave {
		return model.Slave{
			ID: id, Listen: listen,
			Units: []model.SlaveUnit{{
				UnitID: 1,
				Mappings: []model.SlaveMapping{
					{Point: "dev1/p1", Area: model.MappingHolding, Offset: 0},
				},
			}},
		}
	}

	f := baseFile()
	f.Slaves = []model.Slave{slave("a", "127.0.0.1:1502"), slave("b", "127.0.0.1:1502")}
	if err := f.Validate(); err == nil {
		t.Fatal("duplicate listen addresses must be rejected")
	}

	// 端口 0 是随机分配，两个从站都用 :0 不算冲突。
	f = baseFile()
	f.Slaves = []model.Slave{slave("a", "127.0.0.1:0"), slave("b", "127.0.0.1:0")}
	if err := f.Validate(); err != nil {
		t.Fatalf("port 0 must not conflict: %v", err)
	}
}

// 窗口聚合在配置期要挡住四种「不报错但永远算不出值」的写法。
func TestValidateHistoryWindows(t *testing.T) {
	registerDrivers(t)

	mk := func(expr string) *File {
		f := baseFile()
		// baseFile 的 p1 是 float32 寄存器；补一个文本点作为「不可聚合」对照。
		f.Devices[0].Points = append(f.Devices[0].Points, model.Point{
			ID: "label", DataType: model.TypeString, Enabled: true,
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 4, Length: 1, Bit: -1},
		})
		f.Channels = append(f.Channels, model.Channel{
			ID: "ch-calc", Protocol: "computed", Transport: model.Transport{Kind: "memory"},
		})
		f.Devices = append(f.Devices, model.Device{
			ID: "kpi", ChannelID: "ch-calc", Interval: model.Duration(time.Second), Enabled: true,
			Points: []model.Point{{
				ID: "w", DataType: model.TypeFloat64, Enabled: true,
				Address: model.Address{Kind: model.AddressComputed, Expr: expr},
			}},
		})
		return f
	}

	t.Run("accepts when history is enabled", func(t *testing.T) {
		f := mk("avg(dev1:p1, 5m) * 2")
		f.Storage.History.Enabled = true
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	cases := []struct {
		name  string
		expr  string
		tweak func(*File)
		want  string
	}{
		{
			"history disabled", "avg(dev1:p1, 5m)", nil,
			"storage.history.enabled is false",
		},
		{
			"missing reference", "avg(dev1:nope, 5m)",
			func(f *File) { f.Storage.History.Enabled = true },
			"does not exist",
		},
		{
			"text point has no numeric history", "avg(dev1:label, 5m)",
			func(f *File) { f.Storage.History.Enabled = true },
			"no numeric history",
		},
		{
			"window longer than retention", "avg(dev1:p1, 2h)",
			func(f *File) {
				f.Storage.History.Enabled = true
				f.Storage.History.MaxAge = model.Duration(time.Hour)
			},
			"exceeds storage.history.maxAge",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := mk(c.expr)
			if c.tweak != nil {
				c.tweak(f)
			}
			err := f.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}

	t.Run("alarm expression requires history", func(t *testing.T) {
		f := baseFile()
		f.Alarms = []alarm.Rule{{
			ID: "avg-hot", Severity: alarm.SeverityWarning,
			Expr: "avg(dev1:p1, 5m) > 10",
		}}
		err := f.Validate()
		if err == nil || !strings.Contains(err.Error(), "storage.history.enabled is false") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("alarm expression accepts when history is enabled", func(t *testing.T) {
		f := baseFile()
		f.Storage.History.Enabled = true
		f.Alarms = []alarm.Rule{{
			ID: "avg-hot", Severity: alarm.SeverityWarning,
			Expr: "avg(dev1:p1, 5m) > 10",
		}}
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})
}

// Sparkplug B 与 json 共用 MQTT 连接，但主题与载荷契约完全不同。这一组
// 用例钉住「不能靠改名蒙混」的几处：自定义主题、retained、组/节点名里
// 的分隔符都必须在启动前失败。
func TestValidateSparkplugNorthbound(t *testing.T) {
	registerDrivers(t)
	cases := []struct {
		name string
		mod  func(*File)
		want string
	}{
		{"custom topic", func(f *File) {
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB,
				Topic: "uic/{device}/{point}", GroupID: "g", NodeID: "n",
			}
		}, "topic must be empty"},
		{"retained", func(f *File) {
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB,
				Retain: true, GroupID: "g", NodeID: "n",
			}
		}, "retain must be false"},
		{"explicit qos", func(f *File) {
			// QoS 由消息类型定死，不是一个可调项。静默忽略等于允许配出
			// 一个「本地正常、严格 Host 丢弃」的部署。
			q := 1
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB,
				QoS: &q, GroupID: "g", NodeID: "n",
			}
		}, "qos must be omitted"},
		{"bad format", func(f *File) {
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: "sparkplug-c",
			}
		}, "only"},
		{"group separator", func(f *File) {
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB,
				GroupID: "a/b", NodeID: "n",
			}
		}, "must not contain"},
		{"group empty", func(f *File) {
			// Normalize 会给默认值，所以绕过它直接调用 validateNorthbound。
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB,
				GroupID: "", NodeID: "n",
			}
		}, "groupId"},
		{"node wildcard", func(f *File) {
			f.Northbound.MQTT = MQTT{
				Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB,
				GroupID: "g", NodeID: "n+1",
			}
		}, "must not contain"},
	}
	for _, c := range cases {
		f := baseFile()
		c.mod(f)
		var err error
		if c.name == "group empty" {
			err = f.validateNorthbound()
		} else {
			err = f.Validate()
		}
		if err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}
}

func TestSparkplugDefaults(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	f.Site = "plant-a"
	f.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883", Format: MQTTFormatSparkplugB}
	if err := f.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if f.Northbound.MQTT.Topic != "" {
		t.Fatalf("sparkplug must not gain a json topic: %q", f.Northbound.MQTT.Topic)
	}
	if f.Northbound.MQTT.GroupID != "plant-a" || f.Northbound.MQTT.NodeID != "uic-plant-a" {
		t.Fatalf("defaults = %+v", f.Northbound.MQTT)
	}

	// json 仍然是默认格式，并且继续拿默认主题。
	g := baseFile()
	g.Northbound.MQTT = MQTT{Enabled: true, Broker: "broker:1883"}
	if err := g.Validate(); err != nil {
		t.Fatalf("json validate: %v", err)
	}
	if g.Northbound.MQTT.Format != MQTTFormatJSON || g.Northbound.MQTT.Topic == "" {
		t.Fatalf("json defaults = %+v", g.Northbound.MQTT)
	}
}

// TestDescribe 守护站点侧只读描述：影响面计数（设备/点位/通道）、站点、以及
// 配置版本哈希必须等于 VersionOf(Canonical())——后者是中心做「配置下发」
// 版本协商的比对基准，若它漂移，中心会对所有站点误判「需要下发」。
func TestDescribe(t *testing.T) {
	m, _ := openManager(t)

	d := m.Describe()
	if d.Site != "plant-01" {
		t.Fatalf("site = %q, want plant-01", d.Site)
	}
	if d.Devices != 1 || d.Points != 1 || d.Channels != 1 {
		t.Fatalf("counts = dev %d/pt %d/ch %d, want 1/1/1", d.Devices, d.Points, d.Channels)
	}

	// 配置版本哈希必须等于当前配置 canonical 的 VersionOf。
	cur := m.Current()
	canonical, err := cur.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if want := VersionOf(canonical); d.ConfigVersion != want {
		t.Fatalf("configVersion = %q, want %q", d.ConfigVersion, want)
	}

	// 点位数跨设备累加：baseFile 加一台 3 点的设备后应为 4。
	cur.Devices = append(cur.Devices, model.Device{
		ID: "dev2", ChannelID: "ch1", Interval: model.Duration(1e9), Enabled: true,
		Points: []model.Point{
			{ID: "q1", DataType: model.TypeFloat32, Enabled: true,
				Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 10, Length: 2, Bit: -1}},
			{ID: "q2", DataType: model.TypeFloat32, Enabled: true,
				Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 20, Length: 2, Bit: -1}},
			{ID: "q3", DataType: model.TypeFloat32, Enabled: true,
				Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 30, Length: 2, Bit: -1}},
		},
	})
	d2 := m.Describe()
	if d2.Devices != 2 || d2.Points != 4 {
		t.Fatalf("after add: counts = dev %d/pt %d, want 2/4", d2.Devices, d2.Points)
	}
}
