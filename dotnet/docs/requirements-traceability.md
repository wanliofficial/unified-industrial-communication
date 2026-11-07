# 需求追溯

这份表回答一个问题：**`docs/03-product/01-prd.md` 里的每条 FR/NFR，在本仓库里到底落地到了哪一步？**

- 「已实现」= 有代码，且有能自动跑的用例或脚本作为证据；证据名就是 `dotnet run --project tests/Uic.Tests -- --filter <名>` 能跑到的用例名；
- 「部分实现」= 主干能用，但验收标准里有明确没做到的部分，写在备注里；
- 「刻意没做」= 有意识不做，给出原因与补法（与 [implementation-notes.md](implementation-notes.md) 的「刻意没做」一致）。

首版的目标范围是 PRD §6.1 的 Wave 1 子集：**Modbus 三封装 + 虚拟设备 + 四北向通道 + 配置治理 + Outbox + 可观测**。
需要说清楚的一点：**S7、OPC UA 客户端、MQTT 南向在 PRD 里是 M 级要求，本版本没有实现**，
它们不是 §6.1「不做」清单里的项，属于真实缺口；逐协议现状、原因与补法见
[protocol-status.md](protocol-status.md)。

当前测试基线：**161 passed / 0 failed / 1 skipped**（跳过的是需要 `--opcua` 的 OPC UA 端到端用例）。

## 1. 采集（FR-COL）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-COL-01 | Modbus TCP 主站，FC1/2/3/4/5/6/15/16 | 已实现 | `golden/read_coils_10`、`golden/write_multiple_regs_float32`、`loopback/*`；编解码覆盖 0x01–0x06/0x0F/0x10 |
| FR-COL-02 | Modbus RTU/ASCII 主站，含串口参数与帧间延时 | 部分实现 | `serial/rtu_golden_frame_over_pty`、`serial/ascii_golden_frame_over_pty`、`serial/half_duplex_does_not_cross_responses`；波特率 300–921600 可配，半双工由通道闸门 + 发送前清空输入缓冲保证。**没有单独的 3.5 字符静默定时器**，帧边界靠长度/功能码 + CRC 判定；遇到挑剔帧间静默的仪表要在 `RtuFraming` 补 |
| FR-COL-03 | Modbus TCP 从站 | 已实现 | 内置从站被主站回环读写：`loopback/write_then_read_back`、`loopback/write_to_missing_register_is_rejected` |
| FR-COL-04 | S7comm 主站 | 未实现（首版缺口） | PRD 列为 M，本版本没有代码。驱动边界只有 `IDriver`/`IConn`，接入时按 [protocol-status.md](protocol-status.md) 的五步走；TPKT/COTP 分帧、PDU 协商、DB/IB/QB/MB 区都要新写 |
| FR-COL-05 | OPC UA 客户端 | 未实现（首版缺口） | PRD 列为 M，本版本只实现 OPC UA **服务端**（`Uic.OpcUa`）。客户端需要会话/订阅/证书/重连，建议直接基于 `Opc.Ua.Client` 而不是手写栈 |
| FR-COL-06 | MQTT 南向订阅取数 | 未实现（首版缺口） | PRD 列为 M，本版本的 MQTT 只做北向发布与下行命令；南向取数需要另一套连接生命周期、点位映射与质量传播，不能复用北向客户端 |
| FR-COL-07 | 虚拟设备（正弦/斜坡/随机/常量/故障注入） | 已实现 | `loopback/*`、`contract/*` 都以 `virtual` 驱动跑通全链路；五类故障注入见 `Uic.Driver.Virtual` |
| FR-COL-08 | 驱动能力四态声明 | 已实现 | `GET /api/v1/drivers` 暴露 `Capabilities`；控制台「驱动」页渲染八项四态徽标、设备页显示读写/订阅/浏览四项；`contract/drivers_expose_capability_states` 锁住四态线上名字 |
| FR-COL-09 | 批量读规划（合并 / 非法跨越拒绝） | 已实现 | `planner/*` 八条：相邻合并、跨空洞不合并、按寄存器与位上限切分、按区域分离、每点恰好覆盖一次 |
| FR-COL-10 | 每点位独立采集周期 | 刻意没做（S） | 当前只有设备级周期。补法：`DeviceActor` 已按 `PointRef` 规划窗口，把点位按 `EffectiveInterval` 分桶即可，不需要改驱动契约 |
| FR-COL-11 | 写入（单点/多点），写后校验与超时 | 已实现 | 单点/多点、逐点裁决、超时三态（`ok`/`disabled`/`timeout`）已实现：`contract/write_returns_per_point_results`。`confirm: true` 在同一个通道租约内立即回读并比较，结果带 `verified`；不一致返回 `ok=false` + `uncertain` + `verify.mismatch`：`engine/write_confirmation_reads_back`、`engine/write_confirmation_detects_mismatch`、`engine/write_without_confirmation_skips_readback` |
| FR-COL-12 | 订阅型协议（COV/事件/通知） | 刻意没做（C） | 路线图 Wave 2；`Capabilities.Subscribe` 已有四态占位 |

