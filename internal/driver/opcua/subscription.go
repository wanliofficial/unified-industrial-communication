package opcua

import (
	"context"
	"fmt"
	"sync"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/driver"
	"uic/internal/model"
)

// 订阅参数。三个常量放在一起，因为它们的取值互相约束：
// KeepAliveCount 太大会让「断链」在很长时间里表现为「空闲」，
// 太小则空闲时全是保活报文。
const (
	// defaultKeepAliveCount 是「连续多少个发布周期没有通知就发一条保活」。
	// 10 是规范推荐的默认值。
	defaultKeepAliveCount = 10
	// minKeepAliveTimeout 是等待一条 Publish 响应的下限。发布周期 100ms、
	// 保活 10 个周期也只有 1 秒，比任何一次真实网络往返抖动都短；
	// 把它抬到 15 秒，是为了让「网络慢」不要被误判成「订阅死了」。
	minKeepAliveTimeout = 15 * time.Second
	// maxReconnectBackoff 是订阅连接重建的退避上限。
	maxReconnectBackoff = 30 * time.Second
)

// subPoint 是一个被监视的点位。
//
// 一个点位一个监视项（即使两个点位指向同一个 NodeId 也不合并）：合并需要
// 在通知到达时按多个数据类型分别解码，而「同一个节点读成两种类型」本身
// 就是配置异味——真要这么用，代价是多一条通知。
type subPoint struct {
	id     string
	node   ua.NodeID
	dt     model.DataType
	handle uint32
}

// subValue 是最近一次推送的结果。
//
// 存的不是 DataValue 而是解码后的 model.Value：解码只做一次，Read 走
// 热路径时不需要再判断「这个 Variant 能不能塞进 float64」。
type subValue struct {
	val     model.Value
	quality model.Quality
	err     error
	at      time.Time
}

// subState 是一条订阅连接。
//
// 为什么要第二条连接：OPC UA 的 Publish 是长轮询——服务端会把请求压到
// 有通知或保活超时才回复。如果它和 Read/Write 共用一条连接，那么每次
// 发布都要么阻塞读写，要么需要实现「同一连接上多请求并发 + 按 RequestId
// 分派响应」。前者在空闲设备上等于把采集周期拖成保活周期，后者是一套
// 完整的多路复用器。独立连接只要几百字节的额外开销，就换掉了这个复杂度；
// CloseSession(DeleteSubscriptions=true) 保证它不会在服务端泄漏。
type subState struct {
	deviceID  string
	keepAlive uint32

	mu      sync.RWMutex
	conn    *conn
	subID   uint32
	values  map[string]subValue
	healthy bool
	lastAt  time.Time
	lastErr string

	points   []subPoint
	byHandle map[uint32]*subPoint

	open     func(ctx context.Context) (*conn, error)
	metrics  driver.Metrics
	log      func(msg string, args ...any)
	pubEvery time.Duration
	sampling time.Duration

	// done 在发布循环退出时关闭。Close 需要等它：否则「取消 → 循环恰好
	// 刚建立了一条新连接」会让那条连接留着，服务端看到一个永不超时的会话。
	done chan struct{}
}

func newSubState(deviceID string, points []subPoint, pubEvery, sampling time.Duration,
	open func(context.Context) (*conn, error), metrics driver.Metrics,
	log func(string, ...any)) *subState {

	byHandle := make(map[uint32]*subPoint, len(points))
	for i := range points {
		points[i].handle = uint32(i + 1)
		byHandle[points[i].handle] = &points[i]
	}
	return &subState{
		deviceID:  deviceID,
		keepAlive: defaultKeepAliveCount,
		conn:      nil,
		values:    map[string]subValue{},
		points:    points,
		byHandle:  byHandle,
		done:      make(chan struct{}),
		open:      open,
		metrics:   metrics,
		log:       log,
		pubEvery:  pubEvery,
		sampling:  sampling,
	}
}

