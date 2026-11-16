// Package mewtocol 实现 Mewtocol（计算机链接方式）的纯函数编解码。
//
// 只做「字节 ↔ 结构」：不建连接、不读时钟、不持有状态。主站与内置从站
// 共用这里的区域表与帧构造，避免两边对同一个区名的理解漂移。
//
// 帧的全部内容都是 ASCII 字符，没有二进制字段。一条单帧消息是：
//
//	% 01 # R D D0100 00002 XX CR          请求：读 DT100..DT102
//	% 01 $ R D 0063 3344 000A XX CR       正常应答
//	% 01 ! 41 XX CR                      异常应答（BCC 错）
//
// 逐段：
//
//	%            头。`<` 是扩展头的形式（长帧用），本实现只发 `%`。
//	01           站号，**两位十进制** ASCII。
//	#            命令符号；应答是 `$`，异常应答是 `!`。
//	RD           命令码，两个大写字母。
//	D0100 00002  命令正文：区码 + 起始地址 + 结束地址（以 RD 为例）。
//	XX           BCC，两位十六进制 ASCII。
//	CR           结束符 0x0D。
//
// 六个最容易写错的地方，都在这里说清楚：
//
//  1. **站号是十进制，不是十六进制。** 站号 16 写成 "16" 而不是 "10"；
//     而同一帧里的 BCC 与数据是十六进制。两种进制混排，是这套协议最
//     常见的解析错误来源。
//
//  2. **地址是十进制，但继电器的末位是十六进制。** X/Y/R/L 的编号写成
//     「前面十进制 + 末位十六进制」：X1F 是第 1 组第 15 位，线上写
//     "001F"。T/C 的编号全是十进制。本实现让调用方给「字地址 + 位号」
//     两个整数，由这里合成线上文本——把进制混排留在编解码层，点表里
//     不出现字母，也就不会出现「X1F 写成 X1f 算不算同一个点」的问题。
//
//  3. **字内是低字节在前。** 手册的例子：寄存器值 H0063，线上收 "6300"。
//     所以一个字在线上是「低字节, 高字节」。本包的 Encode/DecodeWords
//     在边界上做这次交换，对外一律给出大端规范化的字节——让协议的小端
//     习惯止步于编解码层，比让每个调用方都记着交换一遍安全。
//
//  4. **一次读用「起止地址」表达，不是「起点 + 个数」。** RD 的正文是
//     `区码 + 起始地址(5) + 结束地址(5)`，结束地址必须 >= 起始地址。
//     写成「起点 + 个数」在点数上差一，症状是最后一个点位恒坏。
//
//  5. **写的正文也带结束地址。** 这一点尤其反直觉：WD 的正文是
//     `区码 + 起始地址(5) + 结束地址(5) + 数据`，而不是「起始地址 +
//     数据」。长度其实由数据字符数决定，结束地址是**冗余**的；但协议
//     要求它，而且它必须与数据长度自洽，否则 PLC 回 41H 格式错。本包
//     在组帧时由数据长度推出结束地址，让两者不可能不一致。
//
//  6. **应答回显的命令码只有两个字符。** 请求 `RCC`（按字读接点）的
//     应答是 `$RC`，不是 `$RCC`；`WCS` 的应答是 `$WC`。第三位是
//     「单点/多点/字单位」的变体位，只在请求里出现。所以校验应答时
//     只能比对前两位，按整条命令码比对会把每一条接点命令都判成错帧。
//
// 另外两条容易忽略的差异：
//
//   - **RCC/WCC 的地址是 4 位，RD/WD 的是 5 位。** 两条命令的地址宽度
//     不同，复制粘贴改命令码时会静默错位。
//   - **索引寄存器没有编号。** IX/IY/ID 的区码是**两个字符**，后面紧跟
//     9 个 0——起始与结束地址两段被这 9 个字符整体取代（不是各写 5 个）。
//     读写都一样，只有数据长度不同。
package mewtocol

import (
	"fmt"
	"strconv"
	"strings"
)

// 帧定界符与符号。
const (
	Header     = '%'
	HeaderLong = '<' // 扩展头（长帧），本实现不发，收帧时认它
	SymbolCmd  = '#'
	SymbolOK   = '$'
	SymbolErr  = '!'
	Terminator = '\r'
	// BCCWildcard 是写在 BCC 位上表示「本帧不带块校验」的两个字符（"**"）。
	//
	// 手册（WUME-MEWCP-01 §1.1）只在**发送方**这一侧允许它：主站可以用
	// "**" 代替真实 BCC，PLC 照样执行命令。应答侧没有这条豁免——PLC 的应答
	// 恒带真实 BCC。
	//
	// 因此这个常量只对「解析主站请求」有意义；解析应答时出现 "**" 一律是
	// 坏帧（ChecksumError，可重试）。旧实现两侧都放行，等于把一条未校验的
	// 字节流（噪声、别的协议串话、被中间设备改写过的帧）当成合法应答收下，
	// 而这类帧在总线上出现本身就是链路异常的信号——静默接受比报错危险得多。
	BCCWildcard = '*'
)

