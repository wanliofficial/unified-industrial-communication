package opcua

import (
	"fmt"
	"time"
)

// 订阅相关服务标识（Part 6 附录 A 的 *_Encoding_DefaultBinary）。
//
// 这些编号不像 Read/Write 那样「一看就对」：它们集中在 751–850 这一段，
// 每个服务占 6 个号（请求、响应、再加 4 个保留位）。写错一位的表现不是
// 报错，而是服务端返回 BadServiceUnsupported——看上去像「服务器不支持
// 订阅」，实际是客户端把服务号写错了，排查方向完全相反。
const (
	SvcCreateMonitoredItemsRequest  uint32 = 751
	SvcCreateMonitoredItemsResponse uint32 = 754

	SvcDeleteMonitoredItemsRequest  uint32 = 781
	SvcDeleteMonitoredItemsResponse uint32 = 784

	SvcCreateSubscriptionRequest  uint32 = 787
	SvcCreateSubscriptionResponse uint32 = 790

	SvcSetPublishingModeRequest  uint32 = 799
	SvcSetPublishingModeResponse uint32 = 802

	SvcPublishRequest  uint32 = 826
	SvcPublishResponse uint32 = 829

	SvcRepublishRequest  uint32 = 832
	SvcRepublishResponse uint32 = 835

	SvcDeleteSubscriptionsRequest  uint32 = 847
	SvcDeleteSubscriptionsResponse uint32 = 850
)

// 通知体的扩展对象类型。
//
// PublishResponse 里的通知是一个 ExtensionObject 数组：客户端必须按
// TypeId 分派。只认 811（数据变化）与 820（订阅状态变化）两种；别的
// 类型（事件、聚合）跳过而不是整条响应失败——服务端多发一个我们看不懂
// 的通知，不该让整台设备判故障。
const (
	ExtDataChangeNotification   uint32 = 811
	ExtStatusChangeNotification uint32 = 820
)

// 监视模式。Reporting 才会推送；Sampling 只采样不推送（本实现不用）。
const (
	MonitoringModeDisabled  uint32 = 0
	MonitoringModeSampling  uint32 = 1
	MonitoringModeReporting uint32 = 2
)

// StatusGoodSubscriptionTransferred 等订阅相关状态码只影响「是否该重建」，
// 这里只补两个会被 Publish 响应用到的。
const (
	StatusBadNoSubscription         StatusCode = 0x80790000
	StatusBadSequenceNumberUnknown  StatusCode = 0x807A0000
	StatusBadMessageNotAvailable    StatusCode = 0x807B0000
	StatusBadSubscriptionIDInvalid  StatusCode = 0x80280000
	StatusBadMonitoredItemIDInvalid StatusCode = 0x80420000
)

// ---------------------------------------------------------------------------
// 监视项
// ---------------------------------------------------------------------------

// MonitoringParameters 是监视项的采样与排队参数。
//
// QueueSize=1 + DiscardOldest=true 是网关场景下唯一合理的组合：我们只关心
// 「最新值」，排队 10 个历史值只会让 Read 读到几秒前的数据。需要完整历史
// 时应该用历史库，而不是靠订阅队列。
type MonitoringParameters struct {
	ClientHandle     uint32
	SamplingInterval float64 // 毫秒；-1 表示「跟发布周期一致」
	Filter           ExtensionObject
	QueueSize        uint32
	DiscardOldest    bool
}

func (p MonitoringParameters) encode(e *encoder) {
	e.U32(p.ClientHandle)
	e.F64(p.SamplingInterval)
	p.Filter.Encode(e)
	e.U32(p.QueueSize)
	e.Bool(p.DiscardOldest)
}

func (d *decoder) MonitoringParameters() MonitoringParameters {
	var p MonitoringParameters
	p.ClientHandle = d.U32()
	p.SamplingInterval = d.F64()
	p.Filter = d.ExtensionObject()
	p.QueueSize = d.U32()
	p.DiscardOldest = d.Bool()
	return p
}

