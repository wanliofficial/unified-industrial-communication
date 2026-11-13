// Package sl651 实现《水文监测数据通信规约》SL 651—2014 的纯函数编解码。
//
// 这个协议与仓库里其它表计协议（DL/T 645、CJ/T 188、M-Bus、DLMS）的差别
// 不在「字段多少」，而在三件事，而且三件事都会让「凭印象写」的实现当场崩掉：
//
//  1. **分帧不能扫标志字节**。帧以 `7E 7E` 开头，但正文与校验码里可以合法
//     出现 0x7E（本文档测试向量第一帧的正文首字节就是 0x7E）。任何「读到
//     0x7E 就当帧尾」的写法都会把一帧切成两半。只能先读长度字段再按长度收。
//  2. **长度字段数的是「报文起始符之后、结束符之前」的字节数**，不含 STX、
//     不含 ETX/ETB、也不含 7E 7E。整帧 = 17 + 长度。
//  3. **校验码高位字节在前**，且算法是 CRC-16/MODBUS（初值 FFFF、右移、
//     多项式 8005、**结果不取反**）。仓库里其它协议多用 X-25（结果取反）
//     且低字节在前，照抄会得到 3C51 而不是 C3AE。这三条必须由金向量锁死。
//
// 还有一条只看「上行帧」永远发现不了的差别：**下行帧的中心站地址与遥测站
// 地址顺序相反**（表 11 上行 2=中心站/3=遥测站，表 12 下行 2=遥测站/3=中心站）。
// 只自报不上行的抓包看不出问题，一旦中心站发查询就会把站号发到错误的字段上。
//
// 三条事实都用规约原文（表 11/12、表 20 脚注、附录 H.3.2 的算法原文）与真实帧
// 逐字节验算确认过，不是记忆。
//
// # 数据模型：要素标识符就是数据模型本身
//
// SL 651 的正文不是「固定偏移的寄存器」，而是一串「要素标识符 + 数据」：
//
//	标识符 = 引导符(1 字节) + 数据定义字节(1 字节)
//	数据定义字节 = (数据字节数 << 3) | 小数位数
//
// 也就是说「报文自己告诉你每个值有多宽、有几位小数」，接收方不需要预先
// 协商点表格式。附录 C 的 133 项引导符（水位 Z、雨量 P1/PT、流量 Q、
// 电压 VT、闸位 GH…）就是协议的数据模型本身，本包用 tables.go 里的表
// 承载它，编码/解码一律查表，不按数字硬算。
//
// 例外是「引导符类」：观测时间（F0H）、测站编码（F1H）、人工置数（F2H）、
// 图片（F3H）、批量雨量/水位（F4H..FCH）这几个的长度在规约里固定，因此
// 数据定义字节被规定为固定值（附录 C 注 a~f）。它们的「数据字节数」不能
// 从定义字节的高 5 位读出，必须按引导符判定——这是实现里第二个容易踩的坑。
//
// # 定位：中心站侧，查询 + 应答
//
// 本包按「中心站（主站）」角色实现：
//
//   - 上行：解析遥测站自报的链路维持报（2FH）、测试报（30H）、定时报（32H）、
//     加报报（33H）、均匀时段报（31H）、小时报（34H）；
//   - 下行：生成查询实时数据（37H）、查询时段数据（38H）、查询指定要素（3AH）
//     三类查询帧；
//   - 上行应答：解析这三类查询的应答（结构与定时报同形，见规约表 43/45/49）。
//
// 驱动只做「查询→应答」这一条路径。自报帧的解析仍然实现，因为内置从站要用
// 同一份编码器造帧，自检命令也要能把抓到的自报帧打印成人能看懂的样子。
//
// # 明确不做
//
//   - ASCII 字符编码帧（规约 6.4）：长度字段是 4 个 ASCII 字符、数值用文本
//     表示、要素标识符是字符串。与 HEX/BCD 帧的分帧、校验、数据宽度全不同，
//     硬套只会得到一堆看似合理的错值。Parse 遇到它返回 ErrUnsupported。
//   - M3 多包重组（SYN 0x16 + 包总数/包序号）：重组需要「一次会话内收齐全部
//     包」的状态机，属于会话层。本包只解析单帧，遇到 ETB 结束符会如实标出
//     「后面还有包」，但不合并。
//   - 配置表读写（40H..43H）、控制命令（4CH..4FH）、时钟设置（4AH）、
//     修改密码（49H）、图片报（36H）：这些是写/配置/控制类，驱动是只读的。
//   - FFXXH 自定义扩展要素：XX 的含义只有设备方知道，猜出来的值不会报错、
//     只会污染点位，因此报 ErrUnknownGuide。
package sl651