// 帧长限制。
//
// 手册给的是 `%` 头 118 字符、`<` 头 2048 字符。收帧上限取 2048：现场
// 有网关回长帧，按 118 截断会把一帧好数据切成两半。发送侧仍按 118 校验，
// 因为对端未必支持扩展头。
const (
	MaxFrame    = 2048
	MaxFramePct = 118
	// MinFrame 是最短合法帧：`%`+站号2+符号1+命令码2+BCC2+CR = 9。
	// 异常应答 `%01!41XX CR` 与空数据应答 `%01$RDXX CR` 都是 9。
	MinFrame = 9
	// MaxWords 是一帧能承载的字数上限。
	//
	// 手册没有直接给「一次能读多少字」，只给了帧长 118。读与写的约束
	// 落在不同的帧上，而且**写更紧**——因为写的请求里既有两个地址又有
	// 数据，读的请求只有两个地址：
	//
	//	读请求 RD   `%`+站号2+`#`+`RD`+区码1+5+5+BCC2+CR = 20（定长）
	//	读应答      `%`+站号2+`$`+`RD`+4n+BCC2+CR        = 9+4n
	//	写请求 WD   `%`+站号2+`#`+`WD`+区码1+5+5+4n+BCC2+CR = 20+4n
	//
	// 于是：读受应答限，9+4n ≤ 118 → n ≤ 27；写受请求限，20+4n ≤ 118
	// → n ≤ 24。取较小者 24，一个数覆盖读写两侧，不需要调用方区分。
	//
	// 这个上限会被两处用到：组帧前拦一次（错误信息能指名命令码），
	// 驱动拆分 span 时再拦一次（错误信息能指名是哪台设备的哪一段）。
	MaxWords = 24
)

// 命令码。只收录驱动与内置从站实际用到的九条。
const (
	// CmdReadData 读数据区（DT / LD / FL / IX / IY / ID）。
	CmdReadData = "RD"
	// CmdWriteData 写数据区。
	CmdWriteData = "WD"
	// CmdReadContactWord 按字读接点区（X / Y / R / L）。
	//
	// 手册允许对 T/C 也用这条，但会按「每字 16 个接点」重新编号
	// （0000 表示 T0..T15），与 T/C 自身的十进制编号是两套体系。手册
	// 明确建议不要这么做；本实现对 T/C 只走 RCS/RCP（见下）。
	CmdReadContactWord = "RCC"
	// CmdWriteContactWord 按字写接点区。
	CmdWriteContactWord = "WCC"
	// CmdReadContactOne 读单个接点（RCS）。地址是 4 位接点号。
	CmdReadContactOne = "RCS"
	// CmdReadContacts 读多个接点（RCP，一次 1..8 个）。
	CmdReadContacts = "RCP"
	// CmdWriteContactOne 写单个接点。它是唯一能「只改一位、不动邻位」
	// 的写操作，因此布尔点位一律走它，而不是写字再掩码。
	CmdWriteContactOne = "WCS"
	// CmdWriteContacts 写多个接点（WCP，一次 1..8 个）。
	CmdWriteContacts = "WCP"
	// CmdReadSetValue 读定时器/计数器设定值 SV。
	CmdReadSetValue = "RS"
	// CmdWriteSetValue 写 SV。
	CmdWriteSetValue = "WS"
	// CmdReadElapsed 读定时器/计数器经过值 EV。
	CmdReadElapsed = "RK"
	// CmdWriteElapsed 写 EV。
	CmdWriteElapsed = "WK"
)

// Kind 是区域种类，决定这个区能走哪些命令码。
type Kind int

const (
	// KindContact 是继电器接点区：X / Y / R / L / T / C。
	// 可按位写（WCS），也可按字读写（RCC/WCC）。
	KindContact Kind = iota
	// KindWord 是字区：DT / LD / FL。
	KindWord
	// KindIndex 是索引寄存器：IX / IY / ID。没有编号，地址恒为 0。
	KindIndex
	// KindSetValue 是定时器/计数器的设定值区 SV。
	KindSetValue
	// KindElapsed 是定时器/计数器的经过值区 EV。
	KindElapsed
)

// Area 描述一个可寻址区域。
type Area struct {
	// Code 是线上区域码（1~2 个大写字母）。SV/EV 没有区域码：协议为它们
	// 单开了 RS/WS/RK/WK 四条命令，正文只有裸地址。
	Code string
	// Key 是配置里用的规范短名（小写）。
	Key  string
	Kind Kind
	Name string
	// Hex 表示该区域的接点编号「末位是十六进制」。X/Y/R/L 为真，
	// T/C 为假（全十进制）。
	Hex bool
}

// areas 是区域表。顺序就是文档与枚举里的展示顺序，不按字母排：先接点、
// 再字区、再索引、最后定时器/计数器——与点表里常见的书写顺序一致。
var areas = []Area{
	{Code: "X", Key: "x", Kind: KindContact, Name: "外部输入继电器 X", Hex: true},
	{Code: "Y", Key: "y", Kind: KindContact, Name: "外部输出继电器 Y", Hex: true},
	{Code: "R", Key: "r", Kind: KindContact, Name: "内部继电器 R", Hex: true},
	{Code: "L", Key: "l", Kind: KindContact, Name: "链接继电器 L", Hex: true},
	{Code: "T", Key: "t", Kind: KindContact, Name: "定时器接点 T"},
	{Code: "C", Key: "c", Kind: KindContact, Name: "计数器接点 C"},

	{Code: "D", Key: "dt", Kind: KindWord, Name: "数据寄存器 DT"},
	{Code: "L", Key: "ld", Kind: KindWord, Name: "链接数据寄存器 LD"},
	{Code: "F", Key: "fl", Kind: KindWord, Name: "文件寄存器 FL"},

	{Code: "IX", Key: "ix", Kind: KindIndex, Name: "索引寄存器 IX"},
	{Code: "IY", Key: "iy", Kind: KindIndex, Name: "索引寄存器 IY"},
	{Code: "ID", Key: "id", Kind: KindIndex, Name: "索引寄存器 IX+IY（32 位）"},

	{Code: "", Key: "sv", Kind: KindSetValue, Name: "定时器/计数器设定值 SV"},
	{Code: "", Key: "ev", Kind: KindElapsed, Name: "定时器/计数器经过值 EV"},
}

