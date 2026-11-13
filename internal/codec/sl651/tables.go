package sl651

// 本文件是附录 C（表 C.1）的完整落地：131 项「引导符 → 中文名 / ASCII 简称 /
// 单位 / 数据位数 / 小数位数」。
//
// 表 C.1 一共 133 行，其中 132 行（76H~EFH）是保留区、133 行（FFXXH）是用户
// 自定义扩展，两者都不是可解释的数值要素，因此这里只落 131 项：
// F0H~FDH 的 14 个引导符类 + 01H~75H 的 117 个常规要素。
//
// 为什么把这张表写死在代码里而不是做成配置文件：它是**规约正文**，不是站点
// 配置。同一台终端送来的 39H 在任何工程里都是「瞬时河道水位、潮位」，把
// 它变成可配置项只会让同一份报文在不同站点解出不同含义。
//
// 数据位数 D 与小数位数 d 来自规约的 N(D,d) 定义（注 2：D 是含小数点以外
// 的数据位数，d 是小数点后的位数）。它们只用于两点：
//   - 生成报文的默认数据定义字节（字节数 = ceil(D/2)）；
//   - 校验对端送来的数据定义字是否与规约一致（不一致时按线上的为准，但在
//     自检输出里标出来，因为「表厂把 N(5,1) 写成 2 字节」是现场常见方言）。
type Guide struct {
	// Alias 是附录 C 的 ASCII 简称（如 "Z"、"PT"、"VT"）。
	Alias string
	// Name 是中文名。
	Name string
	// Unit 是量纲单位，空表示无量纲。
	Unit string
	// Digits 是 N(D,d) 的 D；固定长度引导符类为 0。
	Digits int
	// Decimals 是 N(D,d) 的 d。
	Decimals int
	// FixedWidth 非 0 表示「引导符类」的固定字节数（如观测时间 5 字节、
	// 批量雨量 12 字节），此时宽度不可从数据定义字节的高 5 位推导。
	FixedWidth int
	// FixedDef 非 0 表示数据定义字节被规约写死成这个值（附录 C 注 a~f），
	// 不能按 N(D,d) 换算。例如观测时间恒为 F0H、测站编码恒为 F1H——
	// 注意这不是「宽度 30 字节」，按高 5 位读会当场错位。
	FixedDef byte
	// Hex 为真表示数据是 HEX 整数而不是 BCD 十进制。附录 C 里 DRP、DRZ1~8
	// 与 ZT 属于这一类：例如 DRZ 的每个数据是 2 字节 HEX，单位是厘米，
	// 0x01F4 表示 500cm = 5.00m。按 BCD 解会得到 194，看似合理的错值最危险。
	Hex bool
	// Scale 是 HEX 数据的每个最小单位的量值（LSB）。DRP 为 0.1（mm），
	// DRZ 为 0.01（m）。为 0 表示该要素没有数值含义（如 ZT 状态字）。
	Scale float64
	// Slot 非 0 表示这是一个**多时段批量要素**：整组数据由若干等宽时段
	// 组成，每段 Slot 字节。DRP 是 12 组 × 1 字节（5min 雨量），DRZ 是
	// 12 组 × 2 字节（5min 相对水位）。
	//
	// 为什么必须显式写出来：附录 C 只给了「12 字节 HEX」「24 字节 HEX」，
	// 而 Field.Values 是「1 个标识符 + n 个等宽数据」。对 DRP 来说整帧只有
	// 1 个 12 字节数据（12 段挤在一起），若把它当一个 HEX 整数解，会得到
	// 0x1932FF... 这样的溢出值——不报错，但完全是垃圾。所以 Slot 的作用是
	// 告诉上层「这一段还能再切」，切不出来就必须显式拒绝而不是给个数。
	Slot int
}

// Bytes 返回该要素在报文里应占的数据字节数。
//
// N(D,d) 的 D 是十进制的数据位数，BCD 每字节两位，因此字节数 = ceil(D/2)。
// 固定长度引导符类直接给出字节数。
func (g Guide) Bytes() int {
	if g.FixedWidth > 0 {
		return g.FixedWidth
	}
	if g.Digits == 0 {
		return 0
	}
	return (g.Digits + 1) / 2
}

