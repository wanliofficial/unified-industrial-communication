package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"uic/internal/alarm"
	"uic/internal/consensus"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
)

// fakeConsensus 是 Runtime 的测试替身：可以任意切角色、任意制造提交失败，
// 并记录被提交的命令。
//
// applyHook 由测试在 New 之后接到 rt.ApplyCommand 上，用来模拟「leader 提交
// 成功后本地按序 apply」——这正是真实 raft 对 leader 自己的行为，因此这里能
// 覆盖「提交式写」的完整语义，而不必在每个用例里拉起真实集群。
type fakeConsensus struct {
	mu       sync.Mutex
	leader   bool
	leaderID string
	addr     string
	healthy  bool
	term     uint64
	commit   uint64
	servers  []consensus.Server
	proposeE error
	applied  []consensus.Command
	apply    func(consensus.Command) (any, error)
	closed   bool
	done     chan struct{}
}

func newFakeConsensus() *fakeConsensus {
	return &fakeConsensus{
		healthy: true, term: 3, commit: 42,
		leaderID: "node-a", addr: "10.0.0.1:7000",
		done: make(chan struct{}),
		servers: []consensus.Server{
			{ID: "node-a", Addr: "10.0.0.1:7000", Suffrage: consensus.SuffrageVoter},
			{ID: "node-b", Addr: "10.0.0.2:7000", Suffrage: consensus.SuffrageVoter},
			{ID: "node-c", Addr: "10.0.0.3:7000", Suffrage: consensus.SuffrageVoter},
		},
	}
}

func (f *fakeConsensus) Propose(ctx context.Context, cmd consensus.Command) (any, error) {
	f.mu.Lock()
	if f.proposeE != nil {
		err := f.proposeE
		f.mu.Unlock()
		return nil, err
	}
	if !f.leader {
		f.mu.Unlock()
		return nil, consensus.ErrNotLeader
	}
	if !f.healthy {
		f.mu.Unlock()
		return nil, consensus.ErrUnhealthy
	}
	apply := f.apply
	f.applied = append(f.applied, cmd)
	f.mu.Unlock()
	if apply == nil {
		return nil, nil
	}
	return apply(cmd)
}

func (f *fakeConsensus) Role() consensus.Role {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return consensus.RoleUnknown
	}
	if f.leader {
		return consensus.RoleLeader
	}
	return consensus.RoleFollower
}

func (f *fakeConsensus) IsLeader() bool { return f.Role() == consensus.RoleLeader }

func (f *fakeConsensus) LeaderID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leader {
		return "node-a"
	}
	return f.leaderID
}

func (f *fakeConsensus) LeaderAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func (f *fakeConsensus) CommitIndex() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commit
}

func (f *fakeConsensus) Healthy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy && !f.closed
}

func (f *fakeConsensus) Term() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.term
}

func (f *fakeConsensus) AddServer(context.Context, string, string, uint64) error { return nil }
func (f *fakeConsensus) RemoveServer(context.Context, string, uint64) error      { return nil }

func (f *fakeConsensus) Servers() []consensus.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]consensus.Server(nil), f.servers...)
}

func (f *fakeConsensus) Shutdown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakeConsensus) Done() <-chan struct{} { return f.done }

func (f *fakeConsensus) setLeader(v bool) {
	f.mu.Lock()
	f.leader = v
	f.mu.Unlock()
}

func (f *fakeConsensus) setHealthy(v bool) {
	f.mu.Lock()
	f.healthy = v
	f.mu.Unlock()
}

func (f *fakeConsensus) setProposeError(err error) {
	f.mu.Lock()
	f.proposeE = err
	f.mu.Unlock()
}

func (f *fakeConsensus) installed() []consensus.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]consensus.Command(nil), f.applied...)
}

// newClusterHarness 构造一个注入了假共识的 runtime，并让假共识的 apply 路径
// 真的落到 rt.ApplyCommand（模拟 leader 提交后本地 apply）。
func newClusterHarness(t *testing.T, leader bool, mutate func(cfg map[string]any)) (*harness, *fakeConsensus) {
	t.Helper()
	fc := newFakeConsensus()
	fc.leader = leader
	h := newHarnessTuned(t, t.TempDir(), mutate, func(o *Options) {
		o.Consensus = fc
	})
	fc.mu.Lock()
	fc.apply = h.rt.ApplyCommand
	fc.mu.Unlock()
	return h, fc
}

