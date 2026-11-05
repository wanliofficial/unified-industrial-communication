package opcua

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"uic/internal/model"
)

// 这里的断言重点同样是「字节布局」：订阅服务的参数顺序一旦写反，
// 服务端不会报错，而是按错位后的字节解释——比如把 QueueSize 当成
// DiscardOldest，表现是「订阅建起来了但没有通知」。这类错误只能靠
// 逐字段比对发现。

func TestCreateSubscriptionRequestLayout(t *testing.T) {
	req := CreateSubscriptionRequest{
		Header: RequestHeader{
			AuthenticationToken: Numeric(1, 7),
			Timestamp:           time.Unix(0, 0).UTC(),
			RequestHandle:       0xAABBCCDD,
		},
		RequestedPublishingInterval: 250,
		RequestedLifetimeCount:      30,
		RequestedMaxKeepAliveCount:  10,
		MaxNotificationsPerPublish:  0,
		PublishingEnabled:           true,
		Priority:                    1,
	}
	b := req.Encode()

	// 尾部 22 字节是订阅参数本身，前面的请求头不在这里重复断言。
	if len(b) < 22 {
		t.Fatalf("encoded %d bytes", len(b))
	}
	tail := b[len(b)-22:]
	if got := math.Float64frombits(binary.LittleEndian.Uint64(tail[0:8])); got != 250 {
		t.Fatalf("publishing interval = %v", got)
	}
	if got := binary.LittleEndian.Uint32(tail[8:12]); got != 30 {
		t.Fatalf("lifetime = %d", got)
	}
	if got := binary.LittleEndian.Uint32(tail[12:16]); got != 10 {
		t.Fatalf("keepalive = %d", got)
	}
	if got := binary.LittleEndian.Uint32(tail[16:20]); got != 0 {
		t.Fatalf("max notifications = %d", got)
	}
	if tail[20] != 0x01 {
		t.Fatalf("publishing enabled = %#x", tail[20])
	}
	if tail[21] != 1 {
		t.Fatalf("priority = %d", tail[21])
	}

	dec, err := DecodeCreateSubscriptionRequest(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.RequestedPublishingInterval != 250 || dec.RequestedLifetimeCount != 30 ||
		dec.RequestedMaxKeepAliveCount != 10 || !dec.PublishingEnabled || dec.Priority != 1 {
		t.Fatalf("round trip = %+v", dec)
	}
}

func TestCreateSubscriptionResponseRoundTrip(t *testing.T) {
	in := CreateSubscriptionResponse{
		Header:                    ResponseHeader{RequestHandle: 3, ServiceResult: StatusGood},
		SubscriptionID:            42,
		RevisedPublishingInterval: 500,
		RevisedLifetimeCount:      60,
		RevisedMaxKeepAliveCount:  20,
	}
	got, err := DecodeCreateSubscriptionResponse(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubscriptionID != 42 || got.RevisedPublishingInterval != 500 ||
		got.RevisedLifetimeCount != 60 || got.RevisedMaxKeepAliveCount != 20 {
		t.Fatalf("round trip = %+v", got)
	}
	if got.Header.RequestHandle != 3 {
		t.Fatalf("handle = %d", got.Header.RequestHandle)
	}
}

// 监视项的参数顺序是最容易写反的一段：ClientHandle、SamplingInterval、
// Filter（扩展对象）、QueueSize、DiscardOldest。Filter 是个可变长对象，
// 少了它的长度前缀，后面所有字段都会错位。
func TestMonitoredItemParametersRoundTrip(t *testing.T) {
	in := MonitoredItemCreateRequest{
		ItemToMonitor:  ReadValueID{NodeID: StringNode(2, "Plant.Temperature"), AttributeID: AttrValue},
		MonitoringMode: MonitoringModeReporting,
		RequestedParameters: MonitoringParameters{
			ClientHandle:     9,
			SamplingInterval: 100,
			QueueSize:        1,
			DiscardOldest:    true,
		},
	}
	req := CreateMonitoredItemsRequest{
		Header:             RequestHeader{RequestHandle: 5},
		SubscriptionID:     42,
		TimestampsToReturn: TimestampsBoth,
		ItemsToCreate:      []MonitoredItemCreateRequest{in},
	}
	got, err := DecodeCreateMonitoredItemsRequest(req.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubscriptionID != 42 || got.TimestampsToReturn != TimestampsBoth {
		t.Fatalf("header = %+v", got)
	}
	if len(got.ItemsToCreate) != 1 {
		t.Fatalf("items = %d", len(got.ItemsToCreate))
	}
	it := got.ItemsToCreate[0]
	if it.ItemToMonitor.NodeID != StringNode(2, "Plant.Temperature") || it.ItemToMonitor.AttributeID != AttrValue {
		t.Fatalf("item to monitor = %+v", it.ItemToMonitor)
	}
	if it.MonitoringMode != MonitoringModeReporting {
		t.Fatalf("mode = %d", it.MonitoringMode)
	}
	p := it.RequestedParameters
	if p.ClientHandle != 9 || p.SamplingInterval != 100 || p.QueueSize != 1 || !p.DiscardOldest {
		t.Fatalf("parameters = %+v", p)
	}
}

func TestCreateMonitoredItemsResponseRoundTrip(t *testing.T) {
	in := CreateMonitoredItemsResponse{
		Header: ResponseHeader{RequestHandle: 5, ServiceResult: StatusGood},
		Results: []MonitoredItemCreateResult{
			{StatusCode: StatusGood, MonitoredItemID: 1, RevisedSamplingInterval: 100, RevisedQueueSize: 1},
			{StatusCode: StatusBadNodeIDUnknown},
		},
	}
	got, err := DecodeCreateMonitoredItemsResponse(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d", len(got.Results))
	}
	if got.Results[0].MonitoredItemID != 1 || got.Results[0].StatusCode != StatusGood {
		t.Fatalf("first = %+v", got.Results[0])
	}
	if got.Results[1].StatusCode != StatusBadNodeIDUnknown {
		t.Fatalf("second = %+v", got.Results[1])
	}
}

// 确认数组必须紧跟在请求头之后，且可以为空。确认丢了不会立刻报错，
// 但服务端的重传队列会被填满——这是「跑一阵就停」的根因。
func TestPublishRequestAcknowledgements(t *testing.T) {
	in := PublishRequest{
		Header: RequestHeader{RequestHandle: 11},
		SubscriptionAcknowledgements: []SubscriptionAcknowledgement{
			{SubscriptionID: 42, SequenceNumber: 7},
			{SubscriptionID: 42, SequenceNumber: 8},
		},
	}
	got, err := DecodePublishRequest(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.SubscriptionAcknowledgements) != 2 {
		t.Fatalf("acks = %d", len(got.SubscriptionAcknowledgements))
	}
	if got.SubscriptionAcknowledgements[1].SequenceNumber != 8 {
		t.Fatalf("ack = %+v", got.SubscriptionAcknowledgements[1])
	}

	empty := PublishRequest{Header: RequestHeader{RequestHandle: 12}}
	got, err = DecodePublishRequest(empty.Encode())
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(got.SubscriptionAcknowledgements) != 0 {
		t.Fatalf("empty acks = %d", len(got.SubscriptionAcknowledgements))
	}
}

func TestPublishResponseRoundTrip(t *testing.T) {
	dv := Variant{Type: TypeDouble, Float: 21.5}
	msg := NotificationMessage{
		SequenceNumber: 7,
		PublishTime:    time.Unix(1700000000, 0).UTC(),
		NotificationData: []ExtensionObject{NewExtensionObject(ExtDataChangeNotification,
			DataChangeNotification{MonitoredItems: []MonitoredItemNotification{
				{ClientHandle: 3, Value: DataValue{Value: &dv, SourceTimestamp: time.Unix(1700000000, 0).UTC()}},
			}}.Encode())},
	}
	in := PublishResponse{
		Header:                   ResponseHeader{RequestHandle: 11, ServiceResult: StatusGood},
		SubscriptionID:           42,
		AvailableSequenceNumbers: []uint32{6, 7},
		MoreNotifications:        true,
		NotificationMessage:      msg,
		Results:                  []StatusCode{StatusGood},
	}
	got, err := DecodePublishResponse(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubscriptionID != 42 || !got.MoreNotifications {
		t.Fatalf("header = %+v", got)
	}
	if len(got.AvailableSequenceNumbers) != 2 || got.AvailableSequenceNumbers[1] != 7 {
		t.Fatalf("available = %v", got.AvailableSequenceNumbers)
	}
	if got.NotificationMessage.SequenceNumber != 7 {
		t.Fatalf("sequence = %d", got.NotificationMessage.SequenceNumber)
	}
	if len(got.Results) != 1 || got.Results[0] != StatusGood {
		t.Fatalf("results = %v", got.Results)
	}

	dc, sc, err := DecodeNotificationData(got.NotificationMessage.NotificationData[0])
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if sc != nil {
		t.Fatalf("unexpected status change %+v", sc)
	}
	if dc == nil || len(dc.MonitoredItems) != 1 {
		t.Fatalf("data change = %+v", dc)
	}
	mi := dc.MonitoredItems[0]
	if mi.ClientHandle != 3 || mi.Value.Value == nil || mi.Value.Value.Float != 21.5 {
		t.Fatalf("item = %+v", mi)
	}
}

func TestStatusChangeNotificationRoundTrip(t *testing.T) {
	in := StatusChangeNotification{Status: StatusBadTimeout}
	msg, err := DecodeStatusChangeNotification(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Status != StatusBadTimeout {
		t.Fatalf("status = %v", msg.Status)
	}
}

// 未知通知类型必须跳过而不是报错：服务端多发一条事件通知，
// 不该让整台设备判故障。
func TestDecodeNotificationDataSkipsUnknown(t *testing.T) {
	dc, sc, err := DecodeNotificationData(NewExtensionObject(9999, []byte{1, 2, 3}))
	if err != nil || dc != nil || sc != nil {
		t.Fatalf("got %v %v %v", dc, sc, err)
	}
	// 非二进制编码（比如 XML）同样跳过。
	dc, sc, err = DecodeNotificationData(ExtensionObject{TypeID: Numeric(0, ExtDataChangeNotification), Encoding: 0x02})
	if err != nil || dc != nil || sc != nil {
		t.Fatalf("xml: got %v %v %v", dc, sc, err)
	}
}

func TestDeleteSubscriptionsRoundTrip(t *testing.T) {
	req := DeleteSubscriptionsRequest{Header: RequestHeader{RequestHandle: 1}, SubscriptionIDs: []uint32{4, 5}}
	got, err := DecodeDeleteSubscriptionsRequest(req.Encode())
	if err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(got.SubscriptionIDs) != 2 || got.SubscriptionIDs[1] != 5 {
		t.Fatalf("ids = %v", got.SubscriptionIDs)
	}

	resp := DeleteSubscriptionsResponse{
		Header:  ResponseHeader{RequestHandle: 1},
		Results: []StatusCode{StatusGood, StatusBadNoSubscription},
	}
	gotResp, err := DecodeDeleteSubscriptionsResponse(resp.Encode())
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(gotResp.Results) != 2 || gotResp.Results[1] != StatusBadNoSubscription {
		t.Fatalf("results = %v", gotResp.Results)
	}
}

func TestDeleteMonitoredItemsRoundTrip(t *testing.T) {
	req := DeleteMonitoredItemsRequest{
		Header: RequestHeader{RequestHandle: 1}, SubscriptionID: 42, MonitoredItemIDs: []uint32{1, 2, 3},
	}
	got, err := DecodeDeleteMonitoredItemsRequest(req.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubscriptionID != 42 || len(got.MonitoredItemIDs) != 3 {
		t.Fatalf("got %+v", got)
	}
}

// SetPublishingMode 的布尔字段在数组之前，写反会让服务端把订阅 ID
// 当成开关值。这个服务本实现没用到，但编解码必须对。
func TestSetPublishingModeRoundTrip(t *testing.T) {
	req := SetPublishingModeRequest{Header: RequestHeader{RequestHandle: 1}, PublishingEnabled: true, SubscriptionIDs: []uint32{42}}
	got, err := DecodeSetPublishingModeRequest(req.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.PublishingEnabled || len(got.SubscriptionIDs) != 1 || got.SubscriptionIDs[0] != 42 {
		t.Fatalf("got %+v", got)
	}
}

// QualityOf 必须能区分订阅相关的两个高频状态：超时与断链都是通信问题，
// 而「节点不存在」是配置问题。混为一谈会让一个配错的点位熔断整台设备。
func TestQualityOfSubscriptionStatuses(t *testing.T) {
	cases := []struct {
		s    StatusCode
		want model.Quality
	}{
		{StatusGood, model.QualityGood},
		{StatusBadNoCommunication, model.QualityCommError},
		{StatusBadNodeIDUnknown, model.QualityAddressError},
		{StatusBadWaitingForData, model.QualityStale},
		{StatusBadNotWritable, model.QualityBad},
	}
	for _, c := range cases {
		if got := QualityOf(c.s); got != c.want {
			t.Fatalf("%v: got %s want %s", c.s, got, c.want)
		}
	}
}

func TestDeleteMonitoredItemsResponseRoundTrip(t *testing.T) {
	in := DeleteMonitoredItemsResponse{
		Header:  ResponseHeader{RequestHandle: 1, ServiceResult: StatusGood},
		Results: []StatusCode{StatusGood, StatusBadMonitoredItemIDInvalid},
	}
	got, err := DecodeDeleteMonitoredItemsResponse(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 2 || got.Results[1] != StatusBadMonitoredItemIDInvalid {
		t.Fatalf("results = %v", got.Results)
	}
}

func TestSetPublishingModeResponseRoundTrip(t *testing.T) {
	in := SetPublishingModeResponse{
		Header:  ResponseHeader{RequestHandle: 1, ServiceResult: StatusGood},
		Results: []StatusCode{StatusGood},
	}
	got, err := DecodeSetPublishingModeResponse(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0] != StatusGood {
		t.Fatalf("results = %v", got.Results)
	}
}

// 重传是客户端发现序列号跳号时的补救手段。本实现不主动用，但编解码
// 必须对——否则遇到会跳号的服务端时无法补取，只能整条订阅重建。
func TestRepublishRoundTrip(t *testing.T) {
	req := RepublishRequest{
		Header: RequestHeader{RequestHandle: 1}, SubscriptionID: 42, RetransmitSequenceNumber: 9,
	}
	gotReq, err := DecodeRepublishRequest(req.Encode())
	if err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if gotReq.SubscriptionID != 42 || gotReq.RetransmitSequenceNumber != 9 {
		t.Fatalf("request = %+v", gotReq)
	}

	dv := Variant{Type: TypeInt32, Int: 7}
	resp := RepublishResponse{
		Header: ResponseHeader{RequestHandle: 1, ServiceResult: StatusGood},
		NotificationMessage: NotificationMessage{
			SequenceNumber: 9,
			PublishTime:    time.Unix(1700000000, 0).UTC(),
			NotificationData: []ExtensionObject{NewExtensionObject(ExtDataChangeNotification,
				DataChangeNotification{MonitoredItems: []MonitoredItemNotification{
					{ClientHandle: 1, Value: DataValue{Value: &dv}},
				}}.Encode())},
		},
	}
	gotResp, err := DecodeRepublishResponse(resp.Encode())
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if gotResp.NotificationMessage.SequenceNumber != 9 || len(gotResp.NotificationMessage.NotificationData) != 1 {
		t.Fatalf("response = %+v", gotResp)
	}
}