## 2. 数据模型与配置（FR-CFG）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-CFG-01 | 四类寻址统一模型 | 部分实现 | `Address` 已含 register/symbol/object/file 四类字段与校验；**只有 register 有驱动实现**，其余三类是扩展点 |
| FR-CFG-02 | 数据类型系统（≥8 类，含数组） | 部分实现 | 12 种标量类型（bool/int/uint 16/32/64/float 32/64/string/BCD16/BCD32/bytes）；**datetime 与数组未做** |
| FR-CFG-03 | 字节序 ABCD/CDAB/BADC/DCBA | 已实现 | `codec/byte_order_encodes_as_expected`、`codec/byte_order_roundtrip`、`json/enums_are_lowercase_wire_names` |
| FR-CFG-04 | 设备模板 + 参数化实例化（≥50 实例） | 已实现 | `template/instantiate_fifty_devices`（50 个实例逐个断言）、`contract/template_instantiates_and_applies_devices`、`template/rejects_bad_parameters`、`template/store_rejects_unknown_placeholder` |
| FR-CFG-05 | 点表 CSV 导入（地址表达式 + 命名规则） | 已实现 | `csv/address_expressions`、`csv/import_merges_and_reports_conflicts`、`contract/points_csv_import_previews_and_applies`；支持 `40001` / `holding:100` / 分列三种写法 |
| FR-CFG-06 | 命名唯一性与非法字符校验 | 已实现 | `config/rejects_invalid_documents`；通道/设备/点位各自的重复 id 与命名规则都在 `ModelValidator` 里一次列全 |
| FR-CFG-07 | 配置 Schema 校验（启动即校验） | 已实现 | `config/rejects_invalid_documents`、`contract/config_validate_reports_problems`；非法配置退出码 2 |
| FR-CFG-08 | 配置版本化（内容哈希）+ 历史列表 | 已实现 | `config/id_is_content_addressed`、`config/recommit_of_identical_content_keeps_one_version`、`config/bootstrap_prefers_current_pointer` |
| FR-CFG-09 | 配置差异对比 | 已实现 | `config/diff_finds_changed_added_removed`、`config/diff_text_is_human_readable`；字段级 JSON Pointer |
| FR-CFG-10 | 一键回滚（回滚也产生新版本） | 已实现 | `config/rollback_adds_a_version` |
| FR-CFG-11 | 配置原子写（临时文件 + fsync + rename） | 已实现 | `ConfigStore.AtomicWrite`；`config/reload_after_restart` 验证重启后仍读得到 |
| FR-CFG-12 | 热更新：只重启受影响设备 | 已实现 | `contract/template_instantiates_and_applies_devices`（apply 后返回 `restartedDevices`）；diff 按设备/点位粒度 |
| FR-CFG-13 | 配置导入/导出（跨版本迁移） | 已实现 | `config/migration_is_explicit_and_versioned`、`config/migrates_v0_legacy_shapes`、`config/repository_examples_validate`（仓库 `examples/` 三个示例都能直接 `check` 通过）、`contract/config_export_import_round_trip`、`contract/config_import_migrates_legacy_shape`。迁移覆盖 `server.*`、`storage.dir`、`concurrency`、`devices[].channel`、transport 对象、数字 stopBits；时长接受 Go 复合写法 |

