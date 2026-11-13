package sl651

import (
	"errors"
	"testing"
)

// DRP / DRZ / ZT 是 SL 651 里最容易被解错的三类要素：
//
//	DRP（F4H）12 字节 = 12 段 × 1 字节 5min 雨量，单位 0.1mm，HEX
//	DRZ（F5H..FCH）24 字节 = 12 段 × 2 字节 5min 相对水位，单位 0.01m，HEX
//	ZT（45H）4 字节 HEX 状态字，无数值含义（Scale=0）
//
// 规约只写了「每组雨量占 1 字节 HEX，分辨率为 0.1mm」，没有另给组数字段；
// 段数与段宽只能从引导符的固定宽度与要素类型推出来。把 DRP 当一个 12 字节
// HEX 整数解，得到的是 0x1932FF… 这样的溢出值——不报错、值域看着也像大数，
// 是最危险的一类错误。这里用 Slot* 把这件事锁死。
func TestDRPSlots(t *testing.T) {
	// 第 0 段 0x19=25 → 2.5mm；第 1 段 0x32=50 → 5.0mm；第 2 段起缺测。
	data := make([]byte, 12)
	data[0], data[1] = 0x19, 0x32
	for i := 2; i < 12; i++ {
		data[i] = 0xFF
	}
	f, err := NewFieldRaw(GDRP, data)
	if err != nil {
		t.Fatalf("NewFieldRaw: %v", err)
	}

	if f.Count() != 1 {
		t.Fatalf("DRP 在报文里是 1 个数据，Count = %d，want 1（段数见 SlotCount）", f.Count())
	}
	if got := f.SlotCount(0); got != 12 {
		t.Fatalf("SlotCount = %d，want 12", got)
	}
	if got := f.Slots(0); len(got) != 12 {
		t.Fatalf("len(Slots) = %d，want 12", len(got))
	}
	for i := 0; i < 12; i++ {
		if len(f.SlotBytes(0, i)) != 1 {
			t.Fatalf("SlotBytes(%d) 长度 = %d，want 1", i, len(f.SlotBytes(0, i)))
		}
	}

	// 数值换算：与 Field.Number 对单段的处理必须一致（HEX × LSB）。
	if v, err := f.SlotNumber(0, 0); err != nil || v != 2.5 {
		t.Fatalf("SlotNumber(0) = %v, %v，want 2.5", v, err)
	}
	if v, err := f.SlotNumber(0, 1); err != nil || v != 5 {
		t.Fatalf("SlotNumber(1) = %v, %v，want 5", v, err)
	}
	if v := f.SlotHex(0, 0); v != 0x19 {
		t.Fatalf("SlotHex(0) = %#x，want 0x19", v)
	}

	// 缺测：整段全 F。
	for i := 2; i < 12; i++ {
		if !f.SlotNoData(0, i) {
			t.Fatalf("第 %d 段应识别为缺测: % x", i, f.SlotBytes(0, i))
		}
		if _, err := f.SlotNumber(0, i); !errors.Is(err, ErrNoData) {
			t.Fatalf("SlotNumber(%d) = %v，want ErrNoData", i, err)
		}
	}
	if f.SlotNoData(0, 0) {
		t.Fatal("第 0 段有数据，不应判为缺测")
	}
}

// DRZ 是 2 字节/段：0x01F4 = 500cm = 5.00m。按 BCD 解会得到 194，
// 「看着完全合理」，因此这里同时锁 HEX 与段宽。
func TestDRZSlots(t *testing.T) {
	data := make([]byte, 24)
	for i := 0; i < 12; i++ {
		data[i*2], data[i*2+1] = 0x01, 0xF4 // 500 → 5.00m
	}
	f, err := NewFieldRaw(GDRZ1, data)
	if err != nil {
		t.Fatalf("NewFieldRaw: %v", err)
	}
	if got := f.SlotCount(0); got != 12 {
		t.Fatalf("SlotCount = %d，want 12", got)
	}
	if v, err := f.SlotNumber(0, 0); err != nil || v != 5 {
		t.Fatalf("SlotNumber(0) = %v, %v，want 5", v, err)
	}
	for i := 0; i < 12; i++ {
		if len(f.SlotBytes(0, i)) != 2 {
			t.Fatalf("SlotBytes(%d) 长度 = %d，want 2", i, len(f.SlotBytes(0, i)))
		}
		if v, _ := f.SlotNumber(0, i); v != 5 {
			t.Fatalf("SlotNumber(%d) = %v，want 5", i, v)
		}
	}
}

