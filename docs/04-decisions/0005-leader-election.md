# ADR-0005｜多实例 leader 选举：引入第三方 raft 做全状态机复制，推翻 ADR-0004 零依赖约束

- **状态**：提议（Proposed）— 待批准
- **决策日期**：多实例协调收尾阶段
- **决策者**：架构组
- **影响范围**：`go.mod`/`go.sum`/`vendor/`（依赖面由「按构造为零」变为「有限第三方集」）、`internal/consensus`（新包，raft 集成与复制状态机）、`internal/runtime`（采集/投递/写操作的 leader 闸与提交入口）、`internal/northbound/httpapi`（`/api/v1/leader` 块、409 语义、提交式配置/控制端点）、`docs/04-decisions/0004`（其零依赖硬约束被本决策正式修订）
- **相关文档**：ADR-0001（语言选型：单二进制、可交叉编译、零 CGO —— 本决策不推翻它，只重新评估依赖面）、ADR-0004（工具链与依赖安全：零第三方依赖 —— **本决策正式修订它**）、`internal/config`（已有版本化配置存储：`Canonical()` 哈希、`Version()`、快照、回滚、staged —— 复制状态机的天然日志）、`internal/instanceid`（实例身份）、`docs/03-product/07-roadmap.md` §5.2（「多实例协调」路线图项）

---

## 1. 决策摘要

五句话：

1. **引入第三方 raft 库（候选首选 `hashicorp/raft`，备选 `etcd/raft`、`dragonboat`）组建多实例集群，用 raft 的「唯一 leader + 日志复制」同时解决 leader 选举与强一致状态同步。** 不再用同主机 flock，也不再把跨主机协调推给中心聚合器。
2. **复制的是「控制面状态」，不是「采集数据」。** 采集数据继续走 Outbox（at-least-once 已落地）；raft 复制的是**配置、点位/控制命令、报警确认、成员变更**这些必须由集群一致裁决的状态 —— 即「全状态机复制」，复制集是 runtime 的写状态，不是时序样本。
3. **本决策正式推翻 ADR-0004 的「零第三方依赖」硬约束，但守住 ADR-0004 的「安全纪律」。** 依赖面不再按构造为空，改为「**有限、可枚举、可审计**」：固定一组 raft 及其传递依赖，进入 `go.mod` `require` 与 `vendor/`，纳入 `make sbom` 与 CI 漏洞扫描。ADR-0004 §6.3 的反顾触发器「零依赖约束被推翻」被本决策触发。
4. **非 leader 实例仍服务只读端点（`/health`、`/version`、`/metrics`、查询类），但所有写操作（点位写、控制命令、配置应用/回滚、报警确认）改为「提交到 raft 日志」：leader 序列化 → 多数派复制 → 提交后各副本按序 apply。** 副本 apply 失败即判副本不健康、停止服务写（fail-closed）。脑裂由 raft 多数派从根上消除。
5. **单实例部署（集群规模为 1）退化为「永远是 leader」，行为与今天完全一致，且 `raft` 以单节点集群运行、零外部依赖。** 只有显式配置 `cluster.peers`（≥3 节点）时才启用多副本。这是「raft 引入不破坏单实例」的关键保障。

一句话理由：

> 当产品明确要求跨主机、强一致、抗脑裂的协调时，flock（同主机）与软选主（脑裂窗口）都不再够用；raft 是这一需求下被充分验证的答案。它的代价是推翻零依赖约束，但零依赖是这个产品的**差异化支点之一而非目的本身**——用「有限、可审计、vendored 的第三方依赖」换「真正的分布式强一致」，在已确认的产品方向下是值得的、且可控的。

---

## 2. 背景：需求、约束与现状（代码复核）

### 2.1 为什么需要跨主机强一致（问题陈述）

ADR-0005 的前一版（flock 路线）把跨主机协调判为 out of scope，理由是「真实重复采集发生在同主机」。**用户明确不认同这一判断，要求边缘网关自身具备跨主机、强一致、抗脑裂的协调能力。** 因此问题重述为：

| 场景 | 无跨主机强一致的后果 |
|---|---|
| 两台网关配了同一组设备做双活/热备 | 两台都采集、都投递，且无任何机制裁决「谁该写」 |
| 运维对某实例下发配置变更 / 点位写 / 控制命令 | 该操作只作用于被命中的那一台，集群状态出现分歧 |
| 网络分区下两侧各有一台自认为 leader | 两侧都接受写，分区愈合后状态冲突、无冲突检测 |
| 一台网关重启接管 | 新 leader 不知道旧 leader 已下发的配置/未确认报警 |

