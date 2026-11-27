# ADR-0005｜多实例 leader 选举：同主机 flock 强一致选主，跨主机交给中心聚合器，不引入 raft

- **状态**：提议（Proposed）— 待批准
- **决策日期**：多实例协调收尾阶段
- **决策者**：架构组
- **影响范围**：`internal/leader`（新包）、`internal/runtime`（采集/投递/写操作的 leader 闸）、`internal/northbound/httpapi`（`/api/v1/leader` 块与 409 语义）、`go.mod`（依赖面保持为零）
- **相关文档**：ADR-0001（零第三方依赖、零 CGO）、ADR-0004（不引入任何第三方库，连 `go.sum`/`vendor` 都不生成）、`docs/03-product/07-roadmap.md` §5.2（「多实例协调」与「中心聚合器」两条独立路线图项）、`docs/03-product/02-architecture.md` §3（模块图与部署形态）

---

## 1. 决策摘要

四句话：

1. **同主机多实例（多容器 / 多进程 / 双机热备同盘）：用 `flock` 在 `<dataDir>/leader.lock` 上做内核强一致选主。** 持锁者即 leader，崩溃时内核自动释放、无脑裂、无心跳、零新依赖。
2. **跨主机协调不在边缘做，交给「中心聚合器」这条独立路线图项。** 边缘软选主（MQTT retained + 租约）存在**脑裂窗口**，对采集是「重复但幂等」可接受，对**写操作不安全**——而我们的语义要求「写操作须 leader」。用软状态去守一个强一致要求，是把安全边界建立在时序假设上。
3. **明确拒绝 raft（第三方与自研两条路都不走）。** 第三方 raft 违反 ADR-0004 的零依赖硬约束；自研 raft 是把「零依赖工业网关」的可靠性押在手写的共识算法上。更根本的是：**我们只需要「谁是主」，不需要「复制状态机」**——用 raft 解 leader 选举是拿大炮打蚊子，且引入了它全部的复杂度却没有它的收益。
4. **非 leader 实例的行为**：仍可服务 `/health`、`/version`、`/metrics` 等**只读**端点；**不采集、不投递**；**写操作（配置变更 / 点位写 / 控制命令 / 报警确认）返回 `409 not_leader`**，并带 `leader.instanceId` 供运维定位真主。

一句话理由：

> 真实的多实例重复采集发生在**同一台主机**（一台工业机跑两个容器、灰度期新旧进程并存），而这件事 `flock` 能用内核保证做到无脑裂、零依赖。跨主机的强一致协调属于「中心聚合器」的职责，不该塞进边缘网关——那会让每一台边缘设备都背上一套分布式共识的运维成本，只为解决一个绝大多数现场并不存在的问题。

---

## 2. 背景：需求给出的硬约束与现状

### 2.1 为什么需要 leader 选举（问题陈述）

同站点跑两个 UIC 实例是常见部署：

| 场景 | 现状 | 后果 |
|---|---|---|
| 一台主机多容器（旧版/新版并存灰度） | 两实例各自采集、各自投递 | 同一设备被采两遍、同一数据被投两遍 |
| 双机热备（同盘或共享数据目录） | 两实例都以为自己该干活 | 主备同时写、写冲突、数据重复 |
| 运维误起两个进程（systemd 配错） | 无任何拦截 | 静默重复，日志里一片正常 |

`instanceid`（已落地）解决了「同站点多实例 MQTT clientID / Sparkplug nodeId 互相顶掉」的**身份消歧**问题——但那只是让两个实例**不互相踢下线**，**没有解决「两个都在采集投递」**。后者才是 leader 选举要回答的：**同一时刻，谁有资格采集与写入？**

### 2.2 硬约束（不可协商）

这三条来自已接受的 ADR，是本决策的边界：

| # | 约束 | 来源 |
|---|---|---|
| E1 | **零第三方依赖**：不引入任何第三方库，不生成 `go.sum` / `vendor/` | ADR-0004 §1 |
| E2 | **零 CGO、单二进制 ≤40 MB、三平台交叉编译** | ADR-0001 C1/C2/C7 |
| E3 | **依赖面按构造为空**：`go list -m all` 只返回 `uic` 自身 | ADR-0004 §2.1 |

任何「引入 X 库」的方案，都要先回答「X 是否推翻了 E1」，而不是「X 好不好用」。

### 2.3 现状（代码复核）

