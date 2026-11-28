# 08 · 网络安全等级保护 2.0 对照表

> 本文件把本仓已实现的安全能力逐条映射到**GB/T 22239-2019《网络安全等级保护
> 基本要求》**（等保 2.0）第三级的安全通用要求**安全计算环境**控制点，作为政企、
> 关基、水务、楼宇等采购场景的合规证据。
>
> **诚实声明**：本表只登记**代码里真实存在、可复核**的能力。每一条「已落地」都
> 标注了实现出处（包 / 文件 / 关键函数或测试）；每一条「部分落地 / 未落地」都如实
> 写出缺口，绝不虚标。等保 2.0 是**测评结论**，本文件是**能力对照证据**——最终等级
> 由具备资质的测评机构结合物理环境、管理制度、运维流程综合判定，本表不替代测评。
>
> 适用边界：本仓是**边缘侧协议网关软件**，不含物理安全、区域边界设备、安全管理中心
> 平台。因此本表只覆盖「安全计算环境」（主机/应用层可落地的技术控制点），物理安全、
> 安全通信网络（网络层）、安全区域边界（边界设备）、安全管理中心（SOC）、以及管理
> 类要求（制度/机构/人员/建设/运维）需由部署方在其整体架构内另行满足，本表在末尾
> 「不在软件范围内的控制点」一节列出，避免采购方误以为软件已覆盖。

---

## 1. 对照表

状态图例：**✓ 已落地**（代码可复核）、**◐ 部分落地**（核心有、缺子项）、**✗ 未落地**。

### 1.1 身份鉴别（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 对登录用户进行身份标识和鉴别，标识唯一，鉴别有难度 | ✓ | `internal/auth/auth.go` `Store.Authenticate`；`internal/auth/token.go` | Token 明文不落盘：配置可只存 `tokenSha256`（不可逆），比对时先对入参做 SHA-256 再比（`auth.go:225`）。多用户各自唯一标识（用户名进 `Principal`，用于审计）。「有难度」靠 Token 自身熵（≥16 字符、禁弱口令表，见下）。 |
| b) 登录失败处理：失败次数限制、结束会话、超时自动退出 | ✓ | `internal/auth/policy.go` `Policy`；`internal/northbound/httpapi/httpapi.go` `guard`/`authenticate`/`authorized`；`internal/config` `AuthPolicy` | 三条等保子项全部落地（已修复 R-3）：**失败次数限制 + 锁定**——`Policy.RecordFailure` 维护 per-IP 失败计数，达 `maxAuthFailures`（默认 5）即锁定 `lockoutDuration`（默认 15 分钟），锁定期间 `Policy.Allow` 返回 429；**防分布式绕过**——另有全局熔断 `globalMaxFailures`（默认 20）/`globalFailureWindow`（默认 5 分钟），窗口内累计失败触发全站拒绝 `globalLockoutDuration`；**超时自动退出**——`Policy.SessionAlive` 按 Token 的 sha256 记录最后成功时间，闲置超 `idleTimeout`（默认 30 分钟）即拒绝并要求轮换。**可信代理白名单**：`trustedProxies` 控制 XFF 提取真实 IP（反代部署必配，否则按反代 IP 计数致全体被锁）。全部阈值/时长经 `server.auth` 配置，零值回退等保基线默认。 |
| c) 远程管理防鉴别信息被窃听 | ✓ | `cmd/uic/main.go:393` `ListenAndServeTLS`；`internal/model` `Server.TLS`；`internal/tlscert` | 配齐 `server.tls`（enabled + certFile + keyFile）即强制 TLS，传输加密防窃听；或 `auto:true` 启动时生成自签证书（免证书分发、开箱即用，不提供来源可信）。**fail-closed**（已修复 R-1）：`enabled` 却既无证书文件也无 `auto` 时，配置期（`config.validateServerTLS`）直接拒绝，**不再静默回退明文 HTTP**。 |
| d) 口令复杂度与定期更换 | ◐ | `internal/auth/auth.go` `ValidateToken` | 已有：≥16 字符、≤512、禁空白、禁已知弱口令占位符（`weakTokens`）。**缺**：「定期更换」无强制机制（配置项可手工换，无到期强制）。 |