// aliases 把现场常见的写法映射到规范键。
//
// `d` 是最常见的写法（西门子/三菱的习惯），而松下文档里叫 DT；`f` 与
// FL 同理。两个都接受，但内部只认一个键——否则同一个区在规划层会被
// 拆成两个 span，点位看起来「都在读」，实际每次只读一半。
//
// 注意 `k` 映射到 EV 是历史写法（有的文档把经过值记作 K），而 `set`
// 映射到 SV。这两个别名容易混淆，因此在错误信息里永远打印规范键。
var aliases = map[string]string{
	"d":   "dt",
	"dt":  "dt",
	"ld":  "ld",
	"f":   "fl",
	"fl":  "fl",
	"wx":  "x",
	"wy":  "y",
	"wr":  "r",
	"wl":  "l",
	"i0":  "ix",
	"i1":  "iy",
	"sv":  "sv",
	"set": "sv",
	"ev":  "ev",
	"k":   "ev",
}

// Lookup 按区名查区域定义。大小写不敏感，接受别名，也直接接受区域码
// （`DT`、`X`、`IX` 都能写进来）。
func Lookup(name string) (Area, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if a, ok := aliases[key]; ok {
		key = a
	}
	for _, a := range areas {
		if a.Key == key {
			return a, true
		}
		if a.Code != "" && strings.EqualFold(a.Code, name) {
			return a, true
		}
	}
	return Area{}, false
}

// Areas 返回全部区域定义（固定顺序，供枚举与文档使用）。
func Areas() []Area { return append([]Area(nil), areas...) }

// IndexWords 返回索引寄存器区在线上占的字数；非索引区返回 0。
//
// IX/IY 各是一个 16 位寄存器，回 1 字；ID 是 IX+IY 拼成的 32 位寄存器
// （低位字 = IX），回 2 字。RD 的正文对索引寄存器恒为「区码 + 9 个 0」——
// 那条命令里没有起止地址，所以**宽度只能由区码决定**，不能由请求里的
// 地址推出来。
//
// 这一条写在手册 §1.3.7 的应答帧图上：`RDID` 的应答是 `Register contents
// (I0) 4 characters` + `Register contents (I1) 4 characters`，共 8 个字符
// = 2 个字；`RDIX`/`RDIY` 只有 4 个字符 = 1 个字。把它当成「一个区一种宽度」
// 而不区分 ID，会让 ID 的读写稳定失败（读：应答 2 字、请求按 1 字校验；
// 写：只发 I0，I1 保持旧值——而写操作报成功）。
func IndexWords(a Area) int {
	if a.Kind != KindIndex {
		return 0
	}
	if a.Code == "ID" {
		return 2
	}
	return 1
}

// Keys 返回全部规范区名（固定顺序）。
func Keys() []string {
	out := make([]string, 0, len(areas))
	for _, a := range areas {
		out = append(out, a.Key)
	}
	return out
}

// ---------------------------------------------------------------------------
// BCC
// ---------------------------------------------------------------------------

const hexDigits = "0123456789ABCDEF"

// BCC 计算从头部到正文末字符的异或值。
//
// 手册的例子：`%01#RCSX0000` 的异或结果是 0x1D，线上写成 "1D"。异或范围
// **包含头部 `%`**，不包含 BCC 自身与 CR。
func BCC(frame []byte) byte {
	var b byte
	for _, c := range frame {
		b ^= c
	}
	return b
}

// AppendBCC 计算并追加 BCC 与 CR。frame 必须已含头部但**不含** BCC 与 CR。
func AppendBCC(frame []byte) []byte {
	b := BCC(frame)
	out := make([]byte, 0, len(frame)+3)
	out = append(out, frame...)
	out = append(out, hexDigits[b>>4], hexDigits[b&0x0f], Terminator)
	return out
}

// ---------------------------------------------------------------------------
// 站号
// ---------------------------------------------------------------------------

// Broadcast 是广播站号（线上写作 "FF"）。
//
// 广播命令不返回应答，因此采集驱动不会用它：一个没有应答的读请求无法
// 产生任何点位值。它只在显式的运维动作（例如全网置位）里有意义。
const Broadcast Station = 0xFF

// Station 是目标站号。
//
// 范围：手册早期版本写 01..63，更新版本写 01..99（FP7），错误码 26H 的
// 说明写 01..64。三个数字不一致是这个协议的常态。本实现接受 1..99：
// 收了更宽的范围只会让一个错配的站号在运行期表现为「无应答」，而这本来
// 就是最可能的真实原因；反过来卡在 63 会让合法的 FP7 站号被拒在配置期。
type Station int

// Validate 校验站号。
func (s Station) Validate() error {
	if s == Broadcast {
		return nil
	}
	if s < 1 || s > 99 {
		return fmt.Errorf("mewtocol: station number %d out of range 1..99", int(s))
	}
	return nil
}

// Format 返回两位十进制文本（广播为 "FF"）。
func (s Station) Format() string {
	if s == Broadcast {
		return "FF"
	}
	return fmt.Sprintf("%02d", int(s))
}

// ParseStation 解析两位站号文本，"FF" 表示广播。
func ParseStation(text string) (Station, error) {
	if len(text) != 2 {
		return 0, fmt.Errorf("mewtocol: station field %q must be 2 characters", text)
	}
	if strings.EqualFold(text, "FF") {
		return Broadcast, nil
	}
	for i := 0; i < 2; i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, fmt.Errorf("mewtocol: station field %q is not two decimal digits", text)
		}
	}
	n, _ := strconv.Atoi(text)
	if n < 1 {
		return 0, fmt.Errorf("mewtocol: station number %q out of range 01..99", text)
	}
	return Station(n), nil
}

