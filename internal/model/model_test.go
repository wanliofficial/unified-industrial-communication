package model

import (
	"encoding/json"
	"math"
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
		"bad id":          func(p *Point) { p.ID = "P 1" },
		"empty area":      func(p *Point) { p.Address.Area = "" },
		"negative offset": func(p *Point) { p.Address.Offset = -1 },
		"length too big":  func(p *Point) { p.Address.Length = 126 },
		"bad bit":         func(p *Point) { p.Address.Bit = 16 },
		"bit on non-bool": func(p *Point) { p.Address.Bit = 4 },
		"bad type":        func(p *Point) { p.DataType = "float128" },
		"bad byte order":  func(p *Point) { p.ByteOrder = "XYZW" },
		"short interval":  func(p *Point) { p.Interval = Duration(10 * time.Millisecond) },
		"mixed fields":    func(p *Point) { p.Address.Path = "DB1" },
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