import (
	"errors"
	"fmt"
)

// 链路层常量。
const (
	// Start 是帧起始符，连续出现两次。
	Start = 0x7E
	// STX 是报文起始符。
	STX = 0x02
	// Syn 是 M3 多包在「报文起始符」位置使用的同步符。
	Syn = 0x16

	// 上下行标识（长度字段高 4 位）：0000 上行，1000 下行。
	DirUp   = 0x0000
	DirDown = 0x8000

	// MaxLen 是长度字段低 12 位的上限（1..4095）。
	MaxLen = 0x0FFF

	// 结束符。上行用 ETX/ETB；下行用 ENQ/ACK/NAK/EOT/ESC。
	ETX = 0x03 // 报文结束，本帧即全部正文
	ETB = 0x17 // 报文结束，后面还有后续包（M3）
	ENQ = 0x05 // 下行查询（要求应答）
	ACK = 0x06 // 正确接收
	NAK = 0x15 // 未正确接收/要求重发
	EOT = 0x04 // 传输结束
	ESC = 0x1B // 要求终端保持在线
)

// 链路层固定字段的偏移。
const (
	offCenter   = 2  // 中心站地址（1 字节）
	offStation  = 3  // 遥测站地址（5 字节）
	offPassword = 8  // 密码（2 字节）
	offFunc     = 10 // 功能码
	offLen      = 11 // 上下行标识及长度（2 字节）
	offSTX      = 13 // 报文起始符
	crcLen      = 2
	// minFrameLen 是「最短合法帧」的头部长度：7E 7E + 中心站 + 站号 5 +
	// 密码 2 + 功能码 1 + 长度 2 + 报文起始符 1 = 14。
	minFrameLen = 14
)

// Frame 是一个解析后的 SL 651 帧。
type Frame struct {
	// Center 是中心站地址（1..255，0 表示未指定）。
	Center int
	// Station 是 5 字节遥测站地址（BCD）。
	Station [5]byte
	// Password 是 2 字节密码。
	Password [2]byte
	// Func 是功能码（附录 B）。
	Func byte
	// Dir 是上下行标识（DirUp / DirDown）。
	Dir int
	// Len 是长度字段低 12 位：报文起始符之后、结束符之前的字节数。
	Len int
	// Body 是正文（不含 STX 与结束符）。
	Body []byte
	// End 是报文结束符。
	End byte
	// CRC 是校验码原始两字节（高位在前）。
	CRC [2]byte
	// IsM3 为真表示报文起始符位置是 SYN（多包），Packets 有效。
	IsM3 bool
	// Packets 是 M3 的「包总数 + 本包序号」（3 字节，各 12 位）。
	Packets [3]byte
}

// 功能码（附录 B 表 B.1）。
const (
	FuncLink      = 0x2F // 链路维持报（上行）
	FuncTest      = 0x30 // 测试报（上行）
	FuncEven      = 0x31 // 均匀时段水文信息报（上行）
	FuncTimed     = 0x32 // 遥测站定时报（上行）
	FuncSurge     = 0x33 // 遥测站加报报（上行）
	FuncHourly    = 0x34 // 遥测站小时报（上行）
	FuncManual    = 0x35 // 遥测站人工置数报（上行）
	FuncImage     = 0x36 // 图片报/查询图片采集信息（双向）
	FuncQueryAll  = 0x37 // 中心站查询实时数据（下行）
	FuncQueryPer  = 0x38 // 中心站查询时段数据（下行）
	FuncQueryMan  = 0x39 // 中心站查询人工置数（下行）
	FuncQueryElem = 0x3A // 中心站查询指定要素数据（下行）
	FuncCfgSet    = 0x40 // 修改基本配置表（下行）
	FuncCfgGet    = 0x41 // 读取基本配置表/自报（双向）
	FuncParamSet  = 0x42 // 修改运行参数配置表（下行）
	FuncParamGet  = 0x43 // 读取运行参数配置表/自报（双向）
	FuncPump      = 0x44 // 查询水泵电机实时工作数据（下行）
	FuncVersion   = 0x45 // 查询终端软件版本（下行）
	FuncAlarm     = 0x46 // 查询状态和报警信息（下行）
	FuncClockSet  = 0x4A // 设置时钟（下行）
	FuncClockGet  = 0x51 // 查询时钟（下行）
)

// FuncName 返回功能码的中文名。
func FuncName(f byte) string {
	if n, ok := funcNames[f]; ok {
		return n
	}
	return fmt.Sprintf("功能码 %02X", f)
}

