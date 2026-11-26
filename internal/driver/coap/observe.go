package coap

// 本文件实现 RFC 7641 Observe 的客户端侧：用一个独立的 UDP 套接字维持若干
// 资源的观察关系，把「设备主动推送的通知」沉淀进一张最新值表，`Read` 优先读
// 这张表、过期或缺失时退回一次真实的 GET。
//
// 为什么用独立套接字而不是复用主链路：主链路是严格请求/响应的同步事务，
// 一条报文要么是对某次 GET 的应答，要么是无关的杂包（当前一律回 RST）。
// Observe 是长期、单向、由对端驱动的推送流——它的工作时序（重注册、保鲜、
// 乱序丢弃）与同步事务完全不同。两者混在一条链路上，会让「一次读的超时」
// 和「一条推送的到达」互相干扰，正是这个驱动刻意不做 Observe 的旧理由。
// 独立链路让两套时序各自干净，且与 OPC UA「主连接取数、订阅连接推送」的
// 既有形态保持一致。
//
// 范围边界（刻意）：只做**明文 UDP、非分块**的通知。带 Block2 的通知会让
// 「最新值表」语义变得复杂（一次通知要多次往返才能拼完，期间还可能有更新的
// 通知到达），此时一律放弃该点位的推送值、退回真实 GET——诚实的降级，而不是
// 把半条通知当成完整值。DTLS 同样不在范围内。

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	coapcodec "uic/internal/codec/coap"
	"uic/internal/driver"
	"uic/internal/model"
)

// 观察参数。默认值都偏保守：注册要快（让 Open 失败能尽早暴露），保鲜要宽
// （推送本来就稀疏，过于激进的保鲜会频繁退回 GET，把订阅退化成轮询）。
const (
	obsRegisterTimeout = 1 * time.Second // 单次注册确认等待
	obsRegisterBackoff = 2 * time.Second // 注册失败后的基础退避间隔
	obsMaxAttempts     = 6               // 单个点位的最大连续注册失败次数
	obsMinBackoff      = 1 * time.Second // 退避下限
	obsMaxBackoff      = 5 * time.Minute // 退避上限
)

// obsPoint 是单个点位的观察状态。
type obsPoint struct {
	ref    driver.PointRef
	plan   pointPlan
	dt     model.DataType
	maxAge time.Duration

	reg bool // 是否处于「已确认观察」状态

	token    []byte    // 本观察关系的令牌
	seq      uint32    // 最近一次接受的 Observe 序号（用于新鲜度判定）
	received time.Time // 最近一次通知到达时间

	// 注册退避簿记（仅未确认观察时有效）。
	attempts int
	lastSent time.Time
	// lastReg 是最近一次「已确认观察的刷新重注册」时间，用于计算下次重注册，
	// 与 received（最近通知时间，用于新鲜度/保鲜）分开：混用会让「长期无通知」
	// 与「刚重注册过」互相覆盖，导致要么频繁重注册、要么永不重注册。
	lastReg time.Time
}

// obsValue 是沉淀进最新值表的单次读数。
type obsValue struct {
	val     model.Value
	quality model.Quality
	err     error
	at      time.Time
}

// observeState 管理一条 Observe 链路与一张最新值表。
//
// 与 OPC UA 的 subState 同构：单一后台 goroutine 泵入报文（注册、重注册、
// 接收通知），`lookup` 读表；`Read` 在订阅不健康或值过期时退回真实 GET。
type observeState struct {
	devID string

	metrics driver.Metrics

	dialLink func(ctx context.Context) (obsLink, error)

	// 新鲜期：超过这么久没有通知，就认为推送链路静默，退回真实 GET。
	// 取 60s（与 Max-Age 缺省值一致）；点位声明的 Max-Age 更小时按点位收紧。
	staleAfter time.Duration

	mu     sync.Mutex
	link   obsLink
	closed bool
	cancel context.CancelFunc

	mid      uint16
	tokenSeq uint64

	points map[string]*obsPoint // pointID → 状态
	byPath map[string]*obsPoint // 资源路径 → 状态（同一路径只观察一次）

	values map[string]obsValue // pointID → 最新值（仅含成功读数）
}