// MonitoredItemCreateRequest 是「监控这个节点的 Value 属性」。
type MonitoredItemCreateRequest struct {
	ItemToMonitor       ReadValueID
	MonitoringMode      uint32
	RequestedParameters MonitoringParameters
}

func (r MonitoredItemCreateRequest) encode(e *encoder) {
	r.ItemToMonitor.encode(e)
	e.U32(r.MonitoringMode)
	r.RequestedParameters.encode(e)
}

func (d *decoder) MonitoredItemCreateRequest() MonitoredItemCreateRequest {
	var r MonitoredItemCreateRequest
	r.ItemToMonitor = d.ReadValueID()
	r.MonitoringMode = d.U32()
	r.RequestedParameters = d.MonitoringParameters()
	return r
}

// MonitoredItemCreateResult 是逐项结果。部分失败是正常结果（比如某个节点
// 不存在），因此状态码在数组里而不是整条响应上。
type MonitoredItemCreateResult struct {
	StatusCode              StatusCode
	MonitoredItemID         uint32
	RevisedSamplingInterval float64
	RevisedQueueSize        uint32
	FilterResult            ExtensionObject
}

func (r MonitoredItemCreateResult) encode(e *encoder) {
	e.U32(uint32(r.StatusCode))
	e.U32(r.MonitoredItemID)
	e.F64(r.RevisedSamplingInterval)
	e.U32(r.RevisedQueueSize)
	r.FilterResult.Encode(e)
}

func (d *decoder) MonitoredItemCreateResult() MonitoredItemCreateResult {
	var r MonitoredItemCreateResult
	r.StatusCode = StatusCode(d.U32())
	r.MonitoredItemID = d.U32()
	r.RevisedSamplingInterval = d.F64()
	r.RevisedQueueSize = d.U32()
	r.FilterResult = d.ExtensionObject()
	return r
}

// CreateMonitoredItemsRequest 在已有订阅上批量加监视项。
type CreateMonitoredItemsRequest struct {
	Header             RequestHeader
	SubscriptionID     uint32
	TimestampsToReturn uint32
	ItemsToCreate      []MonitoredItemCreateRequest
}

func (r CreateMonitoredItemsRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.SubscriptionID)
	e.U32(r.TimestampsToReturn)
	e.ArrayLength(len(r.ItemsToCreate))
	for _, it := range r.ItemsToCreate {
		it.encode(e)
	}
	return e.Bytes()
}

func DecodeCreateMonitoredItemsRequest(b []byte) (CreateMonitoredItemsRequest, error) {
	d := newDecoder(b)
	var r CreateMonitoredItemsRequest
	r.Header = d.RequestHeader()
	r.SubscriptionID = d.U32()
	r.TimestampsToReturn = d.U32()
	n := d.decodeArrayLen(65535)
	for i := 0; i < n; i++ {
		r.ItemsToCreate = append(r.ItemsToCreate, d.MonitoredItemCreateRequest())
	}
	return r, d.Err()
}

// CreateMonitoredItemsResponse 与请求逐项对应。
type CreateMonitoredItemsResponse struct {
	Header  ResponseHeader
	Results []MonitoredItemCreateResult
}

func (r CreateMonitoredItemsResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Results))
	for _, it := range r.Results {
		it.encode(e)
	}
	e.ArrayLength(0) // diagnosticInfos
	return e.Bytes()
}

func DecodeCreateMonitoredItemsResponse(b []byte) (CreateMonitoredItemsResponse, error) {
	d := newDecoder(b)
	var r CreateMonitoredItemsResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(65535)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, d.MonitoredItemCreateResult())
	}
	m := d.decodeArrayLen(65535)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// DeleteMonitoredItemsRequest 删除监视项。
type DeleteMonitoredItemsRequest struct {
	Header           RequestHeader
	SubscriptionID   uint32
	MonitoredItemIDs []uint32
}

func (r DeleteMonitoredItemsRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.SubscriptionID)
	e.ArrayLength(len(r.MonitoredItemIDs))
	for _, id := range r.MonitoredItemIDs {
		e.U32(id)
	}
	return e.Bytes()
}

func DecodeDeleteMonitoredItemsRequest(b []byte) (DeleteMonitoredItemsRequest, error) {
	d := newDecoder(b)
	var r DeleteMonitoredItemsRequest
	r.Header = d.RequestHeader()
	r.SubscriptionID = d.U32()
	n := d.decodeArrayLen(65535)
	for i := 0; i < n; i++ {
		r.MonitoredItemIDs = append(r.MonitoredItemIDs, d.U32())
	}
	return r, d.Err()
}

// DeleteMonitoredItemsResponse 是逐项状态码。
type DeleteMonitoredItemsResponse struct {
	Header  ResponseHeader
	Results []StatusCode
}

func (r DeleteMonitoredItemsResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Results))
	for _, s := range r.Results {
		e.U32(uint32(s))
	}
	e.ArrayLength(0)
	return e.Bytes()
}

func DecodeDeleteMonitoredItemsResponse(b []byte) (DeleteMonitoredItemsResponse, error) {
	d := newDecoder(b)
	var r DeleteMonitoredItemsResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(65535)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, StatusCode(d.U32()))
	}
	m := d.decodeArrayLen(65535)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// ---------------------------------------------------------------------------
// 订阅
// ---------------------------------------------------------------------------

// CreateSubscriptionRequest 建立订阅。
//
// 三个参数的关系是新手最容易配错的：服务端在 MaxKeepAliveCount 个发布周期
// 内没有通知可发时，会发一条空 PublishResponse 保活。若 KeepAlive 太短，
// 空闲时保活报文会把带宽占满；太长，客户端无法区分「空闲」与「断链」。
// 10 是规范推荐的默认值，也是本实现的取值。
type CreateSubscriptionRequest struct {
	Header                      RequestHeader
	RequestedPublishingInterval float64 // 毫秒
	RequestedLifetimeCount      uint32
	RequestedMaxKeepAliveCount  uint32
	MaxNotificationsPerPublish  uint32
	PublishingEnabled           bool
	Priority                    byte
}

func (r CreateSubscriptionRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.F64(r.RequestedPublishingInterval)
	e.U32(r.RequestedLifetimeCount)
	e.U32(r.RequestedMaxKeepAliveCount)
	e.U32(r.MaxNotificationsPerPublish)
	e.Bool(r.PublishingEnabled)
	e.Byte(r.Priority)
	return e.Bytes()
}

func DecodeCreateSubscriptionRequest(b []byte) (CreateSubscriptionRequest, error) {
	d := newDecoder(b)
	var r CreateSubscriptionRequest
	r.Header = d.RequestHeader()
	r.RequestedPublishingInterval = d.F64()
	r.RequestedLifetimeCount = d.U32()
	r.RequestedMaxKeepAliveCount = d.U32()
	r.MaxNotificationsPerPublish = d.U32()
	r.PublishingEnabled = d.Bool()
	r.Priority = d.Byte()
	return r, d.Err()
}

// CreateSubscriptionResponse 返回服务端「修订后」的参数。
//
// 必须用修订值而不是请求值：服务端有权把 100ms 改成 500ms。拿请求值去算
// 保活超时，会在「服务端不支持高频」时把健康的订阅误判成断链。
type CreateSubscriptionResponse struct {
	Header                    ResponseHeader
	SubscriptionID            uint32
	RevisedPublishingInterval float64
	RevisedLifetimeCount      uint32
	RevisedMaxKeepAliveCount  uint32
}

func (r CreateSubscriptionResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.SubscriptionID)
	e.F64(r.RevisedPublishingInterval)
	e.U32(r.RevisedLifetimeCount)
	e.U32(r.RevisedMaxKeepAliveCount)
	return e.Bytes()
}

