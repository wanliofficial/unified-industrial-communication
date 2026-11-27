package consensus

import "github.com/hashicorp/raft"

// raftBackend 是 raft 四大底层组件的集合，供测试注入内存实现。
//
// 为什么用这个注入而不是直接依赖 boltdb/TCP：consensus 包要能确定性单测
// 「选主、日志复制、failover、快照恢复」这些分布式行为——它们不能依赖真实
// 网络分区与磁盘时序（会 flaky）。hashicorp/raft 自带 inmem_transport /
// inmem_store，就是为这种测试而生。生产路径 TestBackend 为 nil，改用 boltdb
// + TCP transport（见 raft.go 的 newProductionBackend）。
//
// 字段直接是 raft 的接口类型，因为 RaftConsensus 最终要把它们交给 raft.NewRaft。
type raftBackend struct {
	// logs/stable/snaps：日志存储、稳定存储（term/vote）、快照存储。
	logs   raft.LogStore
	stable raft.StableStore
	snaps  raft.SnapshotStore
	// trans：节点间传输。测试用 inmem（多节点互联），实现确定性。
	trans raft.Transport
	// cleanup：释放底层资源（生产路径关 transport + bbolt）。测试为 nil。
	cleanup func() error
}
