package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	codec "uic/internal/codec/sparkplug"
	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/mqtt/mqtttest"
	"uic/internal/outbox"
	"uic/internal/scheduler"
)

// withSparkplug 把北向配置成 Sparkplug B。
func withSparkplug(url string, mutate func(map[string]any)) func(map[string]any) {
	return func(cfg map[string]any) {
		cfg["storage"] = map[string]any{
			"dir": cfg["storage"].(map[string]any)["dir"], "journal": false,
		}
		m := map[string]any{
			"enabled": true, "broker": url,
			"format": "sparkplug-b", "groupId": "plant-a", "nodeId": "gw-01",
		}
		if mutate != nil {
			mutate(m)
		}
		cfg["northbound"] = map[string]any{"mqtt": m}
	}
}

// waitTopic 等到指定主题的消息出现，返回最后一条。
func waitTopic(t *testing.T, b *mqtttest.Broker, topic string) mqtttest.Message {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last mqtttest.Message
	for time.Now().Before(deadline) {
		for _, m := range b.Messages() {
			if m.Topic == topic {
				last = m
			}
		}
		if last.Topic != "" {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no message on %s; got topics %v", topic, topicsOf(b.Messages()))
	return last
}

func topicsOf(msgs []mqtttest.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Topic)
	}
	return out
}

func countTopics(msgs []mqtttest.Message, topic string) int {
	n := 0
	for _, m := range msgs {
		if m.Topic == topic {
			n++
		}
	}
	return n
}

func decodeMsg(t *testing.T, m mqtttest.Message) codec.Payload {
	t.Helper()
	p, err := codec.Decode(m.Payload)
	if err != nil {
		t.Fatalf("%s: %v", m.Topic, err)
	}
	return p
}

