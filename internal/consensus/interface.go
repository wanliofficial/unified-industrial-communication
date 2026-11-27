package consensus

import (
	"context"
	"io"
	"time"
)

// Consensus 是跨实例强一致协调的门面：leader 选举 + 控制面日志复制。
//
// 它刻意窄：只有「提交一条控制面命令」和「查询角色/成员」两类操作。读样本、
// 指标、健康检查都不经过它（那些是本地观测，不需共识）。窄接口让 runtime
// 只依赖「提交裁决 + 知道谁是 leader」，也让这个包可以用内存 transport 单测。
//
// 实现：单节点集群恒为 leader（always-leader）；多节点集群由 raft 选主，
// 写经多数派复制。关闭（Enabled=false）时退化为单节点，零外部依赖。
type Consensus interface {
	// Propose 提交一条控制面命令，等多数派复制并 commit 后返回 apply 结果。
	//
	// 语义（fail-closed）：
	//   - 本副本不是 leader：返回 ErrNotLeader（调用方据此 409 + 重定向）。
	//   - 集群未形成多数派/超时：返回 ErrNoQuorum，绝不返回「成功」。
	//   - FSM apply 失败：返回该错误（副本已判不健康，后续 Propose 也会失败）。
	//   - Consensus 已关闭：返回 ErrClosed。
	Propose(ctx context.Context, cmd Command) (any, error)

	// Role 返回本副本的实时角色。轮询/订阅由调用方按需做，此方法无副作用。
	Role() Role

	// IsLeader 是 Role()==RoleLeader 的便捷判断（采集/写 leader 闸用）。
	IsLeader() bool

	// LeaderID 返回当前 leader 的实例身份；未知（刚启动/选主中）返回空串。
	LeaderID() string

	// LeaderAddr 返回当前 leader 的网络地址（raft transport 地址），供
	// 非 leader 副本在 409 响应里附重定向地址。无则空串。
	LeaderAddr() string

	// CommitIndex 返回已 commit 的日志索引（复制进度，供 /api/v1/leader 暴露）。
	CommitIndex() uint64

	// Term 返回当前任期（供 /api/v1/leader 与指标暴露；term 变化即发生过选主）。
	Term() uint64

	// 成员变更走 raft 原生机制：AddServer 增、RemoveServer 删。它们经 raft 日志
	// 复制（joint consensus），避免在变更期脑裂；不另走 FSM 命令（避免与 raft
	// 自身的成员表管理重复）。
	AddServer(ctx context.Context, id, addr string, prevIndex uint64) error
	RemoveServer(ctx context.Context, id string, prevIndex uint64) error

	// Servers 返回当前集群成员视图（含角色/是否投票者）。
	Servers() []Server

	// Shutdown 关闭 consensus（停 raft、释放存储）。幂等。
	Shutdown() error

	// Done 返回关闭后触发的 channel（runtime 生命周期管理用）。
	Done() <-chan struct{}
}

// Server 是集群成员视图里的一台节点。
type Server struct {
	// ID 是实例身份（来自 instanceid，即 raft 的 ServerID）。
	ID string
	// Addr 是 raft transport 的网络地址（host:port）。
	Addr string
	// Suffrage：voter=有投票权，nonvoter=只读复制副本。当前全为 voter。
	Suffrage Suffrage
}

// Suffrage 是成员的投票权。
type Suffrage string

const (
	// SuffrageVoter：有投票权，参与选主与提交。
	SuffrageVoter Suffrage = "voter"
	// SuffrageNonvoter：无投票权，只复制日志（预留，当前不用）。
	SuffrageNonvoter Suffrage = "nonvoter"
)

// Options 是构造 Consensus 的参数。
type Options struct {
	// NodeID 是本节点身份（来自 instanceid）。不可为空。
	NodeID string
	// Addr 是本节点 raft transport 地址（host:port）。单节点集群也需可路由形式，
	// 但单机不回环，可用 "node-<id>" 占位。
	Addr string
	// Peers 是全部成员（含本节点）。空或仅 1 个成员 → 单节点 always-leader。
	// >=3 才启用多副本；2 个或更少（且 >1）应被上层拒绝（无多数派意义）。
	Peers []Server
	// DataDir 是 raft 存储（日志/快照/稳定存储）的根目录。空用临时目录（测试）。
	DataDir string
	// CommitTimeout 是 Propose 调用 raft.ApplyLog 时的入队/提交等待上限。<=0 用 5s。
	//
	// 注意：它**不**写入 raft 的 Config.CommitTimeout（那个是「leader 补发 commit
	// 进度的静默期」，默认 50ms，必须保持小值，否则 follower 学到 commit 会慢到
	// 秒级）。这里只是「Propose 最多等多久」。控制面写本就低频，5s 足够。
	CommitTimeout time.Duration
	// LogOutput 是 raft 自身日志（hclog）的去处。nil 时静默（测试/未配置）。
	// runtime 接线时传真实 writer（如 os.Stderr 或 slog 适配）。
	LogOutput io.Writer
	// TestBackend 注入 raft 底层（日志/稳定/快照存储 + transport）。
	// 生产为 nil（由 RaftConsensus 按 DataDir 用 boltdb + TCP transport 自建）；
	// 测试传入内存实现（同进程多节点、确定性、无网络）。见 backend_mem.go。
	TestBackend *raftBackend

	// Bootstrap 控制本节点是否在启动时写入初始集群配置（BootstrapCluster）。
	//
	// 语义刻意显式、不做推断：
	//   - 单节点部署（cluster 未启用）：必须为 true，启动即 always-leader。
	//   - 多节点集群：**有且只有一台**（种子节点）为 true，且其 Peers 应只含
	//     自身；其余节点为 false，由种子节点经 AddServer 逐个加入。
	//
	// 为什么不推断（如按 peers[0].ID）：推断依赖「所有节点拿到同一份 peers 且
	// 顺序一致」，一旦配置不一致就会「都当种子」或「都不当」，前者分裂、后者
	// 永不选主，都是静默故障。显式开关把责任交给配置，且可被测试精确控制。
	Bootstrap bool

	// HeartbeatTimeout / ElectionTimeout / LeaderLeaseTimeout 是 raft 的时间参数。
	// <=0 时用 raft 默认（1s/1s/500ms，Consul/Vault 生产验证过的值）。
	//
	// 为什么暴露它们：默认心跳 1s 意味着「一次写提交后，从 leader 的 commit
	// 前进到 follower 学到新 commit」最多要等一个心跳周期；在新集群刚选主、
	// 日志与成员表还在追赶的窗口里，这个延迟会被放大到数秒（实测默认配置下
	// 三节点从提交到全部 apply 约 4.7s）。生产可接受（控制面写本就低频），但
	// 确定性测试必须把它压到毫秒级——raft 官方集成测试也用 50ms 一档。测试
	// 通过 Options 传入小值，生产保持默认。
	HeartbeatTimeout   time.Duration
	ElectionTimeout    time.Duration
	LeaderLeaseTimeout time.Duration
}