### 1.2 访问控制（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 对主体、客体分配标识，设置访问对象粒度 | ✓ | `internal/rbac/rbac.go` `Role`/`Permission`；`internal/model` 设备/点位 | 主体=已认证 `Principal`（用户 + 角色）；客体=设备/点位/配置/审计/命令。访问对象粒度到「设备 + 点位」级（`point.write` 单点与批量）。 |
| b) 按主体角色授予最小权限，权限分离 | ✓ | `internal/rbac/rbac.go` `Grants`/`CommandPermission`；`05-northbound-api-spec.md` §2.2 RBAC 矩阵 | 三角色 viewer/operator/admin，权限按角色最小集合授予（`rbac.go:permissions`）。「权限分离」体现在：配置与审计属 admin 独占，operator 可写点位/确认报警/导诊断但不可改结构。 |
| c) 权限变更及时生效 | ✓ | `internal/auth/auth.go` `Reloadable.Set`（原子替换凭据表） | `server.users` 变更 apply 后下一条请求即按新表判定，删除的用户立刻 401（`auth.go:78` 注释；NFR §4 凭据热更新）。 |
| d) 访问控制粒度：主体到用户级、客体到报表/文档级 | ◐ | `internal/rbac/rbac.go`；`internal/audit` | 主体已到用户级。客体粒度到 API 资源（点位/配置/审计/命令/诊断）；「报表/文档级」对应诊断包导出（脱敏后），粒度到「整包」而非单文件——边缘场景够用，非严格到文档级。 |
| — 最小权限原则 / 未知角色拒绝 | ✓ | `internal/rbac/rbac.go`（未知角色默认拒绝） | 未知角色一律拒绝而非默认放行；`role` 缺省按 admin 仅为兼容历史配置，新配置必须显式写角色（见 API spec §2.2）。 |
| — 高危命令越权防护 | ✓ | `internal/rbac/rbac.go` `CommandPermission` | `outbox.drop`/`config.rollback`/`log.level` 归 `command.danger`（仅 admin）；命令白名单的「需何权」单点定义在权限包，越权评审只看一处（`rbac.go:130` 注释）。 |

### 1.3 安全审计（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 启用安全审计，覆盖到每个用户，记录重要用户行为 | ✓ | `internal/audit/audit.go` `Store.Record`；httpapi 各写端点 | 审计覆盖每个用户（`Principal` 进记录），记录认证/授权失败/配置变更/点位写入/命令/确认等关键行为。 |
| b) 审计记录保护：保护未受未授权删除/修改/覆盖 | ✓ | `internal/audit/audit.go` 哈希链（`Record.Prev`/`Record.Hash`、`chainHash`、`Verify`）；`audit.go:426` 文件 0o600 | **哈希链防篡改**：每条记录带序号 + 前一条哈希 + 自身规范化哈希；事后改一条会被 `Verify` 抓住（`TestVerifyDetectsTampering`），删一条会被序号跳空抓住（`TestVerifyDetectsDeletedRecord`）。重启后链接上（`resumeChain`、`TestChainResumesAcrossRestart`）。文件 0o600 权限（`TestDirAndFileMode`）。 |
| c) 审计进程保护，防未授权中断 | ◐ | `internal/audit/audit.go`；`internal/watchdog` | 审计写失败**不回滚业务**但计入 `uic_audit_write_errors_total`（可见）；有界保留防写满拖垮进程。**但**审计是库而非独立受保护进程——「进程级保护」依赖 OS/部署（非软件可保证）。 |
| d) 保证日志记录时间准确性 | ✓ | `internal/clockmon`；`/api/v1/health` `clock` 块 | 系统时钟跳变检测（向前/向后）落在 health，并有 3 个指标；审计时间戳用 UTC（`time.Now().UTC()`）。 |
| e) 对审计进程保护，防止未授权中断；集中审计 | ◐ | `internal/audit` 有界保留 + `/api/v1/audit/verify` | 已落地审计记录保护与防篡改；**集中审计**（日志外发到 SIEM 等）属部署方运维域，软件提供导出端点但不主动外发。 |
| 审计记录查阅与保护 | ✓ | `internal/rbac` `PermAuditRead`；`GET /api/v1/audit`/`verify`；`internal/audit` 导出 | 审计查询/校验/统计属 `audit.read`（admin 独占）；导出脱敏。 |

