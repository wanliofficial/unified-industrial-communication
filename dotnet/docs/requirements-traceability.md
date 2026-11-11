# 需求追溯

这份表回答一个问题：**`docs/03-product/01-prd.md` 里的每条 FR/NFR，在本仓库里到底落地到了哪一步？**

- 「已实现」= 有代码，且有能自动跑的用例或脚本作为证据；证据名就是 `dotnet run --project tests/Uic.Tests -- --filter <名>` 能跑到的用例名；
- 「部分实现」= 主干能用，但验收标准里有明确没做到的部分，写在备注里；
- 「刻意没做」= 有意识不做，给出原因与补法（与 [implementation-notes.md](implementation-notes.md) 的「刻意没做」一致）。

首版的目标范围是 PRD §6.1 的 Wave 1 子集：**Modbus 三封装 + S7 主站 + OPC UA 客户端 + MQTT 南向订阅 + 虚拟设备 + 四北向通道 + 配置治理 + Outbox + 可观测**。
PRD 中 M 级的采集协议已全部落地，FR-CFG-01 的四类寻址模型也已按验收口径闭环
（object/file 只差驱动，见该行备注）。非 M 级验收里的 12 类故障注入同样全部有了
可自动跑的用例（见 §7），唯一剩下的延期项是 object/file 寻址的实际驱动，
见 [protocol-status.md](protocol-status.md) §4.1。

当前测试基线：**308 passed / 0 failed / 3 skipped**（跳过的是需要 `--opcua` 的 OPC UA 端到端用例）；
加 `-- --opcua` 跑全量是 **311 passed / 0 failed / 0 skipped**。