// ---------------------------------------------------------------------------
// 请求帧
// ---------------------------------------------------------------------------

// BuildReadWords 构造数据区读（RD），一次读 [start, end] 闭区间。
//
// 索引寄存器（IX/IY/ID）没有编号：地址位整体写 9 个 0，起止地址由区码
// 决定（IX/IY 是 0..0，ID 是 0..1）。此时仍要求调用方按 IndexWords 传入
// 那两个值，而不是「随便传、反正被忽略」——静默忽略地址会让一个偏移量
// 配错的点位拿到另一个寄存器的值。
func BuildReadWords(st Station, area Area, start, end int) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	switch area.Kind {
	case KindIndex:
		if w := IndexWords(area); start != 0 || end != w-1 {
			return nil, fmt.Errorf("mewtocol: %s has no address (index registers are not numbered): "+
				"read range must be 0..%d, got %d..%d", area.Key, w-1, start, end)
		}
		return request(st, CmdReadData, area.Code+"000000000")
	case KindWord:
		if start < 0 || start > 99999 {
			return nil, fmt.Errorf("mewtocol: %s address %d out of range 0..99999", area.Key, start)
		}
		if end < start || end > 99999 {
			return nil, fmt.Errorf("mewtocol: %s end address %d out of range %d..99999", area.Key, end, start)
		}
		// 一次请求的字数受帧长限制；早拦一层，错误信息能指出是哪一段。
		if end-start+1 > MaxWords {
			return nil, fmt.Errorf("mewtocol: %d words requested, one frame carries at most %d",
				end-start+1, MaxWords)
		}
		return request(st, CmdReadData, fmt.Sprintf("%s%05d%05d", area.Code, start, end))
	}
	return nil, fmt.Errorf("mewtocol: %s is not a word area", area.Key)
}

// BuildWriteWords 构造数据区写（WD）。
//
// 正文是 `区码 + 起始地址(5) + 结束地址(5) + 每字 4 字符`。结束地址在
// 协议里是冗余的（数据长度已经决定了写到哪），但它必须存在且自洽，
// 否则 PLC 回 41H 格式错。这里由数据长度反推结束地址，两者不可能不一致。
//
// 索引寄存器（IX/IY/ID）没有编号，正文是 `区码 + 9 个 0 + 数据`，而且
// 数据必须正好是该区的宽度（IX/IY 1 字，ID 2 字）：WDIX 写一个字、WDID
// 写两个字是同一条命令的两种合法形态，`WDIX` 带两个字或 `WDID` 只带一个
// 字都不是手册里的帧。尤其是后者——它会把 I1 留在旧值上却报成功，而 I1
// 恰恰是 32 位索引的高半部分。
//
// data 是大端规范化的原始字节，每字 2 字节；本函数负责在线上换成
// 「低字节在前」。
func BuildWriteWords(st Station, area Area, start int, data []byte) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%2 != 0 {
		return nil, fmt.Errorf("mewtocol: word write needs a non-empty even byte count, got %d", len(data))
	}
	words := len(data) / 2
	if words > MaxWords {
		return nil, fmt.Errorf("mewtocol: %d words in one frame, at most %d", words, MaxWords)
	}
	body, err := EncodeWords(data)
	if err != nil {
		return nil, err
	}
	switch area.Kind {
	case KindIndex:
		w := IndexWords(area)
		if start != 0 {
			return nil, fmt.Errorf("mewtocol: %s has no address (index registers are not numbered), "+
				"write offset must be 0, got %d", area.Key, start)
		}
		if words != w {
			return nil, fmt.Errorf("mewtocol: %s is a %d-word register, write carries %d",
				area.Key, w, words)
		}
		return request(st, CmdWriteData, area.Code+"000000000"+body)
	case KindWord:
		if start < 0 || start > 99999 || start+words-1 > 99999 {
			return nil, fmt.Errorf("mewtocol: %s write %d..%d out of range 0..99999",
				area.Key, start, start+words-1)
		}
		return request(st, CmdWriteData,
			fmt.Sprintf("%s%05d%05d%s", area.Code, start, start+words-1, body))
	}
	return nil, fmt.Errorf("mewtocol: %s is not a word area", area.Key)
}

// BuildReadContactWords 构造接点区按字读（RCC），读 [startWord, endWord]
// 闭区间，每字 16 个接点。
//
// 地址是 **4 位**——比 RD 少一位。这不是笔误。
func BuildReadContactWords(st Station, area Area, startWord, endWord int) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if area.Kind != KindContact {
		return nil, fmt.Errorf("mewtocol: %s is not a contact area", area.Key)
	}
	if startWord < 0 || startWord > 9999 {
		return nil, fmt.Errorf("mewtocol: %s word address %d out of range 0..9999", area.Key, startWord)
	}
	if endWord < startWord || endWord > 9999 {
		return nil, fmt.Errorf("mewtocol: %s end word %d out of range %d..9999", area.Key, endWord, startWord)
	}
	if endWord-startWord+1 > MaxWords {
		return nil, fmt.Errorf("mewtocol: %d contact words requested, one frame carries at most %d",
			endWord-startWord+1, MaxWords)
	}
	return request(st, CmdReadContactWord,
		fmt.Sprintf("%s%04d%04d", area.Code, startWord, endWord))
}

