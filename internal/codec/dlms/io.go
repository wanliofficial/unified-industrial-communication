package dlms

import (
	"errors"
	"fmt"

	"uic/internal/transport"
)

// 本文件是「收帧」这一层，与仓库其它表计协议（DL/T 645、CJ/T 188）的 io.go
// 同构：主站与内置从站共用同一份扫描逻辑，避免两边对「哪里是一帧」的理解
// 漂移。
//
// HDLC 的分帧比那两个协议更难，原因有二：
//
//  1. **校验序列有两道**：有 info 时 HCS 覆盖头部（format+dest+src+ctl）、
//     FCS 覆盖全帧；无 info 时 HCS 就是唯一校验序列，长度恒为 7。
//  2. **info 与 FCS 里可以合法出现 0x7E**：类型 3 帧格式不做透明转义
//     （无比特填充、无字节转义），这正是它带 11 位长度字段的理由。按"扫到
//     下一个 7E"分帧会把某些合法帧从中间切断，且症状是"只有个别点位超时"。
//
// 因此分帧依据是**长度字段**（配以结束标志校验），校验序列负责判定这一帧
// 内容对不对。两者分工明确，不要混用。
//
// Reader 不持有连接状态：它只是「一段可增长的缓冲区 + 一个读入回调」，
// 谁提供字节由调用方决定。

// Reader 从字节流中按 HDLC 分帧取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st  transport.Stream
	buf []byte

	// maxFrame 是一帧的上限，防止对端（或噪声）声称一个巨大的长度之后
	// 无限吞字节。默认 MaxFrameLength + 2（标志位）。
	maxFrame int

	// badFrames 累计「看起来像帧但校验不过」的次数。用于诊断：持续增长
	// 说明线路噪声或波特率错；突然归零又说明链路恢复了。
	badFrames int
}

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st transport.Stream) *Reader {
	return &Reader{st: st, maxFrame: MaxFrameLength + 2}
}

// BadFrames 返回累计的坏帧计数。
func (r *Reader) BadFrames() int { return r.badFrames }

// Next 读取下一帧。它只在「拿到一整帧」或「出错」时返回。
//
// 分帧依据是**长度字段**：类型 3 帧在 info 与 FCS 里都可以合法出现 0x7E，
// 所以不能用"扫下一个标志"来定帧尾（见文件头说明）。长度字段可信时，
// 一帧的边界就是它声明的那么多字节；校验序列只负责判定这段内容对不对。
//
// 坏帧被丢弃后从下一个字节重新同步。调用了 SetDeadline 的流在超时后返回
// 底层错误，本函数不吞它。
func (r *Reader) Next() (Frame, error) {
	for {
		fr, consumed, err := ParseRaw(r.buf)
		switch {
		case err == nil:
			r.buf = r.buf[consumed:]
			return fr, nil
		case errors.Is(err, ErrIncomplete):
			// 丢掉起始标志之前的噪声，标志本身留着等后续字节。
			if i := indexFlag(r.buf); i > 0 {
				r.buf = r.buf[i:]
			}
			if len(r.buf) > r.maxFrame {
				r.buf = nil
				return Frame{}, fmt.Errorf("%w: 缓冲超过 %d 字节仍未成帧", ErrFrame, r.maxFrame)
			}
			// 能算出整帧长度就一次读够，避免逐字节调用 ReadFull
			// （串口上每次系统调用都有显著开销）。
			want := 1
			if n, ok := FrameLen(r.buf); ok {
				if need := n - len(r.buf); need > 0 {
					want = need
				}
			}
			if cap(r.buf)-len(r.buf) < want {
				// 扩容，让后续 append 不必反复复制。
				nb := make([]byte, len(r.buf), len(r.buf)+want+256)
				copy(nb, r.buf)
				r.buf = nb
			}
			chunk, rerr := r.st.ReadFull(want)
			if rerr != nil {
				return Frame{}, rerr
			}
			r.buf = append(r.buf, chunk...)
		default:
			// 结构或校验错误：丢掉指定位数，从下一个字节重新同步。
			r.badFrames++
			if consumed <= 0 || consumed > len(r.buf) {
				consumed = 1
			}
			r.buf = r.buf[consumed:]
		}
	}
}

// indexFlag 返回第一个 7E 的下标，找不到返回 -1。
func indexFlag(b []byte) int {
	for i, c := range b {
		if c == Flag {
			return i
		}
	}
	return -1
}

// WriteFrame 写出已组好的帧（自动补首尾 7E 标志），把「写一帧」变成一次调用。
func WriteFrame(st transport.Stream, body []byte) error {
	if len(body) == 0 {
		return errors.New("dlms: 空帧")
	}
	fr := make([]byte, 0, len(body)+2)
	fr = append(fr, Flag)
	fr = append(fr, body...)
	fr = append(fr, Flag)
	return st.Write(fr)
}
