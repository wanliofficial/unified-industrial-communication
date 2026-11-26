# 功能缺口清单（TODO）

> **来源**：`02-synthesis/03-gap-analysis.md`（竞品缺口分析）、`03-product/07-roadmap.md`（路线图实现状态）
> **生成日期**：2026-11-24
> **说明**：本文记录两类功能缺口——设计阶段识别的 12 个共性缺口在当前项目中的落地情况，以及相对路线图尚未实现的功能。状态基于文档标记与代码库目录核对；其中可机读的项（看门狗、健康、构建/SBOM）已用代码复核确认。

---

## 一、设计文档识别的 12 个缺口

来自 `docs/02-synthesis/03-gap-analysis.md`，是 38 个方案整体都没解决、目标产品需要回答的事。

| 编号 | 缺口 | 优先级 | 本项目中状态 |
|---|---|---|---|
| G1 | 可运维性（指标/健康/诊断） | P0 | 已落地（`internal/obs`；健康在 `internal/runtime` 的 `Runtime.Health()` + `internal/northbound/httpapi` 的 `healthz`/`readyz`） |
| G2 | 边缘自治三件套（Outbox/配置回滚/远程命令/看门狗） | P0 | Outbox、配置回滚、看门狗（数据 stale 降级，`runtime.watchdog`/`markStale`；进程级采集中枢卡死检测，`internal/watchdog` + `/api/v1/health` 的 `scheduler` 块 + `internal/heartbeat` 心跳文件）已落地；远程命令未落地 |
| G3 | 无真机测试能力 | P0 | 已落地（`internal/sim`、各协议 `*read` 自检） |
| G4 | 四类寻址统一模型 | P0 | 已落地（`internal/model`、`pointtable`） |
| G5 | 配置生命周期（版本/回滚/diff/灰度） | P0 | 已落地：版本/回滚（`config.Manager`）、diff（`Manager.Diff` + `GET /api/v1/config/diff`）、灰度发布（`config.StagePlan`/`MergeForBatch` + runtime 编排 + `POST /api/v1/config/staged` 分批下发/`advance`/`abort`，含观察窗口与失败率超阈值自动回滚） |
| G6 | 默认安全基线 | P0 | RBAC/审计/Token 已落地（`auth`/`rbac`/`audit`） |
| G7 | 扩展契约（语义化版本 + 能力声明） | P2 | 静态注册为主，**进程外 gRPC 驱动未落地** |
| G8 | 跨平台交付（单二进制/交叉编译） | P1 | 已落地（`Makefile`：`make build/vet/fmt/test/race/check/release/sbom`；交叉编译 linux/amd64·linux/arm64·windows/amd64，单二进制 ≤15MB） |
| G9 | 数据契约版本化 | P1 | 已落地：`schemaVersion`/`contractVersion` 双版本；`contractVersion=2` 时同一份采集数据经两条独立 Outbox lane（`<transport>` + `<transport>#v2`）同时发 v1 与 v2，覆盖 MQTT/Webhook/Journal；运行期原子热切，不重启采集（`internal/northbound/contract` + `runtime.contractVersion`）。Sparkplug B 自带契约、不参与 JSON v2 双发 |
| G10 | 时钟与时间语义 | P2 | 协议层有处理，**系统级时区/时间跳变未落地** |
| G11 | 模板化与批量实例化 | P1 | 已落地（`template`/`instantiate`/`import`） |
| G12 | 许可与供应链（SBOM） | P1 | SBOM 已落地（`make sbom` 从产物内嵌 buildinfo 生成 CycloneDX，零第三方工具、依赖面如实为零）；CI 扫描待平台接入（`make check` 为入口） |

---

## 二、相对路线图尚未实现的功能

Wave 0–2 的协议与核心治理域基本标记为"已落地"。未落地功能集中在 Wave 2 尾部与 Wave 3（行号对应 `docs/03-product/07-roadmap.md`）。

