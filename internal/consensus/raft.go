package consensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// raftConsensus 是 Consensus 的 hashicorp/raft 实现。
//
// 生命周期：构造 → 加入/启动 raft → 运行中 Propose 提交命令 → Role/leader 查询
// → Shutdown 释放。所有 raft 交互都经 r.r（*raft.Raft）这一个对象，状态映射
// 到本包的 Role 语义。
type raftConsensus struct {
	r   *raft.Raft
	fsm *StateMachine
	// backend 是 raft 底层（存储/传输），Shutdown 时据此释放资源。
	backend *raftBackend

	// commitTimeout 是 Propose 里 ApplyLog 的入队等待上限（不是 raft 的
	// Config.CommitTimeout——后者是 commit 传播静默期，保持 raft 默认）。
	commitTimeout time.Duration

	// mu 保护 leaderView（LeaderID/LeaderAddr/roleCache 的最近观测值）。raft 的
	// LeaderWithID 本身并发安全，但本包在 Role() 里要做「state+leader」的一致
	// 快照，需要这把锁把两者一起钉住。
	mu         sync.Mutex
	leaderID   string
	leaderAddr string
	roleCache  Role

	done     chan struct{}
	closeOne sync.Once
}

// New 构造并启动一个 Consensus（生产与测试共用的唯一入口）。
//
// applier 是状态机副作用实现（runtime 注入）；opts 见 Options。返回的
// Consensus 已开始参与选主/复制：单节点 Bootstrap 集群立即成为 leader。
func New(opts Options, applier Applier) (Consensus, error) {
	return newRaftConsensus(opts, applier)
}