## 3. 调度与运行时（FR-RUN）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-RUN-01 | 时间片调度，避免同周期设备同时打满 | 已实现 | 每设备独立 `nextDue` + 50 ms tick；启动时按 `index/count` 把设备自己的 interval 均分（100 台 1s 设备即每 10 ms 一台），失败退避再叠加抖动：`schedule/phase_spreads_devices`、`schedule/scheduler_assigns_phases` |
| FR-RUN-02 | 每设备串行、跨设备并行 | 已实现 | `engine/conn_is_never_called_concurrently`、`loopback/*` |
| FR-RUN-03 | 通道并发上限与写优先 | 已实现 | `ChannelGate`：写等待计数让新读让路；`engine/conn_is_never_called_concurrently` 验证不重叠 |
| FR-RUN-04 | 指数退避 + 抖动重连 | 已实现 | `DeviceActor.LoopAsync`：`interval × 2^failures` 上限为 `RecoverInterval`，叠加 10% 抖动；`devices[].retries` 只重试临时性失败，重试前先断开避免迟到帧：`engine/transient_failure_is_retried`、`engine/bad_quality_batch_is_retried`、`engine/deadline_is_retried_as_timeout`、`engine/permanent_failure_is_not_retried`、`engine/retries_are_finite` |
| FR-RUN-05 | 熔断（连续失败降级 + 定期探活） | 已实现 | `DeviceActor` 状态机 + `ProbeInterval`；`engine/actor_reports_state_and_generation`。`address` 类错误不参与熔断（点表问题不该把设备判离线）：`engine/permanent_failure_is_not_retried` |
| FR-RUN-06 | 代次隔离（旧结果丢弃） | 已实现 | `engine/actor_reports_state_and_generation`；`CollectOnceAsync` 逐样本比对代次 |
| FR-RUN-07 | 质量码 ≥8 态 + 时间戳三分离 | 已实现 | 9 态 `Quality`；`Sample` 含 `DeviceTime`/`CollectedAt`/`AppliedAt`，见 `json/datavalue_wire_shape_is_stable` |
| FR-RUN-08 | 单调时钟用于调度与超时 | 已实现 | `nextDue` 与等待都基于 `Stopwatch.GetTimestamp()` + `ScheduleMath`；NTP 校时/手工改钟不会推迟或提前采集。样本时间戳仍用墙上时钟：`schedule/monotonic_arithmetic` |
| FR-RUN-09 | 运行态与配置态分离 | 已实现 | `ConfiguredState` / `RuntimeState` 两套枚举，API 同时返回 |
| FR-RUN-10 | 看门狗（采集心跳 + 资源水位降级） | 已实现 | 采集心跳：`DeviceStateWatcher` 检测「超过 max(10 周期, 30 s) 无新样本」并记 `uic_watchdog_stale_total`；恢复由 `DeviceActor` 的读超时与重连完成，进程级僵死交给外部 supervisor（NFR 故障矩阵）。资源水位：`ResourceMonitor` 按 `storage.minFreeBytes/resumeFreeBytes/maxMemoryBytes` 判定，磁盘告急时暂停文件日志与 journal、健康转 `degraded`、`problems` 标 `$resources`，恢复后自动重启；证据 `resources/disk_pressure_has_hysteresis`、`resources/memory_pressure_has_hysteresis`、`resources/unreadable_probe_is_not_pressure`、`resources/file_log_pauses_during_pressure`、`contract/resource_pressure_degrades_health` |
| FR-RUN-11 | 优雅退出（停调度、flush 队列、关连接） | 已实现 | `GatewayRuntime.DisposeAsync` 先 flush（3 s）再发 `$death`；SIGTERM 由 Kestrel 的 `RunAsync` 触发生命周期 |