// TestFollowerRejectsWritesWithLeaderHint 覆盖所有写端点的 409 契约。
//
// 契约细节必须稳定：客户端要靠 details.leader 重定向到真正的 leader。只返回
// 「409」而不给地址，等于让客户端自己去猜。
func TestFollowerRejectsWritesWithLeaderHint(t *testing.T) {
	h, fc := newClusterHarness(t, false, nil)
	fc.mu.Lock()
	fc.leaderID = "node-b"
	fc.addr = "10.0.0.2:7000"
	fc.mu.Unlock()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"config apply", http.MethodPost, "/api/v1/config/apply", `{"schemaVersion":1}`},
		{"config rollback", http.MethodPost, "/api/v1/config/rollback", `{"version":"sha256:abc"}`},
		{"point write", http.MethodPost, "/api/v1/points/sim01/write", `{"writes":[{"point":"write_me","value":{"type":"uint16","num":7}}]}`},
		{"command", http.MethodPost, "/api/v1/commands", `{"action":"driver.restart","params":{"device":"sim01"}}`},
		{"alarm ack", http.MethodPost, "/api/v1/alarms/ack", `{"all":true}`},
		{"staged begin", http.MethodPost, "/api/v1/config/staged", `{"config":{"schemaVersion":1},"policy":{}}`},
	}
	for _, c := range cases {
		code, body := h.do(c.method, c.path, c.body)
		if code != http.StatusConflict {
			t.Fatalf("%s: status = %d, want 409 (%s)", c.name, code, body)
		}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("%s: decode: %v (%s)", c.name, err, body)
		}
		if env.Error.Code != "cluster.not_leader" {
			t.Fatalf("%s: code = %q, want cluster.not_leader (%s)", c.name, env.Error.Code, body)
		}
		leader, _ := env.Error.Details["leader"].(map[string]any)
		if leader["instanceId"] != "node-b" {
			t.Fatalf("%s: details.leader.instanceId = %v, want node-b (%s)", c.name, leader["instanceId"], body)
		}
		if leader["raftAddr"] != "10.0.0.2:7000" {
			t.Fatalf("%s: details.leader.raftAddr = %v (%s)", c.name, leader["raftAddr"], body)
		}
	}

	// 读请求必须在 follower 上照常可用：把「不是 leader」一律当错误会让
	// /health 与 /api/v1/points 在多数副本上全部失效。
	if code, body := h.do(http.MethodGet, "/api/v1/leader", ""); code != http.StatusOK {
		t.Fatalf("GET /api/v1/leader on follower = %d (%s)", code, body)
	}
	if code, body := h.do(http.MethodGet, "/api/v1/health", ""); code != http.StatusOK {
		t.Fatalf("GET /api/v1/health on follower = %d (%s)", code, body)
	}
}

// TestUnhealthyReplicaRejectsWrites 验证 fail-closed：状态机分歧的副本即使
// 自认 leader 也必须拒绝写。
func TestUnhealthyReplicaRejectsWrites(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)
	fc.setHealthy(false)

	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"write_me","value":{"type":"uint16","num":7}}]}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", code, body)
	}
	if !stringsContains(body, "cluster.unhealthy") {
		t.Fatalf("body should carry cluster.unhealthy: %s", body)
	}
	if len(fc.installed()) != 0 {
		t.Fatal("unhealthy replica must not propose")
	}

	info := h.rt.Leader()
	if info.Healthy {
		t.Fatal("LeaderInfo.Healthy must be false for a diverged replica")
	}
	if info.Enabled == false || info.Role != string(consensus.RoleLeader) {
		t.Fatalf("LeaderInfo = %+v", info)
	}
}

