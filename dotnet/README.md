# UIC 工业网关 · .NET 10 实现

跨平台工业数据采集网关的 .NET 10 版本：南向接 Modbus（TCP / RTU / ASCII，主站与从站，
寄存器与文件记录寻址）、
S7comm（S7-300/400/1200/1500 主站）、BACnet/IP（对象与属性寻址）、
OPC UA 客户端（None / Basic256Sha256）与
MQTT 订阅（QoS0/1 + TLS，只读），
中间做采集调度、数据加工与持久化队列，北向出 REST / SSE / MQTT / OPC UA 四个通道。
控制台是 Uno Platform 编译到 WebAssembly 的单页应用，由同一个进程托管，浏览器直接访问。

设计重心不在「能连多少种设备」，而在**可治理**：

- **断网不丢数据** —— 每个北向通道独立分区、独立确认位点，磁盘队列三重上限（条数 / 字节 / TTL）；
- **配置改坏能退回** —— 配置即制品：内容寻址、只增不改、可 diff、可回滚；
- **谁改了什么查得到** —— 配置变更、点位写入、远程命令与认证失败都落审计 NDJSON，
  可按时间 / 动作 / 身份查询与导出，重启不清空；
- **设备坏了能看出为什么坏** —— 质量码贯穿采集、缓存、北向全链路，错误原因一路带到 UI；
- **一个二进制能拷到盒子上跑** —— 只依赖框架自带的库，发布产物自包含。

## 快速开始

```bash
# 编译全部项目（含 WASM 前端）
dotnet build uic.sln

# 仿真模式：进程内起 Modbus 从站（15020）、S7 从站（15102）与
# BACnet/IP 从站（47810/UDP）+ 一台虚拟设备，
# 另有三台设备分别走真实的 Modbus / S7 / BACnet 主站链路；
# 端口可用 --port / --s7-port / --bacnet-port 改
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

浏览器打开 `http://127.0.0.1:8080/`。八个页面分别是设备、驱动、点位、监控、日志、审计、
配置、账号，按当前账号的权限显示；全部只通过 REST 与 SSE 通信——前端不认识任何驱动实现，
也不持有第二份状态。
设备页的「新建设备」可以同时新建一条通道并挂上第一台设备（首个点位），提交走
`config/validate` → `config/apply`，与配置页共用同一条版本与回滚链；继续加点位仍走
CSV 导入或配置页。
首次启动会自动创建 `admin`，随机初始口令写在 `data/security/initial-admin-password.txt`：

```bash
cat ./data/security/initial-admin-password.txt   # 只用于第一次登录
```

登录后必须先改密，改密成功该文件会被删除。账号与角色（`admin` / `operator` / `viewer` /
`auditor`）由控制台「账号」页或 REST 管理；忘记口令时可在本机执行
`uic user reset-password --user admin`。静态 `northbound.rest.token` 仍可作为全权限
API Token 给运维脚本使用，但它不再能让浏览器跳过登录。

> 发布必须显式指定 `-f net10.0-browserwasm`：项目是单目标 WASM 工程，
> 省略框架时 MSBuild 会按 `net10.0` 还原并报 `NETSDK1129`。

发布产物本身可以用一个脚本验收（静态层 + 真浏览器层）：

```bash
scripts/wasm-smoke.sh              # 发布 + curl 检查 + Playwright 真跑一遍
scripts/wasm-smoke.sh --no-browser # 只做静态层
scripts/wasm-smoke.sh --browser firefox   # 换引擎（默认 chromium）
```

浏览器层会用初始口令登录、走完强制改密，等设备列表从 REST 渲染出来，再在设备页真填一次
「新建设备」表单并等它出现在列表里，同时断言没有未捕获异常、失败请求或 4xx/5xx ——
裁剪后的运行时问题只有这样才看得见。Chromium 与 Firefox 各跑过一遍。

## 命令行

```
uic run   [--config <file>] [--data <dir>] [--listen <host:port>] [--web <dir>] [-v]
uic sim   [--port <modbus-port>] [--data <dir>] [--listen <host:port>] [-v]
uic check [--config <file>]
uic secret keygen [--out <file>]          # 生成 32 字节密封密钥（仅属主可读）
uic secret seal [--key-file <file>]       # 从 stdin 读明文，输出 enc:v1:...
uic user list [--data <dir>]              # 列出本地账号、角色与最近登录
uic user reset-password --user <id>       # 重置为一次性口令（走 stdout，提示走 stderr）
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
  - northbound.rest.token: plaintext credentials are not allowed; use ${env:NAME}, ...
```