// obsLink 是 observeState 用到的最小 UDP 能力。真实实现转发 transport.Datagram，
// 测试可注入内存链路。
type obsLink interface {
	Write(p []byte) error
	Read() ([]byte, error)
	SetDeadline(t time.Time) error
	Close() error
}

// observePoints 选出「启用且可规划」的点位作为观察对象。
//
// 一个资源路径只观察一次（同一路径可能被多个点位引用），因此返回去重后的
// 点位列表，byPath 由调用方据此建索引。
func observePoints(cfg driver.OpenConfig, byID map[string]pointPlan) ([]obsPoint, error) {
	out := make([]obsPoint, 0, len(cfg.Device.Points))
	for _, p := range cfg.Device.Points {
		if !p.Enabled {
			continue
		}
		ref := driver.PointRefOf(cfg.Device, p)
		plan, ok := byID[ref.ID]
		if !ok {
			var err error
			plan, err = planPoint(ref)
			if err != nil {
				// 单个点位规划失败不应拖垮整台设备的订阅：它会被 Read 的
				// 真实 GET 路径按点位拒绝（与未订阅时一致）。跳过即可。
				continue
			}
		}
		out = append(out, obsPoint{ref: ref, plan: plan, dt: plan.typ})
	}
	return out, nil
}

// newObserveState 创建观察状态。establish 之前 link 可能尚未建立。
//
// dialLink 抽象建链：生产环境传一个 dial UDP 的闭包，测试注入内存链路。
func newObserveState(devID string, dialLink func(ctx context.Context) (obsLink, error),
	points []obsPoint, metrics driver.Metrics) *observeState {

	o := &observeState{
		devID:      devID,
		metrics:    metrics,
		dialLink:   dialLink,
		staleAfter: 60 * time.Second,
		points:     make(map[string]*obsPoint, len(points)),
		byPath:     make(map[string]*obsPoint, len(points)),
		values:     make(map[string]obsValue, len(points)),
	}
	for i := range points {
		p := &points[i]
		p.token = o.nextToken()
		p.maxAge = 60 * time.Second // 缺省 Max-Age，首个通知声明后会收紧
		p.received = time.Now()
		o.points[p.ref.ID] = p
		o.byPath[p.plan.path] = p
	}
	return o
}

func (o *observeState) counter(name string, labels map[string]string) func(float64) {
	if o.metrics == nil {
		return func(float64) {}
	}
	return o.metrics.Counter(name, labels)
}

func (o *observeState) observe(name string, v float64, labels map[string]string) {
	if o.metrics == nil {
		return
	}
	o.metrics.Observe(name, v, labels)
}

// nextToken 返回一个非零的 4 字节令牌，与 conn.nextToken 同理（初值取墙上
// 时间，日志里好对齐）。
func (o *observeState) nextToken() []byte {
	o.tokenSeq++
	var tok [4]byte
	binary.BigEndian.PutUint32(tok[:], uint32(o.tokenSeq))
	return tok[:]
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// establish 同步建立观察链路并向每个资源发送注册请求。失败返回错误，调用方
// 据此让 Open 失败（而不是悄悄退化成轮询——那会让「配置写着订阅、实际在轮询」
// 永远没人发现）。
func (o *observeState) establish(ctx context.Context) error {
	if err := o.ensure(ctx); err != nil {
		return err
	}
	// 把所有注册请求打出去。注册不需要逐条同步等待确认：对端一旦回 2.05 且
	// 带 Observe 选项，pump 就会把它认作注册确认；超时则由 pump 重发。
	o.mu.Lock()
	for _, p := range o.byPath {
		o.sendRegisterLocked(p)
	}
	o.mu.Unlock()
	return nil
}

func (o *observeState) ensure(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return driver.E(driver.ErrDisconnected, "observe", o.devID, "connection closed", true, nil)
	}
	if o.link != nil {
		return nil
	}
	link, err := o.dialLink(ctx)
	if err != nil {
		return driver.E(driver.ErrDisconnected, "observe", o.devID,
			fmt.Sprintf("connect %s failed", "observe link"), true, err)
	}
	o.link = link
	return nil
}