func DecodeCreateSubscriptionResponse(b []byte) (CreateSubscriptionResponse, error) {
	d := newDecoder(b)
	var r CreateSubscriptionResponse
	r.Header = d.ResponseHeader()
	r.SubscriptionID = d.U32()
	r.RevisedPublishingInterval = d.F64()
	r.RevisedLifetimeCount = d.U32()
	r.RevisedMaxKeepAliveCount = d.U32()
	return r, d.Err()
}

// SetPublishingModeRequest 暂停/恢复推送。
type SetPublishingModeRequest struct {
	Header            RequestHeader
	PublishingEnabled bool
	SubscriptionIDs   []uint32
}

func (r SetPublishingModeRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.Bool(r.PublishingEnabled)
	e.ArrayLength(len(r.SubscriptionIDs))
	for _, id := range r.SubscriptionIDs {
		e.U32(id)
	}
	return e.Bytes()
}

func DecodeSetPublishingModeRequest(b []byte) (SetPublishingModeRequest, error) {
	d := newDecoder(b)
	var r SetPublishingModeRequest
	r.Header = d.RequestHeader()
	r.PublishingEnabled = d.Bool()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.SubscriptionIDs = append(r.SubscriptionIDs, d.U32())
	}
	return r, d.Err()
}

// SetPublishingModeResponse 是逐订阅状态码。
type SetPublishingModeResponse struct {
	Header  ResponseHeader
	Results []StatusCode
}

func (r SetPublishingModeResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Results))
	for _, s := range r.Results {
		e.U32(uint32(s))
	}
	e.ArrayLength(0)
	return e.Bytes()
}

func DecodeSetPublishingModeResponse(b []byte) (SetPublishingModeResponse, error) {
	d := newDecoder(b)
	var r SetPublishingModeResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, StatusCode(d.U32()))
	}
	m := d.decodeArrayLen(4096)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// DeleteSubscriptionsRequest 删除订阅（监视项随之释放）。
type DeleteSubscriptionsRequest struct {
	Header          RequestHeader
	SubscriptionIDs []uint32
}

func (r DeleteSubscriptionsRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.SubscriptionIDs))
	for _, id := range r.SubscriptionIDs {
		e.U32(id)
	}
	return e.Bytes()
}

func DecodeDeleteSubscriptionsRequest(b []byte) (DeleteSubscriptionsRequest, error) {
	d := newDecoder(b)
	var r DeleteSubscriptionsRequest
	r.Header = d.RequestHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.SubscriptionIDs = append(r.SubscriptionIDs, d.U32())
	}
	return r, d.Err()
}

// DeleteSubscriptionsResponse 是逐订阅状态码。
type DeleteSubscriptionsResponse struct {
	Header  ResponseHeader
	Results []StatusCode
}

func (r DeleteSubscriptionsResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.Results))
	for _, s := range r.Results {
		e.U32(uint32(s))
	}
	e.ArrayLength(0)
	return e.Bytes()
}

func DecodeDeleteSubscriptionsResponse(b []byte) (DeleteSubscriptionsResponse, error) {
	d := newDecoder(b)
	var r DeleteSubscriptionsResponse
	r.Header = d.ResponseHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.Results = append(r.Results, StatusCode(d.U32()))
	}
	m := d.decodeArrayLen(4096)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// ---------------------------------------------------------------------------
// 发布
// ---------------------------------------------------------------------------

// SubscriptionAcknowledgement 确认「这条通知已处理」。
//
// 不确认的后果不是丢数据，而是服务端会把通知留在重传队列里：队列满了以后
// 开始丢新的通知，表现为「跑一段时间后数据不再更新，重连就好」。所以每个
// 带 sequenceNumber 的通知都必须确认。
type SubscriptionAcknowledgement struct {
	SubscriptionID uint32
	SequenceNumber uint32
}

func (a SubscriptionAcknowledgement) encode(e *encoder) {
	e.U32(a.SubscriptionID)
	e.U32(a.SequenceNumber)
}