// 端到端：采集 → Outbox → Sparkplug。顺序必须是
// NBIRTH → DBIRTH → DDATA，序号在同一设备内连续。
//
// 本实现只扮演 Edge Node，不发 STATE：STATE 是 Host 的出生/死亡证书
// （spBv1.0/STATE/{host_id}），边缘节点发它就是冒用身份。在线语义由
// NBIRTH 表达，离线由 NDEATH 表达。
func TestSparkplugBirthOrderAndData(t *testing.T) {
	b := mqtttest.New(t, nil)
	h := newHarness(t, withSparkplug(b.URL(), nil))
	h.waitPointGood("sim01", "reg0")

	subs := b.WaitSubscriptions(t, 2)
	want := map[string]bool{
		"spBv1.0/plant-a/NCMD/gw-01":   false,
		"spBv1.0/plant-a/DCMD/gw-01/+": false,
	}
	for _, s := range subs {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Fatalf("missing subscription %s in %v", s, subs)
		}
	}

	// 每类消息的 QoS 与 retain 由协议定死。从 Broker 侧断言，才能证明
	// 「线上字节」符合规范，而不只是本地对象符合。
	for _, m := range b.WaitMessages(t, 1) {
		switch {
		case strings.HasPrefix(m.Topic, "spBv1.0/plant-a/NBIRTH/"),
			strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DBIRTH/"),
			strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DDATA/"),
			strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DDEATH/"):
			if m.QoS != 0 || m.Retain {
				t.Fatalf("%s qos=%d retain=%v; edge node data/birth messages MUST be qos0 and non-retained",
					m.Topic, m.QoS, m.Retain)
			}
		}
	}

	birth := waitTopic(t, b, "spBv1.0/plant-a/NBIRTH/gw-01")
	p := decodeMsg(t, birth)
	if p.Seq != 0 {
		t.Fatalf("NBIRTH seq = %d", p.Seq)
	}
	if len(p.Metrics) < 2 || p.Metrics[0].Name != "bdSeq" {
		t.Fatalf("NBIRTH metrics = %+v", p.Metrics)
	}
	// NBIRTH 的 bdSeq 必须与 CONNECT 遗嘱里的完全一致，Host 靠它把一对
	// NDEATH/NBIRTH 配对。
	info := b.WaitConnections(t, 1)[0]
	if !info.HasWill {
		t.Fatal("CONNECT carries no will; a Sparkplug edge node MUST register NDEATH as its LWT")
	}
	willSeq := decodeMsg(t, mqtttest.Message{Topic: info.Will.Topic, Payload: info.Will.Payload})
	if len(willSeq.Metrics) != 1 || willSeq.Metrics[0].Name != "bdSeq" {
		t.Fatalf("NDEATH will metrics = %+v", willSeq.Metrics)
	}
	if bd, ok := p.Metrics[0].Value.(int64); !ok || bd != willSeq.Metrics[0].Value.(int64) {
		t.Fatalf("NBIRTH bdSeq = %v, will bdSeq = %v", p.Metrics[0].Value, willSeq.Metrics[0].Value)
	}
	var rebirth bool
	for _, m := range p.Metrics {
		if m.Name == "Node Control/Rebirth" {
			rebirth = true
		}
	}
	if !rebirth {
		t.Fatal("NBIRTH must declare Node Control/Rebirth")
	}

	data := waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/sim01")
	dp := decodeMsg(t, data)
	if len(dp.Metrics) != 1 || dp.Metrics[0].Name == "" {
		t.Fatalf("DDATA metrics = %+v", dp.Metrics)
	}

	// DBIRTH 是设备的完整点位表，不能只带第一个点。等所有点位都出现过。
	deadline := time.Now().Add(10 * time.Second)
	var dbirth codec.Payload
	for time.Now().Before(deadline) {
		for _, m := range b.Messages() {
			if m.Topic != "spBv1.0/plant-a/DBIRTH/gw-01/sim01" {
				continue
			}
			p := decodeMsg(t, m)
			if len(p.Metrics) > len(dbirth.Metrics) {
				dbirth = p
			}
		}
		if len(dbirth.Metrics) >= 8 { // Device Control/Rebirth + 7 点
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(dbirth.Metrics) < 8 {
		t.Fatalf("DBIRTH metrics = %d, want the whole device point table", len(dbirth.Metrics))
	}
	if dbirth.Metrics[0].Name != "Device Control/Rebirth" {
		t.Fatalf("DBIRTH first metric = %q", dbirth.Metrics[0].Name)
	}
}

// 序号是边缘节点级的：两台设备交替发布时，线上序号必须连续递增，而不是
// 各自从 0 开始（那会得到 0,1,0,1）。严格的主机把「序号不连续」当成丢包，
// 会请求重生或丢弃数据，而发送方完全看不出问题。
func TestSparkplugSeqIsNodeWide(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	deliverSample(t, s, "dev2", "p1", 2)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev2")

	// 按发布顺序取出所有带序号的消息。NDEATH 规范要求不带序号，跳过它。
	var kinds []string
	var seqs []uint64
	for _, m := range b.Messages() {
		switch {
		case strings.HasPrefix(m.Topic, "spBv1.0/plant-a/NBIRTH/"),
			strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DBIRTH/"),
			strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DDATA/"),
			strings.HasPrefix(m.Topic, "spBv1.0/plant-a/DDEATH/"):
		default:
			continue
		}
		p := decodeMsg(t, m)
		if !p.HasSeq {
			t.Fatalf("%s carries no seq; every non-NDEATH message must include one", m.Topic)
		}
		kinds = append(kinds, m.Topic[strings.LastIndex(m.Topic, "/")+1:])
		seqs = append(seqs, p.Seq)
	}
	if len(seqs) < 5 {
		t.Fatalf("got %d sequenced messages (%v), want at least 5", len(seqs), kinds)
	}
	if seqs[0] != 0 {
		t.Fatalf("NBIRTH seq = %d, want 0", seqs[0])
	}
	for i := 1; i < len(seqs); i++ {
		if want := (seqs[i-1] + 1) & 0xFF; seqs[i] != want {
			t.Fatalf("seq[%d] = %d, want %d (topics %v); the counter must span the node and all its devices",
				i, seqs[i], want, kinds)
		}
	}
}

// 设备进入 faulted 必须发 DDEATH，并在恢复后重新 DBIRTH。为了不依赖
// 调制解调器的故障注入时序，这里直接驱动 sink。
func TestSparkplugDeviceDeath(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	waitCount(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1", 1)
	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateCollecting, To: scheduler.StateFaulted,
		At: time.Now(),
	})
	death := waitTopic(t, b, "spBv1.0/plant-a/DDEATH/gw-01/dev1")
	if p := decodeMsg(t, death); len(p.Metrics) != 0 {
		t.Fatalf("DDEATH metrics = %+v", p.Metrics)
	}

	// 恢复采集：第一条数据必须先 DBIRTH，再 DDATA。
	deliverSample(t, s, "dev1", "p1", 2)
	waitCount(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1", 2)
	waitCount(t, b, "spBv1.0/plant-a/DBIRTH/gw-01/dev1", 2)

	// 只看「死亡之后」的那一段：DDEATH 之前还有一条 DDATA，按主题过滤
	// 会把两次出生混在一起。
	var order []string
	seenDeath := false
	for _, m := range b.Messages() {
		if m.Topic == "spBv1.0/plant-a/DDEATH/gw-01/dev1" {
			seenDeath = true
			continue
		}
		if !seenDeath {
			continue
		}
		switch m.Topic {
		case "spBv1.0/plant-a/DBIRTH/gw-01/dev1":
			order = append(order, "DBIRTH")
		case "spBv1.0/plant-a/DDATA/gw-01/dev1":
			order = append(order, "DDATA")
		}
	}
	if len(order) < 2 || order[0] != "DBIRTH" || order[1] != "DDATA" {
		t.Fatalf("rebirth order after DDEATH = %v", order)
	}
}

// NCMD 的 Node Control/Rebirth 必须重发 NBIRTH（以及已知设备的 DBIRTH）。
func TestSparkplugNodeRebirthCommand(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	b.WaitMessages(t, 1)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")

	raw, err := (codec.Payload{Metrics: []codec.Metric{{
		Name: "Node Control/Rebirth", DataType: codec.TypeBoolean, Value: true,
	}}, OmitSeq: true}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	b.Publish("spBv1.0/plant-a/NCMD/gw-01", raw, 1, false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countTopics(b.Messages(), "spBv1.0/plant-a/NBIRTH/gw-01") >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no second NBIRTH after NCMD; topics = %v", topicsOf(b.Messages()))
}

// 连接断开后（Broker 重启 / 网络抖动），下一条数据必须先补 BIRTH。
// 这条路径覆盖「OnConnect 是异步的」这一现实：不能假设它先于投递执行。
func TestSparkplugReconnectRebirths(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/NBIRTH/gw-01")
	first := countTopics(b.Messages(), "spBv1.0/plant-a/NBIRTH/gw-01")

	b.DropAll()

	// 这里不能用「第一次 deliver 返回 nil」判定重连完成。DDATA 是 QoS 0，
	// 没有 PUBACK 兜底：客户端可能把数据成功写进一条**已被对端关闭**的
	// socket（本端发送缓冲区还没报错），于是 deliver 返回成功而数据已经
	// 丢了。断言「重连」的唯一可靠信号是 Broker 上真的出现了新的 NBIRTH。
	deadline := time.Now().Add(15 * time.Second)
	for {
		if countTopics(b.Messages(), "spBv1.0/plant-a/NBIRTH/gw-01") > first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no NBIRTH after reconnect; topics = %v", topicsOf(b.Messages()))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := s.deliver(ctx, []outbox.Record{recordFor(t, "dev1", "p1", 2)})
		cancel()
		// 失败是预期：Outbox 会重试。成功也可能只是写进了已断的 socket，
		// 所以两种结果都继续下一轮，直到看到新 BIRTH。
		_ = err
		time.Sleep(50 * time.Millisecond)
	}

	// BIRTH 之后必须还有数据：只补 BIRTH 不补 DDATA 的话，消费方会得到
	// 一个「刚出生但永远没有值」的设备。
	msgs := b.Messages()
	lastBirth := -1
	for i, m := range msgs {
		if m.Topic == "spBv1.0/plant-a/NBIRTH/gw-01" {
			lastBirth = i
		}
	}
	dataAfterBirth := false
	for _, m := range msgs[lastBirth+1:] {
		if m.Topic == "spBv1.0/plant-a/DDATA/gw-01/dev1" {
			dataAfterBirth = true
			break
		}
	}
	if !dataAfterBirth {
		t.Fatalf("no DDATA after the new NBIRTH; topics = %v", topicsOf(msgs))
	}
}

// close 必须先发 NDEATH，再发 DISCONNECT。
//
// 正常停机时 Broker 不会替我们发遗嘱（发了 DISCONNECT 就抑制 LWT），所以
// 主动补一条是这个场景下唯一的死亡通知。顺序也重要：DISCONNECT 先到会让
// Broker 认为会话正常结束，之后那条 NDEATH 就成了会话外的迟到消息。
func TestSparkplugCloseSendsNDEATH(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")

	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	i := countTopics(b.Messages(), "spBv1.0/plant-a/NDEATH/gw-01")
	if i == 0 {
		t.Fatalf("no NDEATH on close; topics = %v", topicsOf(b.Messages()))
	}
	// 主动发布的 NDEATH 必须与遗嘱同 QoS（1），否则「主动告知的死亡」
	// 反而比「Broker 代发的遗嘱」更容易丢。
	for _, m := range b.Messages() {
		if m.Topic == "spBv1.0/plant-a/NDEATH/gw-01" && m.QoS != 1 {
			t.Fatalf("NDEATH qos = %d, want 1", m.QoS)
		}
		if m.Topic == "spBv1.0/plant-a/NDEATH/gw-01" && m.Retain {
			t.Fatal("NDEATH must not be retained")
		}
	}
	p := decodeMsg(t, mqtttest.Message{
		Topic: "spBv1.0/plant-a/NDEATH/gw-01",
		Payload: func() []byte {
			for _, m := range b.Messages() {
				if m.Topic == "spBv1.0/plant-a/NDEATH/gw-01" {
					return m.Payload
				}
			}
			return nil
		}(),
	})
	if p.HasSeq {
		t.Fatalf("NDEATH carries seq=%d; spec forbids it", p.Seq)
	}
	if len(p.Metrics) != 1 || p.Metrics[0].Name != "bdSeq" {
		t.Fatalf("NDEATH metrics = %+v; only bdSeq is allowed", p.Metrics)
	}
	// 关闭后必须发 DISCONNECT（抑制 Broker 代发遗嘱，避免重复的死亡消息）。
	b.WaitDisconnects(t, 1)
}

// 每次重连都必须重新生成遗嘱：bdSeq 每次 CONNECT +1（255 后回绕 0）。
//
// 这是「遗嘱做成回调而不是常量字段」的直接验证。常量遗嘱会让重连之后
// 所有 NDEATH 带同一个 bdSeq，Host 无法区分这次掉线与上一次的旧遗嘱。
func TestSparkplugBdSeqIncrementsPerConnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	b.WaitConnections(t, 1)

	for i := 0; i < 2; i++ {
		want := i + 2
		b.DropAll()
		// 等客户端发现断链：QoS0 的 Publish 写进一个已经死掉的 socket 也可
		// 能先成功（内核缓冲区），所以必须以「Broker 收到新的 CONNECT」为
		// 准，而不是以 deliver 不报错为准。
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := s.deliver(ctx, []outbox.Record{recordFor(t, "dev1", "p1", float64(want))})
			cancel()
			if err == nil && b.Connects() >= want {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if got := b.Connects(); got < want {
			t.Fatalf("after drop %d: connects = %d, want >= %d", i, got, want)
		}
	}

	infos := b.WaitConnections(t, 3)
	// 第一次 CONNECT 是会话 0（bdSeq 从 0 开始），之后每次 +1。
	for i, ci := range infos {
		if !ci.HasWill {
			t.Fatalf("connect %d: no will", i)
		}
		if ci.Will.QoS != 1 {
			t.Fatalf("connect %d: will qos = %d, must be 1", i, ci.Will.QoS)
		}
		if ci.Will.Retain {
			t.Fatalf("connect %d: will retain must be false", i)
		}
		if !ci.CleanSession {
			t.Fatalf("connect %d: cleanSession must be true", i)
		}
		p := decodeMsg(t, mqtttest.Message{Topic: ci.Will.Topic, Payload: ci.Will.Payload})
		if len(p.Metrics) != 1 || p.Metrics[0].Name != "bdSeq" {
			t.Fatalf("connect %d: will metrics = %+v", i, p.Metrics)
		}
		if got, ok := p.Metrics[0].Value.(int64); !ok || got != int64(i) {
			t.Fatalf("connect %d: will bdSeq = %v, want %d", i, p.Metrics[0].Value, i)
		}
	}

	// 每次会话的 NBIRTH 必须带**该次会话**的 bdSeq。
	//
	// NBIRTH 是 QoS0，一次恰好落在死链路上的发布会被静默丢掉，所以不能
	// 要求「一次 CONNECT 一条 NBIRTH」。能要求的是更强的规范约束：
	// 每条 NBIRTH 的 bdSeq 都必须是某次 CONNECT 遗嘱里的值，且随会话
	// 单调递增——这正是 Host 配对 NDEATH/NBIRTH 的依据。
	wills := make(map[int64]bool, len(infos))
	for _, ci := range infos {
		p := decodeMsg(t, mqtttest.Message{Topic: ci.Will.Topic, Payload: ci.Will.Payload})
		wills[p.Metrics[0].Value.(int64)] = true
	}

	var births []int64
	for _, m := range b.Messages() {
		if m.Topic != "spBv1.0/plant-a/NBIRTH/gw-01" {
			continue
		}
		p := decodeMsg(t, m)
		v, ok := p.Metrics[0].Value.(int64)
		if !ok {
			t.Fatalf("NBIRTH bdSeq type = %T", p.Metrics[0].Value)
		}
		births = append(births, v)
	}
	if len(births) == 0 {
		t.Fatalf("no NBIRTH at all; topics = %v", topicsOf(b.Messages()))
	}
	if births[0] != 0 {
		t.Fatalf("first NBIRTH bdSeq = %d, want 0 (bdSeq starts at 0)", births[0])
	}
	for i, got := range births {
		if !wills[got] {
			t.Fatalf("NBIRTH #%d bdSeq = %d is not any CONNECT's will bdSeq %v", i, got, wills)
		}
		if i > 0 && got <= births[i-1] {
			t.Fatalf("NBIRTH bdSeq not increasing: %v", births)
		}
	}
}

// 异常断线时 Broker 必须代发遗嘱：这条路径覆盖「进程被 kill -9」。
//
// 正常 close() 会发 NDEATH 并 DISCONNECT（抑制遗嘱），所以这里模拟的是
// 连接被对端/网络掐断——消费方只能靠 Broker 代发的遗嘱得知节点已死。
func TestSparkplugWillFiresOnAbnormalDisconnect(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")

	b.DropAll()

	wills := b.WaitWills(t, 1)
	w := wills[0]
	if w.Topic != "spBv1.0/plant-a/NDEATH/gw-01" {
		t.Fatalf("will topic = %q", w.Topic)
	}
	if w.QoS != 1 {
		t.Fatalf("will qos = %d, want 1", w.QoS)
	}
	if w.Retain {
		t.Fatal("will must not be retained")
	}
	p := decodeMsg(t, mqtttest.Message{Topic: w.Topic, Payload: w.Payload})
	if p.HasSeq {
		t.Fatal("NDEATH must not carry a sequence number")
	}
	if len(p.Metrics) != 1 || p.Metrics[0].Name != "bdSeq" {
		t.Fatalf("will metrics = %+v; only bdSeq is allowed", p.Metrics)
	}
}

// waitCount 等到某个主题上的消息数达到 want。
//
// 不能用 len(b.Messages()) 当界碑：Edge Node 的消息是 QoS0，没有 PUBACK，
// deliver 返回时 Broker 可能还没登记这些字节。用主题计数等待才能得到确定
// 的顺序。
func waitCount(t *testing.T, b *mqtttest.Broker, topic string, want int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if countTopics(b.Messages(), topic) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("topic %s: got %d messages, want >= %d; all topics = %v",
		topic, countTopics(b.Messages(), topic), want, topicsOf(b.Messages()))
}

// lastTopic 返回某个主题上的最后一条消息。
func lastTopic(b *mqtttest.Broker, topic string) (mqtttest.Message, bool) {
	var last mqtttest.Message
	found := false
	for _, m := range b.Messages() {
		if m.Topic == topic {
			last, found = m, true
		}
	}
	return last, found
}

func newTestSparkplugSink(t *testing.T, url string) *sparkplugSink {
	t.Helper()
	s, err := newSparkplugSink(config.MQTT{
		Enabled: true, Broker: url,
		Format: config.MQTTFormatSparkplugB, GroupID: "plant-a", NodeID: "gw-01",
	})
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	return s
}

// 设备被从配置里删除时必须发 DDEATH 并忘掉点位。
//
// 不发的话，下一次（重连的）DBIRTH 会把一台已经不在配置里的设备重新宣布
// 为「在线且带若干点位」，Host 会一直采集一堆不存在的量。
func TestSparkplugRemoveSendsDDEATH(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	deliverSample(t, s, "dev1", "p2", 2)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")
	// DDATA 是 QoS0，没有 PUBACK 兜底；必须等两条都落了账，才能把
	// 「之后的 DDEATH 是第一条」当断言。
	waitCount(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1", 2)

	s.onRemove("dev1")
	waitCount(t, b, "spBv1.0/plant-a/DDEATH/gw-01/dev1", 1)
	death, _ := lastTopic(b, "spBv1.0/plant-a/DDEATH/gw-01/dev1")
	if p := decodeMsg(t, death); len(p.Metrics) != 0 {
		t.Fatalf("DDEATH metrics = %+v", p.Metrics)
	}

	// 点位必须真的被忘掉：重连补发时不能再出现这台设备。
	if _, ok := s.devices["dev1"]; ok {
		t.Fatal("device state not cleared after DDEATH")
	}
	if len(s.deviceOrder) != 0 {
		t.Fatalf("deviceOrder = %v", s.deviceOrder)
	}

	// 再删一次不该发第二条 DDEATH：Host 已经不认识它，重复死亡是噪声。
	s.onRemove("dev1")
	time.Sleep(150 * time.Millisecond)
	if got := countTopics(b.Messages(), "spBv1.0/plant-a/DDEATH/gw-01/dev1"); got != 1 {
		t.Fatalf("duplicate DDEATH for an unknown device (count = %d)", got)
	}
}

// 同一 ID 的设备被重配（点位表变了）也要先 DDEATH 再重新出生，
// 否则 DBIRTH 会带上已经删掉的点位。
func TestSparkplugRemoveThenRebirthOmitsDeletedPoints(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	deliverSample(t, s, "dev1", "p2", 2)
	waitCount(t, b, "spBv1.0/plant-a/DBIRTH/gw-01/dev1", 2)

	s.onRemove("dev1")
	waitCount(t, b, "spBv1.0/plant-a/DDEATH/gw-01/dev1", 1)

	deliverSample(t, s, "dev1", "p1", 3)
	// 第三次 DBIRTH 是「删除后重新出生」的那份，等它真的落到 Broker
	// 再断言，否则读到的是删除前的旧报文（QoS0 没有 PUBACK 兜底）。
	waitCount(t, b, "spBv1.0/plant-a/DBIRTH/gw-01/dev1", 3)

	var last codec.Payload
	for _, m := range b.Messages() {
		if m.Topic == "spBv1.0/plant-a/DBIRTH/gw-01/dev1" {
			last = decodeMsg(t, m)
		}
	}
	var points []string
	for _, m := range last.Metrics {
		if m.Name != "Device Control/Rebirth" {
			points = append(points, m.Name)
		}
	}
	if len(points) != 1 || points[0] != "p1" {
		t.Fatalf("DBIRTH points = %v, want [p1]; a deleted point must not reappear", points)
	}
}

// degraded 是「部分点位失败、其余仍在采集」，不能发 DDEATH。
//
// DDEATH 的语义是「这台设备再也不会有数据了」。发它会让 Host 把整台设备
// 从模型里摘掉，正在正常上报的点位也跟着消失——这比不发严重得多。
func TestSparkplugDegradedDoesNotKillDevice(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")
	waitCount(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1", 1)
	deathsBefore := countTopics(b.Messages(), "spBv1.0/plant-a/DDEATH/gw-01/dev1")

	// 进入/离开每一个「不该发 DDEATH」的状态。
	for _, st := range []scheduler.State{
		scheduler.StateConnecting,
		scheduler.StateDegraded,
		scheduler.StateConfigured,
		scheduler.StateCollecting,
		scheduler.StateDegraded,
	} {
		s.onState(scheduler.StateEvent{
			DeviceID: "dev1", From: scheduler.StateCollecting, To: st,
			At: time.Now(),
		})
	}
	time.Sleep(200 * time.Millisecond)

	if got := countTopics(b.Messages(), "spBv1.0/plant-a/DDEATH/gw-01/dev1"); got != deathsBefore {
		t.Fatalf("got %d new DDEATH for transient states; only faulted/disabled may kill a device", got-deathsBefore)
	}
	if _, ok := s.devices["dev1"]; !ok {
		t.Fatal("device forgotten on a transient state")
	}
}

// disabled 与 faulted 一样，都是「这台设备不可达/不该读」。
func TestSparkplugDisabledSendsDDEATH(t *testing.T) {
	b := mqtttest.New(t, nil)
	s := newTestSparkplugSink(t, b.URL())
	defer s.close()

	deliverSample(t, s, "dev1", "p1", 1)
	waitTopic(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1")
	waitCount(t, b, "spBv1.0/plant-a/DDATA/gw-01/dev1", 1)

	s.onState(scheduler.StateEvent{
		DeviceID: "dev1", From: scheduler.StateCollecting, To: scheduler.StateDisabled,
		At: time.Now(),
	})
	waitCount(t, b, "spBv1.0/plant-a/DDEATH/gw-01/dev1", 1)
	death, _ := lastTopic(b, "spBv1.0/plant-a/DDEATH/gw-01/dev1")
	if p := decodeMsg(t, death); len(p.Metrics) != 0 {
		t.Fatalf("DDEATH metrics = %+v", p.Metrics)
	}
}

func recordFor(t *testing.T, device, point string, v float64) outbox.Record {
	t.Helper()
	payload, err := json.Marshal(eventPayload{SchemaVersion: 1, Sample: model.Sample{
		DeviceID: device, PointID: point,
		Value: model.NumValue(model.TypeFloat64, v), Quality: model.QualityGood,
		CollectedAt: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.Record{Sink: "mqtt", Key: device + "/" + point, Data: payload, Ts: time.Now()}
}

func deliverSample(t *testing.T, s *sparkplugSink, device, point string, v float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.deliver(ctx, []outbox.Record{recordFor(t, device, point, v)}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}