它还会用**与启动完全相同的那份驱动注册表**解析「设备 → 通道 → 驱动」这条引用链：
`channels[].protocol` 没注册时同样退出码 2，并指明是哪条通道。

```
invalid: /etc/uic/gateway.json
  unresolved channel/driver references (1 problem(s))
    channels[bus].protocol: no driver is registered for protocol 'modbus-tpc'
```

这条检查只读不写，和启动预检共用同一份实现——「`check` 通过、启动却起不来」
是最难查的一类不一致，不能靠两份各自演进的判定。

## 凭据

配置里的 `northbound.rest.token` 与 `northbound.mqtt.password` 不接受明文，
只接受三种写法之一：

```jsonc
{ "token": "${env:UIC_REST_TOKEN}" }              // 环境变量（compose / systemd / K8s）
{ "token": "${file:/run/secrets/rest_token}" }    // 文件（Docker secret 的挂载方式）
{ "token": "enc:v1:AQID..." }                     // AES-256-GCM 密文
```

```bash
export UIC_REST_TOKEN='...'
uic secret keygen --out /etc/uic/secret.key      # 0600；文件系统不记录权限时会告警
uic check --config gateway.json                  # 逐项打印引用来源，确认都解析得出来
```

密钥从 `UIC_SECRET_KEY`（base64/hex）或 `UIC_SECRET_KEY_FILE` 读取。引用解析
失败——环境变量没注入、密钥不对、密文被改过——启动与 `check` 都直接失败：
令牌取不到时静默退化成空串不是降级，而是让脚本在第一次请求时吃 401——那是最坏的一种失败。落盘、diff、导出
与 `GET /api/v1/config` 里只有引用（读接口一律显示 `***`），明文只在进程内存里。

旧配置里的明文令牌仍能被**读**（迁移用），但 `check`、启动与 `config/apply` 都会
拒绝它并指出字段路径；先用 `uic secret seal` 加密或改成环境变量引用即可。

## TLS

网关可以直接终结 TLS，也可以继续把它交给反向代理 / stunnel。两种都支持，选一种即可：

```jsonc
{
  "rest": {
    "listen": "0.0.0.0:8443",
    "tls": {
      "enabled": true,
      "certificateFile": "security/gateway.pfx",
      "certificatePassword": "${env:UIC_TLS_PFX_PASSWORD}"
    }
  },
  "mqtt": {
    "enabled": true,
    "host": "broker.local",
    "port": 8883,
    "tls": { "enabled": true, "caFile": "security/broker-ca.pem" }
  }
}
```

- `rest.tls`：Kestrel 直接在 `rest.listen` 端口上监听 HTTPS；证书支持 PFX
  （`certificateFile` + `certificatePassword`）与 PEM（`certificateFile` + `keyFile`）。
  证书路径是文件路径，相对路径按 `storage.dataDir` 解析；PFX 口令仍是凭据引用。
- `mqtt.tls`：客户端用 `SslStream` 校验服务端证书链与主机名；自签 Broker 把 CA
  写成 PEM 放进 `caFile`。`insecureSkipVerify` 只用于联调，启动会告警。
- 证书缺失、读不出来或口令不对，`uic check` 与启动都直接失败；启动按配置非法
  退出（退出码 2），不会退回明文端口。
- 证书与监听地址的变更需要重启，不走配置热更新。

自签证书联调（生产请用内部 CA 签发，并在客户端侧用同一 CA 校验）：

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
  -keyout gateway.key -out gateway.crt \
  -subj "/CN=gateway.local" \
  -addext "subjectAltName=DNS:gateway.local,IP:192.168.1.10"
openssl pkcs12 -export -inkey gateway.key -in gateway.crt -out gateway.pfx

ls -l gateway.pfx            # 放进 storage.dataDir（默认 ./data）下的 security/
export UIC_TLS_PFX_PASSWORD='...'
uic check --config gateway.json
```

## 审计

审计是「谁在什么时候改了什么」的落盘记录，和会轮转、会丢的日志分开：

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/v1/audit?action=point.write&from=2026-11-08T00:00:00Z' | jq

curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/v1/audit/export?action=config.apply' -o audit.ndjson
```

