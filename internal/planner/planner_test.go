package planner

import (
	"testing"

	"uic/internal/model"
)

func pt(id string, kind model.AddressKind, area string, offset, length int, dt model.DataType) model.Point {
	return model.Point{
		ID:       id,
		DataType: dt,
		Enabled:  true,
		Address: model.Address{
			Kind: kind, Area: area, Offset: offset, Length: length, Bit: -1,
			Path: area, ObjectType: area, Instance: offset, FileID: offset, RecordID: length,
		},
	}
}

// 相邻寄存器必须合并为一个 span；跨区不合并；容量超限要切分。
func TestBuildMergesAdjacentRegisters(t *testing.T) {
	dev := model.Device{ID: "d1", ChannelID: "c1", Enabled: true, Points: []model.Point{
		pt("a", model.AddressRegister, "holding", 0, 1, model.TypeUint16),
		pt("b", model.AddressRegister, "holding", 1, 1, model.TypeUint16),
		pt("c", model.AddressRegister, "holding", 2, 2, model.TypeUint32),
		pt("x", model.AddressRegister, "input", 0, 1, model.TypeUint16),
	}}
	spans, st := Build(dev, Options{})
	if st.Points != 4 {
		t.Fatalf("points = %d", st.Points)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2: %+v", len(spans), spans)
	}
	// 排序后 holding 段在前（areas 按字典序）。
	h := spans[0]
	if len(h.Points) != 3 {
		t.Fatalf("holding points = %d", len(h.Points))
	}
	if h.Points[0].ID != "a" || h.Points[2].ID != "c" {
		t.Fatalf("unexpected order: %+v", h.Points)
	}
	if st.Coverage != 1 {
		t.Fatalf("coverage = %v, want 1", st.Coverage)
	}
}

// MaxGap 之外必须断开，避免一个 span 覆盖大段无用寄存器。
func TestBuildRespectsMaxGap(t *testing.T) {
	dev := model.Device{ID: "d1", ChannelID: "c1", Enabled: true, Points: []model.Point{
		pt("a", model.AddressRegister, "holding", 0, 1, model.TypeUint16),
		pt("b", model.AddressRegister, "holding", 5, 1, model.TypeUint16),
	}}
	spans, st := Build(dev, Options{MaxGap: 2})
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if st.Coverage != 1 {
		t.Fatalf("coverage = %v", st.Coverage)
	}
	// 空洞 2 时合并；覆盖 6 个寄存器、有效 2 个。
	spans, st = Build(dev, Options{MaxGap: 4})
	if len(spans) != 1 {
		t.Fatalf("spans with gap=4 = %d, want 1", len(spans))
	}
	if st.Coverage >= 1 {
		t.Fatalf("coverage should drop when covering holes, got %v", st.Coverage)
	}
}

// 单 span 不能超过协议上限（寄存器 125）。
func TestBuildSplitsByProtocolLimit(t *testing.T) {
	var points []model.Point
	for i := 0; i < 200; i++ {
		points = append(points, pt(idFor(i), model.AddressRegister, "holding", i, 1, model.TypeUint16))
	}
	dev := model.Device{ID: "d1", ChannelID: "c1", Enabled: true, Points: points}
	spans, _ := Build(dev, Options{MaxGap: 1 << 20})
	if len(spans) < 2 {
		t.Fatalf("spans = %d, want >=2", len(spans))
	}
	for _, s := range spans {
		min, max := spanBounds(s)
		if max-min > 125 {
			t.Fatalf("span covers %d registers (>125)", max-min)
		}
	}
}

// 停用点位不参与规划。
func TestBuildSkipsDisabledPoints(t *testing.T) {
	dev := model.Device{ID: "d1", ChannelID: "c1", Enabled: true, Points: []model.Point{
		pt("a", model.AddressRegister, "holding", 0, 1, model.TypeUint16),
		{ID: "b", Enabled: false, DataType: model.TypeUint16,
			Address: model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 1, Length: 1}},
	}}
	spans, st := Build(dev, Options{})
	if st.Points != 1 || len(spans) != 1 {
		t.Fatalf("points=%d spans=%d", st.Points, len(spans))
	}
}

// 符号地址按根节点分组，对象/文件按实例或记录分组。
func TestBuildGroupsSymbolicAddresses(t *testing.T) {
	sym := func(id, path string) model.Point {
		return model.Point{ID: id, Enabled: true, DataType: model.TypeFloat32,
			Address: model.Address{Kind: model.AddressSymbol, Path: path}}
	}
	dev := model.Device{ID: "d1", ChannelID: "c1", Enabled: true, Points: []model.Point{
		sym("a", "DB1.DBW0"),
		sym("b", "DB1.DBW4"),
		sym("c", "DB2.DBW0"),
	}}
	spans, _ := Build(dev, Options{})
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2: %+v", len(spans), spans)
	}
	if spans[0].ID != "sym:DB1" || len(spans[0].Points) != 2 {
		t.Fatalf("first span = %+v", spans[0])
	}
	if spans[1].ID != "sym:DB2" || len(spans[1].Points) != 1 {
		t.Fatalf("second span = %+v", spans[1])
	}
}

