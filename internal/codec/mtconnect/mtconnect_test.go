package mtconnect

import (
	"strings"
	"testing"
)

// 本文件只测纯解析层：把 XML 字节喂进去，断言解析出的结构。HTTP、超时、
// 多设备选择都在 driver 层测；这里把「XML → Item/Streams/Devices/Error」钉死。

func TestParseStreamsBasic(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<MTConnectStreams>
  <Header version="2.0" sender="agent" instanceId="1" bufferSize="2000"
          creationTime="2026-01-01T00:00:00Z" firstSequence="10"
          lastSequence="12" nextSequence="13"/>
  <Streams>
    <DeviceStream name="VMC" uuid="VMC-001">
      <ComponentStream component="Linear" name="X" componentId="c1">
        <Samples>
          <Position dataItemId="Xabs" subType="ACTUAL" units="MILLIMETER"
                    timestamp="2026-01-01T00:00:00Z" sequence="11">12.5</Position>
        </Samples>
        <Events>
          <Execution dataItemId="exec" timestamp="2026-01-01T00:00:00Z" sequence="12">ACTIVE</Execution>
        </Events>
      </ComponentStream>
    </DeviceStream>
  </Streams>
</MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Header.LastSequence != 12 || s.Header.NextSequence != 13 {
		t.Fatalf("header seq = last=%d next=%d", s.Header.LastSequence, s.Header.NextSequence)
	}
	if len(s.Devices) != 1 || s.Devices[0].UUID != "VMC-001" {
		t.Fatalf("devices = %+v", s.Devices)
	}
	items := s.Items()
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	idx := s.Index()
	it, ok := idx.Lookup("Xabs")
	if !ok {
		t.Fatal("Xabs not found")
	}
	if it.Category != CategorySample || it.Type != "Position" || it.Value() != "12.5" {
		t.Fatalf("Xabs item = %+v", it)
	}
	if it.Units != "MILLIMETER" || it.SubType != "ACTUAL" {
		t.Fatalf("Xabs attrs = units=%q subType=%q", it.Units, it.SubType)
	}
	ev, _ := idx.Lookup("exec")
	if ev.Category != CategoryEvent || ev.Value() != "ACTIVE" {
		t.Fatalf("exec item = %+v", ev)
	}
}

