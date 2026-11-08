package dlt645

import (
	"errors"
	"fmt"

	"uic/internal/transport"
)

// 本文件是「收帧」这一层。它放在 codec 包里而不是驱动里，原因只有一个：
// 主站与内置从站需要用**同一份**扫描逻辑。这个循环里的每个判断（跳过
// 前导、按 L 推算总长、校验失败后回退几个字节）都极其容易写错，而写错
// 的表现是「偶发」——总线安静时一切正常，一旦出现噪声或残留字节就整段
// 数据错位。复制两份就等于给自己留下两个只在现场出现的 bug。
//
// 这里仍然不持有连接状态：Reader 只是「一段可增长的缓冲区 + 一个读入
// 回调」，谁提供字节由调用方决定。

// Reader 从字节流中按 DL/T 645 分帧取出完整帧。
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
//   - 噪声与不完整的前一帧会被丢弃（RLen 语义），直到出现一帧校验正确的；
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
			} else if len(r.buf) == 0 {
				want = 1
			}
			if len(r.buf) > r.maxFrame {
				r.buf = nil
				return Frame{}, fmt.Errorf("dlt645: 缓冲区超过 %d 字节仍未成帧", r.maxFrame)
			}
			chunk, rerr := r.st.ReadFull(want)
			if rerr != nil {
				return Frame{}, rerr
			}
			r.buf = append(r.buf, chunk...)
		default:
			// 同步或校验失败：跳过出错位置，继续扫描。把整段缓冲区清空
			// 会让紧随其后的一帧好数据一起丢掉——总线上一声噪声就让
			// 这一轮采集全空，现场很难理解。
			if consumed <= 0 {
				consumed = 1
			}
			r.buf = r.buf[consumed:]
		}
	}
}

// FrameLen 在缓冲区开头是帧起始符、且已含 10 字节帧头时返回**整帧**长度
// （含 CS 与结束符）。第二个返回值 false 表示还看不出来。
func FrameLen(buf []byte) (int, bool) {
	if len(buf) < FixedLen || buf[0] != StartByte {
		return 0, false
	}
	return FixedLen + int(buf[9]) + 2, true
}

// WriteFrame 组帧并写满。前导字节由 Build 负责，这里只是把「写一帧」变成
// 一次调用，避免调用方漏掉错误处理。
func WriteFrame(st transport.Stream, fr []byte) error {
	if len(fr) == 0 {
		return errors.New("dlt645: 空帧")
	}
	return st.Write(fr)
}

// ---------------------------------------------------------------------------
// 应答解析
// ---------------------------------------------------------------------------

// ReadReply 是一条读数据应答的结构化视图。
type ReadReply struct {
	DI     DI
	Data   []byte // 已减 0x33，不含 DI；读后续帧时也不含末尾的 SEQ
	Seq    byte   // 读后续帧的帧序号（读数据帧为 0）
	Follow bool   // 从站声明还有后续帧
}

// ParseReadReply 解析 0x91 / 0xB1（读数据应答）。
//
// 数据域前 4 字节一定是 DI 回显。为什么要校验回显：总线上若有一台表的
// 应答迟到，它带的 DI 是上一次请求的，长度也不同；不校验就会把别的数据
// 项按本次的小数位解释，读出的电量「看起来合理但差了 1000 倍」。
func ParseReadReply(fr Frame, want DI) (ReadReply, error) {
	return parseDataReply(fr, want, false)
}

// ParseReadNextReply 解析 0x92 / 0xB2（读后续数据应答）。
func ParseReadNextReply(fr Frame, want DI, seq byte) (ReadReply, error) {
	return parseDataReply(fr, want, true, seq)
}

func parseDataReply(fr Frame, want DI, next bool, seq ...byte) (ReadReply, error) {
	if fr.IsError() {
		return ReadReply{}, fr.Exception()
	}
	got, err := DIFromBytes(fr.Data)
	if err != nil {
		return ReadReply{}, fmt.Errorf("dlt645: 应答数据域过短（%d 字节）：%w", len(fr.Data), err)
	}
	if got != want {
		return ReadReply{}, fmt.Errorf("dlt645: 应答数据标识 %s 与请求 %s 不一致", got, want)
	}
	data := fr.Data[4:]
	if next {
		if len(data) < 1 {
			return ReadReply{}, errors.New("dlt645: 读后续应答缺少帧序号")
		}
		gotSeq := data[len(data)-1]
		data = data[:len(data)-1]
		if len(seq) > 0 && gotSeq != seq[0] {
			return ReadReply{}, fmt.Errorf("dlt645: 读后续应答帧序号 %d 与请求 %d 不一致", gotSeq, seq[0])
		}
		return ReadReply{DI: got, Data: data, Seq: gotSeq, Follow: fr.HasFollow()}, nil
	}
	return ReadReply{DI: got, Data: data, Follow: fr.HasFollow()}, nil
}

// ParseAddrReply 解析 0x93（读通信地址应答）：数据域就是 6 字节地址。
func ParseAddrReply(fr Frame) ([AddrLen]byte, error) {
	var out [AddrLen]byte
	if fr.IsError() {
		return out, fr.Exception()
	}
	if len(fr.Data) < AddrLen {
		return out, fmt.Errorf("dlt645: 读地址应答数据域只有 %d 字节", len(fr.Data))
	}
	copy(out[:], fr.Data[:AddrLen])
	return out, nil
}

// WriteAck 判断一条写数据应答是否表示成功。
//
// 成功应答是 0x94 且 L=0；有的表会多回一个字节，这不算错（标准允许
// 厂商扩展），因此只要求「不是异常应答」。
func WriteAck(fr Frame) error {
	if fr.IsError() {
		return fr.Exception()
	}
	if fr.Func() != CtrlWrite {
		return fmt.Errorf("dlt645: 写数据应答控制码 0x%02X 不是 0x94", fr.Ctrl)
	}
	return nil
}

// ExceptionKind 把异常应答的错误信息字归类。分类结果决定上层是否重试、
// 是否熔断——「密码错」重试一万次也没用，「其它错误」则可能是设备忙。
type ExceptionKind int

const (
	// ExceptionAddress 表示「这台表没有这个数据项」类错误。
	ExceptionAddress ExceptionKind = iota + 1
	// ExceptionPermission 表示权限类错误（密码错、未授权）。
	ExceptionPermission
	// ExceptionProtocol 表示帧本身有问题（长度、参数超限等）。
	ExceptionProtocol
)

// Kind 返回错误信息字的分类。
func (e *Exception) Kind() ExceptionKind {
	switch {
	case e.Has(ErrPassword):
		return ExceptionPermission
	case e.Has(ErrNoData):
		return ExceptionAddress
	case e.Has(ErrOther):
		return ExceptionProtocol
	case e.Has(ErrBaud), e.Has(ErrYearZone), e.Has(ErrDayPeriod), e.Has(ErrRateCount):
		// 「速率不能改」「时段数超」这类是参数越界，不是链路故障。
		return ExceptionProtocol
	}
	return ExceptionProtocol
}