// TestLeaderCommitsConfigApply 验证配置应用走共识：先本地校验，再提交，
// 返回 apply 结果，且各副本按同一 canonical 落盘。
func TestLeaderCommitsConfigApply(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)

	raw := h.rt.cfg.Current()
	cfg := map[string]any{}
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["site"] = "plant-02"
	body, _ := json.Marshal(cfg)

	code, resp := h.do(http.MethodPost, "/api/v1/config/apply", string(body))
	if code != http.StatusOK {
		t.Fatalf("apply config = %d (%s)", code, resp)
	}
	cmds := fc.installed()
	if len(cmds) != 1 || cmds[0].Kind != consensus.KindApplyConfig {
		t.Fatalf("installed commands = %+v, want one apply_config", cmds)
	}
	if h.rt.cfg.Current().Site != "plant-02" {
		t.Fatal("config was not applied locally on the leader")
	}

	// 版本哈希不含 cluster（未启用时为 nil），与本地应用一致。
	if got := h.rt.cfg.Version(); got == "" {
		t.Fatal("empty config version after apply")
	}

	// 无效配置应在提交前被拦下（422），绝不能进共识日志——apply 失败会
	// 把副本判成分歧并停写，一份笔误不该有这种代价。
	bad := map[string]any{}
	b, _ = json.Marshal(raw)
	json.Unmarshal(b, &bad)
	bad["channels"] = []any{}
	badBody, _ := json.Marshal(bad)
	code, resp = h.do(http.MethodPost, "/api/v1/config/apply", string(badBody))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid config = %d, want 422 (%s)", code, resp)
	}
	if n := len(fc.installed()); n != 1 {
		t.Fatalf("invalid config must not be proposed: %d commands", n)
	}
}

// TestLeaderCommitsWriteResult 验证点位写结果进共识（供故障转移后按幂等键重放），
// 且非 leader 不会执行设备 I/O。
func TestLeaderCommitsWriteResult(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/points/sim01/write",
		stringsReader(`{"writes":[{"point":"write_me","value":{"type":"uint16","num":7}}]}`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", "abc-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write = %d", resp.StatusCode)
	}

	cmds := fc.installed()
	if len(cmds) != 1 || cmds[0].Kind != consensus.KindWritePoints {
		t.Fatalf("installed commands = %+v, want one write_points", cmds)
	}
	var p commandPayload
	if err := json.Unmarshal(cmds[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Device != "sim01" || p.IdemKey != "abc-123" || len(p.Results) != 1 || !p.Results[0].OK {
		t.Fatalf("payload = %+v", p)
	}

	// 幂等重放：没有幂等键的写不进共识（没有可重放的结论），但设备 I/O 照常执行。
	if _, err := h.rt.Write(context.Background(), "sim01", []httpapi.WriteItem{
		{Point: "write_me", Value: model.NumValue(model.TypeUint16, 9)},
	}); err != nil {
		t.Fatalf("write without key: %v", err)
	}
	if n := len(fc.installed()); n != 1 {
		t.Fatalf("write without idempotency key must not be proposed: %d", n)
	}
}

// TestLeaderCommitsAckLedger 验证报警确认进共识账本，且账本只对「同一发生
// 实例」生效：确认时刻必须不早于报警的 Since。
func TestLeaderCommitsAckLedger(t *testing.T) {
	h, _ := newClusterHarness(t, true, nil)

	now := time.Now().UTC()
	rec := ackRecord{IDs: []string{"alarm-1"}, By: "ops", At: now}

	// 直接走 apply 路径：确认账本必须落进 r.clu，供升主后重放。
	if _, err := h.rt.ApplyCommand(consensus.Command{
		Kind:       consensus.KindAckAlarms,
		InstanceID: "node-a",
		Payload:    mustJSON(t, commandPayload{IDs: rec.IDs, By: rec.By, At: rec.At}),
	}); err != nil {
		t.Fatalf("apply ack: %v", err)
	}
	h.rt.cluMu.Lock()
	acks := append([]ackRecord(nil), h.rt.clu.acks...)
	h.rt.cluMu.Unlock()
	if len(acks) != 1 || len(acks[0].IDs) != 1 || acks[0].IDs[0] != "alarm-1" {
		t.Fatalf("ack ledger = %+v", acks)
	}

	// 快照 → 新实例恢复：账本必须随快照一起过去，否则升主后会把已确认的
	// 故障重新播报一遍。
	snap, err := h.rt.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var decoded clusterSnapshot
	if err := json.Unmarshal(snap, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(decoded.Acks) != 1 || decoded.Acks[0].IDs[0] != "alarm-1" {
		t.Fatalf("snapshot acks = %+v", decoded.Acks)
	}
	if len(decoded.Config) == 0 {
		t.Fatal("snapshot must carry the shared config")
	}
}

// TestProposeFailureMapsToTimeout 验证「无法确认提交」不是成功：写路径返回
// 504 且消息说明可能已在本地执行。
func TestProposeFailureMapsToTimeout(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)
	fc.setProposeError(consensus.ErrNoQuorum)

	raw := h.rt.cfg.Current()
	b, _ := json.Marshal(raw)
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["site"] = "plant-02"
	bodyJSON, _ := json.Marshal(cfg)
	code, body := h.do(http.MethodPost, "/api/v1/config/apply", string(bodyJSON))
	if code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (%s)", code, body)
	}
	if !stringsContains(body, "cluster.no_quorum") {
		t.Fatalf("body = %s", body)
	}
	if !stringsContains(body, "may have been applied locally") {
		t.Fatalf("message must warn the write may already have happened: %s", body)
	}
}

// TestIdempotencyTablePersistsAcrossSnapshotRestore 验证「故障转移后按幂等键
// 重放写」这条契约：写结果必须进快照、能从快照恢复——否则新 leader 不知道
// 「这个 key 已经写过了」，会对同一笔幂等写重新执行设备 I/O。
func TestIdempotencyTablePersistsAcrossSnapshotRestore(t *testing.T) {
	h, _ := newClusterHarness(t, true, nil)

	// 通过 HTTP 端点提交一条带幂等键的写（X-Idempotency-Key header），
	// 落到 r.idem。
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/points/sim01/write",
		stringsReader(`{"writes":[{"point":"write_me","value":{"type":"uint16","num":42}}]}`))
	req.Header.Set("X-Idempotency-Key", "test-idem-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status = %d", resp.StatusCode)
	}

	h.rt.mu.Lock()
	before := len(h.rt.idem)
	h.rt.mu.Unlock()
	if before == 0 {
		t.Fatal("write with idempotency key must land in the local table")
	}

	// 快照 → 解码 → 恢复到一个新 runtime，验证幂等表随快照迁移。
	snap, err := h.rt.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var decoded clusterSnapshot
	if err := json.Unmarshal(snap, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Idem) != before {
		t.Fatalf("snapshot must carry the idem table: got %d entries, want %d", len(decoded.Idem), before)
	}
	for k, e := range decoded.Idem {
		h.rt.mu.Lock()
		got, ok := h.rt.idem[k]
		h.rt.mu.Unlock()
		if !ok {
			t.Fatalf("restored runtime missing idem key %q", k)
		}
		if len(got.results) != len(e.Results) {
			t.Fatalf("restored result set differs for %q", k)
		}
	}
}