func TestParseStreamsNextSequenceFallback(t *testing.T) {
	// 老 Agent 不写 nextSequence：按「最后一条 + 1」补齐。
	doc := `<MTConnectStreams><Header lastSequence="99"/><Streams>` +
		`<DeviceStream name="d" uuid="u"><ComponentStream component="Linear" name="X">` +
		`<Samples><Position dataItemId="Xabs" sequence="99">1</Position></Samples>` +
		`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Header.NextSequence != 100 {
		t.Fatalf("nextSequence = %d, want 100", s.Header.NextSequence)
	}
}

func TestParseStreamsConditionTwoLayer(t *testing.T) {
	// 规范的两层写法：外层是条件类型，内层是级别。
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Controller" name="controller">` +
		`<Conditions><System dataItemId="alm" type="SYSTEM" timestamp="t" sequence="5">` +
		`<Fault timestamp="t" sequence="5" nativeCode="">OVERHEAT</Fault>` +
		`</System></Conditions></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, ok := s.Index().Lookup("alm")
	if !ok {
		t.Fatal("alm not found")
	}
	if !it.IsCondition() {
		t.Fatalf("alm should be a condition: %+v", it)
	}
	if it.Condition != ConditionFault {
		t.Fatalf("condition level = %q, want FAULT", it.Condition)
	}
	// 两层写法里外层元素名（System）就是条件类型。
	if it.Type != "System" {
		t.Fatalf("condition type = %q, want System", it.Type)
	}
	if it.Value() != "FAULT" {
		t.Fatalf("condition value = %q, want FAULT", it.Value())
	}
}

func TestParseStreamsConditionFlat(t *testing.T) {
	// 平铺写法：元素名即级别，`type` 属性是条件类型。
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Controller" name="c">` +
		`<Conditions><Warning dataItemId="w" type="SYSTEM" timestamp="t" sequence="3">watch out</Warning>` +
		`<Normal dataItemId="n" type="SYSTEM" timestamp="t" sequence="4">ok</Normal>` +
		`</Conditions></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	w, _ := s.Index().Lookup("w")
	if w.Condition != ConditionWarning || w.Type != "SYSTEM" || w.Value() != "WARNING" {
		t.Fatalf("warning item = %+v", w)
	}
	n, _ := s.Index().Lookup("n")
	if n.Condition != ConditionNormal {
		t.Fatalf("normal item = %+v", n)
	}
}

func TestParseStreamsConditionUnavailableFlat(t *testing.T) {
	// 平铺写法的 UNAVAILABLE：外层元素名即级别，无内层。
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Controller" name="c">` +
		`<Conditions><Unavailable dataItemId="alm" type="SYSTEM" timestamp="t" sequence="1"/>` +
		`</Conditions></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, _ := s.Index().Lookup("alm")
	if !it.Unavailable() {
		t.Fatalf("alm should be unavailable: %+v", it)
	}
}

// TestParseStreamsConditionWithoutLevel 钉住「条件缺级别」这一形态：它必须
// 保留 Category=CONDITION，同时让 LevelKnown() 明确为 false。
//
// 为什么不能把它当成普通事件：调用方只看 Condition=="" 就会回落成「普通文本」，
// 于是一条读不出严重度的告警会被上报成 quality=good + value="OVERHEAT"——
// 这正是本驱动最容易犯的错（能解析、字段不空、看不出异常）。能否识别出「级别
// 缺失」决定了驱动是 fail-open 还是 fail-closed。
func TestParseStreamsConditionWithoutLevel(t *testing.T) {
	cases := []struct{ name, conds string }{
		{"未知级别元素名", `<Conditions><System dataItemId="alm" type="SYSTEM" timestamp="t" sequence="5">` +
			`<BogusLevel timestamp="t" sequence="5">OVERHEAT</BogusLevel></System></Conditions>`},
		{"外层无内层", `<Conditions><System dataItemId="alm" type="SYSTEM" timestamp="t" sequence="5">` +
			`</System></Conditions>`},
		{"平铺+未知元素名", `<Conditions><Bogus dataItemId="alm" type="SYSTEM" timestamp="t" sequence="5">` +
			`OVERHEAT</Bogus></Conditions>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
				`<ComponentStream component="Controller" name="c">` + c.conds +
				`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`
			s, err := ParseStreams([]byte(doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			it, ok := s.Index().Lookup("alm")
			if !ok {
				t.Fatal("alm 应被索引（它是一条条件观测，不是噪声）")
			}
			if it.Category != CategoryCondition {
				t.Fatalf("Category = %q，应为 CONDITION（语义仍是报警）", it.Category)
			}
			if it.Condition != "" {
				t.Fatalf("Condition = %q，本用例应构造「级别缺失」", it.Condition)
			}
			if it.IsCondition() {
				t.Fatal("IsCondition 应为 false：它表示「带合法级别」")
			}
			if it.LevelKnown() {
				t.Fatal("LevelKnown 应为 false：级别缺失必须让调用方判得出")
			}
		})
	}
}

