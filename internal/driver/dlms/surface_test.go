package dlms

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dl "uic/internal/codec/dlms"
	"uic/internal/driver"
	"uic/internal/model"
	simdlms "uic/internal/sim/dlms"
)

// 本文件补的是「自检命令/配置解析/错误分类会用到、但不参与正常采集主路径」
// 的那部分表面：地址推导的每个分支、options 解析、值转换的各形态、错误到
// 质量的映射、Scan / Association 这类只为 `-probe` / `-scan` 服务的接口。
//
// 这些分支在成功采集里一次都跑不到，但现场看到的每一条"为什么这个点读不出来"
// 都从这里出来——配置写错时给的是 ErrUnsupported 还是 ErrAddress，直接决定
// 运维去查配置文件还是去查表。

// ---------------------------------------------------------------------------
// 配置解析
// ---------------------------------------------------------------------------

// parseOptions 只接受范围内的值；越界或写错的关键字一律**忽略并保留默认**，
// 而不是猜一个近似值。猜错的表现是"地址不对"，比"忽略了你的配置"更难查。
func TestParseOptions(t *testing.T) {
	// 空字符串：全默认。
	d := parseOptions(model.Device{})
	if d.ClientAddr != defaultClientAddr || d.ServerAddr != defaultServerAddr ||
		d.AddrSize != 1 || d.Retries != defaultRetries || d.Gap != defaultGap || d.MaxPDU != 512 {
		t.Fatalf("默认值 = %+v", d)
	}

	// 全字段显式给出。
	o := parseOptions(model.Device{Labels: map[string]string{
		"options": " client = 5 , server=0x? , addr-size = 2 , retries=0 , gap=5ms , max-pdu=1024 ",
	}})
	if o.ClientAddr != 5 {
		t.Errorf("client = %d", o.ClientAddr)
	}
	// `0x?` 不是十进制整数：Atoi 失败，必须保留默认而不是变成 0。
	if o.ServerAddr != defaultServerAddr {
		t.Errorf("server = %d，非法值应保留默认 %d", o.ServerAddr, defaultServerAddr)
	}
	if o.AddrSize != 2 || o.Retries != 0 || o.Gap != 5*time.Millisecond || o.MaxPDU != 1024 {
		t.Errorf("解析结果 = %+v", o)
	}

	// 越界值全部忽略：客户端地址上限 0x7F、服务端 0x3FFF、addr-size 只能
	// 是 1/2/4、retries 上限 10、gap 上限 1 秒、max-pdu 范围 64..4096。
	bad := parseOptions(model.Device{Labels: map[string]string{
		"options": "client=200,server=99999,addr-size=3,retries=99,gap=5s,max-pdu=8",
	}})
	if bad.ClientAddr != defaultClientAddr || bad.ServerAddr != defaultServerAddr ||
		bad.AddrSize != 1 || bad.Retries != defaultRetries || bad.Gap != defaultGap || bad.MaxPDU != 512 {
		t.Fatalf("越界值应全部忽略，得到 %+v", bad)
	}

	// 没有 `=` 的片段与空片段都要能跳过。
	_ = parseOptions(model.Device{Labels: map[string]string{"options": ",,,client,,=,,"}})
}

