package runtime

import (
	"context"
	"errors"
	"net/http"
	"time"

	"uic/internal/alarm"
	"uic/internal/config"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
)

// openAlarms 按配置创建报警引擎。
//
// 引擎在 build 阶段（不启动调度）就存在，因此 -check 之后、Start 之前
// 也能通过 API 看到全部规则——「配置里有 3 条报警、API 里一条都看不到」
// 会让运维以为规则没被加载。
func (r *Runtime) openAlarms(f *config.File) error {
	e, err := alarm.New(alarm.Options{
		Rules: f.Alarms,
		// 只注入只读的 cache.Get：报警引擎拿不到写入与订阅能力，
		// 因此不可能绕过采集层的去重、死序与代次治理。
		Lookup: r.cache.Get,
		// 窗口聚合读历史；与 Lookup 一样是只读入口。历史未开启时
		// 适配器返回 false，带窗口的规则评估为 unknown。
		Window: r.calcWindow,
		Log:    r.log,
	})
	if err != nil {
		return httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid_alarm", "%v", err)
	}
	r.alarms = e
	return nil
}

// alarmSampleLoop 把每个采样喂给报警引擎。
//
// 这里用缓存事件而不是在采集器里直连引擎：报警可能依赖跨设备点位，
// 一台设备的采集器不该知道另一台的值，否则规则就只能在设备内部成立。
func (r *Runtime) alarmSampleLoop(ctx context.Context) {
	defer r.wg.Done()
	ch, cancel := r.cache.Subscribe(1024, nil)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			r.alarms.OnSample(ev.Sample)
		}
	}
}

// alarmEventLoop 消费状态迁移：更新指标、转发给 MQTT、写审计日志。
//
// 与采样循环分开：状态迁移是低频事件，而指标和网络发布都可能变慢，
// 放在采样路径上会拖住采集。
func (r *Runtime) alarmEventLoop(ctx context.Context) {
	defer r.wg.Done()
	ch, cancel := r.alarms.Subscribe(256)
	defer cancel()
	r.refreshAlarmMetrics()
	// 「输入变坏」不产生状态迁移，因此不会走到下面的事件分支；没有这个
	// 定时刷新，uic_alarm_unknown 会永远停在 0——正好和它要暴露的问题相反。
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.refreshAlarmMetrics()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			r.metrics.Inc("uic_alarm_events_total", obs.Labels{
				"type": string(ev.Type), "severity": string(ev.Status.Severity),
			})
			switch ev.Type {
			case alarm.EventRaised:
				r.log.Warn("alarm_raised",
					"alarm", ev.Status.ID, "severity", ev.Status.Severity,
					"value", ev.Status.Value, "quality", ev.Status.Quality)
			case alarm.EventCleared:
				r.log.Info("alarm_cleared", "alarm", ev.Status.ID, "severity", ev.Status.Severity)
			case alarm.EventAcknowledged:
				r.log.Info("alarm_acknowledged", "alarm", ev.Status.ID, "by", ev.Status.AckBy)
			case alarm.EventReset:
				r.log.Warn("alarm_reset", "alarm", ev.Status.ID, "reason", "rule changed")
			case alarm.EventRemoved:
				r.log.Info("alarm_removed", "alarm", ev.Status.ID)
			}
			r.refreshAlarmMetrics()
			if r.mqttSink != nil {
				r.mqttSink.onAlarmEvent(ev)
			}
		}
	}
}

// refreshAlarmMetrics 把「当前事实」投影成 gauge。
//
// 事件计数是增量，而 active/pending/unknown 是瞬时值：只靠事件累加会在
// 重启、规则变更、丢事件之后永久漂移。
func (r *Runtime) refreshAlarmMetrics() {
	if r.alarms == nil {
		return
	}
	c := r.alarms.Counts()
	for _, sev := range []alarm.Severity{alarm.SeverityInfo, alarm.SeverityWarning, alarm.SeverityCritical} {
		n := 0
		for _, st := range r.alarms.List(alarm.Filter{States: []alarm.State{alarm.StateActive, alarm.StateAcked}}) {
			if st.Severity == sev {
				n++
			}
		}
		r.metrics.Set("uic_alarm_active", float64(n), obs.Labels{"severity": string(sev)})
	}
	r.metrics.Set("uic_alarm_pending", float64(c.Pending), nil)
	r.metrics.Set("uic_alarm_cleared", float64(c.Cleared), nil)
	r.metrics.Set("uic_alarm_unknown", float64(c.Unknown), nil)
	r.metrics.Set("uic_alarm_rules", float64(c.Total), nil)
}

// seedAlarmSink 把当前全部报警状态交给 MQTT sink，供（重）连后补发 retained。
func (r *Runtime) seedAlarmSink() {
	if r.mqttSink == nil || r.alarms == nil {
		return
	}
	r.mqttSink.seedAlarms(r.alarms.List(alarm.Filter{}))
}

// ---------------------------------------------------------------------------
// Backend: 报警
// ---------------------------------------------------------------------------

// Alarms 返回满足条件的报警。
func (r *Runtime) Alarms(f alarm.Filter) []alarm.Status {
	if r.alarms == nil {
		return nil
	}
	return r.alarms.List(f)
}

// Alarm 返回单条报警。
func (r *Runtime) Alarm(id string) (alarm.Status, bool) {
	if r.alarms == nil {
		return alarm.Status{}, false
	}
	return r.alarms.Get(id)
}

// AlarmCounts 返回状态汇总（health 与诊断包使用）。
func (r *Runtime) AlarmCounts() alarm.Counts {
	if r.alarms == nil {
		return alarm.Counts{}
	}
	return r.alarms.Counts()
}

// AckAlarms 确认报警。all=true 时确认全部可确认项。
//
// 错误映射刻意区分 404 与 409：ID 不存在是调用方拿错了，而「这条规则现在
// 没有报警」是状态过期——重试没有意义，但也不该被当成服务器故障。
// ackAlarms 是确认的实际实现。导出的 AckAlarms 在它外面包一层审计。
func (r *Runtime) ackAlarms(_ context.Context, ids []string, all bool, by string) ([]alarm.Status, error) {
	if r.alarms == nil {
		return nil, httpapi.Errf(http.StatusNotFound, "alarm.not_found", "no alarms are configured")
	}
	if all {
		return r.alarms.AckAll(by), nil
	}
	if len(ids) == 0 {
		return nil, httpapi.Errf(http.StatusBadRequest, "request.invalid", "ids must not be empty")
	}
	out, err := r.alarms.AckMany(ids, by)
	if err != nil {
		return nil, mapAlarmError(err)
	}
	return out, nil
}

func mapAlarmError(err error) error {
	var code string
	var status int
	switch {
	case errors.Is(err, alarm.ErrNotFound):
		status, code = http.StatusNotFound, "alarm.not_found"
	case errors.Is(err, alarm.ErrNotActive):
		status, code = http.StatusConflict, "alarm.not_active"
	default:
		status, code = http.StatusInternalServerError, "internal.error"
	}
	return httpapi.Errf(status, code, "%v", err)
}

// AlarmStream 订阅报警状态迁移（SSE /api/v1/alarms/stream）。
func (r *Runtime) AlarmStream(buffer int) (<-chan alarm.Event, func()) {
	if r.alarms == nil {
		// 没有配置报警时给一个永不发事件的通道，而不是 nil：
		// select 里 nil 通道永远阻塞，会让 SSE 只剩心跳，看起来像卡死。
		return make(chan alarm.Event), func() {}
	}
	return r.alarms.Subscribe(buffer)
}
