package toledo

import (
	"fmt"

	"uic/internal/transport"
)

// 本文件是「收帧」这一层。连续输出的帧边界只有 CR/LF，没有长度字段也没有
// 二进制同步字，而衡器在总线上是**主动推帧**、还常与其它设备串在同一条线上——
// 这让「怎么从一段字节流里认出一帧」变成一件必须写对、否则症状极其罕见的事。
//
// 典型故障：一次连接上电时仪表打印的欢迎横幅、或另一台设备吐的字节，被当成
// 连续帧的开头，于是此后每一帧都错位一截。因此这里强制以「首字节是已知状态
// 助记符（S/T/U/O/L 之一）或 STX(0x02)」为帧的起点，其余一切丢弃并计数。
//
// 主站驱动与内置秤模拟**都不用**这里的 Reader（它们各自知道自己在读/写），但
// 内置秤的测试要用真实 TCP 回环，走的就是这份分帧逻辑。

// Reader 从字节流中按 CR/LF 分帧取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st  transport.Stream
	buf []byte
	// maxFrame 是单帧上限，防止对端（或噪声）一直不回 CR/LF 时无限吞字节。
	maxFrame int
	// bytesRead 是从链路读到的原始字节总数（含被丢弃的），供诊断区分
	// 「链路上没有字节」与「有字节但一帧都解不出」。
	bytesRead int
	// dropped 累计当作噪声丢弃的字节数，供诊断输出。
	dropped int
}

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st transport.Stream) *Reader {
	return &Reader{st: st, maxFrame: MaxFrame}
}

// Buffered 返回尚未被消费的字节数。
func (r *Reader) Buffered() int { return len(r.buf) }

// BytesRead 返回从链路读到的原始字节总数（含被当作噪声丢弃的）。
//
// 它与 Dropped() 一起构成本驱动最重要的一条诊断判据：「读了 0 字节」是链路/
// 接线问题，「读了 N 字节但一帧都没成」是帧格式/参数问题。两者排查方向完全不同。
func (r *Reader) BytesRead() int { return r.bytesRead }

// Dropped 返回被当作噪声丢弃的字节数。
//
// 这个数字值得上报：一条串着多设备的线上它持续增长，说明有别的设备在串话或
// 波特率对不上；它一直是 0 也不代表链路正常，只是说明没收到过东西。
func (r *Reader) Dropped() int { return r.dropped }

// Next 读取下一帧。只在「拿到一整帧」或「出错」时返回：
//
//   - CR/LF 之前不是「合法首字节」的字节当作噪声丢弃，直到出现一个头；
//   - 拿到合法首字节后，读到 CR 或 LF 即成一帧（行尾 CR 与 LF 一并消费）；
//   - 调用了 SetDeadline 的流在超时后返回底层错误，本函数不吞它。
func (r *Reader) Next() ([]byte, error) {
	for {
		// 先丢掉头部之前的一切。这一步不能省：它同时也是「上一轮残留」
		// 与「线上噪声」的唯一屏障。
		start := -1
		for i, c := range r.buf {
			if isFrameStart(c) {
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
			r.bytesRead += len(chunk)
			r.buf = append(r.buf, chunk...)
			continue
		}
		if start > 0 {
			r.dropped += start
			r.buf = r.buf[start:]
		}

		// 已确认合法首字节：读到 CR 或 LF 即成一帧。
		if end := indexTerm(r.buf); end >= 0 {
			frame := append([]byte(nil), r.buf[:end]...)
			// 消费行尾（CR、LF 或 CRLF 都算边界，一并吃掉避免空帧）。
			adv := end + 1
			if adv < len(r.buf) && r.buf[end] == '\r' && r.buf[adv] == '\n' {
				adv++
			}
			r.buf = r.buf[adv:]
			return frame, nil
		}
		if len(r.buf) >= r.maxFrame {
			// 一段一直没有 CR/LF 的数据：清掉，别让它把后续好帧一起拖死。
			r.dropped += len(r.buf)
			r.buf = nil
			return nil, fmt.Errorf("toledo: no frame terminator (CR/LF) within %d bytes", r.maxFrame)
		}
		chunk, err := r.st.ReadFull(1)
		if err != nil {
			return nil, err
		}
		r.bytesRead += len(chunk)
		r.buf = append(r.buf, chunk...)
	}
}

// isFrameStart 判断一个字节是否可能是连续帧的起点。
//
// 只认大写：真实连续帧的状态段恒为大写（ST/US/OL/UL，见 D39 等实例）。若把
// 小写也纳入，噪声里任何一个 l/o/s/t/u 都会被当成帧起点——现场横幅、其它
// 设备的 ASCII 输出里这些字母太常见了，等于没做噪声过滤。Parse 仍接受小写
// 状态（为健壮），但收帧这一层从严，宁可漏一帧也不误切。
func isFrameStart(c byte) bool {
	return c == 'S' || c == 'T' || c == 'U' || c == 'O' || c == 'L' || c == 0x02
}

// indexTerm 返回 buf 中第一个 CR 或 LF 的下标，没有则返回 -1。
func indexTerm(buf []byte) int {
	for i, c := range buf {
		if c == '\r' || c == '\n' {
			return i
		}
	}
	return -1
}