// sendRegisterLocked 发送一次 GET + Observe:0 注册请求。调用方持有 o.mu。
func (o *observeState) sendRegisterLocked(p *obsPoint) {
	if o.link == nil {
		return
	}
	o.mid++
	opts := pathOptions(p.plan.path)
	opts = append(opts, coapcodec.Option{Number: coapcodec.OptObserve,
		Value: coapcodec.EncodeUint(coapcodec.ObserveRegister)})
	msg := coapcodec.Message{
		Type: coapcodec.TypeConfirmable, Code: coapcodec.CodeGET,
		MID: o.mid, Token: append([]byte(nil), p.token...), Options: opts,
	}
	raw, err := coapcodec.Encode(msg)
	if err != nil {
		return
	}
	// 记录发送时刻与尝试次数，供 nextWait/processTimeouts 计算退避。
	p.lastSent = time.Now()
	p.lastReg = p.lastSent
	if !p.reg {
		p.attempts++
	}
	deadline := time.Now().Add(obsRegisterTimeout)
	_ = o.link.SetDeadline(deadline)
	_ = o.link.Write(raw)
}

// start 启动后台泵。ctx 取消时退出。
func (o *observeState) start(ctx context.Context) {
	go o.pump(ctx)
}

// close 停止泵、向对端注销每个观察并关闭链路。
func (o *observeState) close() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	cancel := o.cancel
	link := o.link
	// 主动注销：对每个已确认观察发一次 GET + Observe:1。失败无所谓——
	// 关闭套接字已足够让服务端停止推送，注销只是让「优雅退出」更快收敛。
	registered := make([]*obsPoint, 0)
	for _, p := range o.byPath {
		if p.reg {
			registered = append(registered, p)
		}
	}
	if link != nil {
		for _, p := range registered {
			o.sendCancelLocked(p, link)
		}
	}
	o.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if link != nil {
		_ = link.Close()
	}
}

// sendCancelLocked 发送 GET + Observe:1 注销。调用方持有 o.mu，link 显式传入。
func (o *observeState) sendCancelLocked(p *obsPoint, link obsLink) {
	o.mid++
	opts := pathOptions(p.plan.path)
	opts = append(opts, coapcodec.Option{Number: coapcodec.OptObserve,
		Value: coapcodec.EncodeUint(coapcodec.ObserveCancel)})
	msg := coapcodec.Message{
		Type: coapcodec.TypeConfirmable, Code: coapcodec.CodeGET,
		MID: o.mid, Token: append([]byte(nil), p.token...), Options: opts,
	}
	raw, err := coapcodec.Encode(msg)
	if err != nil {
		return
	}
	_ = link.SetDeadline(time.Now().Add(obsRegisterTimeout))
	_ = link.Write(raw)
}

// ---------------------------------------------------------------------------
// 后台泵
// ---------------------------------------------------------------------------

