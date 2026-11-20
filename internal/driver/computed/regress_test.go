package computed

import (
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

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