// newRaftConsensus 构造并启动一个 raftConsensus。
//
// applier 是状态机的副作用实现（runtime 注入）；opts 见 Options。
//
// bootstrap 由 opts.Bootstrap 显式决定，不做成员数推断：单节点部署置 true
// 即 always-leader；多节点仅种子节点置 true（Peers 只含自身），其余节点经
// raft 原生 AddServer 逐个加入（见 ADR-0005 §3.3）。
func newRaftConsensus(opts Options, applier Applier) (*raftConsensus, error) {
	if opts.NodeID == "" {
		return nil, errors.New("consensus: NodeID is required")
	}
	if opts.Addr == "" {
		return nil, errors.New("consensus: Addr is required")
	}
	commitTimeout := opts.CommitTimeout
	if commitTimeout <= 0 {
		commitTimeout = 5 * time.Second
	}

	c := &raftConsensus{
		commitTimeout: commitTimeout,
		fsm:           NewStateMachine(applier),
		done:          make(chan struct{}),
	}

	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(opts.NodeID)
	// 注意：**不要**把 opts.CommitTimeout 写进 conf.CommitTimeout。
	//
	// raft 的 Config.CommitTimeout 不是「写超时」，而是「leader 在无新日志时，
	// 隔多久补发一次带当前 commitIndex 的 AppendEntries，让 follower 尽快学到
	// 已提交进度」的静默期（默认 50ms，随机抖动最多 2x）。把它设成秒级会让
	// 「leader 已 commit、follower 的 commitIndex 数秒不动」——实测 3s 配置下
	// 副本要 ~3.2s 才 apply，正是这个原因。这里保留 raft 默认 50ms。
	//
	// opts.CommitTimeout 只用于 Propose 里 ApplyLog 的入队等待（见下）。
	// 时间参数：<=0 用 raft 默认，>0 用调用方指定（测试用小值把提交/心跳压到
	// 毫秒级，消除新集群追赶窗口带来的数秒延迟；生产保持默认）。
	if opts.HeartbeatTimeout > 0 {
		conf.HeartbeatTimeout = opts.HeartbeatTimeout
	}
	if opts.ElectionTimeout > 0 {
		conf.ElectionTimeout = opts.ElectionTimeout
	}
	if opts.LeaderLeaseTimeout > 0 {
		conf.LeaderLeaseTimeout = opts.LeaderLeaseTimeout
	}
	// raft 的 hclog 日志默认写 stderr 且 DEBUG 很吵；由调用方决定输出目标。
	// 为空时静默（测试/未配置日志），runtime 接线时传入真实 writer。
	if opts.LogOutput != nil {
		conf.LogOutput = opts.LogOutput
	} else {
		conf.LogOutput = io.Discard
	}
	if opts.LogLevel != "" {
		// 非法级别会被 hclog 解释成 NoLevel（静默），这里先本地校验，让
		// 配置错误在构造期暴露而不是「日志突然全没了」。
		switch strings.ToLower(opts.LogLevel) {
		case "trace", "debug", "info", "warn", "error":
			conf.LogLevel = strings.ToLower(opts.LogLevel)
		default:
			return nil, fmt.Errorf("consensus: invalid LogLevel %q", opts.LogLevel)
		}
	}

	// 解析底层存储与传输：测试注入内存，生产用 bbolt + TCP。
	backend, err := c.resolveBackend(opts)
	if err != nil {
		return nil, fmt.Errorf("consensus: backend: %w", err)
	}
	c.backend = backend

	r, err := raft.NewRaft(conf, c.fsm, backend.logs, backend.stable, backend.snaps, backend.trans)
	if err != nil {
		if backend.cleanup != nil {
			_ = backend.cleanup()
		}
		return nil, fmt.Errorf("consensus: raft.NewRaft: %w", err)
	}
	c.r = r

	// bootstrap 集群：显式由 Options.Bootstrap 控制（不做推断，见字段注释）。
	// 种子节点以自己的初始配置（通常只含自身）引导后立即成为 leader；其余节点
	// 不写初始配置，等待 leader 经 AddServer 加入。重启后即使重复 Bootstrap，
	// raft 会返回可安全忽略的 ErrCantBootstrap（本地已有状态）。
	if opts.Bootstrap {
		peers := opts.Peers
		if len(peers) == 0 {
			peers = []Server{{ID: opts.NodeID, Addr: opts.Addr}}
		}
		if err := r.BootstrapCluster(raft.Configuration{Servers: toRaftServers(peers)}).Error(); err != nil &&
			!errors.Is(err, raft.ErrCantBootstrap) {
			// ErrCantBootstrap 表示本地已有状态（重启），属正常；其它为真故障。
			if backend.cleanup != nil {
				_ = backend.cleanup()
			}
			_ = r.Shutdown()
			return nil, fmt.Errorf("consensus: bootstrap: %w", err)
		}
	}

	// 启动后立即刷新一次 leader 视图，避免首轮 Role() 返回陈旧 unknown。
	c.refreshLeaderView()
	return c, nil
}

// resolveBackend 根据 Options 选择底层存储/传输：测试用内存，生产用 bbolt+TCP。
//
// 生产路径：bbolt 存日志 + 稳定数据（自写，单依赖），文件存快照（raft 内置），
// TCP transport 走节点间复制。cleanup 负责在 Shutdown 时关闭 bbolt 与 transport。
func (c *raftConsensus) resolveBackend(opts Options) (*raftBackend, error) {
	if opts.TestBackend != nil {
		return opts.TestBackend, nil
	}
	if opts.DataDir == "" {
		return nil, errors.New("consensus: DataDir is required when TestBackend is nil")
	}
	raftDir := filepath.Join(opts.DataDir, "raft")
	if err := os.MkdirAll(raftDir, 0o750); err != nil {
		return nil, fmt.Errorf("consensus: mkdir raft dir: %w", err)
	}
	store, err := newBboltStore(filepath.Join(raftDir, "raft.db"))
	if err != nil {
		return nil, err
	}
	snaps, err := raft.NewFileSnapshotStore(raftDir, 2, io.Discard)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("consensus: file snapshot store: %w", err)
	}
	// advertise=nil 表示用 bindAddr 自身作为对外地址；maxPool=2、timeout=10s。
	trans, err := raft.NewTCPTransport(opts.Addr, nil, 2, 10*time.Second, io.Discard)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("consensus: tcp transport: %w", err)
	}
	return &raftBackend{
		logs:   store,
		stable: store,
		snaps:  snaps,
		trans:  trans,
		// Shutdown 时关闭 transport 与 bbolt。
		cleanup: func() error {
			terr := trans.Close()
			serr := store.Close()
			if terr != nil {
				return terr
			}
			return serr
		},
	}, nil
}