记录覆盖配置生效 / 回滚 / 导入 / 模板实例化、未脱敏导出、逐条点位写入（含旧值与新值）、
REST 与 MQTT 命令、登录 / 登出 / 改密 / 越权与账号变更，以及审计自身的导出动作。
`actor` 是真实账号 id（静态令牌记 `api-token`，认证失败记 `anonymous`）。文件写在
`storage.audit.directory`（默认 `data/audit/`，一行一条 NDJSON），按天与按大小切分，
保留最近 `retain` 份；每条立即 flush，进程被杀也只丢最后半行，重启后序号继续递增。

写审计失败不会让业务失败：只告警并把指标 `uic_audit_write_failures` 置为非零——
审计不可用本身要能被看见，而不是伪装成成功。字段与查询参数见
[docs/northbound-api.md](docs/northbound-api.md#审计)，配置见
[docs/configuration.md](docs/configuration.md#审计)。

## 测试

测试是一个自包含控制台运行器，不引 xUnit、不引 Test SDK —— 离网构建机也能直接跑：

```bash
dotnet run --project tests/Uic.Tests                        # 全部用例
dotnet run --project tests/Uic.Tests -- --list              # 列出用例名
dotnet run --project tests/Uic.Tests -- --filter planner    # 按名字过滤
dotnet run --project tests/Uic.Tests -- --opcua             # 额外跑 OPC UA 端到端验收
dotnet run --project tests/Uic.Tests -- --stop-on-fail
```

正常情况下输出 `362 passed, 0 failed, 3 skipped`（跳过的是默认不启用的 OPC UA 用例）；
加 `-- --opcua` 是 `365 passed, 0 failed, 0 skipped`。
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
  Uic.Codec.S7/       TPKT / COTP / S7 PDU 编解码与地址解析（纯函数）
  Uic.Codec.Bacnet/   BVLC / NPDU 封拆帧、应用与上下文标签、ReadProperty / WriteProperty / Who-Is 编解码
  Uic.Codec.Mqtt/     MQTT 3.1.1 客户端（发布 + 订阅）、主题匹配、TLS 装载
  Uic.Driver.Modbus/  Modbus 主站（TCP / RTU / ASCII）
  Uic.Driver.S7/      S7comm 主站：会话、PDU 协商、按 PDU 切片、位读写
  Uic.Driver.Bacnet/  BACnet/IP 主站：Who-Is 探活、按 invoke-id 配对、对象属性读写
  Uic.Driver.OpcUa/   OPC UA 客户端：NodeId 寻址、批量读、写、订阅、证书/PKI
  Uic.Driver.Mqtt/    MQTT 南向订阅：主题寻址、JSON 字段取值、缓存与重连
  Uic.Driver.Virtual/ 虚拟设备：波形 + 故障注入
  Uic.Sim.Modbus/     Modbus 从站模拟器（被 sim 模式与测试复用）
  Uic.Sim.S7/         S7 从站模拟器（同上）
  Uic.Sim.Bacnet/     BACnet/IP 从站模拟器（同上，UDP）
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
| `OPCFoundation.NetStandard.Opc.Ua.Server` / `.Configuration` 1.5.378.182 | OPC UA 服务端（北向） |
| `OPCFoundation.NetStandard.Opc.Ua.Client` 1.5.378.182 | OPC UA 客户端驱动（南向），测试里同时用它验收服务端 |

前端另用 Uno Platform（`Uno.WinUI` / `Uno.Wasm.Bootstrap`），版本集中在 `src/Uic.Ui/Uic.Ui.csproj`。

## 文档

- [docs/architecture.md](docs/architecture.md) —— 分层、数据流、关键设计决策
- [docs/configuration.md](docs/configuration.md) —— 配置模型、写法、校验规则、版本管理
- [docs/northbound-api.md](docs/northbound-api.md) —— REST / SSE / MQTT / OPC UA 契约
- [docs/testing.md](docs/testing.md) —— 测试矩阵、黄金帧格式、留白说明
- [docs/requirements-traceability.md](docs/requirements-traceability.md) —— PRD 每条 FR/NFR 的落地状态、证据与补法
- [docs/protocol-status.md](docs/protocol-status.md) —— 每个协议做到哪一步、缺什么、为什么
- [docs/implementation-notes.md](docs/implementation-notes.md) —— 与设计文档的对应关系、已知边界
