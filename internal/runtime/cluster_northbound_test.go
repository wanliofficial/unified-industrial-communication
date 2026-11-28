package runtime

import (
	"strings"
	"testing"
	"time"

	"uic/internal/model"
	"uic/internal/mqtt/mqtttest"
)

// TestClusterFollowerDoesNotPublishJsonMQTT 覆盖多实例集群下站点级 json MQTT
// 控制面只能由 leader 发布。
//
// 为什么这是缺陷：json 北向的 `uic/{site}/$birth`、`uic/{site}/{device}/$state`、
// `uic/{site}/$death` 都是**站点级**主题（不含实例身份）。同站点两个实例同时
// 发布，retained 值会互相覆盖：follower 启动时把每台设备写成 `$state=disabled`，
// 看板就会显示「设备被停用」而实际 leader 正在采集；follower 正常停机补发的
// `$death` 更会让消费方以为**整个网关**离线，而 leader 还活着。ADR-0005 §4.4
// 已规定非 leader 不采集、不写 Outbox，这条直接发布路径此前漏掉了同一道闸。
//
// 期望语义：
//   - follower：不连 Broker、不发布任何消息（含 `$birth`/`$state`）。
//   - 升主：补发 `$birth` 与设备 `$state`，随后正常采集。
//   - 降主：停止发布且**不发 `$death`**（另一个实例仍在服务）。
func TestClusterFollowerDoesNotPublishJsonMQTT(t *testing.T) {
	b := mqtttest.New(t, nil)
	h, fc := newClusterHarness(t, false, withMQTT(b.URL(), nil))

	waitCond := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", what)
	}

	// follower 角色生效后，点位被显式标成 standby（本地可见性，非北向发布）。
	waitCond("follower standby", func() bool {
		s, ok := h.rt.Point("sim01", "reg0")
		return ok && s.Quality == model.QualityDisabled
	})
	// 给连接与发布留一个稳定窗口：follower 不该出现任何 MQTT 消息。
	time.Sleep(300 * time.Millisecond)
	if got := b.Messages(); len(got) != 0 {
		t.Fatalf("follower published %d MQTT messages before promotion: %v", len(got), topicsOf(got))
	}

	// 升主：应补发 birth 与设备状态（Publish 内部自动建链）。
	fc.setLeader(true)
	b.WaitMessages(t, 1)
	waitCond("leader birth/state", func() bool {
		var birth, state bool
		for _, m := range b.Messages() {
			switch {
			case strings.HasSuffix(m.Topic, "/$birth"):
				birth = true
			case strings.HasSuffix(m.Topic, "/$state"):
				state = true
			}
		}
		return birth && state
	})

	// 降主：停止发布，且绝不补发 $death。
	fc.setLeader(false)
	waitCond("demoted standby", func() bool {
		s, ok := h.rt.Point("sim01", "reg0")
		return ok && s.Quality == model.QualityDisabled
	})
	time.Sleep(200 * time.Millisecond)
	before := len(b.Messages())
	time.Sleep(300 * time.Millisecond)
	after := b.Messages()
	if len(after) != before {
		t.Fatalf("demoted follower kept publishing: %d -> %d (%v)", before, len(after), topicsOf(after[before:]))
	}
	for _, m := range after {
		if strings.HasSuffix(m.Topic, "/$death") {
			t.Fatalf("demoted follower published $death (would mark the whole site offline): %s", m.Topic)
		}
	}
}
