# UIC 工业网关 · .NET 10 实现

跨平台工业数据采集网关的 .NET 10 版本：南向接 Modbus（TCP / RTU / ASCII，主站与从站），
中间做采集调度、数据加工与持久化队列，北向出 REST / SSE / MQTT / OPC UA 四个通道。
控制台是 Uno Platform 编译到 WebAssembly 的单页应用，由同一个进程托管，浏览器直接访问。

设计重心不在「能连多少种设备」，而在**可治理**：

- **断网不丢数据** —— 每个北向通道独立分区、独立确认位点，磁盘队列三重上限（条数 / 字节 / TTL）；
- **配置改坏能退回** —— 配置即制品：内容寻址、只增不改、可 diff、可回滚；
- **设备坏了能看出为什么坏** —— 质量码贯穿采集、缓存、北向全链路，错误原因一路带到 UI；
- **一个二进制能拷到盒子上跑** —— 只依赖框架自带的库，发布产物自包含。

## 快速开始

```bash
# 编译全部项目（含 WASM 前端）
dotnet build uic.sln

# 仿真模式：进程内起一个 Modbus 从站 + 一台虚拟设备，另有一台设备走真实 Modbus 主站链路
dotnet run --project src/Uic.Host -- sim --listen 127.0.0.1:8080

# 另开一个终端
curl -s localhost:8080/api/v1/health
curl -s localhost:8080/api/v1/points
```

不带参数直接 `run` 时使用内置示例配置（一台虚拟设备），不需要任何硬件：

```bash
dotnet run --project src/Uic.Host -- run --data ./data
```

只校验配置、不启动服务（适合放进 CI 与发布前检查）：

```bash
dotnet run --project src/Uic.Host -- check --config ../examples/demo.json
```

## 控制台（WASM）

前端是独立的 Uno 项目，编译目标是 `net10.0-browserwasm`。发布后把静态目录交给宿主即可：

```bash
dotnet publish src/Uic.Ui -f net10.0-browserwasm -c Release -o /tmp/uic-ui
dotnet run --project src/Uic.Host -- run --web /tmp/uic-ui/wwwroot
```

浏览器打开 `http://127.0.0.1:8080/`。六个页面分别是设备、驱动、点位、监控、日志、配置，
全部只通过 REST 与 SSE 通信——前端不认识任何驱动实现，也不持有第二份状态。
设备页的「新建设备」可以同时新建一条通道并挂上第一台设备（首个点位），提交走
`config/validate` → `config/apply`，与配置页共用同一条版本与回滚链；继续加点位仍走
CSV 导入或配置页。
配置了 `northbound.rest.token` 时，首屏会先要求输入令牌（存在浏览器本地）。

> 发布必须显式指定 `-f net10.0-browserwasm`：项目是单目标 WASM 工程，
> 省略框架时 MSBuild 会按 `net10.0` 还原并报 `NETSDK1129`。

发布产物本身可以用一个脚本验收（静态层 + 真浏览器层）：

```bash
scripts/wasm-smoke.sh              # 发布 + curl 检查 + Playwright 真跑一遍
scripts/wasm-smoke.sh --no-browser # 只做静态层
```

浏览器层会等 WASM 启动、等设备列表从 REST 渲染出来，再在设备页真填一次「新建设备」
表单并等它出现在列表里，同时断言没有未捕获异常、失败请求或 4xx/5xx ——
裁剪后的运行时问题只有这样才看得见。

## 命令行

```
uic run   [--config <file>] [--data <dir>] [--listen <host:port>] [--web <dir>] [-v]
uic sim   [--port <modbus-port>] [--data <dir>] [--listen <host:port>] [-v]
uic check [--config <file>]
uic version
```

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 2 | 配置非法（语法或语义） |
| 3 | 配置无法读取（文件不存在 / 权限） |
| 64 | 命令行参数错误 |

`check` 会把所有问题一次列全，而不是遇到第一条就退出：

```
invalid: /etc/uic/gateway.json
  - site: must match ^[a-z][a-z0-9-]{1,31}$
  - devices[d1].channelId: channel 'missing' not found
  - devices[d1].points: at least one point is required
```

## 测试

测试是一个自包含控制台运行器，不引 xUnit、不引 Test SDK —— 离网构建机也能直接跑：

```bash
dotnet run --project tests/Uic.Tests                        # 全部用例
dotnet run --project tests/Uic.Tests -- --list              # 列出用例名
dotnet run --project tests/Uic.Tests -- --filter planner    # 按名字过滤
dotnet run --project tests/Uic.Tests -- --opcua             # 额外跑 OPC UA 端到端验收
dotnet run --project tests/Uic.Tests -- --stop-on-fail
```