### 1.4 数据完整性（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 重要业务数据完整性鉴别（传输/存储） | ✓ | `internal/audit/audit.go` `chainHash`（SHA-256）；`internal/northbound` 各写端点参数校验 | 审计记录用 SHA-256 哈希链做完整性保护（见 1.3 b）。配置/指令等写入在配置期与写前做结构化校验与边界检查（如点位越界/溢出在写报文前拒绝）。 |
| b) 重要数据存储完整性 | ✓ | `internal/config/config.go` `VersionOf`（SHA-256）；`internal/upgrade/upgrade.go` 逐文件 SHA-256 | 配置落盘前算 `VersionOf`（canonical SHA-256，排除 cluster），版本快照带内容哈希（`config.go` 快照 hash 比对）；离线升级包逐文件 SHA-256 重算比对（`upgrade.go:317`，`cmd/uic/pkg_test.go` `TestUpgradeTamperedRejected`）。 |

### 1.5 数据保密性（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 重要业务数据传输保密性 | ◐ | `cmd/uic/main.go:393` TLS；`internal/outbox`/`internal/northbound` 出站 | 北向 API 支持 TLS（见 1.1 c）。**南向/北向协议传输**（MQTT/OPC UA 等）的保密性由各协议自身承载：MQTT 支持 TLS（凭据在 CONNECT，见 `model.go:889` 注释）；OPC UA 本驱动走 None/None（无安全策略，见驱动 Meta `Name`）。配置文件里的凭据已加密存储（见 1.5 b），即使配置文件泄露凭据也不直接可读。 |
| b) 重要数据存储保密性 | ✓ | `internal/config/envelope`（AES-256-GCM 信封加密）；`internal/config` `encryptChannelSecrets`/`decryptChannelSecrets` | **已修复 R-2**：配置内链路凭据（MQTT/OPC UA 用户名口令）写盘前用信封加密（`enc:v1:` 前缀密文），读盘后解密还原明文给驱动。KEK 来源：环境变量 `UIC_CONFIG_KEK`（推荐，专用密钥）或派生自 `server.token`（回退）。未注入 KEK 时明文落盘（向后兼容存量配置）；KEK 不匹配/密文被篡改时 fail-closed（解密失败拒绝加载）。存储格式 `enc:v1:base64(nonce‖ciphertext+tag)`，纯标准库 `crypto/aes`+`crypto/cipher`，符合 ADR-0001 零第三方依赖。 |

### 1.6 数据备份恢复（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 提供本地数据备份与恢复 | ✓ | `internal/config` 版本快照（`VersionConfig`/`Rollback`）；`internal/history` NDJSON 分段 | 配置有版本快照 + 一键回滚（`Rollback`，每次回滚产生新版本）；采集历史以 NDJSON 分段落盘 + 保留策略。 |
| b) 提供异地实时备份（关键数据） | ◐ | `internal/outbox`；`internal/northbound` MQTT/Sparkplug | 关键数据经 MQTT（QoS1，retained 状态）/ Sparkplug B 实时外发到 Broker，可异地消费——「异地」由 Broker 部署位置决定，软件提供实时外发通道。**但**这是「实时外发」而非「定时备份到异地容灾系统」，后者属运维域。 |
| c) 剩余信息保护：释放前清除敏感信息 | ◐ | `internal/runtime/backends.go` `redactedValue`（`***`）；`internal/upgrade` 密钥不落盘；`internal/config/envelope` 凭据加密 | `GET /config` 返回脱敏（凭据替换为 `***`，`backends.go:134`）；枚举用户绝不回显 Token/哈希（`TestUsersNeverExposeTokens`）；升级私钥经 `-privkey` 传入不落盘；配置文件里的链路凭据已加密存储（见 1.5 b）。**缺**：内存/临时文件中敏感信息的「用后清零」无显式保证。 |

### 1.7 入侵防范（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| a) 遵循最小安装，仅开放必要服务/端口 | ✓ | `cmd/uic/main.go` listener；`internal/northbound/httpapi` 路由 | 单一 HTTP 端口，仅注册北向 API 路由（`route(...)`）；采集/北向外向按需启用，无多余守护端口。 |
| b) 最小授权，及时关闭不需要的会话 | ◐ | `internal/rbac`；`internal/auth` Open 模式；`internal/auth/policy.go` `SessionAlive` | 最小授权已落地（1.2）。**「关闭不需要的会话」**：HTTP 服务设 `ReadHeaderTimeout`/`IdleTimeout`/`MaxHeaderBytes`（`main.go:385`），**会话闲置超时已落地（已修复 R-3）**——`Policy.SessionAlive` 对每个 Token 记录最后成功时间，闲置超 `idleTimeout`（默认 30 分钟）即拒绝并要求轮换，超时阈值经 `server.auth.idleTimeout` 配置。 |
| c) 限制远程管理端口、防暴力破解 | ✓ | `internal/auth/policy.go` `Policy.Allow`/`RecordFailure`；`internal/northbound/httpapi/httpapi.go` `guard` | 已修复 R-3。**登录失败锁定**：`Policy.RecordFailure` 累计 per-IP 失败，达 `maxAuthFailures`（默认 5）锁定 `lockoutDuration`（默认 15 分钟），锁定期间 `guard` 前置 `Policy.Allow` 直接 429（不进认证、不污染审计）；**全局熔断**防分布式绕过（窗口内累计 `globalMaxFailures` 触发全站拒绝）；**可信代理白名单**防反代部署下按反代 IP 误锁。全部可经 `server.auth` 配置。 |
| d) 安装必要运行补丁/安全加固 | ✗ | — | 缺口：补丁/加固属运维域，软件不内置。 |
| e) 主机入侵防范（防恶意代码） | ✗ | — | 不在本软件范围（依赖部署主机）。 |