// pump 是唯一的读循环。它做三件事：
//   - 对「未确认观察」的资源按退避重发注册；
//   - 接收通知并沉淀进最新值表（按新鲜度）；
//   - 定期重注册确认观察仍然存活。
//
// 单一 goroutine 泵入，因此读写 o.points/o.values 都在 o.mu 保护下，而「等
// 报文」用的是带超时的 Read——超时即回到循环顶部重排定时器，不会阻塞 Close。
func (o *observeState) pump(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return
		}
		link := o.link
		o.mu.Unlock()

		if link == nil {
			// 链路断了（读错误后被 dropLink）：尝试重建。重建失败就等一个
			// 退避周期再试——期间 Read 退回真实 GET，所以不必急。
			if err := o.ensure(ctx); err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(obsMinBackoff):
				}
				continue
			}
			continue
		}

		// 直接阻塞在 Read 上：报文一到就返回，不会被「定时器还没到」拖慢。
		// wait 只决定「最迟多久查一次到期任务」，不影响收包时延。
		o.mu.Lock()
		wait := o.nextWait()
		o.mu.Unlock()
		if wait <= 0 {
			o.mu.Lock()
			o.processTimeouts(time.Now())
			o.mu.Unlock()
			continue
		}
		if wait > time.Second {
			wait = time.Second
		}

		_ = link.SetDeadline(time.Now().Add(wait))
		dg, rerr := link.Read()
		if rerr != nil {
			if ctx.Err() != nil {
				return
			}
			if !isTimeout(rerr) {
				// 链路读错误：丢掉链路，下一轮 ensure 会重建。已沉淀的值仍在
				// 表里，但会很快过期 → Read 退回真实 GET，不会返回陈旧值冒充实时。
				o.dropLink()
				continue
			}
			// 超时：��理到期的注册重发 / 确认重注册。
			o.mu.Lock()
			o.processTimeouts(time.Now())
			o.mu.Unlock()
			continue
		}
		msg, derr := coapcodec.Decode(dg)
		if derr != nil {
			continue
		}
		o.handleMessage(msg)
	}
}

// nextWait 返回距下一个需要动作的注册时间还有多久。调用方持有 o.mu。
func (o *observeState) nextWait() time.Duration {
	now := time.Now()
	wait := time.Duration(1<<62 - 1)
	for _, p := range o.byPath {
		if !p.reg {
			// 未确认：按指数退避重发注册。
			backoff := backoffFor(p.attempts)
			d := p.lastSent.Add(backoff).Sub(now)
			if d < wait {
				wait = d
			}
			continue
		}
		// 已确认：在 Max-Age 即将过期时重注册，刷新观察关系。基准用 lastReg
		// （最近一次注册/重注册时间），不用 received——后者只反映通知到达。
		d := p.lastReg.Add(o.reRegisterAfter(p)).Sub(now)
		if d < wait {
			wait = d
		}
	}
	if wait < 0 {
		return 0
	}
	return wait
}

// backoffFor 把第 n 次尝试映射成指数退避时长，钳制在 [obsMinBackoff, obsMaxBackoff]。
func backoffFor(attempt int) time.Duration {
	backoff := obsRegisterBackoff
	for i := 1; i < attempt && backoff < obsMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > obsMaxBackoff {
		backoff = obsMaxBackoff
	}
	if backoff < obsMinBackoff {
		backoff = obsMinBackoff
	}
	return backoff
}

// reRegisterAfter 是已确认观察的「静默多久后重注册」。取 Max-Age 与陈旧期之间
// 的较大值，确保不会因为推送稀疏而误判链路死亡、频繁重注册。
func (o *observeState) reRegisterAfter(p *obsPoint) time.Duration {
	d := p.maxAge * 2
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	if d < o.staleAfter {
		d = o.staleAfter
	}
	return d
}

// processTimeouts 处理到期的注册重发与确认重注册。调用方持有 o.mu。
func (o *observeState) processTimeouts(now time.Time) {
	for _, p := range o.byPath {
		if !p.reg {
			// 退避重发注册。
			if now.Sub(p.lastSent) < backoffFor(p.attempts) {
				continue
			}
			if p.attempts >= obsMaxAttempts {
				// 连续失败次数过多：停止重发，让该点位退回真实 GET。
				// 记一个 metric 便于诊断，但不让整个订阅崩掉。
				o.counter("coap.observe.register_failed",
					map[string]string{"device": o.devID, "path": p.plan.path})(1)
				continue
			}
			o.sendRegisterLocked(p) // 内部会更新 lastSent 与 attempts
			continue
		}
		// 已确认但长时间无通知：重注册刷新（基准用 lastReg，避免与通知时间混淆）。
		if now.Sub(p.lastReg) >= o.reRegisterAfter(p) {
			o.sendRegisterLocked(p)
		}
	}
}

