package computed

import (
	"strings"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

// 64 位整数边界必须判坏，而不是给出一个被裁剪的错误数字。
//
// MaxInt64/MaxUint64 无法精确表示为 float64（会进位到 2^63/2^64）。若用
// 「> float64(MaxInt64)」做上界，恰为 2^63 的输入会通过检查，随后被下游
// codec/value 与 sparkplug 的 int64(v.Num)/uint64(v.Num) 静默裁剪——int64(2^63)
// 在 amd64 上变成 MinInt64，于是「超范围的合计」被当成一个看起来正常的负数
// 写进寄存器/上报出去。这正是 encode 注释里要避免的那类假数字。
func TestInt64BoundaryIsBadNotWrapped(t *testing.T) {
	dev := model.Device{ID: "calc", Points: []model.Point{
		point("over", "9223372036854775808", model.TypeInt64),    // 2^63：int64 存不下
		point("uover", "18446744073709551616", model.TypeUint64), // 2^64：uint64 存不下
		point("min", "-9223372036854775808", model.TypeInt64),    // MinInt64 合法
	}}
	got := read(t, open(t, dev, nil), "over", "uover", "min")

	for _, id := range []string{"over", "uover"} {
		s := got[id]
		if s.Quality != model.QualityBad {
			t.Fatalf("%s 越界应判 bad，得到 %s（会掩盖成假数字）", id, s.Quality)
		}
		if s.Err == nil || !strings.Contains(s.Err.Error(), "out of range") {
			t.Fatalf("%s 应携带 out of range 错误，得到 %v", id, s.Err)
		}
	}
	// 合法下界不能被误伤：MinInt64 恰好等于 -2^63，仍在范围内。
	if s := got["min"]; s.Quality != model.QualityGood {
		t.Fatalf("MinInt64 应判 good，得到 %s / %v", s.Quality, s.Err)
	}
}

// 复核 errOf 的错误分类：计算点求值失败（除零/越界/无输入）归 bad，其携带的错误
// 应分类为 ErrProtocol（协议/数据语义层），而不是 ErrInternal。
//
// ErrInternal 在本仓的语义是「实现 bug / 不可恢复的内部错误」，CountsTowardBreaker
// 为 true。计算点没有物理链路，运行期一个 bad 结果永远来自「表达式或输入」——这是
// 数据语义层失败，与 ErrProtocol 同类，绝不该被标成内部错误。把配置/数据层面的
// 失败标成 ErrInternal 会污染排障分类（运维看到「内部错误」会去查进程，而不是查
// 表达式/点表），也违背「结束码必须自带 Kind、不得兜底成 ErrInternal」的纪律。
func TestEvalFailureIsProtocolNotInternal(t *testing.T) {
	// 除零：求值本身失败 → QualityBad。
	dev := model.Device{ID: "kpi", Points: []model.Point{
		point("v", "1 / 0", model.TypeFloat64),
	}}
	got := read(t, open(t, dev, nil), "v")["v"]
	if got.Quality != model.QualityBad {
		t.Fatalf("除零应判 bad，得到 %s", got.Quality)
	}
	if got.Err == nil {
		t.Fatal("除零应携带错误")
	}
	if k := driver.KindOf(got.Err); k != driver.ErrProtocol {
		t.Fatalf("除零的错误分类 = %s，应为 protocol（表达式/输入失败不是内部错误）", k)
	}
}
