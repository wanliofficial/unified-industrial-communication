package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/northbound/contract"
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
	// 契约版本是运行期热切的状态：这里种下初值，之后由 reload 跟随配置更新。
	// 即使没有任何 sink（outbox 关闭）也种，保证 publish 读到的值一致。
	r.contractVersion.Store(int64(f.Northbound.EffectiveContractVersion()))

	if f.Northbound.Webhook.Enabled {
		if f.Northbound.Webhook.URL == "" {
			r.log.Warn("webhook_enabled_without_url")
		} else {
			timeout := f.Northbound.Webhook.Timeout.Duration()
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			wh := &webhookSink{
				url:     f.Northbound.Webhook.URL,
				headers: f.Northbound.Webhook.Headers,
				limit:   f.Northbound.Webhook.Batch,
				client:  &http.Client{Timeout: timeout},
			}
			sinks["webhook"] = wh.deliverV1
			// 双发：v2 lane 用 contractVersion 信封，与 v1 共享 client/url 但
			// 各自独立确认。webhook 没有「主题」，差异全在载荷信封上。
			sinks["webhook"+contract.LaneSuffixV2] = wh.deliverV2
		}
	}
	if f.Northbound.MQTT.Enabled {
		var sink northboundSink
		var mjson *mqttSink
		var err error
		// 默认 clientID/nodeId 带上实例身份：同站点多实例部署时，若都退回
		// 「uic-<site>」会连同一 Broker 会话互相顶掉（静默断流）。用户显式
		// 配了 clientID/nodeId 时以其为准，只有默认值才派生自实例身份。
		mq := f.Northbound.MQTT
		switch mq.Format {
		case config.MQTTFormatSparkplugB:
			// nodeId 的默认值在 config.Normalize 里已补成「uic-<site>」。这里
			// 识别这个裸默认并追加实例短码：nodeId 是 Sparkplug 的节点身份，
			// 同站点多实例用同一 nodeId 会既撞节点身份又撞 Broker 会话。
			// 用户显式配了别的 nodeId 时（不等于裸默认）保持原样不动。
			//
			// clientId 不在这里补：Sparkplug sink 的契约是「clientId 缺省取
			// nodeId」。提前把 clientId 填成站点派生值会让它丢掉 nodeId 语义，
			// 并覆盖用户显式配的 nodeId 派生结果。只有用户显式配了 clientId
			// 才保留，否则留空交给 sink。
			if mq.NodeID == "uic-"+f.Site {
				mq.NodeID = r.defaultNodeID(f.Site)
			}
			sink, err = newSparkplugSink(mq)
		default:
			if mq.ClientID == "" {
				mq.ClientID = r.defaultClientID(f.Site)
			}
			mjson, err = newMQTTSink(mq, f.Site, mq.ClientID)
			sink = mjson
		}
		if err != nil {
			return fmt.Errorf("runtime: mqtt: %w", err)
		}
		sink.setMetrics(r.metrics)
		r.mqttSink = sink
		sinks["mqtt"] = sink.deliver
		// json 双发：为 v2 lane 再注册一条，用 v2 主题模板。Sparkplug 自带
		// 契约、不在这里开 v2 lane。两条 lane 共享同一 Broker 连接与凭据，
		// 但各自独立确认——v2 消费方拖慢只拖累 mqtt#v2 的积压。
		if mjson != nil {
			sinks["mqtt"+contract.LaneSuffixV2] = mjson.deliverV2
		}
	}
	if f.Storage.Journal {
		path := filepath.Join(r.dataDir, "journal.ndjson")
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("runtime: journal: %w", err)
		}
		r.journalSink = &journalSink{f: fh}
		r.journal = fh
		// 两条 lane 写同一个文件：v1 行带 schemaVersion、v2 行带 contractVersion，
		// 读方按版本字段过滤。deliver 内部对同一 *os.File 加锁串行化，两个
		// dispatchLoop 并发写也不会让一行的字节插到另一行中间。
		sinks["journal"] = r.journalSink.deliver
		sinks["journal"+contract.LaneSuffixV2] = r.journalSink.deliver
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

// defaultClientID 返回 JSON 格式 MQTT sink 的默认 clientID。
//
// 带上实例身份短码：同站点多实例若都用「uic-<site>」会连同一 Broker 会话
// 互相顶掉（后到的把前一个踢下线，静默断流）。用户显式配了 ClientID 时不会
// 走到这里（openSinks 只在 mq.ClientID=="" 时调用）。
func (r *Runtime) defaultClientID(site string) string {
	return "uic-" + site + "-" + r.instanceShort()
}

