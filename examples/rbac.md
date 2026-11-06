# RBAC + 审计示例

这个示例演示三件事：**三个角色能做什么、凭据怎么生成、审计记录长什么样**。

## 1. 生成真实凭据

`rbac.json` 里已经内置了一组**仅用于本地演示**的凭据（明文见下表，仓库里只放
哈希）。它们能直接跑通下面的 curl；任何真实部署都必须换成自己生成的：

| 用户 | 角色 | 演示用明文 Token |
|---|---|---|
| `viewer` | viewer | `2H6oqt_iDzmtpbKS-ZcKB1WgCRKFTeMV` |
| `ops` | operator | `Bnmcwphg0lxA1XTng-SuZJyfXqvYU8bR` |
| `root` | admin | `KilpNTTr9xyEEaJi3uaJucEuNvobBtSH` |

生成自己的凭据：

```bash
./uic token -name viewer -role viewer
./uic token -name ops    -role operator
./uic token -name root   -role admin
```

`uic token` 会同时打印明文 Token（给客户端）与 `tokenSha256`（给配置）。
明文只显示一次；配置里只放哈希，泄露配置不等于泄露凭据。

想一次生成多条、并直接拿到可粘贴的 JSON 数组：

```bash
./uic token -name ops -role operator -count 2 -json
```

## 2. 运行

```bash
./uic -config examples/rbac.json
#   API   http://127.0.0.1:8093/api/v1
#   认证  bearer（3 个用户）
#   审计  开启（0 条，seq=0）
```

## 3. 角色差异

```bash
TOKEN=Bnmcwphg0lxA1XTng-SuZJyfXqvYU8bR   # ops

curl -s -H "Authorization: Bearer $TOKEN" localhost:8093/api/v1/points        # 200
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
     localhost:8093/api/v1/points/press/write \
     -d '{"writes":[{"point":"setpoint","value":{"type":"float32","num":42}}]}'  # 200
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
     localhost:8093/api/v1/commands \
     -d '{"action":"outbox.drop","sink":"journal","confirm":true}'             # 403
curl -s -H "Authorization: Bearer $TOKEN" localhost:8093/api/v1/audit          # 403
```

viewer 连写入也会被拒（`403 auth.forbidden`），但读点位、读报警、看 SSE 都正常。
admin 能读审计、改配置、执行高危命令。

## 4. 审计

```bash
ROOT=KilpNTTr9xyEEaJi3uaJucEuNvobBtSH   # root

curl -s -H "Authorization: Bearer $ROOT" 'localhost:8093/api/v1/audit?limit=20' | jq
curl -s -H "Authorization: Bearer $ROOT" 'localhost:8093/api/v1/audit?result=denied' | jq
curl -s -H "Authorization: Bearer $ROOT" localhost:8093/api/v1/audit/verify | jq
curl -s -H "Authorization: Bearer $ROOT" localhost:8093/api/v1/audit/stats | jq
```

一条写入记录（节选）：

```json
{
  "seq": 12,
  "time": "2026-11-06T04:30:00.123Z",
  "user": "ops",
  "role": "operator",
  "action": "point.write",
  "target": "press/setpoint",
  "result": "ok",
  "detail": { "device": "press", "point": "setpoint", "oldValue": "40", "value": "42", "via": "api" }
}
```

一次越权尝试（节选）：

```json
{
  "seq": 13,
  "user": "ops",
  "role": "operator",
  "action": "auth.forbidden",
  "target": "command.danger",
  "result": "denied",
  "code": "auth.forbidden",
  "message": "action outbox.drop requires command.danger"
}
```

审计文件在 `./data/rbac/audit/*.ndjson`，一行一条，带 `prev`/`hash` 哈希链。
`/audit/verify` 会指出第一条不一致的记录——手工改过文件一定能看出来。

## 5. 开放模式（仅开发）

删掉 `server.users` 且不设 `server.token`，启动后横幅会写：

```
认证  open（未配置凭据，任何能连上端口的人都是 admin）
```

同时日志里有 `auth_open_mode` WARN。这不是缺陷，而是刻意的选择：与其生成一个
永远不会被改的默认口令，不如把「没有认证」明确喊出来。生产环境不要这样跑。
