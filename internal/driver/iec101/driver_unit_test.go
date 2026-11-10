package iec101

// 本文件测的是「配置解析与点位语义」这类不需要走总线的部分。
//
// 端到端测试（driver_test.go）能证明一整轮轮询能跑通，但它跑的是双方都
// 按同一套默认参数实现的路径；现场真正会踩的坑是「装置手册的参数与默认
// 值不同」。因此这里逐项验证标签覆盖、越界拒绝与「一个 IOA 两个点位」
// 的缓存隔离。

import (
	"strings"
	"testing"
	"time"

	iec101codec "uic/internal/codec/iec101"
	"uic/internal/codec/iec60870"
	"uic/internal/driver"
	"uic/internal/model"
)

func devWithLabels(labels map[string]string, points ...model.Point) model.Device {
	return model.Device{
		ID: "rtu", ChannelID: "ch", UnitID: 1, Enabled: true,
		Interval: model.Duration(time.Second),
		Labels:   labels,
		Points:   points,
	}
}

func TestParseOptionsDefaults(t *testing.T) {
	dev := devWithLabels(nil)
	dev.UnitID = 0
	opt, err := parseOptions(dev)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opt.linkAddr != 1 || opt.linkAddrSize != 1 {
		t.Fatalf("link = %d/%d", opt.linkAddr, opt.linkAddrSize)
	}
	if opt.ca != 1 {
		t.Fatalf("ca = %d, want 1 when unitId is unset", opt.ca)
	}
	if opt.profile != (iec60870.Profile{OrgSize: 0, CASize: 1, IOASize: 2}) {
		t.Fatalf("profile = %+v", opt.profile)
	}
	if opt.interrogate != defaultInterrogate || !opt.class2 {
		t.Fatalf("poll options = %+v", opt)
	}
}

func TestParseOptionsLabelOverrides(t *testing.T) {
	dev := devWithLabels(map[string]string{
		"linkAddress":       "247",
		"linkAddressSize":   "2",
		"commonAddress":     "4660",
		"commonAddressSize": "2",
		"ioaSize":           "3",
		"orgSize":           "1",
		"interrogate":       "90s",
		"class2":            "false",
	})
	opt, err := parseOptions(dev)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opt.linkAddr != 247 || opt.linkAddrSize != 2 {
		t.Fatalf("link = %d/%d", opt.linkAddr, opt.linkAddrSize)
	}
	if opt.ca != 4660 {
		t.Fatalf("ca = %d", opt.ca)
	}
	if opt.profile != (iec60870.Profile{OrgSize: 1, CASize: 2, IOASize: 3}) {
		t.Fatalf("profile = %+v", opt.profile)
	}
	if opt.interrogate != 90*time.Second || opt.class2 {
		t.Fatalf("poll options = %+v", opt)
	}
}

// TestParseOptionsRejectsBadValues 逐项验证「宽度与取值必须自洽」。
//
// 这些错误如果放过，症状都不是编译期问题，而是现场的超时或错位：
// 链路地址超过 1 字节会被截断成另一台从站的地址，公共地址超过 1 字节
// 会让装置按「公共地址不匹配」静默丢弃。
func TestParseOptionsRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"链路地址超 1 字节", map[string]string{"linkAddress": "300", "linkAddressSize": "1"}, "exceeds 1 byte"},
		{"链路地址宽度非法", map[string]string{"linkAddressSize": "3"}, "out of range"},
		{"无地址却给地址", map[string]string{"linkAddress": "5", "linkAddressSize": "0"}, "cannot be used"},
		{"公共地址超 1 字节", map[string]string{"commonAddress": "300", "commonAddressSize": "1"}, "exceeds 1 byte"},
		{"IOA 宽度非法", map[string]string{"ioaSize": "4"}, "out of range"},
		{"源发地址宽度非法", map[string]string{"orgSize": "2"}, "out of range"},
		{"总召唤周期不是时长", map[string]string{"interrogate": "5 minutes"}, "unknown unit"},
		{"总召唤周期为负", map[string]string{"interrogate": "-1s"}, "must not be negative"},
		{"布尔标签非法", map[string]string{"class2": "maybe"}, "want true/false"},
		{"整数字段非数字", map[string]string{"ioaSize": "two"}, "not an integer"},
	}
	for _, c := range cases {
		_, err := parseOptions(devWithLabels(c.labels))
		if err == nil {
			t.Fatalf("%s: accepted", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: error = %q, want substring %q", c.name, err, c.want)
		}
	}
}

func TestMaxIOAOf(t *testing.T) {
	cases := map[int]uint32{1: 0xFF, 2: 0xFFFF, 3: 0xFFFFFF}
	for size, want := range cases {
		if got := maxIOAOf(iec60870.Profile{CASize: 1, IOASize: size}); got != want {
			t.Fatalf("iOASize %d: maxIOA = %d, want %d", size, got, want)
		}
	}
}

// TestBuildPlansChecksProfileRange 保证点位地址在开口之前就按剖面校验。
// 超范围的 IOA 到组帧时才报错，会表现为「连上了但每个周期都断」。
func TestBuildPlansChecksProfileRange(t *testing.T) {
	dev := devWithLabels(nil,
		ioaPoint("big", AreaIOA, 300, model.TypeFloat32),
		ioaPoint("ok", AreaIOA, 200, model.TypeFloat32),
	)
	_, _, err := buildPlans(dev, 0xFF) // 1 字节地址空间
	if err == nil {
		t.Fatal("IOA 300 with a 1-byte address space must be rejected")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want address", driver.KindOf(err))
	}
	if _, _, err := buildPlans(dev, 0xFFFF); err != nil {
		t.Fatalf("2-byte address space: %v", err)
	}
}