// BuildWriteContactWords 构造接点区按字写（WCC）。
//
// 与 WD 一样，正文里也要有结束字地址：`区码 + 起始(4) + 结束(4) + 数据`。
// 地址宽度是 4 位——比 WD 的 5 位少一位。
func BuildWriteContactWords(st Station, area Area, startWord int, data []byte) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if area.Kind != KindContact {
		return nil, fmt.Errorf("mewtocol: %s is not a contact area", area.Key)
	}
	if startWord < 0 || startWord > 9999 {
		return nil, fmt.Errorf("mewtocol: %s word address %d out of range 0..9999", area.Key, startWord)
	}
	if len(data) == 0 || len(data)%2 != 0 {
		return nil, fmt.Errorf("mewtocol: contact word write needs a non-empty even byte count, got %d", len(data))
	}
	words := len(data) / 2
	if words > MaxWords {
		return nil, fmt.Errorf("mewtocol: %d words in one frame, at most %d", words, MaxWords)
	}
	if startWord+words-1 > 9999 {
		return nil, fmt.Errorf("mewtocol: %s write %d..%d out of range 0..9999",
			area.Key, startWord, startWord+words-1)
	}
	body, err := EncodeWords(data)
	if err != nil {
		return nil, err
	}
	return request(st, CmdWriteContactWord,
		fmt.Sprintf("%s%04d%04d%s", area.Code, startWord, startWord+words-1, body))
}

// BuildReadContact 构造单接点读（RCS）。正文是 `区码 + 4 位接点号`。
//
// T/C 接点必须走这条（或 RCP），不能走 RCC 的按字读：手册允许按字读
// T/C，但会把它们按「每字 16 个接点」重新编号，与 T/C 自身的十进制编号
// 是两套体系，手册自己也建议不要用。
func BuildReadContact(st Station, area Area, word, bit int) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if area.Kind != KindContact {
		return nil, fmt.Errorf("mewtocol: %s is not a contact area", area.Key)
	}
	num, err := ContactNumber(area, word, bit)
	if err != nil {
		return nil, err
	}
	return request(st, CmdReadContactOne, area.Code+num)
}

// MaxContacts 是一次多点接点读写（RCP/WCP）能带的接点数上限。
//
// 手册给的是 1..8，由命令正文第一位的「个数」字段宽度决定，不是帧长
// 决定的——所以它比 MaxWords 小得多，且不能靠拆帧绕开。
const MaxContacts = 8

// BuildReadContacts 构造多接点读（RCP）。正文是 `个数(1) + 每点(区码 + 4 位接点号)`。
//
// 接点可以落在不同区（例如同时读 X0.1 与 R2.0），因此每个接点自带区码。
func BuildReadContacts(st Station, contacts []Contact) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if len(contacts) < 1 || len(contacts) > MaxContacts {
		return nil, fmt.Errorf("mewtocol: RCP carries 1..%d contacts, got %d", MaxContacts, len(contacts))
	}
	var sb strings.Builder
	sb.WriteByte(byte('0' + len(contacts)))
	for i, c := range contacts {
		if c.Area.Kind != KindContact {
			return nil, fmt.Errorf("mewtocol: contact %d: %s is not a contact area", i+1, c.Area.Key)
		}
		num, err := ContactNumber(c.Area, c.Word, c.Bit)
		if err != nil {
			return nil, fmt.Errorf("mewtocol: contact %d: %w", i+1, err)
		}
		sb.WriteString(c.Area.Code)
		sb.WriteString(num)
	}
	return request(st, CmdReadContacts, sb.String())
}

// BuildWriteContacts 构造多接点写（WCP）。正文是
// `个数(1) + 每点(区码 + 4 位接点号 + 数据位 1)`。
//
// 接点可以落在不同区（例如同时写 Y0.1 与 R2.0），因此每个接点自带区码。
// 驱动当前逐点走 WCS：WCP 在一次要改多个接点时能省掉往返，但它不是
// 原子的——中途失败时前面的接点已经改了，语义比「一条一条写」更难解释，
// 所以只在编解码层提供，留给明确知道代价的调用方。
func BuildWriteContacts(st Station, contacts []Contact) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if len(contacts) < 1 || len(contacts) > MaxContacts {
		return nil, fmt.Errorf("mewtocol: WCP carries 1..%d contacts, got %d", MaxContacts, len(contacts))
	}
	var sb strings.Builder
	sb.WriteByte(byte('0' + len(contacts)))
	for i, c := range contacts {
		if c.Area.Kind != KindContact {
			return nil, fmt.Errorf("mewtocol: contact %d: %s is not a contact area", i+1, c.Area.Key)
		}
		num, err := ContactNumber(c.Area, c.Word, c.Bit)
		if err != nil {
			return nil, fmt.Errorf("mewtocol: contact %d: %w", i+1, err)
		}
		sb.WriteString(c.Area.Code)
		sb.WriteString(num)
		if c.Value {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	}
	return request(st, CmdWriteContacts, sb.String())
}

// BuildWriteContact 构造单接点写（WCS）。
//
// 这是布尔点位唯一的写路径：它只改一位，不会碰到同字里的邻位。用「读字
// → 改位 → 写字」当然也能做到，但那把一次写变成一次读加一次写，而且
// 在两个事务之间别的客户端可能改了同一个字——最后写入者会把对方的位
// 一起覆盖掉。协议既然提供了原子的单点写，就没有理由不用。
func BuildWriteContact(st Station, area Area, word, bit int, value bool) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if area.Kind != KindContact {
		return nil, fmt.Errorf("mewtocol: %s is not a contact area", area.Key)
	}
	num, err := ContactNumber(area, word, bit)
	if err != nil {
		return nil, err
	}
	d := "0"
	if value {
		d = "1"
	}
	// 注意这里**不加** "S" 前缀：单点/多点/字单位的变体位就是命令码的
	// 第三个字符（WCS 的 S、WCP 的 P、WCC 的 C）。再写一次 "S" 会得到
	// 「WCSS...」，PLC 回 41H。
	return request(st, CmdWriteContactOne, fmt.Sprintf("%s%s%s", area.Code, num, d))
}

