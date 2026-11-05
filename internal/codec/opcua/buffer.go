// Package opcua 实现 OPC UA 二进制协议（IEC 62541）的最小子集：
// 连接握手、安全通道、会话、读、写、浏览。
//
// 它与 codec/modbus、codec/s7 的定位相同：纯函数编解码，不做 I/O、
// 不读时钟（除报文头的时间戳由调用方传入）、不持有连接状态。会话状态
// 在 driver/opcua 里，地址空间在 sim/opcua 里。
//
// 只实现 SecurityPolicy=None / SecurityMode=None。这不是「先做个简化版」，
// 而是一个明确的边界：证书链、签名与加密需要另一套密钥生命周期管理，
// 与采集链路正交；把两者混在一起，会让「连不上」的原因变得不可判定。
package opcua

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrShort 表示报文在读取过程中提前结束。
var ErrShort = errors.New("opcua: truncated message")

// decoder 是带错误状态的小端解码器。任何一步失败后后续读取都返回零值，
// 调用方只需在末尾检查 Err()——这让「解析一个结构」不必每一步都写 if。
type decoder struct {
	b   []byte
	pos int
	err error
}

func newDecoder(b []byte) *decoder { return &decoder{b: b} }

// u32 / appendU32 是报文头所需的极短工具：报文头固定小端，与结构体
// 编解码共用同一种字节序。
func u32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func appendU32(dst []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

func (d *decoder) Err() error { return d.err }

func (d *decoder) Remaining() int { return len(d.b) - d.pos }

func (d *decoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || d.pos+n > len(d.b) {
		d.err = ErrShort
		return nil
	}
	out := d.b[d.pos : d.pos+n]
	d.pos += n
	return out
}

func (d *decoder) Byte() byte {
	b := d.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (d *decoder) Bool() bool { return d.Byte() != 0 }

func (d *decoder) U16() uint16 {
	b := d.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (d *decoder) U32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (d *decoder) U64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (d *decoder) I16() int16   { return int16(d.U16()) }
func (d *decoder) I32() int32   { return int32(d.U32()) }
func (d *decoder) I64() int64   { return int64(d.U64()) }
func (d *decoder) F32() float32 { return math.Float32frombits(d.U32()) }
func (d *decoder) F64() float64 { return math.Float64frombits(d.U64()) }

// String 读取 OPC UA String：int32 长度 + UTF-8。长度 -1（或 0）表示空。
func (d *decoder) String() string {
	n := d.I32()
	if n <= 0 {
		if n < -1 {
			d.fail(fmt.Errorf("opcua: invalid string length %d", n))
		}
		return ""
	}
	return string(d.take(int(n)))
}

// ByteString 读取 ByteString：int32 长度 + 原始字节。-1 表示 null。
func (d *decoder) ByteString() []byte {
	n := d.I32()
	if n <= 0 {
		if n < -1 {
			d.fail(fmt.Errorf("opcua: invalid bytes length %d", n))
		}
		return nil
	}
	b := d.take(int(n))
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// Time 读取 DateTime：自 1601-01-01 起的 100ns 数。
func (d *decoder) Time() time.Time {
	v := d.U64()
	if d.err != nil || v == 0 {
		return time.Time{}
	}
	const epochOffset = 116444736000000000 // 1601 → 1970，单位 100ns
	return time.Unix(0, int64(v-epochOffset)*100).UTC()
}

func (d *decoder) Skip(n int) { d.take(n) }

// ---------------------------------------------------------------------------
// 编码器
// ---------------------------------------------------------------------------

type encoder struct{ b []byte }

func (e *encoder) Bytes() []byte { return e.b }

func (e *encoder) Byte(v byte) { e.b = append(e.b, v) }
func (e *encoder) Bool(v bool) {
	if v {
		e.Byte(1)
		return
	}
	e.Byte(0)
}

func (e *encoder) U16(v uint16) {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	e.b = append(e.b, buf[:]...)
}

func (e *encoder) U32(v uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	e.b = append(e.b, buf[:]...)
}

func (e *encoder) U64(v uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	e.b = append(e.b, buf[:]...)
}

func (e *encoder) I16(v int16)   { e.U16(uint16(v)) }
func (e *encoder) I32(v int32)   { e.U32(uint32(v)) }
func (e *encoder) I64(v int64)   { e.U64(uint64(v)) }
func (e *encoder) F32(v float32) { e.U32(math.Float32bits(v)) }
func (e *encoder) F64(v float64) { e.U64(math.Float64bits(v)) }

// String 写 OPC UA String。空串写成 null（-1）：两者的语义在所有
// 服务端实现里都被视为空，而 null 少两个字节且不会与「长度 0」混淆。
func (e *encoder) String(s string) {
	if s == "" {
		e.I32(-1)
		return
	}
	e.I32(int32(len(s)))
	e.b = append(e.b, s...)
}

func (e *encoder) ByteString(b []byte) {
	if b == nil {
		e.I32(-1)
		return
	}
	e.I32(int32(len(b)))
	e.b = append(e.b, b...)
}

func (e *encoder) Time(t time.Time) {
	if t.IsZero() {
		e.U64(0)
		return
	}
	const epochOffset = 116444736000000000
	e.U64(uint64(t.UTC().UnixNano()/100 + epochOffset))
}

func (e *encoder) Skip(n int) {
	for i := 0; i < n; i++ {
		e.Byte(0)
	}
}

// ArrayLength 写数组长度；nil 与空都写 -1（null），避免某些服务端
// 把「长度 0 的数组」当成需要额外类型信息的空数组。
func (e *encoder) ArrayLength(n int) {
	if n <= 0 {
		e.I32(-1)
		return
	}
	e.I32(int32(n))
}

// decodeArrayLen 读取数组长度并做上界检查，防止坏报文触发巨量分配。
func (d *decoder) decodeArrayLen(max int) int {
	n := d.I32()
	if d.err != nil {
		return 0
	}
	if n <= 0 {
		return 0
	}
	if int(n) > max {
		d.fail(fmt.Errorf("opcua: array length %d exceeds limit %d", n, max))
		return 0
	}
	return int(n)
}
