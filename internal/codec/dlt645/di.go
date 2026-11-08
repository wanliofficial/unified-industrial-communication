package dlt645

import (
	"fmt"
	"strconv"
	"strings"
)

// DI 是 4 字节数据标识。字段命名与标准的 DI3..DI0 一一对应，避免用下标
// 时搞反——线上是 DI0 先传，而人在手册上看到的顺序是 DI3 DI2 DI1 DI0。
type DI struct {
	DI3 byte // 类别：00 电能量 / 01 最大需量 / 02 变量 / 03 事件 / 04 参变量
	DI2 byte
	DI1 byte
	DI0 byte
}

// Bytes 返回**传输顺序**的 4 字节（DI0 在前）。
func (d DI) Bytes() [4]byte { return [4]byte{d.DI0, d.DI1, d.DI2, d.DI3} }

// String 返回手册书写顺序的 8 位十六进制（DI3 在前）。
func (d DI) String() string { return fmt.Sprintf("%02X%02X%02X%02X", d.DI3, d.DI2, d.DI1, d.DI0) }

// DIFromBytes 从**传输顺序**的 4 字节（DI0 在前）还原数据标识。
//
// 应答帧里 DI 会被原样回显，但同样按 DI0 先传；用「按手册顺序解析」的
// 直觉去读这 4 个字节会得到一个完全不同的 DI（例如把 02010100 读成
// 00010102），而这种错位往往仍然命中表里的另一项，于是值看起来「有，
// 但不对」——最难查的一类故障。
func DIFromBytes(b []byte) (DI, error) {
	if len(b) < 4 {
		return DI{}, fmt.Errorf("dlt645: 数据标识需要 4 字节，实际 %d", len(b))
	}
	return DI{DI3: b[3], DI2: b[2], DI1: b[1], DI0: b[0]}, nil
}

// Block 表示这是一个数据块标识（任一字节为 FF）。
func (d DI) Block() bool {
	return d.DI3 == 0xFF || d.DI2 == 0xFF || d.DI1 == 0xFF || d.DI0 == 0xFF
}

// ParseDI 解析 8 位十六进制数据标识（DI3DI2DI1DI0 顺序）。
func ParseDI(s string) (DI, error) {
	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F') {
			return DI{}, fmt.Errorf("dlt645: 数据标识 %q 含非十六进制字符 %q", s, r)
		}
	}
	if len(s) != 8 {
		return DI{}, fmt.Errorf("dlt645: 数据标识 %q 必须是 8 位十六进制（DI3DI2DI1DI0）", s)
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return DI{}, fmt.Errorf("dlt645: 数据标识 %q 非法: %w", s, err)
	}
	return DI{DI3: byte(n >> 24), DI2: byte(n >> 16), DI1: byte(n >> 8), DI0: byte(n)}, nil
}

// ItemKind 决定解码方式。
type ItemKind int

const (
	// KindNumber 是压缩 BCD 数值，按 Decimals 换算，可能带符号位。
	KindNumber ItemKind = iota
	// KindTime 是 6 字节时间（ss mm hh DD MM YY）。
	KindTime
	// KindDate 是 4 字节日期时间（MMDDhhmm）。
	KindDate
	// KindAddr 是 6 字节通信地址（BCD，低字节在前）。
	KindAddr
	// KindDemand 是「最大需量 + 发生时间」8 字节复合项。
	KindDemand
	// KindBits 是位域状态字，按无符号整数解出。
	KindBits
	// KindBCD 是宽度由调用方给出、无小数位的 BCD 数（如密码/特征字）。
	KindBCD
)

// Item 是一个数据项的定义。
//
// Width 是标准给出的字节数。对数据块（DI 含 FF）它是被请求块的整体宽度，
// 因此驱动在读回长度与 Width 不符时按**实际长度**解码，而不是按 Width 截断：
// 现场有表把「电压数据块」按 3 相 ×3 项给 18 字节，也有表只给 6 字节。
type Item struct {
	DI       DI
	Name     string
	Width    int
	Decimals int
	Signed   bool
	Kind     ItemKind
	// Writable 标记标准里该数据项允许写。只用于文档/配置提示，不参与
	// 校验：真正的权限由电表的密码等级决定。
	Writable bool
}