// Propose 实现 Consensus：提交一条命令，等多数派复制并 commit。
func (c *raftConsensus) Propose(ctx context.Context, cmd Command) (any, error) {
	if c.r == nil {
		return nil, ErrClosed
	}
	// 只有 leader 能提交：先判角色，非 leader 立即 ErrNotLeader（fail-closed）。
	// 这一步也避免了 follower 把命令发给 raft 后白白等待超时。
	if !c.IsLeader() {
		return nil, ErrNotLeader
	}
	// 状态机已分歧（apply 失败过）的副本不能再提交：它已经无法保证 apply
	// 结果与集群一致，继续写只会扩大分歧。这是 fail-closed。
	if !c.Healthy() {
		return nil, ErrUnhealthy
	}

	future := c.r.ApplyLog(raft.Log{Data: encodeCommand(cmd)}, c.commitTimeout)
	// ctx 取消也应当取消等待：用一个 goroutine 把 ctx.Done 转成 future 完成。
	// 但 raft.Apply 的 future 不接受外部取消，因此我们手动 select ctx.Done。
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := future.Error(); err != nil {
		// raft 层错误：失去 leadership（ErrLeadershipLost）、已关闭、超时。
		switch {
		case errors.Is(err, raft.ErrLeadershipLost):
			return nil, ErrNotLeader
		case errors.Is(err, raft.ErrRaftShutdown):
			return nil, ErrClosed
		default:
			// 超时（未能在 commitTimeout 内复制）归为 ErrNoQuorum。
			return nil, fmt.Errorf("%w: %v", ErrNoQuorum, err)
		}
	}
	// 关键：FSM.Apply 的错误由 future.Response() 返回，不是 future.Error()。
	// raft 文档明确要求检查 Response。FSM apply 失败即副本不健康。
	if resp := future.Response(); resp != nil {
		if err, ok := resp.(error); ok && err != nil {
			return nil, err
		}
		return resp, nil
	}
	return nil, nil
}

// Role 实现 Consensus：映射 raft.State() 到本包 Role。
func (c *raftConsensus) Role() Role {
	c.refreshLeaderView()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roleCache
}

// refreshLeaderView 刷新 leader 观测值与角色缓存。无锁，调用方负责后续加锁。
func (c *raftConsensus) refreshLeaderView() {
	state := c.r.State()
	c.mu.Lock()
	switch state {
	case raft.Leader:
		addr, id := c.r.LeaderWithID()
		c.leaderID = string(id)
		c.leaderAddr = string(addr)
		c.roleCache = RoleLeader
	case raft.Candidate:
		c.roleCache = RoleCandidate
	case raft.Follower:
		c.roleCache = RoleFollower
	case raft.Shutdown:
		c.roleCache = RoleUnknown
	default:
		c.roleCache = RoleUnknown
	}
	c.mu.Unlock()
}

// IsLeader 实现 Consensus。
func (c *raftConsensus) IsLeader() bool { return c.Role() == RoleLeader }

// Healthy 实现 Consensus：状态机无分歧且 raft 未关闭才算健康。
func (c *raftConsensus) Healthy() bool {
	if c.r == nil || c.fsm == nil {
		return false
	}
	if c.r.State() == raft.Shutdown {
		return false
	}
	return c.fsm.IsHealthy()
}

// LeaderID 实现 Consensus。
func (c *raftConsensus) LeaderID() string {
	c.refreshLeaderView()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaderID
}