// TestParseStreamsLevelKnownTrue 是上一条的对照组：带合法级别的条件
// LevelKnown 必须为 true，否则修复会「一律判坏」。
func TestParseStreamsLevelKnownTrue(t *testing.T) {
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Controller" name="c">` +
		`<Conditions><System dataItemId="alm" type="SYSTEM" timestamp="t" sequence="5">` +
		`<Fault timestamp="t" sequence="5">OVERHEAT</Fault></System></Conditions>` +
		`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, _ := s.Index().Lookup("alm")
	if !it.LevelKnown() || !it.IsCondition() || it.Condition != ConditionFault {
		t.Fatalf("带 Fault 级别的条件应 LevelKnown=true: %+v", it)
	}
	// 非条件观测不该被 LevelKnown 误伤。
	sample := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Axes" name="x">` +
		`<Samples><Position dataItemId="Xabs" timestamp="t" sequence="1">12.5</Position>` +
		`</Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s2, err := ParseStreams([]byte(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pos, _ := s2.Index().Lookup("Xabs")
	if !pos.LevelKnown() {
		t.Fatalf("非条件观测应恒为 LevelKnown=true: %+v", pos)
	}
}

func TestParseStreamsUnavailableSample(t *testing.T) {
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Systems" name="s">` +
		`<Samples><Tool_Offset dataItemId="toolOffset" timestamp="t" sequence="1">UNAVAILABLE</Tool_Offset>` +
		`</Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, _ := s.Index().Lookup("toolOffset")
	if !it.Unavailable() {
		t.Fatalf("toolOffset should be unavailable: text=%q", it.Text)
	}
	// 值文本（不是级别）：UNAVAILABLE 哨兵。
	if it.Value() != "UNAVAILABLE" {
		t.Fatalf("value = %q", it.Value())
	}
}

func TestParseStreamsDuplicateIDMerges(t *testing.T) {
	// 同一个 dataItemId 出现两次（挂在两个组件下）：取更严重/更新的。
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Linear" name="X"><Samples>` +
		`<Position dataItemId="Xabs" sequence="5">1.0</Position></Samples></ComponentStream>` +
		`<ComponentStream component="Linear" name="X2"><Samples>` +
		`<Position dataItemId="Xabs" sequence="9">2.0</Position></Samples></ComponentStream>` +
		`</DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	idx := s.Index()
	if idx.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", idx.Duplicates)
	}
	it, _ := idx.Lookup("Xabs")
	if it.Sequence != 9 || it.Value() != "2.0" {
		t.Fatalf("merged item = %+v (want seq 9 / 2.0)", it)
	}
}

func TestParseStreamsConditionMergeTakesWorst(t *testing.T) {
	// 同一条件 ID 有 Normal 与 Fault：取 FAULT。
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Controller" name="c"><Conditions>` +
		`<System dataItemId="alm" type="SYSTEM" sequence="1"><Normal sequence="1">ok</Normal></System>` +
		`<System dataItemId="alm" type="SYSTEM" sequence="2"><Fault sequence="2">bad</Fault></System>` +
		`</Conditions></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, _ := s.Index().Lookup("alm")
	if it.Condition != ConditionFault {
		t.Fatalf("merged condition = %q, want FAULT", it.Condition)
	}
	if s.Index().Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", s.Index().Duplicates)
	}
}

func TestParseStreamsErrorDocument(t *testing.T) {
	doc := `<MTConnectStreams><Header/><Streams/></MTConnectStreams>` // valid but empty
	_, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("empty streams should parse: %v", err)
	}

	errDoc := `<?xml version="1.0"?><MTConnectError><Header version="2.0"/>` +
		`<Errors><Error errorCode="OUT_OF_RANGE">from=99999 exceeds buffer</Error></Errors></MTConnectError>`
	_, err = ParseStreams([]byte(errDoc))
	if err == nil {
		t.Fatal("error document should return an error")
	}
	if !strings.Contains(err.Error(), "OUT_OF_RANGE") {
		t.Fatalf("error = %v, want code in message", err)
	}
}

func TestParseStreamsWrongRoot(t *testing.T) {
	_, err := ParseStreams([]byte(`<html><body>not xml streams</body></html>`))
	if err == nil {
		t.Fatal("wrong root element should error")
	}
}

func TestParseStreamsEmptyBody(t *testing.T) {
	if _, err := ParseStreams([]byte("")); err == nil {
		t.Fatal("empty body should error")
	}
	if _, err := ParseStreams([]byte("   \n  ")); err == nil {
		t.Fatal("whitespace body should error")
	}
}

func TestParseStreamsMalformedXML(t *testing.T) {
	if _, err := ParseStreams([]byte("<MTConnectStreams><Header></MTConnectStreams>")); err == nil {
		t.Fatal("unclosed element should error")
	}
}

func TestParseStreamsIgnoresNamespace(t *testing.T) {
	// 带命名空间前缀的元素仍要按本地名匹配。
	doc := `<mt:MTConnectStreams xmlns:mt="urn:mtconnect.org:MTConnectStreams:2.0">` +
		`<mt:Header/><mt:Streams><mt:DeviceStream name="d" uuid="u">` +
		`<mt:ComponentStream component="Linear" name="X">` +
		`<mt:Samples><mt:Position dataItemId="Xabs" sequence="1">5</mt:Position></mt:Samples>` +
		`</mt:ComponentStream></mt:DeviceStream></mt:Streams></mt:MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, ok := s.Index().Lookup("Xabs")
	if !ok || it.Value() != "5" {
		t.Fatalf("namespaced item = %+v ok=%v", it, ok)
	}
}

func TestParseStreamsSingleConditionContainer(t *testing.T) {
	// 1.1 时代的单数容器 <Condition>（不是 <Conditions>）。
	doc := `<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">` +
		`<ComponentStream component="Controller" name="c"><Condition>` +
		`<Fault dataItemId="alm" type="SYSTEM" sequence="1">bad</Fault>` +
		`</Condition></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	s, err := ParseStreams([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it, ok := s.Index().Lookup("alm")
	if !ok || it.Condition != ConditionFault {
		t.Fatalf("single Condition container item = %+v ok=%v", it, ok)
	}
}

func TestParseStreamsISO8859_1(t *testing.T) {
	// ISO-8859-1 声明 + 一个重音字符（0xE9 = é）。解码器要转成 UTF-8。
	// 0xE9 in latin-1 is 'é' -> UTF-8 0xC3 0xA9
	raw := append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?>`+
		`<MTConnectStreams><Header/><Streams><DeviceStream name="d" uuid="u">`+
		`<ComponentStream component="Controller" name="c"><Events>`+
		`<Program dataItemId="prog" sequence="1">`),
		0xE9)
	raw = append(raw, []byte(`</Program></Events></ComponentStream></DeviceStream></Streams></MTConnectStreams>`)...)
	s, err := ParseStreams(raw)
	if err != nil {
		t.Fatalf("parse iso-8859-1: %v", err)
	}
	it, ok := s.Index().Lookup("prog")
	if !ok {
		t.Fatal("prog item missing")
	}
	// é 的 UTF-8 编码是 0xC3 0xA9。
	if it.Text != "\u00e9" {
		t.Fatalf("decoded text = %q, want é", it.Text)
	}
}