// 位区（线圈）的地址单位是位，合并上限也应该按位计算。
func TestBuildBitAreas(t *testing.T) {
	dev := model.Device{ID: "d1", ChannelID: "c1", Enabled: true, Points: []model.Point{
		pt("c0", model.AddressRegister, "coil", 0, 1, model.TypeBool),
		pt("c1", model.AddressRegister, "coil", 1, 1, model.TypeBool),
		pt("c9", model.AddressRegister, "coil", 9, 1, model.TypeBool),
	}}
	spans, _ := Build(dev, Options{MaxGap: 1})
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1 (bit addresses are 1 unit each)", len(spans))
	}
	if len(spans[0].Points) != 3 {
		t.Fatalf("points = %d", len(spans[0].Points))
	}
}

func TestSymbolRoot(t *testing.T) {
	cases := map[string]string{
		"DB1.DBW0":          "DB1",
		"Program:Main.Tag":  "Program:Main",
		"single":            "single",
		".leading":          ".leading",
		"ns:Program.Main.X": "ns:Program.Main",
	}
	for in, want := range cases {
		if got := symbolRoot(in); got != want {
			t.Fatalf("symbolRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func idFor(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "p0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return "p" + string(b)
}

// 同一个区、同一个偏移但不同 DB 必须分开：否则 DB1.DBW0 与 DB2.DBW0
// 会被合并成一次读取，读回来的值张冠李戴，而且很难从日志看出来。
func TestBuildSeparatesDataBlocks(t *testing.T) {
	mk := func(id string, db, offset int) model.Point {
		p := pt(id, model.AddressRegister, "db", offset, 2, model.TypeUint16)
		p.Address.DB = db
		return p
	}
	dev := model.Device{ID: "d", ChannelID: "c", Enabled: true, Points: []model.Point{
		mk("a", 1, 0), mk("b", 1, 2), mk("c", 2, 0), mk("d", 2, 2),
	}}
	spans, st := Build(dev, Options{})
	if st.Points != 4 {
		t.Fatalf("points = %d", st.Points)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2 (one per DB): %+v", len(spans), spans)
	}
	seen := map[int]bool{}
	for _, s := range spans {
		db := s.Points[0].Address.DB
		if seen[db] {
			t.Fatalf("db %d appears in more than one span", db)
		}
		seen[db] = true
		for _, p := range s.Points {
			if p.Address.DB != db {
				t.Fatalf("span mixes db %d and %d", db, p.Address.DB)
			}
		}
	}
}

// 按数据标识寻址的协议（如 645）里，area 就是 DI，offset 恒为 0。规划层
// 不需要知道这件事，但必须保证「不同 DI 不会被合并进同一个 span」——否则
// 驱动会收到一个看起来连续、实际毫无意义的地址区间。
//
// 这里的判据是 area 不同就分组；顺带固定住「同一个 DI 的两个点位落在同一
// span」这个行为（驱动会逐点读，span 只是一个分组容器）。
func TestBuildSeparatesByDataIdentifier(t *testing.T) {
	mk := func(id, di string, length int) model.Point {
		return pt(id, model.AddressRegister, di, 0, length, model.TypeFloat64)
	}
	dev := model.Device{ID: "d", ChannelID: "c", Enabled: true, Points: []model.Point{
		mk("volt", "02010100", 0),
		mk("curr", "02020100", 0),
		mk("energy", "00000000", 0),
		mk("power-raw", "02030000", 0),
		mk("power-scaled", "02030000", 0),
	}}
	spans, st := Build(dev, Options{})
	if st.Points != 5 {
		t.Fatalf("points = %d", st.Points)
	}
	if len(spans) != 4 {
		t.Fatalf("spans = %d, want 4 (one per DI): %+v", len(spans), spans)
	}
	byDI := map[string]int{}
	for _, s := range spans {
		di := s.Points[0].Address.Area
		byDI[di] += len(s.Points)
		for _, p := range s.Points {
			if p.Address.Area != di {
				t.Fatalf("span %s mixes DI %s and %s", s.ID, di, p.Address.Area)
			}
			if p.Address.Offset != 0 {
				t.Fatalf("DI point %s has offset %d", p.ID, p.Address.Offset)
			}
		}
	}
	if byDI["02030000"] != 2 {
		t.Fatalf("same-DI points = %d, want 2", byDI["02030000"])
	}
}
