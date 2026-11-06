package runtime

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uic/internal/config"
	"uic/internal/driver"
	opcuadriver "uic/internal/driver/opcua"
	"uic/internal/model"
	"uic/internal/obs"
)

// northboundConfig 是一份最小可跑配置：虚拟设备 + 北向 OPC UA。
//
// 用虚拟驱动而不是 Modbus 回环：这个用例要验证的是「配置 → 地址空间 →
// 真实客户端读写」这条链，南向协议的细节与它无关。多引一个 sim 只会让
// 测试失败时多一种可能。
func northboundConfig(dir string, extraPoint bool) map[string]any {
	points := []any{
		map[string]any{
			"id": "temperature", "name": "温度", "dataType": "float32", "enabled": true,
			"address": map[string]any{"kind": "register", "area": "holding", "length": 2, "bit": -1},
		},
		map[string]any{
			"id": "setpoint", "name": "设定值", "dataType": "float32", "writable": true, "enabled": true,
			"address": map[string]any{"kind": "register", "area": "holding", "offset": 20, "length": 2, "bit": -1},
		},
	}
	if extraPoint {
		points = append(points, map[string]any{
			"id": "pressure", "name": "压力", "dataType": "float64", "enabled": true,
			"address": map[string]any{"kind": "register", "area": "holding", "offset": 2, "length": 4, "bit": -1},
		})
	}
	return map[string]any{
		"schemaVersion": 1,
		"site":          "test-site",
		"server":        map[string]any{"listen": "127.0.0.1:0"},
		"storage":       map[string]any{"dir": dir, "journal": false},
		"logging":       map[string]any{"level": "info"},
		"northbound": map[string]any{"opcua": map[string]any{
			"enabled": true, "listen": "127.0.0.1:0", "policy": "None", "allowAnonymous": true,
		}},
		"channels": []any{map[string]any{
			"id": "ch-virtual", "protocol": "virtual",
			"transport":   map[string]any{"kind": "memory"},
			"concurrency": 4, "timeout": "1s",
		}},
		"devices": []any{map[string]any{
			"id": "line-a", "name": "产线 A", "channel": "ch-virtual",
			"interval": "200ms", "timeout": "1s", "enabled": true,
			"labels": map[string]any{"mode": "sine"},
			"points": points,
		}},
	}
}

func writeConfig(t *testing.T, dir string, cfg map[string]any) string {
	t.Helper()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func northClient(t *testing.T, addr string) driver.Conn {
	t.Helper()
	if addr == "" {
		t.Fatal("northbound address is empty")
	}
	ch := model.Channel{
		ID: "ch-nb", Protocol: "opcua",
		Transport: model.Transport{Kind: "tcp", Address: addr},
		Timeout:   model.Duration(2 * time.Second),
	}
	c, err := opcuadriver.New().Open(context.Background(), driver.OpenConfig{
		Channel: ch,
		Device: model.Device{
			ID: "client", ChannelID: ch.ID,
			Interval: model.Duration(time.Second), Enabled: true,
		},
	})
	if err != nil {
		t.Fatalf("open northbound client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readNorth(t *testing.T, c driver.Conn, path string, dt model.DataType) driver.RawSample {
	t.Helper()
	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{{
			ID: "p", DataType: dt,
			Address: model.Address{Kind: model.AddressSymbol, Path: path, Index: -1},
		}}}})
	if len(samples) == 0 {
		t.Fatalf("read %s: no samples (err=%v)", path, err)
	}
	// 单点坏质量会让整次 Read 返回错误（驱动约定：全部失败才算失败）。
	// 调用方按 sample.Quality 判断，这里不把「还没采到」当成测试失败。
	return samples[0]
}