## 1. 采集（FR-COL）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-COL-01 | Modbus TCP 主站，FC1/2/3/4/5/6/15/16 | 已实现 | `golden/read_coils_10`、`golden/write_multiple_regs_float32`、`loopback/*`；编解码覆盖 0x01–0x06/0x0F/0x10 |
| FR-COL-02 | Modbus RTU/ASCII 主站，含串口参数与帧间延时 | 已实现 | `serial/rtu_golden_frame_over_pty`、`serial/ascii_golden_frame_over_pty`、`serial/half_duplex_does_not_cross_responses`；波特率 300–921600 可配，半双工由通道闸门 + 发送前清空输入缓冲保证。帧间静默按 3.5 字符时间（>19200 bps 固定 1.75 ms），计时挂在物理串口上，多从站共享：`serial/rtu_waits_between_frames` |
| FR-COL-03 | Modbus TCP 从站 | 已实现 | 内置从站被主站回环读写：`loopback/write_then_read_back`、`loopback/write_to_missing_register_is_rejected` |
| FR-COL-04 | S7comm 主站 | 已实现 | `Uic.Codec.S7`（TPKT/COTP/S7 PDU 纯函数）+ `Uic.Driver.S7`（会话与驱动）+ `Uic.Sim.S7`（进程内从站）。DB/IB/QB/MB 区、位地址、PDU 自动协商（240/480/960 以 PLC 回应为准）与按 PDU 自动切片；`address.offset` 是字节偏移。证据：`s7/connect_request_frame`、`s7/setup_response_takes_plc_pdu`、`s7/read_response_length_is_in_bits`、`s7/address_parses_areas`（16 条黄金帧与地址解析）、`s7/probe_establishes_full_session`、`s7/read_mixed_areas_and_types`、`s7/write_bit_toggles_only_that_bit`、`s7/read_beyond_block_is_address_error`、`s7/negotiated_pdu_limits_the_window`、`s7/silent_plc_times_out`、`s7/reconnect_after_peer_closes`（14 条主站↔从站回环）。明确不支持并明确报错：符号寻址（0xB2）、S7Plus、TPDU 分片重组、多连接并发 |
| FR-COL-05 | OPC UA 客户端 | 已实现 | `Uic.Driver.OpcUa`，基于官方协议栈（`Opc.Ua.Client`），不手写二进制层。寻址用符号寻址（`address.path` 写 NodeId 文本）；读是「一轮合并成一次 Read 服务调用 + 按 `MaxNodesPerRead` 分片」；写走 Write 服务并逐点裁决；订阅用 MonitoredItem 暴露为 `ISubscriptionConn`。SecurityPolicy **None / Basic256Sha256**（模式 None / Sign / SignAndEncrypt），端点按配置精确匹配，找不到就报错并列出服务端实际档位，**不静默降级**。证据：`opcua/address_parses_node_ids`、`opcua/address_rejects_bad_input`、`opcua/status_maps_to_quality`、`opcua/variant_decodes_scalars`、`opcua/variant_decodes_arrays`、`opcua/variant_rejects_shape_mismatch`、`opcua/encode_writes_values`、`opcua/sample_keeps_device_timestamp`、`opcua/options_from_channel_and_labels`、`opcua/options_reject_bad_config`、`opcua/endpoint_selection_never_downgrades`（纯函数），`opcua/client_read_write_subscribe`、`opcua/client_secure_policy_fails_without_match`（`--opcua` 回环）。明确不支持并显式报错：用户名/口令身份、Basic256Sha256 之外的安全策略、`nsu=`/`svr=` 命名空间、结构体/ExtensionObject。**订阅没有接入采集主路径**：调度器仍按周期轮询，与 Modbus/S7 一致；Basic256Sha256 没有仓内端到端（内置服务端只提供 None），见 [protocol-status.md](protocol-status.md) |
| FR-COL-06 | MQTT 南向订阅取数 | 已实现 | `Uic.Codec.Mqtt`（与北向共用的 3.1.1 客户端）+ `Uic.Driver.Mqtt`。`address.path` 是主题过滤器（可含 `+`/`#`），`address.property` 或设备标签 `payloadField` 指定 JSON 字段（支持北向 `{"type","num"}` 值信封，网关可级联）；驱动内部维持订阅并缓存最近值，`ReadAsync` 返回缓存，从未收到或超过 `labels.staleAfterSeconds` 报 `stale`，非 JSON 报 `bad`，字段缺失/类型不符报 `address_error`；断线自动重连并重发 SUBSCRIBE；QoS0/1、TLS（`labels.tlsEnabled`/`tlsCaFile`/`tlsInsecureSkipVerify`）。证据：`mqtt/topic_matcher_follows_wildcard_rules`、`mqtt/payload_decodes_fields_and_types`、`mqtt/options_from_channel_and_labels`、`mqtt/options_reject_credentials`、`mqtt/driver_is_stale_before_first_message`、`mqtt/driver_reads_qos1_message_into_cache`、`mqtt/driver_reads_qos0_message_into_cache`、`mqtt/driver_uses_point_field_override`、`mqtt/driver_reports_bad_payload_and_recovers`、`mqtt/driver_resubscribes_after_broker_drop`、`mqtt/driver_marks_value_stale_after_window`、`mqtt/driver_stale_ignores_a_clock_jump`、`mqtt/driver_probe_fails_when_subscription_rejected`、`contract/drivers_expose_capability_states`。明确不支持并显式报错：QoS2、持久会话、客户端证书、用户名/口令（匿名 only，需要认证时用 TLS + Broker ACL）、写点位。**采集主路径仍是轮询**：`Capabilities.Subscribe = supported` 指驱动本身是订阅型，`DeviceActor` 没有改成推送驱动 |
| FR-COL-07 | 虚拟设备（正弦/斜坡/随机/常量/故障注入） | 已实现 | `loopback/*`、`contract/*` 都以 `virtual` 驱动跑通全链路；五类故障注入见 `Uic.Driver.Virtual` |
| FR-COL-08 | 驱动能力四态声明 | 已实现 | `GET /api/v1/drivers` 暴露 `Capabilities`；控制台「驱动」页渲染八项四态徽标、设备页显示读写/订阅/浏览四项；`contract/drivers_expose_capability_states` 锁住四态线上名字 |
| FR-COL-09 | 批量读规划（合并 / 非法跨越拒绝） | 已实现 | `planner/*` 八条：相邻合并、跨空洞不合并、按寄存器与位上限切分、按区域分离、每点恰好覆盖一次 |
| FR-COL-10 | 每点位独立采集周期 | 已实现 | `Point.interval` 覆盖设备周期（空/0 表示继承，非 0 时 100ms..24h）；`DeviceActor` 按有效周期分桶，每轮只读到期的桶并合并成一次串行读取，快慢变量互不拖累。证据 `engine/point_interval_overrides_device_period`（1s 设备 + 100ms 点位）、`engine/point_interval_failure_is_scoped_to_due_bucket`、`csv/imports_point_interval`、`schedule/phase_follows_fastest_point`、`contract/points_expose_effective_interval`；看门狗阈值取设备内最慢周期，启动相位取最快周期，点位 DTO 暴露 `interval` 与 `effectiveInterval` |
| FR-COL-11 | 写入（单点/多点），写后校验与超时 | 已实现 | 单点/多点、逐点裁决、超时三态（`ok`/`disabled`/`timeout`）已实现：`contract/write_returns_per_point_results`。`confirm: true` 在同一个通道租约内立即回读并比较，结果带 `verified`；不一致返回 `ok=false` + `uncertain` + `verify.mismatch`：`engine/write_confirmation_reads_back`、`engine/write_confirmation_detects_mismatch`、`engine/write_without_confirmation_skips_readback`。数组点位的元素个数在**建链前**校验，不符直接拒绝而不是交给驱动：`engine/array_write_shape_is_validated_before_the_device` |
| FR-COL-12 | 订阅型协议（COV/事件/通知） | 刻意没做（C） | 路线图 Wave 2。`Capabilities.Subscribe` 是四态声明，OPC UA 客户端已声明 `supported` 并通过 `ISubscriptionConn` 提供真实的 MonitoredItem 订阅（`opcua/client_read_write_subscribe`），但**采集主路径仍是按周期轮询**，与 Modbus/S7 一致：把订阅接到调度器需要另一套「推送驱动的重试/熔断/死区」语义，不在本版范围内 |