// BuildReadTimerValues 构造 SV/EV 读（RS/RK）。正文是裸的 4 位起止地址。
func BuildReadTimerValues(st Station, area Area, start, end int) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	cmd, err := timerCommand(area, false)
	if err != nil {
		return nil, err
	}
	if start < 0 || start > 9999 {
		return nil, fmt.Errorf("mewtocol: %s address %d out of range 0..9999", area.Key, start)
	}
	if end < start || end > 9999 {
		return nil, fmt.Errorf("mewtocol: %s end address %d out of range %d..9999", area.Key, end, start)
	}
	if end-start+1 > MaxWords {
		return nil, fmt.Errorf("mewtocol: %d values requested, one frame carries at most %d",
			end-start+1, MaxWords)
	}
	return request(st, cmd, fmt.Sprintf("%04d%04d", start, end))
}

// BuildWriteTimerValues 构造 SV/EV 写（WS/WK）。正文同样是 `起始(4) +
// 结束(4) + 数据`，没有区域码。
func BuildWriteTimerValues(st Station, area Area, start int, data []byte) ([]byte, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	cmd, err := timerCommand(area, true)
	if err != nil {
		return nil, err
	}
	if start < 0 || start > 9999 {
		return nil, fmt.Errorf("mewtocol: %s address %d out of range 0..9999", area.Key, start)
	}
	if len(data) == 0 || len(data)%2 != 0 {
		return nil, fmt.Errorf("mewtocol: %s write needs a non-empty even byte count, got %d", area.Key, len(data))
	}
	words := len(data) / 2
	if words > MaxWords {
		return nil, fmt.Errorf("mewtocol: %d words in one frame, at most %d", words, MaxWords)
	}
	if start+words-1 > 9999 {
		return nil, fmt.Errorf("mewtocol: %s write %d..%d out of range 0..9999",
			area.Key, start, start+words-1)
	}
	body, err := EncodeWords(data)
	if err != nil {
		return nil, err
	}
	return request(st, cmd, fmt.Sprintf("%04d%04d%s", start, start+words-1, body))
}

// timerCommand 返回 SV/EV 对应的命令码。
func timerCommand(area Area, write bool) (string, error) {
	switch area.Kind {
	case KindSetValue:
		if write {
			return CmdWriteSetValue, nil
		}
		return CmdReadSetValue, nil
	case KindElapsed:
		if write {
			return CmdWriteElapsed, nil
		}
		return CmdReadElapsed, nil
	}
	return "", fmt.Errorf("mewtocol: %s is not a timer/counter value area", area.Key)
}

// request 组一条请求帧：`%` + 站号 + `#` + 命令码 + 正文 + BCC + CR。
func request(st Station, cmd, body string) ([]byte, error) {
	frame := make([]byte, 0, 7+len(body))
	frame = append(frame, Header)
	frame = append(frame, st.Format()...)
	frame = append(frame, SymbolCmd)
	frame = append(frame, cmd...)
	frame = append(frame, body...)
	out := AppendBCC(frame)
	if len(out) > MaxFramePct {
		return nil, fmt.Errorf("mewtocol: %s request is %d characters, over the %d-character limit",
			cmd, len(out), MaxFramePct)
	}
	return out, nil
}

// ContactNumber 把「字地址 + 位号」合成 4 位接点号文本。
//
// 接点区（X/Y/R/L）的编号是「前面十进制、末位十六进制」：字 1 位 15
// 写作 "001F"，字 16 位 3 写作 "0163"。它不是纯十六进制，也不是纯十进制。
// T/C 的编号全是十进制：T10 就是 "0010"。
//
// 位号对 X/Y/R/L 一律 0..15；T/C 没有位号，bit 忽略（可传 -1）。
func ContactNumber(area Area, word, bit int) (string, error) {
	if word < 0 {
		return "", fmt.Errorf("mewtocol: contact %s%d out of range (word >= 0)", area.Code, word)
	}
	switch area.Kind {
	case KindContact:
	case KindWord, KindIndex:
		return "", fmt.Errorf("mewtocol: %s is a word area, not a contact", area.Key)
	default:
		return "", fmt.Errorf("mewtocol: %s has no contact numbering", area.Key)
	}
	if !area.Hex {
		// T/C：全十进制，一个号就是一个接点，没有位号。
		if word > 9999 {
			return "", fmt.Errorf("mewtocol: %s%d out of range 0..9999", area.Code, word)
		}
		return fmt.Sprintf("%04d", word), nil
	}
	if bit < 0 || bit > 15 {
		return "", fmt.Errorf("mewtocol: contact %s%d.%d out of range (bit 0..15)", area.Code, word, bit)
	}
	if word > 999 {
		return "", fmt.Errorf("mewtocol: %s%d out of range 0..999", area.Code, word)
	}
	return fmt.Sprintf("%03d%X", word, bit), nil
}

