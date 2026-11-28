package i18n

// catalog 是「源语言模板 → 目标语言模板」的映射表。
//
// key 必须与代码里 Errf 的 format 字面量逐字节相同（catalog_test.go 用 go/ast
// 扫全仓校验，任何拼写偏差都会让测试变红，而不是静默回退到英语）。
//
// value 里的占位符必须与 key 完全同序同型：`%q` 换成 `%s` 会让设备 ID 失去引
// 号、`%v` 换成 `%d` 会打印 `%!d(string=...)`。同样由测试校验。
//
// 译文原则：
//   - 保留技术标识原样（`confirm: true`、`RFC3339`、`device/point`），它们是
//     要照抄进代码/URL 的东西，翻译了反而误导；
//   - 保留参数名原样（`ids`、`limit`、`observe`），它们与配置字段一一对应；
//   - 句子以中文标点收尾，但 key 里的 ASCII 冒号保持不变，便于日志里按冒号
//     切分定位。
var catalog = map[Locale]map[string]string{
	ZH: zhCatalog,
}

// zhCatalog 是简体中文文案。
// 纯透传模板（`%v`、`from: %v` 之类只包一层底层错误文本、没有自然语言）故意
// 不收录在此：它们没有可翻译的内容，回退源模板就是正确行为。哪些属于透传由
// catalog_test.go 的 passThrough 显式列出，不允许「悄悄漏掉」。
var zhCatalog = map[string]string{
	"a staged rollout is already in progress (next batch %d/%d); advance or abort it first": "已有灰度发布在进行中（下一批 %d/%d）；请先推进或中止它",
	"action is required": "action 不能为空",
	"agg: %q is not one of last|first|avg|min|max|count": "agg: %q 不在 last|first|avg|min|max|count 之内",
	"alarm %q not found": "报警 %q 不存在",
	"all %d batches already applied; nothing to advance": "全部 %d 批均已下发，没有可推进的批次",
	"all and ids are mutually exclusive":                 "all 与 ids 不能同时指定",
	"at most 100 writes per request":                     "单个请求最多 100 条写入",
	"audit storage is closed":                            "审计存储已关闭",
	"audit storage is not enabled in this configuration": "当前配置未启用审计存储",
	"authentication attempts exhausted; retry later":     "认证尝试次数已用尽；请稍后再试",
	"before must be RFC3339: %v":                         "before 必须是 RFC3339 时间: %v",
	"body ids/all are not allowed on a single-alarm ack": "单条报警确认的请求体不允许携带 ids/all",
	"body point %q does not match path %q":               "请求体中的点位 %q 与路径中的 %q 不一致",
	"by must be <= 64 bytes":                             "by 不得超过 64 字节",
	"cannot parse config: %v":                            "无法解析配置: %v",
	"cannot encode config: %v":                           "无法编码配置: %v",
	"cannot commit to the cluster consensus (%v); the operation may have been applied locally": "无法提交到集群共识（%v）；该操作可能已在本地执行",
	"cluster consensus is shutting down; retry after restart":                                  "集群共识正在关闭；请重启后重试",
	"cluster state machine on this instance is unhealthy; writes are rejected":                 "本实例的集群状态机不健康；已拒绝写入",
	"this instance is not the cluster leader":                                                  "本实例不是集群 leader",
	"channel %q: protocol %q not registered":                                                   "通道 %q: 协议 %q 未注册",
	"channel %q: protocol %q supports transports %v, got %q":                                   "通道 %q: 协议 %q 支持传输方式 %v，实际为 %q",
	"config is required": "config 不能为空",
	"config.rollback changes the running configuration; pass confirm: true": "config.rollback 会改动运行中的配置；请传 confirm: true",
	"decode body: %v":                 "解析请求体失败: %v",
	"device %q is not running":        "设备 %q 未在运行",
	"device %q not found":             "设备 %q 不存在",
	"device %q unavailable: %v":       "设备 %q 不可用: %v",
	"device %q: channel %q not found": "设备 %q: 通道 %q 不存在",
	"device and point are required (e.g. ?device=press-01&point=power)": "device 与 point 为必填（例如 ?device=press-01&point=power）",
	"empty body":                "请求体为空",
	"from must be RFC3339: %v":  "from 必须是 RFC3339 时间: %v",
	"history storage is closed": "历史存储已关闭",
	"history storage is not enabled in this configuration":                  "当前配置未启用历史存储",
	"ids must be a list of non-empty strings":                               "ids 必须由非空字符串组成",
	"ids must be a string or a list of strings":                             "ids 必须是字符串或字符串列表",
	"ids must not be empty":                                                 "ids 不能为空",
	"ids must not be empty unless all is true":                              "除非 all 为 true，否则 ids 不能为空",
	"internal error":                                                        "内部错误",
	"invalid JSON body: %v":                                                 "JSON 请求体非法: %v",
	"invalid device wildcard %q":                                            "设备通配符 %q 非法",
	"invalid point wildcard %q":                                             "点位通配符 %q 非法",
	"invalid points pattern %q (want device/point)":                         "点位匹配串 %q 非法（应为 device/point）",
	"level must be debug/info/warn/error":                                   "level 必须是 debug/info/warn/error",
	"limit must be a non-negative integer":                                  "limit 必须是非负整数",
	"limit must be a positive integer":                                      "limit 必须是正整数",
	"maxFailureRate must be within [0,1], got %v":                           "maxFailureRate 必须落在 [0,1] 区间，实际为 %v",
	"missing or invalid bearer token":                                       "缺少 Bearer Token 或 Token 无效",
	"no alarms are configured":                                              "未配置任何报警",
	"no northbound sink configured":                                         "未配置任何北向接收端",
	"no staged rollout in progress":                                         "当前没有进行中的灰度发布",
	"not found in current config: %s":                                       "当前配置中不存在: %s",
	"observation window still open for %s; refusing to advance batch %d/%d": "%s 的观察窗口尚未结束，拒绝推进第 %d/%d 批",
	"observe must not be negative":                                          "observe 不能为负数",
	"outbox.drop discards data; pass confirm: true to proceed":              "outbox.drop 会丢弃数据；请传 confirm: true 以继续",
	"param %q is required":                                                  "参数 %q 为必填",
	"param %q must be a non-empty string":                                   "参数 %q 必须是非空字符串",
	"point %q not found on device %q":                                       "点位 %q 在设备 %q 上不存在",
	"protocol %q not registered":                                            "协议 %q 未注册",
	"read body: %v":                                                         "读取请求体失败: %v",
	"request canceled":                                                      "请求已取消",
	"request timed out":                                                     "请求超时",
	"role %q cannot run %q (requires %q)":                                   "角色 %q 不能执行 %q（需要 %q）",
	"role %q lacks permission %q":                                           "角色 %q 缺少权限 %q",
	"step must be > 0":                                                      "step 必须大于 0",
	"step: %q is not a duration (try 10s, 1m, 1h)":                          "step: %q 不是合法时长（可写 10s、1m、1h）",
	"streaming not supported":                                               "不支持流式响应",
	"to must be RFC3339: %v":                                                "to 必须是 RFC3339 时间: %v",
	"unknown action %q":                                                     "未知的操作 %q",
	"unknown severity %q (want info|warning|critical)":                      "未知的严重级别 %q（应为 info|warning|critical）",
	"unknown state %q (want normal|pending|active|acked|cleared)":           "未知的状态 %q（应为 normal|pending|active|acked|cleared）",
	"unexpected apply result type %T":                                       "配置应用返回了非预期的结果类型 %T",
	"unexpected rollback result type %T":                                    "回滚返回了非预期的结果类型 %T",
	"version is required":                                                   "version 不能为空",
	"writes must not be empty":                                              "写入列表不能为空",
}