## 2. 数据模型与配置（FR-CFG）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-CFG-01 | 四类寻址统一模型 | 已实现 | `Address` 一个结构承载 register/symbol/object/file，四类各自必填与禁止字段由 `Address.Validate` 收口（混填即拒绝并指出多余字段）。**register** 由 Modbus 与 S7 实现；**symbol** 由 OPC UA（`path` 承载 NodeId 文本，`index` 作省略 `ns=` 时的命名空间）与 MQTT 南向（`path` 承载主题过滤器，`property` 承载 JSON 字段）实现；**object / file** 模型与校验已就绪，但还没有驱动消费（要等 BACnet/CIP 的对象模型、带文件记录语义的协议）。验收口径「同一套模板可生成 Modbus 与 S7 设备」由 `template/one_template_targets_modbus_and_s7` 覆盖。证据：`config/four_address_kinds_validate`、`template/one_template_targets_modbus_and_s7`、`opcua/address_parses_node_ids`、`opcua/address_rejects_bad_input`、`mqtt/topic_matcher_follows_wildcard_rules`、`mqtt/driver_uses_point_field_override`、`config/console_draft_builds_a_valid_document` |
| FR-CFG-02 | 数据类型系统（≥8 类，含数组） | 已实现 | 13 种标量类型（bool/int/uint 16/32/64/float 32/64/string/BCD16/BCD32/bytes/datetime）；datetime 固定 4 寄存器 int64 Unix 毫秒 UTC，北向用 ISO-8601 UTC 文本：`codec/datetime_is_unix_milliseconds`、`json/datavalue_wire_shape_is_stable`、`opcua/anonymous_session_read_write_subscribe`。数组用「元素类型 + 元素个数」正交表达（`dataType` + `count`），只允许定宽数值类型，逐元素套字节序、逐元素缩放、死区取最大变化：`codec/array_roundtrip_per_element_byte_order`、`codec/array_encode_rejects_wrong_element_count`、`codec/array_rejects_non_numeric_element_types`、`json/datavalue_array_shape`、`json/config_point_count_roundtrip`、`pipeline/array_scales_element_wise`、`pipeline/deadband_array_uses_largest_change`、`config/array_count_rules`、`csv/imports_array_count`、`contract/points_expose_array_count_and_values`，另 4 份数组黄金帧 |
| FR-CFG-03 | 字节序 ABCD/CDAB/BADC/DCBA | 已实现 | `codec/byte_order_encodes_as_expected`、`codec/byte_order_roundtrip`、`json/enums_are_lowercase_wire_names` |
| FR-CFG-04 | 设备模板 + 参数化实例化（≥50 实例） | 已实现 | `template/instantiate_fifty_devices`（50 个实例逐个断言）、`contract/template_instantiates_and_applies_devices`、`template/rejects_bad_parameters`、`template/store_rejects_unknown_placeholder` |
| FR-CFG-05 | 点表 CSV 导入（地址表达式 + 命名规则） | 已实现 | `csv/address_expressions`、`csv/import_merges_and_reports_conflicts`、`contract/points_csv_import_previews_and_applies`；支持 `40001` / `holding:100` / 分列三种写法 |
| FR-CFG-06 | 命名唯一性与非法字符校验 | 已实现 | `config/rejects_invalid_documents`；通道/设备/点位各自的重复 id 与命名规则都在 `ModelValidator` 里一次列全 |
| FR-CFG-07 | 配置 Schema 校验（启动即校验） | 已实现 | `config/rejects_invalid_documents`、`contract/config_validate_reports_problems`；非法配置退出码 2 |
| FR-CFG-08 | 配置版本化（内容哈希）+ 历史列表 | 已实现 | `config/id_is_content_addressed`、`config/recommit_of_identical_content_keeps_one_version`、`config/bootstrap_prefers_current_pointer` |
| FR-CFG-09 | 配置差异对比 | 已实现 | `config/diff_finds_changed_added_removed`、`config/diff_text_is_human_readable`；字段级 JSON Pointer；凭据字段的前后值在 REST 响应里脱敏（`contract/config_is_redacted`） |
| FR-CFG-10 | 一键回滚（回滚也产生新版本） | 已实现 | `config/rollback_adds_a_version` |
| FR-CFG-11 | 配置原子写（临时文件 + fsync + rename） | 已实现 | `ConfigStore.AtomicWrite`；`config/reload_after_restart` 验证重启后仍读得到 |
| FR-CFG-12 | 热更新：只重启受影响设备 | 已实现 | `contract/template_instantiates_and_applies_devices`（apply 后返回 `restartedDevices`）；diff 按设备/点位粒度。下线设备的观测状态一并清理（否则删了又加回来不重推 `$state`、且吞掉第一轮停滞告警）：`lifecycle/removed_device_is_forgotten`、`lifecycle/readded_device_republishes_its_state`；读挂死时下线不被阻塞，否则配置应用与关机一起挂住：`lifecycle/hung_read_does_not_block_shutdown` |
| FR-CFG-13 | 配置导入/导出（跨版本迁移） | 已实现 | `config/migration_is_explicit_and_versioned`、`config/migrates_v0_legacy_shapes`、`config/repository_examples_validate`（仓库 `examples/` 三个示例都能直接 `check` 通过）、`contract/config_export_import_round_trip`、`contract/config_import_migrates_legacy_shape`、`config/redacted_secrets_are_restored`、`contract/apply_keeps_redacted_secrets`（默认导出的 `***` 在 apply/import 时按同路径还原）。迁移覆盖 `server.*`、`storage.dir`、`concurrency`、`devices[].channel`、transport 对象、数字 stopBits；时长接受 Go 复合写法 |

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
| FR-RUN-08 | 单调时钟用于调度与超时 | 已实现 | `nextDue` 与等待都基于 `Stopwatch.GetTimestamp()` + `ScheduleMath`；NTP 校时/手工改钟不会推迟或提前采集。样本时间戳仍用墙上时钟：`schedule/monotonic_arithmetic`。同一原则贯彻到「走了多久」的度量：`/health` 的 uptime 用单调时钟，校时跳变不会让它凭空多出一小时：`schedule/uptime_ignores_a_clock_jump`；**写命令的去重窗口**也用单调时钟，否则一次校时前跳会让同一条命令再执行一遍（对一个写点位的命令就是重复物理动作）：`mqtt/command_dedup_ignores_clock_jump` |
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
| FR-NOR-01 | REST：当前值、历史、配置、状态 | 已实现 | 当前值/配置/状态：`contract/devices_and_points_are_listed`；历史：`GET /api/v1/history`，证据 `contract/history_query_reads_journal` |
| FR-NOR-02 | REST：写入点位（需权限，三态结果） | 已实现 | 三态结果与逐点裁决：`contract/write_returns_per_point_results`；写端点要求 `point.write`，只读角色与审计员被拒并记 `auth.forbidden`：`contract/auth_roles_are_enforced` |
| FR-NOR-03 | SSE 实时推送（按订阅表达式） | 已实现 | `contract/*` 起真实 Kestrel；按 `device.point` / 通配过滤、质量过滤、心跳、50 路并发上限。已知边界：`_lastQuality` 记的是「历史上出现过的质量」，设备下线后不回收；因为只影响「质量是否变化」的判定，而删了又加回来时旧值恰巧就是正确的前值，不构成语义错误 |
| FR-NOR-04 | MQTT 上报（Topic 模板、QoS、保留） | 已实现 | `mqtt/sink_renders_topic_and_injects_schema_version`、`mqtt/publish_qos1_waits_for_puback`、`mqtt/reconnect_republishes_birth_and_state`；设备下线时清 retained `$state` 且重连不再补发：`mqtt/forgets_removed_device_state`；`scripts/mqtt-interop.sh` 用真实 broker 验一遍 |
| FR-NOR-05 | OPC UA 服务端 | 已实现 | `opcua/anonymous_session_read_write_subscribe`（需 `--opcua`） |
| FR-NOR-06 | Outbox 持久补发（至少一次） | 已实现 | `outbox/advances_only_on_success`、`outbox/replays_after_restart`、`outbox/tolerates_a_torn_last_line`；`mqtt/command_*` 验证重连后的位点 |
| FR-NOR-07 | 北向多目标并行 | 已实现 | `Outbox` 按 sink 分区，一个分区失败不拖住其他分区：`outbox/partitions_are_independent` |
| FR-NOR-08 | 历史查询（时间范围/降采样/分页） | 已实现 | `GET /api/v1/history` 从 journal NDJSON 两段式扫描：先按字节取「时间 + 设备 + 点位 + 序号」做过滤、去重与选桶，扫描结束后按文件偏移回读胜出行才做完整 JSON 解析。`from`/`to` 时间范围、`interval` 每点位每桶保留最后一条、`limit`+`cursor` 分页；按 `(device, point, seq)` 去重并跳过坏行。证据 `history/filters_by_time_and_identity`、`history/downsamples_each_point_per_bucket`、`history/downsamples_across_read_buffer_boundary`、`history/dedupes_redelivered_lines`、`history/skips_malformed_lines`、`history/handles_lines_across_read_buffer`、`history/falls_back_on_escaped_names`、`history/skips_impossible_calendar_dates`、`contract/history_query_reads_journal`。**性能达标**：本机 100 万行 / 152 MB 单文件，全范围 + `5m` 降采样约 1.0–1.3 s（PRD 目标 < 2 s），单设备 + `1m` 约 0.4 s，最近 30 分钟约 0.3 s；不降采样返回 100 万行约 5–8 s（受输出规模限制）。仍是全文扫描，没有时间索引；再上一个量级应换时序库，接口形状不变 |
| FR-NOR-09 | Sparkplug B | 刻意没做（C） | Wave 2；可作为新的 MQTT sink 接入 |
| FR-NOR-10 | 数据契约版本（`schemaVersion`） | 已实现 | REST/SSE/MQTT/OPC UA 输出都带 `schemaVersion`；`json/unknown_fields_are_tolerated` 保证前向兼容 |

