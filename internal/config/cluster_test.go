package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"uic/internal/model"
)

// TestClusterValidation 覆盖 cluster 块的全部配置期不变量。
//
// 这些约束必须在配置期 fail-fast：一次集群配置错误（缺地址、两节点、端口
// 冲突）若拖到启动期才发现，表现会是「选不出 leader」——所有写 409/504，
// 而日志里只有 raft 连接失败，运维很难联想到是配置少写了一个字段。
func TestClusterValidation(t *testing.T) {
	registerDrivers(t)

	valid := func(mut func(*Cluster)) *File {
		f := baseFile()
		c := &Cluster{Enabled: true, NodeID: "node-a", Listen: "10.0.0.1:7000"}
		mut(c)
		f.Cluster = c
		return f
	}

	t.Run("single node without peers", func(t *testing.T) {
		f := valid(func(*Cluster) {})
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if f.Cluster.DataDir != "raft" {
			t.Fatalf("dataDir default = %q, want raft", f.Cluster.DataDir)
		}
	})

	t.Run("three nodes", func(t *testing.T) {
		f := valid(func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		})
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	t.Run("disabled cluster ignores the block", func(t *testing.T) {
		f := baseFile()
		// 关掉集群时即便字段残缺也不该拦：这些值不参与任何运行期行为。
		f.Cluster = &Cluster{Enabled: false, Listen: "not-an-address"}
		if err := f.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	bad := map[string]func(*Cluster){
		"missing listen": func(c *Cluster) { c.Listen = "" },
		"listen without port": func(c *Cluster) {
			c.Listen = "10.0.0.1"
		},
		"listen port zero": func(c *Cluster) {
			// 端口 0 是随机分配：对端无法据此连进来，多节点必然失败。
			c.Listen = "10.0.0.1:0"
		},
		"two nodes": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
			}
		},
		"peers without nodeId": func(c *Cluster) {
			c.NodeID = ""
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"nodeId absent from peers": func(c *Cluster) {
			c.NodeID = "node-z"
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"peer addr differs from listen": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.9:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"duplicate peer id": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-a", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"duplicate peer addr": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.1:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"peer without id": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"peer without addr": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: ""},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
		"peer addr without port": func(c *Cluster) {
			c.Peers = []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			}
		},
	}
	for name, mut := range bad {
		f := valid(mut)
		err := f.Validate()
		if err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
		if !strings.Contains(err.Error(), "cluster.") {
			t.Fatalf("%s: error %q should name the offending cluster field", name, err)
		}
	}
}

// TestCanonicalExcludesCluster 验证 cluster 块不参与版本哈希。
//
// 它是节点本地启动参数：同一份逻辑配置在各节点的 nodeId 必然不同，若纳入
// 哈希，「全集群同一个配置版本」这个可核对的契约就不成立了；节点身份还会
// 被写进版本号与对外响应。
func TestCanonicalExcludesCluster(t *testing.T) {
	registerDrivers(t)

	a := baseFile()
	a.Cluster = &Cluster{Enabled: true, NodeID: "node-a", Listen: "10.0.0.1:7000", DataDir: "raft"}
	b := baseFile()
	b.Cluster = &Cluster{Enabled: true, NodeID: "node-b", Listen: "10.0.0.2:7000", DataDir: "raft"}
	none := baseFile()

	ca, err := a.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	cb, err := b.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	cn, err := none.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if VersionOf(ca) != VersionOf(cb) {
		t.Fatalf("cluster block leaked into version hash: %s vs %s", VersionOf(ca), VersionOf(cb))
	}
	if VersionOf(ca) != VersionOf(cn) {
		t.Fatal("adding a cluster block must not change the version hash")
	}
	if strings.Contains(string(ca), "node-a") {
		t.Fatal("canonical output must not contain node identity")
	}

	// MarshalFull 是落盘形式，必须保留节点本地块。
	full, err := a.MarshalFull()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(full), "node-a") || !strings.Contains(string(full), "10.0.0.1:7000") {
		t.Fatalf("MarshalFull dropped the local cluster block: %s", full)
	}
}

// TestApplyKeepsLocalCluster 验证共识复制来的配置不会带走本机的 cluster 块。
//
// 配置通过 raft 复制时，载荷是 canonical 字节（cluster 已被排除）。但如果
// 副本改动别的节点身份，落盘就会把本机的 nodeId/listen 换成别人的——重启后
// 它会以错误的身份加入集群。
func TestApplyKeepsLocalCluster(t *testing.T) {
	registerDrivers(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	local := baseFile()
	local.Cluster = &Cluster{
		Enabled: true, NodeID: "node-a", Listen: "10.0.0.1:7000", DataDir: "raft",
	}
	raw, err := local.MarshalFull()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 另一节点的配置：共享内容有变更（site 变了），cluster 身份不同。
	remote := baseFile()
	remote.Site = "plant-02"
	remote.Cluster = &Cluster{
		Enabled: true, NodeID: "node-b", Listen: "10.0.0.2:7000", DataDir: "raft",
	}
	res, err := m.Apply(remote, "raft")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Applied {
		t.Fatal("expected config to be applied")
	}

	if got := m.Current().Cluster; got == nil || got.NodeID != "node-a" || got.Listen != "10.0.0.1:7000" {
		t.Fatalf("in-memory cluster = %+v, want local node-a identity", got)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(onDisk, &f); err != nil {
		t.Fatal(err)
	}
	if f.Cluster == nil || f.Cluster.NodeID != "node-a" || f.Cluster.Listen != "10.0.0.1:7000" {
		t.Fatalf("persisted cluster = %+v, want local node-a identity", f.Cluster)
	}
	if f.Site != "plant-02" {
		t.Fatalf("persisted site = %q, want shared content applied", f.Site)
	}

	// 入参为 nil cluster 时同样保留本机块（回滚快照、老配置都可能没有它）。
	noCluster := baseFile()
	noCluster.Site = "plant-03"
	if _, err := m.Apply(noCluster, "rollback"); err != nil {
		t.Fatalf("apply without cluster: %v", err)
	}
	onDisk, _ = os.ReadFile(path)
	if !strings.Contains(string(onDisk), `"nodeId": "node-a"`) {
		t.Fatalf("local cluster block lost after applying a config without one: %s", onDisk)
	}
}

// TestVersionStableAcrossNodes 用真实落盘路径确认「同内容不同节点 = 同版本」。
func TestVersionStableAcrossNodes(t *testing.T) {
	registerDrivers(t)

	open := func(nodeID, addr string) (*Manager, string) {
		dir := t.TempDir()
		f := baseFile()
		f.Cluster = &Cluster{Enabled: true, NodeID: nodeID, Listen: addr, DataDir: "raft"}
		raw, err := f.MarshalFull()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := Open(path, dir)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return m, path
	}

	a, _ := open("node-a", "10.0.0.1:7000")
	b, _ := open("node-b", "10.0.0.2:7000")
	if a.Version() != b.Version() {
		t.Fatalf("version differs across nodes: %s vs %s", a.Version(), b.Version())
	}
	if a.Version() == "" {
		t.Fatal("empty version")
	}
}

// TestClusterRejectsSlaves 验证多节点集群不允许内置从站。
//
// 从站监听地址（TCP listen 或串口路径）是复制配置的一部分：多节点共用同一
// 份配置意味着第二个实例启动时 net.Listen 必然地址冲突，openSlaves 是硬启
// 动步骤，冲突会导致该实例直接 abort；即使串口可用，降主副本仍会以 disabled
// 点位回主站。因此配置期必须拦截。
func TestClusterRejectsSlaves(t *testing.T) {
	registerDrivers(t)

	multiCluster := func(slaves ...model.Slave) *File {
		f := baseFile()
		f.Cluster = &Cluster{
			Enabled: true, NodeID: "node-a", Listen: "10.0.0.1:7000",
			Peers: []Peer{
				{ID: "node-a", Addr: "10.0.0.1:7000"},
				{ID: "node-b", Addr: "10.0.0.2:7000"},
				{ID: "node-c", Addr: "10.0.0.3:7000"},
			},
		}
		f.Slaves = slaves
		return f
	}
	tcpSlave := func(listen string) model.Slave {
		return model.Slave{
			ID:     "tcp-" + strings.ReplaceAll(listen, ":", "_"),
			Listen: listen,
			Units: []model.SlaveUnit{{
				UnitID: 1,
				Mappings: []model.SlaveMapping{
					{Point: "dev1/p1", Area: model.MappingHolding, Offset: 0},
				},
			}},
		}
	}
	serialSlave := func(port string) model.Slave {
		return model.Slave{
			ID:     "serial0",
			Serial: &model.SlaveSerial{Port: port, Framing: "rtu", Baud: 19200},
			Units: []model.SlaveUnit{{
				UnitID: 1,
				Mappings: []model.SlaveMapping{
					{Point: "dev1/p1", Area: model.MappingHolding, Offset: 0},
				},
			}},
		}
	}

	t.Run("multi-node rejects TCP slave", func(t *testing.T) {
		f := multiCluster(tcpSlave("127.0.0.1:15020"))
		if err := f.Validate(); err == nil {
			t.Fatal("expected validation error when multi-node cluster has TCP slave")
		} else if !strings.Contains(err.Error(), "slaves are not supported") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("multi-node rejects serial slave", func(t *testing.T) {
		f := multiCluster(serialSlave("/dev/ttyUSB0"))
		if err := f.Validate(); err == nil {
			t.Fatal("expected validation error when multi-node cluster has serial slave")
		} else if !strings.Contains(err.Error(), "slaves are not supported") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("multi-node without slaves is accepted", func(t *testing.T) {
		f := multiCluster()
		if err := f.Validate(); err != nil {
			t.Fatalf("multi-node cluster without slaves should be accepted: %v", err)
		}
	})

	t.Run("single-node cluster allows TCP slave", func(t *testing.T) {
		f := baseFile()
		f.Cluster = &Cluster{Enabled: true, NodeID: "node-a", Listen: "10.0.0.1:7000"}
		f.Slaves = []model.Slave{tcpSlave("127.0.0.1:15020")}
		if err := f.Validate(); err != nil {
			t.Fatalf("single-node cluster with TCP slave should be accepted: %v", err)
		}
	})

	t.Run("disabled cluster ignores the guard", func(t *testing.T) {
		f := baseFile()
		f.Cluster = &Cluster{Enabled: false, NodeID: "node-a", Listen: "10.0.0.1:7000", Peers: []Peer{{ID: "node-a", Addr: "10.0.0.1:7000"}}}
		f.Slaves = []model.Slave{tcpSlave("127.0.0.1:15020")}
		if err := f.Validate(); err != nil {
			t.Fatalf("disabled cluster with slave should be accepted: %v", err)
		}
	})
}
