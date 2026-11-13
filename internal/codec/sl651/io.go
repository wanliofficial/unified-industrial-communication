package sl651

import (
	"errors"
	"fmt"
)

// 金向量：由规约 6.6 的结构规则构造，并用「CRC 覆盖 7E7E 至结束符全部字节、
// 高位字节在前、长度 = 报文起始符之后结束符之前的字节数」三条件逐字节验算。
//
// 三条最容易写错的事实都靠这两帧回归：
//   - 正文里可以合法出现 0x7E（第一帧正文首字节就是 0x7E），所以分帧只能按
//     长度字段，不能扫描 7E；
//   - CRC 覆盖起点包含 7E 7E（不含起始符的计算结果不匹配）；
//   - CRC 高位字节在前（把两字节对调后校验失败）。
var goldenFrames = [][]byte{
	// 链路维持报（2FH）：正文 = 流水号 + 发报时间。
	// 7E7E | 00 | 98 76 54 32 10 | 12 34 | 2F | 0008 | 02 | 7E 08 22 10 11 10 07 47 | 03 | C3 AE
	{
		0x7E, 0x7E, 0x00, 0x98, 0x76, 0x54, 0x32, 0x10,
		0x12, 0x34, 0x2F, 0x00, 0x08, 0x02, 0x7E, 0x08,
		0x22, 0x10, 0x11, 0x10, 0x07, 0x47, 0x03, 0xC3,
		0xAE,
	},
	// 测试报（30H）：流水号 + 发报时间 + F1F1 站号 + 分类码 + F0F0 观测时间
	// + 当前降水量 0.5mm + 瞬时水位 0.127m + 电压 11.15V。
	// 7E7E | 01 | 00 12 34 56 78 | 12 34 | 30 | 002B | 02
	//   00 03 | 24 01 11 15 49 47 | F1 F1 | 00 12 34 56 78 | 48 | F0 F0 | 24 01 11 15 49
	//   20 19 00 00 05 | 39 23 00 00 01 27 | 38 12 11 15 | 03 | 71 7A
	{
		0x7E, 0x7E, 0x01, 0x00, 0x12, 0x34, 0x56, 0x78,
		0x12, 0x34, 0x30, 0x00, 0x2B, 0x02, 0x00, 0x03,
		0x24, 0x01, 0x11, 0x15, 0x49, 0x47, 0xF1, 0xF1,
		0x00, 0x12, 0x34, 0x56, 0x78, 0x48, 0xF0, 0xF0,
		0x24, 0x01, 0x11, 0x15, 0x49, 0x20, 0x19, 0x00,
		0x00, 0x05, 0x26, 0x19, 0x00, 0x00, 0x05, 0x39,
		0x23, 0x00, 0x00, 0x01, 0x27, 0x38, 0x12, 0x11,
		0x15, 0x03, 0x71, 0x7A,
	},
}

// ErrBadLen 表示长度字段与实际不符。
var ErrBadLen = errors.New("sl651: length mismatch")

// IsFrameStart 判断 buf 是否以 7E 7E 开头（帧起始符）。
func IsFrameStart(buf []byte) bool {
	return len(buf) >= 2 && buf[0] == Start && buf[1] == Start
}

// FrameLen 返回整帧字节数：17 + 长度字段值。
//
// 拆分：起始符 2 + 中心站 1 + 站号 5 + 密码 2 + 功能码 1 + 长度 2 = 13，
// 再加密文起始符 1 + 正文（长度）+ 结束符 1 + 校验码 2 = 17 + 长度。
func FrameLen(bodyLen int) int { return 17 + bodyLen }

