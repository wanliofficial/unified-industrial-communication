package opcuaserver

import (
	"sync"
	"time"

	ua "uic/internal/codec/opcua"
)

// 订阅：Publish 是长轮询，服务端要「有通知就回，没通知就等到发布周期」。
//
// 这里刻意保持了一个真实服务端该有的三个行为，因为它们正是客户端最容易
// 写错的地方：
//
//  1. 建立监视项后立刻上报一次当前值（初始样本）。少了它，客户端在第一个
//     发布周期之前拿不到任何数据，只能在 Read 里兜底——很多实现因此把
//     「订阅」写成了「轮询 + 一份缓存」。
//  2. 每个带数据的 NotificationMessage 推进一次序列号；客户端必须确认。
//     不确认虽不立刻丢数据，但真实服务端会在重传队列满后开始丢新通知。
//  3. 空闲时按 MaxKeepAliveCount 个发布周期发一条空通知。客户端靠它区分
//     「设备没变化」与「链路已经静默失联」。

// subscription 是一个订阅。锁的粒度是订阅而不是服务器：Publish 可能阻塞
// 数秒，拿服务器全局锁会把 Read/Write 一起拖住。
type subscription struct {
	id        uint32
	sessionID uint32

	mu          sync.Mutex
	interval    time.Duration // 发布周期
	keepAlive   uint32        // 多少个发布周期无数据就发保活
	lifetime    uint32        // 多少个发布周期没有 Publish 就删除订阅
	seq         uint32        // 最近一次发出的序列号
	lastPublish time.Time
	items       map[uint32]*monitoredItem
	nextItem    uint32
	pending     []ua.MonitoredItemNotification
	wake        chan struct{}
	deleted     bool
}

type monitoredItem struct {
	id           uint32
	node         ua.NodeID
	clientHandle uint32
	queueSize    uint32
}

func (sub *subscription) keepAliveEvery() time.Duration {
	n := time.Duration(sub.keepAlive)
	if n <= 0 {
		n = 1
	}
	return time.Duration(n) * sub.interval
}

func (sub *subscription) lifetimeEvery() time.Duration {
	n := time.Duration(sub.lifetime)
	if n <= 0 {
		n = 3
	}
	return time.Duration(n) * sub.interval
}

