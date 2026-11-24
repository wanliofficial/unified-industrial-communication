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
| G2 | 边缘自治三件套（Outbox/配置回滚/远程命令/看门狗） | P0 | Outbox、配置回滚、看门狗（数据 stale 降级，`runtime.watchdog`/`markStale`）已落地；远程命令、进程级自愈看门狗未落地 |
| G3 | 无真机测试能力 | P0 | 已落地（`internal/sim`、各协议 `*read` 自检） |
| G4 | 四类寻址统一模型 | P0 | 已落地（`internal/model`、`pointtable`） |
| G5 | 配置生命周期（版本/回滚/diff/灰度） | P0 | 版本/回滚已落地；**diff、灰度未落地** |
| G6 | 默认安全基线 | P0 | RBAC/审计/Token 已落地（`auth`/`rbac`/`audit`） |
| G7 | 扩展契约（语义化版本 + 能力声明） | P2 | 静态注册为主，**进程外 gRPC 驱动未落地** |
| G8 | 跨平台交付（单二进制/交叉编译） | P1 | 已落地（`Makefile`：`make build/vet/fmt/test/race/check/release/sbom`；交叉编译 linux/amd64·linux/arm64·windows/amd64，单二进制 ≤15MB） |
| G9 | 数据契约版本化 | P1 | 有 `schemaVersion`，**契约 v2 双发未落地** |
| G10 | 时钟与时间语义 | P2 | 协议层有处理，**系统级时区/时间跳变未落地** |
| G11 | 模板化与批量实例化 | P1 | 已落地（`template`/`instantiate`/`import`） |
| G12 | 许可与供应链（SBOM） | P1 | SBOM 已落地（`make sbom` 从产物内嵌 buildinfo 生成 CycloneDX，零第三方工具、依赖面如实为零）；CI 扫描待平台接入（`make check` 为入口） |

---

## 二、相对路线图尚未实现的功能

Wave 0–2 的协议与核心治理域基本标记为"已落地"。未落地功能集中在 Wave 2 尾部与 Wave 3（行号对应 `docs/03-product/07-roadmap.md`）。

- **前端/组态**：Web 控制台 6 页（代码库无 `web`/`frontend`/`ui` 目录）
- **协议**：OPC DA 桥接（独立 Windows 桥接进程，`:152`）
- **架构**：进程外 gRPC 驱动、驱动市场（签名/版本/兼容性）、多实例协调（`:183`）
- **集中管理**：中心聚合器——只读镜像、配置下发、批量升级、站点分组（`:184`）
- **灰度发布**：分批下发、观察窗口、自动暂停（`:185`）
- **数据契约**：v2 双发过渡、Sparkplug B 的 DataSet/模板（`:186`，README 也声明 DataSet/模板不做）
- **运维**：看门狗数据 stale 降级自愈已落地（`runtime.markStale`）；远程日志拉取、离线升级包、进程级自愈看门狗、Wave 2 的网络诊断与远程配置下发（`:155`/`:187`）未落地
- **国际化**：中/英双语、时区、单位制（`:188`，无 `i18n` 目录）
- **合规交付**：SBOM 已落地（`make sbom`，CycloneDX，零依赖如实表达）、`make release` 三平台交叉编译验收已可执行（`Makefile`）；签名发布、等保对照表、CI 流水线待落地
- **测试**：1000 设备负载测试、驱动一致性扩展（`:158`）