// Parse 解析一个完整上行/下行帧（HEX/BCD 编码）。
//
// 输入必须是完整帧（含 CRC 与结束符）。分帧由 Reader 完成，本函数只负责
// 结构解析 + 校验。返回的错误分类：
//
//	ErrShort        字节不足
//	ErrBadFrame     起始符/结束符/结构不符
//	ErrCRC          校验失败
//	ErrUnsupported  ASCII 字符编码帧
func Parse(buf []byte) (*Frame, error) {
	if len(buf) < minFrameLen {
		return nil, ErrShort
	}
	if !IsFrameStart(buf) {
		return nil, fmt.Errorf("%w: missing 7E 7E start", ErrBadFrame)
	}

	// 长度字段高 4 位只有两种合法取值：0000 上行、1000 下行。ASCII 字符编码
	// 帧这一位置是 4 个 ASCII 字符，首字符要么是 '0'(0x30) 要么是 '8'(0x38)，
	// 不满足下面两条之一，一律按「不支持」报出去，而不是硬套 HEX 规则。
	f := &Frame{
		Password: [2]byte{buf[offPassword], buf[offPassword+1]},
		Func:     buf[offFunc],
	}

	// 长度字段高 4 位只有两种合法取值：0000 上行、1000 下行。ASCII 字符编码
	// 帧这一位置是 4 个 ASCII 字符，首字符要么是 '0'(0x30) 要么是 '8'(0x38)，
	// 不满足下面两条之一，一律按「不支持」报出去，而不是硬套 HEX 规则。
	//
	// 注意判的是高 4 位而不是整字节：正文 256..4095 字节时高字节低 4 位非零
	// （例如 0x1FF 字节的上行帧高字节是 0x01、下行是 0x81），按整字节比会把
	// 所有长帧误判成 ASCII 帧。
	if lenHi := buf[offLen]; lenHi&0xF0 != 0x00 && lenHi&0xF0 != 0x80 {
		return nil, fmt.Errorf("%w: length high byte 0x%02X (ASCII frame or corrupt)", ErrUnsupported, lenHi)
	}

	rawLen := int(buf[offLen])<<8 | int(buf[offLen+1])
	f.Dir = rawLen & 0xF000
	f.Len = rawLen & MaxLen
	if f.Len == 0 {
		return nil, fmt.Errorf("%w: zero body length", ErrBadFrame)
	}

	// 地址字段顺序随方向变化（表 11 / 表 12）：
	//   上行：2=中心站地址(1)  3=遥测站地址(5)
	//   下行：2=遥测站地址(5)  3=中心站地址(1)
	// 其它字段（密码、功能码、长度）位置不变，所以可以先读长度定方向，再读地址。
	if f.Dir == DirUp {
		f.Center = int(buf[offCenter])
		copy(f.Station[:], buf[offStation:offStation+5])
	} else {
		copy(f.Station[:], buf[offCenter:offCenter+5])
		f.Center = int(buf[offStation+4])
	}

	// 报文起始符位置：STX(0x02)；M3 多包在该位置放 SYN(0x16)，其后跟
	// 3 字节「包总数 + 包序号」（各 12 位，高 12 位包总数、低 12 位本包序号）。
	if buf[offSTX] == Syn {
		f.IsM3 = true
		if len(buf) < offSTX+4 {
			return nil, ErrShort
		}
		f.Packets = [3]byte{buf[offSTX+1], buf[offSTX+2], buf[offSTX+3]}
	} else if buf[offSTX] != STX {
		return nil, fmt.Errorf("%w: expected STX/SYN at %d, got 0x%02X", ErrBadFrame, offSTX, buf[offSTX])
	}

	// 正文 = 报文起始符之后、结束符之前的 Len 个字节；结束符紧随其后；校验码
	// 再往后 2 字节。
	//
	// 长度一律从报文起始符（offSTX）算起：M3 多包时该位置是 SYN，后面 3 字节
	// 包信息也计在 Len 之内（规约 6.4 的「报文起始符之后」）。因此 M3 帧的
	// Body 里前 3 字节是包信息，本包不拆包（见包注释）。
	endPos := offSTX + 1 + f.Len
	crcPos := endPos + 1
	if len(buf) < crcPos+crcLen {
		return nil, fmt.Errorf("%w: need %d bytes, got %d", ErrShort, crcPos+crcLen, len(buf))
	}

	f.Body = make([]byte, f.Len)
	copy(f.Body, buf[offSTX+1:endPos])
	f.End = buf[endPos]
	f.CRC = [2]byte{buf[crcPos], buf[crcPos+1]}

	// 校验：覆盖从 7E 7E 起、到结束符止的全部字节（不含校验码本身）。
	//
	// 注意不能写成 CheckCRC(buf[:crcPos])：CheckCRC 的约定是「末 2 字节即为
	// 校验码」，那样会把结束符和倒数第一个正文字节当成校验码来比。
	if got, want := CRC16(buf[:crcPos]),
		uint16(f.CRC[0])<<8|uint16(f.CRC[1]); got != want {
		return nil, fmt.Errorf("%w: got %04X, want %04X", ErrCRC, got, want)
	}

	// 结束符：上行 ETX/ETB；下行 ENQ/ACK/NAK/EOT/ESC。
	if f.Dir == DirUp {
		if f.End != ETX && f.End != ETB {
			return nil, fmt.Errorf("%w: bad uplink end 0x%02X", ErrBadFrame, f.End)
		}
	} else {
		switch f.End {
		case ENQ, ACK, NAK, EOT, ESC:
		default:
			return nil, fmt.Errorf("%w: bad downlink end 0x%02X", ErrBadFrame, f.End)
		}
	}

	return f, nil
}

