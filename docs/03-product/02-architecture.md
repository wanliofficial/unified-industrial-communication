# 总体架构

> **版本**：v0.1
> **上游**：`01-prd.md`
> **下游**：`03-domain-model.md`、`04-driver-sdk-spec.md`、`05-northbound-api-spec.md`
> **代码对应**：仓库根目录的 Go module `uic`（骨架实现）

---

## 1. 架构目标

| 目标 | 架构手段 | 验证方式 |
|---|---|---|
| 单二进制、可交叉编译 | 零 CGO、纯 Go 依赖、`go:embed` 前端 | CI 四平台构建 |
| 无头运行 | 运行时库 + `cmd/uic` 入口；UI 只是 HTTP 客户端 | 容器内启动测试 |
| 数百设备可控 | 时间片 + 设备 actor + 通道信号量 | 1000 设备压测 |
| 故障不扩散 | 每设备熔断 + 有界队列 + 降级 | 故障注入测试 |
| 配置可回滚 | 配置版本 + 快照 + 状态文件 | 注入非法配置 |
| 可诊断 | 指标 + 日志 + 健康 + 诊断包 | 故障演练 |
| 驱动可测试 | Codec 纯函数 + 虚拟设备 + 金报文 | `go test` |

---

## 2. 进程与部署视图

```
┌─────────────────────────────────────────────────────────────┐
│                        单进程 uic                            │
│                                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────────┐  │
│  │  HTTP    │  │  SSE     │  │  MQTT    │  │  OPC UA    │  │
│  │  API     │  │  Hub     │  │  Sink    │  │  Server    │  │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └─────┬──────┘  │
│       │             │             │              │          │
│  ┌────┴─────────────┴─────────────┴──────────────┴──────┐   │
│  │                    cache（唯一快照来源）              │   │
│  └──────────────────────────┬───────────────────────────┘   │
│                             │                               │
│  ┌──────────────────────────┴───────────────────────────┐   │
│  │              runtime（编排 / supervisor）             │   │
│  │   scheduler ── pipeline ── outbox ── watchdog        │   │
│  └──────────────────────────┬───────────────────────────┘   │
│                             │                               │
│  ┌──────────────────────────┴───────────────────────────┐   │
│  │                 driver registry                      │   │
│  │   modbus-tcp │ modbus-rtu │ s7 │ opcua │ mqtt │ virtual│
│  └──────────────────────────┬───────────────────────────┘   │
│                             │                               │
│  ┌──────────────────────────┴───────────────────────────┐   │
│  │   transport（tcp / serial / tls / loopback）          │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
        │                    │                    │
     PLC/仪表            MQTT Broker          SCADA/UA 客户端
```

**部署形态**：

| 形态 | 产物 | 启动方式 |
|---|---|---|
| Linux 裸机 | `uic` 单文件 + `/etc/uic/config.json` + `/var/lib/uic` | systemd unit |
| Windows | `uic.exe` + `config.json` + `data\` | Windows 服务（`uic -install-service`） |
| 容器 | distroless 镜像 | `docker run -v ...` |
| 开发 | `go run ./cmd/uic -config dev.json` | 前台 |

---

## 3. 模块与依赖方向

```
cmd/uic
  └→ internal/runtime
       ├→ internal/config      （配置加载/校验/版本/回滚）
       ├→ internal/scheduler   （时间片 + actor + 通道闸门）
       │    ├→ internal/planner（批读规划）
       │    ├→ internal/driver （接口 + 注册表）
       │    └→ internal/pipeline（缩放/死区/质量传播）
       ├→ internal/cache       （快照 + 订阅）
       ├→ internal/outbox      （持久队列）
       ├→ internal/northbound  （http / sse / mqtt / opcua）
       └→ internal/obs         （指标 + 日志）

internal/driver/modbus  ──→ internal/codec/modbus（纯函数）
                        ──→ internal/transport