- **前端/组态**：Web 控制台 6 页（代码库无 `web`/`frontend`/`ui` 目录）
- **协议**：OPC DA 桥接（独立 Windows 桥接进程，`:152`）
- **架构**：进程外 gRPC 驱动、驱动市场（签名/版本/兼容性）、多实例协调（`:183`）。多实例协调中的**实例标识消歧**已落地（`internal/instanceid` 持久化稳定 `instanceId` 到 `<dataDir>/instance.id`；MQTT `clientId` / Sparkplug `nodeId` 默认值派生为 `uic-<site>-<短码>`，消除同站点多实例互相顶掉 Broker 会话的静默失效；暴露到 `GET /api/v1/version` 与诊断包 `manifest.json`）；跨实例 leader 选举/集中协调仍未落地
- **集中管理**：中心聚合器——只读镜像、配置下发、批量升级、站点分组（`:184`）
- **灰度发布**：分批下发、观察窗口、失败率超阈值自动回滚已落地（`POST /api/v1/config/staged` + `/advance` + `/abort`，看门狗兜底自动回滚）；「暂停但不回滚」的形态刻意不做（坏配置继续跑不如直接回滚）（`:185`）
- **数据契约**：v2 双发过渡已落地（`contractVersion=2` 双 lane 热切，见上表 G9）；Sparkplug B 的 DataSet/模板未做（`:186`，README 也声明 DataSet/模板不做）
- **运维**：看门狗数据 stale 降级自愈已落地（`runtime.markStale`）；**进程级采集中枢卡死检测已落地**（`internal/watchdog` 纯判定 + `/api/v1/health` 的 `scheduler` 块 `verdict=healthy|stalled|unknown` + `status="stalled"` 与 `state="stalled"` problem + 指标 `uic_scheduler_stalled_total`/`uic_scheduler_tick_age_seconds` + `internal/heartbeat` 原子写 `<dataDir>/heartbeat.json` 供不访问 HTTP 的 supervisor 区分「进程死了」与「活着但卡死」；`/healthz` 无条件 200 的既有契约保持不变）；远程日志拉取已落地（`GET /api/v1/logs`，按 device/level/limit 过滤，此前是假阴性）、诊断包已落地（`GET /api/v1/diagnostics/bundle`）；网络探测已落地（`GET /api/v1/diagnostics/probe`，仅探测配置内通道/设备、TCP 握手 + RTT、不发协议字节，`internal/probe`，此前规范里的 `/network?device=` 从未实现）；**离线升级包校验已落地**（`internal/upgrade` + `uic makepkg`/`uic upgrade`：gzip+tar 包内含 `manifest.json`（schema=`uic-upgrade/v1`，逐文件 sha256 + 目标 os/arch + 入口二进制）与可选 `manifest.sig`（ed25519 对 manifest 原文签名）；分层校验来源（ed25519）/完整性（sha256）/适配性（os/arch）三层，且「给了公钥但包未签名」与「包已签名但缺公钥」两种半验证都显式拒绝；`uic upgrade` 校验通过后走 tmp+rename 原子安装并备份旧二进制到 `<path>.bak`，**不自动重启**，需 `-yes` 人工闸；17 个变异体全部先编译通过再变红）；远程配置下发（`:155`/`:187`）未落地
- **国际化**：中/英双语、时区、单位制（`:188`，无 `i18n` 目录）
- **合规交付**：SBOM 已落地（`make sbom`，CycloneDX，零依赖如实表达）、`make release` 三平台交叉编译验收已可执行（`Makefile`）；ed25519 签名能力已落地并用于离线升级包（`internal/upgrade` 的 `manifest.sig`，标准库 `crypto/ed25519`，密钥经 `-privkey` 传入不落盘），但发布通道签名/验签、等保对照表、CI 流水线待落地
- **测试**：1000 设备负载测试已落地（`make load` / `cmd/loadgen`，真实 runtime 端到端，ADR C4/C5 NFR 断言见 `internal/loadtest`）；驱动一致性扩展已落地（`internal/driver/conformance` 共享套件 `Check`/`CheckRegistry`，`internal/runtime/conformance_test.go` 遍历注册表全量驱动）；凭据声明收敛到单一事实源（`driver.ConfigField.Secret` + `driver.LooksSecret` 名字兜底，脱敏与一致性套件同源；顺带修掉 SNMP 团体名明文回显）；实例标识消歧测试已落地（`internal/instanceid` 首次生成/重启复用/空文件重生/不同目录不同身份；`internal/runtime` 验证 `/version` 暴露、诊断包含 `instanceId`、MQTT clientID 与 Sparkplug nodeID 默认带实例短码且不覆盖用户显式配置，8 个变异体全部变红）；网络探测测试已落地（`internal/probe` 可达/不可达/超时/缺端口/UDP unverified/unsupported/保序/ctx 取消/并发有界；`internal/runtime` 设备→通道解析、全文/子集/未知 id 400、RBAC、真实回环 ok 与未监听 fail；9 个变异体全部变红，覆盖 UDP 假可达、状态翻转、summary 计数、未知 id 静默、超时夹取、参数乱序、设备解析、设备未知静默、DTO 映射）；进程级看门狗测试已落地（`internal/watchdog` healthy/stalled/边界严格大于/unknown/时钟回拨/默认阈值；`internal/scheduler` LastTick 启动前 unknown、Start 同步种子、tick 持续推进、空设备仍推进；`internal/heartbeat` 原子写/无残留 tmp/覆盖写/亚秒 TSMs；`internal/runtime` 正常 healthy、注入未来时刻判 stalled 且 `status=stalled` 压过设备态、阈值真的生效、心跳文件生成/ts 前进/启动即写/禁用不生成、一次卡死只记一次并写 tick 年龄 gauge、`/healthz` 契约不变；18 个变异体全部先编译通过再变红，覆盖边界 `>` 改 `>=`、stalled↔healthy 翻转、unknown 判 stalled、负 age 不夹零、health 不置 stalled/不追加 problem、阈值忽略注入、每周期重复计数、恢复不清零、心跳禁用失效/周期被硬编码/TSMs 恒 0/不 rename/启动不写、scheduler 不存或恒报 false、Start 不种子）；**离线升级包测试已落地**（`internal/upgrade` 签名 round-trip、未签名默认拒 `ErrUnsigned`、签名需正确公钥、`ErrPubkeyRequired` 哨兵、篡改 sha256 报完整性、缺文件报错、os/arch 不匹配拒绝、schema 不符拒绝、Apply 替换+备份/首次安装无备份/备份路径、`Build` 重算哈希/缺二进制报错、`Open` 拒垃圾；`cmd/uic/pkg_test.go` 真实二进制 exec 端到端 8 例：签名替换、错误公钥拒、签名缺公钥拒、未签名策略、需 `-yes`、dry-run（含 `-yes` 仍不替换）、arch 不匹配、篡改包拒；17 个变异体全部先编译通过再变红，覆盖验签恒真/验签失败返回 nil、签名包缺公钥放过、给了公钥但包未签名放过、未签名默认放行、schema 不校验、sha256 比对跳过、缺文件不报错、os/arch 不匹配放过、Build 不按实际内容重算哈希、Apply 不备份/首次安装伪装有备份/备份路径算错、CLI 校验失败仍继续安装/无 `-yes` 也替换/dry-run 仍替换）
