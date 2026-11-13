package main

import (
	"testing"

	dl "uic/internal/codec/dlms"
	"uic/internal/driver"
	"uic/internal/model"
)

// 自检 CLI 的输入解析是「现场第一次接触就用到」的一层：这些函数必须在发出
// 任何帧之前就把明显错误的地址拦住，并且报错要说清"错在哪一层"。DLMS 有四层
// 承载，把 OBIS 写错的报错说成"地址越界"会把人带去改波特率。

func TestParseServerAddr(t *testing.T) {
	good := map[string]int{
		"1": 1, "0": 0, " 17 ": 17, "0x10": 0x10, "16383": 0x3FFF, "0x3fff": 0x3FFF,
	}
	for in, want := range good {
		got, err := parseServerAddr(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q = %d，应为 %d", in, got, want)
		}
	}

	bads := map[string]string{
		"":       "不是整数",
		"abc":    "不是整数",
		"1.5":    "不是整数",
		"-1":     "不是整数",
		"16384":  "14 位",
		"0x4000": "14 位",
		"127":    "广播",
		"0x7f":   "广播",
	}
	for in, want := range bads {
		_, err := parseServerAddr(in)
		if err == nil {
			t.Fatalf("%q 应被拒绝", in)
		}
		if !contains(err.Error(), want) {
			t.Fatalf("%q 的报错应提到 %q，得到 %v", in, want, err)
		}
	}
}

func TestDLMSRefs(t *testing.T) {
	got := dlmsRefs(" 1.0.1.8.0.255 , 1.0.32.7.0.255:3:2 ,, ")
	want := []string{"1.0.1.8.0.255", "1.0.32.7.0.255:3:2"}
	if len(got) != len(want) {
		t.Fatalf("got %v，应为 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v，应为 %v", got, want)
		}
	}
	if len(dlmsRefs("  ,  ")) != 0 {
		t.Fatal("只有空白时应返回空列表")
	}
}

// `<OBIS>[:<class>[:<attribute>]]` 必须都能切开。难点是 OBIS 的标准写法
// （`1-0:1-8-0-255`）本身就含冒号，所以切点只能靠「前缀能解析成 6 段」确定。
func TestDLMSSplitAddr(t *testing.T) {
	cases := []struct{ in, obis, suffix string }{
		{"1.0.1.8.0.255", "1.0.1.8.0.255", ""},
		{"1.0.1.8.0.255:3:2", "1.0.1.8.0.255", "3:2"},
		{"1.0.1.8.0.255:4", "1.0.1.8.0.255", "4"},
		// 标准正文写法：冒号在 OBIS 内部，不是后缀。
		{"1-0:1-8-0-255", "1-0:1-8-0-255", ""},
		{"1-0:1-8-0-255:3:2", "1-0:1-8-0-255", "3:2"},
		// 空格与多余空白：后缀归一化后输出，便于粘进点表。
		{" 1-0:1-8-0-255 : 3 : 2 ", "1-0:1-8-0-255", "3:2"},
		// 解析不了的地址原样返回，不能崩也不能编造类号。
		{"1.0.1.8", "1.0.1.8", ""},
	}
	for _, c := range cases {
		obis, suffix := dlmsSplitAddr(c.in)
		if obis != c.obis || suffix != c.suffix {
			t.Fatalf("%q → (%q, %q)，应为 (%q, %q)", c.in, obis, suffix, c.obis, c.suffix)
		}
	}
}