// ParseContactNumber 是 ContactNumber 的逆操作（内置从站解析请求时用）。
//
// 它必须与 ContactNumber 使用同一套进制规则；分成两处实现是这类协议里
// 「主站算出来是 001F、从站读成 31」的经典来源。
func ParseContactNumber(area Area, text string) (word, bit int, err error) {
	if len(text) != 4 {
		return 0, 0, fmt.Errorf("mewtocol: contact number %q must be 4 characters", text)
	}
	if !area.Hex {
		w, e := strconv.Atoi(text)
		if e != nil || w < 0 || w > 9999 {
			return 0, 0, fmt.Errorf("mewtocol: contact number %q is not a valid decimal %s address", text, area.Code)
		}
		return w, 0, nil
	}
	w, e := strconv.Atoi(text[:3])
	if e != nil || w < 0 || w > 999 {
		return 0, 0, fmt.Errorf("mewtocol: contact number %q has a non-decimal word part", text)
	}
	b, e := strconv.ParseUint(text[3:], 16, 8)
	if e != nil || b > 15 {
		return 0, 0, fmt.Errorf("mewtocol: contact number %q has an invalid hex bit digit", text)
	}
	return w, int(b), nil
}

// ---------------------------------------------------------------------------
// 应答
// ---------------------------------------------------------------------------

// Response 是一条已解析的应答帧。
type Response struct {
	Station Station
	// Code 是正常应答里回显的命令码；异常应答为空。
	Code string
	// Data 是正常应答的正文（不含命令码与 BCC）。
	Data string
	// ErrCode 是异常应答的两位十六进制错误码；正常应答为 0。
	ErrCode byte
	// Raw 是收到的原始帧，供诊断日志与 CLII 的 -raw 使用。
	Raw []byte
}

// OK 表示这是一条正常应答。
func (r Response) OK() bool { return r.ErrCode == 0 && r.Code != "" }

// Err 返回异常应答对应的错误；正常应答返回 nil。
func (r Response) Err() error {
	if r.ErrCode == 0 {
		return nil
	}
	return &ErrorCode{Code: r.ErrCode}
}

// ErrorCode 是 PLC 返回的异常应答码（`!` 后面的两位十六进制）。
type ErrorCode struct {
	Code byte
}

func (e *ErrorCode) Error() string {
	return fmt.Sprintf("mewtocol: error %02XH: %s", e.Code, e.Text())
}

// Text 给出错误码的人话说明。
//
// 文本来自手册的四张表：链路（22H..38H）、基本过程（40H..43H）、处理
// （50H..53H）、PLC 应用（60H..74H）。分四类不只是措辞问题：它决定
// 驱动该不该重试——链路类多半是暂时性的，参数/地址类重试一万次也一样。
func (e *ErrorCode) Text() string {
	if s, ok := errText[e.Code]; ok {
		return s
	}
	return "unknown error code"
}

// Kind 返回错误码类别。
func (e *ErrorCode) Kind() ErrorClass {
	switch {
	case e.Code >= 0x22 && e.Code <= 0x38:
		return ClassLink
	case e.Code >= 0x40 && e.Code <= 0x43:
		return ClassProcedure
	case e.Code >= 0x50 && e.Code <= 0x53:
		return ClassProcessing
	case e.Code >= 0x60 && e.Code <= 0x74:
		return ClassApplication
	}
	return ClassUnknown
}

// Retryable 表示这次失败「下一次可能就好了」。
//
// 只把暂时性的链路/处理错误算作可重试。地址错（66H）、参数错（60H）、
// 写保护（65H）重试一万次也是同一个结果；把它们算进熔断只会让一台配置
// 错的设备把整条通道拖黑。
func (e *ErrorCode) Retryable() bool {
	switch e.Kind() {
	case ClassLink:
		// 23H 站号冲突、26H 站号设置错、24H 硬件错都不是重试能解决的。
		switch e.Code {
		case 0x23, 0x24, 0x26:
			return false
		}
		return true
	case ClassProcedure:
		// 40H BCC 错值得重发一次；41H..43H 是组帧或时序错，重发通常
		// 同样失败，但成本低，仍算可重试。
		return true
	case ClassProcessing:
		return true
	case ClassApplication:
		switch e.Code {
		case 0x53, 0x72, 0x73, 0x74:
			return true
		}
		return false
	}
	return false
}

// ErrorClass 是错误码的四大类。
type ErrorClass int

const (
	ClassUnknown ErrorClass = iota
	ClassLink
	ClassProcedure
	ClassProcessing
	ClassApplication
)

func (c ErrorClass) String() string {
	switch c {
	case ClassLink:
		return "link"
	case ClassProcedure:
		return "procedure"
	case ClassProcessing:
		return "processing"
	case ClassApplication:
		return "application"
	}
	return "unknown"
}

var errText = map[byte]string{
	// 链路系统错误
	0x22: "WACK error: the partner node's receive buffer overflowed",
	0x23: "station number overlap: the source station number duplicates another node",
	0x24: "communication unit hardware error",
	0x26: "station number setting error: outside the allowed range",
	0x27: "no support error: the packet is not supported",
	0x28: "no response error: timed out waiting for the partner station",
	0x30: "time-out error: ongoing transmission disabled",
	0x32: "transmission impossible error: the source node buffer overflowed",
	0x33: "communication stop: the network access switch is off",
	0x36: "no destination error: no partner station on the network",
	0x38: "other communication error",
	// 基本过程错误
	0x40: "BCC error: the command data failed the block check",
	0x41: "format error: command data length or structure is wrong",
	0x42: "no support error: the command is not supported",
	0x43: "procedure error: a different command arrived while a transfer was pending",
	// 处理错误
	0x50: "link setting error: the specified route number does not exist",
	0x51: "simultaneous operation error: the transmission buffer overflowed",
	0x52: "transmit disable error: data cannot be transmitted",
	0x53: "busy error: a command arrived while multiple frames were being processed",
	// PLC 应用错误
	0x60: "parameter error: unsupported area or unit code",
	0x61: "data error: contact/area number or data format is out of range",
	0x62: "registration error: too many registrations, or none registered",
	0x63: "mode error: the operation mode does not allow this command",
	0x65: "protect error: the memory is write-protected",
	0x66: "address error: the address is outside the valid range",
	0x67: "no data error: the requested data does not exist",
	0x72: "time-out error: timed out waiting for a transmission answer",
	0x73: "time-out error: timed out waiting for the transmission buffer to empty",
	0x74: "time-out error: timed out waiting for a response",
}

