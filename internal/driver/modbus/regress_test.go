package modbus

import (
	"context"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

// 异常码分类的回归。
//
// Modbus 把从站异常分成两类语义：「请求本身有问题」（01 非法功能 / 02 非法
// 地址 / 03 非法值 / 08 存储奇偶错）不可重试，改了也没用；「从站临时不可用」
// （04 设备故障 / 05 Acknowledge 慢操作 / 06 设备忙）可重试，应当让上层
// 稍后重试而不是把点位永久钉死为坏值。
//
// 历史缺陷：05（Acknowledge）漏在 default 分支被归成不可重试，与 06（Busy）
// 不对称——一次「延时命令」式的慢响应会让该点永久判坏。
func TestExceptionCodeRetryability(t *testing.T) {
	cases := []struct {
		code      byte
		wantKind  driver.ErrorKind
		wantRetry bool
	}{
		{0x01, driver.ErrProtocol, false}, // illegal function
		{0x02, driver.ErrAddress, false},  // illegal data address
		{0x03, driver.ErrProtocol, false}, // illegal data value
		{0x04, driver.ErrProtocol, true},  // server device failure
		{0x05, driver.ErrProtocol, true},  // acknowledge（稍后轮询）
		{0x06, driver.ErrProtocol, true},  // server device busy
		{0x08, driver.ErrProtocol, false}, // memory parity error
		{0x0B, driver.ErrProtocol, false}, // gateway target failed to respond（默认分支）
	}
	for _, tc := range cases {
		c := openScripted(t, func(req []byte) ([]byte, error) {
			return adu(req, []byte{req[7] | 0x80, tc.code}), nil
		})
		_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
			regPoint("p", "holding", 0, model.TypeUint16, 1),
		}}})
		if err == nil {
			t.Fatalf("exception 0x%02x: expected error", tc.code)
		}
		if got := driver.KindOf(err); got != tc.wantKind {
			t.Errorf("exception 0x%02x kind = %s, want %s (err=%v)", tc.code, got, tc.wantKind, err)
		}
		if got := driver.IsRetryable(err); got != tc.wantRetry {
			t.Errorf("exception 0x%02x retryable = %v, want %v (err=%v)", tc.code, got, tc.wantRetry, err)
		}
	}
}
