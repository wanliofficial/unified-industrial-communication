package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/driver"
	"uic/internal/driver/modbus"
	"uic/internal/driver/virtual"
	"uic/internal/model"
)

func registerDrivers(t *testing.T) {
	t.Helper()
	if _, ok := driver.Get("virtual"); ok {
		return
	}
	for _, d := range []driver.Driver{virtual.New(), modbus.New()} {
		if err := driver.Register(d); err != nil {
			t.Fatalf("register: %v", err)
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
func TestValidateEmptyChannelsDoesNotPanic(t *testing.T) {
	registerDrivers(t)
	f := baseFile()
	f.Channels = nil
	f.Devices = nil
	if err := f.Validate(); err == nil {
		t.Fatal("expected error")
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