// labels.address 优先于 options 的 server=，因为它能区分"配了 0"与"没配"
// ——options 里 server=0 会被当成默认值处理，而 0 在某些表上是合法地址。
func TestAddressOf(t *testing.T) {
	opt := Options{ServerAddr: 7, AddrSize: 1}

	a, err := addressOf(model.Device{}, opt)
	if err != nil {
		t.Fatalf("addressOf: %v", err)
	}
	if a.Upper != 7 || a.Size != 1 {
		t.Errorf("默认取 options.ServerAddr：%+v", a)
	}

	a, err = addressOf(model.Device{Labels: map[string]string{"address": " 3 "}}, opt)
	if err != nil {
		t.Fatalf("addressOf: %v", err)
	}
	if a.Upper != 3 {
		t.Errorf("labels.address 应覆盖 options：%+v", a)
	}

	// 非整数：明确报 ErrUnsupported（这是配置写错，不是通信故障）。
	if _, err := addressOf(model.Device{Labels: map[string]string{"address": "1-0:1"}}, opt); err == nil {
		t.Fatal("非整数地址应报错")
	} else if k := driver.KindOf(err); k != driver.ErrUnsupported {
		t.Errorf("错误分类 = %s", k)
	}

	// 地址域宽度 1/2/4 各自生效。
	for _, size := range []int{1, 2, 4} {
		a, err := addressOf(model.Device{}, Options{ServerAddr: 1, AddrSize: size})
		if err != nil {
			t.Fatalf("addr-size=%d: %v", size, err)
		}
		if a.Size != size {
			t.Errorf("addr-size=%d 得到 Size=%d", size, a.Size)
		}
	}
	// 3 字节不是 HDLC 的合法宽度。
	if _, err := addressOf(model.Device{}, Options{ServerAddr: 1, AddrSize: 3}); err == nil {
		t.Fatal("addr-size=3 应报错")
	} else if k := driver.KindOf(err); k != driver.ErrUnsupported {
		t.Errorf("错误分类 = %s", k)
	}
}

// attributeOf 的地址语法分支：非符号地址、空路径、只给类号、坏后缀。
func TestAttributeOfErrors(t *testing.T) {
	cases := []model.Address{
		{Kind: model.AddressRegister, Path: "1"},             // 不是符号地址
		{Kind: model.AddressSymbol, Path: "   "},             // 空路径
		{Kind: model.AddressSymbol, Path: "1.0.1.8.0"},       // 5 段
		{Kind: model.AddressSymbol, Path: "1.0.1.8.0.255:x"}, // 类号非数字
	}
	for _, a := range cases {
		if _, err := attributeOf(a); err == nil {
			t.Errorf("%+v 应报错", a)
		} else if k := driver.KindOf(err); k != driver.ErrAddress {
			t.Errorf("%+v 的错误分类 = %s", a, k)
		}
	}

	// 只给类号、不给属性号：默认 attribute 2（value）。现场手动试读时
	// 常这么写，比强制写 `:3:2` 少一次记错的机会。
	got, err := attributeOf(model.Address{Kind: model.AddressSymbol, Path: "1.0.99.1.0.255:4"})
	if err != nil {
		t.Fatalf("attributeOf: %v", err)
	}
	if got.ClassID != dl.ClassExtendedRegister || got.Attribute != dl.AttrValue {
		t.Errorf("只给类号 = class %d attr %d", got.ClassID, got.Attribute)
	}

	// 标准正文写法（冒号在 OBIS 内部）也要能解析。
	if _, err := attributeOf(model.Address{Kind: model.AddressSymbol, Path: "1-0:1-8-0-255:3:2"}); err != nil {
		t.Errorf("标准写法解析失败: %v", err)
	}

	// 显式属性号 3（scaler_unit）不能被改写成 2。
	su, err := attributeOf(model.Address{Kind: model.AddressSymbol, Path: "1.0.32.7.0.255:3:3"})
	if err != nil {
		t.Fatalf("attributeOf: %v", err)
	}
	if su.Attribute != dl.AttrScalerUnit {
		t.Errorf("属性号 = %d，应为 3", su.Attribute)
	}
}

// ---------------------------------------------------------------------------
// 值转换
// ---------------------------------------------------------------------------

