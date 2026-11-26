// Package contract 定义北向数据契约的版本号与编解码。
//
// 为什么独立成包：契约（一条 Sample 在 v1 / v2 下各长什么样、v2 lane 在
// outbox 里叫什么）是「北向消费者 ↔ 本平台」之间的接口。把它从 runtime 里
// 抽成一个只依赖 model 的纯包，让「切换契约版本不需要重启采集」这件事里
// 最确定的那一半——两种载荷的字节形态、lane 命名规则——可以被单元测试钉死，
// 而不是散落在各 sink 的 marshal 调用里。runtime 只负责「何时发哪一版」，
// 本包不关心传输（MQTT / Webhook / Journal 共用同一份契约定义）。
package contract

import (
	"encoding/json"
	"strings"
	"time"

	"uic/internal/model"
)

// 契约版本。
const (
	// Version1 是既有契约：一条 Sample 平铺在顶层并带 schemaVersion。
	// 它是默认值，也是双发过渡期里始终保留的那一条，保证老消费方零改动。
	Version1 = 1
	// Version2 是新契约：Sample 收进 sample 字段，外层带 contractVersion。
	// 字段名用驼峰（deviceId / pointId / deviceTime / collectedAt），
	// 与 v1 的扁平 snake_case 有意不同——这正是「需要一个能被区分的新版本」
	// 的原因：消费方一眼就能从载荷本身认出它属于哪一版。
	Version2 = 2
)

// LaneSuffixV2 是 outbox 里 v2 lane 在 v1 lane 名之后追加的后缀。
//
// 双发过渡期，同一份采集数据同时进入两条 outbox 分区：`<name>`（v1）与
// `<name>#v2`（v2）。两条 lane 各自持有独立的确认位点与重试队列——v2 的
// 消费方坏掉（不回 ack）只拖累 v2 lane 自己的积压，不会把 v1 的位点卡住，
// 反之亦然。IsV2Lane 用这个后缀区分两条 lane。
const LaneSuffixV2 = "#v2"

// IsV2Lane 判断一个 outbox lane 名是不是 v2 分区。
//
// 形如 "mqtt#v2" → true，"mqtt" → false。后缀锚定在末尾，避免把业务里
// 恰好含 "#v2" 的名字误判（MQTT 主题层级不允许 '#' 出现在层级中段，业务
// lane 名不会带这个后缀）。
func IsV2Lane(name string) bool {
	return strings.HasSuffix(name, LaneSuffixV2)
}

// BaseLane 去掉 v2 后缀，返回该 lane 的 v1 基名。
//
// "mqtt#v2" → "mqtt"，"mqtt" → "mqtt"。用于把 v2 lane 的事件归并回它的
// 源传输（统计、日志）。
func BaseLane(name string) string {
	return strings.TrimSuffix(name, LaneSuffixV2)
}

// eventPayloadV1 是 v1 的消息体：Sample 平铺 + schemaVersion。
// 与 docs/03-product/05-northbound-api-spec.md §6.2 及历史字节完全一致，
// 因此双发过渡期 v1 lane 的产出对老消费方而言没有任何变化。
type eventPayloadV1 struct {
	SchemaVersion int `json:"schemaVersion"`
	model.Sample
}

// EncodeV1 编解码 v1 载荷。返回字节与改造前 publish 里的 marshal 结果一致。
func EncodeV1(s model.Sample) ([]byte, error) {
	return json.Marshal(eventPayloadV1{SchemaVersion: Version1, Sample: s})
}

// sampleV2 是 v2 契约里 sample 字段的结构。
//
// 字段有意逐一点名（而非内嵌 model.Sample）：v2 的对外形态是一份「稳定的
// 外部契约」，应当显式列出，将来 model.Sample 增删内部字段时不会无意中把
// 外部契约也改了——破坏性变更必须走升版本。
type sampleV2 struct {
	DeviceID    string        `json:"deviceId"`
	PointID     string        `json:"pointId"`
	Value       model.Value   `json:"value"`
	Quality     model.Quality `json:"quality"`
	DeviceTime  *time.Time    `json:"deviceTime,omitempty"`
	CollectedAt time.Time     `json:"collectedAt"`
	Seq         uint64        `json:"seq"`
	Unit        string        `json:"unit,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// eventPayloadV2 是 v2 的消息体：外层 contractVersion + 内层 sample。
type eventPayloadV2 struct {
	ContractVersion int      `json:"contractVersion"`
	Sample          sampleV2 `json:"sample"`
}

// EncodeV2 编解码 v2 载荷。
func EncodeV2(s model.Sample) ([]byte, error) {
	return json.Marshal(eventPayloadV2{
		ContractVersion: Version2,
		Sample: sampleV2{
			DeviceID:    s.DeviceID,
			PointID:     s.PointID,
			Value:       s.Value,
			Quality:     s.Quality,
			DeviceTime:  s.DeviceTime,
			CollectedAt: s.CollectedAt,
			Seq:         s.Seq,
			Unit:        s.Unit,
			Error:       s.Error,
		},
	})
}

// Batch 把一批 Sample 编成指定版本的载荷。任一失败即返回错误，不返回半批。
//
// version 取 Version1 / Version2；其他值按 v1 处理（v1 是兜底默认）。
func Batch(version int, items []model.Sample) ([][]byte, error) {
	out := make([][]byte, len(items))
	for i, s := range items {
		var (
			b   []byte
			err error
		)
		if version == Version2 {
			b, err = EncodeV2(s)
		} else {
			b, err = EncodeV1(s)
		}
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}
