package opcuaserver

import (
	"testing"
	"time"

	ua "uic/internal/codec/opcua"
)

// 订阅用例同样走裸报文：Publish 是长轮询，用驱动测会掩盖「服务端到底
// 在什么时候回包」这个最关键的事实——而客户端的去重、确认、保活超时
// 全建立在这个事实上。

func (c *client) recvService() (uint32, uint32, []byte) {
	c.t.Helper()
	raw := c.readRaw()
	m, err := ua.DecodeSecureMessage(raw)
	if err != nil {
		c.t.Fatalf("decode MSG: %v", err)
	}
	if m.Type != ua.MsgMessage {
		c.t.Fatalf("expected MSG, got %q", m.Type)
	}
	return m.Seq.RequestID, m.ServiceID, m.Body
}

func (c *client) send(serviceID uint32, body []byte) uint32 {
	c.t.Helper()
	c.seq++
	c.write(ua.EncodeSecureMessage(ua.MsgMessage, c.ch, c.tok,
		ua.SequenceHeader{Number: c.seq, RequestID: c.seq}, serviceID, body))
	return c.seq
}

func (c *client) createSubscription(token ua.NodeID, intervalMS float64, keepAlive uint32) uint32 {
	c.t.Helper()
	id, body := c.call(ua.SvcCreateSubscriptionRequest, ua.CreateSubscriptionRequest{
		Header:                      ua.RequestHeader{RequestHandle: 20, AuthenticationToken: token},
		RequestedPublishingInterval: intervalMS,
		RequestedLifetimeCount:      3 * keepAlive,
		RequestedMaxKeepAliveCount:  keepAlive,
		PublishingEnabled:           true,
	}.Encode())
	if id != ua.SvcCreateSubscriptionResponse {
		c.t.Fatalf("create subscription: %v", decodeFault(c.t, body))
	}
	resp, err := ua.DecodeCreateSubscriptionResponse(body)
	if err != nil {
		c.t.Fatalf("decode create subscription: %v", err)
	}
	if resp.SubscriptionID == 0 {
		c.t.Fatal("subscription id is zero")
	}
	if resp.RevisedPublishingInterval <= 0 {
		c.t.Fatalf("revised interval = %v", resp.RevisedPublishingInterval)
	}
	return resp.SubscriptionID
}

func (c *client) monitor(token ua.NodeID, subID uint32, nodes ...ua.NodeID) []ua.MonitoredItemCreateResult {
	c.t.Helper()
	items := make([]ua.MonitoredItemCreateRequest, 0, len(nodes))
	for i, n := range nodes {
		items = append(items, ua.MonitoredItemCreateRequest{
			ItemToMonitor:  ua.ReadValueID{NodeID: n, AttributeID: ua.AttrValue},
			MonitoringMode: ua.MonitoringModeReporting,
			RequestedParameters: ua.MonitoringParameters{
				ClientHandle: uint32(i + 1), QueueSize: 1, DiscardOldest: true, SamplingInterval: 50,
			},
		})
	}
	id, body := c.call(ua.SvcCreateMonitoredItemsRequest, ua.CreateMonitoredItemsRequest{
		Header:             ua.RequestHeader{RequestHandle: 21, AuthenticationToken: token},
		SubscriptionID:     subID,
		TimestampsToReturn: ua.TimestampsBoth,
		ItemsToCreate:      items,
	}.Encode())
	if id != ua.SvcCreateMonitoredItemsResponse {
		c.t.Fatalf("create monitored items: %v", decodeFault(c.t, body))
	}
	resp, err := ua.DecodeCreateMonitoredItemsResponse(body)
	if err != nil {
		c.t.Fatalf("decode monitored items: %v", err)
	}
	return resp.Results
}

func (c *client) publish(token ua.NodeID, acks ...ua.SubscriptionAcknowledgement) ua.PublishResponse {
	c.t.Helper()
	id, body := c.call(ua.SvcPublishRequest, ua.PublishRequest{
		Header:                       ua.RequestHeader{RequestHandle: 22, AuthenticationToken: token},
		SubscriptionAcknowledgements: acks,
	}.Encode())
	if id != ua.SvcPublishResponse {
		c.t.Fatalf("publish: %v", decodeFault(c.t, body))
	}
	resp, err := ua.DecodePublishResponse(body)
	if err != nil {
		c.t.Fatalf("decode publish: %v", err)
	}
	return resp
}