// DataDef 返回该要素的默认数据定义字节：(字节数 << 3) | 小数位数。
//
// 注 a~f 规定的固定定义字节优先：观测时间必须是 F0H、测站编码必须是 F1H、
// 图片必须是 F3H、批量流速必须是 F6H，按宽度换算出来的数值对端一律不认。
func (g Guide) DataDef() byte {
	if g.FixedDef != 0 {
		return g.FixedDef
	}
	return byte(g.Bytes()<<3) | byte(g.Decimals&0x07)
}

// Variable 判断该要素的数据长度是否无法从标识符推出。
//
// 人工置数（F2H）、图片（F3H）、批量流速（FDH）三项在规约里都只给了「数据
// 定义字节固定值」，实际数据长度由报文总长决定（注 c、d、f）。它们不是
// 可解析的数值点位，本包一律显式拒绝，而不是按高 5 位猜一个长度出来。
func (g Guide) Variable() bool {
	return g.FixedDef != 0 && g.FixedWidth == 0
}

// 引导符取值（附录 C 表 C.1），按编码顺序排列，便于对照原文核对。
const (
	GTT   = 0xF0 // 观测时间（定义字节固定 F0H）
	GSI   = 0xF1 // 测站编码（定义字节固定 F1H）
	GRGZS = 0xF2 // 人工置数
	GPIC  = 0xF3 // 图片信息
	GDRP  = 0xF4 // 1h 内每 5min 时段雨量（12 字节 HEX）
	GDRZ1 = 0xF5 // 1h 内 5min 间隔相对水位 1（24 字节 HEX）
	GDRZ8 = 0xFC // 1h 内 5min 间隔相对水位 8
	GDATA = 0xFD // 流速批量数据传输

	GAC   = 0x01 // 断面面积
	GAI   = 0x02 // 瞬时气温
	GC    = 0x03 // 瞬时水温
	GDR   = 0x04 // 时间步长码
	GDT   = 0x05 // 时段长
	GED   = 0x06 // 日蒸发量
	GEJ   = 0x07 // 当前蒸发
	GFL   = 0x08 // 气压
	GGH   = 0x09 // 闸门开启高度
	GGN   = 0x0A // 输水设备/闸门编号
	GGS   = 0x0B // 输水设备类别
	GGT   = 0x0C // 闸门开启孔数
	GGTP  = 0x0D // 地温
	GH    = 0x0E // 地下水瞬时埋深
	GHW   = 0x0F // 波浪高度
	GM10  = 0x10 // 10cm 土壤含水量
	GM100 = 0x17 // 100cm 土壤含水量
	GMST  = 0x18 // 湿度
	GNS   = 0x19 // 开机台数
	GP1   = 0x1A // 1h 时段降水量
	GP2   = 0x1B // 2h 时段降水量
	GP3   = 0x1C // 3h 时段降水量
	GP6   = 0x1D // 6h 时段降水量
	GP12  = 0x1E // 12h 时段降水量
	GPD   = 0x1F // 日降水量
	GPJ   = 0x20 // 当前降水量
	GPN01 = 0x21 // 1min 时段降水量
	GPN05 = 0x22 // 5min 时段降水量
	GPN10 = 0x23 // 10min 时段降水量
	GPN30 = 0x24 // 30min 时段降水量
	GPR   = 0x25 // 暴雨量
	GPT   = 0x26 // 降水量累计值
	GQ    = 0x27 // 瞬时流量
	GQ1   = 0x28 // 取（排）水口流量 1
	GQ8   = 0x2F // 取（排）水口流量 8
	GQA   = 0x30 // 总出库流量
	GQZ   = 0x31 // 输水设备流量
	GSW   = 0x32 // 输沙量
	GUC   = 0x33 // 风向
	GUE   = 0x34 // 风力（级）
	GUS   = 0x35 // 风速
	GVA   = 0x36 // 断面平均流速
	GVJ   = 0x37 // 当前瞬时流速
	GVT   = 0x38 // 电源电压
	GZ    = 0x39 // 瞬时河道水位、潮位
	GZB   = 0x3A // 库（闸、站）下水位
	GZU   = 0x3B // 库（闸、站）上水位
	GZ1   = 0x3C // 取（排）水口水位 1
	GZ8   = 0x43 // 取（排）水口水位 8
	GSQ   = 0x44 // 含沙量
	GZT   = 0x45 // 遥测站状态及报警信息（4 字节 HEX）
	GPH   = 0x46 // pH 值
	GDO   = 0x47 // 溶解氧
	GCOND = 0x48 // 电导率
	GTURB = 0x49 // 浊度
	GCODM = 0x4A // 高锰酸盐指数
	GREDX = 0x4B // 氧化还原电位
	GNH4N = 0x4C // 氨氮
	GTP   = 0x4D // 总磷
	GTN   = 0x4E // 总氮
	GTOC  = 0x4F // 总有机碳
	GCU   = 0x50 // 铜
	GZN   = 0x51 // 锌
	GSE   = 0x52 // 硒
	GAS   = 0x53 // 砷
	GTHG  = 0x54 // 总汞
	GCD   = 0x55 // 镉
	GPB   = 0x56 // 铅
	GCHLA = 0x57 // 叶绿素 a
	GWP1  = 0x58 // 水压 1
	GWP8  = 0x5F // 水压 8
	GSYL1 = 0x60 // 水表 1 剩余水量
	GSYL8 = 0x67 // 水表 8 剩余水量
	GSBL1 = 0x68 // 水表 1 每小时水量
	GSBL8 = 0x6F // 水表 8 每小时水量
	GVTA  = 0x70 // 交流 A 相电压
	GVTB  = 0x71 // 交流 B 相电压
	GVTC  = 0x72 // 交流 C 相电压
	GVIA  = 0x73 // 交流 A 相电流
	GVIB  = 0x74 // 交流 B 相电流
	GVIC  = 0x75 // 交流 C 相电流
)

