# 统一工业通信网关（UIC）

跨平台工业数据采集中间件：南向接协议，中间做建模与调度，北向出标准接口（REST / SSE / MQTT / OPC UA）。

设计重心不在「能连多少种设备」，而在**可治理**：断网不丢数据、配置改坏能退回、设备坏了能看出为什么坏、一个二进制能拷到 ARM 盒子上跑。

## 快速开始

```bash
go build -o uic ./cmd/uic
./uic -check -config examples/demo.json   # 只校验配置
./uic -config examples/demo.json          # 虚拟设备演示，无需硬件
curl -s localhost:8080/api/v1/health
```

真实协议回环（主站 + 内置从站，同一进程树内）：

```bash
./uic sim -listen 127.0.0.1:15020 &
./uic -config examples/modbus-loopback.json
curl -s localhost:8081/api/v1/points
```

串口 / Modbus RTU（有硬件改端口即可，无硬件可用 `socat` 造一对 pty）：

```bash
./uic -check -config examples/modbus-rtu.json
./uic -config examples/modbus-rtu.json
```

Modbus ASCII（`:` + 十六进制 + LRC + CRLF，老式仪表常见）：

```bash
./uic -check -config examples/modbus-ascii.json
```

S7（S7comm / ISO-on-TCP，TCP 102）：

```bash
./uic sim -protocol s7 -listen 127.0.0.1:15102 &   # 内置 S7 从站
./uic -config examples/s7.json
curl -s localhost:8085/api/v1/points
#   7 点：DB1 的 REAL/INT16/DINT32/STRING、DB2 WORD、IW0、M0.3
#   -pdu 240 可验证按协商 PDU 切块
```

OPC UA（二进制，SecurityPolicy None / 匿名）：

```bash
./uic sim -protocol opcua -listen 127.0.0.1:14840 &   # 内置 OPC UA 服务端
./uic -config examples/opcua.json
curl -s localhost:8086/api/v1/points
#   6 点：double / int32 / bool / string / 可写 double / 标准节点 ns=0;i=2258
#   写穿透：curl -X POST .../points/ua1/setpoint/write
```

从站（把网关采集到的点位反向暴露给 SCADA / 第三方主站）：

```bash
./uic sim -listen 127.0.0.1:15020 &           # 上游设备
./uic -config examples/modbus-slave.json &    # REST :8086，Modbus 从站 :1502
./uic mbread -addr 127.0.0.1:1502 -unit 1 -area holding -offset 0 -qty 4
./uic mbread -addr 127.0.0.1:1502 -area holding -offset 3 -write 777
```

`uic mbread` 是内置的极简主站：寄存器打印 uint16 / int16 / 十六进制并拼 float32，
位区逐位打印。写入会穿透到真实设备，不是改缓存。

MQTT 北向（需要一个 Broker；`mosquitto` 或任意 MQTT 3.1.1 服务端均可）：

```bash
mosquitto -p 1883 &
./uic -config examples/mqtt.json &
mosquitto_sub -h 127.0.0.1 -t 'uic/mqtt-lab/#' -v
#   uic/mqtt-lab/$birth                       {"schemaVersion":1,"online":true,...}
#   uic/mqtt-lab/line-a/temperature/value     {"schemaVersion":1,"device":"line-a",...}
#   uic/mqtt-lab/line-a/$state                {"schemaVersion":1,"state":"collecting",...}
```

Broker 断开时数据进本地 Outbox（不丢），恢复后自动补投；`$state`/`$birth` 是
retained 的，消费方连上即可读到当前状态。

MQTT 南向（订阅别人发来的数据，方向相反）：

```bash
./uic sim -protocol mqtt -listen 127.0.0.1:18830 &   # 内置 Broker，每秒发布演示点位
./uic -config examples/mqtt-southbound.json          # REST :8087
curl -s -H 'Authorization: Bearer dev-token' localhost:8087/api/v1/points
#   6 点：纯文本标量 temp / counter，JSON 字段 running / mode / cycle，可写 setpoint
#   写穿透：POST .../points/edge01/write  → 发布到 plant/line1/setpoint
```