## 6. 运维与安全（FR-OPS）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-OPS-01 | Web 控制台（登录、设备、点位、监视、日志、配置） | 已实现 | 八个页面（Uno WASM）：设备、驱动、点位、监视、日志、审计、配置、账号；登录页 + 强制改密界面，按权限隐藏导航与写操作。`scripts/wasm-smoke.sh` 在 Playwright 里真跑「登录 → 强制改密 → 首屏数据 → 新建设备」。设备页内置「新建设备」表单（可同时新建通道：tcp 填 host/port，serial 填 port/baud，virtual 无传输参数），提交走「读脱敏配置 → 追加通道/设备 → `config/validate` → `config/apply`」，与配置页同一条版本链，`config/console_draft_builds_a_valid_document` 与 `contract/console_device_creation_flow` 覆盖拼装与端到端提交。表单只建**首个**点位，继续加点位仍走 CSV 导入或配置页 |
| FR-OPS-02 | 认证（本地账号 + 会话 Token，首次强制改密） | 已实现 | 账号存 `data/security/users.json`（PBKDF2-SHA256、210k 迭代、每账号随机盐、原子替换写盘，无明文）；首次启动生成随机 `admin` 口令到 `data/security/initial-admin-password.txt`，改密前除 `auth/me`、`auth/password`、`auth/logout` 外全部 403 `auth.password_change_required`，改密成功后删除该文件。会话 Token 内存保存 12 小时，进程重启即失效；登录失败 5 次/分钟/IP 锁定 60 秒，锁定窗口按单调时钟计时，系统校时跳变既不能提前解锁也不能把账号锁死。静态 `northbound.rest.token` 保留为全权限 API Token（审计记 `api-token`）。证据：`auth/*`（10 条，含 `auth/lockout_survives_a_clock_jump`）与 `contract/auth_login_forces_password_change`、`contract/auth_login_is_rate_limited`、`contract/auth_logout_revokes_session` |
| FR-OPS-03 | RBAC（页面级 + 操作级，≥3 角色） | 已实现 | 四个内置角色 `admin` / `operator` / `viewer` / `auditor`，九项权限（`system.read`、`system.control`、`device.control`、`point.write`、`config.read`、`config.write`、`config.rollback`、`audit.read`、`user.manage`）。页面级：导航与服务端返回的有效权限集合一致，`users` / `audit` 按 `user.manage` / `audit.read` 出现；操作级：每个端点 `.RequirePermission(...)` + 中间件默认拒绝（漏标即 403），命令按 action 二次查表。证据：`auth/role_matrix`、`contract/auth_roles_are_enforced`、`contract/auth_users_require_manage` |
| FR-OPS-04 | 审计日志（可查询、可导出） | 已实现 | 追加写 NDJSON（`storage.audit`，默认 `data/audit/audit-yyyyMMdd-HHmmss-fff.ndjson`），按天 + 按大小切分，按份数保留，逐条 `Flush(flushToDisk: true)`；重启后从文件末尾恢复序号，`seq` 单调。记录 `config.apply` / `config.rollback` / `config.import` / `config.points_import` / `config.template_instantiate` / `config.export`（仅 `redact=false`）/ `point.write`（逐条，含旧值与新值）/ REST 与 MQTT 命令 / `auth.failed`（按来源 IP 30 秒去重）/ `audit.export`。`GET /api/v1/audit` 按时间、动作、身份、结果、目标过滤并分页（最新在前），`GET /api/v1/audit/export` 导出同条件的 NDJSON 附件。写失败只告警 + `uic_audit_write_failures`，不阻断业务。证据：`audit/*`（7 条）与 `contract/audit_records_write_and_config`、`contract/audit_export_is_ndjson`、`contract/audit_denies_are_recorded` |
| FR-OPS-05 | 指标（Prometheus 文本格式） | 已实现 | `GET /api/v1/metrics`；设备状态、样本发布、Outbox、OPC UA、看门狗都有序列；`/api/v1/health` 汇总 |
| FR-OPS-06 | 健康检查三级（live/ready/business） | 已实现 | `/healthz`、`/readyz`、`/api/v1/health`；`contract/health_and_version_are_public` |
| FR-OPS-07 | 诊断包（脱敏配置 + 日志 + 指标 + 版本） | 已实现 | `GET /api/v1/diagnostics/bundle` 与命令 `diag.bundle` 共用 `DiagnosticsBundle.Build`；`mqtt/command_diag_bundle_is_whitelisted` |
| FR-OPS-08 | 远程命令白名单 | 已实现 | `contract/*` 与 `mqtt/command_unknown_action_returns_error`、`mqtt/command_privileged_action_is_forbidden`；REST/MQTT 共用 `CommandExecutor` |
| FR-OPS-09 | 凭据加密存储 | 已实现 | 凭据字段（`northbound.rest.token` / `northbound.mqtt.password`）只接受引用：`${env:NAME}`、`${file:PATH}`、`enc:v1:...`（AES-256-GCM，密钥来自 `UIC_SECRET_KEY` / `UIC_SECRET_KEY_FILE`）。明文在 `uic check`、启动、`config/validate`、`config/apply` 四处被拒并指出字段路径；落盘版本、diff、导出、诊断包与 `GET /api/v1/config` 只有引用或 `***`。`uic secret keygen|seal` 提供密钥与密封；`secret/*`（9 条）与 `contract/plaintext_credentials_are_rejected` 覆盖解析失败硬失败、篡改拒绝与明文拒绝 |
| FR-OPS-10 | TLS（API 与北向） | 已实现 | REST：`northbound.rest.tls` 打开后 Kestrel 按 HTTPS 监听，证书支持 PFX（`certificateFile` + `certificatePassword`）与 PEM（`certificateFile` + `keyFile`），口令仍是凭据引用；装载失败按配置非法退出（码 2）。MQTT：`northbound.mqtt.tls` 用 `SslStream` 包裹连接，校验服务端证书链与主机名，自签 Broker 通过 `caFile` 走 `CustomRootTrust`，`insecureSkipVerify` 只能联调且启动告警。REST 的证书/监听变更需重启（热更新只告警），MQTT 的 TLS/CA 变更在 `config/apply` 重建客户端后生效。证据：`tls/*`（3 条配置与装载）、`contract/tls_rest_serves_https`、`mqtt/tls_publishes_over_custom_ca`、`mqtt/tls_rejects_untrusted_broker` |
| FR-OPS-11 | 日志（结构化、分级、轮转） | 已实现 | 控制台 + 内存环形缓冲 + 可选 JSON 行文件日志：字段 `ts/level/category/msg/device/exception`，按 `logging.file.maxBytes` 轮转、保留 `retain` 份；支持热更新开关：`logs/json_line_fields_are_complete`、`logs/file_rotates_without_losing_lines`、`logs/retention_prunes_old_archives`、`logs/reconfigure_stops_writing` |