// table 是常用数据项的静态表。
//
// 只覆盖「电表点表里 90% 会出现」的项。没有列出的 DI 一律报错而不是猜：
// 猜宽度会让读回来的值错位，而错位的电能读数会被当成真实电量入账。
var table = map[DI]Item{
	// 电能量：4 字节，两位小数，最高位是符号位（0 正 1 负）。
	{DI3: 0x00, DI2: 0x00, DI1: 0x00, DI0: 0x00}: {Name: "组合有功总电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x01, DI1: 0x00, DI0: 0x00}: {Name: "正向有功总电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x02, DI1: 0x00, DI0: 0x00}: {Name: "反向有功总电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x03, DI1: 0x00, DI0: 0x00}: {Name: "组合无功 1 总电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x04, DI1: 0x00, DI0: 0x00}: {Name: "组合无功 2 总电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x15, DI1: 0x00, DI0: 0x00}: {Name: "A 相正向有功电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x29, DI1: 0x00, DI0: 0x00}: {Name: "A 相反向有功电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x35, DI1: 0x00, DI0: 0x00}: {Name: "A 相正向无功电能", Width: 4, Decimals: 2, Signed: true},
	{DI3: 0x00, DI2: 0x49, DI1: 0x00, DI0: 0x00}: {Name: "A 相反向无功电能", Width: 4, Decimals: 2, Signed: true},

	// 最大需量及发生时间：4 字节需量（XX.XXXX）+ 4 字节时间（MMDDhhmm）。
	{DI3: 0x01, DI2: 0x01, DI1: 0x00, DI0: 0x00}: {Name: "正向有功总最大需量及发生时间", Width: 8, Decimals: 4, Signed: true, Kind: KindDemand},
	{DI3: 0x01, DI2: 0x02, DI1: 0x00, DI0: 0x00}: {Name: "反向有功总最大需量及发生时间", Width: 8, Decimals: 4, Signed: true, Kind: KindDemand},

	// 变量：电压 2B/V×0.1，电流 3B/A×0.001，功率 3B/kW×0.0001，功率因数 X.XXX。
	{DI3: 0x02, DI2: 0x01, DI1: 0x01, DI0: 0x00}: {Name: "A 相电压", Width: 2, Decimals: 1},
	{DI3: 0x02, DI2: 0x01, DI1: 0x02, DI0: 0x00}: {Name: "B 相电压", Width: 2, Decimals: 1},
	{DI3: 0x02, DI2: 0x01, DI1: 0x03, DI0: 0x00}: {Name: "C 相电压", Width: 2, Decimals: 1},
	{DI3: 0x02, DI2: 0x02, DI1: 0x01, DI0: 0x00}: {Name: "A 相电流", Width: 3, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x02, DI1: 0x02, DI0: 0x00}: {Name: "B 相电流", Width: 3, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x02, DI1: 0x03, DI0: 0x00}: {Name: "C 相电流", Width: 3, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x03, DI1: 0x00, DI0: 0x00}: {Name: "瞬时总有功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x03, DI1: 0x01, DI0: 0x00}: {Name: "瞬时 A 相有功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x03, DI1: 0x02, DI0: 0x00}: {Name: "瞬时 B 相有功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x03, DI1: 0x03, DI0: 0x00}: {Name: "瞬时 C 相有功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x04, DI1: 0x00, DI0: 0x00}: {Name: "瞬时总无功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x04, DI1: 0x01, DI0: 0x00}: {Name: "瞬时 A 相无功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x04, DI1: 0x02, DI0: 0x00}: {Name: "瞬时 B 相无功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x04, DI1: 0x03, DI0: 0x00}: {Name: "瞬时 C 相无功功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x05, DI1: 0x00, DI0: 0x00}: {Name: "瞬时总视在功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x05, DI1: 0x01, DI0: 0x00}: {Name: "瞬时 A 相视在功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x05, DI1: 0x02, DI0: 0x00}: {Name: "瞬时 B 相视在功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x05, DI1: 0x03, DI0: 0x00}: {Name: "瞬时 C 相视在功率", Width: 3, Decimals: 4, Signed: true},
	{DI3: 0x02, DI2: 0x06, DI1: 0x00, DI0: 0x00}: {Name: "瞬时总功率因数", Width: 2, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x06, DI1: 0x01, DI0: 0x00}: {Name: "瞬时 A 相功率因数", Width: 2, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x06, DI1: 0x02, DI0: 0x00}: {Name: "瞬时 B 相功率因数", Width: 2, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x06, DI1: 0x03, DI0: 0x00}: {Name: "瞬时 C 相功率因数", Width: 2, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x80, DI1: 0x00, DI0: 0x02}: {Name: "电网频率", Width: 2, Decimals: 2},

	// 变量：电表温度/内部时钟等常用项之外的「表号/时间/状态」。
	{DI3: 0x04, DI2: 0x00, DI1: 0x01, DI0: 0x01}: {Name: "日期及时间", Width: 6, Kind: KindTime, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x01, DI0: 0x02}: {Name: "通信地址", Width: 6, Kind: KindAddr, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x01, DI0: 0x03}: {Name: "表号", Width: 6, Kind: KindAddr, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x01}: {Name: "通信地址", Width: 6, Kind: KindAddr, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x02}: {Name: "电表运行状态字 1", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x03}: {Name: "电表运行状态字 2", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x04}: {Name: "电表运行状态字 3", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x05}: {Name: "电表运行状态字 4", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x06}: {Name: "电表运行状态字 5", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x07}: {Name: "电表运行状态字 6", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x08}: {Name: "电表运行状态字 7", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x09}: {Name: "电表运行状态字 8", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x04, DI0: 0x0A}: {Name: "电表运行状态字 9", Width: 2, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x05, DI0: 0x01}: {Name: "A 相电压状态字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x05, DI0: 0x02}: {Name: "B 相电压状态字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x05, DI0: 0x03}: {Name: "C 相电压状态字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x06, DI0: 0x01}: {Name: "A 相电流状态字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x06, DI0: 0x02}: {Name: "B 相电流状态字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x06, DI0: 0x03}: {Name: "C 相电流状态字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x07, DI0: 0x01}: {Name: "周休日特征字", Width: 1, Kind: KindBits, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x08, DI0: 0x01}: {Name: "通信速率特征字", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x09, DI0: 0x01}: {Name: "当前费率", Width: 1, Kind: KindBits},
	{DI3: 0x04, DI2: 0x00, DI1: 0x0B, DI0: 0x01}: {Name: "第 1 结算日", Width: 4, Kind: KindDate, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x0B, DI0: 0x02}: {Name: "第 2 结算日", Width: 4, Kind: KindDate, Writable: true},
	{DI3: 0x04, DI2: 0x00, DI1: 0x0B, DI0: 0x03}: {Name: "第 3 结算日", Width: 4, Kind: KindDate, Writable: true},

	// 数据块（任一 DI 字节为 FF）：宽度不固定，读回多少解多少。
	{DI3: 0x00, DI2: 0x00, DI1: 0xFF, DI0: 0x00}: {Name: "电能数据块", Width: 0, Decimals: 2, Signed: true},
	{DI3: 0x02, DI2: 0x01, DI1: 0xFF, DI0: 0x00}: {Name: "电压数据块", Width: 0, Decimals: 1},
	{DI3: 0x02, DI2: 0x02, DI1: 0xFF, DI0: 0x00}: {Name: "电流数据块", Width: 0, Decimals: 3, Signed: true},
	{DI3: 0x02, DI2: 0x03, DI1: 0xFF, DI0: 0x00}: {Name: "有功功率数据块", Width: 0, Decimals: 4, Signed: true},
}

// Known 返回该 DI 是否在静态表里（或属于可推导的能量家族）。
func Known(di DI) bool {
	_, ok := Lookup(di)
	return ok
}

// Lookup 查表；未收录时按「能量家族」推导。
//
// 能量家族的规律是稳定的：DI3=0x00，DI0 是结算日（0x00 当前、其余为上 n
// 结算日）或费率，DI1 是费率号，DI2 是电能种类。它们**全部**是 4 字节、
// 两位小数、带符号位的 BCD。把这条规律写进代码而不是逼运维为每个费率各
// 建一行表，是为了让「读全部费率」这种点表不必手抄 40 行。
//
// 其余未收录的 DI 一律返回 false——宁可报「不认识这个数据标识」，也不要
// 用一个猜出来的宽度去解析真实电量。
func Lookup(di DI) (Item, bool) {
	if it, ok := table[di]; ok {
		it.DI = di
		return it, true
	}
	if di.Block() {
		// 未知数据块：宽度由响应决定，小数位无法推导。
		return Item{DI: di, Name: "未知数据块", Width: 0}, true
	}
	if di.DI3 == 0x00 {
		return Item{DI: di, Name: "电能数据项", Width: 4, Decimals: 2, Signed: true}, true
	}
	return Item{}, false
}

// Width 是驱动/点表用来推导 length 的入口。
//
// 返回 0 表示「宽度由响应决定」（数据块），点表应当显式给出长度或按 0 处理。
func Width(di DI) (int, error) {
	it, ok := Lookup(di)
	if !ok {
		return 0, fmt.Errorf("dlt645: 未收录的数据标识 %s，请在点表里显式给出长度", di)
	}
	return it.Width, nil
}