// LeaderAddr 实现 Consensus。
func (c *raftConsensus) LeaderAddr() string {
	c.refreshLeaderView()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaderAddr
}

// CommitIndex 实现 Consensus。
func (c *raftConsensus) CommitIndex() uint64 { return c.r.CommitIndex() }

// Term 实现 Consensus：raft 直接暴露 CurrentTerm。
func (c *raftConsensus) Term() uint64 {
	if c.r == nil {
		return 0
	}
	return c.r.CurrentTerm()
}

// AddServer 实现 Consensus：经 raft 日志在线增成员（joint consensus）。
func (c *raftConsensus) AddServer(ctx context.Context, id, addr string, prevIndex uint64) error {
	if c.r == nil {
		return ErrClosed
	}
	if !c.IsLeader() {
		return ErrNotLeader
	}
	future := c.r.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), prevIndex, c.commitTimeout)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := future.Error(); err != nil {
		switch {
		case errors.Is(err, raft.ErrLeadershipLost):
			return ErrNotLeader
		case errors.Is(err, raft.ErrRaftShutdown):
			return ErrClosed
		default:
			return fmt.Errorf("%w: %v", ErrNoQuorum, err)
		}
	}
	return nil
}

// RemoveServer 实现 Consensus：经 raft 日志在线删成员。
func (c *raftConsensus) RemoveServer(ctx context.Context, id string, prevIndex uint64) error {
	if c.r == nil {
		return ErrClosed
	}
	if !c.IsLeader() {
		return ErrNotLeader
	}
	future := c.r.RemoveServer(raft.ServerID(id), prevIndex, c.commitTimeout)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := future.Error(); err != nil {
		switch {
		case errors.Is(err, raft.ErrLeadershipLost):
			return ErrNotLeader
		case errors.Is(err, raft.ErrRaftShutdown):
			return ErrClosed
		default:
			return fmt.Errorf("%w: %v", ErrNoQuorum, err)
		}
	}
	return nil
}

// Servers 实现 Consensus：返回当前配置里的成员视图。
func (c *raftConsensus) Servers() []Server {
	cfgFuture := c.r.GetConfiguration()
	if err := cfgFuture.Error(); err != nil {
		return nil
	}
	cfg := cfgFuture.Configuration()
	out := make([]Server, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		out = append(out, Server{
			ID:       string(s.ID),
			Addr:     string(s.Address),
			Suffrage: suffrageFromRaft(s.Suffrage),
		})
	}
	return out
}

// Shutdown 实现 Consensus：关闭 raft 并释放资源。幂等。
func (c *raftConsensus) Shutdown() error {
	c.closeOne.Do(func() {
		close(c.done)
	})
	if c.r == nil {
		return nil
	}
	err := c.r.Shutdown().Error()
	// raft 不负责关闭我们注入的存储/传输，由 backend.cleanup 释放
	// （生产路径关 bbolt + TCP transport）。测试路径的 cleanup 可能为 nil。
	if c.backend != nil && c.backend.cleanup != nil {
		if cerr := c.backend.cleanup(); err == nil {
			err = cerr
		}
	}
	return err
}

// Done 实现 Consensus。
func (c *raftConsensus) Done() <-chan struct{} { return c.done }

// toRaftServers 把本包 Server 转成 raft.Server。
func toRaftServers(peers []Server) []raft.Server {
	out := make([]raft.Server, 0, len(peers))
	for _, p := range peers {
		out = append(out, raft.Server{
			ID:       raft.ServerID(p.ID),
			Address:  raft.ServerAddress(p.Addr),
			Suffrage: raft.Voter,
		})
	}
	return out
}

// suffrageFromRaft 把 raft.ServerSuffrage 映射为本包 Suffrage。
func suffrageFromRaft(s raft.ServerSuffrage) Suffrage {
	if s == raft.Voter {
		return SuffrageVoter
	}
	return SuffrageNonvoter
}