flock 与 MQTT 软选主都无法覆盖后三者——这正是「分布式强一致」的领地，也是 raft 的正当场景。

### 2.2 被本决策触碰的硬约束（必须正面处理，不能绕过）

| 约束 | 来源 | 本决策如何处置 |
|---|---|---|
| 零第三方依赖、连 `go.sum`/`vendor` 都不生成 | ADR-0004 §1 | **正式推翻。** 改为「有限可枚举依赖 + vendor 固化 + SBOM/CI 审计」 |
| 单二进制 ≤40 MB、三平台交叉编译 | ADR-0001 C1/C2 | 重新评估：raft + 依赖计入体积预算，逐平台验交叉编译（见 §6） |
| 零 CGO | ADR-0001 C7 | 候选库须满足 `CGO_ENABLED=0` 可构建（`hashicorp/raft` 满足；`dragonboat` 的存储层需核对） |
| 依赖面按构造为空（`go list -m all` 只返回 `uic`） | ADR-0004 §2.1 | **作废**，改为「`go list -m all` 返回固定有限集」 |

> 纪律要求：推翻一条已接受的 ADR，必须**显式、可追溯**——本 ADR 即那份记录，ADR-0004 状态由「Accepted」改为「Superseded（部分，见 ADR-0005）」。

### 2.3 现状（代码复核）

- `go.mod` 仅两行（`module uic` + `go 1.26.10`），无 `require`、无 `go.sum`、无 `vendor/`、源码零第三方 import —— **零依赖现状 100% 确认**，本决策将改变它。
- **配置已是天然的复制日志**：`internal/config.Manager` 用 `File.Canonical()` 算稳定哈希 → `VersionOf(canonical)` 作版本号 → 快照落盘 + 回滚 + staged 计划（`BeginStaged`/`AdvanceStaged`/`AbortStaged`）。raft 复制的「配置条目」可以直接是「配置版本 + canonical 内容」，apply 即 `Manager.Apply`。**这是把 raft 接入现有系统的最小切口。**
- **写面已成形**：`httpapi.Backend` 接口的写类方法 = `Write` / `Command` / `ApplyConfig` / `Rollback` / `BeginStaged` / `AdvanceStaged` / `AbortStaged` / `AckAlarms`。leader 闸与「提交到 raft 日志」只需包裹这一组方法。
- **采集/投递入口**：`runtime.Start` → `dispatchLoop`。leader 闸可在此拦截：非 leader 不产生采集请求、不进 Outbox（比「采了再丢」正确）。
- **身份已就位**：`instanceid.Load` 提供稳定 `instanceId`，raft 的 `ServerID` 直接复用它，`/api/v1/leader` 的 `instanceId` 与 `/version`、诊断包一致。
- `grep -rniE "leader|raft|quorum" internal/` 的命中仍全部在协议编解码层（IEC 61850/KNX/ADS 的设备角色），**跨实例协调层是空白**。

---

## 3. 候选方案

### 3.1 `hashicorp/raft`（**首选**）

**做法**：引入 `github.com/hashicorp/raft`，配 `raft-boltdb`（或自研快照存储）作 `StableStore`/`LogStore`，自写 `Transport`（复用现有 TCP/HTTP 能力）。

**为什么首选**：

1. **纯 Go、零 CGO、社区最成熟** —— 满足 ADR-0001 C7；被 Consul/Nomad/Vault 生产验证，选举、日志复制、快照、成员变更（joint consensus）路径久经考验。
2. **API 边界清晰**：你提供一个实现 `fsm.FSM` 接口的状态机（`Apply`/`Snapshot`/`Restore`），raft 只负责「把日志一致地喂给每个副本的 FSM」——这正好对应本决策的「全状态机复制」。
3. **体积/依赖可控**：核心库 + boltdb + 少量传递依赖（go-msgpack 等），vendor 后可枚举；二进制增量在可接受范围（需 §6 实测确认，不臆断）。

**代价**：传递依赖约 6~10 个模块（`go-hclog`、`go-metrics`、`go-msgpack`、`raft-boltdb`→`bbolt` 等）；boltdb 的 `mmap` 在 Windows/ARM 需验交叉编译；这些都要进 SBOM。

### 3.2 `go.etcd.io/raft`（备选）

**做法**：仅引入共识核心 `go.etcd.io/raft`（不含 etcd 服务器），自备 `WAL`、`Transport`、`Ready` 处理循环。