func (sub *subscription) markDeleted() {
	sub.mu.Lock()
	sub.deleted = true
	sub.mu.Unlock()
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

// wake 唤醒一个阻塞在 Publish 上的调用者。
func (sub *subscription) poke() {
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

// enqueue 把一条数据变化放进待发布队列。
//
// 队列语义与监视项请求的 QueueSize=1 + DiscardOldest 一致：同一个监视项
// 只保留最新一条。真实服务端按每个监视项排队，这里用「同句柄去重」达到
// 同样效果，同时保证 pending 不会随时间无限增长。
func (sub *subscription) enqueue(clientHandle uint32, dv ua.DataValue) {
	sub.mu.Lock()
	kept := sub.pending[:0]
	for _, it := range sub.pending {
		if it.ClientHandle != clientHandle {
			kept = append(kept, it)
		}
	}
	sub.pending = append(kept, ua.MonitoredItemNotification{ClientHandle: clientHandle, Value: dv})
	sub.mu.Unlock()
	sub.poke()
}

// notifyChange 把一次值变化分发给所有监视该节点的订阅。
//
// 调用方必须已经释放 s.mu：本函数要拿订阅锁，而 Publish 路径持有订阅锁
// 时会回读服务器节点表——反过来加锁就是一个典型的两把锁互等。
func (s *Server) notifyChange(nid ua.NodeID, v ua.Variant, status ua.StatusCode, at time.Time) {
	key := nid.String()

	s.mu.RLock()
	subs := make([]*subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.RUnlock()

	dv := ua.DataValue{Status: status, SourceTimestamp: at, ServerTimestamp: at}
	if status.IsGood() {
		val := v
		dv.Value = &val
	}

	for _, sub := range subs {
		sub.mu.Lock()
		type hit struct {
			handle uint32
			qsize  uint32
		}
		var hits []hit
		if !sub.deleted {
			for _, it := range sub.items {
				if it.node.String() == key {
					hits = append(hits, hit{handle: it.clientHandle, qsize: it.queueSize})
				}
			}
		}
		sub.mu.Unlock()
		for _, h := range hits {
			sub.enqueue(h.handle, dv)
		}
	}
}

func (s *Server) pickSubscription(sessionID uint32) *subscription {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sub := range s.subs {
		if sub.sessionID != sessionID {
			continue
		}
		// deleted 由订阅自己的锁保护：删除发生在 markDeleted 里，而这里
		// 持有的是服务器锁。锁顺序固定为 s.mu → sub.mu，反向会互等。
		sub.mu.Lock()
		deleted := sub.deleted
		sub.mu.Unlock()
		if !deleted {
			return sub
		}
	}
	return nil
}

func (s *Server) dropSessionSubscriptions(sessionID uint32) {
	s.mu.Lock()
	var dropped []*subscription
	for id, sub := range s.subs {
		if sub.sessionID == sessionID {
			dropped = append(dropped, sub)
			delete(s.subs, id)
		}
	}
	s.mu.Unlock()
	for _, sub := range dropped {
		sub.markDeleted()
	}
}

// ---------------------------------------------------------------------------
// 服务实现
// ---------------------------------------------------------------------------

func (s *Server) handleCreateSubscription(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeCreateSubscriptionRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}

	interval := time.Duration(req.RequestedPublishingInterval) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	// 50ms 下限不是规范要求，是「演示服务端不该被 1ms 的发布周期打满 CPU」
	// 的自我约束。真实服务端同样会修订这个值，客户端必须读修订后的值。
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	keepAlive := req.RequestedMaxKeepAliveCount
	if keepAlive == 0 {
		keepAlive = 10
	}
	lifetime := req.RequestedLifetimeCount
	if lifetime < 3*keepAlive {
		lifetime = 3 * keepAlive
	}

	sub := &subscription{
		sessionID:   sess.sessionID,
		interval:    interval,
		keepAlive:   keepAlive,
		lifetime:    lifetime,
		items:       map[uint32]*monitoredItem{},
		wake:        make(chan struct{}, 1),
		lastPublish: time.Now(),
	}

	s.mu.Lock()
	s.nextSub++
	sub.id = s.nextSub
	s.subs[sub.id] = sub
	s.mu.Unlock()

	resp := ua.CreateSubscriptionResponse{
		Header: ua.ResponseHeader{
			Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle,
		},
		SubscriptionID:            sub.id,
		RevisedPublishingInterval: float64(interval / time.Millisecond),
		RevisedLifetimeCount:      lifetime,
		RevisedMaxKeepAliveCount:  keepAlive,
	}
	return ua.SvcCreateSubscriptionResponse, resp.Encode()
}

func (s *Server) handleCreateMonitoredItems(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeCreateMonitoredItemsRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}
	sub := s.pickSubscription(sess.sessionID)
	if sub == nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
			ua.StatusBadNoSubscription, "subscription not found")
	}

	now := time.Now()
	results := make([]ua.MonitoredItemCreateResult, len(req.ItemsToCreate))
	for i, item := range req.ItemsToCreate {
		// 只允许监视 Value 属性：这个仿真服务端没有事件、没有别名，
		// 假装支持别的属性只会让客户端以为「监视 DisplayName 也能拿到值」。
		if item.ItemToMonitor.AttributeID != ua.AttrValue {
			results[i] = ua.MonitoredItemCreateResult{StatusCode: ua.StatusBadAttributeInvalid}
			continue
		}
		nid := item.ItemToMonitor.NodeID

		s.mu.RLock()
		n, ok := s.nodes[nid.String()]
		var sample ua.DataValue
		if ok {
			sample = dataValueOf(n, now)
		}
		s.mu.RUnlock()
		if !ok {
			results[i] = ua.MonitoredItemCreateResult{StatusCode: ua.StatusBadNodeIDUnknown}
			continue
		}
		if n.class != ua.NodeClassVariable {
			results[i] = ua.MonitoredItemCreateResult{StatusCode: ua.StatusBadAttributeInvalid}
			continue
		}

		qsize := item.RequestedParameters.QueueSize
		if qsize == 0 {
			qsize = 1
		}

		sub.mu.Lock()
		sub.nextItem++
		mi := &monitoredItem{
			id:           sub.nextItem,
			node:         nid,
			clientHandle: item.RequestedParameters.ClientHandle,
			queueSize:    qsize,
		}
		sub.items[mi.id] = mi
		sub.mu.Unlock()

		// 初始样本：真实服务端在监视项建立后立刻上报一次当前值（含质量码）。
		// 少了这一步，客户端在第一个发布周期之前没有任何数据，「订阅」就
		// 退化成了「轮询 + 一份缓存」。
		sub.enqueue(mi.clientHandle, sample)

		results[i] = ua.MonitoredItemCreateResult{
			StatusCode:              ua.StatusGood,
			MonitoredItemID:         mi.id,
			RevisedSamplingInterval: float64(sub.interval / time.Millisecond),
			RevisedQueueSize:        qsize,
		}
	}

	resp := ua.CreateMonitoredItemsResponse{
		Header:  ua.ResponseHeader{Timestamp: now, RequestHandle: req.Header.RequestHandle},
		Results: results,
	}
	return ua.SvcCreateMonitoredItemsResponse, resp.Encode()
}