// 非批量要素：Slot* 一律退化为「整块数据」，段号只能是 0。
func TestSlotNonBatch(t *testing.T) {
	z, err := NewField(GZ, 127) // 0.127 m，4 字节 BCD
	if err != nil {
		t.Fatalf("NewField: %v", err)
	}
	if got := z.SlotCount(0); got != 1 {
		t.Fatalf("非批量要素 SlotCount = %d，want 1", got)
	}
	if z.Slots(0) != nil {
		t.Fatalf("非批量要素 Slots = %v，want nil", z.Slots(0))
	}
	if len(z.SlotBytes(0, 0)) != 4 {
		t.Fatalf("SlotBytes(0,0) 长度 = %d，want 4", len(z.SlotBytes(0, 0)))
	}
	if v, err := z.SlotNumber(0, 0); err != nil || v != 0.127 {
		t.Fatalf("SlotNumber = %v, %v，want 0.127", v, err)
	}
	if v, _ := z.SlotNumber(0, 1); v != 0 {
		t.Fatal("越界段号应返回 nil / 0 而不 panic")
	}
}

// ZT 状态字：4 字节 HEX 但 Scale=0，SlotNumber 必须显式拒绝而不是给一个
// 看似合理的 0 值。
func TestSlotStatusWord(t *testing.T) {
	f, err := NewFieldRaw(GZT, []byte{0x00, 0x00, 0x00, 0x01})
	if err != nil {
		t.Fatalf("NewFieldRaw: %v", err)
	}
	if got := f.SlotCount(0); got != 1 {
		t.Fatalf("ZT SlotCount = %d，want 1", got)
	}
	if _, err := f.SlotNumber(0, 0); !errors.Is(err, ErrBadElement) {
		t.Fatalf("ZT SlotNumber = %v，want ErrBadElement（无数值含义）", err)
	}
	if v := f.SlotHex(0, 0); v != 1 {
		t.Fatalf("ZT SlotHex = %#x，want 0x1", v)
	}
}

// SlotNoData 的越界行为：宁可判为缺测，也不要 panic。上层据此降级质量，
// 比崩溃或返回垃圾值都好。
//
// 顺带把「整数据缺测」与「单段缺测」分开锁：前者是 Values[0] 全 F（例如
// 整组取排水口水位都没测），后者只影响一段（例如 DRP 的前 2 段有值、
// 后 10 段没测）。两者都会走 NoData / SlotNoData，不能混。
func TestSlotNoDataBounds(t *testing.T) {
	f, err := NewField(GZ, 127) // 0.127 m，4 字节 BCD
	if err != nil {
		t.Fatalf("NewField: %v", err)
	}
	if !f.SlotNoData(9, 0) || !f.SlotNoData(0, 3) || !f.SlotNoData(-1, 0) {
		t.Fatal("越界应判为缺测")
	}
	if f.SlotNoData(0, 0) {
		t.Fatal("有数据不应判为缺测")
	}
	if f.NoData(0) {
		t.Fatal("有数据不应判为缺测（数据级）")
	}

	// 数据级缺测：规约的缺测填充是「与数据位数一样的 F」，因此 N(7,3) 水位
	// 缺测填 8 个 F，而不是 NewField(9999) 造出来的 0xFF0F0F…。
	all, err := NewFieldRaw(GZ1, []byte{0xFF, 0xFF, 0xFF, 0xFF})
	if err != nil {
		t.Fatalf("NewFieldRaw(GZ1): %v", err)
	}
	if !all.NoData(0) || !all.SlotNoData(0, 0) {
		t.Fatal("整组缺测应同时被 NoData 与 SlotNoData 识别")
	}
}

// SplitBytes 与 Slots 的差别：SplitBytes 靠调用方给宽度（适合均匀时段报
// 这类「时段数与段宽由报文长度推出」的场景），Slots 的段宽是规约事实。
func TestSplitBytesWidths(t *testing.T) {
	buf := []byte{0x01, 0xF4, 0x03, 0xE8}
	parts := SplitBytes(buf, 2)
	if len(parts) != 2 || parts[0][0] != 0x01 || parts[1][1] != 0xE8 {
		t.Fatalf("SplitBytes = % x，want [01F4 03E8]", parts)
	}
	if SplitBytes(buf, 3) != nil {
		t.Fatal("宽度不整除应返回 nil")
	}
	if SplitBytes(buf, 0) != nil {
		t.Fatal("宽度为 0 应返回 nil")
	}
}