// unitConn 造一个不上总线的连接，只用来测「收到 ASDU 之后怎么记账」。
func unitConn(t *testing.T, dev model.Device) *conn {
	t.Helper()
	opt, err := parseOptions(dev)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	plans, byIOA, err := buildPlans(dev, maxIOAOf(opt.profile))
	if err != nil {
		t.Fatalf("buildPlans: %v", err)
	}
	return &conn{
		deviceID: dev.ID,
		opt:      opt,
		profile:  opt.profile,
		maxIOA:   maxIOAOf(opt.profile),
		points:   plans,
		byIOA:    byIOA,
		values:   map[string]cachedValue{},
		observed: map[uint32]Observed{},
	}
}

// TestCacheIsPerPointNotPerIOA 是这个驱动里最容易出错的一处记账。
//
// 一个 IOA 可以同时是遥信（bool）与遥控（uint16）：两套语义、两种平台
// 类型。按 IOA 缓存会让后写入的一方覆盖另一方，症状是「布尔遥信显示成
// double 的 2」。按点位 ID 缓存之后，两边各存一份。
func TestCacheIsPerPointNotPerIOA(t *testing.T) {
	dev := devWithLabels(nil,
		ioaPoint("breaker", AreaIOA, 2003, model.TypeBool),
		ioaPoint("breaker-cmd", AreaDoubleCommand, 2003, model.TypeUint16),
	)
	c := unitConn(t, dev)

	raw, err := iec60870.BuildASDUProfile(iec60870.ASDU{
		TypeID: iec60870.TypeDoublePoint, Cause: iec60870.CauseInterrogated, CommonAddr: 1,
		Objects: []iec60870.Object{{IOA: 2003, Uint: 2}},
	}, c.profile)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a := c.consume(raw); a != nil {
		t.Fatalf("monitoring ASDU returned a command confirmation: %+v", a)
	}

	got := c.lookupLocked("breaker")
	if got.Err != nil || got.Value.Type != model.TypeBool || !got.Value.Bool {
		t.Fatalf("breaker = %+v, want bool true", got)
	}
	cmd := c.lookupLocked("breaker-cmd")
	if cmd.Err != nil || cmd.Value.Type != model.TypeUint16 || cmd.Value.Num != 2 {
		t.Fatalf("breaker-cmd = %+v, want uint16 2", cmd)
	}
}

// TestLookupExplainsControlOnlyPoint 保证「遥控点读不到值」给的是可执行
// 的解释，而不是笼统的「还没收到数据」。
func TestLookupExplainsControlOnlyPoint(t *testing.T) {
	dev := devWithLabels(nil, ioaPoint("cmd", AreaSingleCommand, 4001, model.TypeBool))
	c := unitConn(t, dev)
	got := c.lookupLocked("cmd")
	if got.Err == nil {
		t.Fatal("control-only point without a written value must carry an error")
	}
	if !strings.Contains(got.Err.Error(), "control direction") {
		t.Fatalf("error = %q", got.Err)
	}
	if got.Quality != model.QualityBad {
		t.Fatalf("quality = %s", got.Quality)
	}
}

// TestAddresslessFixedFrame 验证 linkAddressSize=0 的工程配置。
//
// 「不带链路地址」在标准的某些配套里是合法选择，帧会短一个字节；
// 组帧时按有地址去写，会让装置把控制域后面那个字节当成地址，整帧失效。
func TestAddresslessFixedFrame(t *testing.T) {
	dev := devWithLabels(map[string]string{"linkAddress": "0", "linkAddressSize": "0"})
	c := unitConn(t, dev)
	frame, err := c.fixedFrame(9) // 请求链路状态
	if err != nil {
		t.Fatalf("fixedFrame: %v", err)
	}
	if len(frame) != 4 {
		t.Fatalf("addressless fixed frame length = %d, want 4: % X", len(frame), frame)
	}
	pr, err := iec101codec.ParseAt(frame, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fr := pr.Frame
	if fr.Kind != iec101codec.KindFixed || fr.Ctrl.Func != 9 || fr.Ctrl.PRM != true {
		t.Fatalf("frame = %+v", fr)
	}
}

// TestUserFrameTogglesFCB 验证帧计数位逐帧翻转，且重传语义依赖「同一帧
// 字节」。这里只验证翻转本身；重传复用同一份字节由 sendUser 保证。
func TestUserFrameTogglesFCB(t *testing.T) {
	dev := devWithLabels(nil, ioaPoint("temp", AreaIOA, 1001, model.TypeFloat32))
	c := unitConn(t, dev)
	asdu, err := iec60870.BuildInterrogationProfile(1, iec60870.QOIStation, c.profile)
	if err != nil {
		t.Fatal(err)
	}
	var bits []bool
	for i := 0; i < 3; i++ {
		raw, err := c.userFrame(asdu)
		if err != nil {
			t.Fatalf("userFrame: %v", err)
		}
		pr, err := iec101codec.ParseAt(raw, c.opt.linkAddrSize)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		fr := pr.Frame
		if !fr.Ctrl.FCV || fr.Ctrl.Func != iec101codec.FCSendConfirm {
			t.Fatalf("control = %+v", fr.Ctrl)
		}
		bits = append(bits, fr.Ctrl.FCB)
	}
	want := []bool{true, false, true}
	for i := range want {
		if bits[i] != want[i] {
			t.Fatalf("FCB sequence = %v, want %v", bits, want)
		}
	}
}