// establish 建立订阅连接、订阅与全部监视项。
//
// 失败时返回错误让 Open 失败（而不是静默退化成轮询）：配置里写了
// subscribe=true，实际却在轮询，是那种「跑起来一切正常、三个月后有人
// 问为什么数据延迟变了」的问题。要轮询就把这个开关关掉。
func (s *subState) establish(ctx context.Context) error {
	// establish 的语义是「用一条新连接替换当前连接」，因此必须先释放上一条。
	// 重连循环（run）在 publishUntilError 出错后直接进来，而上一条连接并
	// 不保证已经关闭：错误可能来自「订阅状态为 Bad」或一条解不开的通知，
	// 此时 socket 与会话都还活着。不先关掉就覆盖 s.conn，等于每次重连都泄漏
	// 一条 TCP fd 和一个服务端会话——表现是「跑几天后 fd 用尽 / 服务端会话
	// 暴涨」。首次 establish 时 s.conn 为 nil，closeCurrent 是空操作。
	s.closeCurrent()

	c, err := s.open(ctx)
	if err != nil {
		return err
	}

	pubMS := float64(s.pubEvery / time.Millisecond)
	req := ua.CreateSubscriptionRequest{
		Header:                      c.newHeader(),
		RequestedPublishingInterval: pubMS,
		// Lifetime 必须 ≥ 3 × KeepAliveCount，否则服务端会自己改大。
		RequestedLifetimeCount:     3 * s.keepAlive,
		RequestedMaxKeepAliveCount: s.keepAlive,
		MaxNotificationsPerPublish: 0, // 0 = 不限
		PublishingEnabled:          true,
		Priority:                   0,
	}
	if err := c.call(ctx, "create-subscription", ua.SvcCreateSubscriptionRequest, req.Encode(), ua.SvcCreateSubscriptionResponse,
		func(resp ua.SecureMessage) error {
			r, derr := ua.DecodeCreateSubscriptionResponse(resp.Body)
			if derr != nil {
				return derr
			}
			if r.Header.ServiceResult.IsBad() {
				return r.Header.ServiceResult
			}
			s.subID = r.SubscriptionID
			// 用修订后的发布周期算保活超时：服务端有权把 100ms 改成 500ms，
			// 拿请求值去算会得到一个偏小的超时，然后把健康的订阅判死。
			if r.RevisedPublishingInterval > 0 {
				s.pubEvery = time.Duration(r.RevisedPublishingInterval) * time.Millisecond
			}
			if r.RevisedMaxKeepAliveCount > 0 {
				s.keepAlive = r.RevisedMaxKeepAliveCount
			}
			return nil
		}); err != nil {
		_ = c.Close()
		return driver.E(statusKindOf(err), "subscribe", s.deviceID, err.Error(), false, err)
	}

	items := make([]ua.MonitoredItemCreateRequest, 0, len(s.points))
	for _, p := range s.points {
		items = append(items, ua.MonitoredItemCreateRequest{
			ItemToMonitor:  ua.ReadValueID{NodeID: p.node, AttributeID: ua.AttrValue},
			MonitoringMode: ua.MonitoringModeReporting,
			RequestedParameters: ua.MonitoringParameters{
				ClientHandle: p.handle,
				// 采样周期默认等于发布周期；通道上显式配了就用配置值。
				SamplingInterval: float64(s.sampling / time.Millisecond),
				// 1 + 丢旧值：网关只要「现在是多少」，排队反而让 Read
				// 读到几秒前的值。
				QueueSize:     1,
				DiscardOldest: true,
			},
		})
	}
	mreq := ua.CreateMonitoredItemsRequest{
		Header:             c.newHeader(),
		SubscriptionID:     s.subID,
		TimestampsToReturn: ua.TimestampsBoth,
		ItemsToCreate:      items,
	}
	if err := c.call(ctx, "create-monitored-items", ua.SvcCreateMonitoredItemsRequest, mreq.Encode(),
		ua.SvcCreateMonitoredItemsResponse, func(resp ua.SecureMessage) error {
			r, derr := ua.DecodeCreateMonitoredItemsResponse(resp.Body)
			if derr != nil {
				return derr
			}
			if r.Header.ServiceResult.IsBad() {
				return r.Header.ServiceResult
			}
			if len(r.Results) != len(s.points) {
				return fmt.Errorf("%w: got %d results for %d items", errProtocol, len(r.Results), len(s.points))
			}
			for i, res := range r.Results {
				if res.StatusCode.IsGood() {
					continue
				}
				// 单个节点被拒（不存在 / 不可读）不是订阅失败：其余点位
				// 仍然能正常推送。把这个错误钉在该点位上，Read 会像轮询
				// 模式那样如实报告「地址错」，而不是让整台设备判故障。
				p := s.points[i]
				s.mu.Lock()
				s.values[p.id] = subValue{
					quality: ua.QualityOf(res.StatusCode),
					err: driver.E(statusKind(res.StatusCode), "subscribe", s.deviceID,
						fmt.Sprintf("%s: %s", p.id, res.StatusCode.Text()), false, res.StatusCode),
					at: time.Now(),
				}
				s.mu.Unlock()
				s.log("opcua_monitored_item_rejected",
					"device", s.deviceID, "point", p.id, "status", res.StatusCode.Text())
			}
			return nil
		}); err != nil {
		_ = c.Close()
		return driver.E(statusKindOf(err), "subscribe", s.deviceID, err.Error(), false, err)
	}

	s.mu.Lock()
	s.conn = c
	s.healthy = true
	s.lastAt = time.Now()
	s.lastErr = ""
	s.mu.Unlock()
	s.log("opcua_subscription_established",
		"device", s.deviceID, "subscription", s.subID, "items", len(s.points),
		"publishingInterval", s.pubEvery.String())
	return nil
}