func dataChanges(t *testing.T, r ua.PublishResponse) []ua.MonitoredItemNotification {
	t.Helper()
	var out []ua.MonitoredItemNotification
	for _, nd := range r.NotificationMessage.NotificationData {
		dc, sc, err := ua.DecodeNotificationData(nd)
		if err != nil {
			t.Fatalf("decode notification: %v", err)
		}
		if sc != nil {
			t.Fatalf("unexpected status change %v", sc.Status)
		}
		if dc != nil {
			out = append(out, dc.MonitoredItems...)
		}
	}
	return out
}

// 监视项建立后必须立刻收到一次当前值。少了这一步，客户端在第一个发布
// 周期之前没有任何数据，「订阅」就退化成了「轮询 + 缓存」。
func TestSubscriptionReportsInitialValues(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 100, 10)
	results := c.monitor(token, subID,
		ua.StringNode(2, "Plant.Temperature"),
		ua.StringNode(2, "Plant.Running"),
	)
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for i, r := range results {
		if r.StatusCode != ua.StatusGood {
			t.Fatalf("item %d: %v", i, r.StatusCode)
		}
		if r.MonitoredItemID == 0 {
			t.Fatalf("item %d has no id", i)
		}
	}

	resp := c.publish(token)
	if resp.SubscriptionID != subID {
		t.Fatalf("subscription id = %d", resp.SubscriptionID)
	}
	if resp.NotificationMessage.SequenceNumber == 0 {
		t.Fatal("data publish must advance the sequence number")
	}
	items := dataChanges(t, resp)
	if len(items) != 2 {
		t.Fatalf("notifications = %d", len(items))
	}
	byHandle := map[uint32]ua.MonitoredItemNotification{}
	for _, it := range items {
		byHandle[it.ClientHandle] = it
	}
	temp := byHandle[1]
	if temp.Value.Value == nil || temp.Value.Value.Float != 21.5 {
		t.Fatalf("temperature = %+v", temp.Value)
	}
	run := byHandle[2]
	if run.Value.Value == nil || !run.Value.Value.Bool {
		t.Fatalf("running = %+v", run.Value)
	}

	// 写入必须回流成一条通知，否则现场会看到「控制生效了但状态不变」。
	if id, body := c.call(ua.SvcWriteRequest, ua.WriteRequest{
		Header: ua.RequestHeader{RequestHandle: 23, AuthenticationToken: token},
		Nodes: []ua.WriteValue{{
			NodeID:      ua.StringNode(2, "Plant.Setpoint"),
			AttributeID: ua.AttrValue,
			Value:       ua.DataValue{Value: &ua.Variant{Type: ua.TypeDouble, Float: 42}},
		}},
	}.Encode()); id != ua.SvcWriteResponse {
		t.Fatalf("write: %v", decodeFault(t, body))
	}

	srv.Set("ns=2;s=Plant.Setpoint", ua.Variant{Type: ua.TypeDouble, Float: 43})
	// 写入与 Set 都改的是 Setpoint；这里没有监视它，所以下一次发布
	// 应当是没有数据的保活——顺带验证「没变化就不编数据」。
}

func TestSubscriptionPushesChanges(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 60, 5)
	c.monitor(token, subID, ua.StringNode(2, "Plant.Temperature"))

	first := c.publish(token)
	items := dataChanges(t, first)
	if len(items) != 1 {
		t.Fatalf("initial notifications = %d", len(items))
	}
	// 确认序列号：不确认不会立刻丢数据，但真实服务端的重传队列会被填满。
	seq := first.NotificationMessage.SequenceNumber
	acks := []ua.SubscriptionAcknowledgement{{SubscriptionID: subID, SequenceNumber: seq}}

	srv.Set("ns=2;s=Plant.Temperature", ua.Variant{Type: ua.TypeDouble, Float: 30.25})
	second := c.publish(token, acks...)
	if len(second.Results) != 1 || second.Results[0] != ua.StatusGood {
		t.Fatalf("ack result = %v", second.Results)
	}
	items = dataChanges(t, second)
	if len(items) != 1 {
		t.Fatalf("change notifications = %d", len(items))
	}
	if items[0].Value.Value == nil || items[0].Value.Value.Float != 30.25 {
		t.Fatalf("pushed value = %+v", items[0].Value)
	}
	if second.NotificationMessage.SequenceNumber != seq+1 {
		t.Fatalf("sequence %d, want %d", second.NotificationMessage.SequenceNumber, seq+1)
	}
}