// handleMessage 处理一条收到的报文。它对注册确认与通知一视同仁——两者都是
// 「同 Token 的 2.05 且带 Observe 选项」。对端拒绝观察（4.xx/5.xx）或回 RST
// 则标记该点位退回真实 GET。
func (o *observeState) handleMessage(msg coapcodec.Message) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	// 找本观察关系的令牌。通知与注册确认都带着当初注册用的 Token。
	var p *obsPoint
	if len(msg.Token) > 0 {
		for _, cand := range o.byPath {
			if bytesEqual(cand.token, msg.Token) {
				p = cand
				break
			}
		}
	}
	if p == nil {
		// 不属于任何观察关系的报文（可能是迟到/串包）：CON 回 RST，让其停止。
		if msg.Type == coapcodec.TypeConfirmable && o.link != nil {
			o.sendEmptyLocked(msg.MID)
		}
		return
	}
	if msg.Type == coapcodec.TypeReset {
		// 对端拒绝/取消了观察：退回真实 GET。
		p.reg = false
		delete(o.values, p.ref.ID)
		return
	}
	if !msg.Code.IsResponse() {
		return
	}
	if !msg.Code.Success() {
		// 对端在注册或通知上回了错误码：放弃该观察。
		p.reg = false
		delete(o.values, p.ref.ID)
		return
	}
	seq, hasObs := msg.Observe()
	if !hasObs {
		// 没有 Observe 选项的响应不是通知（可能是注册后第一条纯 2.05，多数
		// 服务端会带；带不了的也按「初始读数」接受，reg 置真、seq 视为 0）。
		if p.reg {
			// 已确认后又来一条无 Observe 的 2.05：当成一次普通通知处理。
			o.applyLocked(p, msg, 0, time.Now())
		} else {
			p.reg = true
			p.attempts = 0
			p.received = time.Now()
			o.applyLocked(p, msg, 0, time.Now())
		}
		return
	}
	if !p.reg {
		// 首次确认：注册成功。
		p.reg = true
		p.attempts = 0
		p.seq = seq
		p.received = time.Now()
		o.applyMaxAge(msg, p)
		o.applyLocked(p, msg, seq, time.Now())
		o.counter("coap.observe.registered",
			map[string]string{"device": o.devID, "path": p.plan.path})(1)
		return
	}
	// 已确认观察：按新鲜度接受通知。
	if coapcodec.ObserveFresh(p.seq, seq, p.received, time.Now()) {
		now := time.Now()
		// 通知间隔（设备两次推送的间隔），用于诊断推送频率是否异常。
		o.observe("coap.observe.interval",
			now.Sub(p.received).Seconds(),
			map[string]string{"device": o.devID, "path": p.plan.path})
		p.seq = seq
		p.received = now
		o.applyMaxAge(msg, p)
		o.applyLocked(p, msg, seq, now)
		o.counter("coap.observe.notification",
			map[string]string{"device": o.devID, "path": p.plan.path})(1)
	} else {
		// 乱序/重复：丢弃，但刷新 received（链路其实是活的）。
		p.received = time.Now()
		o.counter("coap.observe.stale_dropped",
			map[string]string{"device": o.devID, "path": p.plan.path})(1)
	}
}

// applyMaxAge 用通知声明的 Max-Age 收紧该点位的 maxAge（用于重注册节奏）。
func (o *observeState) applyMaxAge(msg coapcodec.Message, p *obsPoint) {
	if ma := msg.MaxAge(); ma > 0 {
		p.maxAge = ma
	}
}