// IsUplinkReport 判断功能码是否为遥测站**自报**类上行帧。
//
// 自报是终端主动发起的（定时/加报/小时报/链路维持），中心站不索取。
// 注意它不含查询应答：37H/38H/3AH 在上行方向仍然是同一个功能码，
// 用 IsResponse 判断「这一帧是不是可解析的上行报文」。
func IsUplinkReport(f byte) bool {
	switch f {
	case FuncLink, FuncTest, FuncEven, FuncTimed, FuncSurge, FuncHourly, FuncManual:
		return true
	}
	return false
}

// IsResponse 判断功能码在上行方向是否是可解析的报文。
//
// 为什么需要它而不是直接用 IsUplinkReport：查询类功能码（37H 实时、38H 时段、
// 3AH 指定要素）的**应答沿用查询的功能码**（表 43/45/49），只是方向变成上行。
// 只看功能码无法区分「下行查询」和「上行应答」，必须配合帧的 Dir 字段。
// 漏掉这一条的症状很隐蔽：自报能解析、查询应答全被当成「不是上行报」丢掉，
// 而 total 上 BCD 与 CRC 都正确。
func IsResponse(f byte) bool {
	return IsUplinkReport(f) || IsQuery(f)
}

// IsQuery 判断功能码是否为「中心站查询」下行帧。
func IsQuery(f byte) bool {
	switch f {
	case FuncQueryAll, FuncQueryPer, FuncQueryMan, FuncQueryElem:
		return true
	}
	return false
}

// 遥测站分类码（附录 A）。ASCII 首字母即编码低位。
const (
	ClassRain      = 0x50 // P 降水
	ClassRiver     = 0x48 // H 河道
	ClassReservoir = 0x4B // K 水库（闸坝）
	ClassGate      = 0x5A // Z 闸坝
	ClassPump      = 0x44 // D 泵站
	ClassSilt      = 0x54 // T 凝沙
	ClassSoil      = 0x4D // M 墒情
	ClassGW        = 0x47 // G 地下水
	ClassWater     = 0x51 // Q 水质
	ClassCanal     = 0x49 // I 渠水口
	ClassDrain     = 0x4F // O 排水口
)

// ClassName 把分类码翻成中文名。
func ClassName(c byte) string {
	if n, ok := classNames[c]; ok {
		return n
	}
	return fmt.Sprintf("分类码 %02X", c)
}

// 错误分类。驱动把这些错误翻成 driver.ErrProtocol / ErrUnsupported。
var (
	// ErrBadFrame 表示帧结构不符（起始符、报文起始符、结束符、长度）。
	ErrBadFrame = errors.New("sl651: bad frame")
	// ErrShort 表示字节不足。
	ErrShort = errors.New("sl651: short buffer")
	// ErrCRC 表示校验码不匹配。
	ErrCRC = errors.New("sl651: crc mismatch")
	// ErrUnsupported 表示帧/功能码/要素不在本次实现范围内。
	ErrUnsupported = errors.New("sl651: unsupported")
)

var classNames = map[byte]string{
	ClassRain:      "降水",
	ClassRiver:     "河道",
	ClassReservoir: "水库（闸坝）",
	ClassGate:      "闸坝",
	ClassPump:      "泵站",
	ClassSilt:      "凝沙",
	ClassSoil:      "墒情",
	ClassGW:        "地下水",
	ClassWater:     "水质",
	ClassCanal:     "渠水口",
	ClassDrain:     "排水口",
}

var funcNames = map[byte]string{
	FuncLink:      "链路维持报",
	FuncTest:      "测试报",
	FuncEven:      "均匀时段水文信息报",
	FuncTimed:     "遥测站定时报",
	FuncSurge:     "遥测站加报报",
	FuncHourly:    "遥测站小时报",
	FuncManual:    "遥测站人工置数报",
	FuncImage:     "图片报",
	FuncQueryAll:  "查询实时数据",
	FuncQueryPer:  "查询时段数据",
	FuncQueryMan:  "查询人工置数",
	FuncQueryElem: "查询指定要素数据",
	FuncCfgSet:    "修改基本配置表",
	FuncCfgGet:    "读取基本配置表",
	FuncParamSet:  "修改运行参数配置表",
	FuncParamGet:  "读取运行参数配置表",
	FuncPump:      "查询水泵电机实时工作数据",
	FuncVersion:   "查询终端软件版本",
	FuncAlarm:     "查询状态和报警信息",
	FuncClockSet:  "设置时钟",
	FuncClockGet:  "查询时钟",
}