// 空闲时必须按 MaxKeepAliveCount 个发布周期回一条空通知，否则客户端
// 无法区分「设备没变化」与「链路已经静默失联」。
func TestSubscriptionKeepAlive(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 50, 2)
	c.monitor(token, subID, ua.StringNode(2, "Plant.Temperature"))
	// 吃掉初始样本。
	first := c.publish(token)
	if len(dataChanges(t, first)) != 1 {
		t.Fatal("expected the initial sample")
	}

	start := time.Now()
	kp := c.publish(token)
	elapsed := time.Since(start)
	if n := len(dataChanges(t, kp)); n != 0 {
		t.Fatalf("keepalive carried %d notifications", n)
	}
	if kp.NotificationMessage.SequenceNumber != first.NotificationMessage.SequenceNumber {
		t.Fatalf("keepalive must not advance the sequence number: %d -> %d",
			first.NotificationMessage.SequenceNumber, kp.NotificationMessage.SequenceNumber)
	}
	// 2 × 50ms = 100ms。给足调度余量，但必须明显小于任何「超时」量级。
	if elapsed > 2*time.Second {
		t.Fatalf("keepalive took %v", elapsed)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("keepalive came too early: %v", elapsed)
	}
}

// DeleteSubscriptions 必须能唤醒阻塞中的 Publish：这要求服务端把 Publish
// 放到独立 goroutine，否则同一连接上的删除请求永远排不到。
func TestDeleteSubscriptionWakesPendingPublish(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 5000, 10)
	c.monitor(token, subID, ua.StringNode(2, "Plant.Temperature"))
	// 先取走初始样本，让下一次 Publish 真正进入等待。
	_ = c.publish(token)

	pubReq := c.send(ua.SvcPublishRequest, ua.PublishRequest{
		Header: ua.RequestHeader{RequestHandle: 30, AuthenticationToken: token},
	}.Encode())
	delReq := c.send(ua.SvcDeleteSubscriptionsRequest, ua.DeleteSubscriptionsRequest{
		Header:          ua.RequestHeader{RequestHandle: 31, AuthenticationToken: token},
		SubscriptionIDs: []uint32{subID},
	}.Encode())

	// 两条响应可以乱序到达，按 RequestId 分派。
	start := time.Now()
	got := map[uint32]bool{}
	for len(got) < 2 {
		reqID, svc, body := c.recvService()
		switch reqID {
		case pubReq:
			if svc != ua.SvcServiceFaultResponse {
				t.Fatalf("publish after delete: service %d", svc)
			}
			if f := decodeFault(t, body); f.Header.ServiceResult != ua.StatusBadNoSubscription {
				t.Fatalf("publish after delete: %v", f.Header.ServiceResult)
			}
		case delReq:
			if svc != ua.SvcDeleteSubscriptionsResponse {
				t.Fatalf("delete: service %d: %v", svc, decodeFault(t, body))
			}
			resp, err := ua.DecodeDeleteSubscriptionsResponse(body)
			if err != nil {
				t.Fatalf("decode delete: %v", err)
			}
			if len(resp.Results) != 1 || resp.Results[0] != ua.StatusGood {
				t.Fatalf("delete results = %v", resp.Results)
			}
		}
		got[reqID] = true
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("delete did not wake publish: %v", d)
	}
}

// 监视不存在的节点只影响那一项，整条订阅仍然可用。
func TestMonitoredItemUnknownNodeOnlyFailsThatItem(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 100, 10)
	results := c.monitor(token, subID,
		ua.StringNode(2, "NoSuchNode"),
		ua.StringNode(2, "Plant.Temperature"),
	)
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	if results[0].StatusCode != ua.StatusBadNodeIDUnknown {
		t.Fatalf("unknown node = %v", results[0].StatusCode)
	}
	if results[1].StatusCode != ua.StatusGood {
		t.Fatalf("valid node = %v", results[1].StatusCode)
	}
	if items := dataChanges(t, c.publish(token)); len(items) != 1 {
		t.Fatalf("notifications = %d", len(items))
	}
}

func TestPublishWithoutSubscriptionIsFault(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	id, body := c.call(ua.SvcPublishRequest, ua.PublishRequest{
		Header: ua.RequestHeader{RequestHandle: 40, AuthenticationToken: token},
	}.Encode())
	if id != ua.SvcServiceFaultResponse {
		t.Fatalf("service = %d", id)
	}
	if f := decodeFault(t, body); f.Header.ServiceResult != ua.StatusBadNoSubscription {
		t.Fatalf("status = %v", f.Header.ServiceResult)
	}
}