- `internal/instanceid` 已落地：持久化稳定身份到 `<dataDir>/instance.id`，派生 MQTT clientID / Sparkplug nodeId，暴露到 `GET /api/v1/version` 与诊断包 `manifest.json`。**这是「不冲突」，不是「不重复」。**
- `grep -rniE "leader|election|lease|quorum|raft" internal/` 的命中**全部在协议编解码层**（IEC 61850 的 `leader` 是 MMS 关联里的角色、KNX 的 `leader` 是总线主站、ADS 的 `leader` 是符号名），**没有任何跨实例协调代码**。leader 选举是真正的空白。
- `docs/03-product/07-roadmap.md` §5.2 把「多实例协调」放在**架构**类、「中心聚合器」放在**集中管理**类——两条独立路线图项。本 ADR 只裁「多实例协调」里的边缘 leader 选举，不碰中心聚合器。

---

## 3. 候选方案

### 3.1 方案 A：第三方 raft（`hashicorp/raft` / `go.etcd.io/raft` / `lni/dragonboat`）

**做法**：引入成熟 raft 实现，多实例组成集群选主。

**为什么被拒**：

1. **直接推翻 ADR-0004 的 E1。** `hashicorp/raft` 的传递依赖至少包括 `armon/go-metrics`、`hashicorp/go-hclog`、`hashicorp/go-msgpack`、`hashicorp/go-sockaddr`、`mitchellh/mapstructure`，配 `raft-boltdb` 还要 `go.etcd.io/bbolt`——**8~10 个直接/传递第三方模块**。`go.etcd.io/raft` 稍小但仍需自备 WAL 与 transport，实际落地通常连 `etcd/server`（体量巨大）。`dragonboat` 更重（自带存储、需要 cgo 可选、交叉编译矩阵复杂）。这不是「加一个库」，是**把零依赖这个产品差异化支点拆掉**（ADR-0001 把「单二进制、可交叉编译、可无头运行」列为第一支点，权重 20）。
2. **体积与交叉编译代价**：raft + 其存储 + msgpack 会把二进制从「≤15MB 级」推向需要重新评估 40MB 上限的程度，且 `bbolt` 的 `mmap` 在 Windows/ARM 上有额外注意事项——每一平台都要重新验。
3. **它解决的是「复制状态机」，我们只需要「选主」。** raft 的价值在于「在不可靠网络上复制一份一致的日志」；我们没有任何需要复制的状态——采集数据走 Outbox（at-least-once 已落地）、配置走版本管理（已落地）、报警走引擎（已落地）。**用 raft 只为选一个主，是拿它 90% 的复杂度换 10% 的收益**，而且这 10%（强一致选主）在同主机场景下 `flock` 免费提供。

### 3.2 方案 B：自研 raft

**做法**：标准库手写一个 raft（选举超时、日志复制、快照、成员变更）。

**为什么被拒**：

1. **正确性风险不可接受。** raft 的 bug 是分布式系统里最隐蔽的一类：多数情况下能跑，只在网络分区、时钟漂移、成员变更的罕见交错下丢数据或双主。MIT 6.824 的参考实现要 2000~4000 行才勉强正确，而工业网关没有分布式测试集群去兜底这些路径。
2. **违反「慢工出细活」的收益比。** 把有限精力投在「手写共识算法」上，远不如投在协议编解码的现场正确性（那里一个字节错就是全站数据错）。自研 raft 是**看起来硬核、实则把风险后置**的选择。
3. **它仍然解决不了我们不需要解决的问题**（见 3.1 第 3 点）。

### 3.3 方案 C：`flock` 同主机强一致选主（**推荐**）

**做法**：

- 启动时 `open(<dataDir>/leader.lock, O_CREAT|O_RDWR|O_CLOEXEC, 0644)`，对 fd 调 `syscall.Flock(fd, LOCK_EX|LOCK_NB)`。
- 抢到 → 本实例为 `leader`；`EWOULDBLOCK` → 本实例为 `standby`。
- `standby` 每 5s（可配）重试抢锁；leader 进程崩溃/被杀时内核随 fd 关闭**自动释放锁**，`standby` 下一轮接管。
- 无心跳、无租约、无脑裂——**flock 是内核状态，进程死了就是死了，不存在「另一个进程以为它还活着」**。

**为什么这是对的选择**：

1. **零新依赖**：`syscall.Flock` 是标准库，`O_CLOEXEC` 是 `os` 默认行为，不碰 E1/E2/E3 任何一条。
2. **强一致、无脑裂**：内核保证同一时刻只有一个 fd 持有排他锁。这是**比 raft 更强**的保证（raft 只给线性一致性，且要多数派；flock 给的是「这台机器上唯一」）。
3. **精准命中真实问题**：见 §2.1，真实重复采集就发生在**同主机**。flock 把这类问题 100% 解决。
4. **崩溃自愈**：fd 关闭即释放，无需实现「租约过期判定」这类容易写错的逻辑。
5. **可测试**：`flock` 可以用「同一测试进程里开两个 fd 模拟两个实例」来单测，不依赖真实多进程。