// valueOf 覆盖 A-XDR 的每个 Kind。关键点：
//   - octet-string 全可打印时给 ASCII（表号），否则给十六进制（时钟状态）；
//   - date_time 解不出来时必须给原始字节，不能输出零值时间；
//   - null 给 0 而不是报"类型不支持"（否则一条 null 会让整批点位变坏）。
func TestValueOf(t *testing.T) {
	ref := driver.PointRef{ID: "p"}

	cases := []struct {
		name  string
		v     dl.Value
		typ   model.DataType
		num   float64
		text  string
		isNum bool
	}{
		{"structure", dl.Structure(dl.I8(-2), dl.Enum(29)), model.TypeString, 0, "{-2, 29}", false},
		{"array", dl.Array(dl.U8(1), dl.U8(2)), model.TypeString, 0, "{1, 2}", false},
		{"octets-ascii", dl.Octets([]byte("12345678")), model.TypeString, 0, "12345678", false},
		{"octets-binary", dl.Octets([]byte{0x00, 0x01, 0xFF}), model.TypeString, 0, "00 01 FF", false},
		{"visible", dl.Text("LD1"), model.TypeString, 0, "LD1", false},
		{"null", dl.Value{Tag: dl.TagNull}, model.TypeFloat64, 0, "", true},
		{"uint", dl.U32(4000), model.TypeFloat64, 4000, "", true},
		{"int", dl.I32(-7), model.TypeFloat64, -7, "", true},
		{"float", dl.Float64(1.25), model.TypeFloat64, 1.25, "", true},
		{"bool", dl.Bool(true), model.TypeFloat64, 1, "", true},
	}
	for _, c := range cases {
		got, err := valueOf(ref, c.v)
		if err != nil {
			t.Errorf("%s: valueOf: %v", c.name, err)
			continue
		}
		if got.Type != c.typ {
			t.Errorf("%s: Type = %s，应为 %s", c.name, got.Type, c.typ)
		}
		if c.isNum {
			if n, ok := got.AsFloat(); !ok || n != c.num {
				t.Errorf("%s: 数值 = %v ok=%v，应为 %v", c.name, n, ok, c.num)
			}
			continue
		}
		if got.Text != c.text {
			t.Errorf("%s: Text = %q，应为 %q", c.name, got.Text, c.text)
		}
	}

	// date_time：能解就格式化，解不出就保留原始字节。
	clock := dl.Value{Tag: dl.TagDateTime, Raw: []byte{
		0x07, 0xEA, 0x0B, 0x0D, 0x05, 0x08, 0x1E, 0x00, 0x00, 0x00, 0x00, 0x00}}
	got, err := valueOf(ref, clock)
	if err != nil {
		t.Fatalf("date_time: %v", err)
	}
	if got.Text != "2026-11-13T08:30:00" {
		t.Errorf("date_time = %q", got.Text)
	}

	// 全 0xFF 是"未指定"，不能格式化成 0001-01-01。
	blank := dl.Value{Tag: dl.TagDateTime, Raw: []byte{
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}}
	got, err = valueOf(ref, blank)
	if err != nil {
		t.Fatalf("未指定时钟: %v", err)
	}
	if strings.Contains(got.Text, "0001-01-01") {
		t.Errorf("未指定的时钟被格式化成了零值时间：%q", got.Text)
	}
}

// scaler_unit 结构只认 `{整数, 枚举}` 两元素；形态不对就报"没有 scaler"，
// 不能默认 0——默认 0 意味着"不缩放"，而真值可能差 1000 倍。
func TestScalerUnitShape(t *testing.T) {
	if s, ok := scalerUnit(dl.Structure(dl.I8(-1), dl.Enum(34))); !ok || s != -1 {
		t.Errorf("正常形态 = %d ok=%v", s, ok)
	}
	bad := []dl.Value{
		dl.Structure(dl.I8(-1)),                          // 只有一个元素
		dl.Structure(dl.I8(-1), dl.Enum(34), dl.Enum(0)), // 三个元素
		dl.Structure(dl.Text("x"), dl.Enum(34)),          // scaler 不是数值
		dl.Structure(dl.I8(-1), dl.Text("V")),            // unit 不是数值
		dl.U8(1),                                         // 根本不是结构
	}
	for i, v := range bad {
		if _, ok := scalerUnit(v); ok {
			t.Errorf("第 %d 个异常形态不应给出 scaler：%s", i+1, v.String())
		}
	}
}

