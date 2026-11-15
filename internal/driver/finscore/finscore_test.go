package finscore

import (
	"errors"
	"testing"

	fins "uic/internal/codec/fins"
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

// *TCPError（FINS/TCP 封装层错误码）必须让质量码是 comm_error。
//
// 这条用例钉的是「内层错误要解包」这一步：封装层错误码（节点被占用、连接数
// 满）本身没有 driver.Kind，只有 Comm() == true。QualityOf 若只看外层
// OpError.Kind（这里是 disconnected，会落到兜底 bad 之前的 switch——
// disconnected 其实会落 comm_error，所以真正的失败形态是**内层是结束码或
// / 其它无 Kind 的错误时被误判**），就必须解包到内层才能给出 comm_error。
//
// 早先的版本里 *TCPError 根本没有 Comm() 方法，`errors.As(err, &CommError)`
// 匹配不上，质量一路落到 bad；这条用例就是为那次回归加的。
func TestTCPErrorQualityIsCommError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want model.Quality
	}{
		{"tcp transport error", &fins.TCPError{Code: 0x21}, model.QualityCommError},
		{"tcp error wrapped in OpError", driver.E(driver.ErrDisconnected, "read", "",
			"FINS/TCP transport error", true, &fins.TCPError{Code: 0x21}), model.QualityCommError},
		{"fins end code address", &fins.EndCodeError{Code: 0x1103}, model.QualityAddressError},
		{"fins end code link", &fins.EndCodeError{Code: 0x0202}, model.QualityCommError},
		{"fins end code other", &fins.EndCodeError{Code: 0x0001}, model.QualityBad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QualityOf(tc.err); got != tc.want {
				t.Fatalf("QualityOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// endCodeError 是 finscore 自己造的结束码错误（点表地址不合法等），必须带
// 分类：调用方拿到的是 driver.OpError，外层 Kind 是 address/protocol，内层
// 是结束码。QualityOf 必须能透过 OpError 看到内层分类，否则点表里的地址错
// 在 TCP 上显示 bad、在串口上显示 bad，与信封层直接返回结束码的路径不一致。
func TestEndCodeErrorKeepsClassificationThroughOpError(t *testing.T) {
	cases := []struct {
		code    uint16
		kind    driver.ErrorKind
		retry   bool
		quality model.Quality
	}{
		{0x1101, driver.ErrAddress, false, model.QualityAddressError},
		{0x1103, driver.ErrAddress, false, model.QualityAddressError},
		{0x0202, driver.ErrProtocol, true, model.QualityCommError},
		{0x0301, driver.ErrProtocol, true, model.QualityCommError},
		{0x0018, driver.ErrProtocol, false, model.QualityBad},
	}
	for _, tc := range cases {
		err := endCodeError(tc.code, "test")
		if got := driver.KindOf(err); got != tc.kind {
			t.Errorf("结束码 %04X：kind = %q，要 %q（%v）", tc.code, got, tc.kind, err)
		}
		if got := driver.IsRetryable(err); got != tc.retry {
			t.Errorf("结束码 %04X：retryable = %v，要 %v（%v）", tc.code, got, tc.retry, err)
		}
		if got := QualityOf(err); got != tc.quality {
			t.Errorf("结束码 %04X：质量 = %q，要 %q（%v）", tc.code, got, tc.quality, err)
		}
	}
}

// TestQualityOf 是既有的直接映射表，保留原样：它不覆盖「结束码藏在 OpError
// 里」的情况，那部分由上面两条覆盖。
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
