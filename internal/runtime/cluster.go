package runtime

// 跨实例协调（raft）在 runtime 侧的接线：Applier 实现、leader 闸、提交式写、
// 角色驱动的采集开关、成员对账与角色指标。
//
// 分工刻意清楚：
//   - internal/consensus 只负责「选主 + 把已提交命令按序喂给 Applier」；
//   - 本文件是 Applier 的实现，也是所有写路径的提交入口；
//   - 命令载荷的编码/解码在这里（consensus 不 import 任何业务包）。
//
// 复制的边界（见 ADR-0005 §4.1）：
//   - 复制：配置应用/回滚（每个副本都要按相同顺序得到同一份配置）、点位写
//     结果（用于故障转移后仍能按幂等键重放）、报警确认记录（新 leader 可据此
//     恢复同一发生实例的确认状态）、控制命令记录（复制决策日志）。
//   - 不复制：采集样本、设备 I/O 副作用本身、指标/日志/心跳。设备写与控制命令
//     只在 leader 上真正执行一次；副本 apply 的是「决策与结果记录」，不会对着
//     自己的设备再执行一遍——那会把一次写变成 N 次。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"uic/internal/config"
	"uic/internal/consensus"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
)

// ---------------------------------------------------------------------------
// 载荷
// ---------------------------------------------------------------------------

