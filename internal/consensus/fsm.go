package consensus

import (
	"io"

	"github.com/hashicorp/raft"
)

// Applier 把一条已提交的 Command 应用到本副本的状态机，并管理状态机快照。
//
// 为什么用回调而不是直接 import config/alarm：consensus 不能依赖 runtime/config
// （依赖方向会倒过来，且让 consensus 无法单测）。由 runtime 实现 Applier 并注入
// ——这样 consensus 只负责「把日志按序、确定地喂给 Applier」，副作用归 runtime。
// / 这也让 FSM 可以在内存里用假 Applier 单测，不碰任何真实配置/报警。
//
// Applier 直接采用与 raft.FSM 相同的形状（Apply/Snapshot/Restore），因此
// StateMachine 只是它的薄桥。runtime 实现 Applier 时就是「把 raft 接到
// config.Manager / alarm / 命令处理器」，编码格式由 runtime 自定，consensus 不管。
type Applier interface {
	// ApplyCommand 执行一条已提交的命令，返回其结果（会作为 ApplyFuture.Response
	// 回传给 Propose 的调用方）。
	//
	// 契约（raft 对 FSM 的要求）：
	//   - 确定性：同一条已提交日志序列，在所有副本 apply 结果必须一致。
	//     实现里禁止用墙钟/随机数/无序 map 遍历等不可重放来源决定状态。
	//   - 串行：ApplyCommand 由 raft 单循环串行调用，无需自行加锁 apply 路径。
	//   - 可失败但不可分歧：失败时返回 error，本副本据此判不健康、拒绝后续写
	//     （fail-closed），绝不「失败也照常推进」。
	ApplyCommand(cmd Command) (any, error)

	// Snapshot 返回当前状态机的可持久化快照（实现 raft.FSMSnapshot）。
	// raft 用它做日志压缩与新副本追赶。快照必须表达「截至某索引、已 apply
	// 全部 ≤该索引日志」的完整状态。
	Snapshot() (raft.FSMSnapshot, error)

	// Restore 从一个快照流恢复状态。raft 在新副本启动、或日志被压缩后追不上时
	// 调用。恢复后本副本状态必须与写下该快照的副本一致。
	Restore(snapshot io.ReadCloser) error
}

// StateMachine 是 raft.FSM 的实现：raft commit 一条日志就调用一次 Apply。
//
// 它本身不含业务状态——业务状态在 runtime 注入的 Applier 里。StateMachine 只
// 承担「桥接 raft 日志 ↔ Applier」与「分歧标记」两件事，刻意做薄，让确定性
// 责任落在 Applier 上、可测性落在内存单测上。
type StateMachine struct {
	applier Applier

	// lastApplied 是最近一条成功 apply 的日志索引（raft 的 Log.Index）。
	// 仅用于诊断与水位线，不参与裁决。
	lastApplied uint64

	// applyErr 记录最近一次 apply/decode 的错误。非 nil 表示「本副本状态机已
	// 与应用它的日志不一致」，副本必须 fail-closed（拒绝服务写）。这是 raft
	// 路线相对软选主的关键差异：分歧不是「容忍」，是「停机告警」。
	applyErr error
}

// NewStateMachine 构造一个由 applier 驱动的状态机。
func NewStateMachine(applier Applier) *StateMachine {
	return &StateMachine{applier: applier}
}

// Apply 实现 raft.FSM。raft 在一条日志被多数派 commit 后调用它。
//
// 把 raft.Log 还原成 Command 交给 Applier。任何错误都记入 applyErr 并返回给
// raft（作为 ApplyFuture.Response）——raft 不因 apply 错误停止复制，但本副本
// 已与应用它的日志产生分歧，后续 IsHealthy() 会据此拒绝服务（fail-closed）。
func (s *StateMachine) Apply(log *raft.Log) interface{} {
	cmd, err := decodeCommand(log.Data)
	if err != nil {
		// 解码失败意味着这条日志在集群里就是坏的——无法 apply、无法跳过
		// （跳过=分歧）。记为不可恢复，交由上层告警。
		s.applyErr = err
		return err
	}
	if !cmd.Kind.Valid() {
		s.applyErr = ErrBadCommand
		return ErrBadCommand
	}
	res, err := s.applier.ApplyCommand(cmd)
	if err != nil {
		s.applyErr = err
		return err
	}
	s.lastApplied = log.Index
	return res
}

// Snapshot 实现 raft.FSM。直接委托 Applier.Snapshot。
func (s *StateMachine) Snapshot() (raft.FSMSnapshot, error) {
	return s.applier.Snapshot()
}

// Restore 实现 raft.FSM。从快照恢复状态机，委托 Applier.Restore。
//
// 恢复成功即清除旧的分歧标记——能从快照追平，说明状态已一致。
func (s *StateMachine) Restore(snapshot io.ReadCloser) error {
	defer snapshot.Close()
	if err := s.applier.Restore(snapshot); err != nil {
		s.applyErr = err
		return err
	}
	s.applyErr = nil
	return nil
}

// IsHealthy 报告状态机是否处于「可服务」状态。
//
// 一旦 applyErr 非 nil（发生过 apply/decode 失败），副本判定不健康、拒绝写。
// runtime 应据此把 /health 标记为 degraded 并停止 Propose。这是 fail-closed。
func (s *StateMachine) IsHealthy() bool { return s.applyErr == nil }

// LastApplied 返回最近成功 apply 的日志索引（水位线）。
func (s *StateMachine) LastApplied() uint64 { return s.lastApplied }

// ErrBadCommand 表示日志载荷无法解码为合法 Command。
//
// 这是不可恢复错误：坏日志不能跳过（跳过即分歧），只能由上层告警并人工介入。
var ErrBadCommand = errBadCommand{}

type errBadCommand struct{}

func (errBadCommand) Error() string { return "consensus: bad or malformed command" }