// 端到端：配置 → 运行时 → 北向 OPC UA → 真实客户端。
func TestNorthboundOPCUAEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("network e2e")
	}
	dir := t.TempDir()
	path := writeConfig(t, dir, northboundConfig(dir, false))
	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = rt.Stop(5 * time.Second)
	})

	rt.nbMu.Lock()
	addr := rt.nbOPCUA.srv.Addr()
	rt.nbMu.Unlock()
	if addr == "" {
		t.Fatal("northbound server not started")
	}
	if v := rt.metrics.Value("uic_opcua_server_up", obs.Labels{"site": "test-site"}); v != 1 {
		t.Fatalf("uic_opcua_server_up = %v", v)
	}

	c := northClient(t, addr)
	tempPath := "ns=2;s=test-site/line-a/temperature"

	// 采集循环一起来，读到的就不是占位样本。
	deadline := time.Now().Add(5 * time.Second)
	var smp driver.RawSample
	for {
		smp = readNorth(t, c, tempPath, model.TypeFloat32)
		if smp.Quality == model.QualityGood && smp.Value.Type == model.TypeFloat32 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temperature never became good: %+v", smp)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 写入走真实南向设备：结果 Good 表示设备接受了写入。
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "setpoint", DataType: model.TypeFloat32,
		Address: model.Address{Kind: model.AddressSymbol, Path: "ns=2;s=test-site/line-a/setpoint", Index: -1},
		Value:   model.NumValue(model.TypeFloat32, 42.5),
	}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("write result = %+v", res)
	}
	back := readNorth(t, c, "ns=2;s=test-site/line-a/setpoint", model.TypeFloat32)
	if back.Value.Num != 42.5 {
		t.Fatalf("read back = %+v", back)
	}

	// 热更新：新增点位必须出现在地址空间里。旧客户端会随服务端重建断开，
	// 所以这里重新建一个连接——这也正是 SCADA 在现场会经历的行为。
	raw, err := json.Marshal(northboundConfig(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ApplyConfig(raw, "test"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rt.nbMu.Lock()
	addr2 := rt.nbOPCUA.srv.Addr()
	rt.nbMu.Unlock()
	c2 := northClient(t, addr2)
	deadline = time.Now().Add(5 * time.Second)
	for {
		smp = readNorth(t, c2, "ns=2;s=test-site/line-a/pressure", model.TypeFloat64)
		if smp.Quality == model.QualityGood {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new point never appeared: %+v", smp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 指纹只应在「地址空间真的会变」时变化。改显示名不该触发重建——
// 每次重建都意味着 SCADA 掉线重连一次。
func TestOPCUAFingerprint(t *testing.T) {
	dir := t.TempDir()
	a := northboundConfig(dir, false)
	f1 := configOf(t, a)
	f2 := configOf(t, a)
	if opcuaFingerprint(f1) != opcuaFingerprint(f2) {
		t.Fatal("fingerprint must be stable")
	}

	// 改点位名（不影响地址空间）不应改变指纹。
	b := northboundConfig(dir, false)
	b["devices"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)["name"] = "改了名字"
	if opcuaFingerprint(f1) != opcuaFingerprint(configOf(t, b)) {
		t.Fatal("display name must not change the fingerprint")
	}

	// 加点位必须改变指纹。
	if opcuaFingerprint(f1) == opcuaFingerprint(configOf(t, northboundConfig(dir, true))) {
		t.Fatal("new point must change the fingerprint")
	}

	// 换监听地址必须改变指纹。
	c := northboundConfig(dir, false)
	c["northbound"].(map[string]any)["opcua"].(map[string]any)["listen"] = "127.0.0.1:14841"
	if opcuaFingerprint(f1) == opcuaFingerprint(configOf(t, c)) {
		t.Fatal("listen must change the fingerprint")
	}
}

func configOf(t *testing.T, m map[string]any) *config.File {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var f config.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	f.Normalize()
	return &f
}

// 端口被占用必须让启动失败：网关起来了但 SCADA 连不上，是最难排查的
// 一类部署故障。这里占用一个真实端口，断言 New 直接报错。
func TestNorthboundOPCUAPortBusyFailsStartup(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	dir := t.TempDir()
	cfg := northboundConfig(dir, false)
	cfg["northbound"].(map[string]any)["opcua"].(map[string]any)["listen"] = busy.Addr().String()
	path := writeConfig(t, dir, cfg)

	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err == nil {
		_ = rt.Stop(time.Second)
		t.Fatal("startup must fail when the OPC UA port is taken")
	}
	if !strings.Contains(err.Error(), "opcua") {
		t.Fatalf("error should mention opcua: %v", err)
	}
}

// 热更新抢不到新端口时，必须回退到上一份配置继续服务，而不是让北向
// 直接消失。配置已经落盘了，用户看到的应该是「新配置没生效」的日志，
// 而不是 SCADA 全红。
func TestNorthboundOPCUARestartFailureRestoresPrevious(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	dir := t.TempDir()
	path := writeConfig(t, dir, northboundConfig(dir, false))
	rt, err := New(Options{ConfigPath: path, DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = rt.Stop(5 * time.Second)
	})

	cfg := northboundConfig(dir, false)
	cfg["northbound"].(map[string]any)["opcua"].(map[string]any)["listen"] = busy.Addr().String()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ApplyConfig(raw, "test"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rt.nbMu.Lock()
	nb := rt.nbOPCUA
	rt.nbMu.Unlock()
	if nb == nil {
		t.Fatal("northbound server disappeared after a failed restart")
	}
	c := northClient(t, nb.srv.Addr())
	if smp := readNorth(t, c, "ns=2;s=test-site/line-a/temperature", model.TypeFloat32); smp.Quality == model.QualityAddressError {
		t.Fatalf("restored address space is broken: %+v", smp)
	}
}