### 1.8 恶意代码防范 / 资源管理（控制点 8.1.4）

| 等保要求（第三级） | 状态 | 实现与出处 | 说明 |
|---|---|---|---|
| 内存/协程等资源滥用防护 | ✓ | `internal/outbox` 有界；`internal/audit` 有界；`internal/history` 保留策略 | Outbox/审计/历史均**有界**（防写满磁盘拖垮采集，见 `audit.go:11` 注释）；watchdog 防采集中枢卡死。属「资源管理」可落地的软件侧证据。 |
| 输入校验（防注入/畸形） | ✓ | `internal/config` `Validate`（`DisallowUnknownFields`）；各驱动写路径边界校验 | 配置解码 `DisallowUnknownFields`（`backends.go:44`）；协议驱动写路径在发报文前拒绝越界/带小数/溢出/超长（见驱动 SDK spec，如 ENIP/S7 写路径）。 |

---

## 2. 已知缺口与风险登记

等保三级测评若以本软件为被测对象，以下项是**必须正视的缺口**，按风险等级排列：

| 编号 | 缺口 | 对应控制点 | 风险 | 处置建议 |
|---|---|---|---|---|
| **R-1** ✅已修复 | ~~TLS 开启但缺证书时静默回退明文 HTTP~~：已改为 fail-closed——`enabled` 无证书文件且无 `auto` 时配置期（`config.validateServerTLS`）直接拒绝，不再静默降级；同时实现 `server.tls.auto` 自动生成自签证书（`internal/tlscert`，纯标准库），免证书分发即可拿到传输加密 | 安全通信 / 数据保密性 (传输) | ~~高~~ 已消除 | 已落地并测试守护（`tlscert.TestGenerateProducesUsableCert` 证书可用、config `TestValidateServerTLS` 三合法两非法形态）。自签不提供来源可信，生产仍建议 CA 证书。 |
| **R-2** ✅已修复 | ~~配置内链路凭据明文存储~~：已改为信封加密——`internal/config/envelope`（AES-256-GCM）+ `config.encryptChannelSecrets`/`decryptChannelSecrets`，写盘加密、读盘解密，KEK 来自 `UIC_CONFIG_KEK` 环境变量或派生自 `server.token`；未注入 KEK 时明文落盘（向后兼容），KEK 不匹配/篡改时 fail-closed | 数据保密性 (存储) / 剩余信息保护 | ~~中~~ 已消除 | 已落地并测试守护（`envelope.TestRoundTrip`/`TestTamperDetection`/`TestWrongKEK`、`config.TestConfigEnvelopeEncryption` 端到端）。3 个变异体验证。自签证书不提供来源可信，生产仍建议专用 KEK（环境变量）+ 定期轮换。 |
| **R-3** ✅已修复 | ~~无登录失败锁定 / 无会话超时退出~~：已落地 `internal/auth/policy.go`——per-IP 失败锁定（`maxAuthFailures` 默认 5 次/`lockoutDuration` 默认 15 分钟）+ 全局熔断防分布式绕过（`globalMaxFailures` 默认 20/`globalFailureWindow` 默认 5 分钟）+ 会话闲置超时（`idleTimeout` 默认 30 分钟，`SessionAlive` 按 Token sha256 记最后成功时间）+ 可信代理白名单（XFF 防误锁）。全部经 `server.auth` 配置，零值回退等保基线。同时修复 `ServeHTTP.authorized` 单凭据前置认证与 `guard` 双重认证割裂（错 token 在入口即被拦、policy 收不到失败事件）的设计缺陷 | 身份鉴别 b) / 入侵防范 c) | ~~中~~ 已消除 | 已落地并测试守护（`auth` 14 单测 + `httpapi` 4 集成测试：IP 锁定、全局熔断、会话超时、可信代理 XFF）。3 个变异体验证（RecordFailure 不计数、SessionAlive 超时放行、Allow 锁定失效，均先编译通过再变红）。 |
| **R-4** | **无口令到期强制更换** | 身份鉴别 d) | 低 | 配置项可手工轮换；建议纳入运维制度。 |
| **R-5** | **依赖与工具链漏洞面按构造为零，但工具链自身仅 1 条已知 advisory**（ADR-0004：GO-2026-6859，1.26.11 修复） | 安全审计 / 入侵防范 | 低：advisory 在固定 1.26.10 上「已知但接受」，bump 受 GOSUMDB=off 阻断 | 维持 ADR-0004 决策；升 1.26.11 需同时启用 sumdb 或 vendor 工具链。 |

