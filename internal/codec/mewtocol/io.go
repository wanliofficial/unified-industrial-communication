package mewtocol

import (
	"fmt"

	"uic/internal/transport"
)

// 本文件是「收帧」这一层，放在 codec 包里而不是驱动里：主站与内置从站
// 需要用**同一份**扫描逻辑。Mewtocol 是全 ASCII 的，没有长度字段也没有
// 二进制同步字，帧边界只有 `%` 与 CR —— 这让「怎么从一段字节流里认出
// 一帧」变成一件必须写对、否则症状极其罕见的事。
//
// 典型故障：半双工总线上一次迟到的应答在下一轮被当成新应答的首部。若
// 收帧只看「读到 CR」，就会把上一轮的旧数据当成本轮结果，值看起来合理
// 但永远滞后一轮——这种错误在现场极难发现，因为「值都有，只是慢一秒」。
// 因此这里强制要求首字节是 `%`（或 `<`），其余一切丢弃。

// Reader 从字节流中按 Mewtocol 分帧取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st  transport.Stream
	buf []byte
	// maxFrame 是单帧上限，防止对端（或噪声）一直不回 CR 时无限吞字节。
	maxFrame int
	// dropped 累计丢弃的噪声字节数，供诊断输出。
	dropped int
}

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st transport.Stream) *Reader {
	return &Reader{st: st, maxFrame: MaxFrame}
}

// Buffered 返回尚未被消费的字节数。
func (r *Reader) Buffered() int { return len(r.buf) }

// Dropped 返回被当作噪声丢弃的字节数。
//
// 这个数字值得上报：一条 485 总线上它持续增长，说明有别的设备在串话或
// 波特率对不上；它一直是 0 也不代表链路正常，只是说明没收到过东西。
func (r *Reader) Dropped() int { return r.dropped }

// Next 读取下一帧。只在「拿到一整帧」或「出错」时返回：
//
//   - CR 之前不是 `%`/`<` 的字节当作噪声丢弃，直到出现一个头；
//   - 调用了 SetDeadline 的流在超时后返回底层错误，本函数不吞它。
func (r *Reader) Next() ([]byte, error) {
	for {
		// 先丢掉头部之前的一切。这一步不能省：它同时也是「上一轮残留」
		// 的唯一屏障。
		start := -1
		for i, c := range r.buf {
			if c == Header || c == HeaderLong {
				start = i
				break
			}
		}
		if start < 0 {
			r.dropped += len(r.buf)
			r.buf = nil
			chunk, err := r.st.ReadFull(1)
			if err != nil {
				return nil, err
			}
			r.buf = append(r.buf, chunk...)
			continue
		}
		if start > 0 {
			r.dropped += start
			r.buf = r.buf[start:]
		}

		if n, ok := FrameLen(r.buf); ok {
			frame := append([]byte(nil), r.buf[:n]...)
			r.buf = r.buf[n:]
			return frame, nil
		}
		if len(r.buf) >= r.maxFrame {
			// 一段一直没有 CR 的数据：清掉，别让它把后续好帧一起拖死。
			r.dropped += len(r.buf)
			r.buf = nil
			return nil, fmt.Errorf("mewtocol: no frame terminator within %d bytes", r.maxFrame)
		}
		chunk, err := r.st.ReadFull(1)
		if err != nil {
			return nil, err
		}
		r.buf = append(r.buf, chunk...)
	}
}