MQTT 是推送型协议，而驱动契约是拉取型，所以驱动在连接时订阅、把消息存进本地
最新值表，`Read` 只查表。由此两条语义必须记住：**质量码只反映消息新鲜度**
（超时 → stale 但保留最后值），以及**一条消息都没来不等于断链**（只有安静超过
一个新鲜度窗口且 PING 往返失败才判断链）。写入成功也只代表「已发出」，
结果质量码是 `uncertain`，回读一致才转 good。

## 文档

- **[docs/00-README.md](docs/00-README.md)** — 总索引：阅读路径、38 个参考方案总览、结论速查、代码地图、接口速查、实现边界
- `docs/01-project-analysis/` — 38 份逐方案六章分析（PA-01 … PA-38）
- `docs/02-synthesis/` — 能力矩阵 / 架构原型 / 缺口分析 / 协议目录
- `docs/03-product/` — PRD / 架构 / 领域模型 / 驱动 SDK / 北向 API / NFR / 路线图
- `docs/04-decisions/` — ADR（语言选型）

## 开发

```bash
go build ./... && go vet ./... && gofmt -l internal cmd
go test -race -count=1 ./...
```

零第三方依赖、零 CGO，仅标准库。

## 目录

| 路径 | 内容 |
|---|---|
| `cmd/uic/` | 单入口：`run` / `sim`（Modbus / S7 / OPC UA / MQTT）/ `mbread` / `-check` / `version` |
| `internal/model` | 通道 / 设备 / 点位 / 地址 / 采样 / 质量码 |
| `internal/driver` | 驱动契约（Codec / Transport / Driver）、错误分类、注册表 |
| `internal/codec/modbus` | Modbus 纯函数编解码（主站与从站共用） |
| `internal/codec/value` | 协议无关值编解码（重排 / 数值 / 字符串 / BCD） |
| `internal/codec/s7` | S7 纯函数编解码（TPKT / COTP / S7 PDU） |
| `internal/codec/opcua` | OPC UA 纯函数编解码（UACP、安全报文、NodeId、Variant、九类服务） |
| `internal/driver/modbus` | Modbus TCP + RTU + ASCII 主站 |
| `internal/driver/s7` | S7（S7comm）主站 |
| `internal/driver/opcua` | OPC UA 主站（None/None + 匿名，三层状态机） |
| `internal/driver/mqtt` | MQTT 南向（订阅取数）：推送适配成拉取，三档新鲜度质量码 |
| `internal/driver/virtual` | 虚拟设备（波形 + 故障注入） |
| `internal/sim/modbus` | 内置 Modbus TCP 从站 |
| `internal/sim/s7` | 内置 S7 从站（内存 DB/M/I/Q 区） |
| `internal/sim/opcua` | 内置 OPC UA 服务端（内存地址空间） |
| `internal/sim/mqtt` | 内置 MQTT 3.1.1 Broker（兼 `uic sim -protocol mqtt` 运行时） |
| `internal/slave/modbus` | Modbus TCP 从站（把点位映射给外部主站，写穿透到南向） |
| `internal/planner` | 点位 → 批量读请求 |
| `internal/scheduler` | tick / actor / 信号量 / 退避 / 熔断 / 代次隔离 |
| `internal/pipeline` | 缩放 / 质量 / 死区 |
| `internal/cache` | 当前值唯一事实源 + 变更订阅 |
| `internal/outbox` | 持久上报队列（分区 / WAL / 确认位点 / 三重上限） |
| `internal/config` | 配置校验 / 内容哈希版本 / 快照 / 回滚 |
| `internal/obs` | 指标 / 结构化日志 / 日志环形缓冲 |
| `internal/northbound/httpapi` | REST + SSE + 认证 + 错误码 |
| `internal/mqtt` | 自写 MQTT 3.1.1 客户端（北向发布 + 南向订阅共用；QoS0/1、retained、LWT、PING 往返探测） |
| `internal/mqtt/mqtttest` | 测试封装：`internal/sim/mqtt` 的 `*testing.T` 生命周期 + 等待断言 |
| `internal/runtime` | 唯一编排层 + 看门狗 + 后端实现（含各 sink 装配） |

## 许可

待定（`docs/04-decisions/` 补充）。