func TestPrintableOrHex(t *testing.T) {
	if got := printableOrHex(nil); got != "" {
		t.Errorf("空字节 = %q", got)
	}
	if got := printableOrHex([]byte("LD1")); got != "LD1" {
		t.Errorf("可打印 = %q", got)
	}
	// 0x7F 是 DEL，按协议不可打印，必须走十六进制。
	if got := printableOrHex([]byte{0x41, 0x7F}); got != "41 7F" {
		t.Errorf("含不可打印字节 = %q", got)
	}
	if got := printableOrHex([]byte{0x1F}); got != "1F" {
		t.Errorf("控制字符 = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 错误分类与样本构造
// ---------------------------------------------------------------------------

// qualityOf 决定管线里点位被标成哪一种坏质量。分类错会让"表没应答"看起来
// 像"地址写错"，现场就会去查点表。
func TestQualityOf(t *testing.T) {
	cases := map[driver.ErrorKind]model.Quality{
		driver.ErrTimeout:      model.QualityTimeout,
		driver.ErrDisconnected: model.QualityCommError,
		driver.ErrAddress:      model.QualityAddressError,
		driver.ErrUnsupported:  model.QualityBad,
		driver.ErrPermission:   model.QualityBad,
		driver.ErrProtocol:     model.QualityBad,
	}
	for k, want := range cases {
		err := driver.E(k, "read", "dev", "x", false, nil)
		if got := qualityOf(err); got != want {
			t.Errorf("%s → %s，应为 %s", k, got, want)
		}
	}
	// 未分类错误按通信错误处理（最保守：会让管线重试）。
	if got := qualityOf(errors.New("随便什么")); got != model.QualityCommError {
		t.Errorf("未分类错误 → %s", got)
	}
}

// allFailed 给每个点位造一个带同一原因的坏样本：链路级错误时必须整批标记，
// 少标一个会让那个点位显示上一次的旧值。
func TestAllFailed(t *testing.T) {
	refs := []driver.PointRef{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	err := driver.E(driver.ErrTimeout, "read", "dev", "超时", true, nil)
	got := allFailed(refs, err)
	if len(got) != 3 {
		t.Fatalf("样本数 = %d", len(got))
	}
	for i, s := range got {
		if s.PointID != refs[i].ID {
			t.Errorf("第 %d 个 PointID = %q", i+1, s.PointID)
		}
		if s.Quality != model.QualityTimeout {
			t.Errorf("第 %d 个质量 = %s", i+1, s.Quality)
		}
		if s.Err == nil {
			t.Errorf("第 %d 个缺错误原因", i+1)
		}
	}
}

// flattenRefs 摊平多层 span，并按 PointID 去重、丢弃空 ID。
// 不去重的话同一个点位在一轮里会被读两遍，往返数白翻倍。
func TestFlattenRefs(t *testing.T) {
	spans := []driver.ReadSpan{
		{ID: "s1", Points: []driver.PointRef{{ID: "a"}, {ID: "b"}, {ID: ""}}},
		{ID: "s2", Points: []driver.PointRef{{ID: "b"}, {ID: "c"}}},
		{ID: "s3"},
	}
	got := flattenRefs(spans)
	if len(got) != 3 {
		t.Fatalf("摊平结果 = %d 条：%+v", len(got), got)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].ID != want {
			t.Errorf("第 %d 条 = %q，应为 %q（顺序必须稳定，日志才对得上）", i, got[i].ID, want)
		}
	}
	if got := flattenRefs(nil); len(got) != 0 {
		t.Errorf("空输入 = %d 条", len(got))
	}
}

func TestIsTimeoutAndLinkFailure(t *testing.T) {
	// net.Error 的 Timeout() 是超时的信号。
	if !isTimeout(&fakeTimeout{}) {
		t.Error("Timeout()=true 的错误应判为超时")
	}
	if !isTimeout(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded 应判为超时")
	}
	if isTimeout(nil) || isTimeout(errors.New("x")) {
		t.Error("普通错误不应判为超时")
	}

	if !isLinkFailure(driver.E(driver.ErrTimeout, "read", "d", "x", true, nil)) {
		t.Error("超时应算链路失败")
	}
	if !isLinkFailure(driver.E(driver.ErrDisconnected, "read", "d", "x", true, nil)) {
		t.Error("断连应算链路失败")
	}
	// 地址/协议错误是"这一个点位不对"，不能升级成链路失败——否则一个错点
	// 会把整批点位标记坏。
	for _, k := range []driver.ErrorKind{driver.ErrAddress, driver.ErrProtocol, driver.ErrUnsupported} {
		if isLinkFailure(driver.E(k, "read", "d", "x", false, nil)) {
			t.Errorf("%s 不应算链路失败", k)
		}
	}
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

// ---------------------------------------------------------------------------
// 需要真实从站的接口：Scan / Association
// ---------------------------------------------------------------------------

func openTestConn(t *testing.T, srv *simdlms.Server) driver.Conn {
	t.Helper()
	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter", Timeout: model.Duration(500 * time.Millisecond),
			Labels: map[string]string{"options": "client=16,server=1,retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Scan 只试内置常见表里的条目，并如实标注哪些表上没有。这里锁两件事：
// 1) 结果条数等于内置表条数（不能"少几行"让人以为表里没有）；
// 2) 读到的 register 必须带出 scaler_unit 文本（现场核对单位要看它）。
func TestScan(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	sc, ok := openTestConn(t, srv).(interface {
		Scan(context.Context) ([]ScanResult, error)
	})
	if !ok {
		t.Fatal("conn 未实现 Scan")
	}
	got, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != len(dl.CommonObjects()) {
		t.Fatalf("Scan 条数 = %d，应为 %d", len(got), len(dl.CommonObjects()))
	}
	byOBIS := map[string]ScanResult{}
	for _, r := range got {
		byOBIS[r.OBIS] = r
	}
	// 从站里有的条目必须读成功。
	energy, ok := byOBIS["1.0.1.8.0.255"]
	if !ok {
		t.Fatal("结果里缺 1.0.1.8.0.255")
	}
	if !energy.OK {
		t.Fatalf("电能应读成功：%s", energy.Err)
	}
	if energy.Text == "" {
		t.Error("读成功却没有值文本")
	}
	// scaler_unit 文本是现场核对单位的关键：写错单位会让读数差 1000 倍。
	if energy.ScalerUnit == "" {
		t.Error("register 的 Scan 结果应带 scaler_unit 文本")
	}
	if !strings.Contains(energy.ScalerUnit, "29") {
		t.Errorf("scaler_unit = %q，应含单位码 29（Wh）", energy.ScalerUnit)
	}

	// 从站里没有的条目必须标失败并给出原因，而不是静默消失。
	var missing int
	for _, r := range got {
		if !r.OK {
			missing++
			if r.Err == "" {
				t.Errorf("%s 失败却没有原因", r.OBIS)
			}
		}
	}
	if missing == 0 {
		t.Error("从站里只放了一部分对象，应至少有一条失败")
	}
}

// Association 报告的是**协商后**的参数。按请求值判断会得出错误结论：
// 标准允许服务端砍掉一半能力。
func TestAssociation(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	conn := openTestConn(t, srv)
	ac, ok := conn.(interface{ Association() AssociationInfo })
	if !ok {
		t.Fatal("conn 未实现 Association")
	}
	info := ac.Association()
	if !info.Associated {
		t.Error("建链成功后 Associated 应为 true")
	}
	if info.DLMSVersion != 6 {
		t.Errorf("DLMS 版本 = %d", info.DLMSVersion)
	}
	if info.VAAName != 0x0007 {
		t.Errorf("VAA 名 = %#04x，应为 0x0007", info.VAAName)
	}
	if info.AddrSize != 1 || info.ClientAddr != 0x10 || info.ServerAddr != 1 {
		t.Errorf("地址 = %+v", info)
	}
	if !info.Conformance.Has(dl.ConfGet) {
		t.Errorf("协商出的 conformance %06X 应含 get", uint32(info.Conformance))
	}
	if info.MaxPDU == 0 {
		t.Error("MaxPDU 不能为 0")
	}
}

// 从站回的 max-PDU 比我们请求的小时，驱动必须采用较小的那个：超过它
// 的请求表会拒绝或分块，而本驱动不支持分块。
func TestAssociationAdoptsSmallerMaxPDU(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter",
			Labels: map[string]string{"options": "retries=0,gap=1ms,max-pdu=4096"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()
	info := conn.(interface{ Association() AssociationInfo }).Association()
	// 从站固定回 0x0200 = 512。
	if info.MaxPDU != 512 {
		t.Errorf("MaxPDU = %d，应采用从站的 512", info.MaxPDU)
	}
}

// scaler 的应用结果必须是精确值，不能出现 `50.010000000000005` 这类尾数。
//
// 这条用频率点锁死：5001 以 0.01 Hz 为单位存储（scaler=-2）。若实现按
// `raw * pow(10, -2)`（即乘 0.01 这个不精确的 double）算，就会得到
// 50.010000000000005——数值"看起来对"，但屏幕上是一串尾数，且写入
// 时序库后会污染聚合值。
func TestReadScaledValuesExact(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	conn := openTestConn(t, srv)
	samples, err := conn.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{
			{ID: "freq", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.14.7.0.255"}},
			{ID: "pf", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.13.7.0.255"}},
			{ID: "volt", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.32.7.0.255"}},
			{ID: "cur", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.31.7.0.255"}},
			{ID: "power", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.1.7.0.255"}},
		}}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := map[string]float64{
		"freq":  50.01,
		"pf":    0.985,
		"volt":  220.1,
		"cur":   5.12,
		"power": 345.6,
	}
	for _, s := range samples {
		w, ok := want[s.PointID]
		if !ok {
			continue
		}
		got, isNum := s.Value.AsFloat()
		if !isNum {
			t.Errorf("%s 不是数值：%s", s.PointID, s.Value.String())
			continue
		}
		// 用 != 而不是近似比较：这里的正确性标准就是"每一位都对"。
		if got != w {
			t.Errorf("%s = %v（%s），应为 %v", s.PointID, got, s.Value.String(), w)
		}
	}
}

// ---------------------------------------------------------------------------
// 关闭 / 空请求 / 链路失败缓存
// ---------------------------------------------------------------------------

func TestClosedConnRejectsEverything(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	conn := openTestConn(t, srv)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 重复 Close 必须幂等。
	if err := conn.Close(); err != nil {
		t.Errorf("第二次 Close: %v", err)
	}

	ctx := context.Background()
	if k := driver.KindOf(conn.Probe(ctx)); k != driver.ErrDisconnected {
		t.Errorf("Probe 错误分类 = %s", k)
	}
	_, err = conn.Read(ctx, []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{{ID: "a", Address: model.Address{
			Kind: model.AddressSymbol, Path: "1.0.1.8.0.255"}}}}})
	if k := driver.KindOf(err); k != driver.ErrDisconnected {
		t.Errorf("Read 错误分类 = %s", k)
	}
	sc := conn.(interface {
		Scan(context.Context) ([]ScanResult, error)
	})
	if _, err := sc.Scan(ctx); driver.KindOf(err) != driver.ErrDisconnected {
		t.Errorf("Scan 错误分类 = %s", driver.KindOf(err))
	}
}

// Read 传空 span 列表时返回 (nil, nil)：这不是错误，是"没什么要读"。
func TestReadNoPoints(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	conn := openTestConn(t, srv)
	got, err := conn.Read(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("空请求 = %v, %v", got, err)
	}
	// 有 span 但没有点位同理。
	got, err = conn.Read(context.Background(), []driver.ReadSpan{{ID: "s"}})
	if err != nil || got != nil {
		t.Fatalf("空 span = %v, %v", got, err)
	}
}

// 链路级失败之后，同一轮里剩下的点位不应再各等一次超时：驱动记住首个
// 链路错误，后续点位直接复用。
func TestBadChecksumIsProtocolError(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	conn := openTestConn(t, srv)
	// 建链之后再打开坏校验，否则连 SNRM 都应答不了了。
	srv.SetBadChecksum(true)

	got, _ := conn.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{
			{ID: "a", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.1.8.0.255"}},
		}}})
	if len(got) != 1 {
		t.Fatalf("样本数 = %d", len(got))
	}
	if got[0].Err == nil {
		t.Fatal("坏校验序列应让该点位失败")
	}
	// 坏帧归 ErrProtocol 而不是 ErrTimeout：线路上确实有字节回来，只是我们
	// 没解对，最可能是波特率/校验位配错。报超时会让现场去查线。
	if k := driver.KindOf(got[0].Err); k != driver.ErrProtocol {
		t.Fatalf("错误分类 = %s，坏帧应归 ErrProtocol（提示查波特率/校验位）", k)
	}
	if got[0].Quality == model.QualityGood {
		t.Error("坏帧不应标成 good")
	}
	if !strings.Contains(got[0].Err.Error(), "坏帧") {
		t.Errorf("错误信息应点明坏帧：%v", got[0].Err)
	}
}

// TestDMIsNotRetried 表回了 DM（断开模式）：帧结构完好、地址正确，只是语义上
// 表示「表拒绝继续通讯」。规范要求这类失败**不重发**——重发同一请求只会再收到
// 一个 DM。它与 FCS 坏（线路干扰、可重发）在错误种类上同为 ErrProtocol，若重发
// 循环只看 KindOf 就会一起重试到上限。
func TestDMIsNotRetried(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter", Timeout: model.Duration(500 * time.Millisecond),
			Labels: map[string]string{"options": "client=16,server=1,addr-size=1,retries=2,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(2 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	// 建链之后再打开 DM，否则 SNRM 都建不起来。
	srv.SetReplyDM(true)
	_, _, getBefore, _ := srv.Counts()

	got, _ := conn.Read(context.Background(), []driver.ReadSpan{{ID: "s",
		Points: []driver.PointRef{
			{ID: "a", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.1.8.0.255"}},
		}}})
	if len(got) != 1 {
		t.Fatalf("样本数 = %d", len(got))
	}
	if got[0].Err == nil {
		t.Fatal("DM 应让该点位失败")
	}
	if k := driver.KindOf(got[0].Err); k != driver.ErrProtocol {
		t.Fatalf("错误分类 = %v，应为 ErrProtocol", k)
	}
	if driver.IsRetryable(got[0].Err) {
		t.Fatal("DM 必须标为不可重发")
	}
	_, _, getAfter, _ := srv.Counts()
	// retries=2 时若误重发会发 3 次 Get；正确行为是只发 1 次。
	if n := getAfter - getBefore; n != 1 {
		t.Fatalf("DM 不应重发（只发 1 次 Get），实际 %d 次", n)
	}
}

// 链路错误一旦记住，后续 Read 应立即整批标坏（不再逐点等超时）。// 用静默从站构造：超时会被 isLinkFailure 认成链路级错误。
func TestLinkErrCached(t *testing.T) {
	srv, err := simdlms.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动从站失败: %v", err)
	}
	defer srv.Close()

	d := New()
	conn, err := d.Open(context.Background(), driver.OpenConfig{
		Device: model.Device{ID: "meter", Timeout: model.Duration(200 * time.Millisecond),
			Labels: map[string]string{"options": "retries=0,gap=1ms"}},
		Channel: model.Channel{
			Transport: model.Transport{Kind: "tcp", Address: srv.Addr()},
			Timeout:   model.Duration(200 * time.Millisecond),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	srv.SetSilent(true)
	spans := []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "a", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.1.8.0.255"}},
		{ID: "b", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.32.7.0.255"}},
		{ID: "c", Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.31.7.0.255"}},
	}}}
	got, err := conn.Read(context.Background(), spans)
	if err == nil {
		t.Fatal("静默从站应报错")
	}
	if len(got) != 3 {
		t.Fatalf("样本数 = %d，应整批标坏", len(got))
	}
	for _, s := range got {
		if s.Quality != model.QualityTimeout {
			t.Errorf("%s 质量 = %s", s.PointID, s.Quality)
		}
	}

	// 第二轮：链路错误已缓存，必须立刻返回且不再发包。
	_, _, before, _ := srv.Counts()
	start := time.Now()
	got, err = conn.Read(context.Background(), spans)
	if err == nil {
		t.Fatal("第二轮也应报错")
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("缓存链路错误后应立即返回，实际耗时 %v", d)
	}
	if len(got) != 3 {
		t.Errorf("第二轮样本数 = %d", len(got))
	}
	_, _, after, _ := srv.Counts()
	if after != before {
		t.Errorf("第二轮不应再发 GET（%d → %d）", before, after)
	}
}
