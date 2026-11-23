package iec60870

import (
	"testing"
)

// 本文件钉死 M_ST_NA_1（步位置）与 M_BO_NA_1（32 位位串）的字节布局，
// 以及 M_IT_TB_1 的类型号——后者上一轮曾把类型号误标成 32（32 实为
// M_ST_TB_1），会让真实设备发来的类型 37 被拒绝、而类型 32 被当成
// 「带时标累计量」整帧错位解析。这条回归测试就是防那个错误再回来。

// TestM_IT_TB_1TypeID 钉死带时标累计量的类型号 = 37（不是 32）。
func TestM_IT_TB_1TypeID(t *testing.T) {
	if TypeIntegratedTotalsTime != 37 {
		t.Fatalf("M_IT_TB_1 类型号 = %d，标准规定 37（32 是 M_ST_TB_1）", TypeIntegratedTotalsTime)
	}
	// 类型 32 必须是一个「未实现」的类型，否则我们会重蹈把步位置带时标
	// 当累计量解析的覆辙。ElementSize 对未实现类型返回 ok=false。
	if _, ok := ElementSize(32); ok {
		t.Fatalf("类型 32（M_ST_TB_1）未实现，ElementSize 应返回 ok=false")
	}
}

// TestStepPositionRoundTrip 步位置：值 + QDS 品质，2 字节。
func TestStepPositionRoundTrip(t *testing.T) {
	in := ASDU{TypeID: TypeStepPosition, Cause: CauseSpontaneous, CommonAddr: 1,
		Objects: []Object{{IOA: 1101, Uint: 5, Quality: QSubstituted | QInvalid}}}
	raw, err := BuildASDU(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	o := out.Objects[0]
	if o.Uint != 5 {
		t.Fatalf("step value = %d，want 5", o.Uint)
	}
	if !o.Quality.Has(QSubstituted) || !o.Quality.Has(QInvalid) {
		t.Fatalf("quality = %v，want substituted|invalid", o.Quality)
	}
	// VTI 低 7 位是值，bit7 是暂态位；这里 Raw 应恰好等于值（无暂态）。
	if o.Raw != 5 {
		t.Fatalf("raw VTI = 0x%02X，want 0x05", o.Raw)
	}
}

// TestStepPositionTransientBit 暂态位 T（bit7）必须无损保留且不影响取值。
func TestStepPositionTransientBit(t *testing.T) {
	// 构造一个「值=3、暂态位置位」的对象：直接走编解码，确认 Raw 的 bit7 被保住。
	in := ASDU{TypeID: TypeStepPosition, Cause: CauseSpontaneous, CommonAddr: 1,
		Objects: []Object{{IOA: 1102, Uint: 3, Raw: 0x80 | 3}}}
	raw, err := BuildASDU(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	o := out.Objects[0]
	if o.Uint != 3 {
		t.Fatalf("step value = %d，want 3（暂态位不得污染取值）", o.Uint)
	}
	if o.Raw&0x80 == 0 {
		t.Fatalf("暂态位 T 丢失：raw VTI = 0x%02X", o.Raw)
	}
}

// TestStepPositionRejectsOversize 步位置值只有 7 位，超过 127 必须拒绝。
func TestStepPositionRejectsOversize(t *testing.T) {
	a := ASDU{TypeID: TypeStepPosition, Cause: CauseSpontaneous,
		Objects: []Object{{Uint: 200}}}
	if _, err := BuildASDU(a); err == nil {
		t.Fatal("step position 200 必须被拒绝（超过 7 位）")
	}
}

// TestBitstringRoundTrip 32 位位串：bit0 = data[0] 的 LSB，4 字节无品质。
func TestBitstringRoundTrip(t *testing.T) {
	// 0x80000001 的位：bit0 与 bit31 置位。小端布局下 data[0] 含 bit0，
	// data[3] 含 bit31。
	in := uint32(0x80000001)
	in_ASDU := ASDU{TypeID: TypeBitstring, Cause: CauseSpontaneous, CommonAddr: 1,
		Objects: []Object{{IOA: 1201, Uint: in}}}
	raw, err := BuildASDU(in_ASDU)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Objects[0].Uint != in {
		t.Fatalf("bitstring = 0x%08X，want 0x%08X", out.Objects[0].Uint, in)
	}
	if out.Objects[0].Quality != 0 {
		t.Fatalf("bitstring 无品质描述符，quality = %v，want 0", out.Objects[0].Quality)
	}
}

// TestBitstringLittleEndian 逐字节钉死位序：bit0 落在最低字节的最低位。
func TestBitstringLittleEndian(t *testing.T) {
	raw := BuildFrame(TypeBitstring, 1, Object{Uint: 0x00000001})
	out, err := ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Objects[0].Uint != 1 {
		t.Fatalf("bitstring = 0x%08X，want 0x00000001（bit0 必须在最低字节）", out.Objects[0].Uint)
	}
	// 反向：bit31 置位应落在第 4 字节（data[3]）的最高位。
	raw = BuildFrame(TypeBitstring, 1, Object{Uint: 0x80000000})
	out, err = ParseASDU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Objects[0].Uint != 0x80000000 {
		t.Fatalf("bitstring = 0x%08X，want 0x80000000", out.Objects[0].Uint)
	}
}

// BuildFrame 是构造单对象 ASDU 并附上类型头的便捷封装，专供位级测试用。
// 它直接走 BuildASDU，只是把参数摆得更短。
func BuildFrame(typeID byte, ca uint16, o Object) []byte {
	raw, err := BuildASDU(ASDU{TypeID: typeID, Cause: CauseSpontaneous, CommonAddr: ca,
		Objects: []Object{o}})
	if err != nil {
		panic(err)
	}
	return raw
}

// TestStepPositionBitLayout 逐字节钉死 VTI/QDS 布局，防止有人改坏偏移。
func TestStepPositionBitLayout(t *testing.T) {
	// 值=6（0b0000110），QDS 的 IV 位（bit7）置位。
	raw := BuildFrame(TypeStepPosition, 1, Object{Uint: 6, Quality: QInvalid})
	// ASDU 头之后第一个信息元素：data[0]=VTI，data[1]=QDS。
	elem := raw[len(raw)-2:]
	if elem[0] != 6 {
		t.Fatalf("VTI = 0x%02X，want 0x06（值 6，无暂态位）", elem[0])
	}
	if elem[1]&0x80 == 0 {
		t.Fatalf("QDS 的 IV 位未置位：0x%02X", elem[1])
	}
}

// TestStepCommandTypeID 钉死调节步命令 C_RC_NA_1 的类型号 = 47（0x2F）。
//
// 47 是 IEC 60870-5-101/-104 一致规定的：S/E+QU+RCO 的 1 字节调节步命令。
// 这个号曾与 45/46/48 的命令族相邻，容易在补类型时被记错，所以和步位置的
// 类型号一样钉死。
func TestStepCommandTypeID(t *testing.T) {
	if TypeStepCommand != 47 {
		t.Fatalf("C_RC_NA_1 类型号 = %d，标准规定 47", TypeStepCommand)
	}
	if !IsCommandType(TypeStepCommand) {
		t.Fatal("调节步命令必须是命令类型（IsCommandType）")
	}
	if IsData(TypeStepCommand) {
		t.Fatal("调节步命令不能同时是数据类型（IsData）")
	}
	if size, ok := ElementSize(TypeStepCommand); !ok || size != 1 {
		t.Fatalf("调节步命令信息对象宽度 = %d/%v，want 1/true", size, ok)
	}
}

// TestStepCommandBitLayout 逐字节钉死 RCO 布局：bit7=S/E、bit2..6=QU、bit0..1=RCS。
func TestStepCommandBitLayout(t *testing.T) {
	// 升档（RCS=2）、选择位（S/E=1）、QU=0x05。
	raw := BuildFrame(TypeStepCommand, 1, Object{Uint: 2, Select: true, Qualifier: 0x05})
	// 信息对象只有 1 字节（RCO），没有 IOA 之外的元素；取 ASDU 最后一字节。
	rco := raw[len(raw)-1]
	if rco&rcsMask != rcsHigher {
		t.Fatalf("RCS = %d，want 2（升档）", rco&rcsMask)
	}
	if rco&0x80 == 0 {
		t.Fatal("S/E（选择位 bit7）未置位")
	}
	if (rco>>2)&0x1F != 0x05 {
		t.Fatalf("QU = %#x，want 0x05", (rco>>2)&0x1F)
	}
}

// TestStepCommandDirectionRoundTrip 升/降两档方向必须无损往返，且 0/3 被拒。
func TestStepCommandDirectionRoundTrip(t *testing.T) {
	for _, dir := range []byte{rcsLower, rcsHigher} {
		raw, err := BuildASDU(ASDU{TypeID: TypeStepCommand, Cause: CauseActivation, CommonAddr: 1,
			Objects: []Object{{IOA: 505, Uint: uint32(dir)}}})
		if err != nil {
			t.Fatalf("direction %d: build: %v", dir, err)
		}
		out, err := ParseASDU(raw)
		if err != nil {
			t.Fatalf("direction %d: parse: %v", dir, err)
		}
		if out.Objects[0].Uint != uint32(dir) {
			t.Fatalf("direction %d round-trip = %d", dir, out.Objects[0].Uint)
		}
	}
	// 0 与 3 是「不允许」：编码必须拒绝，不能把设备明说「不接受」的方向当
	// 合法命令发下去（双点命令把 0/3 当中态、设定值的越界值拒绝同理）。
	for _, bad := range []uint32{0, 3, 4, 99} {
		if _, err := BuildASDU(ASDU{TypeID: TypeStepCommand, Cause: CauseActivation, CommonAddr: 1,
			Objects: []Object{{IOA: 505, Uint: bad}}}); err == nil {
			t.Fatalf("direction %d 必须被拒绝（not permitted）", bad)
		}
	}
}