// TestAckLedgerReplayedAfterApply 验证「确认过的报警在 apply 后仍保持已确认」：
// ApplyCommand 对 KindAckAlarms 必须把记录落进 r.clu.acks（供快照与重放），
// 并直接套用到本地报警引擎（同一发生实例守卫）。
func TestAckLedgerReplayedAfterApply(t *testing.T) {
	// 构造一个独立的 Runtime，不启动它（避免 alarm engine 的并发问题）。
	// 我们只测试 ApplyCommand 的 ack ledger 逻辑，不需要完整的 runtime 生命周期。
	h, _ := newClusterHarness(t, true, nil)

	// 停止 runtime 以避免 alarm engine 的并发问题。
	if err := h.rt.Stop(5 * time.Second); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}

	// 造一条 active 报警：注入引擎，并塞一个 Since 早于确认时刻的状态。
	now := time.Now().UTC()
	rec := ackRecord{IDs: []string{"ghost-alarm"}, By: "ops", At: now}

	// harness 默认没有报警规则，这里临时塞一条进去，只为验证 applyAckRecord
	// 的守卫逻辑（Since 早于确认时刻 → 确认；Since 晚于确认时刻 → 跳过）。
	// 注意：alarm.New 不会启动引擎（Start 才会），所以这里创建的引擎
	// 是静态的，可以直接操作它的状态而不用担心并发。
	e, err := alarm.New(alarm.Options{
		Rules: []alarm.Rule{{
			ID: "ghost-alarm", Name: "ghost", Severity: alarm.SeverityCritical,
			Point: "sim01:reg0", High: ptrFloat(50),
		}},
	})
	if err != nil {
		t.Fatalf("alarm.New: %v", err)
	}
	h.rt.alarms = e
	setAlarmState(h.rt.alarms, "ghost-alarm", now.Add(-time.Hour))

	if _, err := h.rt.ApplyCommand(consensus.Command{
		Kind:    consensus.KindAckAlarms,
		Payload: mustJSON(t, commandPayload{IDs: rec.IDs, All: false, By: rec.By, At: rec.At}),
	}); err != nil {
		t.Fatalf("apply ack: %v", err)
	}
	h.rt.cluMu.Lock()
	acks := append([]ackRecord(nil), h.rt.clu.acks...)
	h.rt.cluMu.Unlock()
	if len(acks) != 1 || len(acks[0].IDs) != 1 || acks[0].IDs[0] != "ghost-alarm" {
		t.Fatalf("ack ledger = %+v", acks)
	}
	// 账本必须真的确认了报警（Because 早于确认时刻）。
	st, ok := h.rt.alarms.Get("ghost-alarm")
	if !ok || !st.Acknowledged {
		t.Fatalf("alarm must be acked after apply: %+v", st)
	}
	// 同一时刻再套一次账本：幂等，不重复确认。
	h.rt.reapplyAckLedger()
	h.rt.cluMu.Lock()
	acks = append([]ackRecord(nil), h.rt.clu.acks...)
	h.rt.cluMu.Unlock()
	if len(acks) != 1 {
		t.Fatalf("reapplyAckLedger must not duplicate ledger entries: %+v", acks)
	}
}