// Reader 从字节流中按 SL 651 分帧取出完整帧。
//
// 与 DL/T 645 的分帧差别只有一点，但它是致命的：DL/T 645 用 `68 … 68` 双起始符
// 定界，SL 651 虽然也以 `7E 7E` 开头，**正文与校验码里可以合法出现 0x7E**
// （见 goldenFrames[0] 的正文首字节）。任何「扫描 0x7E 找帧尾」的写法都会在
// 遇到这类帧时把一帧切成两半。因此这里只按长度字段推算整帧长度。
type Reader struct {
	st interface {
		ReadFull(n int) ([]byte, error)
	}

	// maxFrame 是一帧上限（17 + 0xFFF），防止对端（或线路噪声）声称一个
	// 巨大长度后无限吞字节。
	maxFrame int

	// bad 是已丢弃的字节数（不是帧起始符的噪声、长度越界、校验失败的帧）。
	//
	// Next 故意不把坏帧抛给调用方：现场更常见的是「线路上有字节回来、只是
	// 全都不成帧」（波特率/校验位/接线问题）与「线路完全没动静」（站号错、
	// 线断、终端没电）两种情况，驱动要靠这个计数把它们分开报出来，否则现场
	// 看到的永远只有「超时」。
	bad int
}

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st interface {
	ReadFull(n int) ([]byte, error)
}) *Reader {
	return &Reader{st: st, maxFrame: FrameLen(MaxLen)}
}

// BadBytes 返回已被丢弃的字节数。
func (r *Reader) BadBytes() int { return r.bad }

// Next 读取下一帧。它只在「拿到一整帧」或「出错」时返回：
//
//   - 噪声与不完整的前一帧会被丢弃，直到出现一帧校验正确的；
//   - 调用了 SetDeadline 的流在超时后返回底层错误，本函数不吞它。
func (r *Reader) Next() (*Frame, error) {
	for {
		head, err := r.st.ReadFull(2)
		if err != nil {
			return nil, err
		}
		if !IsFrameStart(head) {
			r.bad += 2
			continue
		}

		// 再读 12 字节到长度字段末尾（含中心站 1 + 站号 5 + 密码 2 + 功能码 1
		// + 长度 2 + 报文起始符 1）；minFrameLen = 14。
		rest, err := r.st.ReadFull(minFrameLen - 2)
		if err != nil {
			return nil, err
		}
		head = append(head, rest...)

		lenHi := head[offLen]
		if lenHi&0xF0 != 0x00 && lenHi&0xF0 != 0x80 {
			// ASCII 帧或噪声：丢掉已读的头部，继续找下一帧。
			r.bad += len(head)
			continue
		}

		rawLen := int(head[offLen])<<8 | int(head[offLen+1])
		bodyLen := rawLen & MaxLen
		frameLen := FrameLen(bodyLen)
		if bodyLen == 0 || frameLen > r.maxFrame {
			r.bad += len(head)
			continue
		}

		if need := frameLen - minFrameLen; need > 0 {
			tail, err := r.st.ReadFull(need)
			if err != nil {
				return nil, err
			}
			head = append(head, tail...)
		}

		f, err := Parse(head)
		if err != nil {
			if errors.Is(err, ErrShort) {
				return nil, err
			}
			r.bad += len(head)
			continue
		}
		return f, nil
	}
}
