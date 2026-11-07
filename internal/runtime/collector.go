package runtime

import (
	"context"
	"math"
	"sync"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	"uic/internal/obs"
	"uic/internal/pipeline"
	"uic/internal/planner"
	"uic/internal/scheduler"
)

// collector 是一台设备的采集器：持有连接、批读计划、死区处理器与序号。
//
// 生命周期由调度器驱动：调度器保证同一台设备的 Collect 不会并发；
// collector 自己再用 mu 保护「Close / 手工探活 / 写入」这些可能来自
// 其他协程的入口。连接是懒建立的，因此重启设备不需要 runtime 参与。
type collector struct {
	rt     *Runtime
	dev    model.Device
	ch     model.Channel
	drv    driver.Driver
	plan   []driver.ReadSpan
	points map[string]model.Point
	pipe   *pipeline.Processor

	mu      sync.Mutex
	conn    driver.Conn
	openGen uint64
	opened  bool
	seq     uint64
}

func newCollector(rt *Runtime, dev model.Device, ch model.Channel) (*collector, error) {
	drv, ok := driver.Get(ch.Protocol)
	if !ok {
		return nil, httpapi.Errf(422, "config.invalid_protocol", "protocol %q not registered", ch.Protocol)
	}
	if err := checkTransportSupport(drv.Meta(), ch); err != nil {
		return nil, err
	}
	plan, _ := planner.Build(dev, planner.Options{})
	points := make(map[string]model.Point, len(dev.Points))
	for _, p := range dev.Points {
		points[p.ID] = p
	}
	return &collector{
		rt: rt, dev: dev, ch: ch, drv: drv, plan: plan,
		points: points, pipe: pipeline.NewProcessor(),
	}, nil
}

// CheckDriverBindings 校验每条通道声明的协议与传输方式是否匹配。
//
// 供 `uic -check` 与 runtime 装配共用，避免两处规则漂移：CI 里 `-check` 通过
// 而运行时启动失败，是最令人困惑的一类不一致。
func CheckDriverBindings(channels []model.Channel) error {
	for _, ch := range channels {
		drv, ok := driver.Get(ch.Protocol)
		if !ok {
			return httpapi.Errf(422, "config.invalid_protocol",
				"channel %q: protocol %q not registered", ch.ID, ch.Protocol)
		}
		if err := checkTransportSupport(drv.Meta(), ch); err != nil {
			return err
		}
	}
	return nil
}

// checkTransportSupport 校验通道的传输方式是否在该驱动的声明范围内。
//
// 这项校验放在装配期而不是 Open 期，是因为它是**静态配置错误**：把
// modbus-tcp 配到串口上永远不会成功，重试一万次也没用。若留到 Open，
// 现场看到的是设备一直「连接失败」，而不是「配置写错了」。
func checkTransportSupport(m driver.Meta, ch model.Channel) error {
	kind := ch.Transport.Kind
	for _, t := range m.Transports {
		if t == kind {
			return nil
		}
	}
	return httpapi.Errf(422, "config.unsupported_transport",
		"channel %q: protocol %q supports transports %v, got %q",
		ch.ID, ch.Protocol, m.Transports, kind)
}

// fingerprint 决定热更新时该设备是否需要重建。通道参数变化也会导致重建，
// 因为地址/超时/并发都是连接级属性。
func fingerprint(dev model.Device, ch model.Channel) string {
	return hashJSON(struct {
		Device  model.Device  `json:"device"`
		Channel model.Channel `json:"channel"`
	}{dev, ch})
}

func (c *collector) ensure(ctx context.Context, gen uint64) error {
	if c.opened && c.conn != nil && c.openGen == gen {
		return nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.opened = false
	}
	conn, err := c.drv.Open(ctx, driver.OpenConfig{
		Channel: c.ch,
		Device:  c.dev,
		Log:     c.rt.log.With("device", c.dev.ID, "channel", c.ch.ID, "protocol", c.ch.Protocol),
		Metrics: obs.DriverAdapter{M: c.rt.metrics},
		// 只注入只读的 Get：计算点驱动能读到别的点位，但拿不到写入与订阅
		// 能力，因此不可能绕过采集层的治理。
		Lookup: c.rt.cache.Get,
		// 历史窗口聚合（avg(x, 5m) 等）的只读数据源；同样带 TTL 缓存。
		History: c.rt.historyWindow,
	})
	if err != nil {
		return err
	}
	if err := conn.Probe(ctx); err != nil {
		_ = conn.Close()
		return err
	}
	c.conn = conn
	c.openGen = gen
	c.opened = true
	c.rt.metrics.Inc("uic_driver_connect_total", obs.Labels{
		"device": c.dev.ID, "protocol": c.ch.Protocol, "status": "ok",
	})
	return nil
}