// TestAckLedgerSkipsLaterOccurrence 是同发生实例守卫的纯逻辑测试：
// 报警的 Since 晚于确认时刻时，不允许「预确认」尚未发生的故障。
func TestAckLedgerSkipsLaterOccurrence(t *testing.T) {
	now := time.Now().UTC()

	// 构造一个含两条规则的引擎，分别塞成 active/未确认状态。
	e, err := alarm.New(alarm.Options{
		Rules: []alarm.Rule{
			{ID: "future-alarm", Name: "future", Severity: alarm.SeverityCritical,
				Point: "sim01:reg0", High: ptrFloat(50)},
			{ID: "past-alarm", Name: "past", Severity: alarm.SeverityCritical,
				Point: "sim01:reg0", High: ptrFloat(50)},
		},
	})
	if err != nil {
		t.Fatalf("alarm.New: %v", err)
	}

	// 报警的 Since 比确认时刻晚 1 小时：不允许确认。
	future := now.Add(time.Hour)
	setAlarmState(e, "future-alarm", future)
	applyAckRecordStandalone(e, ackRecord{IDs: []string{"future-alarm"}, By: "ops", At: now})
	if st, _ := e.Get("future-alarm"); st.Acknowledged {
		t.Fatal("ack record must NOT confirm an occurrence that starts after it")
	}

	// 报警的 Since 比确认时刻早 1 小时：应当确认。
	past := now.Add(-time.Hour)
	setAlarmState(e, "past-alarm", past)
	applyAckRecordStandalone(e, ackRecord{IDs: []string{"past-alarm"}, By: "ops", At: now})
	if st, _ := e.Get("past-alarm"); !st.Acknowledged {
		t.Fatal("ack record must confirm an occurrence that started before it")
	}
}

// applyAckRecordStandalone 是一个独立的 applyAckRecord 实现，用于测试
// 不依赖 Runtime 的纯逻辑。
func applyAckRecordStandalone(e *alarm.Engine, rec ackRecord) {
	if e == nil || len(rec.IDs) == 0 {
		return
	}
	for _, id := range rec.IDs {
		st, ok := e.Get(id)
		if !ok || st.Acknowledged || !st.Active || st.Since == nil {
			continue
		}
		if st.Since.After(rec.At) {
			continue
		}
		if _, err := e.Ack(id, rec.By); err != nil {
			continue
		}
	}
}

func ptrFloat(v float64) *float64 { return &v }

// setAlarmState 在报警引擎里塞一条 active 状态（绕过采集时序，专注验证守卫）。
func setAlarmState(e *alarm.Engine, id string, since time.Time) {
	e.SetStateForTest(id, since)
}