**边界（必须写进文档与配置校验）**：

- `<dataDir>` 必须是**本地文件系统**。NFS 上 `flock` 的语义因实现而异（部分 NFS 服务端会把 flock 退化为 `fcntl` 记录锁，跨客户端不可靠）。启动时若检测到 dataDir 在 NFS/CIFS 上，应**告警并降级为「不做跨实例保证」**，而不是假装选主成功——**静默的假 leader 比没有 leader 更危险**。
- flock 是「打开文件描述符」级别的：同一进程 `fork` 出的子进程会继承 fd。Go 的 `os.OpenFile` 默认 `O_CLOEXEC`，但若未来某处 `exec` 子进程，必须确保 fd 不被继承（`O_CLOEXEC` 已覆盖）。

### 3.4 方案 D：`flock` + MQTT 跨主机软选主（**不推荐，但列出来说明为什么不**）

**做法**：同主机用 flock，跨主机用 MQTT retained 主题 + 租约（`{instanceId, tsMs, ttlMs}`）软选主。

**为什么不推荐**：

1. **脑裂窗口对写操作不安全。** MQTT 是软状态：两个实例同时认为自己是 leader 的窗口 = 租约 TTL（默认秒级）。对**采集**，这窗口的后果是「重复但幂等」——Outbox 去重、契约 v2 双发都已保证重复不丢不坏。但对**写操作**（点位写、控制命令、配置变更），脑裂窗口内两个实例都能写，**后写的覆盖先写的，且没有任何冲突检测**。而我们的语义要求「写操作须 leader」——用一个允许脑裂的机制去守一个不容脑裂的要求，是自相矛盾。
2. **跨主机重复采集在真实拓扑里几乎不存在。** 工业现场的正常形态是「每台网关负责一组设备」，不同网关管不同设备，天然不重复。会重复的情况是「两台网关配了同一组设备做冗余」，那是**热备**，应该由**中心聚合器**统一裁决（它能看到全部站点、能做配置下发与去重），而不是让每台边缘网关各自跑一套软选主。
3. **它把分布式系统的运维成本摊到每台边缘设备上。** MQTT 软选主要运维 broker 的 retained 语义、租约 TTL、时钟同步（租约靠 `tsMs` 判断过期，而**时钟跳变恰好是 G10 要监测的问题**）——每台边缘盒子都要正确配置这些，只为解决一个绝大多数现场不发生的问题。

**结论**：跨主机协调留给中心聚合器（独立路线图项）。边缘只做同主机 flock。这不是「偷懒」，是**把强一致需求交给能提供强一致的层**（同主机交给内核，跨主机交给中心）。

### 3.5 方案 E：不做，只暴露身份

**做法**：只把 `instanceId` 暴露到 `/api/v1/version`（已落地），不选主。

**为什么被拒**：这正是 `instanceid` 已落地但问题仍在的原因（§2.3）——它让实例**可区分**，但**不阻止两个实例都干活**。运维仍会看到重复采集。不做等于把问题留给现场。

### 3.6 方案对比

| 维度 | A 第三方 raft | B 自研 raft | **C flock（推荐）** | D flock+MQTT | E 不做 |
|---|---|---|---|---|---|
| 零第三方依赖（E1） | ❌ 8~10 模块 | ✅ | ✅ | ✅ | ✅ |
| 零 CGO / 交叉编译（E2） | ⚠️ bbolt mmap | ✅ | ✅ | ✅ | ✅ |
| 同主机强一致 | ✅ | ⚠️ 需正确 | ✅ 内核保证 | ✅ | ❌ |
| 跨主机强一致 | ✅ | ⚠️ | ❌ 明确 out of scope | ❌ 软状态 | ❌ |
| 写操作安全 | ✅ | ⚠️ | ✅（同主机） | ❌ 脑裂窗口 | ❌ |
| 实现复杂度 | 中（集成） | **极高** | **低** | 中 | 无 |
| 解决真实问题 | 过度 | 过度 | **精准** | 过度 | 不解决 |

---

## 4. 设计要点（方案 C 的具体契约）

### 4.1 纯包 `internal/leader`

判定与 I/O 分离，沿用本仓「纯判定可单测」的一贯做法（见 `watchdog` / `clockmon` / `heartbeat`）：