func TestParseStreamsUnsupportedCharset(t *testing.T) {
	// 不支持的字符集（UTF-16）必须报错，而不是静默产生乱码。
	raw := []byte(`<?xml version="1.0" encoding="UTF-16"?><MTConnectStreams/>`)
	if _, err := ParseStreams(raw); err == nil {
		t.Fatal("unsupported charset should error")
	}
}

func TestParseStreamsUTF8BOM(t *testing.T) {
	// 带 UTF-8 BOM 的文档要能解析。
	raw := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte(`<?xml version="1.0"?><MTConnectStreams><Header/><Streams>`+
			`<DeviceStream name="d" uuid="u"><ComponentStream component="Linear" name="X">`+
			`<Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples>`+
			`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`)...)
	s, err := ParseStreams(raw)
	if err != nil {
		t.Fatalf("parse bom: %v", err)
	}
	if _, ok := s.Index().Lookup("Xabs"); !ok {
		t.Fatal("BOM-prefixed document lost items")
	}
}

func TestParseDevices(t *testing.T) {
	doc := `<?xml version="1.0"?>
<MTConnectDevices>
  <Header version="2.0" instanceId="1"/>
  <Devices>
    <Device id="VMC-3AXIS-001" name="VMC-3AXIS" uuid="VMC-3AXIS-001">
      <Description>三轴立式加工中心</Description>
      <Manufacturer>ACME</Manufacturer>
      <Model>VMC-750</Model>
      <SerialNumber>SN123</SerialNumber>
      <DataItems>
        <DataItem category="EVENT" type="AVAILABILITY" id="avail"/>
      </DataItems>
      <Components>
        <Linear id="X" name="X" type="LINEAR">
          <DataItems>
            <DataItem category="SAMPLE" type="POSITION" id="Xabs" subType="ACTUAL" units="MILLIMETER"/>
          </DataItems>
        </Linear>
      </Components>
    </Device>
  </Devices>
</MTConnectDevices>`
	devs, err := ParseDevices([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(devs.Devices) != 1 {
		t.Fatalf("devices = %d", len(devs.Devices))
	}
	d := devs.Devices[0]
	if d.UUID != "VMC-3AXIS-001" || d.Manufacturer != "ACME" || d.Model != "VMC-750" {
		t.Fatalf("device = %+v", d)
	}
	// 组件数据项的组件路径要拼出来。
	found := map[string]bool{}
	for _, di := range d.DataItems {
		found[di.ID+"@"+di.Component] = true
	}
	if !found["avail@VMC-3AXIS"] {
		t.Fatalf("device-level item component path wrong: %+v", d.DataItems)
	}
	if !found["Xabs@X"] {
		t.Fatalf("component item path wrong: %+v", d.DataItems)
	}
	// 类别/类型要归一。
	for _, di := range d.DataItems {
		if di.Category == CategoryUnknown {
			t.Fatalf("category not normalized: %+v", di)
		}
	}
}

func TestParseErrorDocument(t *testing.T) {
	raw := `<?xml version="1.0"?><MTConnectError><Header version="2.0"/>` +
		`<Errors><Error errorCode="INTERNAL_ERROR">boom</Error></Errors></MTConnectError>`
	e, ok := ParseError([]byte(raw))
	if !ok {
		t.Fatal("should recognize error document")
	}
	if e.Code != "INTERNAL_ERROR" || len(e.Messages) != 1 || e.Messages[0] != "boom" {
		t.Fatalf("error doc = %+v", e)
	}
	if !strings.Contains(e.Error(), "INTERNAL_ERROR") || !strings.Contains(e.Error(), "boom") {
		t.Fatalf("Error() = %q", e.Error())
	}
	// 非错误文档返回 ok=false。
	if _, ok := ParseError([]byte(`<MTConnectStreams/>`)); ok {
		t.Fatal("streams doc must not be an error document")
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"2026-01-01T12:00:00Z", true},
		{"2026-01-01T12:00:00.123456Z", true},
		{"2026-01-01T12:00:00", true},
		{"", false},
		{"not-a-time", false},
	}
	for _, tc := range cases {
		_, ok := parseTimestamp(tc.in)
		if ok != tc.want {
			t.Fatalf("parseTimestamp(%q) ok=%v, want %v", tc.in, ok, tc.want)
		}
	}
}