正常情况下输出 `199 passed, 0 failed, 1 skipped`（跳过的是默认不启用的 OPC UA 用例）。
黄金帧测试默认从仓库的 `testdata/golden/` 读取，可用 `UIC_TESTDATA` 指向别的目录。

串口用例（T15–T18）会用 libc 开一对伪终端，在 Linux 上默认执行，其它平台报 `skip`；
不需要 `socat`，也不需要真实串口设备。

MQTT 用例连的是测试进程内的最小 broker，做协议级验收。与真实 broker 的互操作
由 `scripts/mqtt-interop.sh` 完成（起官方 mosquitto 镜像，验证上线/重连/下行命令/遗嘱）：

```bash
scripts/mqtt-interop.sh          # 需要 docker，结束后自动清理
scripts/mqtt-interop.sh --keep   # 保留配置、日志与抓包，便于排查
```

覆盖范围与规范用例（T1–T18）的对应关系见 [docs/testing.md](docs/testing.md)，
PRD 需求逐条追溯见 [docs/requirements-traceability.md](docs/requirements-traceability.md)，
逐协议的实现现状见 [docs/protocol-status.md](docs/protocol-status.md)。

## 目录

```
src/
  Uic.Domain/         领域模型与线上序列化契约（无外部依赖）
  Uic.Driver/         驱动 SDK：接口、元数据、错误分类
  Uic.Codec.Modbus/   Modbus PDU 编解码、校验和、字节序、批量窗口
  Uic.Driver.Modbus/  Modbus 主站（TCP / RTU / ASCII）
  Uic.Driver.Virtual/ 虚拟设备：波形 + 故障注入
  Uic.Sim.Modbus/     Modbus 从站模拟器（被 sim 模式与测试复用）
  Uic.Transport/      字节流：TCP / 串口 / 内存
  Uic.Engine/         调度器、设备 actor、读规划、数据加工、点位缓存
  Uic.Outbox/         持久化上报队列（分区分位点）
  Uic.Config/         配置存储：内容寻址、版本、diff、回滚、兜底配置
  Uic.Obs/            指标注册表、环形缓冲日志、JSON 行文件日志
  Uic.Northbound/     REST / SSE / MQTT / 错误信封 / 限流
  Uic.OpcUa/          OPC UA 服务端与地址空间
  Uic.Runtime/        装配层：把上面这些接在一起
  Uic.Host/           宿主进程与命令行
  Uic.Ui/             Uno WASM 控制台
tests/Uic.Tests/      自包含测试运行器与全部用例
scripts/              外部依赖的验收脚本（容量基准、历史查询基准、mosquitto 互操作、WASM 浏览器冒烟）
testdata/golden/      黄金帧（请求字节 / 响应字节 / 期望值）
docs/                 架构、配置、北向契约、测试矩阵、实现说明
```

依赖方向是单向的：`Domain ← Driver ← Codec/Transport ← Engine ← Outbox/Northbound ← Runtime ← Host`。
任何一层都不认识它上面的层，`Runtime` 是唯一做装配的地方。

## 第三方依赖

| 包 | 用途 |
|---|---|
| `Microsoft.Extensions.Logging.Abstractions` 10.0.14 | 日志抽象（实现由宿主注入） |
| `System.IO.Ports` 10.0.14 | 串口字节流 |
| `FrameworkReference Microsoft.AspNetCore.App` | 宿主、REST、SSE |
| `OPCFoundation.NetStandard.Opc.Ua.Server` / `.Configuration` 1.5.378.182 | OPC UA 服务端 |
| `OPCFoundation.NetStandard.Opc.Ua.Client` 1.5.378.182 | 仅测试：用标准客户端验收端点 |

前端另用 Uno Platform（`Uno.WinUI` / `Uno.Wasm.Bootstrap`），版本集中在 `src/Uic.Ui/Uic.Ui.csproj`。

## 文档

- [docs/architecture.md](docs/architecture.md) —— 分层、数据流、关键设计决策
- [docs/configuration.md](docs/configuration.md) —— 配置模型、写法、校验规则、版本管理
- [docs/northbound-api.md](docs/northbound-api.md) —— REST / SSE / MQTT / OPC UA 契约
- [docs/testing.md](docs/testing.md) —— 测试矩阵、黄金帧格式、留白说明
- [docs/requirements-traceability.md](docs/requirements-traceability.md) —— PRD 每条 FR/NFR 的落地状态、证据与补法
- [docs/protocol-status.md](docs/protocol-status.md) —— 每个协议做到哪一步、缺什么、为什么
- [docs/implementation-notes.md](docs/implementation-notes.md) —— 与设计文档的对应关系、已知边界