// defaultNodeID 返回 Sparkplug 的默认 nodeId，同样带上实例身份短码。
func (r *Runtime) defaultNodeID(site string) string {
	return "uic-" + site + "-" + r.instanceShort()
}

// instanceShort 返回实例身份的短码，用于拼进 clientID/nodeId。
//
// 实例身份形如「inst-<12 hex>」，这里取「inst-」后的 hex 段全量（12 字符，
// 48 bit，碰撞概率可忽略）作为短码；身份缺失时返回空串，退化回旧的
// 「uic-<site>」默认（不阻断启动，只失去多实例默认不冲突这层兜底）。
func (r *Runtime) instanceShort() string {
	const prefix = "inst-"
	if len(r.instanceID) > len(prefix) && r.instanceID[:len(prefix)] == prefix {
		return r.instanceID[len(prefix):]
	}
	return ""
}

// publish 把一次变化事件写入所有已启用的 sink。
//
// 双发过渡期（contractVersion==2）：同一条采样同时编成 v1 与 v2 两份载荷，
// 分别入 v1 lane 与 v2 lane。两条 lane 有各自独立的 outbox 分区与确认位点，
// 互不影响。contractVersion 是原子值：运行期由 reload 热切，切换只改变
// 「要不要往 v2 lane 入队」，不重建协程、不重启采集。
func (r *Runtime) publish(s model.Sample) {
	if r.box == nil || len(r.sinkNames) == 0 {
		return
	}
	v1, err := contract.EncodeV1(s)
	if err != nil {
		r.log.Error("outbox_marshal_failed", "version", contract.Version1, "device", s.DeviceID, "point", s.PointID, "error", errText(err))
		return
	}
	dual := r.contractVersion.Load() >= int64(contract.Version2)
	var v2 []byte
	if dual {
		v2, err = contract.EncodeV2(s)
		if err != nil {
			r.log.Error("outbox_marshal_failed", "version", contract.Version2, "device", s.DeviceID, "point", s.PointID, "error", errText(err))
			return
		}
	}
	now := time.Now()
	for _, sink := range r.sinkNames {
		if contract.IsV2Lane(sink) {
			// v1-only 模式：v2 lane 空转（不占位点、不重试）。切到 v2 时
			// 下一条采样就会开始入队，不需要重建 lane。
			if !dual {
				continue
			}
			if _, err := r.box.Enqueue(sink, s.Key(), v2, now); err != nil {
				r.log.Error("outbox_enqueue_failed", "sink", sink, "error", errText(err))
			}
			continue
		}
		if _, err := r.box.Enqueue(sink, s.Key(), v1, now); err != nil {
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
//
// 双发过渡期会有两个 dispatchLoop（journal 与 journal#v2）并发写同一个
// *os.File。mu 把「整批拼好 + 一次 Write」串行化，避免两批的字节插花。
type journalSink struct {
	mu sync.Mutex
	f  *os.File
}

func (j *journalSink) deliver(_ context.Context, recs []outbox.Record) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
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
	j.mu.Lock()
	defer j.mu.Unlock()
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
//
// 双发过渡期：v1 lane 与 v2 lane 共用同一个 url/headers/client，但各自拼
// 不同的信封（schemaVersion 1 vs contractVersion 2）。两条 lane 独立确认。
type webhookSink struct {
	url     string
	headers map[string]string
	client  *http.Client
	limit   int
}

// deliverV1 发送 v1 信封：{"schemaVersion":1,"items":[...]}。
func (w *webhookSink) deliverV1(ctx context.Context, recs []outbox.Record) (int, error) {
	return w.postItems(ctx, recs, `{"schemaVersion":1,"items":[`)
}

// deliverV2 发送 v2 信封：{"contractVersion":2,"items":[...]}。
func (w *webhookSink) deliverV2(ctx context.Context, recs []outbox.Record) (int, error) {
	return w.postItems(ctx, recs, `{"contractVersion":2,"items":[`)
}

func (w *webhookSink) postItems(ctx context.Context, recs []outbox.Record, open string) (int, error) {
	if w.limit > 0 && len(recs) > w.limit {
		recs = recs[:w.limit]
	}
	var body bytes.Buffer
	body.WriteString(open)
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