## 4. 数据处理（FR-PROC）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-PROC-01 | 线性缩放与偏移 | 已实现 | `pipeline/scale_and_offset`、`pipeline/rounding_for_integer_types` |
| FR-PROC-02 | 死区（绝对值/百分比/变化率） | 已实现 | `pipeline/deadband_percent_uses_range_span`、`pipeline/deadband_rate_uses_elapsed_time`、`cache/publish_configured_applies_point_rule`、`csv/imports_deadband_mode_and_range`；`Point.DeadbandMode` 与 `Pipeline.ExceedsDeadband` 三种模式，语义见 [configuration.md](configuration.md#死区) |
| FR-PROC-03 | 质量传播（Bad/Stale 不参与计算与上报） | 已实现 | `pipeline/bad_quality_skips_scaling`、`pipeline/bad_quality_preserves_reason`、`pipeline/deadband_reports_quality_transitions` |
| FR-PROC-04 | 单位与量程元数据 | 已实现 | `unit` 贯穿配置与北向；`rangeMin`/`rangeMax` 进入 `Point`、配置校验、CSV 导入与点位 JSON，证据 `config/deadband_and_range_rules`、`csv/imports_deadband_mode_and_range` |
| FR-PROC-05 | 计算点（表达式） | 刻意没做（C） | Wave 2；`Pipeline` 已留出质量传播点 |
| FR-PROC-06 | 报警判定 | 刻意没做（C） | Wave 2；需要独立的报警状态机、确认与抑制 |

## 5. 北向输出（FR-NOR）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-NOR-01 | REST：当前值、历史、配置、状态 | 部分实现 | 当前值/配置/状态已实现：`contract/devices_and_points_are_listed`；**历史查询未做**（见 FR-NOR-08） |
| FR-NOR-02 | REST：写入点位（需权限，三态结果） | 部分实现 | 三态结果与逐点裁决已实现：`contract/write_returns_per_point_results`；**权限是单一 Bearer Token**，没有 RBAC 细粒度 |
| FR-NOR-03 | SSE 实时推送（按订阅表达式） | 已实现 | `contract/*` 起真实 Kestrel；按 `device.point` / 通配过滤、质量过滤、心跳、50 路并发上限 |
| FR-NOR-04 | MQTT 上报（Topic 模板、QoS、保留） | 已实现 | `mqtt/sink_renders_topic_and_injects_schema_version`、`mqtt/publish_qos1_waits_for_puback`、`mqtt/reconnect_republishes_birth_and_state`；`scripts/mqtt-interop.sh` 用真实 broker 验一遍 |
| FR-NOR-05 | OPC UA 服务端 | 已实现 | `opcua/anonymous_session_read_write_subscribe`（需 `--opcua`） |
| FR-NOR-06 | Outbox 持久补发（至少一次） | 已实现 | `outbox/advances_only_on_success`、`outbox/replays_after_restart`、`outbox/tolerates_a_torn_last_line`；`mqtt/command_*` 验证重连后的位点 |
| FR-NOR-07 | 北向多目标并行 | 已实现 | `Outbox` 按 sink 分区，一个分区失败不拖住其他分区：`outbox/partitions_are_independent` |
| FR-NOR-08 | 历史查询（时间范围/降采样/分页） | 刻意没做（S） | 需要存储层与保留策略。补法：`journal` sink 已把样本落成本地 NDJSON，可先做「按时间扫文件 + 分页」，再换时序库 |
| FR-NOR-09 | Sparkplug B | 刻意没做（C） | Wave 2；可作为新的 MQTT sink 接入 |
| FR-NOR-10 | 数据契约版本（`schemaVersion`） | 已实现 | REST/SSE/MQTT/OPC UA 输出都带 `schemaVersion`；`json/unknown_fields_are_tolerated` 保证前向兼容 |

## 6. 运维与安全（FR-OPS）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-OPS-01 | Web 控制台（登录、设备、点位、监视、日志、配置） | 部分实现 | 六个页面都在（Uno WASM，含驱动能力页），`scripts/wasm-smoke.sh` 在 Playwright 里真跑首屏与 REST 调用；**没有「新建通道/设备」表单**，配置改动仍走配置页或 REST |
| FR-OPS-02 | 认证（本地账号 + 会话 Token，首次强制改密） | 刻意没做（M） | 首版只有配置里的单个 Bearer Token，没有账号库。这是与 PRD 差距最大的一条，见文末「红线」 |
| FR-OPS-03 | RBAC（页面级 + 操作级，≥3 角色） | 刻意没做（S） | 依赖 FR-OPS-02；命令白名单是硬编码能力边界，不是角色 |
| FR-OPS-04 | 审计日志（可查询、可导出） | 刻意没做（S） | 命令与配置变更只进环形日志，重启即清空；审计需要落盘与查询接口 |
| FR-OPS-05 | 指标（Prometheus 文本格式） | 已实现 | `GET /api/v1/metrics`；设备状态、样本发布、Outbox、OPC UA、看门狗都有序列；`/api/v1/health` 汇总 |
| FR-OPS-06 | 健康检查三级（live/ready/business） | 已实现 | `/healthz`、`/readyz`、`/api/v1/health`；`contract/health_and_version_are_public` |
| FR-OPS-07 | 诊断包（脱敏配置 + 日志 + 指标 + 版本） | 已实现 | `GET /api/v1/diagnostics/bundle` 与命令 `diag.bundle` 共用 `DiagnosticsBundle.Build`；`mqtt/command_diag_bundle_is_whitelisted` |
| FR-OPS-08 | 远程命令白名单 | 已实现 | `contract/*` 与 `mqtt/command_unknown_action_returns_error`、`mqtt/command_privileged_action_is_forbidden`；REST/MQTT 共用 `CommandExecutor` |
| FR-OPS-09 | 凭据加密存储 | 刻意没做（M） | 配置里的 token/口令是明文。**这是 PRD 安全红线之一**，见文末 |
| FR-OPS-10 | TLS（API 与北向） | 刻意没做（M） | 由反向代理 / stunnel 终结；本仓库只监听明文端口 |
| FR-OPS-11 | 日志（结构化、分级、轮转） | 已实现 | 控制台 + 内存环形缓冲 + 可选 JSON 行文件日志：字段 `ts/level/category/msg/device/exception`，按 `logging.file.maxBytes` 轮转、保留 `retain` 份；支持热更新开关：`logs/json_line_fields_are_complete`、`logs/file_rotates_without_losing_lines`、`logs/retention_prunes_old_archives`、`logs/reconfigure_stops_writing` |

## 7. 驱动开发与测试（FR-SDK）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-SDK-01 | 驱动三层：Codec / Transport / Driver | 已实现 | 项目切分即边界：`Uic.Codec.Modbus` 不依赖 `net`，`Uic.Transport` 只管字节流，`Uic.Driver.Modbus` 组装 |
| FR-SDK-02 | 能力与地址类型声明 | 已实现 | `DriverMeta.Capabilities` / `ConfigFields`；加载时不匹配直接拒绝 |
| FR-SDK-03 | 驱动契约语义化版本 | 已实现 | `DriverRegistry.Register` 校验 `DriverSdk.Version`，不匹配抛出并说明原因 |
| FR-SDK-04 | 虚拟设备（按协议实现从站） | 已实现 | `Uic.Host/Simulator.cs` 起 Modbus TCP 从站；`loopback/*` 全链路 |
| FR-SDK-05 | 故障注入（丢包/延迟/错帧/异常码/连接重置） | 已实现 | `Uic.Driver.Virtual` 的五类故障；`loopback/probe_times_out`、`loopback/silent_slave_times_out` |
| FR-SDK-06 | 金报文测试套件 | 已实现 | `testdata/golden/*.json` 16 份，`golden/*` 逐字节比对（TCP/RTU/ASCII） |
| FR-SDK-07 | 一致性测试套件（连接/超时/重连/并发/取消） | 已实现 | T1–T18 全部有对应用例，映射表见 [testing.md](testing.md)；单个驱动跑完约 30 s |
| FR-SDK-08 | 进程外驱动（gRPC） | 刻意没做（C） | Wave 3；需要先有稳定的 IPC 契约与进程生命周期管理 |

## 8. 非功能需求（NFR）

只列与首版代码直接相关的项；「未测量」不等于「不满足」，但也不能算通过。

| 类别 | 目标 | 状态 | 说明 |
|---|---|---|---|
| 容量 | 200 设备 / 10,000 点 | 未测量 | 需要批量实例化 + 压测脚本；模板实例化已能一次生成 50 台，可作为压测起点 |
| 采集周期下限 | 100 ms | 已实现 | `Validation` 拒绝小于 100 ms 的设备周期 |
| 写命令 P95 ≤ 300 ms | 读满载下 | 未测量 | `ChannelGate` 的写优先是机制保证，但没有基准测试数据 |
| 配置应用 ≤ 2 s | 改 1 台设备 | 已实现 | `contract/template_instantiates_and_applies_devices` 每次 apply 约 100–300 ms（含真实 Kestrel） |
| 配置回滚 ≤ 30 s | 全量 | 已实现 | `config/rollback_adds_a_version` 亚秒级 |
| 断网补发 24h 无空洞 | — | 已实现（机制） | `outbox/replays_after_restart` 验证位点与重放；24h 时长需要浸泡测试 |
| 配置回滚成功率 100% | 注入 20 类非法配置 | 部分实现 | `config/rejects_invalid_documents` 与 `contract/config_validate_reports_problems` 覆盖主要类别，未凑满 20 类 |
| 单设备故障隔离 | 不影响其他设备 | 已实现 | 每设备 actor + 通道闸门；`outbox/partitions_are_independent` 同样思路 |
| 采集假死检测 ≤ 60 s | — | 已实现 | `DeviceStateWatcher`：max(10 周期, 30 s) |
| 优雅退出 ≤ 10 s | SIGTERM | 已实现 | flush 3 s + `$death` 2 s 的预算，留有余量 |
| 掉电配置完整性 | 100% | 已实现（机制） | 临时文件 + `Flush(flushToDisk: true)` + `File.Move(overwrite)` |
| 故障注入矩阵 12 类 | 全通过 | 部分实现 | 连接拒绝/中断/超时/半包/错 CRC/异常码/部分响应/慢响应/Broker 断线都有用例；磁盘满、时钟跳变、进程假死属环境级，未自动化 |
| 安全：认证默认开启 | — | 不满足 | 未配置 token 时明确告警但不阻断（本机调试场景） |
| 安全：配置文件无明文口令 | 红线 | 不满足 | 见 FR-OPS-09 |
| 安全：未认证即可写点位 | 红线 | 满足（配置了 token 时） | 写端点走鉴权中间件；不配 token 时等同于开放 |
| 安全：诊断包含凭据 | 红线 | 满足 | `Redact` 把 `token`/`password`/`secret` 替换为 `***`，`contract/config_is_redacted` 验证 |
| 可观测：日志字段 | `ts/level/msg/device/...` | 已实现 | 环形缓冲与 JSON 行文件共用同一套字段：`ts/level/category/msg/device/exception`；`logs/json_line_fields_are_complete` |
| 兼容：配置向后兼容 ≥2 大版本 | 自动迁移 | 部分实现 | 迁移链已就位，但只有 v0→v1 两条真实规则 |
| 兼容：浏览器最近 2 个版本 | Chrome/Edge/Firefox | 已验证 Chrome | `scripts/wasm-smoke.sh` 用 Playwright Chromium；其余浏览器未跑 |
| 可维护：单文件 ≤600 行 | — | 已实现 | 新增端点都拆成 `RestApi.*.cs` partial |
| 可维护：依赖数量 ≤10 | 直接依赖 | 已实现 | 生产 4 个（日志抽象、串口、OPC UA Server/Configuration）+ ASP.NET Core 框架引用；测试 1 个 |
| 构建/测试时间 | ≤60 s / ≤120 s | 已实现 | 全量构建约 20 s，测试约 32 s |

## 9. 与 PRD 安全红线的差距

PRD `06-nfr.md` §4 列了五条红线，首版**有意**只满足其中三条：

| 红线 | 首版 | 怎么补 |
|---|---|---|
| 存在默认口令 | **没有账号体系，因此也没有默认口令**（形式上满足，实质上是把问题推给了「必须配 token」） | 引入账号库时同时引入首次改密流程 |
| 配置文件含明文口令 | **不满足**：`northbound.rest.token` 与 MQTT 口令是明文 | 配置字段标记为 `secret`，写入前用 AES-GCM 加密，密钥来自环境变量或系统密钥环 |
| 未认证即可写点位 | 配了 token 就满足；不配则公开（启动告警） | 把「无 token」从告警改成拒绝启动，或仅允许 `127.0.0.1` 绑定 |
| 依赖含 GPL/AGPL | 满足 | — |
| 诊断包含凭据 | 满足 | — |

这份差距是**首版范围的取舍**，不是实现疏漏：PRD §6.1 把账号、RBAC、TLS 都放在了「不做」里。
把这三条补上之后，FR-OPS-02/03/04/09/10 才能从「刻意没做」转为「已实现」。

## 10. 怎么用这份表

- 评审时按行看「状态」列，不要按「需求优先级」看：`M` 里也有刻意没做的（FR-OPS-02/09/10），`S`/`C` 里也有已实现的；
- 要复现某条证据：`dotnet run --project tests/Uic.Tests -- --filter <用例名>`；
- 要补某条：先看「补法」，再看 [implementation-notes.md](implementation-notes.md) 的扩展点章节；
- 新增 FR 或改变状态时，**同一提交里**更新这份表与 `testing.md` 的用例计数。
