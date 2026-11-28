// Package consensus 提供跨实例的强一致协调（leader 选举 + 控制面状态机复制）。
//
// 为什么需要它：同站点跑两个 UIC 实例是常见部署（双机热备、灰度期新旧进程并存、
// 一台主机多容器）。`instanceid` 解决了「两个实例 MQTT clientID 互相顶掉」的
// 身份消歧问题，但那只是让它们「可区分」——没有解决「同一时刻谁该采集、谁该写」。
// 网络分区下两侧各自自认 leader 并都接受写，会在分区愈合后留下无法检测的分歧。
// 这类问题单机文件锁（flock）结构上解决不了：flock 只能守同主机，跨主机无效。
//
// 解决办法是用 raft 把「控制面状态」在集群内做成复制状态机：集群里有且仅有一个
// leader，所有写操作都序列化成日志条目、经多数派复制并 commit 后，每个副本按
// 相同顺序 apply 到各自的状态机。网络分区无法产生双 leader（少数派选不出主），
// 从根上消除脑裂。
//
// 复制什么、不复制什么（这是边界，也是共识收益的来源）：
//   - 复制：配置应用/回滚、点位写命令、控制命令、报警确认、集群成员变更——
//     即任何「必须全集群一致裁决」的控制面状态。这些低频、分歧即事故，共识收益最大。
//   - 不复制：采集样本（时序数据）。它高频、可重放、可丢可重，走 Outbox
//     at-least-once 即可，塞进共识只会拖垮吞吐、毫无收益。
//
// 因此本包叫 consensus（共识）而不是 storage：它复制的是「裁决」，不是「数据」。
//
// 与零第三方依赖的关系：见 ADR-0004（其零依赖约束由 ADR-0005 正式修订）与
// ADR-0005。raft 用 hashicorp/raft（纯 Go、零 CGO）。依赖面不再是「按构造为空」，
// 而是「有限、可枚举、vendor 固化、纳入 SBOM/CI 审计」。
package consensus

import "errors"

// ErrNotLeader 表示本副本当前不是 leader，无法提交写操作。
//
// 调用方（北向写端点）据此返回 409 并附带 leader 的网络地址，让客户端重定向。
// 这是 fail-closed：无法确认自己有权提交时，宁可拒绝也不冒充 leader。
var ErrNotLeader = errors.New("consensus: not leader")

// ErrNoQuorum 表示集群尚未形成多数派（节点数不足、分区隔离、全部刚启动）。
//
// 提交会超时并返回本错误而非静默成功——「静默吞掉一个未确认的写」是缺陷，
// 宁可显式报错让调用方重试或告警。
var ErrNoQuorum = errors.New("consensus: no quorum")

// ErrNotVoter 表示本副本在集群配置里既不是 leader 也不是投票成员（observer）。
//
// 预留：当前集群所有成员都是投票者，本错误暂不触发，但保留语义以便未来
// 引入「只读 observer 副本」时不破坏 API 契约。
var ErrNotVoter = errors.New("consensus: not a voter")

// ErrClosed 表示 Consensus 已关闭，不再接受 Propose。
var ErrClosed = errors.New("consensus: closed")

// ErrUnhealthy 表示本副本状态机已与日志分歧（apply/decode 失败过），不再接受
// 会改变状态的写。这是 fail-closed：宁可拒绝，也不把另一个版本的状态写进去。
var ErrUnhealthy = errors.New("consensus: state machine unhealthy")

// Role 是本副本在集群里的实时角色。
//
// 取值与 raft 术语一致：leader 是唯一可提交写、可发起采集的副本；follower 复制
// 日志、apply 状态机、但不接受写；candidate 是选举中间态，转瞬即逝，对外按
// follower 处理（不接受写）。
type Role string

const (
	// RoleLeader：集群唯一主，可 Propose 写、可发起采集/投递。
	RoleLeader Role = "leader"
	// RoleFollower：从主复制日志、按序 apply，但不接受写。
	RoleFollower Role = "follower"
	// RoleCandidate：选举中间态。短暂存在，对外等同于 follower（不写）。
	RoleCandidate Role = "candidate"
	// RoleUnknown：刚启动尚未加入集群，或 Consensus 已关闭。对外按「不可写」处理。
	RoleUnknown Role = "unknown"
)

// Writable 报告该角色是否可接受写操作（Propose）。
//
// 只有 leader 可写；candidate/unknown/follower 一律不可写。这是 leader 闸的
// 唯一判据，集中在这里避免各调用点各自判断导致语义漂移。
func (r Role) Writable() bool { return r == RoleLeader }

// Command 是一条 raft 日志的载荷，即「一次控制面裁决」的最小单元。
//
// 它是 FSM.apply 的唯一输入，必须可序列化、可确定性重放：同样的 Command 序列，
// 在任何副本上 apply 都必须得到同样的状态机结果（否则副本间分歧）。因此
// Command 里不放时间戳、随机数等不可重放的东西——需要「何时发生」时用
// 提交时由 leader 单调分配的序号（Index），而不是各副本各自的墙钟。
type Command struct {
	// Kind 决定 apply 的分派。用字符串而非整型常量，便于日志可读与跨版本演进。
	Kind Kind
	// InstanceID 是发起方实例的稳定身份（来自 instanceid）。用于审计与
	// 「谁提交的」追溯，不参与一致性裁决（裁决只看 Kind+Payload）。
	InstanceID string
	// Payload 是 Kind 对应的具体参数，已序列化为可确定性 apply 的形式。
	// 各 Kind 的 apply 语义见 fsm.go 的 applyConfig/applyWrite 等。
	Payload []byte
}

// Kind 是 Command 的种类。
type Kind string

const (
	// KindApplyConfig：应用一份新配置（payload 为规范化后的配置内容）。
	// apply 即调用 config.Manager.Apply——配置版本天然进 raft 日志。
	KindApplyConfig Kind = "apply_config"
	// KindRollbackConfig：回滚到某配置版本（payload 为版本 ID）。
	KindRollbackConfig Kind = "rollback_config"
	// KindWritePoints：点位写命令（payload 为 device+items 的幂等意图）。
	// 注意复制的是「写意图与幂等结果」，不是采集值。
	KindWritePoints Kind = "write_points"
	// KindCommand：控制命令（重启设备、模式切换等，payload 为 action+params）。
	KindCommand Kind = "command"
	// KindAckAlarms：报警确认（payload 为要确认的 alarm IDs）。
	KindAckAlarms Kind = "ack_alarms"
)

// 所有 Kind 的集合，用于校验与测试遍历。
var allKinds = []Kind{
	KindApplyConfig, KindRollbackConfig, KindWritePoints,
	KindCommand, KindAckAlarms,
}

// Valid 报告 Kind 是否为已知种类。
func (k Kind) Valid() bool {
	for _, known := range allKinds {
		if k == known {
			return true
		}
	}
	return false
}