func (d *decoder) SubscriptionAcknowledgement() SubscriptionAcknowledgement {
	return SubscriptionAcknowledgement{SubscriptionID: d.U32(), SequenceNumber: d.U32()}
}

// PublishRequest 是「有通知就发给我，没有就等保活」的长轮询。
//
// 它必须带上一批待确认的序列号：OPC UA 的确认是「下一次请求顺带带上」，
// 没有独立的 Ack 服务。这也是为什么 Publish 循环不能停——停了以后确认
// 发不出去，服务端的重传队列会被填满。
type PublishRequest struct {
	Header                       RequestHeader
	SubscriptionAcknowledgements []SubscriptionAcknowledgement
}

func (r PublishRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.ArrayLength(len(r.SubscriptionAcknowledgements))
	for _, a := range r.SubscriptionAcknowledgements {
		a.encode(e)
	}
	return e.Bytes()
}

func DecodePublishRequest(b []byte) (PublishRequest, error) {
	d := newDecoder(b)
	var r PublishRequest
	r.Header = d.RequestHeader()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.SubscriptionAcknowledgements = append(r.SubscriptionAcknowledgements, d.SubscriptionAcknowledgement())
	}
	return r, d.Err()
}

// NotificationMessage 是一次发布携带的通知集合。
type NotificationMessage struct {
	SequenceNumber   uint32
	PublishTime      time.Time
	NotificationData []ExtensionObject
}

func (m NotificationMessage) encode(e *encoder) {
	e.U32(m.SequenceNumber)
	e.Time(m.PublishTime)
	e.ArrayLength(len(m.NotificationData))
	for _, x := range m.NotificationData {
		x.Encode(e)
	}
}

func (d *decoder) NotificationMessage() NotificationMessage {
	var m NotificationMessage
	m.SequenceNumber = d.U32()
	m.PublishTime = d.Time()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		m.NotificationData = append(m.NotificationData, d.ExtensionObject())
	}
	return m
}

// MonitoredItemNotification 是「某个被监视的节点变了」。
type MonitoredItemNotification struct {
	ClientHandle uint32
	Value        DataValue
}

func (n MonitoredItemNotification) encode(e *encoder) {
	e.U32(n.ClientHandle)
	n.Value.Encode(e)
}

func (d *decoder) MonitoredItemNotification() MonitoredItemNotification {
	return MonitoredItemNotification{ClientHandle: d.U32(), Value: d.DataValue()}
}

// DataChangeNotification 是一批数据变化。
type DataChangeNotification struct {
	MonitoredItems  []MonitoredItemNotification
	DiagnosticInfos []string
}

func (n DataChangeNotification) Encode() []byte {
	e := &encoder{}
	e.ArrayLength(len(n.MonitoredItems))
	for _, it := range n.MonitoredItems {
		it.encode(e)
	}
	e.ArrayLength(0) // diagnosticInfos
	return e.Bytes()
}

func DecodeDataChangeNotification(b []byte) (DataChangeNotification, error) {
	d := newDecoder(b)
	var n DataChangeNotification
	cnt := d.decodeArrayLen(65535)
	for i := 0; i < cnt; i++ {
		n.MonitoredItems = append(n.MonitoredItems, d.MonitoredItemNotification())
	}
	m := d.decodeArrayLen(65535)
	for i := 0; i < m; i++ {
		d.diagnosticInfo(0)
	}
	return n, d.Err()
}

// StatusChangeNotification 表示订阅本身的状态变了（超时、传输失败、
// 被服务端关闭）。收到它必须立刻把订阅判为不可用：继续相信本地表里的
// 旧值，就是拿「最后一次成功的读数」冒充「当前值」。
type StatusChangeNotification struct {
	Status StatusCode
}

func (n StatusChangeNotification) Encode() []byte {
	e := &encoder{}
	e.U32(uint32(n.Status))
	ExtensionObject{}.Encode(e) // diagnosticInfo：空
	return e.Bytes()
}