// start 启动发布循环。
func (s *subState) start(ctx context.Context) {
	go s.run(ctx)
}

// abortConn 打断阻塞中的 Publish。
func (s *subState) abortConn() {
	s.mu.RLock()
	c := s.conn
	s.mu.RUnlock()
	if c != nil {
		c.abort()
	}
}

// waitDone 等发布循环退出；超时返回 false。
func (s *subState) waitDone(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-t.C:
		return false
	}
}

// run 是发布循环：Publish → 处理通知 → 再 Publish，直到出错。
//
// 出错后不放弃：主连接可能完全正常，只是这条订阅连接断了。重建订阅比
// 让整台设备判故障更符合现场预期——数据只是变慢/回落到轮询，而不是消失。
func (s *subState) run(ctx context.Context) {
	defer close(s.done)
	defer s.closeCurrent()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.publishUntilError(ctx)
		if ctx.Err() != nil {
			return
		}
		s.markDown(err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		if backoff < maxReconnectBackoff {
			backoff *= 2
		}
		if e := s.establish(ctx); e != nil {
			s.markDown(e)
			continue
		}
		backoff = time.Second
	}
}

func (s *subState) publishUntilError(ctx context.Context) error {
	s.mu.RLock()
	c := s.conn
	s.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("%w: subscription not established", errProtocol)
	}

	var acks []ua.SubscriptionAcknowledgement
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := s.keepAliveTimeout()
		rctx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := s.publishOnce(rctx, c, acks)
		cancel()
		if err != nil {
			return err
		}
		// 每一个带序列号的通知都必须确认。不确认不会立刻丢数据，但服务端
		// 的重传队列会被填满，之后开始丢新通知——表现是「跑一阵就不动了」。
		acks = append(acks[:0], ackOf(s.subID, resp)...)

		s.mu.Lock()
		s.healthy = true
		s.lastAt = time.Now()
		s.lastErr = ""
		s.mu.Unlock()

		for _, nd := range resp.NotificationMessage.NotificationData {
			dc, sc, derr := ua.DecodeNotificationData(nd)
			if derr != nil {
				// 一条解不开的通知不该拖垮整个订阅。
				s.log("opcua_notification_decode_failed", "device", s.deviceID, "error", derr.Error())
				continue
			}
			if sc != nil {
				if sc.Status.IsBad() {
					return driver.E(statusKind(sc.Status), "publish", s.deviceID,
						"subscription status: "+sc.Status.Text(), true, sc.Status)
				}
				continue
			}
			if dc != nil {
				s.apply(dc.MonitoredItems)
			}
		}
	}
}

func ackOf(subID uint32, r ua.PublishResponse) []ua.SubscriptionAcknowledgement {
	seq := r.NotificationMessage.SequenceNumber
	if seq == 0 {
		return nil
	}
	// 保活消息不带通知，但它同样推进序列号；一起确认是安全的（服务端
	// 对未知序列号只会返回 BadSequenceNumberUnknown，不影响数据）。
	return []ua.SubscriptionAcknowledgement{{SubscriptionID: subID, SequenceNumber: seq}}
}

func (s *subState) publishOnce(ctx context.Context, c *conn, acks []ua.SubscriptionAcknowledgement) (ua.PublishResponse, error) {
	// 与主连接一样，一次事务独占连接：exchange 会自增 seq/reqID 并读到
	// 「匹配这个 requestId」的响应。两个 goroutine 同时发请求，序号会撞车，
	// 表现是其中一方把另一方的响应当成自己的，或者等到超时。
	c.mu.Lock()
	defer c.mu.Unlock()

	req := ua.PublishRequest{Header: c.newHeader(), SubscriptionAcknowledgements: acks}
	req.Header.TimeoutHint = uint32(s.keepAliveTimeout() / time.Millisecond)

	var out ua.PublishResponse
	err := c.call(ctx, "publish", ua.SvcPublishRequest, req.Encode(), ua.SvcPublishResponse,
		func(m ua.SecureMessage) error {
			r, derr := ua.DecodePublishResponse(m.Body)
			if derr != nil {
				return derr
			}
			if r.Header.ServiceResult.IsBad() {
				return r.Header.ServiceResult
			}
			out = r
			return nil
		})
	return out, err
}

