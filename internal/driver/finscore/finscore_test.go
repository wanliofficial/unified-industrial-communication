package finscore

import (
	"errors"
	"testing"

	finscodec "uic/internal/codec/fins"
	"uic/internal/driver"
	"uic/internal/model"
)

// 这个文件只测共享层自己的判断：字节序默认值、上限夹紧、错误到质量的映射、
// 字数推断。内存区读写的端到端行为由两个信封各自的驱动测试覆盖（那边才是
// 「帧真的发出去了」的证据）。

func TestOrderFor(t *testing.T) {
	if OrderFor(2, "") != model.OrderCDAB || OrderFor(8, "") != model.OrderDCBA {
		t.Fatalf("defaults = %q/%q", OrderFor(2, ""), OrderFor(8, ""))
	}
	if OrderFor(2, model.OrderABCD) != model.OrderABCD {
		t.Fatal("explicit order ignored")
	}
}

func TestLimitsNormalize(t *testing.T) {
	// 未设置或超协议上限 → 回落默认值，而不是报错（上限是性能参数，
	// 不是地址语义；写错不该让设备起不来）。
	l := Limits{}.normalize()
	d := DefaultLimits()
	if l != d {
		t.Fatalf("zero limits = %+v, want %+v", l, d)
	}
	l = Limits{
		WordsPerRead:  finscodec.MaxWords + 1,
		WordsPerWrite: -5,
		BitsPerRead:   finscodec.MaxBits + 1,
		BitsPerWrite:  0,
	}.normalize()
	if l != d {
		t.Fatalf("out-of-range limits = %+v, want %+v", l, d)
	}
	// 合法值必须原样保留：Host Link 的 269/267 就是靠这条活下来的。
	in := Limits{WordsPerRead: 269, WordsPerWrite: 267, BitsPerRead: 538, BitsPerWrite: 534}
	if got := in.normalize(); got != in {
		t.Fatalf("valid limits changed: %+v", got)
	}
}

func TestDefaultLimitsFitTheProtocol(t *testing.T) {
	d := DefaultLimits()
	if d.WordsPerRead > finscodec.MaxWords || d.WordsPerWrite > finscodec.MaxWords {
		t.Fatalf("word limits exceed the protocol: %+v", d)
	}
	if d.BitsPerRead > finscodec.MaxBits || d.BitsPerWrite > finscodec.MaxBits {
		t.Fatalf("bit limits exceed the protocol: %+v", d)
	}
}

func TestQualityOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want model.Quality
	}{
		{"fins address", &finscodec.EndCodeError{Code: 0x1103}, model.QualityAddressError},
		{"fins link", &finscodec.EndCodeError{Code: 0x0202}, model.QualityCommError},
		{"fins other", &finscodec.EndCodeError{Code: 0x0401}, model.QualityBad},
		{"comm", commErr{}, model.QualityCommError},
		{"timeout", driver.E(driver.ErrTimeout, "read", "d", "x", true, nil), model.QualityTimeout},
		{"address", driver.E(driver.ErrAddress, "read", "d", "x", false, nil), model.QualityAddressError},
		{"disconnected", driver.E(driver.ErrDisconnected, "read", "d", "x", true, nil), model.QualityCommError},
		{"wrapped timeout", wrap(driver.E(driver.ErrTimeout, "read", "d", "x", true, nil)), model.QualityTimeout},
		{"nil-ish", errors.New("boom"), model.QualityBad},
	}
	for _, c := range cases {
		if got := QualityOf(c.err); got != c.want {
			t.Fatalf("%s: QualityOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// commErr 模拟信封层的「线路问题」标记。
type commErr struct{}

func (commErr) Error() string { return "line noise" }
func (commErr) Comm() bool    { return true }

type wrapped struct{ err error }

func (w wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }

func wrap(err error) error { return wrapped{err: err} }

func TestReqWords(t *testing.T) {
	// 显式宽度优先。
	if got := reqWords(driver.WriteRequest{
		Address:  model.Address{Length: 4},
		DataType: model.TypeUint16,
	}); got != 4 {
		t.Fatalf("explicit length = %d", got)
	}
	// 没有显式宽度时按数据类型推。
	if got := reqWords(driver.WriteRequest{DataType: model.TypeUint32}); got != 2 {
		t.Fatalf("uint32 = %d", got)
	}
	// 未知类型退化为 1 字，不能返回 0（那会让写请求被当成「没有宽度」）。
	if got := reqWords(driver.WriteRequest{}); got != 1 {
		t.Fatalf("zero = %d", got)
	}
}

func TestRegisterWidth(t *testing.T) {
	if registerWidth(model.TypeFloat32) != 2 {
		t.Fatalf("float32 = %d", registerWidth(model.TypeFloat32))
	}
	if registerWidth("nonsense") != 1 {
		t.Fatalf("unknown = %d", registerWidth("nonsense"))
	}
}

func TestDefaultTimeoutIsSane(t *testing.T) {
	// 9600bps 下最长一帧约 1.2 秒，加 PLC 响应等待 150ms；3 秒要能覆盖它，
	// 又不能长到让「波特率配错」看不出问题。
	if DefaultTimeout() < 2e9 || DefaultTimeout() > 10e9 {
		t.Fatalf("DefaultTimeout = %v", DefaultTimeout())
	}
}