**权衡**：核心库依赖更少、更「纯共识」，但它**只给你共识算法，存储/传输/快照全要自研**——集成代码量大、正确性责任落在自己写的 `Ready` 处理循环上。适合「要共识但不要 boltdb」的团队，对我们是「省了依赖、费了工」。

### 3.3 `lni/dragonboat`（备选）

**做法**：引入 `dragonboat`，它把 raft + 多 raft 组 + 状态机快照打包好。

**权衡**：功能最全（多 raft 组、多线程），但体量大、存储层历史上与 CGO/平台耦合更紧、交叉编译矩阵复杂。对我们「单 raft 组、单状态机」的需求是**过度**，且最可能触碰 ADR-0001 交叉编译/零 CGO 约束。

### 3.4 候选对比

| 维度 | hashicorp/raft（首选） | etcd/raft 核心 | dragonboat |
|---|---|---|---|
| 纯 Go / 零 CGO | ✅ | ✅ | ⚠️ 需核对 |
| 生产验证度 | 高（Consul/Vault） | 中（库本身） | 中 |
| 需自研量 | 低（FSM + Transport） | **高**（WAL/Ready/Transport 全自研） | 低 |
| 传递依赖数 | 6~10 | 少 | 多 |
| 体积/交叉编译代价 | 中 | 中 | **高** |
| 单状态机契合度 | ✅ | ✅ | 过度（多 raft 组） |

**决策**：选 `hashicorp/raft`。理由：生产验证度最高、需自研量最低、API 与「FSM 状态机复制」天然对应，代价（依赖数/体积）在可接受且可审计范围。

### 3.5 前一版 flock 路线（保留作对照，已被用户否决）

前一版 ADR 结论「flock 同主机 + 跨主机交给中心聚合器 + 不引入 raft」被用户明确否决（「就是要 raft / 分布式强一致」）。归档于此仅为保留决策演进痕迹：flock 的「内核强一致」在同主机场景仍成立，但它**结构上无法覆盖跨主机**，故不满足产品方向。raft 取代它作为统一方案。

---

## 4. 设计要点（raft 路线的具体契约）

### 4.1 复制什么：控制面状态机（「全状态机复制」的边界）

复制集 = runtime 的**写状态**，即任何「必须全集群一致的裁决」。明确划界：

| 复制（进 raft 日志，全副本 apply） | 不复制（留在本地，不进共识） |
|---|---|
| 配置应用 / 回滚（`config.Version` + canonical） | 采集样本（时序数据，走 Outbox at-least-once） |
| 点位写命令（`Write` 的意图与幂等结果） | 北向投递结果（Outbox/SSE/MQTT 各自本地） |
| 控制命令（`Command`：重启设备、模式切换等） | 历史/缓存（本地有界存储） |
| 报警确认（`AckAlarms`） | 指标、日志、心跳（本地观测） |
| 集群成员变更（增删节点） | 只读查询结果（`/health` 等） |

**理由**：样本是高频、幂等可重放、可丢可重的，共识它毫无收益且会拖垮吞吐；控制面状态是低频、必须一致、分歧即事故的——**共识它收益最大**。「全状态机复制」指的就是这一控制面状态机的完整复制，不是把一切塞进共识。

### 4.2 纯包 `internal/consensus`

沿用本仓「纯判定可单测」的一贯做法，把 raft 与 runtime 解耦：

- **`Consensus` 接口**：`Propose(cmd Command) error` / `Apply() <-chan Command` / `IsLeader() bool` / `LeaderID() string` / `AddPeer`/`RemovePeer` / `Shutdown()`。
- **`RaftConsensus`**：`hashicorp/raft` 的薄封装。实现 `raft.FSM` 接口（`Apply`/`Snapshot`/`Restore`）把日志命令桥接到内部 apply 通道。
- **`StateMachine`（runtime 提供）**：实现 raft FSM 的 `Apply`，按序把每条命令应用到本地 `config.Manager` / 报警 / 命令处理器。apply 失败 → 副本标记不健康、拒绝后续写（fail-closed）。
- **存储后端**：`LogStore`/`StableStore` 用 `raft-boltdb`，快照存 `<dataDir>/raft/`（与配置快照分离）。

### 4.3 写操作的「提交式」语义（替代前一版的 409）

- **leader 收到写请求**（`Write`/`Command`/`ApplyConfig`/`Rollback`/`AckAlarms`/staged）：
  1. 序列化为 `Command`，`Propose` 到 raft；
  2. 等待多数派复制并 commit（带超时）；
  3. 本副本 FSM `Apply` 后返回结果给调用方。