func DecodeStatusChangeNotification(b []byte) (StatusChangeNotification, error) {
	d := newDecoder(b)
	var n StatusChangeNotification
	n.Status = StatusCode(d.U32())
	d.diagnosticInfo(0)
	return n, d.Err()
}

// PublishResponse 是一条发布结果。
type PublishResponse struct {
	Header                   ResponseHeader
	SubscriptionID           uint32
	AvailableSequenceNumbers []uint32
	MoreNotifications        bool
	NotificationMessage      NotificationMessage
	Results                  []StatusCode
}

func (r PublishResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.SubscriptionID)
	e.ArrayLength(len(r.AvailableSequenceNumbers))
	for _, s := range r.AvailableSequenceNumbers {
		e.U32(s)
	}
	e.Bool(r.MoreNotifications)
	r.NotificationMessage.encode(e)
	e.ArrayLength(len(r.Results))
	for _, s := range r.Results {
		e.U32(uint32(s))
	}
	e.ArrayLength(0) // diagnosticInfos
	return e.Bytes()
}

func DecodePublishResponse(b []byte) (PublishResponse, error) {
	d := newDecoder(b)
	var r PublishResponse
	r.Header = d.ResponseHeader()
	r.SubscriptionID = d.U32()
	n := d.decodeArrayLen(4096)
	for i := 0; i < n; i++ {
		r.AvailableSequenceNumbers = append(r.AvailableSequenceNumbers, d.U32())
	}
	r.MoreNotifications = d.Bool()
	r.NotificationMessage = d.NotificationMessage()
	m := d.decodeArrayLen(4096)
	for i := 0; i < m; i++ {
		r.Results = append(r.Results, StatusCode(d.U32()))
	}
	k := d.decodeArrayLen(4096)
	for i := 0; i < k; i++ {
		d.diagnosticInfo(0)
	}
	return r, d.Err()
}

// RepublishRequest 重传某条丢失的通知。
type RepublishRequest struct {
	Header                   RequestHeader
	SubscriptionID           uint32
	RetransmitSequenceNumber uint32
}

func (r RepublishRequest) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	e.U32(r.SubscriptionID)
	e.U32(r.RetransmitSequenceNumber)
	return e.Bytes()
}

func DecodeRepublishRequest(b []byte) (RepublishRequest, error) {
	d := newDecoder(b)
	var r RepublishRequest
	r.Header = d.RequestHeader()
	r.SubscriptionID = d.U32()
	r.RetransmitSequenceNumber = d.U32()
	return r, d.Err()
}

// RepublishResponse 返回被重传的通知。
type RepublishResponse struct {
	Header              ResponseHeader
	NotificationMessage NotificationMessage
}

func (r RepublishResponse) Encode() []byte {
	e := &encoder{}
	r.Header.encode(e)
	r.NotificationMessage.encode(e)
	return e.Bytes()
}

func DecodeRepublishResponse(b []byte) (RepublishResponse, error) {
	d := newDecoder(b)
	var r RepublishResponse
	r.Header = d.ResponseHeader()
	r.NotificationMessage = d.NotificationMessage()
	return r, d.Err()
}

// DecodeNotificationData 按 TypeId 分派一条通知。
//
// 未知类型返回 (nil, nil)：跳过而不是报错。服务端多发一个事件通知，
// 不应该让整台设备判故障。
func DecodeNotificationData(x ExtensionObject) (*DataChangeNotification, *StatusChangeNotification, error) {
	if x.Encoding != 0x01 {
		return nil, nil, nil
	}
	switch x.TypeID.ID {
	case ExtDataChangeNotification:
		n, err := DecodeDataChangeNotification(x.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("opcua: data change notification: %w", err)
		}
		return &n, nil, nil
	case ExtStatusChangeNotification:
		n, err := DecodeStatusChangeNotification(x.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("opcua: status change notification: %w", err)
		}
		return nil, &n, nil
	}
	return nil, nil, nil
}