internal/driver/virtual ──→ internal/model
internal/sim/modbus     ──→ internal/codec/modbus（复用同一编解码）
```

**硬性规则**：

| 规则 | 原因 |
|---|---|
| `model` 不依赖任何其他内部包 | 领域模型是共同语言 |
| `codec` 只依赖 `model`，不依赖 `transport` | 纯函数，可单测、可被从站复用 |
| `driver` 不依赖 `scheduler` | 驱动不知道谁在调它，便于测试与复用 |
| `northbound` 只读 `cache`，不直接读驱动 | 单一事实来源 |
| `runtime` 是唯一知道全部模块的地方 | 依赖注入集中在一处 |

---

## 4. 核心数据流

### 4.1 采集路径

```
ticker(50ms)
  └→ scheduler 取到期设备（最小堆 by nextDue）
       └→ device actor（每设备一个容量 1 的 mailbox，已有任务则跳过）
            └→ channel gate（信号量，写优先）
                 └→ planner.Plan(device.Points) → []ReadSpan
                      └→ driver.Read(ctx, span)
                           ├→ codec.EncodeRead → transport.RoundTrip → codec.DecodeRead
                           └→ error → classify（超时/断连/异常码/地址错）
                 └→ pipeline.Apply(point, raw) → Sample
                      ├→ cache.Set(sample)          → SSE/MQTT/UA 订阅者
                      ├→ outbox.Enqueue(sample)     → 持久化后投递
                      └→ obs.Record(...)            → 指标
```

### 4.2 写入路径（写优先）

```
HTTP POST /points/{d}/{p}/write
  └→ auth 校验 + 权限
       └→ scheduler.SubmitWrite（进入设备 actor 的高优先级队列）
            └→ channel gate.AcquireWrite（等待写标记，读请求让行）
                 └→ driver.Write
                      └→ cache.Set（乐观更新为 Uncertain）
                           └→ 下一次采集确认（Good/Bad）
```

**为什么写要单独排队**：读请求是「数量多、可延迟」，写请求是「数量少、延迟敏感」。共用一个 FIFO 会让写排在 200 个读后面。

### 4.3 北向路径

```
cache 变更事件
  ├→ SSE Hub：按订阅过滤 → 每客户端有界缓冲 → 慢客户端丢弃最旧
  ├→ Outbox：写入 WAL → 投递 worker → 成功后删除
  ├→ OPC UA：更新节点值 + 触发 DataChange 通知
  └→ 指标：计数/延迟
```

---

## 5. 并发模型

### 5.1 三类并发单元

| 单元 | 数量级 | 职责 | 生命周期 |
|---|---|---|---|
| 调度协程 | 1 | tick 推进、到期判定、投递 | 进程级 |
| 设备任务 | 每设备 ≤ 1 个在飞 | 一次采集（含批量读） | 采集级 |
| 投递 worker | 每 sink 1–4 个 | Outbox 投递 | 进程级 |
| HTTP handler | 每请求 1 个 | API | 请求级 |

**关键约束**：同一设备的采集任务永不并发。实现方式是「每设备一个容量为 1 的 channel」，投递失败即跳过本轮（记录 `uic_scheduler_skipped_total`）。

### 5.2 通道闸门

```go
type Gate struct {
    sem     chan struct{} // 容量 = channel.Concurrency
    writeCh chan struct{} // 写优先：写请求先占位
}
```

规则：

1. 读请求：`select { case sem <- struct{}{}: ... case <-ctx.Done(): }`；
2. 写请求：先递增 `waitingWrites`，读请求在 `waitingWrites > 0` 时让行；
3. 串口通道：额外一把「端口锁」（同端口跨通道共享）。

### 5.3 代次隔离

每次设备启动/重连 `generation++`。采集任务携带启动时的代次；结果写回前比对，不匹配则丢弃并计数。

### 5.4 时间与超时

| 用途 | 时钟 | 原因 |
|---|---|---|
| 调度 tick | `time.Ticker`（单调） | 不受系统时间跳变影响 |
| 单次采集超时 | `context.WithTimeout` | 可取消 |
| 业务时间戳 | `time.Now().UTC()` | 可展示 |
| 设备时间 | 协议解析结果（可空） | 设备侧时标 |

---

## 6. 状态机

### 6.1 设备状态

```
        ┌────────────┐
        │ configured │  ← 配置已加载，未启动
        └─────┬──────┘
              │ start
        ┌─────▼──────┐   fail   ┌──────────┐
        │ connecting ├─────────→│ degraded │
        └─────┬──────┘          └────┬─────┘
              │ ok                    │ 探活成功
        ┌─────▼──────┐               │
        │ collecting │◄──────────────┘
        └─────┬──────┘
              │ 连续失败 ≥ N
        ┌─────▼──────┐
        │  faulted   │  ← 熔断，定期探活
        └─────┬──────┘
              │ stop
        ┌─────▼──────┐
        │  disabled  │
        └────────────┘