- **非 leader 收到写请求**：返回 `409 Conflict` + `{"error":"not_leader","leader":{"instanceId":"...","api":"..."}}`，**并附 leader 的网络地址**（若集群内可达），让客户端重定向——比单纯拒绝更有用，且不改变「写必须 leader」的强一致语义。
- **超时未提交**：返回 `504`/明确错误，不静默吞掉（静默忽略写请求是缺陷）。**fail-closed**：无法确认提交成功的写，绝不当作成功。

### 4.4 采集与投递的 leader 闸

- `runtime.Start` 的 `dispatchLoop` 前置 `IsLeader()` 判定：非 leader 不产生采集请求、不写 Outbox。leader 切换时，新 leader 自然接管采集，旧 leader 停采——**重复采集由 raft 唯一 leader 从根上消除**，不再依赖「配置相同就不冲突」的假设。
- leader 切换有过渡窗口（旧 leader 未察觉已失主）：用「提交式写 + Outbox 幂等去重」兜底，窗口内的重复不丢不坏。

### 4.5 故障转移与成员变更

- **故障转移**：raft 内建。leader 失联，剩余多数派在 election timeout 内重新选主（默认百毫秒~秒级）。单节点集群无此需求（永远是 leader）。
- **成员变更**：增删节点走 raft 的 joint consensus（`AddPeer`/`RemovePeer` 经日志复制），避免变更期脑裂。规模固定为 3 时成员变更极少用，但接口要留。
- **持久化**：raft 的 `CurrentTerm`/`VotedFor`/日志落盘，重启恢复——**重启不破坏多数派、不会双主**。

### 4.6 契约暴露

- **`GET /api/v1/leader`**：`{"instanceId":"...","role":"leader|follower|candidate","term":N,"leaderId":"...","peers":[...],"commitIndex":N,"since":"RFC3339"}`。`/health` 加 `leader` 块（`role`/`term`/`peers`/`commitIndex`），**不参与 status 判定**（无 leader 是正常角色，集群可能正在选主）。
- **指标**：`uic_leader_role{role="leader|follower|candidate"}`（gauge）、`uic_raft_term`（gauge）、`uic_raft_commit_index`（gauge）、`uic_raft_proposals_total{result="committed|rejected|timeout"}`、`uic_raft_membership_changes_total`。

### 4.7 配置

```json
"cluster": {
  "enabled": false,
  "nodeId": "",                  // 缺省复用 instanceid
  "dataDir": "raft",             // 相对 dataDir
  "listen": "0.0.0.0:7000",      // raft Transport
  "peers": [                      // >=3 才启用多副本；<=1 或空 = 单节点 always-leader
    {"id":"uic-<site>-<a>","addr":"10.0.0.1:7000"},
    {"id":"uic-<site>-<b>","addr":"10.0.0.2:7000"},
    {"id":"uic-<site>-<c>","addr":"10.0.0.3:7000"}
  ]
}
```

- **默认 `enabled: false`**：退化为单节点 always-leader，零外部依赖、行为与今天一致。
- `peers` 含本节点自身 `nodeId`；`peers` 数量 < 3 时拒绝启用多副本（2 节点无多数派意义），fail-fast 配置报错。
- 进 `Canonical()` 哈希，版本化与回滚天然支持。

---

## 5. 与参考方案的差异

| 维度 | 常见 raft 集成 | 本决策 |
|---|---|---|
| 复制内容 | 常复制全量状态 | **只复制控制面状态机**，样本走 Outbox |
| 单实例兼容 | 多数需额外开关 | **单节点 raft 集群天然 always-leader**，默认关闭 |
| 依赖 | 引入即无约束 | **有限可枚举 + vendor + SBOM/CI 审计**（守住 ADR-0004 纪律） |
| 非 leader 写 | 直接拒绝 | 409 + leader 地址，支持重定向 |

---

## 6. 后果

### 6.1 正面

- **真正的跨主机强一致、抗脑裂**：raft 多数派 + 持久化，从根上消除双主，覆盖双活/热备/分区/接管全场景。
- **控制面状态全副本一致**：配置、命令、报警确认、成员变更全集群按序 apply。
- **写操作语义清晰**：提交式写，未确认即明确报错（fail-closed），无静默吞请求。
- **单实例零影响**：默认关闭、单节点 always-leader，绝大多数部署行为不变。

### 6.2 负面与缓解