> **为什么不在本表把这些标成 ✓**：本仓的安全设计有一条自我纪律（见 `06-nfr.md` §8 安全红线与驱动能力「能力位=实际行为」原则）——**不确定时不虚标**。等保对照表的每一条都是采购/测评要采信的证据，虚标一条就会让整张表的合规性崩塌。因此缺口一律如实登记在 §2，并给出可执行的处置建议。

---

## 3. 不在软件范围内的控制点

以下等保 2.0 控制点**不是本仓软件能覆盖的**，需由部署方的整体架构与运维体系满足。列出以免采购方误读为「软件已含」：

- **安全物理环境**（8.1.1）：机房、设备、介质物理防护——纯设施侧。
- **安全通信网络**（8.1.2）网络层：网络架构划分、传输保密性（网络层）、网络整体可证——依赖部署网络拓扑；本软件仅提供传输加密能力（API TLS、协议 TLS），不提供网络层隔离。
- **安全区域边界**（8.1.3）：边界防火墙、入侵检测/防御、边界完整性——安全设备侧。
- **安全管理中心**（8.1.5）：系统管理/审计管理/安全管理三员分立、集中管控、日志集中分析——需 SOC 平台；本软件提供审计导出与三角色分离的**雏形**，但集中化属平台侧。
- **安全管理制度 / 机构 / 人员 / 建设 /运维**（8.1.6–8.1.10）：管理类要求——组织侧。

---

## 4. 验证与复核

本表的每一条「✓ / ◐」都对应**可运行、可复核**的代码与测试：

- 身份鉴别：`internal/auth` `auth_test.go`（Open 模式、sha256-only 凭据不接受明文、用户枚举不泄露 Token、弱口令拒绝）+ `policy_test.go`（per-IP 锁定与解锁、全局熔断与窗口重置、会话闲置超时、可信代理 XFF 提取、默认阈值）+ `internal/northbound/httpapi/httpapi_test.go` 集成测试（IP 锁定、全局熔断、会话超时、可信代理 XFF 端到端）。
- 认证策略配置：`internal/config`（`AuthPolicy` 字符串时长 ↔ `auth.AuthPolicy` 时长转换，见 `runtime.go` `buildAuth` 后的转换闭包）。
- 访问控制：`internal/rbac/rbac_test.go`（未知角色拒绝、危险命令归 `command.danger`）。
- 安全审计：`internal/audit/audit_test.go`（哈希链成立、篡改被抓、删除被抓、重启续链、0o600 权限、淘汰后可校验）。
- 数据完整性：配置 `VersionOf` 快照哈希、升级包逐文件 SHA-256（`upgrade_test.go`、`cmd/uic/pkg_test.go` `TestUpgradeTamperedRejected`）。
- 传输加密：`cmd/uic/main.go:393` TLS 分支。

> 本表本身是**文档**，不含可执行断言，故无变异测试。但它映射的每一条能力都有对应的回归测试在 `make check` 中守护——任一能力被破坏，相关测试即变红，本表也就随之失效。这是本表「只登记真实能力」的执行机制。

---

## 5. 关联

- `06-nfr.md` §4 安全、§10 测试策略、§11 需求追踪矩阵
- `05-northbound-api-spec.md` §2.2 RBAC 权限矩阵
- `docs/04-decisions/0004-toolchain-dependency-security.md`（工具链与依赖安全，对应 §2 R-5）
- `docs/todo.md` G12 许可与供应链（SBOM 已落地、发布通道签名待落地）