// Collect 执行一轮采集：建链 → 批量读 → 缩放/死区 → 写缓存 → 入 Outbox。
func (c *collector) Collect(ctx context.Context, lease *scheduler.Lease) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensure(ctx, lease.Generation); err != nil {
		c.rt.metrics.Inc("uic_driver_connect_total", obs.Labels{
			"device": c.dev.ID, "protocol": c.ch.Protocol, "status": "error",
		})
		c.markUnavailable(lease, err)
		return err
	}

	rctx, cancel := context.WithTimeout(ctx, c.dev.TimeoutOrDefault())
	defer cancel()

	raws, err := c.conn.Read(rctx, c.plan)
	if err != nil {
		// 断链/超时：立刻丢弃连接，下一轮重新建链。
		switch driver.KindOf(err) {
		case driver.ErrDisconnected, driver.ErrTimeout, driver.ErrProtocol:
			_ = c.conn.Close()
			c.conn = nil
			c.opened = false
		}
	}
	if len(c.plan) == 0 {
		return err
	}

	now := time.Now().UTC()
	c.seq++
	seen := make(map[string]bool, len(raws))
	for _, raw := range raws {
		pt, ok := c.points[raw.PointID]
		if !ok {
			continue
		}
		seen[raw.PointID] = true
		c.apply(lease, pt, raw, now)
	}
	// 驱动没有返回的点位不能「保持旧值看起来正常」，必须显式降级。
	for id, pt := range c.points {
		if seen[id] || !pt.Enabled {
			continue
		}
		c.apply(lease, pt, driver.RawSample{
			PointID: id, Quality: model.QualityBad,
			Err: driver.E(driver.ErrProtocol, "read", c.dev.ID, "point missing from response", false, nil),
		}, now)
	}
	return err
}

// Probe 只做连通性检查，不读点位；用于熔断后的探活。
func (c *collector) Probe(ctx context.Context, lease *scheduler.Lease) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(ctx, lease.Generation); err != nil {
		c.markUnavailable(lease, err)
		return err
	}
	if err := c.conn.Probe(ctx); err != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.opened = false
		return err
	}
	return nil
}

// probeNow 供 device.selftest 命令使用（不经过调度器的探活周期）。
func (c *collector) probeNow(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	gen := uint64(0)
	if c.rt.sched != nil {
		if g, ok := c.rt.sched.Generation(c.dev.ID); ok {
			gen = g
		}
	}
	if err := c.ensure(ctx, gen); err != nil {
		return err
	}
	return c.conn.Probe(ctx)
}

func (c *collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.opened = false
	return err
}

func (c *collector) apply(lease *scheduler.Lease, pt model.Point, raw driver.RawSample, at time.Time) {
	if !pt.Enabled {
		raw = driver.RawSample{
			PointID: pt.ID, Quality: model.QualityDisabled,
			Err: driver.E(driver.ErrUnsupported, "read", c.dev.ID, "point disabled", false, nil),
		}
	}
	res := c.pipe.Apply(c.dev, pt, raw, at, c.seq)
	if !lease.Live() {
		return
	}
	c.rt.cache.Set(res.Sample)
	if res.Changed {
		c.rt.publish(res.Sample)
	}
}

// markUnavailable 在建链失败时把全部点位置为不可用，避免 API 里留着上一次的好值。
func (c *collector) markUnavailable(lease *scheduler.Lease, err error) {
	q := qualityOf(err)
	now := time.Now().UTC()
	c.seq++
	for _, pt := range c.points {
		if !pt.Enabled {
			continue
		}
		c.apply(lease, pt, driver.RawSample{PointID: pt.ID, Quality: q, Err: err}, now)
	}
}

func qualityOf(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	case driver.ErrUnsupported, driver.ErrPermission:
		return model.QualityBad
	}
	return model.QualityBad
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

type preparedWrite struct {
	item httpapi.WriteItem
	pt   model.Point
	req  driver.WriteRequest
}

