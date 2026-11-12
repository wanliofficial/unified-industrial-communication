package mbus

import (
	"errors"
	"fmt"

	"uic/internal/transport"
)

// 本文件是"收帧"这一层。它放在 codec 包里而不是驱动里，理由与仓库里其它
// 表计协议相同：主站与内置从站必须用**同一份**扫描逻辑。
//
// M-Bus 的扫描比 DL/T 645 更难一点，因为它有四种起始符（E5/10/68/68），
// 其中 0x68 既是长帧头也是控制帧头，而 ACK 只有 1 个字节——"读到一个
// 字节就完成一帧"意味着如果主站把 ACK 与紧随其后的应答粘在一起读，
// 必须能先把 ACK 单独切出来。下面的循环对每个分支都只信任 Parse 的结论。
//
// Reader 不持有连接状态：它只是"一段可增长的缓冲区 + 一次读入"，谁提供
// 字节由调用方决定。

// Reader 从字节流中按 M-Bus 分帧取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st  transport.Stream
	buf []byte

	// maxFrame 是一帧的上限，防止对端（或噪声）声称一个巨大的 L 之后
	// 无限吞字节。长帧的 L 是单字节，天然不超过 255，这里的上限只是
	// 兜底。
	maxFrame int

	// bad 是"被跳过的坏帧"个数（校验和错、结构错、长度不符）。
	//
	// Next 故意不把坏帧抛给调用方（见下），但"线路上有字节回来、只是
	// 全都不成帧"与"线路完全没动静"是两种完全不同的故障：前者通常是
	// 波特率/校验位配错或终端电阻缺失，后者是站号错、线断了、表没电。
	// 驱动需要这个计数才能把两者分开报出来，否则现场只能看到"超时"。
	bad int
}

// BadFrames 返回已被跳过的坏帧个数。
func (r *Reader) BadFrames() int { return r.bad }

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st transport.Stream) *Reader { return &Reader{st: st, maxFrame: 300} }

// Buffered 返回尚未被消费的字节数，供诊断使用。
func (r *Reader) Buffered() int { return len(r.buf) }

// Next 读取下一帧。它只在"拿到一整帧"或"出错"时返回：
//
//   - 噪声与不完整的前一帧会被丢弃，直到出现一帧校验正确的；
//   - 调用了 SetDeadline 的流在超时后返回底层错误，本函数不吞它。
//
// 注意它会**跳过**校验和错误的帧继续扫描，而不是把错误抛给调用方：
// 半双工总线上一次应答可能被噪声切开，紧跟其后的往往是一帧完整的好数据。
// 需要知道"这条链路上出现过坏帧"的调用方应当自己统计——驱动的做法是
// 在超时且从未收到过好帧时才把问题归因为物理层。
func (r *Reader) Next() (Frame, error) {
	for {
		fr, consumed, err := Parse(r.buf)
		switch {
		case err == nil:
			r.buf = r.buf[consumed:]
			return fr, nil
		case errors.Is(err, ErrIncomplete):
			// 丢弃起始符之前的噪声（consumed 就是那些字节数），起始符
			// 本身必须留着等后续字节。
			if consumed > 0 {
				r.buf = r.buf[consumed:]
			}
			want := 1
			if n, ok := FrameLen(r.buf); ok {
				if need := n - len(r.buf); need > 0 {
					want = need
				}
			}
			if len(r.buf) > r.maxFrame {
				r.buf = nil
				return Frame{}, fmt.Errorf("mbus: 缓冲区超过 %d 字节仍未成帧", r.maxFrame)
			}
			chunk, rerr := r.st.ReadFull(want)
			if rerr != nil {
				return Frame{}, rerr
			}
			r.buf = append(r.buf, chunk...)
		default:
			// 结构错误或校验和错误：跳过出错位置继续扫描。把整段缓冲
			// 清空会让紧随其后的一帧好数据一起丢掉。
			//
			// 只有"确实像是帧、但内容不对"才计入 bad；单纯的重同步
			// （起始符之前的噪声）不算——总线上电瞬态本来就会有几字节
			// 垃圾，把它当成"坏帧"会让驱动误报线路质量差。
			if !errors.Is(err, ErrResync) {
				r.bad++
			}
			if consumed <= 0 {
				consumed = 1
			}
			r.buf = r.buf[consumed:]
		}
	}
}

// WriteFrame 写出已组好的帧，把"写一帧"变成一次带错误处理的调用。
func WriteFrame(st transport.Stream, fr []byte) error {
	if len(fr) == 0 {
		return errors.New("mbus: 空帧")
	}
	return st.Write(fr)
}
