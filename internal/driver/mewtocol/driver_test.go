package mewtocol

import (
	"errors"
	"testing"

	"uic/internal/codec/mewtocol"
	"uic/internal/driver"
	"uic/internal/model"
)

// 纯函数辅助的单元测试：它们决定读多少字、按什么字节序解、错误归到哪个
// 质量位，出错时不会有任何 I/O 迹象，只能靠这些断言钉住。

func TestPointWords(t *testing.T) {
	cases := []struct {
		name string
		p    driver.PointRef
		want int
	}{
		{"bool is one word", driver.PointRef{DataType: model.TypeBool, Length: 1}, 1},
		{"bool ignores length", driver.PointRef{DataType: model.TypeBool, Length: 5}, 1},
		{"explicit length wins", driver.PointRef{DataType: model.TypeUint16, Length: 3}, 3},
		{"falls back to type width", driver.PointRef{DataType: model.TypeUint32, Length: 0}, 2},
		{"unknown type defaults to one", driver.PointRef{DataType: model.TypeUint16, Length: 0}, 1},
	}
	for _, c := range cases {
		if got := pointWords(c.p); got != c.want {
			t.Errorf("%s: pointWords = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestReqWords(t *testing.T) {
	cases := []struct {
		name string
		r    driver.WriteRequest
		want int
	}{
		{"explicit length", driver.WriteRequest{Address: model.Address{Length: 4}, DataType: model.TypeUint16}, 4},
		{"type width when no length", driver.WriteRequest{Address: model.Address{Length: 0}, DataType: model.TypeFloat64}, 4},
		{"default one", driver.WriteRequest{Address: model.Address{Length: 0}, DataType: model.TypeUint16}, 1},
	}
	for _, c := range cases {
		if got := reqWords(c.r); got != c.want {
			t.Errorf("%s: reqWords = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestOrderFor(t *testing.T) {
	// 未显式配置时按字节数选默认：>=8 字用 DCBA，否则 CDAB；显式配置覆盖。
	if got := orderFor(4, ""); got != model.OrderCDAB {
		t.Errorf("orderFor(4,\"\") = %v, want CDAB", got)
	}
	if got := orderFor(8, ""); got != model.OrderDCBA {
		t.Errorf("orderFor(8,\"\") = %v, want DCBA", got)
	}
	if got := orderFor(2, model.OrderABCD); got != model.OrderABCD {
		t.Errorf("explicit order should win: %v", got)
	}
}

// 错误分类：PLC 回 66H（地址错）必须落到地址类错误 + 地址质量位，
// 65H（写保护）落到权限类。分类错了，采集层会一遍遍重试一个永远写不进的点。
func TestClassify(t *testing.T) {
	cn := &conn{devID: "dev1"}
	cases := []struct {
		code    byte
		wantErr driver.ErrorKind
	}{
		{0x66, driver.ErrAddress},
		{0x65, driver.ErrPermission},
		{0x41, driver.ErrProtocol}, // 格式错：重发有意义
		{0x53, driver.ErrProtocol}, // busy：可重试
		{0x60, driver.ErrAddress},  // 参数错：区码/单元码不受支持，配置问题
	}
	for _, tc := range cases {
		ec := &mewtocol.ErrorCode{Code: tc.code}
		err := cn.classify("write", ec)
		if got := driver.KindOf(err); got != tc.wantErr {
			t.Errorf("%02XH: kind = %v, want %v", tc.code, got, tc.wantErr)
		}
	}
}

func TestQualityOf(t *testing.T) {
	if q := qualityOf(&mewtocol.ErrorCode{Code: 0x66}); q != model.QualityAddressError {
		t.Errorf("0x66 quality = %v, want address error", q)
	}
	if q := qualityOf(&mewtocol.ErrorCode{Code: 0x22}); q != model.QualityCommError {
		t.Errorf("0x22 quality = %v, want comm error", q)
	}
	if q := qualityOf(&mewtocol.ErrorCode{Code: 0x41}); q != model.QualityCommError {
		t.Errorf("0x41 quality = %v, want comm error (retryable format error)", q)
	}
	if q := qualityOf(driver.E(driver.ErrTimeout, "read", "d", "", true, nil)); q != model.QualityTimeout {
		t.Errorf("timeout quality = %v, want timeout", q)
	}
	// 普通协议错（非 ErrorCode）落到 Bad。
	if q := qualityOf(errors.New("generic")); q != model.QualityBad {
		t.Errorf("generic quality = %v, want bad", q)
	}
}

// 质量位必须与错误码自身的 Kind()/Retryable() 一致：手抄的链路码白名单曾
// 漏掉 0x29/2A/2B/2C/2D/2E/2F/31/34/35/37 共 11 个码，把可重试的链路故障
// 显示成 bad。这里遍历整段链路码与全部协议/处理码，逐码钉住派生关系。
func TestQualityOfDerivesFromErrorCode(t *testing.T) {
	// 链路类：0x22..0x38 整段都应是 comm_error（含被旧白名单漏掉的那些）。
	for code := 0x22; code <= 0x38; code++ {
		ec := &mewtocol.ErrorCode{Code: byte(code)}
		if got := qualityOf(ec); got != model.QualityCommError {
			t.Errorf("%02XH (kind=%v): quality = %v, want comm error", code, ec.Kind(), got)
		}
	}
	// 可重试的协议/处理类：同样是通信问题，得 comm_error。
	for _, code := range []byte{0x40, 0x41, 0x42, 0x43, 0x50, 0x51, 0x52, 0x53} {
		ec := &mewtocol.ErrorCode{Code: code}
		if !ec.Retryable() {
			t.Fatalf("%02XH 应是可重试的，测试前提有误", code)
		}
		if got := qualityOf(ec); got != model.QualityCommError {
			t.Errorf("%02XH (retryable=%v): quality = %v, want comm error", code, ec.Retryable(), got)
		}
	}
	// 地址类固定 address_error，不受派生规则影响。
	for _, code := range []byte{0x60, 0x61, 0x66, 0x67} {
		if got := qualityOf(&mewtocol.ErrorCode{Code: code}); got != model.QualityAddressError {
			t.Errorf("%02XH: quality = %v, want address error", code, got)
		}
	}
	// 不可重试的应用类：配置/模式问题，落 bad。
	for _, code := range []byte{0x62, 0x63, 0x64, 0x65, 0x68} {
		ec := &mewtocol.ErrorCode{Code: code}
		if ec.Retryable() {
			t.Fatalf("%02XH 不应是可重试的，测试前提有误", code)
		}
		if got := qualityOf(ec); got != model.QualityBad {
			t.Errorf("%02XH: quality = %v, want bad", code, got)
		}
	}
}