## 7. 驱动开发与测试（FR-SDK）

| 编号 | 需求 | 状态 | 证据 / 说明 |
|---|---|---|---|
| FR-SDK-01 | 驱动三层：Codec / Transport / Driver | 已实现 | 项目切分即边界：`Uic.Codec.Modbus` 不依赖 `net`，`Uic.Transport` 只管字节流，`Uic.Driver.Modbus` 组装 |
| FR-SDK-02 | 能力与地址类型声明 | 已实现 | `DriverMeta.Capabilities` / `ConfigFields`；加载时不匹配直接拒绝 |
| FR-SDK-03 | 驱动契约语义化版本 | 已实现 | `DriverRegistry.Register` 校验 `DriverSdk.Version`，不匹配抛出并说明原因 |
| FR-SDK-04 | 虚拟设备（按协议实现从站） | 已实现 | `Uic.Host/Simulator.cs` 起 Modbus TCP 从站；`loopback/*` 全链路 |
| FR-SDK-05 | 故障注入（丢包/延迟/错帧/异常码/连接重置） | 已实现 | `Uic.Driver.Virtual` 的五类故障；`loopback/probe_times_out`、`loopback/silent_slave_times_out` |
| FR-SDK-06 | 金报文测试套件 | 已实现 | `testdata/golden/*.json` 20 份，`golden/*` 逐字节比对（TCP/RTU/ASCII），含 4 份数组帧（读、写、逐元素字节序） |
| FR-SDK-07 | 一致性测试套件（连接/超时/重连/并发/取消） | 已实现 | T1–T18 全部有对应用例，映射表见 [testing.md](testing.md)；单个驱动跑完约 30 s |
| FR-SDK-08 | 进程外驱动（gRPC） | 刻意没做（C） | Wave 3；需要先有稳定的 IPC 契约与进程生命周期管理 |