func (s *Server) handleDeleteMonitoredItems(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeDeleteMonitoredItemsRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}
	sub := s.pickSubscription(sess.sessionID)
	if sub == nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
			ua.StatusBadNoSubscription, "subscription not found")
	}

	results := make([]ua.StatusCode, len(req.MonitoredItemIDs))
	sub.mu.Lock()
	for i, id := range req.MonitoredItemIDs {
		if _, ok := sub.items[id]; ok {
			delete(sub.items, id)
			results[i] = ua.StatusGood
		} else {
			results[i] = ua.StatusBadMonitoredItemIDInvalid
		}
	}
	sub.mu.Unlock()

	resp := ua.DeleteMonitoredItemsResponse{
		Header:  ua.ResponseHeader{Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle},
		Results: results,
	}
	return ua.SvcDeleteMonitoredItemsResponse, resp.Encode()
}

func (s *Server) handleDeleteSubscriptions(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeDeleteSubscriptionsRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}

	results := make([]ua.StatusCode, len(req.SubscriptionIDs))
	for i, id := range req.SubscriptionIDs {
		s.mu.Lock()
		sub, ok := s.subs[id]
		if ok && sub.sessionID == sess.sessionID {
			delete(s.subs, id)
		} else {
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			results[i] = ua.StatusBadNoSubscription
			continue
		}
		sub.markDeleted()
		results[i] = ua.StatusGood
	}

	resp := ua.DeleteSubscriptionsResponse{
		Header:  ua.ResponseHeader{Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle},
		Results: results,
	}
	return ua.SvcDeleteSubscriptionsResponse, resp.Encode()
}

func (s *Server) handleSetPublishingMode(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodeSetPublishingModeRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}
	// 暂停/恢复推送不在本仿真服务端的语义里：它只在值变化时才有通知，
	// 没有「周期上报」可暂停。返回 Good 而不是假装支持。
	results := make([]ua.StatusCode, len(req.SubscriptionIDs))
	for i := range results {
		results[i] = ua.StatusGood
	}
	resp := ua.SetPublishingModeResponse{
		Header:  ua.ResponseHeader{Timestamp: time.Now(), RequestHandle: req.Header.RequestHandle},
		Results: results,
	}
	return ua.SvcSetPublishingModeResponse, resp.Encode()
}