// commandPayload 是复制到 raft 日志里的业务载荷。
//
// 一个结构承载五种命令而不是每类一个：字段少、互斥清晰，且避免为每类再写一套
// 编解码（编解码越少，「同一条日志在各副本解出不同东西」的面越小）。kind 由
// Command.Kind 给出，apply 时按 kind 取对应字段，缺字段一律报错（fail-closed）。
type commandPayload struct {
	// Source 是配置变更来源（api/rollback/staged-*）。
	Source string `json:"source,omitempty"`
	// Config 是共享配置的 canonical 字节（已排除节点本地 cluster 块）。
	Config json.RawMessage `json:"config,omitempty"`
	// Version 是回滚目标版本号。
	Version string `json:"version,omitempty"`

	// Device/IdemKey/Items/Results 是点位写的结果记录。Results 由 leader 在
	// 执行设备写之后填入，随日志复制——副本据此在故障转移后仍能按幂等键
	// 返回与 leader 相同的结论。
	Device  string                `json:"device,omitempty"`
	IdemKey string                `json:"idemKey,omitempty"`
	Items   []httpapi.WriteItem   `json:"items,omitempty"`
	Results []httpapi.WriteResult `json:"results,omitempty"`

	// Action/Params/Result/CommandErr 是控制命令的决策记录。
	Action     string          `json:"action,omitempty"`
	Params     map[string]any  `json:"params,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	CommandErr string          `json:"commandError,omitempty"`

	// IDs/All/By 是报警确认记录（IDs 取实际被确认的报警，而不是请求里的
	// 原始参数：all=true 时原始参数没有具体 ID）。
	IDs []string `json:"ids,omitempty"`
	All bool     `json:"all,omitempty"`
	By  string   `json:"by,omitempty"`

	// At 是 leader 决定该命令的时刻（UTC）。幂等记录与确认记录的时效都以
	// 它为准，而不是各副本 apply 的时刻——否则同一份日志在不同副本上会
	// 得到不同的过期结论。
	At time.Time `json:"at"`
}

// actorWire 是随日志复制的调用方身份。
type actorWire struct {
	User      string `json:"user,omitempty"`
	Role      string `json:"role,omitempty"`
	Remote    string `json:"remote,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

func actorOf(ctx context.Context) actorWire {
	a, ok := httpapi.ActorFrom(ctx)
	if !ok {
		return actorWire{}
	}
	return actorWire{User: a.User, Role: string(a.Role), Remote: a.Remote, RequestID: a.RequestID}
}

// context 还原审计/来源上下文：副本 apply 时没有原请求的 context，但审计仍应
// 记录「这是谁发起的、从哪个面发起」。
func (p commandPayload) context() context.Context {
	ctx := context.Background()
	if p.Source != "" {
		ctx = withSource(ctx, p.Source)
	}
	return ctx
}

// ---------------------------------------------------------------------------
// 被复制的记录（同时进快照）
// ---------------------------------------------------------------------------

type commandRecord struct {
	Action     string          `json:"action"`
	Params     map[string]any  `json:"params,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	InstanceID string          `json:"instanceId,omitempty"`
	At         time.Time       `json:"at"`
}

type ackRecord struct {
	IDs []string  `json:"ids"`
	All bool      `json:"all,omitempty"`
	By  string    `json:"by"`
	At  time.Time `json:"at"`
}

// clusterRecords 是被共识复制的「决策日志」：控制命令与报警确认。
//
// 有界保留（各 512 条）：它们是审计/复盘证据，不是业务状态，不能让日志与
// 内存无界增长。
type clusterRecords struct {
	commands []commandRecord
	acks     []ackRecord
}

const (
	clusterCommandLimit = 512
	clusterAckLimit     = 512
)

func (c *clusterRecords) addCommand(rec commandRecord) {
	c.commands = append(c.commands, rec)
	if n := len(c.commands) - clusterCommandLimit; n > 0 {
		c.commands = append([]commandRecord(nil), c.commands[n:]...)
	}
}

func (c *clusterRecords) addAck(rec ackRecord) {
	c.acks = append(c.acks, rec)
	if n := len(c.acks) - clusterAckLimit; n > 0 {
		c.acks = append([]ackRecord(nil), c.acks[n:]...)
	}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// openConsensus 按配置构造 raft 集群；未启用时保持 r.consensus == nil。
//
// 单节点判定刻意简单：config 里没有 peers（或只有自身）= 单节点 always-leader。
// 多节点时初始投票集**只写自身一条**——种子节点若在引导时把尚未启动的节点
// 写进投票集，会因凑不齐多数派而永远选不出 leader（这是 raft 引导最常见的
// 事故）。其余节点由 leader 启动后经 AddServer 加入（reconcileMembership）。
func (r *Runtime) openConsensus(f *config.File) error {
	if r.opt.Consensus != nil {
		r.consensus = r.opt.Consensus
		if f.Cluster != nil {
			r.clusterCfg = cloneCluster(f.Cluster)
		}
		return nil
	}
	if !f.ClusterEnabled() {
		return nil
	}
	c := f.Cluster
	nodeID := c.NodeID
	if nodeID == "" {
		nodeID = r.instanceID
	}
	if nodeID == "" {
		return errors.New("runtime: cluster.nodeId is empty and no instance id is available")
	}
	dataDir := c.DataDir
	if dataDir == "" {
		dataDir = "raft"
	}
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(r.dataDir, dataDir)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("runtime: cluster data dir: %w", err)
	}

	cp := cloneCluster(c)
	r.clusterCfg = cp
	peers := cp.Peers
	single := len(peers) <= 1
	bootstrap := c.Bootstrap || single

	cs, err := consensus.New(consensus.Options{
		NodeID:    nodeID,
		Addr:      c.Listen,
		DataDir:   dataDir,
		Bootstrap: bootstrap,
		// 初始投票集只含自身；Peers 的**完整**成员表由 runtime 对账使用。
		Peers:     []consensus.Server{{ID: nodeID, Addr: c.Listen}},
		LogOutput: os.Stderr,
		LogLevel:  "warn",
	}, r)
	if err != nil {
		return fmt.Errorf("runtime: cluster: %w", err)
	}
	r.consensus = cs
	r.nodeID = nodeID
	r.clusterAddr = c.Listen
	r.setRoleSince(time.Now())
	if !single && !c.Bootstrap {
		// 非种子节点要等 leader 经 AddServer 加入，这里说明清楚，免得
		// 运维看到「一直 candidate」去查网络。
		r.log.Info("cluster_waiting_for_leader", "nodeId", nodeID, "listen", c.Listen)
	}
	r.log.Info("cluster_started", "nodeId", nodeID, "listen", c.Listen,
		"bootstrap", bootstrap, "peers", len(peers), "dataDir", dataDir)
	return nil
}

func cloneCluster(c *config.Cluster) *config.Cluster {
	if c == nil {
		return nil
	}
	cp := *c
	if c.Peers != nil {
		cp.Peers = append([]config.Peer(nil), c.Peers...)
	}
	return &cp
}

func (r *Runtime) setRoleSince(t time.Time) {
	r.roleSinceMu.Lock()
	r.roleSince = t
	r.roleSinceMu.Unlock()
}

func (r *Runtime) roleSinceTime() time.Time {
	r.roleSinceMu.Lock()
	defer r.roleSinceMu.Unlock()
	return r.roleSince
}

// closeConsensus 关闭 raft（幂等）。Stop 与启动失败收口都走这里。
func (r *Runtime) closeConsensus() {
	if r.consensus == nil {
		return
	}
	if err := r.consensus.Shutdown(); err != nil {
		r.log.Error("cluster_shutdown_failed", "error", errText(err))
	}
}

// ---------------------------------------------------------------------------
// leader 判定与写闸
// ---------------------------------------------------------------------------

// isLeader 是采集与投递的闸：未启用集群恒为 true（单实例行为不变）。
//
// 状态机不一致（apply 失败过）的副本即使 raft 认为它是 leader 也不能采集/写：
// 它已经无法保证自己与集群状态一致，继续产出只会制造需要人工对账的数据。
func (r *Runtime) isLeader() bool {
	if r.consensus == nil {
		return true
	}
	return r.consensus.Healthy() && r.consensus.IsLeader()
}

// requireLeader 是写路径的闸，返回可直接写给客户端的 APIError。
func (r *Runtime) requireLeader() error {
	if r.consensus == nil {
		return nil
	}
	if !r.consensus.Healthy() {
		return httpapi.Errf(http.StatusServiceUnavailable, "cluster.unhealthy",
			"cluster state machine on this instance is unhealthy; writes are rejected")
	}
	if !r.consensus.IsLeader() {
		e := httpapi.Errf(http.StatusConflict, "cluster.not_leader",
			"this instance is not the cluster leader")
		leader := map[string]any{"instanceId": r.consensus.LeaderID()}
		if addr := r.consensus.LeaderAddr(); addr != "" {
			leader["raftAddr"] = addr
		}
		if r.consensus.LeaderID() == "" {
			leader["note"] = "leader election in progress"
		}
		e.Details = map[string]any{"leader": leader}
		return e
	}
	return nil
}

// clusterWriteError 把 consensus 的错误翻译成对外契约。
//
// 关键：**凡是「不能确认已提交」的错误都不是成功**。写路径在调用方看来必须
// 是 4xx/5xx，并在消息里说明「可能已在本地执行」——静默当成功是缺陷。
func (r *Runtime) clusterWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, consensus.ErrNotLeader):
		return r.requireLeader()
	case errors.Is(err, consensus.ErrUnhealthy):
		return httpapi.Errf(http.StatusServiceUnavailable, "cluster.unhealthy",
			"cluster state machine on this instance is unhealthy; writes are rejected")
	case errors.Is(err, consensus.ErrClosed):
		return httpapi.Errf(http.StatusServiceUnavailable, "cluster.closed",
			"cluster consensus is shutting down; retry after restart")
	case errors.Is(err, consensus.ErrNoQuorum),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return httpapi.Errf(http.StatusGatewayTimeout, "cluster.no_quorum",
			"cannot commit to the cluster consensus (%v); the operation may have been applied locally", err)
	default:
		return httpapi.Errf(http.StatusServiceUnavailable, "cluster.commit_failed",
			"cannot commit to the cluster consensus (%v); the operation may have been applied locally", err)
	}
}

// propose 提交一条命令并记录指标。r.consensus 为 nil 时返回明确错误（调用方
// 应先判 isLeader/consensus==nil）。
func (r *Runtime) propose(ctx context.Context, kind consensus.Kind, p commandPayload) (any, error) {
	if r.consensus == nil {
		return nil, consensus.ErrClosed
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("runtime: encode %s: %w", kind, err)
	}
	res, err := r.consensus.Propose(ctx, consensus.Command{Kind: kind, InstanceID: r.instanceID, Payload: b})
	switch {
	case err == nil:
		r.metrics.Inc("uic_raft_proposals_total", obs.Labels{"result": "committed"})
	case errors.Is(err, consensus.ErrNotLeader), errors.Is(err, consensus.ErrUnhealthy):
		r.metrics.Inc("uic_raft_proposals_total", obs.Labels{"result": "rejected"})
	default:
		r.metrics.Inc("uic_raft_proposals_total", obs.Labels{"result": "timeout"})
	}
	return res, err
}

// ---------------------------------------------------------------------------
// Applier 实现
// ---------------------------------------------------------------------------

// ApplyCommand 实现 consensus.Applier。raft 在命令被多数派 commit 后按序调用。
//
// 这里是**所有副本**都会走到的路径：只能做确定性状态变更（配置落盘、记录
// 写入、确认账本），绝不能再触发设备 I/O、Outbox 入队、审计追加等副作用——
// 那些只在 leader 的请求处理路径上发生一次。
func (r *Runtime) ApplyCommand(cmd consensus.Command) (any, error) {
	r.cluMu.Lock()
	defer r.cluMu.Unlock()

	var p commandPayload
	if len(cmd.Payload) > 0 {
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return nil, fmt.Errorf("runtime: apply %s: %w", cmd.Kind, err)
		}
	}
	switch cmd.Kind {
	case consensus.KindApplyConfig:
		if len(p.Config) == 0 {
			return nil, errors.New("runtime: apply_config without config payload")
		}
		return r.applyConfigCanonical(p.context(), p.Config, p.Source)
	case consensus.KindRollbackConfig:
		if p.Version == "" {
			return nil, errors.New("runtime: rollback_config without version")
		}
		return r.rollbackCanonical(p.context(), p.Version)
	case consensus.KindWritePoints:
		if p.IdemKey == "" {
			// 没有幂等键的写没有可复制的结论：宁可拒绝，也不把一条无主记录
			// 塞进各副本的幂等表。
			return nil, errors.New("runtime: write_points without idem key")
		}
		r.idemPutAt(p.Device+"\x00"+p.IdemKey, p.Results, p.At)
		return nil, nil
	case consensus.KindCommand:
		if p.Action == "" {
			return nil, errors.New("runtime: command without action")
		}
		r.clu.addCommand(commandRecord{
			Action: p.Action, Params: p.Params, Result: p.Result,
			Error: p.CommandErr, InstanceID: cmd.InstanceID, At: p.At,
		})
		r.log.Debug("cluster_command_recorded", "action", p.Action, "at", p.At)
		return nil, nil
	case consensus.KindAckAlarms:
		r.clu.addAck(ackRecord{IDs: p.IDs, All: p.All, By: p.By, At: p.At})
		r.applyAckRecord(ackRecord{IDs: p.IDs, By: p.By, At: p.At})
		return nil, nil
	default:
		return nil, fmt.Errorf("runtime: unknown command kind %q", cmd.Kind)
	}
}

// applyAckRecord 把一条确认记录套用到本地报警引擎，但**只限同一发生实例**：
// 报警当前的 Since 必须不晚于确认时刻。否则「上次故障被确认过」会让下一次
// 真实故障一出现就被自动确认——那比丢一次确认危险得多。
func (r *Runtime) applyAckRecord(rec ackRecord) {
	if r.alarms == nil || len(rec.IDs) == 0 {
		return
	}
	for _, id := range rec.IDs {
		st, ok := r.alarms.Get(id)
		if !ok || st.Acknowledged || !st.Active || st.Since == nil {
			continue
		}
		if st.Since.After(rec.At) {
			continue
		}
		if _, err := r.alarms.Ack(id, rec.By); err != nil {
			continue
		}
	}
}

// reapplyAckLedger 在成为 leader 后尝试套用账本里的确认记录。
//
// follower 不采集，报警状态是空的；升主后第一次采集才会重新产生报警。周期性
// 套用让「确认过的同一发生实例」在升主后保持已确认，而不是把整个故障重新
// 播报一遍。失败不致命：报警仍然是 active（只是未确认），方向是安全的。
func (r *Runtime) reapplyAckLedger() {
	if r.consensus == nil || r.alarms == nil {
		return
	}
	r.cluMu.Lock()
	acks := append([]ackRecord(nil), r.clu.acks...)
	r.cluMu.Unlock()
	for _, rec := range acks {
		r.applyAckRecord(rec)
	}
}

// ---------------------------------------------------------------------------
// 配置：提交式应用
// ---------------------------------------------------------------------------

// applyConfigCanonical 在本地应用一份共享配置（canonical 已排除 cluster 块）。
//
// 它是 FSM 的 apply 路径，因此必须能被所有副本执行：config.Manager 会保留
// 本机的 cluster 块（见 config.Manager.Apply），reload 只重建差异设备。
func (r *Runtime) applyConfigCanonical(ctx context.Context, canonical []byte, source string) (config.ApplyResult, error) {
	var f config.File
	if err := json.Unmarshal(canonical, &f); err != nil {
		r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "error"})
		return config.ApplyResult{}, fmt.Errorf("config: parse replicated config: %w", err)
	}
	before := r.cfg.Current()
	beforeNB := northboundRestartFingerprint(before.Northbound, before.Storage)
	res, err := r.cfg.Apply(&f, source)
	if err != nil {
		r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "error"})
		return config.ApplyResult{}, err
	}
	if !res.Applied {
		return res, nil
	}
	res.Restarted = r.reload(before)
	r.metrics.Inc("uic_config_apply_total", obs.Labels{"status": "ok"})

	after := r.cfg.Current()
	afterNB := northboundRestartFingerprint(after.Northbound, after.Storage)
	if string(beforeNB) != string(afterNB) {
		r.log.Warn("northbound_config_changed", "detail", "MQTT/Webhook 参数变更需重启进程生效；OPC UA 北向已热重建")
	}
	// 历史库的目录与保留策略在 Open 时就固定了。采样参数可以热改，
	// 但这四项不能——必须明确告诉运维「改了但没生效」，否则他会盯着
	// 一个没变的水位以为配置没生效。
	if historyFingerprint(before.Storage.History) != historyFingerprint(after.Storage.History) {
		r.log.Warn("history_config_changed",
			"detail", "storage.history 的 dir/maxBytes/segmentBytes/maxAge 变更需重启进程生效；minInterval/heartbeat/deadband 已即时生效")
	}
	return res, nil
}

// rollbackCanonical 回滚到指定版本（FSM apply 路径）。
//
// reload 内部会重建凭据表与审计存储，因此这里不需要再单独 buildAuth——
// 重复重建只会多做一次，且两次之间的失败更难解释。
func (r *Runtime) rollbackCanonical(ctx context.Context, version string) (config.ApplyResult, error) {
	before := r.cfg.Current()
	res, err := r.cfg.Rollback(version)
	if err != nil {
		return config.ApplyResult{}, err
	}
	res.Restarted = r.reload(before)
	return res, nil
}

// ---------------------------------------------------------------------------
// 快照
// ---------------------------------------------------------------------------

type idemSnapshot struct {
	At      time.Time             `json:"at"`
	Results []httpapi.WriteResult `json:"results"`
}

type clusterSnapshot struct {
	Version  string                  `json:"version"`
	Config   json.RawMessage         `json:"config"`
	Idem     map[string]idemSnapshot `json:"idem,omitempty"`
	Acks     []ackRecord             `json:"acks,omitempty"`
	Commands []commandRecord         `json:"commands,omitempty"`
}

// Snapshot 实现 consensus.Applier：物化一份一致点上的完整状态机状态。
//
// 捕获在 cluMu 内完成（apply 路径也持这把锁），因此配置、幂等表与记录来自
// 同一个 apply 序列位置；raft 只负责把字节落盘。
func (r *Runtime) Snapshot() ([]byte, error) {
	r.cluMu.Lock()
	defer r.cluMu.Unlock()

	canonical, err := r.cfg.Current().Canonical()
	if err != nil {
		return nil, err
	}
	snap := clusterSnapshot{
		Version:  r.cfg.Version(),
		Config:   canonical,
		Acks:     append([]ackRecord(nil), r.clu.acks...),
		Commands: append([]commandRecord(nil), r.clu.commands...),
	}
	r.mu.Lock()
	if len(r.idem) > 0 {
		snap.Idem = make(map[string]idemSnapshot, len(r.idem))
		for k, e := range r.idem {
			snap.Idem[k] = idemSnapshot{At: e.at, Results: append([]httpapi.WriteResult(nil), e.results...)}
		}
	}
	r.mu.Unlock()
	return json.Marshal(snap)
}

// Restore 实现 consensus.Applier：从快照恢复状态机。
//
// 配置恢复走 applyConfigCanonical（幂等：版本相同则不重复应用），因此新副本
// 追上快照后，其配置与写下快照的副本一致；节点本地 cluster 块由 config.Manager
// 保留，不会被快照里的另一方身份覆盖。
func (r *Runtime) Restore(data []byte) error {
	r.cluMu.Lock()
	defer r.cluMu.Unlock()

	var snap clusterSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("runtime: decode snapshot: %w", err)
	}
	if len(snap.Config) == 0 {
		return errors.New("runtime: snapshot without config")
	}
	if _, err := r.applyConfigCanonical(context.Background(), snap.Config, "raft-snapshot"); err != nil {
		return fmt.Errorf("runtime: restore config: %w", err)
	}
	r.mu.Lock()
	r.idem = make(map[string]idemEntry, len(snap.Idem))
	for k, e := range snap.Idem {
		r.idem[k] = idemEntry{at: e.At, results: e.Results}
	}
	r.mu.Unlock()
	r.clu = clusterRecords{
		acks:     append([]ackRecord(nil), snap.Acks...),
		commands: append([]commandRecord(nil), snap.Commands...),
	}
	return nil
}

// ---------------------------------------------------------------------------
// 角色循环：采集开关、指标、成员对账
// ---------------------------------------------------------------------------

// clusterLoop 跟随角色切换采集开关，并暴露 role/term/commitIndex 指标。
func (r *Runtime) clusterLoop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	r.applyRole(r.isLeader(), time.Now())
	lastReconcile := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			leader := r.isLeader()
			r.applyRole(leader, now)
			r.refreshClusterMetrics()
			r.reapplyAckLedger()
			if leader && now.Sub(lastReconcile) >= 30*time.Second {
				r.reconcileMembership()
				lastReconcile = now
			}
		}
	}
}

// applyRole 在「是/不是 leader」切换时启用/停用采集。
//
// 为什么不是每个 tick 都改设备状态：SetEnabled(true) 会把 nextDue 拉到当下，
// 每个 tick 都调等于把采集周期变成 500ms。只在边沿动作。
func (r *Runtime) applyRole(leader bool, now time.Time) {
	prev := r.leaderActive.Swap(leader)
	if r.roleKnown.Swap(true) && prev == leader {
		return
	}
	if leader {
		r.log.Info("cluster_role", "role", "leader", "nodeId", r.nodeID)
	} else {
		// 丢主时把本机的灰度计划就地终止（见 abandonStagedOnDemotion）。
		r.abandonStagedOnDemotion()
		if r.consensus != nil {
			r.log.Info("cluster_role", "role", string(r.consensus.Role()),
				"leaderId", r.consensus.LeaderID(), "term", r.consensus.Term())
		}
	}
	r.setRoleSince(now)
	// 顺序很关键：
	//   - 升主：先恢复采集（状态迁移落到本地快照）再把 sink 置活，republish
	//     补发的是「刚恢复的」状态，而不是待命时的 disabled；
	//   - 降主：先把 sink 置死（优雅断开、不再发布），再停采集——否则停采产生
	//     的一串 disabled 会被当作站点级最终事实发到 Broker。
	if leader {
		r.setCollectionActive(true)
		r.setMqttSinkActive(true)
	} else {
		r.setMqttSinkActive(false)
		r.setCollectionActive(false)
	}
}

// setMqttSinkActive 把「站点级北向控制面发布权」交给/收回 leader。
// 未配置 MQTT 时为 no-op；Sparkplug 的 setActive 也是 no-op（节点身份带实例）。
func (r *Runtime) setMqttSinkActive(active bool) {
	if r.mqttSink != nil {
		r.mqttSink.setActive(active)
	}
}

// setCollectionActive 把本机设备切到「采集/待命」。
//
// 停用用 scheduler.SetEnabled(false) + 点位标 disabled：它是显式的「本副本
// 不采集」，而不是让点位保持最后一次好值假装新鲜。启用只恢复配置里本来就
// enabled 的设备（配置停用的仍标 disabled）。
func (r *Runtime) setCollectionActive(active bool) {
	f := r.cfg.Current()
	for _, dev := range f.Devices {
		if active && dev.Enabled {
			if err := r.sched.SetEnabled(dev.ID, true); err != nil {
				r.log.Warn("cluster_enable_device_failed", "device", dev.ID, "error", errText(err))
			}
			continue
		}
		if err := r.sched.SetEnabled(dev.ID, false); err != nil {
			r.log.Warn("cluster_disable_device_failed", "device", dev.ID, "error", errText(err))
		}
		r.markStandby(dev)
	}
}

// markStandby 把待命副本的点位显式标为 disabled（区别于「配置停用」）。
func (r *Runtime) markStandby(dev model.Device) {
	now := time.Now().UTC()
	for _, p := range dev.Points {
		if !p.Enabled {
			continue
		}
		r.cache.Set(model.Sample{
			DeviceID: dev.ID, PointID: p.ID, Quality: model.QualityDisabled,
			Value:       model.Value{Type: p.DataType},
			CollectedAt: now, Error: "standby: this instance is not the cluster leader",
		})
	}
}

// refreshClusterMetrics 写入角色与复制水位指标。
func (r *Runtime) refreshClusterMetrics() {
	if r.consensus == nil {
		return
	}
	role := r.consensus.Role()
	for _, want := range []consensus.Role{consensus.RoleLeader, consensus.RoleFollower, consensus.RoleCandidate} {
		v := 0.0
		if role == want {
			v = 1
		}
		r.metrics.Set("uic_leader_role", v, obs.Labels{"role": string(want)})
	}
	r.metrics.Set("uic_raft_term", float64(r.consensus.Term()), nil)
	r.metrics.Set("uic_raft_commit_index", float64(r.consensus.CommitIndex()), nil)
}

// reconcileMembership 让 raft 成员表与配置里的期望成员表对齐（只增不删）。
//
// 只增不删是刻意的：自动删除会把「配置少写了一行」变成一次不可逆的缩容，
// 而缩容是降低容错度的动作，必须由人确认。多余的成员打 Warning 让运维处理。
func (r *Runtime) reconcileMembership() {
	if r.consensus == nil || r.clusterCfg == nil || !r.isLeader() {
		return
	}
	have := map[string]consensus.Server{}
	for _, s := range r.consensus.Servers() {
		have[s.ID] = s
	}
	desired := map[string]bool{}
	for _, p := range r.clusterCfg.Peers {
		desired[p.ID] = true
		s, ok := have[p.ID]
		if ok {
			if s.Addr != p.Addr {
				r.log.Warn("cluster_peer_addr_mismatch", "nodeId", p.ID,
					"have", s.Addr, "want", p.Addr,
					"detail", "raft 成员地址与配置不一致；改地址需要走 RemoveServer/AddServer，本进程不会自动迁移")
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := r.consensus.AddServer(ctx, p.ID, p.Addr, 0)
		cancel()
		if err != nil {
			r.log.Warn("cluster_add_server_failed", "nodeId", p.ID, "addr", p.Addr, "error", errText(err))
			continue
		}
		r.metrics.Inc("uic_raft_membership_changes_total", obs.Labels{"op": "add"})
		r.log.Info("cluster_member_added", "nodeId", p.ID, "addr", p.Addr)
	}
	for id := range have {
		if !desired[id] {
			r.log.Warn("cluster_member_not_in_config", "nodeId", id,
				"detail", "成员表里有配置中不存在的节点；缩容需人工执行 RemoveServer，进程不会自动删除")
		}
	}
}

// Leader 实现 httpapi.Backend：本副本的集群角色视图。
func (r *Runtime) Leader() httpapi.LeaderInfo {
	info := httpapi.LeaderInfo{
		Enabled:    r.consensus != nil,
		Role:       "leader",
		InstanceID: r.instanceID,
		Healthy:    true,
		Since:      r.startedAt,
	}
	if r.consensus == nil {
		return info
	}
	info.Role = string(r.consensus.Role())
	info.Healthy = r.consensus.Healthy()
	info.Term = r.consensus.Term()
	info.LeaderID = r.consensus.LeaderID()
	info.LeaderAddr = r.consensus.LeaderAddr()
	info.CommitIndex = r.consensus.CommitIndex()
	if since := r.roleSinceTime(); !since.IsZero() {
		info.Since = since
	}
	servers := r.consensus.Servers()
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	for _, s := range servers {
		info.Peers = append(info.Peers, httpapi.PeerInfo{
			ID: s.ID, Addr: s.Addr, Voter: s.Suffrage == consensus.SuffrageVoter,
		})
	}
	return info
}
