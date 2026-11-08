package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/obs"
	"uic/internal/outbox"
)

// eventPayload 是进入 Outbox 的北向消息体：Sample 平铺 + schemaVersion。
// 与 docs/03-product/05-northbound-api-spec.md §6.2 一致。
type eventPayload struct {
	SchemaVersion int `json:"schemaVersion"`
	model.Sample
}

type deliverFunc func(ctx context.Context, recs []outbox.Record) (int, error)

// openSinks 依据配置创建北向上报通道。没有通道时 Outbox 完全关闭
// （不写盘、不占内存），这是一个显式选择，而不是静默降级。
func (r *Runtime) openSinks(f *config.File) error {
	sinks := map[string]deliverFunc{}

	if f.Northbound.Webhook.Enabled {
		if f.Northbound.Webhook.URL == "" {
			r.log.Warn("webhook_enabled_without_url")
		} else {
			timeout := f.Northbound.Webhook.Timeout.Duration()
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			sinks["webhook"] = (&webhookSink{
				url:     f.Northbound.Webhook.URL,
				headers: f.Northbound.Webhook.Headers,
				limit:   f.Northbound.Webhook.Batch,
				client:  &http.Client{Timeout: timeout},
			}).deliver
		}
	}
	if f.Northbound.MQTT.Enabled {
		var sink northboundSink
		var err error
		switch f.Northbound.MQTT.Format {
		case config.MQTTFormatSparkplugB:
			sink, err = newSparkplugSink(f.Northbound.MQTT)
		default:
			sink, err = newMQTTSink(f.Northbound.MQTT, f.Site, "uic-"+f.Site)
		}
		if err != nil {
			return fmt.Errorf("runtime: mqtt: %w", err)
		}
		sink.setMetrics(r.metrics)
		r.mqttSink = sink
		sinks["mqtt"] = sink.deliver
	}
	if f.Storage.Journal {
		path := filepath.Join(r.dataDir, "journal.ndjson")
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("runtime: journal: %w", err)
		}
		r.journalSink = &journalSink{f: fh}
		r.journal = fh
		sinks["journal"] = r.journalSink.deliver
	}

	if len(sinks) == 0 {
		r.log.Info("outbox_disabled", "detail", "未配置北向 sink；变化事件只进缓存与 SSE")
		return nil
	}
	box, err := outbox.Open(filepath.Join(r.dataDir, "outbox"), outbox.Options{
		MaxBytes:   f.Storage.OutboxMaxBytes,
		MaxRecords: f.Storage.OutboxMaxRecords,
		TTL:        f.Storage.OutboxTTL.Duration(),
	})
	if err != nil {
		return fmt.Errorf("runtime: outbox: %w", err)
	}
	r.box = box
	r.sinks = sinks
	r.sinkNames = sortedSinks(sinks)
	r.log.Info("outbox_enabled", "sinks", r.sinkNames, "dir", box.Dir())
	return nil
}

// publish 把一次变化事件写入所有已启用的 sink。
func (r *Runtime) publish(s model.Sample) {
	if r.box == nil || len(r.sinkNames) == 0 {
		return
	}
	payload, err := json.Marshal(eventPayload{SchemaVersion: 1, Sample: s})
	if err != nil {
		r.log.Error("outbox_marshal_failed", "device", s.DeviceID, "point", s.PointID, "error", errText(err))
		return
	}
	now := time.Now()
	for _, sink := range r.sinkNames {
		if _, err := r.box.Enqueue(sink, s.Key(), payload, now); err != nil {
			r.log.Error("outbox_enqueue_failed", "sink", sink, "error", errText(err))
		}
	}
	r.wakeUp()
}

func (r *Runtime) wakeUp() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runtime) dispatchLoop(ctx context.Context, sink string, deliver deliverFunc) {
	defer r.wg.Done()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 关闭前尽力排空，但不再无限重试。
			fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			r.drain(fctx, sink, deliver)
			cancel()
			return
		case <-t.C:
		case <-r.wake:
		}
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		r.drain(dctx, sink, deliver)
		cancel()
		r.refreshQueueMetrics()
	}
}

// drain 按批投递：只有整批成功（或前缀成功）才推进确认位点。
func (r *Runtime) drain(ctx context.Context, sink string, deliver deliverFunc) {
	if r.box == nil {
		return
	}
	for i := 0; i < 32; i++ {
		recs, err := r.box.Pending(sink, 64, time.Now())
		if err != nil {
			r.log.Warn("outbox_pending_failed", "sink", sink, "error", errText(err))
			return
		}
		if len(recs) == 0 {
			return
		}
		n, err := deliver(ctx, recs)
		if n > 0 {
			if aerr := r.box.Ack(sink, recs[n-1].Seq); aerr != nil {
				r.log.Warn("outbox_ack_failed", "sink", sink, "error", aerr)
				return
			}
		}
		if err != nil {
			r.log.Warn("outbox_deliver_failed", "sink", sink, "records", len(recs), "error", errText(err))
			return
		}
		if n < len(recs) {
			return
		}
	}
}

func (r *Runtime) refreshQueueMetrics() {
	if r.box == nil {
		return
	}
	r.mu.Lock()
	if r.dropSeen == nil {
		r.dropSeen = map[string]map[string]int64{}
	}
	r.mu.Unlock()

	for _, st := range r.box.Stats() {
		r.metrics.Set("uic_outbox_pending", float64(st.Pending), obs.Labels{"sink": st.Name})
		r.metrics.Set("uic_outbox_pending_bytes", float64(st.PendingBytes), obs.Labels{"sink": st.Name})

		r.mu.Lock()
		prev := r.dropSeen[st.Name]
		if prev == nil {
			prev = map[string]int64{}
			r.dropSeen[st.Name] = prev
		}
		for reason, n := range st.Dropped {
			if delta := n - prev[reason]; delta > 0 {
				r.metrics.Add("uic_outbox_dropped_total", float64(delta),
					obs.Labels{"sink": st.Name, "reason": reason})
			}
			prev[reason] = n
		}
		r.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// 内置 sink
// ---------------------------------------------------------------------------

// journalSink 把每条变化追加为一行 JSON。它是「断网留痕」的最低成本实现，
// 也让 Outbox 在没有 MQTT/Webhook 的开发环境里可被观察。
type journalSink struct {
	f *os.File
}

func (j *journalSink) deliver(_ context.Context, recs []outbox.Record) (int, error) {
	if j.f == nil {
		return 0, fmt.Errorf("journal: closed")
	}
	buf := bytes.NewBuffer(make([]byte, 0, 64*len(recs)))
	for _, rec := range recs {
		buf.Write(rec.Data)
		buf.WriteByte('\n')
	}
	if _, err := j.f.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return len(recs), nil
}

func (j *journalSink) close() error {
	if j.f == nil {
		return nil
	}
	err := j.f.Sync()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	j.f = nil
	return err
}

// webhookSink 把样本批量 POST 到用户端点。
type webhookSink struct {
	url     string
	headers map[string]string
	client  *http.Client
	limit   int
}

func (w *webhookSink) deliver(ctx context.Context, recs []outbox.Record) (int, error) {
	if w.limit > 0 && len(recs) > w.limit {
		recs = recs[:w.limit]
	}
	var body bytes.Buffer
	body.WriteString(`{"schemaVersion":1,"items":[`)
	for i, rec := range recs {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(rec.Data)
	}
	body.WriteString(`]}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return len(recs), nil
}

// sortedSinks 返回稳定的 sink 顺序，便于日志与断言。
func sortedSinks(m map[string]deliverFunc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