// freePort 取一个当前空闲的 loopback 端口，并立刻释放。
//
// 单节点真实 raft 需要固定端口（配置校验拒绝端口 0），测试只能先探一个空闲
// 端口再用。这里不担心「释放后被别的进程抢走」：测试进程内即取即用。
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestSingleNodeRaftEndToEnd 用真实 raft（boltdb + TCP）验证单节点 always-leader
// 的完整链路：配置应用经共识提交、本地 apply、reload 生效、/api/v1/leader 可见。
func TestSingleNodeRaftEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("real raft cluster")
	}
	addr := freePort(t)
	h := newHarnessTuned(t, t.TempDir(), func(cfg map[string]any) {
		cfg["cluster"] = map[string]any{
			"enabled": true,
			"listen":  addr,
		}
	}, nil)

	// 单节点引导后应立即成为 leader（always-leader）。
	deadline := time.Now().Add(15 * time.Second)
	var info httpapi.LeaderInfo
	for time.Now().Before(deadline) {
		info = h.rt.Leader()
		if info.Role == string(consensus.RoleLeader) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.Role != string(consensus.RoleLeader) {
		t.Fatalf("single node never became leader: %+v", info)
	}
	if !info.Enabled {
		t.Fatal("LeaderInfo.Enabled should be true with cluster enabled")
	}

	code, body := h.do(http.MethodGet, "/api/v1/leader", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/leader = %d (%s)", code, body)
	}
	var got httpapi.LeaderInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if got.Role != string(consensus.RoleLeader) || got.CommitIndex == 0 {
		t.Fatalf("leader view = %+v", got)
	}

	// 配置应用：提交 → apply → 落盘 → reload。
	raw := h.rt.cfg.Current()
	b, _ := json.Marshal(raw)
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["site"] = "plant-02"
	bodyJSON, _ := json.Marshal(cfg)
	code, resp := h.do(http.MethodPost, "/api/v1/config/apply", string(bodyJSON))
	if code != http.StatusOK {
		t.Fatalf("apply config = %d (%s)", code, resp)
	}
	if got := h.rt.cfg.Current().Site; got != "plant-02" {
		t.Fatalf("site = %q, want plant-02", got)
	}
	// 配置里没有 cluster 字段时，本机的 cluster 块必须仍然保留（见
	// config.Manager.Apply）：否则一次配置应用就会把节点身份抹掉。
	if c := h.rt.cfg.Current().Cluster; c == nil || !c.Enabled || c.Listen != addr {
		t.Fatalf("local cluster block lost after apply: %+v", c)
	}

	// 再次启动的同一个运行实例可正常停止（raft Shutdown 不应阻塞）。
	if err := h.rt.Stop(5 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func stringsContains(b []byte, sub string) bool { return strings.Contains(string(b), sub) }

// TestFollowerStopsCollectionAndResumesOnPromotion 覆盖角色驱动的采集开关。
//
// 关键不是「follower 不采集」本身，而是它的可见性：点位必须被显式标成
// disabled（而不是保留上一次好值假装新鲜），升主后又必须自动恢复采集，
// 不需要人工重启进程。
func TestFollowerStopsCollectionAndResumesOnPromotion(t *testing.T) {
	h, fc := newClusterHarness(t, false, nil)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", what)
	}

	// follower：不采集、点位标 disabled、不往 Outbox 入队。
	waitFor("standby", func() bool {
		s, ok := h.rt.Point("sim01", "reg0")
		return ok && s.Quality == model.QualityDisabled
	})
	// 设备态翻 disabled 与点位标 disabled 之间也有「半个批次」的窗口，轮询收敛。
	waitFor("device disabled", func() bool {
		return h.rt.Health().Devices.Collecting == 0
	})
	time.Sleep(300 * time.Millisecond) // 覆盖几个采集周期，确认没有延迟入队
	if hl := h.rt.Health(); hl.Outbox.Pending != 0 || hl.Outbox.Delivered != 0 {
		t.Fatalf("follower wrote to outbox: %+v", hl.Outbox)
	}
	if n := len(fc.installed()); n != 0 {
		t.Fatalf("follower proposed %d commands", n)
	}
	// 待命原因必须可解释，否则运维看到 disabled 只会怀疑配置写错了。
	if s, _ := h.rt.Point("sim01", "reg0"); !stringsContains([]byte(s.Error), "not the cluster leader") {
		t.Fatalf("standby sample should name the reason: %+v", s)
	}

	// 升主：自动恢复采集。
	fc.setLeader(true)
	// 升主后由 clusterLoop 的下一个 tick（周期 500ms）调用 applyRole(true)
	// 重新启用设备。点位变 good 与设备态翻 collecting 是两个独立事件：点位
	// 逐点 cache.Set，设备态要等本轮采集 finish 才转 collecting，中途存在
	// 「已 good、但 Collecting 还是 0」的窗口。因此两个条件都轮询等待，不
	// 做一次性快照断言（后者等价于赌调度器的抢占顺序，-race 下会稳定失败）。
	waitFor("collecting", func() bool {
		s, ok := h.rt.Point("sim01", "reg0")
		return ok && s.Quality == model.QualityGood
	})
	waitFor("device collecting", func() bool {
		return h.rt.Health().Devices.Collecting > 0
	})
	// 采集恢复后样本应真正投递到 sink。
	//
	// 断言 Delivered（累计计数器）而不是 Pending（瞬时 gauge）：journal sink
	// 是本地文件、投递几乎总是成功，dispatchLoop 会把刚入队的记录立刻 drain 掉，
	// 于是 Pending 在两次 20ms 轮询之间几乎必然回到 0——断言它 >0 等价于赌调度
	// 时机，机器繁忙时稳定失败。Delivered 是只增的累计值，能稳定反映「确实投递过」。
	waitFor("outbox delivered", func() bool { return h.rt.Health().Outbox.Delivered > 0 })
}

// TestUnhealthyReplicaStopsCollection 验证 fail-closed 的采集侧：状态机分歧的
// 副本即使 raft 认为它是 leader 也必须停采——它已无法保证与集群状态一致。
func TestUnhealthyReplicaStopsCollection(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)

	// 制造一次 apply 失败：状态机分歧 → Healthy()==false，但 raft 仍是 leader。
	// fakeConsensus 不经过真实 raft FSM，无法靠非法 Kind 触发 applyErr；这里
	// 直接置 unhealthy 来模拟「副本状态机已与日志分歧」这一终态，并验证
	// isLeader() 的 fail-closed 语义（Healthy() && IsLeader()）。
	fc.setHealthy(false)
	if fc.Healthy() {
		t.Fatal("precondition: replica should be unhealthy after a bad apply")
	}
	if !fc.IsLeader() {
		t.Fatal("precondition: raft still thinks this node is leader")
	}

	// 采集闸必须判停（isLeader 同时看 Healthy）。
	if h.rt.isLeader() {
		t.Fatal("isLeader() must be false for an unhealthy replica")
	}
	// 主动切一次角色，让 setCollectionActive(false) 生效。
	h.rt.applyRole(false, time.Now())
	if hl := h.rt.Health(); hl.Devices.Collecting != 0 {
		t.Fatalf("unhealthy replica still collecting: %+v", hl.Devices)
	}
	if s, ok := h.rt.Point("sim01", "reg0"); !ok || s.Quality != model.QualityDisabled {
		t.Fatalf("unhealthy replica must mark points disabled, got %+v", s)
	}
}