```

**API 必须同时返回 `configuredState` 与 `runtimeState`**，否则「配置了但没采」会被误判为正常。

### 6.2 质量码

| 码 | 含义 | 产生条件 |
|---|---|---|
| `good` | 可信 | 读成功且校验通过 |
| `uncertain` | 可疑 | 写入后未确认、值跳变超限 |
| `stale` | 过期 | 超过 3 个采集周期未更新 |
| `bad` | 不可信 | 解析失败、越界、类型不符 |
| `timeout` | 超时 | 请求超时 |
| `comm_error` | 通信错误 | 连接断开、CRC 错、异常响应 |
| `address_error` | 地址错误 | 异常码 02、符号不存在 |
| `not_connected` | 未连接 | 初始状态 |
| `disabled` | 已禁用 | 点位被禁用 |

---

## 7. 存储布局

```
/var/lib/uic/
  ├── config.json           当前生效配置（原子写）
  ├── config.state.json     版本状态（current/desired/backup/rollbackCount）
  ├── snapshots/
  │     ├── v-<hash>.json   历史配置快照（保留 N 份）
  │     └── ...
  ├── outbox/
  │     ├── wal.log         追加写日志（记录/确认）
  │     └── cursor.json     各 sink 的确认位点
  ├── journal.ndjson        本地归档 sink（storage.journal=true 时）
  ├── history.db            SQLite：历史数据（Wave 2）
  └── uic.log               结构化日志（轮转）
```

**四个写者分离的理由**：

| 库 | 写频率 | 失败影响 | 策略 |
|---|---|---|---|
| 配置 | 极低 | 致命 | 原子写 + 备份 |
| Outbox | 高 | 数据丢失 | 追加写 + 定期压缩 |
| journal | 高 | 可丢 | 追加写 + 批量 flush，不逐条 fsync |
| 历史 | 高 | 可丢 | 有界 + 降采样 |

`journal.ndjson` 与 `outbox/` 的区别在于**职责**：Outbox 是「还没送出去的东西」，会被确认、删除、压缩；journal 是「已经发生的事实」，只追加、不删除，由外部轮转或运维清理。因此 journal 不参与 Outbox 的三重上限判定，它的增长由磁盘容量与日志轮转策略兜底。开启方式见 `03-domain-model.md` §10.0.1。

---

## 8. 关键接口（Go 签名）

> 完整规范见 `04-driver-sdk-spec.md`；此处只列骨架，确保架构可落地。

```go
// model
type AddressKind string // register | symbol | object | file

// driver
type Driver interface {
    Meta() Meta
    Open(ctx context.Context, cfg DeviceConfig) (Conn, error)
}

type Conn interface {
    Probe(ctx context.Context) error
    Read(ctx context.Context, spans []ReadSpan) ([]RawSample, error)
    Write(ctx context.Context, reqs []WriteRequest) ([]WriteResult, error)
    Close() error
}

// codec（纯函数，无 I/O、无状态）
type Codec interface {
    EncodeRead(ReadSpan) ([]byte, error)
    DecodeRead(req ReadSpan, resp []byte) ([]RawSample, error)
    EncodeWrite(WriteRequest) ([]byte, error)
    DecodeWrite(WriteRequest, []byte) (WriteResult, error)
}

// transport
type Transport interface {
    RoundTrip(ctx context.Context, req []byte) ([]byte, error)
    Close() error
}
```

**三条设计约束**：

1. `Codec` 必须是纯函数——同一份代码被主站、从站、仿真器、测试四处复用；
2. `Conn.Read` 接收 `[]ReadSpan` 而不是单点——批量优化在驱动外完成，但驱动可以选择进一步合并；
3. 所有 I/O 方法都带 `context.Context`——超时与取消是默认能力，不是可选项。

---

## 9. 可观测性设计

### 9.1 指标命名

```
uic_<subsystem>_<name>{<labels>}
```

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `uic_collect_total` | counter | `device,protocol,status` | 采集次数与结果 |
| `uic_collect_duration_seconds` | histogram | `device,protocol` | 采集耗时 |
| `uic_device_state` | gauge | `device,state` | 状态（1 表示当前态） |
| `uic_point_quality` | gauge | `device,quality` | 各质量码点位数 |
| `uic_scheduler_skipped_total` | counter | `device,reason` | 跳过轮次 |
| `uic_channel_inflight` | gauge | `channel` | 通道在飞请求数 |
| `uic_outbox_pending` | gauge | `sink` | 待投递条数 |
| `uic_outbox_dropped_total` | counter | `sink,reason` | 丢弃（ttl/size/count） |
| `uic_http_requests_total` | counter | `path,code` | API 请求 |
| `uic_sse_clients` | gauge | — | SSE 连接数 |

### 9.2 日志字段（最小集）

```json
{"ts":"...","level":"info","msg":"read_failed","device":"plc-01","channel":"ch1",
 "protocol":"modbus-tcp","op":"read","err":"timeout","duration_ms":3000,"generation":7}
