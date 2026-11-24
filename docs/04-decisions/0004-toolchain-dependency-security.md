# ADR-0004｜工具链与依赖安全：零第三方依赖 + 暂不升级 Go 工具链堵一个不可达的 stdlib advisory

- **状态**：已决定（Accepted）
- **决策日期**：静态安全收尾时
- **决策者**：架构组
- **影响范围**：`go.mod`（`go` directive 固定 1.26.10）、构建/CI 环境的 `GOSUMDB`、`internal/northbound/httpapi`（`MaxBytesReader` 两处使用点）
- **相关文档**：`README.md` §开发（`go build && go vet && gofmt -l` + `go test -race -count=1 ./...`）、ADR-0001（语言选型：零第三方依赖、零 CGO）

---

## 1. 决策摘要

三句话：

1. **依赖面保持零第三方依赖：不引入任何第三方库，连 `go.sum` / `vendor/` 都不生成。**
2. **用 OSV 直查 Go vuln DB 做工具链漏洞扫描，全仓唯一命中一条 stdlib advisory（`GO-2026-6859`），经代码核实在本仓不可达。**
3. **暂不升级 `go` directive 去堵这条 advisory——在当前构建环境下它会直接破坏构建，是净负收益；改为记录不可达结论 + 明确的 revisit 触发器。**

理由压成一句：

> 为一个「当前代码路径触不到」的 advisory，去改一个会让默认环境 `go build ./...` 直接失败的 `go`  directive，是把一个不可见的风险换成了一个确定的、人人都会撞上的构建破坏。正确做法是把可达性核实清楚、写下来、给出触发条件，而不是用一个粗暴的版本 bump 掩盖判断。

---

## 2. 背景：唯一的命中与它的可达性

### 2.1 扫描方法与依赖面事实

本仓 `go.mod` 只有 `module uic` 与 `go 1.26.10` 两行，无 `require` 块；`go list -m all` 只返回 `uic` 自身；无 `go.sum`、无 `vendor/`、源码里零第三方 import、零 CGO。因此依赖漏洞面（`govulncheck` 扫描的对象）**按构造为空**——没有第三方代码进入构建。

剩下唯一的攻击面是**工具链本身**（`go` / `GOTOOLCHAIN` 固定的标准库）。用 OSV 的 `Go` 生态直查 `1.26.10`，全仓只命中一条：

| Advisory | 摘要 | 修复版本 |
|---|---|---|
| `GO-2026-6859` | HTTP/1 server 因 `MaxBytesReader` 头写竞态而 panic | 1.26.11（1.27 线为 1.27.4） |

`GO-2026-6859` 的触发条件（据原文）：HTTP/1 server 使用了 `MaxBytesReader` / `MaxBytesHandler`，且 **handler 在读请求体的同时并发写响应头**——`MaxBytesReader` 超限设置 `Connection: close` 头时未与 handler 显式设置头同步，竞态触发 runtime panic。

### 2.2 可达性核实：本仓不可达

`MaxBytesReader` 在本仓仅两处使用，都在 `internal/northbound/httpapi`：

| 位置 | 函数 | 模式 |
|---|---|---|
| `httpapi.go:812` | `Server.body` | `json.Decoder.Decode(dst)` **同步读完 body** 后才返回，调用方之后才写响应 |
| `httpapi.go:1227` | `Server.readConfig` | `io.ReadAll(r.Body)` **同步读完** 后才返回 |

逐条核实竞态的前提在本仓**全部不成立**：

1. **body 读与响应写不并发**：两处都是「先把 body 读到内存，再走业务、再写响应」的串行形态，`Decode`/`ReadAll` 返回之前没有任何响应头被写。
2. **`httpapi` 包零 goroutine**：`grep` 整个包没有一处 `go func` / `go <method>`，不存在「一个 goroutine 读 body、另一个写头」的并发结构。
3. **dispatch wrapper 串行**：`Server.handle` 就是 `fn(w, r)` 出错再 `s.fail` 写 JSON，单请求内顺序执行，无并发。
4. **SSE 流式端点不读 body**：`alarmsStream` / `stream` 只写事件流、从不碰 `r.Body`，不可能触发该竞态。

**结论**：触发条件（body 读与响应头写并发）在本仓不存在，`GO-2026-6859` **当前不可达**。「当前不可达」不是零风险——它依赖「没有人往 `httpapi` 加并发读 body 的 handler」这条约定，见 §5.3 的触发器。

---

## 3. 关键设计点

### 3.1 升级能堵漏，但在本环境直接破坏构建

把 `go` directive 从 `1.26.10` 升到 `1.26.11`，`GOTOOLCHAIN=auto` 会强制拉取 `go1.26.11` 工具链模块。实测在本环境（`GOSUMDB=off`，写在 `go env` 配置里而非 shell 变量）：

```
$ GOTOOLCHAIN=go1.26.11 go version
go: downloading go1.26.11 ...
go: download go1.26.11: golang.org/toolchain@v0.0.1-go1.26.11.linux-amd64:
  verifying module: checksum database disabled by GOSUMDB=off
```

