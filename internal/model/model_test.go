package model

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func validPoint() Point {
	return Point{
		ID: "p1", DataType: TypeFloat32, Enabled: true,
		Address: Address{Kind: AddressRegister, Area: "holding", Offset: 0, Length: 2, Bit: -1},
	}
}

func validDevice() Device {
	return Device{
		ID: "d1", ChannelID: "ch1", Interval: Duration(time.Second), Enabled: true,
		Points: []Point{validPoint()},
	}
}

func TestPointValidate(t *testing.T) {
	if err := validPoint().Validate(); err != nil {
		t.Fatalf("valid point: %v", err)
	}
	cases := map[string]func(*Point){
		"bad id":               func(p *Point) { p.ID = "P 1" },
		"empty area":           func(p *Point) { p.Address.Area = "" },
		"negative offset":      func(p *Point) { p.Address.Offset = -1 },
		"length too big":       func(p *Point) { p.Address.Length = 126 },
		"bad bit":              func(p *Point) { p.Address.Bit = 16 },
		"bit on non-bool":      func(p *Point) { p.Address.Bit = 4 },
		"bit zero on non-bool": func(p *Point) { p.Address.Bit = 0 },
		"bad type":             func(p *Point) { p.DataType = "float128" },
		"bad byte order":       func(p *Point) { p.ByteOrder = "XYZW" },
		"short interval":       func(p *Point) { p.Interval = Duration(10 * time.Millisecond) },
		"mixed fields":         func(p *Point) { p.Address.Path = "DB1" },
	}
	for name, mut := range cases {
		p := validPoint()
		mut(&p)
		if err := p.Validate(); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	// bool + bit 合法；bit=-1 的普通数值点也合法。
	p := validPoint()
	p.DataType = TypeBool
	p.Address.Bit = 3
	if err := p.Validate(); err != nil {
		t.Fatalf("bool bit: %v", err)
	}
}

func TestSymbolAndObjectAddresses(t *testing.T) {
	sym := Point{ID: "s", DataType: TypeFloat32, Enabled: true,
		Address: Address{Kind: AddressSymbol, Path: "DB1.DBW0", Index: -1}}
	if err := sym.Validate(); err != nil {
		t.Fatalf("symbol: %v", err)
	}
	sym.Address.Path = "has space"
	if err := sym.Validate(); err == nil {
		t.Fatal("path with space should fail")
	}
	obj := Point{ID: "o", DataType: TypeUint32, Enabled: true,
		Address: Address{Kind: AddressObject, ObjectType: "binary_input", Instance: 1, Property: "present_value"}}
	if err := obj.Validate(); err != nil {
		t.Fatalf("object: %v", err)
	}
	obj.Address.Property = ""
	if err := obj.Validate(); err == nil {
		t.Fatal("object without property should fail")
	}
}

func TestDeviceValidate(t *testing.T) {
	if err := validDevice().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Device){
		"bad id":         func(d *Device) { d.ID = "D 1" },
		"no channel":     func(d *Device) { d.ChannelID = "" },
		"short interval": func(d *Device) { d.Interval = Duration(time.Millisecond) },
		"duplicate point": func(d *Device) {
			d.Points = append(d.Points, d.Points[0])
		},
		"bad point": func(d *Device) { d.Points[0].DataType = "" },
	}
	for name, mut := range cases {
		d := validDevice()
		mut(&d)
		if err := d.Validate(); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestChannelValidate(t *testing.T) {
	c := Channel{ID: "ch1", Protocol: "modbus-tcp", Transport: Transport{Kind: "tcp", Address: "127.0.0.1:502"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Channel){
		"no protocol":    func(c *Channel) { c.Protocol = "" },
		"tcp no addr":    func(c *Channel) { c.Transport.Address = "" },
		"serial no port": func(c *Channel) { c.Transport = Transport{Kind: "serial"} },
		"bad kind":       func(c *Channel) { c.Transport.Kind = "carrier-pigeon" },
		"negative baud":  func(c *Channel) { c.Transport = Transport{Kind: "serial", Port: "/dev/ttyS0", Baud: -1} },
	} {
		cc := c
		mut(&cc)
		if err := cc.Validate(); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	// 并发与超时有默认值且被夹紧。
	c.Concurrency = 0
	if c.EffectiveConcurrency() != 4 {
		t.Fatalf("default concurrency = %d", c.EffectiveConcurrency())
	}
	c.Concurrency = 1000
	if c.EffectiveConcurrency() != 64 {
		t.Fatalf("clamped concurrency = %d", c.EffectiveConcurrency())
	}
	c.Timeout = 0
	if c.EffectiveTimeout() != 3*time.Second {
		t.Fatalf("default timeout = %v", c.EffectiveTimeout())
	}
}

func TestValueSemantics(t *testing.T) {
	if !NumValue(TypeFloat32, 1.5).Equal(NumValue(TypeFloat32, 1.5)) {
		t.Fatal("equal numbers")
	}
	if NumValue(TypeFloat32, 1.5).Equal(NumValue(TypeFloat32, 1.6)) {
		t.Fatal("different numbers")
	}
	if NumValue(TypeFloat32, 1).Equal(NumValue(TypeInt32, 1)) {
		t.Fatal("type must participate in equality")
	}
	if !BoolValue(true).Equal(BoolValue(true)) || BoolValue(true).Equal(BoolValue(false)) {
		t.Fatal("bool equality")
	}
	if !TextValue(TypeString, "x").Equal(TextValue(TypeString, "x")) {
		t.Fatal("text equality")
	}
	if v, ok := BoolValue(true).AsFloat(); !ok || v != 1 {
		t.Fatal("bool as float")
	}
	if _, ok := TextValue(TypeString, "x").AsFloat(); ok {
		t.Fatal("string must not convert to number")
	}
	if NumValue(TypeUint16, 42).String() != "42" {
		t.Fatalf("string = %q", NumValue(TypeUint16, 42).String())
	}
	if BoolValue(false).String() != "false" {
		t.Fatal("bool string")
	}
}

func TestTypeWidths(t *testing.T) {
	cases := []struct {
		t    DataType
		regs int
	}{
		{TypeBool, 1}, {TypeInt16, 1}, {TypeUint16, 1}, {TypeInt32, 2},
		{TypeFloat32, 2}, {TypeFloat64, 4}, {TypeInt64, 4}, {TypeBCD32, 2},
	}
	for _, c := range cases {
		if got := c.t.Registers(); got != c.regs {
			t.Fatalf("%s registers = %d, want %d", c.t, got, c.regs)
		}
	}
	if TypeBool.IsNumeric() || !TypeFloat64.IsNumeric() {
		t.Fatal("numeric classification")
	}
}

func TestQualitySemantics(t *testing.T) {
	if !QualityGood.IsUsable() || !QualityUncertain.IsUsable() {
		t.Fatal("good/uncertain must be usable")
	}
	for _, q := range []Quality{QualityStale, QualityBad, QualityTimeout, QualityCommError,
		QualityAddressError, QualityNotConnected, QualityDisabled} {
		if q.IsUsable() {
			t.Fatalf("%s must not be usable", q)
		}
		if !q.Valid() {
			t.Fatalf("%s should be a known quality", q)
		}
	}
	if Quality("nonsense").Valid() {
		t.Fatal("unknown quality must be invalid")
	}
}

func TestSampleValidateAndKey(t *testing.T) {
	s := Sample{DeviceID: "d1", PointID: "p1", Value: NumValue(TypeFloat32, 1), Quality: QualityGood}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Key() != "d1/p1" {
		t.Fatalf("key = %q", s.Key())
	}
	bad := s
	bad.PointID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("missing point must fail")
	}
	bad = s
	bad.Quality = "weird"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown quality must fail")
	}
	bad = s
	bad.Value = Value{}
	if err := bad.Validate(); err == nil {
		t.Fatal("good quality without typed value must fail")
	}
}

func TestWindowStatsAvg(t *testing.T) {
	cases := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{Count: 0}, 0}, // 绝不返回 NaN
		{WindowStats{Count: 1, Sum: 7, Min: 7, Max: 7}, 7},
		{WindowStats{Count: 4, Sum: 40, Min: 5, Max: 15}, 10},
	}
	for _, c := range cases {
		if got := c.stats.Avg(); got != c.want {
			t.Errorf("Avg(%+v) = %v, want %v", c.stats, got, c.want)
		}
	}
}

