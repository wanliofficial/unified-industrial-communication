package cjt188

import (
	"errors"
	"fmt"

	"uic/internal/transport"
)

// 本文件是「收帧」这一层。它放在 codec 包里而不是驱动里，原因与 DL/T 645
// 相同：主站与内置从站必须用**同一份**扫描逻辑。跳过前导、按 L 推算总长、
// 校验失败后回退几个字节——每个判断都容易写错，而写错的表现是「偶发」。
//
// Reader 不持有连接状态：它只是「一段可增长的缓冲区 + 一个读入回调」，
// 谁提供字节由调用方决定。

// Reader 从字节流中按 CJ/T 188 分帧取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st  transport.Stream
	buf []byte

	// maxFrame 是一帧的上限，防止对端（或噪声）声称一个巨大的 L 之后
	// 无限吞字节。默认 MaxFrame。
	maxFrame int
}

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st transport.Stream) *Reader { return &Reader{st: st, maxFrame: MaxFrame} }

// Buffered 返回尚未被消费的字节数，供诊断使用。
func (r *Reader) Buffered() int { return len(r.buf) }

// Next 读取下一帧。它只在「拿到一整帧」或「出错」时返回：
//
//   - 前导 FE 与噪声会被丢弃，直到出现一帧校验正确的；
//   - 调用了 SetDeadline 的流在超时后返回底层错误，本函数不吞它。
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
				// 已知总长，一次读满剩余部分，避免逐字节系统调用。
				if need := n - len(r.buf); need > 0 {
					want = need
				}
			}
			if len(r.buf) > r.maxFrame {
				r.buf = nil
				return Frame{}, fmt.Errorf("cjt188: 缓冲区超过 %d 字节仍未成帧", r.maxFrame)
			}
			chunk, rerr := r.st.ReadFull(want)
			if rerr != nil {
				return Frame{}, rerr
			}
			r.buf = append(r.buf, chunk...)
		default:
			// 同步或校验失败：跳过出错位置，继续扫描。把整段缓冲区清空
			// 会让紧随其后的一帧好数据一起丢掉。
			if consumed <= 0 {
				consumed = 1
			}
			r.buf = r.buf[consumed:]
		}
	}
}

// WriteFrame 组帧并写满。前导字节由 Build 负责，这里只是把「写一帧」变成
// 一次调用，避免调用方漏掉错误处理。
func WriteFrame(st transport.Stream, fr []byte) error {
	if len(fr) == 0 {
		return errors.New("cjt188: 空帧")
	}
	return st.Write(fr)
}