- **`Election` 接口**：`Acquire() (bool, error)` / `Release() error` / `IsLeader() bool`。
- **`FileElection`**：基于 `flock` 的实现。`Acquire` 用 `LOCK_EX|LOCK_NB`；返回 `false, nil` 表示「别人持有」（正常 standby），返回 `true, nil` 表示「本实例成为 leader」，返回 `err` 表示「无法判定」（如 dataDir 不可写）——**无法判定必须 fail-closed**（当作非 leader，并告警），绝不 fail-open（当作 leader 去采集）。
- **文件系统类型探测**：`<dataDir>` 在 NFS/CIFS 上时，`FileElection` 应返回一个明确的 `ErrUnsupportedFS`，由 runtime 决定「降级为单实例假设 + 告警」还是「拒绝启动」。默认**告警后按单实例运行**（工业现场 NFS 是少数，且拒绝启动会误伤），但告警必须显式（`leader_unsupported_fs`），不能静默。

### 4.2 runtime 接线

- 启动时构造 `leader.Election`，`Acquire` 一次确定初始角色；`standby` 起一个 5s ticker 重试（**独立协程，但只有一个 ticker**，不挂 watchdog——watchdog 的语义是采集中枢活性，leader 重试是另一件事；混在一起会让「leader 重试」污染 watchdog 的 tick 语义）。
- **采集与投递的闸**：collector 的调度入口在 `!IsLeader()` 时直接跳过（不产生请求、不产生 Outbox 记录）。这比「采了再丢弃」正确——后者会浪费协议往返、污染成功率的统计。
- **写操作的闸**：`POST /api/v1/points/*/write`、`POST /api/v1/config/*`、`POST /api/v1/commands`、`POST /api/v1/alarms/*/ack` 在 `!IsLeader()` 时返回 `409 Conflict`，body 为 `{"error":"not_leader","leader":{"instanceId":"..."},"message":"..."}`。**读操作（GET）不受影响**——运维要能看到 standby 的健康与配置。
- **角色变化的下游通知**：`leader → standby`（锁丢失，理论上只在异常时）与 `standby → leader`（抢到锁）都必须写日志（`leader_acquired` / `leader_lost`）并推进指标（`uic_leader_is_leader` gauge、`uic_leader_changes_total{to}` counter）。

### 4.3 契约暴露

- **`GET /api/v1/leader`**：`{"instanceId":"...","role":"leader|standby","since":"RFC3339","lockPath":"<dataDir>/leader.lock"}`。无锁配置时 `role` 恒为 `leader`（单实例默认），`since` 为进程启动时刻。
- **`/api/v1/health`** 加 `leader` 块（`role` / `instanceId` / `changes`）。**不参与 `status` 判定**——standby 不是故障，是正常角色（与 `clock` 块的「事实非故障」同理）。
- **指标**：`uic_leader_is_leader`（gauge，1/0）、`uic_leader_changes_total{to="leader|standby"}`（counter）、`uic_leader_acquire_errors_total`（counter）。

### 4.4 与 `instanceid` 的关系

leader 的 `instanceId` **直接复用 `instanceid.Load`** 的结果，不另造身份。这样 `/api/v1/leader` 的 `instanceId` 与 `/api/v1/version`、诊断包 `manifest.json` 里的完全一致——运维凭一个 ID 就能在三个地方对上同一台实例。

### 4.5 配置

```json
"leader": {
  "enabled": true,          // 默认 false（单实例行为不变）
  "lockFile": "leader.lock", // 相对 dataDir
  "retryInterval": "5s"
}
```

- **默认 `enabled: false`**：单实例部署（绝大多数）不受影响，行为与今天完全一致。
- `enabled: true` 时才抢锁；`lockFile` 必须落在 dataDir 内（防止误配到系统目录）。
- 配置进 `Canonical()` 哈希，版本化与回滚天然支持。

---

## 5. 与参考方案的差异

| 维度 | 参考实现常见做法（raft/etcd/zk） | 本决策 |
|---|---|---|
| 选主机制 | 分布式共识（多数派） | 同主机内核 flock（单机唯一） |
| 跨主机 | 一并解决 | **明确 out of scope，交给中心聚合器** |
| 依赖 | 引入共识库 | 零依赖（标准库 `syscall`） |
| 心跳/租约 | 需要（判定存活） | 不需要（fd 关闭即释放） |
| 脑裂 | 靠多数派避免 | 同主机不存在脑裂；跨主机不做 |
| 运维成本 | 每台边缘设备都要正确配置 | 一个配置文件项，默认关闭 |

---

## 6. 后果

### 6.1 正面