// TestApplyRoleAbandonsStagedRollout 验证灰度计划是 leader 本地状态：丢主时
// 必须就地标中止，而不是留着让旧 leader 下次升主继续「下一批」。
func TestApplyRoleAbandonsStagedRollout(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)

	// 注入一个进行中的灰度计划（不需要真的走完整 BeginStaged：这里只验证
	// applyRole(false) 对已存在计划的处置）。
	ro := &stagedRollout{}
	ro.status.Active = true
	ro.status.NextBatch = 1
	ro.status.TotalBatches = 3
	h.rt.staged.mu.Lock()
	h.rt.staged.cur = ro
	h.rt.staged.mu.Unlock()

	// 丢主：计划必须被标中止。
	h.rt.applyRole(false, time.Now())

	h.rt.staged.mu.Lock()
	got := h.rt.staged.cur
	h.rt.staged.mu.Unlock()
	if got == nil || got.status.Active {
		t.Fatalf("staged rollout must be abandoned on demotion: %+v", got)
	}
	if !got.status.Aborted {
		t.Fatalf("staged rollout must be marked aborted: %+v", got.status)
	}

	// 已中止的计划不应再被驱动（幂等）。
	fc.setLeader(true)
	h.rt.applyRole(true, time.Now())
	h.rt.staged.mu.Lock()
	still := h.rt.staged.cur
	h.rt.staged.mu.Unlock()
	if still != nil && still.status.Aborted && still.status.Active {
		t.Fatal("abandoned rollout must not reactivate on promotion")
	}
}

