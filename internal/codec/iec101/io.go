package iec101

import (
	"errors"
	"fmt"

	"uic/internal/transport"
)

// 本文件是「从字节流里切出帧」这一层。它放在 codec 包里而不是驱动里，
// 原因与其它串行协议相同：主站与内置从站必须用**同一份**扫描逻辑。
// 噪声怎么跳、长度字段不可信时退几个字节、单字节确认怎么认——每一处
// 判断都容易写错，而写错的表现是「偶发丢帧」，在真实总线上极难复现。
//
// Reader 不持有连接状态：它只是「一段可增长的缓冲区 + 一个读入回调」，
// 谁提供字节由调用方决定。

// Reader 从字节流中按 FT1.2 取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st       transport.Stream
	buf      []byte
	addrSize int
	maxFrame int
}

// NewReader 返回从 st 收帧的 Reader。addrSize 是链路地址字节数（0..2）。
func NewReader(st transport.Stream, addrSize int) *Reader {
	return &Reader{st: st, addrSize: addrSize, maxFrame: MaxLen}
}

// Buffered 返回尚未被消费的字节数，供诊断使用。
func (r *Reader) Buffered() int { return len(r.buf) }

// Reset 丢弃尚未消费的字节。
//
// 调用时机只有一个：一次事务超时之后、开始下一次事务之前。串行总线上的
// 「上一次的应答」会晚到几百毫秒，如果不丢掉，下一次事务就会把它当成
// 自己的应答——症状是「命令总是回上一次的结果」，且只在慢设备上出现。
//
// 不能靠链路层的 Drain 代替：Drain 清的是内核/串口缓冲，而这里清的是
// 已经读进用户态、还没被解析的字节。
func (r *Reader) Reset() {
	r.buf = nil
}

// Next 读取下一帧。它只在「拿到一整帧」或「出错」时返回：
//
//   - 起始标识之前的噪声会被丢弃，直到出现一帧校验正确的；
//   - 调用了 SetDeadline 的流在超时后返回底层错误，本函数不吞它。
func (r *Reader) Next() (Frame, error) {
	for {
		if len(r.buf) > 0 {
			res, err := ParseAt(r.buf, r.addrSize)
			switch {
			case err == nil:
				r.buf = r.buf[res.Consumed:]
				return res.Frame, nil
			case errors.Is(err, ErrIncomplete):
				// 起始标识之前的噪声可以先丢掉；起始标识本身要留着等后续字节。
				if res.Consumed > 0 {
					r.buf = r.buf[res.Consumed:]
				}
			default:
				// 同步或校验失败：按解析结果前进，继续扫描。把整段缓冲区
				// 清空会让紧随其后的一帧好数据一起丢掉。
				if res.Consumed <= 0 {
					res.Consumed = 1
				}
				r.buf = r.buf[res.Consumed:]
				continue
			}
		}
		if len(r.buf) > r.maxFrame {
			r.buf = nil
			return Frame{}, fmt.Errorf("iec101: buffer exceeded %d bytes without a valid frame", r.maxFrame)
		}
		// 已知总长就一次读满，避免逐字节系统调用；否则先读 1 字节，
		// 等起始标识与长度字段到齐后再算。
		want := 1
		if n, ok := FrameLen(r.buf, r.addrSize); ok {
			if need := n - len(r.buf); need > 0 {
				want = need
			}
		}
		chunk, err := r.st.ReadFull(want)
		if err != nil {
			return Frame{}, err
		}
		r.buf = append(r.buf, chunk...)
	}
}

// WriteFrame 写一整帧。单独成函数只是为了让调用方不会漏掉错误处理。
func WriteFrame(st transport.Stream, fr []byte) error {
	if len(fr) == 0 {
		return errors.New("iec101: empty frame")
	}
	return st.Write(fr)
}