| 负面 | 缓解 |
|---|---|
| 推翻零依赖约束（产品差异化支点之一） | 依赖有限可枚举、vendor 固化、纳入 SBOM/CI；ADR-0004 正式改为 Superseded（部分）并留痕 |
| 二进制体积上升、可能逼近 40 MB | §6 实测：引入前后体积对比，写进 ADR；若逼近上限，评估裁剪（如去掉 go-metrics 的 expvar 依赖） |
| 传递依赖的安全面扩大 | 沿用 ADR-0004 工具链：OSV/`govulncheck` 扫描，CI 接入；定期复核 |
| 交叉编译矩阵（尤其 boltdb mmap 在 Windows/ARM） | 三平台 `make` 交叉编译验收纳入 CI；不满足则换纯 mmap 替代存储 |
| 运维需理解 raft（分区、多数派、选主） | `/api/v1/leader` + 指标暴露 term/commitIndex；文档给出「3 节点部署」「故障切换」runbook |
| 单组 raft 的写吞吐上限 | 控制面写本就低频；样本不入共识，吞吐不受 raft 限制 |

### 6.3 退出条件 / revisit 触发器

出现以下任一情况，重评本决策（另开 ADR）：

1. **`hashicorp/raft` 体积/依赖超出可接受**，或交叉编译/零 CGO 无法满足 → 评估 `etcd/raft` 核心（少依赖、多自研）或重新设计状态机边界。
2. **出现「需要多 raft 组 / 数据平面也要共识」的需求** → 评估 `dragonboat`（多 raft 组）。
3. **中心聚合器这条独立路线图项成熟**，且能提供更低成本的跨主机协调 → 评估是否让边缘退回「raft 选主 + 中心下发」混合。
4. **零依赖被重新确立为不可让渡的产品红线**（战略回退）→ 本决策整体作废，回到「同主机 flock + 跨主机不强一致」的降级方案。

---

## 7. 验证（变异测试计划）

沿用本仓「先编译通过、再让测试变红」的硬标准。引入 raft 后，用「内存 FSM + 模拟 Transport / 多 in-proc 节点」做确定性测试（不依赖真实网络分区），预计 20+ 变异体：

| 类别 | 变异体（必须变红） |
|---|---|
| leader 闸 | 非 leader 仍 Propose 写；非 leader 仍采集；写失败返回 200 而非 504/409 |
| 提交语义 | 未多数派复制就返回成功；提交超时不报错（静默吞）；Propose 不等 commit |
| FSM apply | apply 命令顺序错乱（乱序 apply）；apply 失败仍判副本健康；apply panic 不恢复 |
| 快照/恢复 | Snapshot 不含未 apply 的已提交日志；Restore 不重置状态；快照后丢失已提交条目 |
| 成员变更 | 变更不经日志复制即生效；变更期允许双主；AddPeer 不校验多数派 |
| 故障转移 | 选主不靠多数派（单节点也能选）；leader 失联后不重选；term 不回退 |
| fail-closed | 无法判 leader 时默认当 leader 采写（fail-open）；集群未成共识仍接受写 |
| 契约 | `/api/v1/leader` 缺 `term`/`commitIndex`；非 leader 409 不带 leader 地址；health.leader 参与 status 判定 |
| 单实例兼容 | `enabled:false` 时行为变化；单节点集群仍走选主（应为 always-leader）；`peers<3` 未 fail-fast |
| 依赖/构建 | 未 vendor（依赖不可复现构建）；`go list -m all` 含意外模块；体积超预算未告警 |

---

## 8. 关键结论

1. **用户要的是跨主机、强一致、抗脑裂的协调，flock 结构上做不到、软选主不安全，raft 是这一需求的标准答案。**
2. **引入第三方 raft（首选 `hashicorp/raft`）正式推翻 ADR-0004 的零依赖约束**——但零依赖是手段不是目的；本决策用「有限可枚举 + vendor + SBOM/CI 审计」守住 ADR-0004 真正的安全纪律。
3. **复制的是控制面状态机（配置/命令/报警确认/成员变更），不是采集数据**——样本继续走 Outbox 不丢不坏；「全状态机复制」指控制面状态机的完整复制。
4. **单实例默认关闭、单节点 always-leader**，绝大多数部署零影响；`peers>=3` 才启用多副本，`peers<3` fail-fast。
5. **提交式写 + fail-closed + 非 leader 409 带重定向地址**，是 raft 路线下保证「写强一致、绝不静默吞请求」的四条语义边界。

> 一句话收尾：**当产品方向是分布式强一致时，零依赖必须让位——但让位不等于放任，用可审计的有限依赖把 raft 接进来，用单一共识层把控制面复制成一致状态机。**