工具链模块需要校验签名，而 `GOSUMDB=off` 拒绝校验 → **下载失败 → `go build ./...` 直接失败**。临时用 `GOSUMDB=sum.golang.org` 可成功拉取并通过全量 `build` / `vet` / `test -race`（已实测：113 包 0 FAIL），但这是给维护者环境加一条必须手设的开关，不是默认行为。

> 为一个「当前不可达」的 advisory，把「拷一个二进制就能跑」变成「拷二进制 + 手工设 GOSUMDB 才能构建」，是确定的、立即生效的负面，换不确定的、当前为零的安全收益。

### 3.2 用 `go env` 而非改 `go.mod` 来解锁一次性升级

若确实需要拉新工具链（例如本地想验证 1.26.11 兼容性），正确做法是**不改仓库**、只在当次命令前临时 `GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.26.11 go test ...`，工具链会缓存供后续默认环境使用；仓库的 `go.mod` 保持 `1.26.10`，CI / 其他人的默认构建不受影响。

### 3.3 不可达结论必须配一个可执行的触发器

「当前不可达」单独存在是脆的：一次 innocuous 的改动（给某 handler 加 `go func` 流式读 body）就能让它变成可达，而没有任何报错提示。因此把结论落到 §5.3——**给 revisit 触发器与负责人**，让「不可达」在触发条件出现时被重新评估，而不是被遗忘。

---

## 4. 与「默认就升级」的差异

| 维度 | 直接 bump 到 1.26.11 | 本决策（保持 1.26.10 + 记录） |
|---|---|---|
| 默认环境 `go build ./...` | **失败**（GOSUMDB=off 拒绝校验 toolchain 模块） | 通过 |
| `GO-2026-6859` 暴露面 | 关闭（但本已不可达） | 不变（仍不可达） |
| 构建可移植性 | 破坏「拷二进制即跑」 | 保持 |
| 维护者负担 | 需全局设 GOSUMDB 才能构建 | 无新增 |
| 风险可见性 | 版本号升高，给人「已修」的错觉 | 可达性 + 触发器写清楚 |

---

## 5. 后果

### 5.1 正面

- 默认构建保持绿色、零第三方依赖不变（`go.sum` / `vendor` 仍不生成）。
- 工具链漏洞面的唯一命中已被代码级核实为不可达，结论与依据留档，避免后人重复排查。
- 升级路径清晰：要么给构建环境启用 sumdb 后再 bump，要么等触发条件出现时连同新 handler 一起评估。

### 5.2 负面

- `go.mod` 固定在 `1.26.10`，比最新工具链落后一条 advisory；该 advisory 在 `1.26.11` 之前一直「已知但接受」。
- 不可达性依赖一条**约定**（`httpapi` 不并发读 body），约定靠 code review 维持，没有编译期强制。
- 维护者若不知道 §3.2，自己 bump 后会撞上 GOSUMDB 失败并被误导成「仓库坏了」。

### 5.3 退出条件 / revisit 触发器

出现以下任一情况时，应重新评估（优先升级工具链到 ≥1.26.11 再考虑）：

1. **新增任何「在读 `r.Body` 的同时写响应头」的 handler**（例如 `go func` 流式读 body、或把 body 读放进单独 goroutine）——`GO-2026-6859` 的不可达前提被打破，必须先升级；
2. **构建 / CI 环境启用 `GOSUMDB`**（或改为 vendor 工具链）——§3.1 的构建破坏消失，升级成本归零，可直接 bump；
3. **出现影响本仓实际使用的其它 stdlib advisory**（不止 `MaxBytesReader`）——按可达性逐条评估，但会改变「工具链整体够用」的判断；
4. **本仓需要 `1.27` 线特性**——届时连带必须处理一批 advisory，升级成为主线任务。

替换边界已收敛：本决策只动 `go.mod` 的 `go` directive 与构建环境约定，不涉及任何运行时代码；回退（保持 1.26.10）零成本。

---

## 6. 验证

| 断言 | 位置 / 命令 |
|---|---|
| 依赖面为零第三方依赖、无 `go.sum` / `vendor` / CGO | `go list -m all`（仅 uic）、`go.mod` 内容、源码 import 扫描 |
| `MaxBytesReader` 仅两处且均同步读完再写响应 | `internal/northbound/httpapi/httpapi.go:812` / `:1227` |
| `httpapi` 包零 goroutine（无并发读 body 结构） | `grep "go func\|go <method>" internal/northbound/httpapi/`（空） |
| 全仓工具链漏洞仅 `GO-2026-6859` 一条且不可达 | OSV 直查（Go 生态, 1.26.10）+ 上三行可达性核实 |
| bump 到 1.26.11 在默认环境破坏构建 | `GOTOOLCHAIN=go1.26.11 go build ./...`（GOSUMDB=off → 校验失败） |
| 启用 sumdb 时 1.26.11 全量兼容 | `GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.26.11 go test -race -count=1 ./...`（113 包 0 FAIL） |
| 开发仪式当前通过 | `go build ./... && go vet ./... && gofmt -l internal cmd`（空）+ `go test -race -count=1 ./...`（113 包 0 FAIL / 0 DATA RACE） |