// applyLocked 把一条通知的值沉淀进最新值表。调用方持有 o.mu。
// 分块通知（带 Block2）不在此处理：那条通知会被跳过，值退回真实 GET。
func (o *observeState) applyLocked(p *obsPoint, msg coapcodec.Message, seq uint32, at time.Time) {
	if _, has := msg.Get(coapcodec.OptBlock2); has {
		// 分块通知：不接受半条通知冒充完整值。
		return
	}
	if len(msg.Payload) == 0 {
		// 空载荷通知（某些服务端用空 2.04 表示「无变化」）：值没变，只刷新
		// 到达时刻以延长保鲜期——保留上一条成功读数，绝不把它清成零值。
		if ex, ok := o.values[p.ref.ID]; ok {
			ex.at = at
			o.values[p.ref.ID] = ex
		}
		return
	}
	v, err := decodeValue(p.plan, msg)
	o.values[p.ref.ID] = obsValue{val: v, quality: model.QualityGood, err: err, at: at}
}

// lookup 返回某点位的最新推送值。这是 Read 的入口。
//
// 退回真实 GET 的条件（任一成立）：
//   - 没有该点位的观察（规划失败 / 已停用）；
//   - 观察未确认（注册还没成功）；
//   - 表里没有该点位的值；
//   - 值已经过期（超过保鲜期）；
//   - 值解码出错（不拿坏值冒充实时）；
//   - 点位被重新配置成别的类型（订阅里的旧值不可比）。
func (o *observeState) lookup(pointID string, dt model.DataType) (driver.RawSample, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, found := o.values[pointID]
	if !found {
		return driver.RawSample{}, false
	}
	if v.err != nil {
		return driver.RawSample{}, false
	}
	// 空载荷「无变化」通知：表里保留的是上一条成功读数，其 at 是上一条时间。
	if v.at.IsZero() {
		return driver.RawSample{}, false
	}
	// 保鲜期：超过则退回真实 GET，不拿陈旧值冒充实时。
	age := time.Since(v.at)
	stale := o.staleAfter
	// 用该点位声明的 Max-Age 进一步收紧（如果有）。
	if p := o.points[pointID]; p != nil && p.maxAge > 0 && p.maxAge < stale {
		stale = p.maxAge
	}
	if age > stale {
		return driver.RawSample{}, false
	}
	// 类型不一致：点位被改配成别的类型，订阅里的旧值不可比，交给真实 GET。
	p := o.points[pointID]
	if p == nil || p.dt != dt {
		return driver.RawSample{}, false
	}
	return driver.RawSample{PointID: pointID, Value: v.val, Quality: model.QualityGood}, true
}

// healthy 报告整条观察链路是否至少有一个已确认观察（供诊断/Probe）。
func (o *observeState) healthy() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.byPath {
		if p.reg {
			return true
		}
	}
	return false
}

// dropLink 关闭并丢弃观察链路（读错误时调用）。pump 下一轮 ensure 会重建。
func (o *observeState) dropLink() {
	o.mu.Lock()
	link := o.link
	o.link = nil
	// 链路断了，所有观察的「确认」状态不可信：清空，等待重新注册。
	for _, p := range o.byPath {
		p.reg = false
		p.attempts = 0
	}
	o.mu.Unlock()
	if link != nil {
		_ = link.Close()
	}
}

// sendEmptyLocked 发送空报文（ACK 或 RST）。调用方持有 o.mu。
func (o *observeState) sendEmptyLocked(mid uint16) {
	if o.link == nil {
		return
	}
	b, err := coapcodec.Encode(coapcodec.Message{Type: coapcodec.TypeReset, Code: coapcodec.CodeEmpty, MID: mid})
	if err != nil {
		return
	}
	_ = o.link.SetDeadline(time.Now().Add(obsRegisterTimeout))
	_ = o.link.Write(b)
}

// bytesEqual 比较两个切片（避免在本文件再引 bytes 包）。
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