- **精准解决同主机重复采集**（§2.1 全部三种场景），且是内核强一致，比任何软选主都可靠。
- **零新依赖**，ADR-0004 的 E1/E2/E3 一条不破，`make sbom` 依然「依赖面如实为零」。
- **默认关闭**，单实例行为零变化，无回归风险。
- **写操作安全**：同主机内 flock 保证唯一 leader，写操作 409 语义清晰。
- **可测试**：flock 可在一个测试进程里用两个 fd 模拟，不依赖真实多进程；跨进程行为可用 `go test` 起子进程验（`os/exec`）。

### 6.2 负面与缓解

| 负面 | 缓解 |
|---|---|
| 跨主机重复采集未解决 | 文档明确「跨主机协调交给中心聚合器」，并在 `05-northbound-api-spec` 的 leader 小节写清边界；真需要时按 §7 重新评估 |
| NFS 上 flock 不可靠 | 启动探测文件系统类型，NFS/CIFS 告警 `leader_unsupported_fs`，按单实例运行（显式告警，不静默） |
| 同主机 flock 只能防「同一 dataDir」 | 文档要求：做 leader 选举的多实例必须共享同一个 dataDir；不同 dataDir 视为不同实例（各自无条件 leader）——这是正确语义 |
| standby 不采集，设备在 standby 侧显示「无数据」 | 这是 leader 语义的必然：standby 本来就不该有数据。`/api/v1/leader` 明确标注 `role: standby`，运维不会误判 |

### 6.3 退出条件 / revisit 触发器

出现以下任一情况时，重新评估本决策（**届时另开 ADR**，不在本 ADR 上打补丁）：

1. **现场出现「两台及以上跨主机网关必须对同一批设备做去重」的真实需求**——先评估「中心聚合器」能否满足；若必须边缘强一致，再评估 raft（此时应连同「是否值得推翻 ADR-0004」一起决策）。
2. **本仓需要跨主机复制「状态」而非仅选主**（例如配置必须在多实例间强一致同步）——那是 raft/etcd 的正当场景，但仍应优先考虑中心聚合器架构。
3. **NFS/CIFS 成为主流部署载体**（当前判断是少数）——flock 的降级告警会频繁出现，需重新设计跨主机方案。
4. **ADR-0004 的零依赖约束被推翻**（例如产品战略转向「允许 vendor 依赖」）——届时 raft 的集成成本大幅下降，可重新评估。

---

## 7. 验证（变异测试计划）

沿用本仓「先编译通过、再让测试变红」的硬标准，预计 20+ 变异体：

| 类别 | 变异体（必须变红） |
|---|---|
| flock 判定 | `LOCK_NB` 改成阻塞 `LOCK_EX`（死锁/挂起）；`EWOULDBLOCK` 当成错误而非「别人持有」；抢锁成功返回 `false`；抢锁失败返回 `true`（fail-open） |
| 角色 | `IsLeader` 恒 `true`；`IsLeader` 恒 `false`；角色变化不写日志；角色变化不推进指标 |
| runtime 闸 | 非 leader 仍采集；非 leader 仍投递；非 leader 写操作返回 200；非 leader 读操作被拒（读必须放行） |
| 释放 | `Release` 不关 fd；崩溃后不释放（用子进程验证）；`Release` 后仍报 leader |
| 文件系统 | NFS 探测恒成功；NFS 不告警；`lockFile` 允许逃出 dataDir |
| 契约 | `/api/v1/leader` 缺 `role`；`instanceId` 与 `/version` 不一致；`health.leader` 参与 status 判定（standby 被误判故障） |
| 配置 | `enabled` 默认 `true`（改变单实例行为）；`retryInterval<=0` 不回退默认 |

---

## 8. 关键结论

1. **leader 选举要回答的是「谁有资格采集与写入」，而真实问题只发生在同主机**——`flock` 用内核强一致解决它，零依赖、无脑裂、崩溃自愈。
2. **跨主机协调属于「中心聚合器」**，不该让每台边缘网关背分布式共识。这是职责划分，不是能力缺失。
3. **raft 被明确拒绝**：第三方违反 ADR-0004，自研高风险，且它解决的是我们不需要的「复制状态机」。若未来真的有跨主机强一致需求，**另开 ADR** 重新评估，而不是在本功能里顺带引入。
4. **默认关闭、写操作 409、读操作放行、不参与 status**——四条语义边界确保单实例零变化、运维可观测、故障可定位。

> 一句话收尾：**把强一致需求交给能提供强一致的层**——同主机交给内核（flock），跨主机交给中心聚合器。边缘网关的职责是采集与投递，不是共识。