```

约束：

- 必须有 `device` / `channel` / `protocol`（能按设备过滤）；
- 不允许打印凭据与完整帧（帧只打印前 32 字节 + 长度）；
- 高频错误做采样（同一设备同一错误每分钟最多 N 条）。

### 9.3 健康检查

| 端点 | 语义 | 失败含义 |
|---|---|---|
| `/healthz` | 进程存活 | 进程应被重启 |
| `/readyz` | 依赖就绪（配置已加载、存储可写、至少一个驱动可用） | 不应接收流量 |
| `/api/v1/health` | 业务聚合（每设备状态、成功率、队列水位） | 需要人看 |

---

## 10. 安全边界

| 边界 | 控制 |
|---|---|
| 浏览器 → API | TLS + Bearer Token + CORS 白名单 |
| API → 设备 | 只允许配置中的地址（禁止 SSRF 式任意地址） |
| 驱动 → 主进程 | 第一版同进程（可信）；第二版进程外（隔离） |
| 配置 → 执行 | 远程动作白名单 + Schema 校验 + 版本单调 |
| 日志/诊断包 | 凭据脱敏 + 帧截断 |
| 依赖 | 白名单 + SBOM |

---

## 11. 启动与关闭顺序

### 11.1 启动

```
1. 解析参数（-config / -listen / -log-level）
2. 加载配置 → Schema 校验 → 失败则进入安全模式（仅 /healthz + /api/v1/config/validate）
3. 初始化存储（目录、权限、磁盘水位检查）
4. 初始化指标与日志
5. 启动 cache 与 outbox（重放未确认）
6. 启动驱动注册表（内置驱动）
7. 启动 scheduler（设备按通道分组）
8. 启动北向（HTTP → SSE → MQTT → OPC UA）
9. 标记 ready
```

### 11.2 关闭（`SIGTERM`）

```
1. 标记 not ready（停止接收新请求）
2. 停止 scheduler（取消所有采集 context）
3. 等待在飞采集结束（≤ 5s）
4. flush outbox（≤ 3s，未完成则保留）
5. 关闭驱动连接
6. 关闭 HTTP 服务（优雅）
7. 退出
```

**总预算 10 秒**，超时则强制退出并记录。

---

## 12. 技术选型摘要

| 领域 | 选择 | 备选与否决理由 |
|---|---|---|
| 语言 | Go 1.22+ | 见 ADR-0001 |
| HTTP | 标准库 `net/http`（1.22 路由增强） | 不引入框架，减少依赖 |
| 日志 | `log/slog` | 结构化、标准库 |
| 配置 | JSON + 手写校验 | 不引入 YAML 依赖；JSON 可被机器生成 |
| 存储 | 文件（配置/Outbox）+ SQLite（历史，Wave 2） | 单文件、可备份 |
| MQTT | 自研最小客户端（骨架）/ 成熟库（生产） | 骨架零依赖 |
| OPC UA | 自研封装（生产用 gopcua） | 见 ADR-0001 |
| 前端 | 单文件 HTML + 原生 JS（骨架）/ 轻量 SPA（生产） | `go:embed` 进二进制 |
| 测试 | 标准 `testing` + 自研 drivertest | 不引入断言库 |

---

## 13. 与参考方案的差异（一句话对照）

| 维度 | 参考方案常见做法 | UIC 的做法 |
|---|---|---|
| 进程形态 | 桌面壳 + 采集同进程 | 无头服务 + 浏览器客户端 |
| 驱动扩展 | 枚举 + switch | 注册表 + 能力四态 + 契约版本 |
| 调度 | 每设备一线程 | 时间片 + actor + 通道闸门 |
| 配置 | 单文件，改后重启 | 版本 + 差异 + 回滚 + 热更新 |
| 断网 | 内存队列 | 持久 Outbox + TTL + 限速重放 |
| 测试 | 依赖真机 | 虚拟设备 + 故障注入 + 金报文 |
| 部署 | 安装包 / 容器 | 单二进制 + 可选容器 |

---

## 14. 架构决策记录（索引）

| ADR | 主题 | 状态 |
|---|---|---|
| 0001 | 语言选型（Go + C# 辅助） | Accepted |
| 0002 | 四类寻址统一模型 | 见 `03-domain-model.md` |
| 0003 | 驱动三层分离 | 见 `04-driver-sdk-spec.md` |
| 0004 | 北向四通道 | 见 `05-northbound-api-spec.md` |
| 0005 | Outbox 语义（at-least-once） | 见本文 §4.3 与 `06-nfr.md` |