// apply 把一批通知写进本地表。
func (s *subState) apply(items []ua.MonitoredItemNotification) {
	if len(items) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	for _, it := range items {
		p := s.byHandle[it.ClientHandle]
		if p == nil {
			// 未知句柄：服务端多发的（比如上一个订阅残留的通知）。忽略。
			continue
		}
		v := subValue{at: now, quality: ua.QualityOf(it.Value.Status)}
		switch {
		case it.Value.Status.IsBad():
			v.err = driver.E(statusKind(it.Value.Status), "publish", s.deviceID,
				p.id+": "+it.Value.Status.Text(), false, it.Value.Status)
		case it.Value.Value == nil:
			v.quality = model.QualityBad
			v.err = driver.E(driver.ErrProtocol, "publish", s.deviceID,
				p.id+": notification carries no value", false, nil)
		default:
			val, derr := ua.DecodeValue(*it.Value.Value, p.dt)
			if derr != nil {
				v.quality = model.QualityBad
				v.err = driver.E(driver.ErrProtocol, "publish", s.deviceID,
					p.id+": "+derr.Error(), false, derr)
			} else {
				v.val = val
			}
		}
		// 值级错误不算「订阅挂了」：单个节点的 BadStatus 是设备/配置问题。
		if v.err != nil && v.quality == model.QualityGood {
			v.quality = model.QualityBad
		}
		s.values[p.id] = v
	}
	s.mu.Unlock()
	if s.metrics != nil && len(items) > 0 {
		s.metrics.Counter("uic_driver_notifications_total",
			map[string]string{"protocol": "opcua", "device": s.deviceID})(float64(len(items)))
	}
}

// lookup 尝试用订阅到的值满足一次读取。第二个返回值为 false 表示
// 「这次读不到订阅值，请走真实 Read」。
func (s *subState) lookup(pointID string, dt model.DataType) (driver.RawSample, bool) {
	s.mu.RLock()
	v, found := s.values[pointID]
	healthy, lastAt := s.healthy, s.lastAt
	s.mu.RUnlock()

	// 订阅不通时退化为轮询：与其拿最后一次成功的读数冒充当前值，不如
	// 老老实实问一次。订阅连接会自己重建，重建好之后这里又走本地表。
	if !healthy || !found {
		return driver.RawSample{}, false
	}
	// 保活都收不到，说明这条发布链路已经静默失联。
	if !lastAt.IsZero() && time.Since(lastAt) > s.staleAfter() {
		return driver.RawSample{}, false
	}
	// 点位被重新配置成别的类型：订阅里的旧值不可比，交给真实 Read。
	if v.err == nil && v.val.Type != dt {
		return driver.RawSample{}, false
	}
	return driver.RawSample{PointID: pointID, Value: v.val, Quality: v.quality, Err: v.err}, true
}

func (s *subState) keepAliveTimeout() time.Duration {
	d := s.pubEvery * time.Duration(s.keepAlive) * 2
	if d < minKeepAliveTimeout {
		d = minKeepAliveTimeout
	}
	return d
}

func (s *subState) staleAfter() time.Duration { return s.keepAliveTimeout() + time.Second }

func (s *subState) markDown(err error) {
	s.mu.Lock()
	was := s.healthy
	s.healthy = false
	s.lastErr = errText(err)
	s.mu.Unlock()
	if was {
		s.log("opcua_subscription_down", "device", s.deviceID, "error", errText(err))
	}
	if s.metrics != nil {
		s.metrics.Counter("uic_driver_subscription_errors_total",
			map[string]string{"protocol": "opcua", "device": s.deviceID})(1)
	}
}

// closeCurrent 关闭当前的订阅连接。先删订阅再关会话：只关 TCP 会让服务端
// 把订阅一直保留到会话超时，某些产品会因此拒绝新建订阅。
func (s *subState) closeCurrent() {
	s.mu.Lock()
	c := s.conn
	s.conn = nil
	s.healthy = false
	subID := s.subID
	s.mu.Unlock()
	if c == nil {
		return
	}
	if subID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := ua.DeleteSubscriptionsRequest{Header: c.newHeader(), SubscriptionIDs: []uint32{subID}}
		_ = c.call(ctx, "delete-subscription", ua.SvcDeleteSubscriptionsRequest, req.Encode(), ua.SvcDeleteSubscriptionsResponse,
			func(ua.SecureMessage) error { return nil })
		cancel()
	}
	_ = c.Close()
}

// healthy 返回订阅是否可用（诊断/测试用）。
func (s *subState) isHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthy
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errProtocol 是内部错误哨兵，避免为「响应格式不对」引入新的错误类型。
var errProtocol = fmt.Errorf("opcua: protocol error")

// statusKindOf 让 ServiceFault 状态码走正常分类；其他错误原样返回。
func statusKindOf(err error) driver.ErrorKind {
	if s, ok := err.(ua.StatusCode); ok {
		return statusKind(s)
	}
	return driver.KindOf(err)
}