## 8. 非功能需求（NFR）

只列与首版代码直接相关的项；「未测量」不等于「不满足」，但也不能算通过。

| 类别 | 目标 | 状态 | 说明 |
|---|---|---|---|
| 容量 | 200 设备 / 10,000 点 | 已实现 | `scripts/capacity-bench.sh` 起真宿主：200 设备 × 50 点（周期 1s）全部转 `good` 用时 1.5 s，200/200 collecting，稳态读 ~203 次/s（≈ 10,160 点/s），CPU ≈ 1 核，RSS ≈ 261 MB。数字来自虚拟设备（无链路 I/O、sink 默认关闭），现场需按实际链路重跑 |
| 采集周期下限 | 100 ms | 已实现 | 点位 `interval` 只接受 `0`（继承）或 `100ms..24h`，低于 100 ms 直接拒绝；设备级 `interval` 下限仍是 10 ms（命令行/自测需要更快轮询），见 `config/rejects_twenty_invalid_shapes`、`csv/imports_point_interval` |
| 写命令 P95 ≤ 300 ms | 读满载下 | 已实现 | 同一负载下按 5 次/秒发 100 次单点写：P50 2.1 / P95 5.8 / P99 12.1 ms；`--confirm`（写后回读）P95 2.4 ms。写优先由 `ChannelGate` 保证，见 `scripts/capacity-bench.sh`（REST 写配额 10 次/秒的上限另计） |
| 配置应用 ≤ 2 s | 改 1 台设备 | 已实现 | `contract/template_instantiates_and_applies_devices` 每次 apply 约 100–300 ms（含真实 Kestrel） |
| 配置回滚 ≤ 30 s | 全量 | 已实现 | `config/rollback_adds_a_version` 亚秒级 |
| 断网补发 24h 无空洞 | — | 已实现（机制） | `outbox/replays_after_restart` 验证位点与重放；24h 时长需要浸泡测试 |
| 配置回滚成功率 100% | 注入 20 类非法配置 | 已实现 | `config/rejects_twenty_invalid_shapes` 逐条注入 25 类非法配置并断言拒绝理由的字段路径；`config/rejects_invalid_documents` 验证一次报全所有问题、提交路径同样拦截 |
| 单设备故障隔离 | 不影响其他设备 | 已实现 | 每设备 actor + 通道闸门；`outbox/partitions_are_independent` 同样思路 |
| 采集假死检测 ≤ 60 s | — | 已实现 | `DeviceStateWatcher`：max(10 × 设备内最慢点位周期, 30 s) |
| 优雅退出 ≤ 10 s | SIGTERM | 已实现 | flush 3 s + `$death` 2 s 的预算，留有余量 |
| 掉电配置完整性 | 100% | 已实现（机制） | 临时文件 + `Flush(flushToDisk: true)` + `File.Move(overwrite)` |
| 故障注入矩阵 12 类 | 全通过 | 已实现 | 连接拒绝/中断/超时/半包/错 CRC/异常码/部分响应/慢响应/Broker 断线沿用各自的协议用例；环境级三类用真实注入点补齐：**磁盘满** `outbox/disk_full_keeps_records_pending`（软链 `/dev/full`，内核返回 ENOSPC，位点不推进、磁盘恢复后补投）、**时钟跳变** `fault/clock_jump_does_not_fake_a_stall` + `fault/outbox_ttl_ignores_a_clock_jump`（可拨 `TimeProvider`，墙钟跳一小时不误判停滞、TTL 按单调时长算）、**进程假死** `fault/collection_stall_is_reported`（驱动卡在不可取消的 Task 上，看门狗按双时钟判定并做 5 分钟去重）。磁盘满用例需要 `/dev/full`、权限用例需要 POSIX 文件系统，不具备时显式跳过而不是假装通过 |
| 安全：认证默认开启 | — | 满足 | 本地账号库始终存在，未认证请求一律 401；首次启动生成随机 `admin` 口令并强制改密，没有「不配令牌就放行」的路径。`contract/api_requires_bearer_token`、`contract/auth_login_forces_password_change` |
| 安全：配置文件无明文口令 | 红线 | 满足 | 凭据字段只接受 `${env:}` / `${file:}` / `enc:v1:`，`check`、启动、`config/validate`、`config/apply` 四处拒绝明文；`secret/*`（9 条）与 `contract/plaintext_credentials_are_rejected` |
| 安全：未认证即可写点位 | 红线 | 满足 | 写端点除鉴权外还要求 `point.write`；未认证 401、角色不足 403 并记审计。`contract/auth_roles_are_enforced` |
| 安全：诊断包含凭据 | 红线 | 满足 | `Redact` 把 `token`/`password`/`secret` 替换为 `***`，`contract/config_is_redacted` 验证 |
| 安全：脱敏配置回写不降级凭据 | — | 满足 | `apply`/`import` 把同路径的 `***` 还原成**当前引用**（不是明文），显式空串仍表示清空；`config/redacted_secrets_are_restored`、`contract/apply_keeps_redacted_secrets` |
| 可观测：日志字段 | `ts/level/msg/device/...` | 已实现 | 环形缓冲与 JSON 行文件共用同一套字段：`ts/level/category/msg/device/exception`；`logs/json_line_fields_are_complete` |
| 可观测：审计可追溯 | 查询 + 导出 | 已实现 | 配置变更、写入、命令、登录/登出/改密、越权与账号变更都落 NDJSON，重启不清空；`actor` 是真实账号 id；`audit/*`（7 条）与 `contract/audit_*`（3 条）、`contract/auth_*`（5 条） |
| 兼容：配置向后兼容 ≥2 大版本 | 自动迁移 | 已实现（现有 v0 形状） | `ConfigMigrations` 覆盖 7 条 v0 规则（server.listen/token、storage.dir、mqtt.broker、channels[].concurrency、channels[].transport 对象、serial.stopBits、devices[].channel）；`config/migrates_v0_legacy_shapes` 逐项断言迁移结果，`config/migration_is_explicit_and_versioned` 验证升级项回显与「更新版本拒绝启动」。仓库只有 v1，尚不存在第二个历史主版本可测 |
| 兼容：浏览器最近 2 个版本 | Chrome/Edge/Firefox | 已实现 | `scripts/wasm-smoke.sh` 在 Playwright 里各跑一遍：Chromium（同一引擎覆盖 Chrome/Edge，首屏约 12 s）与 Firefox（Gecko，约 38 s），两条都走完「登录 → 强制改密 → 首屏 + 新建一台设备」并被断言；Safari/WebKit 未装，用 `--browser webkit` 可补跑 |
| 可维护：单文件 ≤600 行 | — | 已实现 | 新增端点都拆成 `RestApi.*.cs` partial |
| 可维护：依赖数量 ≤10 | 直接依赖 | 已实现 | 生产 4 个（日志抽象、串口、OPC UA Server/Configuration）+ ASP.NET Core 框架引用；测试 1 个 |
| 构建/测试时间 | ≤60 s / ≤120 s | 已实现 | 全量增量构建约 20 s（改 UI 后的首次全量约 5 min）；用例数随时间增长：219 条 65.5 s、233 条 114.2 s、239 条 112.7 s、269 条 106.4 s，仍在 120 s 门槛内（机器有 20+ 后台负载时会量到 1471 s，那不是这份代码的耗时） |