// guides 是表 C.1 的完整映射。
var guides = map[byte]Guide{
	// 引导符类：数据定义字节由注 a~f 写死（FixedDef），宽度由规约固定
	// （FixedWidth）或由报文总长决定（Variable）。
	GTT:   {Alias: "TT", Name: "观测时间", FixedWidth: 5, FixedDef: 0xF0},
	GSI:   {Alias: "SI", Name: "测站编码", FixedWidth: 5, FixedDef: 0xF1},
	GRGZS: {Alias: "RGZS", Name: "人工置数", FixedDef: 0xF2},
	GPIC:  {Alias: "PIC", Name: "图片信息", FixedDef: 0xF3},
	GDRP:  {Alias: "DRP", Name: "1h 内每 5min 时段雨量", Unit: "mm", FixedWidth: 12, Hex: true, Scale: 0.1, Slot: 1},
	GDRZ1: {Alias: "DRZ1", Name: "1h 内 5min 间隔相对水位 1", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	0xF6:  {Alias: "DRZ2", Name: "1h 内 5min 间隔相对水位 2", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	0xF7:  {Alias: "DRZ3", Name: "1h 内 5min 间隔相对水位 3", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	0xF8:  {Alias: "DRZ4", Name: "1h 内 5min 间隔相对水位 4", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	0xF9:  {Alias: "DRZ5", Name: "1h 内 5min 间隔相对水位 5", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	0xFA:  {Alias: "DRZ6", Name: "1h 内 5min 间隔相对水位 6", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	0xFB:  {Alias: "DRZ7", Name: "1h 内 5min 间隔相对水位 7", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	GDRZ8: {Alias: "DRZ8", Name: "1h 内 5min 间隔相对水位 8", Unit: "m", FixedWidth: 24, Hex: true, Scale: 0.01, Slot: 2},
	GDATA: {Alias: "DATA", Name: "流速批量数据", FixedDef: 0xF6},

	// 常规要素。
	GAC: {Alias: "AC", Name: "断面面积", Unit: "m²", Digits: 8, Decimals: 2},
	GAI: {Alias: "AI", Name: "瞬时气温", Unit: "℃", Digits: 3, Decimals: 1},
	GC:  {Alias: "C", Name: "瞬时水温", Unit: "℃", Digits: 3, Decimals: 1},
	// 时间步长码：表 C.3 规定 HEX/BCD 编码时数据定义字节固定为 18H（3 字节
	// BCD、无小数），三个字节分别是「单位位 + 时 + 分」的组合（如 000030 表示
	// 30min、010000 表示 1h）。全 0 是特定含义：其后必跟 DRP/DRZ 类 1h 时段
	// 组合数据（附录 C 注 g）。它不是 N(3) 的 2 字节——按 2 字节读会让整帧错位。
	GDR:   {Alias: "DRnnn", Name: "时间步长码", Digits: 6, FixedWidth: 3, FixedDef: 0x18},
	GDT:   {Alias: "DT", Name: "时段长、降水历时", Unit: "h.min", Digits: 4},
	GED:   {Alias: "ED", Name: "日蒸发量", Unit: "mm", Digits: 5, Decimals: 1},
	GEJ:   {Alias: "EJ", Name: "当前蒸发", Unit: "mm", Digits: 5, Decimals: 1},
	GFL:   {Alias: "FL", Name: "气压", Unit: "hPa", Digits: 5},
	GGH:   {Alias: "GH", Name: "闸门开启高度", Unit: "m", Digits: 5, Decimals: 2},
	GGN:   {Alias: "GN", Name: "输水设备/闸门（组）编号", Digits: 3},
	GGS:   {Alias: "GS", Name: "输水设备类别", Digits: 1},
	GGT:   {Alias: "GT", Name: "闸门开启孔数", Unit: "孔", Digits: 3},
	GGTP:  {Alias: "GTP", Name: "地温", Unit: "℃", Digits: 3, Decimals: 1},
	GH:    {Alias: "H", Name: "地下水瞬时埋深", Unit: "m", Digits: 6, Decimals: 2},
	GHW:   {Alias: "HW", Name: "波浪高度", Unit: "m", Digits: 5, Decimals: 2},
	GM10:  {Alias: "M10", Name: "10cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	0x11:  {Alias: "M20", Name: "20cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	0x12:  {Alias: "M30", Name: "30cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	0x13:  {Alias: "M40", Name: "40cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	0x14:  {Alias: "M50", Name: "50cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	0x15:  {Alias: "M60", Name: "60cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	0x16:  {Alias: "M80", Name: "80cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	GM100: {Alias: "M100", Name: "100cm 处土壤含水量", Unit: "%", Digits: 4, Decimals: 1},
	GMST:  {Alias: "MST", Name: "湿度", Unit: "%", Digits: 4, Decimals: 1},
	GNS:   {Alias: "NS", Name: "开机台数", Unit: "台", Digits: 2},
	GP1:   {Alias: "P1", Name: "1h 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GP2:   {Alias: "P2", Name: "2h 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GP3:   {Alias: "P3", Name: "3h 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GP6:   {Alias: "P6", Name: "6h 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GP12:  {Alias: "P12", Name: "12h 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPD:   {Alias: "PD", Name: "日降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPJ:   {Alias: "PJ", Name: "当前降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPN01: {Alias: "PN01", Name: "1min 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPN05: {Alias: "PN05", Name: "5min 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPN10: {Alias: "PN10", Name: "10min 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPN30: {Alias: "PN30", Name: "30min 时段降水量", Unit: "mm", Digits: 5, Decimals: 1},
	GPR:   {Alias: "PR", Name: "暴雨量", Unit: "mm", Digits: 5, Decimals: 1},
	GPT:   {Alias: "PT", Name: "降水量累计值", Unit: "mm", Digits: 6, Decimals: 1},
	GQ:    {Alias: "Q", Name: "瞬时流量、抽水流量", Unit: "m³/s", Digits: 9, Decimals: 3},
	GQ1:   {Alias: "Q1", Name: "取（排）水口流量 1", Unit: "m³/s", Digits: 9, Decimals: 3},
	0x29:  {Alias: "Q2", Name: "取（排）水口流量 2", Unit: "m³/s", Digits: 9, Decimals: 3},
	0x2A:  {Alias: "Q3", Name: "取（排）水口流量 3", Unit: "m³/s", Digits: 9, Decimals: 3},
	0x2B:  {Alias: "Q4", Name: "取（排）水口流量 4", Unit: "m³/s", Digits: 9, Decimals: 3},
	0x2C:  {Alias: "Q5", Name: "取（排）水口流量 5", Unit: "m³/s", Digits: 9, Decimals: 3},
	0x2D:  {Alias: "Q6", Name: "取（排）水口流量 6", Unit: "m³/s", Digits: 9, Decimals: 3},
	0x2E:  {Alias: "Q7", Name: "取（排）水口流量 7", Unit: "m³/s", Digits: 9, Decimals: 3},
	GQ8:   {Alias: "Q8", Name: "取（排）水口流量 8", Unit: "m³/s", Digits: 9, Decimals: 3},
	GQA:   {Alias: "QA", Name: "总出库流量、过闸总流量", Unit: "m³/s", Digits: 9, Decimals: 3},
	GQZ:   {Alias: "QZ", Name: "输水设备流量、过闸（组）流量", Unit: "m³/s", Digits: 9, Decimals: 3},
	GSW:   {Alias: "SW", Name: "输沙量", Unit: "万t", Digits: 11, Decimals: 3},
	GUC:   {Alias: "UC", Name: "风向", Digits: 2},
	GUE:   {Alias: "UE", Name: "风力（级）", Digits: 2},
	GUS:   {Alias: "US", Name: "风速", Unit: "m/s", Digits: 4, Decimals: 1},
	GVA:   {Alias: "VA", Name: "断面平均流速", Unit: "m/s", Digits: 5, Decimals: 3},
	GVJ:   {Alias: "VJ", Name: "当前瞬时流速", Unit: "m/s", Digits: 5, Decimals: 3},
	GVT:   {Alias: "VT", Name: "电源电压", Unit: "V", Digits: 4, Decimals: 2},
	GZ:    {Alias: "Z", Name: "瞬时河道水位、潮位", Unit: "m", Digits: 7, Decimals: 3},
	GZB:   {Alias: "ZB", Name: "库（闸、站）下水位", Unit: "m", Digits: 7, Decimals: 3},
	GZU:   {Alias: "ZU", Name: "库（闸、站）上水位", Unit: "m", Digits: 7, Decimals: 3},
	GZ1:   {Alias: "Z1", Name: "取（排）水口水位 1", Unit: "m", Digits: 7, Decimals: 3},
	0x3D:  {Alias: "Z2", Name: "取（排）水口水位 2", Unit: "m", Digits: 7, Decimals: 3},
	0x3E:  {Alias: "Z3", Name: "取（排）水口水位 3", Unit: "m", Digits: 7, Decimals: 3},
	0x3F:  {Alias: "Z4", Name: "取（排）水口水位 4", Unit: "m", Digits: 7, Decimals: 3},
	0x40:  {Alias: "Z5", Name: "取（排）水口水位 5", Unit: "m", Digits: 7, Decimals: 3},
	0x41:  {Alias: "Z6", Name: "取（排）水口水位 6", Unit: "m", Digits: 7, Decimals: 3},
	0x42:  {Alias: "Z7", Name: "取（排）水口水位 7", Unit: "m", Digits: 7, Decimals: 3},
	GZ8:   {Alias: "Z8", Name: "取（排）水口水位 8", Unit: "m", Digits: 7, Decimals: 3},
	GSQ:   {Alias: "SQ", Name: "含沙量", Unit: "kg/m³", Digits: 9, Decimals: 3},
	GZT:   {Alias: "ZT", Name: "遥测站状态及报警信息", FixedWidth: 4, Hex: true},
	GPH:   {Alias: "pH", Name: "pH 值", Digits: 4, Decimals: 2},
	GDO:   {Alias: "DO", Name: "溶解氧", Unit: "mg/L", Digits: 4, Decimals: 1},
	GCOND: {Alias: "COND", Name: "电导率", Unit: "μS/cm", Digits: 5},
	GTURB: {Alias: "TURB", Name: "浊度", Unit: "度", Digits: 3},
	GCODM: {Alias: "CODMN", Name: "高锰酸盐指数", Unit: "mg/L", Digits: 4, Decimals: 1},
	GREDX: {Alias: "REDOX", Name: "氧化还原电位", Unit: "mV", Digits: 5, Decimals: 1},
	GNH4N: {Alias: "NH4N", Name: "氨氮", Unit: "mg/L", Digits: 6, Decimals: 2},
	GTP:   {Alias: "TP", Name: "总磷", Unit: "mg/L", Digits: 5, Decimals: 3},
	GTN:   {Alias: "TN", Name: "总氮", Unit: "mg/L", Digits: 5, Decimals: 2},
	GTOC:  {Alias: "TOC", Name: "总有机碳", Unit: "mg/L", Digits: 4, Decimals: 2},
	GCU:   {Alias: "CU", Name: "铜", Unit: "mg/L", Digits: 7, Decimals: 4},
	GZN:   {Alias: "ZN", Name: "锌", Unit: "mg/L", Digits: 6, Decimals: 4},
	GSE:   {Alias: "SE", Name: "硒", Unit: "mg/L", Digits: 7, Decimals: 5},
	GAS:   {Alias: "AS", Name: "砷", Unit: "mg/L", Digits: 7, Decimals: 5},
	GTHG:  {Alias: "THG", Name: "总汞", Unit: "mg/L", Digits: 7, Decimals: 5},
	GCD:   {Alias: "CD", Name: "镉", Unit: "mg/L", Digits: 7, Decimals: 5},
	GPB:   {Alias: "PB", Name: "铅", Unit: "mg/L", Digits: 7, Decimals: 5},
	GCHLA: {Alias: "CHLA", Name: "叶绿素 a", Unit: "mg/L", Digits: 4, Decimals: 2},
	GWP1:  {Alias: "WP1", Name: "水压 1", Unit: "kPa", Digits: 5, Decimals: 2},
	0x59:  {Alias: "WP2", Name: "水压 2", Unit: "kPa", Digits: 5, Decimals: 2},
	0x5A:  {Alias: "WP3", Name: "水压 3", Unit: "kPa", Digits: 5, Decimals: 2},
	0x5B:  {Alias: "WP4", Name: "水压 4", Unit: "kPa", Digits: 5, Decimals: 2},
	0x5C:  {Alias: "WP5", Name: "水压 5", Unit: "kPa", Digits: 5, Decimals: 2},
	0x5D:  {Alias: "WP6", Name: "水压 6", Unit: "kPa", Digits: 5, Decimals: 2},
	0x5E:  {Alias: "WP7", Name: "水压 7", Unit: "kPa", Digits: 5, Decimals: 2},
	GWP8:  {Alias: "WP8", Name: "水压 8", Unit: "kPa", Digits: 5, Decimals: 2},
	GSYL1: {Alias: "SYL1", Name: "水表 1 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	0x61:  {Alias: "SYL2", Name: "水表 2 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	0x62:  {Alias: "SYL3", Name: "水表 3 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	0x63:  {Alias: "SYL4", Name: "水表 4 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	0x64:  {Alias: "SYL5", Name: "水表 5 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	0x65:  {Alias: "SYL6", Name: "水表 6 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	0x66:  {Alias: "SYL7", Name: "水表 7 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	GSYL8: {Alias: "SYL8", Name: "水表 8 剩余水量", Unit: "m³", Digits: 11, Decimals: 3},
	GSBL1: {Alias: "SBL1", Name: "水表 1 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	0x69:  {Alias: "SBL2", Name: "水表 2 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	0x6A:  {Alias: "SBL3", Name: "水表 3 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	0x6B:  {Alias: "SBL4", Name: "水表 4 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	0x6C:  {Alias: "SBL5", Name: "水表 5 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	0x6D:  {Alias: "SBL6", Name: "水表 6 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	0x6E:  {Alias: "SBL7", Name: "水表 7 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	GSBL8: {Alias: "SBL8", Name: "水表 8 每小时水量", Unit: "m³/h", Digits: 10, Decimals: 2},
	GVTA:  {Alias: "VTA", Name: "交流 A 相电压", Unit: "V", Digits: 4, Decimals: 1},
	GVTB:  {Alias: "VTB", Name: "交流 B 相电压", Unit: "V", Digits: 4, Decimals: 1},
	GVTC:  {Alias: "VTC", Name: "交流 C 相电压", Unit: "V", Digits: 4, Decimals: 1},
	GVIA:  {Alias: "VIA", Name: "交流 A 相电流", Unit: "A", Digits: 4, Decimals: 1},
	GVIB:  {Alias: "VIB", Name: "交流 B 相电流", Unit: "A", Digits: 4, Decimals: 1},
	GVIC:  {Alias: "VIC", Name: "交流 C 相电流", Unit: "A", Digits: 4, Decimals: 1},
}

// GuideInfo 返回引导符的规约定义。第二个返回值表示该引导符是否在表 C.1 内。
func GuideInfo(guide byte) (Guide, bool) {
	g, ok := guides[guide]
	return g, ok
}

// Guides 返回全部已定义的引导符（升序），供自检命令列出可用要素。
func Guides() []byte {
	out := make([]byte, 0, len(guides))
	for c := range guides {
		out = append(out, c)
	}
	// 简单插入排序：133 项，避免为一处排序引入 sort 依赖。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// GuideAlias 返回 ASCII 简称。
func GuideAlias(guide byte) string {
	if g, ok := guides[guide]; ok {
		return g.Alias
	}
	return ""
}

// GuideName 返回引导符的中文名。
func GuideName(guide byte) string {
	if g, ok := guides[guide]; ok {
		return g.Name
	}
	return "引导符 " + hex2(guide)
}

// GuideUnit 返回引导符的默认单位。
func GuideUnit(guide byte) string {
	if g, ok := guides[guide]; ok {
		return g.Unit
	}
	return ""
}

func hex2(b byte) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[b>>4], digits[b&0x0F]})
}