// SplitRef 是驱动与 CLI 共用的唯一切分点，因此这几种边界必须锁死：
// 只给 OBIS、只给 class、给全、以及标准冒号写法。
func TestDLMSSplitRef(t *testing.T) {
	cases := []struct {
		in            string
		obis          string
		class, attrib int
	}{
		{"1.0.1.8.0.255", "1.0.1.8.0.255", -1, -1},
		{"1.0.1.8.0.255:3", "1.0.1.8.0.255", 3, -1},
		{"1.0.1.8.0.255:3:2", "1.0.1.8.0.255", 3, 2},
		{"1-0:1-8-0-255", "1-0:1-8-0-255", -1, -1},
		{"1-0:1-8-0-255:3:2", "1-0:1-8-0-255", 3, 2},
		{"0.0.96.1.0.255:1:2", "0.0.96.1.0.255", 1, 2},
	}
	for _, c := range cases {
		obis, class, attr, err := dl.SplitRef(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if obis != c.obis || class != c.class || attr != c.attrib {
			t.Fatalf("%q → (%q, %d, %d)，应为 (%q, %d, %d)",
				c.in, obis, class, attr, c.obis, c.class, c.attrib)
		}
	}

	bads := []string{"", "1.0.1.8", "1.0.1.8.0.255:x", "1.0.1.8.0.255:3:2:1",
		"1.0.1.8.0.255:3:999", "1.0.1.8.0.256"}
	for _, in := range bads {
		if _, _, _, err := dl.SplitRef(in); err == nil {
			t.Fatalf("%q 应被拒绝", in)
		}
	}
}

// 输出的一行要在两种用法下都正确：
//   - 地址里带 `:<class>:<attr>` 时按显式值显示（私有 OBIS 只能这么读）；
//   - 只写 OBIS 时按内置表推导，并且「含义」一列要有中文名；
//   - 两边都查不到时留白，不能编一个类号。
func TestDLMSRowOfMakesCopyableAddress(t *testing.T) {
	sample := driver.RawSample{
		PointID: "1.0.32.7.0.255",
		Value:   model.NumValue(model.TypeFloat64, 220.1),
		Quality: model.QualityGood,
	}
	ref := driver.PointRef{
		ID:      "1.0.32.7.0.255",
		Address: model.Address{Kind: model.AddressSymbol, Path: "1.0.32.7.0.255"},
	}
	row := dlmsRowOf(sample, ref)
	if row.OBIS != "1.0.32.7.0.255" {
		t.Fatalf("obis = %q", row.OBIS)
	}
	if row.Class != dl.ClassRegister {
		t.Fatalf("类号 = %d，应从内置表推导出 %d", row.Class, dl.ClassRegister)
	}
	if row.Attribute != dl.AttrValue {
		t.Fatalf("属性号 = %d", row.Attribute)
	}
	if row.Name == "" {
		t.Fatal("内置表能查到的 OBIS 必须给出中文含义")
	}
	if row.Unit != "V" {
		t.Fatalf("单位 = %q，应为 V", row.Unit)
	}
	if !contains(row.Value, "220.1") {
		t.Fatalf("值 = %q", row.Value)
	}
	if !row.HasNumeric || row.Numeric != 220.1 {
		t.Fatalf("数值 = %v", row.Numeric)
	}
	if row.Quality != string(model.QualityGood) {
		t.Fatalf("质量 = %q", row.Quality)
	}

	// 显式后缀优先于内置表：现场常有厂商把同一 OBIS 挂到别的类上。
	ref.Address.Path = "1.0.32.7.0.255:4:3"
	row = dlmsRowOf(sample, ref)
	if row.Class != dl.ClassExtendedRegister || row.Attribute != dl.AttrScalerUnit {
		t.Fatalf("显式后缀未生效：class=%d attr=%d", row.Class, row.Attribute)
	}
	if row.Name == "" {
		t.Fatal("显式后缀下仍应有中文含义")
	}
	if row.ClassName == "" {
		t.Fatal("类名不应为空")
	}

	// 私有 OBIS + 显式后缀：没名字是正常的，但类号必须显示出来。
	ref.Address.Path = "1.0.128.7.0.255:4:2"
	row = dlmsRowOf(sample, ref)
	if row.Class != dl.ClassExtendedRegister {
		t.Fatalf("class = %d", row.Class)
	}
	if row.Name != "" {
		t.Fatalf("私有 OBIS 不应编造含义，得到 %q", row.Name)
	}

	// 私有 OBIS + 不带后缀：类号与属性号都留白，也不能崩。
	ref.Address.Path = "1.0.128.7.0.255"
	row = dlmsRowOf(sample, ref)
	if row.Class != 0 || row.Name != "" {
		t.Fatalf("不认识的 OBIS 应为空：class=%d name=%q", row.Class, row.Name)
	}
	if row.Attribute != dl.AttrValue {
		t.Fatalf("属性号默认应为 2，得到 %d", row.Attribute)
	}
}

// 采集失败的一行必须把错误写进 Error，并且不再填 Value/Type——否则表格里
// 会出现一个「看起来像读数」的空值。
func TestDLMSRowOfError(t *testing.T) {
	sample := driver.RawSample{
		PointID: "1.0.99.99.0.255",
		Quality: model.QualityAddressError,
		Err:     driver.E(driver.ErrAddress, "read", "", "object-undefined", false, nil),
	}
	ref := driver.PointRef{Address: model.Address{
		Kind: model.AddressSymbol, Path: "1.0.99.99.0.255:3:2"}}
	row := dlmsRowOf(sample, ref)
	if row.Error == "" {
		t.Fatal("失败点位的 Error 不应为空")
	}
	if row.Value != "" || row.Type != "" {
		t.Fatalf("失败点位不应有值：value=%q type=%q", row.Value, row.Type)
	}
	if row.Quality != string(model.QualityAddressError) {
		t.Fatalf("质量 = %q", row.Quality)
	}
}

// 默认点表是「一上手就能判断这条链路通不通」的最小集合，因此它必须：
//   - 每一条都能在内置表里查到（否则输出会大面积留白）；
//   - 覆盖 uint32 / 带 scaler 的 uint16 / octet-string 三种 A-XDR 形态；
//   - 覆盖 class 3 Register 与 class 1 Data 两类对象。
func TestDefaultDLMSPoints(t *testing.T) {
	pts := defaultDLMSPoints()
	if len(pts) < 5 {
		t.Fatalf("默认点表只有 %d 条，太少了", len(pts))
	}
	if len(pts) != 7 {
		t.Fatalf("默认点表应为 7 条（覆盖三类形态），得到 %d 条", len(pts))
	}
	seen := map[string]bool{}
	var haveRegister, haveData, haveScaler bool
	for _, p := range pts {
		if seen[p] {
			t.Fatalf("默认点表重复：%s", p)
		}
		seen[p] = true
		obj, ok := dl.LookupOBIS(p)
		if !ok {
			t.Fatalf("默认点位 %s 不在内置表里", p)
		}
		switch obj.Class {
		case dl.ClassRegister:
			haveRegister = true
		case dl.ClassData:
			haveData = true
		default:
			t.Fatalf("%s 的类号 %d 不常见", p, obj.Class)
		}
		if obj.Attribute == dl.AttrValue && obj.Unit != "" {
			haveScaler = true
		}
	}
	if !haveRegister || !haveData {
		t.Fatalf("默认点表应同时含 Register 与 Data：register=%v data=%v", haveRegister, haveData)
	}
	if !haveScaler {
		t.Fatal("默认点表应含带单位的瞬时量（用于验证 scaler）")
	}
}

// `-list` 打印的每一行都必须能反解析回同一个对象，否则「点表地址（可粘贴）」
// 这一列的承诺就不成立。
func TestDLMSCommonListAddresses(t *testing.T) {
	objs := dl.CommonObjects()
	if len(objs) < 20 {
		t.Fatalf("内置表只有 %d 条", len(objs))
	}
	for _, o := range objs {
		addr := o.OBIS + ":" + itoa(int(o.Class)) + ":" + itoa(int(o.Attribute))
		obis, suffix := dlmsSplitAddr(addr)
		if obis != o.OBIS {
			t.Fatalf("%s 切出 OBIS = %q", addr, obis)
		}
		if suffix == "" {
			t.Fatalf("%s 缺类号后缀", addr)
		}
		ref := driver.PointRef{ID: addr, Address: model.Address{
			Kind: model.AddressSymbol, Path: addr}}
		row := dlmsRowOf(driver.RawSample{PointID: addr,
			Value:   model.NumValue(model.TypeFloat64, 0),
			Quality: model.QualityGood}, ref)
		if row.Class != o.Class || row.Attribute != o.Attribute {
			t.Fatalf("%s → class=%d attr=%d，应为 %d/%d",
				addr, row.Class, row.Attribute, o.Class, o.Attribute)
		}
		if row.Name != o.Name {
			t.Fatalf("%s 的含义 = %q，应为 %q", addr, row.Name, o.Name)
		}
	}
}

// firstLine 只用于把多行错误压进表格，必须保留首行原文（结果码在里面）。
func TestFirstLine(t *testing.T) {
	if got := firstLine("第一行\n第二行"); got != "第一行" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine("没有换行"); got != "没有换行" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// itoa 是测试里的小工具（避免为一行字符串再引一次 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