// ChecksumError 表示 BCC 不匹配。
type ChecksumError struct {
	// Got 是从帧里算出来的 BCC。
	Got byte
	// Want 是帧里携带的 BCC；帧里写的是 "**"（不带块校验）时为 0，
	// 并由 Note 说明。
	Want byte
	// Note 是给人看的补充说明，例如「应答里出现 ** 而本协议不允许」。
	Note string
}

func (e *ChecksumError) Error() string {
	if e.Note != "" {
		return fmt.Sprintf("mewtocol: BCC mismatch: %s (computed %02XH)", e.Note, e.Got)
	}
	return fmt.Sprintf("mewtocol: BCC mismatch: frame carries %02XH, computed %02XH", e.Want, e.Got)
}

// ParseResponse 校验并解析一条完整应答帧（含 BCC 与 CR）。
func ParseResponse(raw []byte) (Response, error) {
	if len(raw) < MinFrame {
		return Response{}, fmt.Errorf("mewtocol: response too short (%d bytes)", len(raw))
	}
	if raw[len(raw)-1] != Terminator {
		return Response{}, fmt.Errorf("mewtocol: response does not end with CR")
	}
	if raw[0] != Header && raw[0] != HeaderLong {
		return Response{}, fmt.Errorf("mewtocol: bad header %q", raw[0])
	}
	body := raw[:len(raw)-1]
	// `% 01 $ RD <data> XX`
	//  0  1  2  3  4..
	if len(body) < 7 {
		return Response{}, fmt.Errorf("mewtocol: response body too short (%d bytes)", len(body))
	}
	station, err := ParseStation(string(body[1:3]))
	if err != nil {
		return Response{}, err
	}
	// 应答侧不接受 "**"：手册只给主站的命令留了这条豁免。
	if err := checkBCC(body, false); err != nil {
		return Response{}, err
	}
	switch body[3] {
	case SymbolOK:
		return Response{
			Station: station, Code: string(body[4:6]),
			Data: string(body[6:bccAtOf(body)]), Raw: raw,
		}, nil
	case SymbolErr:
		code, err := parseHexByte(string(body[4:6]))
		if err != nil {
			return Response{}, fmt.Errorf("mewtocol: bad error code %q: %w", body[4:6], err)
		}
		// 异常应答里错误码之后直接是 BCC，理论上不该有额外字符。
		// 收下但不解释，只把多余部分留在 Raw 里——报错比猜测安全。
		return Response{Station: station, ErrCode: code, Raw: raw}, nil
	}
	return Response{}, fmt.Errorf("mewtocol: unexpected response symbol %q", body[3])
}

// ---------------------------------------------------------------------------
// 正文编解码
// ---------------------------------------------------------------------------

// DecodeWords 把应答正文（每字 4 个十六进制字符）解成大端规范化的字节。
//
// 线上是「低字节, 高字节」：H0063 写成 "6300"，这里输出 0x00,0x63。
func DecodeWords(data string) ([]byte, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("mewtocol: word data length %d is not a multiple of 4", len(data))
	}
	out := make([]byte, 0, len(data)/2)
	for i := 0; i < len(data); i += 4 {
		lo, err := parseHexByte(data[i : i+2])
		if err != nil {
			return nil, fmt.Errorf("mewtocol: bad word data %q at offset %d", data[i:i+4], i)
		}
		hi, err := parseHexByte(data[i+2 : i+4])
		if err != nil {
			return nil, fmt.Errorf("mewtocol: bad word data %q at offset %d", data[i:i+4], i)
		}
		out = append(out, hi, lo)
	}
	return out, nil
}

// EncodeWords 是 DecodeWords 的逆操作：把大端字节写成「低字节在前」。
func EncodeWords(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("mewtocol: word encode needs an even byte count, got %d", len(b))
	}
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for i := 0; i < len(b); i += 2 {
		sb.WriteString(string(hexDigits[b[i+1]>>4]))
		sb.WriteString(string(hexDigits[b[i+1]&0x0f]))
		sb.WriteString(string(hexDigits[b[i]>>4]))
		sb.WriteString(string(hexDigits[b[i]&0x0f]))
	}
	return sb.String(), nil
}

// Words 返回一段正文声明的字数。调用方用它校验应答是否与请求等长。
func Words(data string) int { return len(data) / 4 }

func parseHexByte(s string) (byte, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("want 2 hex digits, got %d", len(s))
	}
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0, err
	}
	return byte(v), nil
}

// FrameLen 判断缓冲区开头这一帧是否已完整，返回帧长（含 CR）。
//
// 与二进制协议不同，这里没有长度字段：帧的边界只有 CR。返回 false 表示
// 还没读到 CR（或已超 MaxFrame）。
func FrameLen(buf []byte) (int, bool) {
	for i, c := range buf {
		if c == Terminator {
			return i + 1, true
		}
		if i >= MaxFrame-1 {
			return 0, false
		}
	}
	return 0, false
}