// handlePublish 可能阻塞到保活周期，因此它在独立 goroutine 里执行
// （见 dispatch）：否则同一连接上的 DeleteSubscriptions 永远排不到。
// OPC UA 允许响应乱序，客户端按 RequestId 匹配。
func (s *Server) handlePublish(sess *session, body []byte) (uint32, []byte) {
	req, err := ua.DecodePublishRequest(body)
	if err != nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, 0, ua.StatusBadUnexpectedError, err.Error())
	}
	if id, f := s.checkSession(sess, req.Header.AuthenticationToken, req.Header.RequestHandle); f != nil {
		return id, f
	}
	sub := s.pickSubscription(sess.sessionID)
	if sub == nil {
		return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
			ua.StatusBadNoSubscription, "subscription not found")
	}

	sub.mu.Lock()
	last := sub.seq
	sub.mu.Unlock()
	results := make([]ua.StatusCode, len(req.SubscriptionAcknowledgements))
	for i, a := range req.SubscriptionAcknowledgements {
		switch {
		case a.SubscriptionID != sub.id:
			results[i] = ua.StatusBadSubscriptionIDInvalid
		case a.SequenceNumber <= last:
			results[i] = ua.StatusGood
		default:
			results[i] = ua.StatusBadSequenceNumberUnknown
		}
	}

	deadline := time.Now().Add(sub.lifetimeEvery())
	for {
		select {
		case <-sess.done:
			return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
				ua.StatusBadSessionIDInvalid, "connection closed")
		default:
		}

		sub.mu.Lock()
		if sub.deleted {
			sub.mu.Unlock()
			return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
				ua.StatusBadNoSubscription, "subscription deleted")
		}
		if len(sub.pending) > 0 {
			items := sub.pending
			sub.pending = nil
			sub.seq++
			seq := sub.seq
			now := time.Now()
			sub.lastPublish = now
			sub.mu.Unlock()

			dc := ua.DataChangeNotification{MonitoredItems: items}
			msg := ua.NotificationMessage{
				SequenceNumber:   seq,
				PublishTime:      now,
				NotificationData: []ua.ExtensionObject{ua.NewExtensionObject(ua.ExtDataChangeNotification, dc.Encode())},
			}
			resp := ua.PublishResponse{
				Header:              ua.ResponseHeader{Timestamp: now, RequestHandle: req.Header.RequestHandle},
				SubscriptionID:      sub.id,
				MoreNotifications:   false,
				NotificationMessage: msg,
				Results:             results,
			}
			return ua.SvcPublishResponse, resp.Encode()
		}
		now := time.Now()
		keepAt := sub.lastPublish.Add(sub.keepAliveEvery())
		sub.mu.Unlock()

		if now.After(deadline) {
			return ua.SvcServiceFaultResponse, s.fault(nil, req.Header.RequestHandle,
				ua.StatusBadTimeout, "no publish within lifetime")
		}
		if !now.Before(keepAt) {
			// 保活：空通知，不推进序列号（客户端确认它也无害，
			// 本服务端会对未知序列号返回 BadSequenceNumberUnknown）。
			sub.mu.Lock()
			seq := sub.seq
			sub.lastPublish = now
			sub.mu.Unlock()
			resp := ua.PublishResponse{
				Header:         ua.ResponseHeader{Timestamp: now, RequestHandle: req.Header.RequestHandle},
				SubscriptionID: sub.id,
				NotificationMessage: ua.NotificationMessage{
					SequenceNumber: seq,
					PublishTime:    now,
				},
				Results: results,
			}
			return ua.SvcPublishResponse, resp.Encode()
		}

		timer := time.NewTimer(keepAt.Sub(now))
		select {
		case <-sub.wake:
		case <-timer.C:
		case <-sess.done:
		}
		timer.Stop()
	}
}