// write 在设备 actor 内执行写入，并对成功项做乐观更新（Uncertain），
// confirm=true 时再回读一次确认。
func (c *collector) write(ctx context.Context, reqs []preparedWrite, confirm bool) []httpapi.WriteResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	at := now
	results := make([]httpapi.WriteResult, len(reqs))
	fail := func(i int, q model.Quality, code, msg string) {
		results[i] = httpapi.WriteResult{
			Point: reqs[i].item.Point, OK: false, Quality: q,
			Code: code, Message: msg,
		}
	}

	gen := uint64(0)
	if c.rt.sched != nil {
		if g, ok := c.rt.sched.Generation(c.dev.ID); ok {
			gen = g
		}
	}
	if err := c.ensure(ctx, gen); err != nil {
		for i := range reqs {
			fail(i, qualityOf(err), "driver.write_failed", err.Error())
		}
		return results
	}

	drivers := make([]driver.WriteRequest, 0, len(reqs))
	index := make(map[string]int, len(reqs))
	for i, p := range reqs {
		drivers = append(drivers, p.req)
		index[p.item.Point] = i
	}

	wrs, err := c.conn.Write(ctx, drivers)
	if err != nil && len(wrs) == 0 {
		for i := range reqs {
			fail(i, model.QualityCommError, "driver.write_failed", err.Error())
		}
		return results
	}

	c.seq++
	for _, wr := range wrs {
		i, ok := index[wr.PointID]
		if !ok {
			continue
		}
		if !wr.OK {
			q := wr.Quality
			if q == "" {
				q = model.QualityBad
			}
			msg := "write rejected"
			if wr.Err != nil {
				msg = wr.Err.Error()
			}
			results[i] = httpapi.WriteResult{
				Point: wr.PointID, OK: false, Quality: q,
				Code: "driver.write_failed", Message: msg,
			}
			continue
		}
		results[i] = httpapi.WriteResult{
			Point: wr.PointID, OK: true, Quality: model.QualityUncertain, AppliedAt: &at,
		}
		// 乐观更新：让 UI/SSE 立刻看到目标值，下一次采集负责确认。
		//
		// 必须用 req.Value 而不是 item.Value：调用方可以省略 value.type，
		// 由运行时按点位 dataType 补齐。item.Value 是补之前的原始请求，
		// 拿它写进缓存会得到一个「没有类型」的采样。
		c.rt.cache.Set(model.Sample{
			DeviceID: c.dev.ID, PointID: wr.PointID,
			Value: reqs[i].req.Value, Quality: model.QualityUncertain,
			CollectedAt: now, Seq: c.seq, Unit: reqs[i].pt.Unit,
		})
	}

	// 写入未知点位 / 驱动少返回：显式失败，不能静默。
	for i := range reqs {
		if results[i].Point == "" {
			fail(i, model.QualityBad, "driver.write_failed", "driver returned no result for point")
		}
	}

	if confirm {
		c.confirm(ctx, reqs, results, now)
	}
	return results
}

func (c *collector) confirm(ctx context.Context, reqs []preparedWrite, results []httpapi.WriteResult, at time.Time) {
	for i := range reqs {
		if !results[i].OK {
			continue
		}
		p := reqs[i]
		span := driver.ReadSpan{
			ID:     "confirm:" + p.item.Point,
			Points: []driver.PointRef{driver.PointRefOf(c.dev, p.pt)},
		}
		rctx, cancel := context.WithTimeout(ctx, c.dev.TimeoutOrDefault())
		raws, err := c.conn.Read(rctx, []driver.ReadSpan{span})
		cancel()
		if err != nil || len(raws) == 0 {
			results[i].Quality = model.QualityUncertain
			results[i].Message = "write applied but read-back failed"
			continue
		}
		got := raws[0]
		if got.Quality.IsUsable() && valuesMatch(got.Value, p.req.Value) {
			results[i].Quality = model.QualityGood
			c.rt.cache.Set(model.Sample{
				DeviceID: c.dev.ID, PointID: p.item.Point, Value: got.Value,
				Quality: model.QualityGood, CollectedAt: at, Seq: c.seq, Unit: p.pt.Unit,
			})
			continue
		}
		results[i].Quality = model.QualityUncertain
		results[i].Message = "read-back value differs from written value"
		c.rt.cache.Set(model.Sample{
			DeviceID: c.dev.ID, PointID: p.item.Point, Value: got.Value,
			Quality: model.QualityUncertain, CollectedAt: at, Seq: c.seq, Unit: p.pt.Unit,
		})
	}
}

// valuesMatch 判断读回值是否等于刚写入的值。
//
// 不能直接用 Value.Equal 做逐位比较，两点原因：
//
//  1. 调用方可以省略 value.type，由运行时按点位 dataType 补齐。要比较的
//     是「补齐后的请求值」，而不是原始 JSON——否则空类型永远不等于读回
//     类型，每次写都报「读回不一致」。
//  2. 设备把浮点存成 32/64 位。写入 0.1 读回 0.10000000149 是正常的
//     二次舍入，逐位相等会把一次成功写入报成 uncertain。
//
// 布尔与字符串保持严格：`true` 读成 `false`、`on` 读成 `on ` 都是真问题，
// 放宽只会把它掩盖成「已写入」。
func valuesMatch(got, want model.Value) bool {
	switch want.Type {
	case model.TypeBool:
		return got.Type == want.Type && got.Bool == want.Bool
	case model.TypeString, model.TypeBytes:
		return got.Type == want.Type && got.Text == want.Text
	}
	if !want.Type.IsNumeric() || !got.Type.IsNumeric() {
		return want.Type == got.Type && got.Equal(want)
	}
	var eps float64
	switch want.Type {
	case model.TypeFloat32:
		// float32 的有效位约 7 位十进制，1e-6 留一位余量。
		eps = 1e-6
	case model.TypeFloat64:
		eps = 1e-12
	case model.TypeBCD16, model.TypeBCD32:
		eps = 1e-6
	default:
		// 整数：必须完全相等。读回 777.0000001 说明设备做了缩放，
		// 那不是这次写入的值。
		return got.Num == want.Num
	}
	scale := math.Abs(want.Num)
	if scale < 1 {
		scale = 1
	}
	return math.Abs(got.Num-want.Num) <= eps*scale
}