func TestDurationJSON(t *testing.T) {
	type wrapper struct {
		D Duration `json:"d"`
	}
	var w wrapper
	if err := json.Unmarshal([]byte(`{"d":"1.5s"}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.D.Duration() != 1500*time.Millisecond {
		t.Fatalf("duration = %v", w.D.Duration())
	}
	if err := json.Unmarshal([]byte(`{"d":1000000000}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.D.Duration() != time.Second {
		t.Fatalf("nanoseconds form = %v", w.D.Duration())
	}
	if err := json.Unmarshal([]byte(`{"d":"soon"}`), &w); err == nil {
		t.Fatal("invalid duration must fail")
	}
	b, err := json.Marshal(wrapper{D: Duration(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"d":"2s"}` {
		t.Fatalf("marshal = %s", b)
	}
}

func TestByteOrderValid(t *testing.T) {
	for _, o := range []ByteOrder{"", OrderABCD, OrderCDAB, OrderBADC, OrderDCBA} {
		if !o.Valid() {
			t.Fatalf("%q should be valid", o)
		}
	}
	if ByteOrder("ABCDE").Valid() {
		t.Fatal("unknown order accepted")
	}
}

func TestPointLengthAndEffectiveValues(t *testing.T) {
	p := Point{ID: "p", DataType: TypeFloat32,
		Address: Address{Kind: AddressRegister, Area: "holding", Length: 0}}
	if p.Length() != 2 {
		t.Fatalf("derived length = %d", p.Length())
	}
	p.Address.Length = 3
	if p.Length() != 3 {
		t.Fatalf("explicit length = %d", p.Length())
	}
	p.Scale = 0
	if p.EffectiveScale() != 1 {
		t.Fatal("empty scale must be 1")
	}
	p.Scale = 2.5
	if p.EffectiveScale() != 2.5 {
		t.Fatal("scale lost")
	}
	d := Device{Interval: Duration(time.Second)}
	p.Interval = 0
	if p.EffectiveInterval(d) != time.Second {
		t.Fatal("device interval fallback")
	}
	p.Interval = Duration(2 * time.Second)
	if p.EffectiveInterval(d) != 2*time.Second {
		t.Fatal("point interval override")
	}
	d.Timeout = 0
	if d.TimeoutOrDefault() != 3*time.Second {
		t.Fatal("default timeout")
	}
	if math.Abs(NumValue(TypeFloat32, 0.1).Num-0.1) > 1e-12 {
		t.Fatal("float value corrupted")
	}
}

// DB 号是 S7 地址命名空间的一部分：它必须与 area 联动校验，否则
// DB1.DBW0 会被当成 DB0（不存在的块）或与 DB2 混为一谈。
func TestAddressDBValidateAndCanonical(t *testing.T) {
	ok := Address{Kind: AddressRegister, Area: "db", DB: 3, Offset: 0, Length: 2, Bit: -1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid db address: %v", err)
	}
	if got := ok.Canonical(); got != "db3:0:2" {
		t.Fatalf("canonical = %q", got)
	}
	// 不同 DB、相同偏移必须产生不同的规范地址，否则缓存键会撞。
	other := ok
	other.DB = 4
	if other.Canonical() == ok.Canonical() {
		t.Fatal("canonical must include db number")
	}

	bad := map[string]Address{
		"db area without db": {Kind: AddressRegister, Area: "db", Offset: 0, Bit: -1},
		"db on non-db area":  {Kind: AddressRegister, Area: "merker", DB: 2, Offset: 0, Bit: -1},
		"negative db":        {Kind: AddressRegister, Area: "db", DB: -1, Offset: 0, Bit: -1},
	}
	for name, a := range bad {
		if err := a.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

// S7 的机架/槽必须能配置：S7-1200/1500 在槽 1，S7-300/400 在槽 2。
func TestChannelRackSlotDefaults(t *testing.T) {
	var c Channel
	if c.EffectiveRack() != 0 || c.EffectiveSlot() != 2 {
		t.Fatalf("defaults = rack %d slot %d, want 0/2", c.EffectiveRack(), c.EffectiveSlot())
	}
	c = Channel{Rack: 1, Slot: 4}
	if c.EffectiveRack() != 1 || c.EffectiveSlot() != 4 {
		t.Fatalf("explicit = rack %d slot %d", c.EffectiveRack(), c.EffectiveSlot())
	}
	for _, bad := range []Channel{
		{ID: "c", Protocol: "s7", Transport: Transport{Kind: "tcp", Address: "h:102"}, Rack: 8},
		{ID: "c", Protocol: "s7", Transport: Transport{Kind: "tcp", Address: "h:102"}, Slot: 32},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("rack/slot out of range must be rejected: %+v", bad)
		}
	}
}

// 映射宽度必须由点位类型确定，不能靠调用方猜。
// string/bytes 的 length 是「寄存器个数」，这是 Modbus 从站映射最容易配错
// 的地方：把 10 写成 10 个字符，会占掉 10 个寄存器而不是 5 个。
func TestPointRegisterWidth(t *testing.T) {
	cases := []struct {
		name string
		pt   Point
		want int
	}{
		{"bool", Point{DataType: TypeBool}, 1},
		{"uint16", Point{DataType: TypeUint16}, 1},
		{"int16 no length", Point{DataType: TypeInt16}, 1},
		{"float32", Point{DataType: TypeFloat32}, 2},
		{"float64", Point{DataType: TypeFloat64}, 4},
		{"uint32 explicit", Point{DataType: TypeUint32, Address: Address{Kind: AddressRegister, Length: 3}}, 3},
		{"string 10 regs", Point{DataType: TypeString, Address: Address{Kind: AddressRegister, Length: 10}}, 10},
		{"bytes 4 regs", Point{DataType: TypeBytes, Address: Address{Kind: AddressRegister, Length: 4}}, 4},
		{"string no length", Point{DataType: TypeString}, 1},
	}
	for _, c := range cases {
		if got := c.pt.RegisterWidth(); got != c.want {
			t.Fatalf("%s: width = %d, want %d", c.name, got, c.want)
		}
	}
}

// 从站校验要挡住三类错误：地址重叠、越界、可写性与点位不一致。
// 这些错误如果放到运行期，表现是「谁后写谁生效」或「写下去没反应」，
// 几乎不可能靠日志定位。
func TestSlaveValidateBoundaries(t *testing.T) {
	points := map[string]Point{
		"plc/u16": {ID: "u16", DataType: TypeUint16},
		"plc/f32": {ID: "f32", DataType: TypeFloat32},
		"plc/ro":  {ID: "ro", DataType: TypeUint16},
		"plc/rw":  {ID: "rw", DataType: TypeUint16, Writable: true},
	}
	base := func() Slave {
		return Slave{
			ID: "scada", Listen: "127.0.0.1:1502",
			Units: []SlaveUnit{{
				UnitID: 1,
				Mappings: []SlaveMapping{
					{Point: "plc/u16", Area: MappingHolding, Offset: 0},
				},
			}},
		}
	}
	if err := base().Validate(points); err != nil {
		t.Fatalf("valid: %v", err)
	}

	// 边界：65535 处放 1 个寄存器刚好占满地址空间，合法。
	edge := base()
	edge.Units[0].Mappings[0].Offset = 65535
	if err := edge.Validate(points); err != nil {
		t.Fatalf("last register must be addressable: %v", err)
	}
	edge.Units[0].Mappings[0].Offset = 65536
	if err := edge.Validate(points); err == nil {
		t.Fatal("offset 65536 must be rejected")
	}

	// 65535 处放 float32（2 寄存器）会越界。
	over := base()
	over.Units[0].Mappings[0] = SlaveMapping{Point: "plc/f32", Area: MappingHolding, Offset: 65535}
	if err := over.Validate(points); err == nil {
		t.Fatal("float32 at 65535 overflows address space")
	}

	// 同一区不允许重叠；不同区允许同偏移。
	same := base()
	same.Units[0].Mappings = append(same.Units[0].Mappings,
		SlaveMapping{Point: "plc/f32", Area: MappingHolding, Offset: 0})
	if err := same.Validate(points); err == nil {
		t.Fatal("overlap within an area must be rejected")
	}
	other := base()
	other.Units[0].Mappings = append(other.Units[0].Mappings,
		SlaveMapping{Point: "plc/f32", Area: MappingInput, Offset: 0})
	if err := other.Validate(points); err != nil {
		t.Fatalf("same offset in a different area is legal: %v", err)
	}

	// 可写映射必须两端都允许写。
	roArea := base()
	roArea.Units[0].Mappings[0] = SlaveMapping{Point: "plc/rw", Area: MappingInput, Offset: 0, Writable: true}
	if err := roArea.Validate(points); err == nil {
		t.Fatal("writable mapping in input area must be rejected")
	}
	roPoint := base()
	roPoint.Units[0].Mappings[0] = SlaveMapping{Point: "plc/ro", Area: MappingHolding, Offset: 0, Writable: true}
	if err := roPoint.Validate(points); err == nil {
		t.Fatal("writable mapping of a read-only point must be rejected")
	}

	// 单元与从站层面的重复。
	dupUnit := base()
	dupUnit.Units = append(dupUnit.Units, dupUnit.Units[0])
	if err := dupUnit.Validate(points); err == nil {
		t.Fatal("duplicate unitId must be rejected")
	}
	var noUnits Slave
	noUnits.ID, noUnits.Listen = "x", ":1502"
	if err := noUnits.Validate(points); err == nil {
		t.Fatal("slave without units must be rejected")
	}

	// 默认连接数：0 → 8，超上限截到 64。
	if got := (Slave{}).EffectiveMaxConnections(); got != 8 {
		t.Fatalf("default max connections = %d", got)
	}
	if got := (Slave{MaxConnections: 1000}).EffectiveMaxConnections(); got != 64 {
		t.Fatalf("max connections cap = %d", got)
	}
}

// 设备标识直接进 FC43 响应，长度与字符集在这里挡住：运行期才发现超长
// 就只能返回异常码，而上位机看到的是「这台设备不支持识别」。
func TestSlaveIdentityValidate(t *testing.T) {
	points := map[string]Point{"plc/u16": {ID: "u16", DataType: TypeUint16}}
	base := func() Slave {
		return Slave{
			ID: "scada", Listen: "127.0.0.1:1502",
			Units: []SlaveUnit{{
				UnitID:   1,
				Mappings: []SlaveMapping{{Point: "plc/u16", Area: MappingHolding, Offset: 0}},
			}},
		}
	}

	ok := base()
	ok.Identity = &SlaveIdentity{
		VendorName: "ACME", ProductCode: "PLC-2000", MajorMinorRevision: "2.3",
		VendorURL: "https://acme.example", ProductName: "ACME Controller",
		ModelName: "PLC-2000-X", UserApplicationName: "Line 3",
	}
	if err := ok.Validate(points); err != nil {
		t.Fatalf("valid identity: %v", err)
	}

	cases := map[string]func(*SlaveIdentity){
		"vendor too long":   func(i *SlaveIdentity) { i.VendorName = strings.Repeat("x", 121) },
		"revision too long": func(i *SlaveIdentity) { i.MajorMinorRevision = strings.Repeat("1", 33) },
		"control char":      func(i *SlaveIdentity) { i.ProductCode = "PLC\x00" },
		"del char":          func(i *SlaveIdentity) { i.ModelName = "PLC\x7f" },
	}
	for name, mut := range cases {
		s := base()
		s.Identity = &SlaveIdentity{VendorName: "ACME"}
		mut(s.Identity)
		if err := s.Validate(points); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}

	// 边界值恰好合法：120/32 字节都不是「超过」。
	edge := base()
	edge.Identity = &SlaveIdentity{
		VendorName:         strings.Repeat("v", 120),
		MajorMinorRevision: strings.Repeat("r", 32),
	}
	if err := edge.Validate(points); err != nil {
		t.Fatalf("boundary identity: %v", err)
	}
}

// 计算点地址：必须带表达式、不接受任何物理字段，且要有稳定的规范形式。
// 规范形式进入缓存键与配置版本计算，因此表达式本身必须是它的一部分：
// 改一个常数就意味着「这个点位换了算法」，不能被当成同一个版本。
func TestComputedAddress(t *testing.T) {
	a := Address{Kind: AddressComputed, Expr: "line-a:power * 2"}
	if err := a.Validate(); err != nil {
		t.Fatalf("valid computed address: %v", err)
	}
	if got := a.Canonical(); got != "expr:line-a:power * 2" {
		t.Fatalf("canonical = %q", got)
	}
	other := a
	other.Expr = "line-a:power * 3"
	if other.Canonical() == a.Canonical() {
		t.Fatal("canonical must include the expression")
	}

	bad := map[string]Address{
		"no expr":      {Kind: AddressComputed},
		"blank expr":   {Kind: AddressComputed, Expr: "   "},
		"too long":     {Kind: AddressComputed, Expr: strings.Repeat("1", 1025)},
		"control char": {Kind: AddressComputed, Expr: "1 +\n2"},
		// 物理字段混填是最容易犯的错：把计算点当寄存器写，或者把
		// 寄存器改成计算点时忘了删掉 offset。
		"offset":  {Kind: AddressComputed, Expr: "1 + 1", Offset: 4},
		"area":    {Kind: AddressComputed, Expr: "1 + 1", Area: "holding"},
		"path":    {Kind: AddressComputed, Expr: "1 + 1", Path: "DB1"},
		"length":  {Kind: AddressComputed, Expr: "1 + 1", Length: 2},
		"bit":     {Kind: AddressComputed, Expr: "1 + 1", Bit: 3},
		"db":      {Kind: AddressComputed, Expr: "1 + 1", DB: 1},
		"file id": {Kind: AddressComputed, Expr: "1 + 1", FileID: 2},
	}
	for name, addr := range bad {
		if err := addr.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

// 计算点只能是只读的数值/布尔点。允许可写或 string/BCD 会在运行时
// 产生一条永远失败的写入路径，或一个没有任何运算符可以消费的值。
func TestComputedPointRules(t *testing.T) {
	mk := func(dt DataType, writable bool) Point {
		return Point{ID: "p", DataType: dt, Writable: writable, Enabled: true,
			Address: Address{Kind: AddressComputed, Expr: "1 + 1"}}
	}
	for _, dt := range []DataType{TypeBool, TypeInt16, TypeUint16, TypeInt32, TypeUint32,
		TypeInt64, TypeUint64, TypeFloat32, TypeFloat64} {
		if err := mk(dt, false).Validate(); err != nil {
			t.Fatalf("%s: %v", dt, err)
		}
	}
	if err := mk(TypeFloat64, true).Validate(); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("writable computed point must be rejected, got %v", err)
	}
	for _, dt := range []DataType{TypeString, TypeBytes, TypeBCD16, TypeBCD32} {
		if err := mk(dt, false).Validate(); err == nil {
			t.Fatalf("%s computed point must be rejected", dt)
		}
	}
}

// Canonical 是缓存键与北向节点名的唯一来源，驱动不得自行实现。每种寻址
// 都必须产出不同且稳定的字符串：两个不同的地址得到同一个键，会导致
// 读回来的值张冠李戴——这类错误在运行时完全看不出来。
func TestCanonicalCoversEveryKind(t *testing.T) {
	cases := []struct {
		a    Address
		want string
	}{
		{Address{Kind: AddressRegister, Area: "holding", Offset: 3, Length: 2, Bit: -1}, "holding:3:2"},
		{Address{Kind: AddressRegister, Area: "coil", Offset: 3, Length: 1, Bit: 3}, "coil:3.3:1"},
		{Address{Kind: AddressRegister, Area: "db", DB: 2, Offset: 3, Length: 2, Bit: -1}, "db2:3:2"},
		{Address{Kind: AddressSymbol, Path: "DB1.DBW0", Index: -1}, "DB1.DBW0"},
		{Address{Kind: AddressSymbol, Path: "array", Index: 4}, "array[4]"},
		{Address{Kind: AddressSymbol, Path: "device0", Index: -1, Selector: "temp"}, "device0#temp"},
		// ADS 的 STRING/bytes 宽度由点表给出：长度必须进入 canonical，
		// 否则 `NAME:80` 与 `NAME:81` 会被当成同一个缓存键。
		{Address{Kind: AddressSymbol, Path: "MAIN.sRecipe", Index: -1, Length: 81}, "MAIN.sRecipe:81"},
		{Address{Kind: AddressObject, ObjectType: "binary_input", Instance: 2, Property: "present_value", PropIndex: -1}, "binary_input:2.present_value"},
		{Address{Kind: AddressObject, ObjectType: "analog_input", Instance: 2, Property: "priority_array", PropIndex: 3}, "analog_input:2.priority_array[3]"},
		{Address{Kind: AddressFile, FileID: 3, RecordID: 5, Offset: 7}, "file:3/5:7"},
		{Address{Kind: AddressComputed, Expr: "1 + 2"}, "expr:1 + 2"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		got := c.a.Canonical()
		if got != c.want {
			t.Errorf("%s canonical = %q, want %q", c.a.Kind, got, c.want)
		}
		if seen[got] {
			t.Errorf("duplicate canonical key %q", got)
		}
		seen[got] = true
	}
	if got := (Address{Kind: AddressKind("nope")}).Canonical(); got != "" {
		t.Errorf("unknown kind canonical = %q, want empty", got)
	}
}

// 订阅的两个周期字段只在 subscribe=true 时有意义。配了却没开订阅是
// 一个没有线索的静默错误（「我改了周期为什么没生效」），必须在配置
// 校验阶段挡住；而 0 表示「跟随设备采集周期」，是合法的。
func TestChannelSubscriptionFields(t *testing.T) {
	base := Channel{ID: "c", Protocol: "opcua", Transport: Transport{Kind: "tcp", Address: "h:4840"}}

	off := base
	off.Subscribe = false
	if err := off.Validate(); err != nil {
		t.Fatalf("polling channel: %v", err)
	}
	on := base
	on.Subscribe = true
	on.PublishingInterval = Duration(200 * time.Millisecond)
	on.SamplingInterval = Duration(100 * time.Millisecond)
	if err := on.Validate(); err != nil {
		t.Fatalf("subscribed channel: %v", err)
	}
	// 0 是「跟随设备采集周期」的合法写法。
	zero := base
	zero.Subscribe = true
	if err := zero.Validate(); err != nil {
		t.Fatalf("zero intervals: %v", err)
	}

	bad := []Channel{
		{ID: "c", Protocol: "opcua", Transport: Transport{Kind: "tcp", Address: "h:4840"}, PublishingInterval: Duration(time.Second)},
		{ID: "c", Protocol: "opcua", Transport: Transport{Kind: "tcp", Address: "h:4840"}, SamplingInterval: Duration(time.Second)},
		{ID: "c", Protocol: "opcua", Transport: Transport{Kind: "tcp", Address: "h:4840"}, Subscribe: true, PublishingInterval: Duration(-time.Second)},
		{ID: "c", Protocol: "opcua", Transport: Transport{Kind: "tcp", Address: "h:4840"}, Subscribe: true, SamplingInterval: Duration(-time.Second)},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("must be rejected: %+v", c)
		}
	}
}

// 串口从站端点：listen 与 serial 必须恰好选一个，串口参数必须在配置期
// 就拦住。串口参数错了不会报错，只会「读不到东西」——那是最难排查的一类
// 故障，必须在加载配置时就失败。
func TestSlaveSerialEndpoint(t *testing.T) {
	points := map[string]Point{"plc/u16": {ID: "u16", DataType: TypeUint16}}
	units := []SlaveUnit{{
		UnitID:   1,
		Mappings: []SlaveMapping{{Point: "plc/u16", Area: MappingHolding, Offset: 0}},
	}}
	base := func() Slave {
		return Slave{
			ID:     "scada",
			Serial: &SlaveSerial{Port: "/dev/ttyUSB0", Framing: "rtu"},
			Units:  units,
		}
	}
	if err := base().Validate(points); err != nil {
		t.Fatalf("valid serial slave: %v", err)
	}
	if got := base().Endpoint(); got != "/dev/ttyUSB0 rtu" {
		t.Fatalf("endpoint = %q", got)
	}

	// 两个都给：实际监听在哪取决于实现里 if 的先后，是配置期必须拒绝的歧义。
	both := base()
	both.Listen = "127.0.0.1:1502"
	if err := both.Validate(points); err == nil {
		t.Fatal("listen + serial must be rejected")
	}
	// 两个都不给。
	none := base()
	none.Serial = nil
	if err := none.Validate(points); err == nil {
		t.Fatal("missing endpoint must be rejected")
	}
	// 连接数是 TCP 概念：串口上配了也不会生效，必须报错而不是忽略。
	mc := base()
	mc.MaxConnections = 4
	if err := mc.Validate(points); err == nil {
		t.Fatal("maxConnections on serial must be rejected")
	}

	cases := map[string]func(*SlaveSerial){
		"missing port":  func(s *SlaveSerial) { s.Port = "" },
		"bad framing":   func(s *SlaveSerial) { s.Framing = "modbus" },
		"missing frame": func(s *SlaveSerial) { s.Framing = "" },
		"bad baud":      func(s *SlaveSerial) { s.Baud = 1234 },
		"bad parity":    func(s *SlaveSerial) { s.Parity = "mark" },
		"bad data bits": func(s *SlaveSerial) { s.DataBits = 9 },
		"bad stop bits": func(s *SlaveSerial) { s.StopBits = 3 },
	}
	for name, mut := range cases {
		s := base()
		mut(s.Serial)
		if err := s.Validate(points); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}

	// 边界值：5 数据位、2 停止位、偶校验、921600 都是合法的。
	edge := base()
	edge.Serial = &SlaveSerial{
		Port: "/dev/ttyS1", Framing: "ascii",
		Baud: 921600, DataBits: 5, Parity: "even", StopBits: 2,
	}
	if err := edge.Validate(points); err != nil {
		t.Fatalf("boundary serial params: %v", err)
	}

	// 零值补默认值，且负数不被悄悄改成默认值。
	n := SlaveSerial{Port: "/dev/ttyS0", Framing: "rtu"}.Normalized()
	if n.Baud != 9600 || n.DataBits != 8 || n.Parity != "none" || n.StopBits != 1 {
		t.Fatalf("defaults = %+v", n)
	}
}