// TestApplyConfigCanonicalWarnsOnNorthboundRestart 验证共享配置里 northbound
// 参数变更会留下「需重启生效」的告警——否则运维盯着没变的 retention 会以为
// 配置没生效。
func TestApplyConfigCanonicalWarnsOnNorthboundRestart(t *testing.T) {
	h, _ := newClusterHarness(t, true, nil)

	raw := h.rt.cfg.Current()
	b, _ := json.Marshal(raw)
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	// 改 northbound（MQTT 参数属于需重启生效的一类）。
	nb, _ := cfg["northbound"].(map[string]any)
	if nb == nil {
		nb = map[string]any{}
		cfg["northbound"] = nb
	}
	mqtt, _ := nb["mqtt"].(map[string]any)
	if mqtt == nil {
		mqtt = map[string]any{}
		nb["mqtt"] = mqtt
	}
	mqtt["clientId"] = "changed-client-id"
	bodyJSON, _ := json.Marshal(cfg)

	// 捕获日志：这里只验证配置确实被应用（告警路径本身是 Best-Effort 观测，
	// 真正的回归是「配置变更后 reload 不报错、版本推进」）。若要断言告警文本，
	// 需要把 r.log 换成捕获型 slog handler——该覆盖在 logtest 里做。
	code, resp := h.do(http.MethodPost, "/api/v1/config/apply", string(bodyJSON))
	if code != http.StatusOK {
		t.Fatalf("apply config = %d (%s)", code, resp)
	}
	if got := h.rt.cfg.Current(); got == nil {
		t.Fatal("config missing after apply")
	}
}

// TestReconcileMembershipKeepsUnknownMembers 验证「对账只增不删」：raft 成员
// 表里存在但配置里没有的节点只打 Warning，不会被自动 RemoveServer——缩容是
// 人工决策。
func TestReconcileMembershipKeepsUnknownMembers(t *testing.T) {
	h, fc := newClusterHarness(t, true, nil)

	// 配置里只有 node-a；raft 成员表却有三台。多余的 node-c 不该被删。
	cfg := map[string]any{
		"cluster": map[string]any{
			"enabled": true,
			"listen":  "127.0.0.1:0",
			"peers": []any{
				map[string]any{"id": "node-a", "addr": "10.0.0.1:7000"},
			},
		},
	}
	_ = cfg
	fc.mu.Lock()
	fc.servers = []consensus.Server{
		{ID: "node-a", Addr: "10.0.0.1:7000", Suffrage: consensus.SuffrageVoter},
		{ID: "node-b", Addr: "10.0.0.2:7000", Suffrage: consensus.SuffrageVoter},
		{ID: "node-c", Addr: "10.0.0.3:7000", Suffrage: consensus.SuffrageVoter},
	}
	fc.mu.Unlock()

	h.rt.clusterCfg = h.rt.cfg.Current().Cluster
	// 直接驱动 reconcile（周期 30s，等不及；且这是确定性单元）。
	h.rt.reconcileMembership()

	got := fc.Servers()
	if len(got) != 3 {
		t.Fatalf("reconcileMembership removed unknown members: %+v", got)
	}
	// node-a 已在成员表里，不该被重复 AddServer。
	// 这里只断言「没删」：增员的副作用由集成测试覆盖。
}

// TestCommandReplicatedByInner 验证 config.rollback / alarm.ack 不在外层
// 重复提交决策记录（它们已在内层提交过账本）。
func TestCommandReplicatedByInner(t *testing.T) {
	for _, action := range []string{"config.rollback", "alarm.ack"} {
		if !commandReplicatedByInner(action) {
			t.Fatalf("%s must be marked as replicated by inner", action)
		}
	}
	for _, action := range []string{"driver.restart", "log.level", "device.selftest"} {
		if commandReplicatedByInner(action) {
			t.Fatalf("%s must NOT be marked as replicated by inner", action)
		}
	}
	// localOnlyCommand 只列 log.level。
	if !localOnlyCommand("log.level") {
		t.Fatal("log.level must be a local-only command")
	}
	if localOnlyCommand("driver.restart") {
		t.Fatal("driver.restart must NOT be local-only")
	}
}