// 删除监视项后该节点不再推送，但订阅本身仍然可用。
func TestDeleteMonitoredItemStopsNotifications(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 60, 5)
	results := c.monitor(token, subID,
		ua.StringNode(2, "Plant.Temperature"),
		ua.StringNode(2, "Plant.Counter"),
	)
	if len(results) != 2 {
		t.Fatalf("create results = %d", len(results))
	}
	if items := dataChanges(t, c.publish(token)); len(items) != 2 {
		t.Fatalf("initial notifications = %d", len(items))
	}

	ids := []uint32{results[0].MonitoredItemID}
	id, body := c.call(ua.SvcDeleteMonitoredItemsRequest, ua.DeleteMonitoredItemsRequest{
		Header:           ua.RequestHeader{RequestHandle: 50, AuthenticationToken: token},
		SubscriptionID:   subID,
		MonitoredItemIDs: ids,
	}.Encode())
	if id != ua.SvcDeleteMonitoredItemsResponse {
		t.Fatalf("delete items: %v", decodeFault(t, body))
	}
	resp, err := ua.DecodeDeleteMonitoredItemsResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0] != ua.StatusGood {
		t.Fatalf("results = %v", resp.Results)
	}

	// 删掉的节点变化不再出现；没删的仍然推送。
	srv.Set("ns=2;s=Plant.Temperature", ua.Variant{Type: ua.TypeDouble, Float: 99})
	srv.Set("ns=2;s=Plant.Counter", ua.Variant{Type: ua.TypeInt32, Int: 5})
	items := dataChanges(t, c.publish(token))
	if len(items) != 1 {
		t.Fatalf("notifications after delete = %d", len(items))
	}
	if items[0].ClientHandle != 2 {
		t.Fatalf("unexpected item %+v", items[0])
	}
}

// 重复删除同一个监视项要回逐项错误码，而不是整条请求失败。
func TestDeleteUnknownMonitoredItemIsPerItemError(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 100, 5)
	id, body := c.call(ua.SvcDeleteMonitoredItemsRequest, ua.DeleteMonitoredItemsRequest{
		Header:           ua.RequestHeader{RequestHandle: 51, AuthenticationToken: token},
		SubscriptionID:   subID,
		MonitoredItemIDs: []uint32{404},
	}.Encode())
	if id != ua.SvcDeleteMonitoredItemsResponse {
		t.Fatalf("service = %d", id)
	}
	resp, err := ua.DecodeDeleteMonitoredItemsResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0] != ua.StatusBadMonitoredItemIDInvalid {
		t.Fatalf("results = %v", resp.Results)
	}
}

func TestSetPublishingModeIsAccepted(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 100, 5)
	id, body := c.call(ua.SvcSetPublishingModeRequest, ua.SetPublishingModeRequest{
		Header:            ua.RequestHeader{RequestHandle: 52, AuthenticationToken: token},
		PublishingEnabled: false,
		SubscriptionIDs:   []uint32{subID},
	}.Encode())
	if id != ua.SvcSetPublishingModeResponse {
		t.Fatalf("service = %d: %v", id, decodeFault(t, body))
	}
	resp, err := ua.DecodeSetPublishingModeResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0] != ua.StatusGood {
		t.Fatalf("results = %v", resp.Results)
	}
}

// 关闭会话必须连带删除订阅：否则服务端会为一个已消失的客户端继续采样，
// 现场表现为「服务端会话数只增不减」。
func TestCloseSessionDropsSubscriptions(t *testing.T) {
	srv := newSim(t)
	c := dial(t, srv)
	c.hello(srv)
	c.open()
	token := c.createSession()

	subID := c.createSubscription(token, 100, 5)
	if id, body := c.call(ua.SvcCloseSessionRequest, ua.CloseSessionRequest{
		Header:              ua.RequestHeader{RequestHandle: 60, AuthenticationToken: token},
		DeleteSubscriptions: true,
	}.Encode()); id != ua.SvcCloseSessionResponse {
		t.Fatalf("close session: %v", decodeFault(t, body))
	}

	srv.mu.RLock()
	_, alive := srv.subs[subID]
	srv.mu.RUnlock()
	if alive {
		t.Fatal("subscription survived session close")
	}
}