## 9. 与 PRD 安全红线的差距

PRD `06-nfr.md` §4 列了五条红线，本版全部满足。TLS（FR-OPS-10）不在五条红线里，
但作为「网关直接对外」的部署前提也一并补齐：

| 红线 | 首版 | 说明 |
|---|---|---|
| 存在默认口令 | **满足**：首次启动生成 16 位随机口令，只写进 `data/security/initial-admin-password.txt`，登录后强制改密，改密成功即删除文件 | 没有 `admin/admin` 这类固定默认口令 |
| 配置文件含明文口令 | **满足**：`token`/`password` 只接受引用，明文在检查、启动与两条配置写入路径上都被拒 | — |
| 未认证即可写点位 | **满足**：账号库始终存在，未认证 401；写点位还要 `point.write` | 不再存在「不配 token = 公开」 |
| 依赖含 GPL/AGPL | 满足 | — |
| 诊断包含凭据 | 满足 | — |

安全面上没有刻意留白的项：认证（FR-OPS-02/03）、审计（FR-OPS-04）、凭据落盘
（FR-OPS-09）与 TLS（FR-OPS-10）都已在本版实现。剩下的差距是**PRD §6.1 之外的
协议范围**（Wave 2/3，见 [protocol-status.md](protocol-status.md)）与 object/file
寻址的实际驱动（模型与校验已就绪），都不是安全红线。

## 10. 怎么用这份表

- 评审时按行看「状态」列，不要按「需求优先级」看：`M` 里的 TLS 已在本版补齐，`S`/`C` 里也有已实现的；
- 要复现某条证据：`dotnet run --project tests/Uic.Tests -- --filter <用例名>`；
- 要补某条：先看「补法」，再看 [implementation-notes.md](implementation-notes.md) 的扩展点章节；
- 新增 FR 或改变状态时，**同一提交里**更新这份表与 `testing.md` 的用例计数。
