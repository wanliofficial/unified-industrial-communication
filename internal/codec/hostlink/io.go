package hostlink

import (
	"fmt"

	"uic/internal/transport"
)

// 本文件是「收帧」这一层。放在 codec 包里而不是驱动里，是因为主站与内置
// 从站需要**同一份**扫描逻辑：一边按 `*`+CR 收，另一边按同样的规则收，
// 才能保证「本地能回环、现场也能对上」。
//
// 为什么不能「读到 CR 就算一帧」：
//
//   - 噪声、别的主站的 C 模式残留、以及某些设备在 `*CR` 后补的 CRLF 都会
//     带来裸 CR。按第一个 CR 收尾就会把一帧切成两半，FCS 永远算不对；
//   - 半双工总线上一次迟到的应答会污染下一轮。只看「有 CR」就会把上一轮
//     的旧数据当成新应答，值看起来合理但永远滞后一轮——现场极难发现。
//
// 因此这里强制首字节是 `@`：`@` 之前的一切丢弃并计数。这不是洁癖，它是
// 「上一轮残留」的唯一屏障。

// Reader 从字节流中按 Host Link 规则取出完整帧。
//
// 一个 Reader 只能被一个 goroutine 使用（它持有未消费的残余字节）。
type Reader struct {
	st  transport.Stream
	buf []byte
	// dropped 累计丢弃的噪声字节数，供诊断输出。
	dropped int
}

// NewReader 返回从 st 收帧的 Reader。
func NewReader(st transport.Stream) *Reader {
	return &Reader{st: st}
}

// Buffered 返回尚未被消费的字节数。
func (r *Reader) Buffered() int { return len(r.buf) }

// Dropped 返回被当作噪声丢弃的字节数。
//
// 这个数字值得上报：一条 485 总线上它持续增长，说明有别的设备在串话、
// 波特率/校验位对不上，或者对端在发 C 模式的别的帧。
func (r *Reader) Dropped() int { return r.dropped }

// Next 读取下一帧（含 `*` 与 CR 的原始字节）。只在「拿到一整帧」或
// 「出错」时返回。
//
// 流上的 SetDeadline 由调用方设置；超时后底层读取返回错误，本函数不吞它。
func (r *Reader) Next() ([]byte, error) {
	for {
		// 丢掉 `@` 之前的噪声。
		if start := indexByte(r.buf, Start); start < 0 {
			r.dropped += len(r.buf)
			r.buf = r.buf[:0]
		} else if start > 0 {
			r.dropped += start
			r.buf = r.buf[start:]
		}

		if len(r.buf) > 0 {
			if n, ok := FrameLen(r.buf); ok {
				frame := append([]byte(nil), r.buf[:n]...)
				r.buf = append(r.buf[:0], r.buf[n:]...)
				return frame, nil
			}
			if len(r.buf) >= MaxFrame {
				// 一段一直没有 `*`+CR 的数据：清掉，别让它把后续好帧一起
				// 拖死。这通常意味着波特率或校验位不对，收到的全是乱码。
				r.dropped += len(r.buf)
				r.buf = r.buf[:0]
				return nil, fmt.Errorf("hostlink: no frame terminator within %d bytes "+
					"(check baud rate, parity, data bits and stop bits)", MaxFrame)
			}
		}

		chunk, err := r.st.ReadFull(1)
		if err != nil {
			return nil, err
		}
		r.buf = append(r.buf, chunk...)
	}
}

// ReadFrame 读一帧并解析成应答。三步固定：收帧 → 解析（校验 FCS）→
// 校验单元号与 SID。
//
// 为什么把「单元号/SID 校验」放在这里而不是驱动里：任何一处漏掉它，症状
// 都是「值有，但偶尔跳到另一个设备的数」，而且不会报错。放进唯一的收帧
// 入口，就没有漏的可能。
func (r *Reader) ReadFrame(unit, sid byte) (Response, error) {
	raw, err := r.Next()
	if err != nil {
		return Response{}, err
	}
	resp, err := ParseResponse(raw)
	if err != nil {
		return Response{}, err
	}
	if resp.Unit != unit {
		return Response{}, &UnitMismatch{Got: resp.Unit, Want: unit}
	}
	if resp.SID != sid {
		return Response{}, &SIDMismatch{Got: resp.SID, Want: sid}
	}
	if resp.EndCode != 0 {
		return Response{}, &EndCodeError{Code: resp.EndCode}
	}
	return resp, nil
}

// SIDMismatch 表示应答的 SID 与请求不一致，即「这条应答不是给我这一轮
// 请求的」。典型原因是上一轮超时后应答迟到，落在这一轮的窗口里。
type SIDMismatch struct {
	Got  byte
	Want byte
}

func (e *SIDMismatch) Error() string {
	return fmt.Sprintf("hostlink: response carries SID %02X, expected %02X "+
		"(a late response from a previous request?)", e.Got, e.Want)
}

// Comm 标记这是「应答对不上号」，属线路/时序问题而非点位问题。
func (e *SIDMismatch) Comm() bool { return true }

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
