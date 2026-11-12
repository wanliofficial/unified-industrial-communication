package main

import (
	"testing"

	mtcodec "uic/internal/codec/mtconnect"
	"uic/internal/model"
)

// 与其它 *read 的自检用例一致：这里只测「命令行输入 → 平台值 / 过滤」这类
// 纯函数。网络与解析由 driver/mtconnect 和 sim/mtconnect 的用例覆盖，不在
// CLI 层再造一个客户端。

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" Xabs , exec,,toolOffset ")
	want := []string{"Xabs", "exec", "toolOffset"} // 排序后：注意大小写 ASCII
	if len(got) != len(want) {
		t.Fatalf("splitCSV = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitCSV = %v, want %v", got, want)
		}
	}
	if len(splitCSV("  ")) != 0 {
		t.Fatalf("blank input should yield no ids")
	}
}

func TestMTRowsFromIndex(t *testing.T) {
	items := []mtcodec.Item{
		{DataItemID: "Xabs", Category: mtcodec.CategorySample, Text: "12.5", Sequence: 1},
		{DataItemID: "exec", Category: mtcodec.CategoryEvent, Text: "ACTIVE", Sequence: 2},
		{DataItemID: "toolOffset", Category: mtcodec.CategorySample, Text: mtcodec.Unavailable, Sequence: 3},
	}
	idx := mtcodec.BuildIndex(items)

	// 无过滤：全部返回。
	rows := mtRowsFromIndex(idx, nil)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	// 有过滤：只保留指定 ID。
	rows = mtRowsFromIndex(idx, []string{"Xabs", "toolOffset"})
	if len(rows) != 2 {
		t.Fatalf("filtered rows = %d, want 2", len(rows))
	}
	m := map[string]mtRow{}
	for _, r := range rows {
		m[r.ID] = r
	}
	if m["Xabs"].Value != "12.5" || m["Xabs"].Category != "SAMPLE" {
		t.Fatalf("Xabs row = %+v", m["Xabs"])
	}
	// UNAVAILABLE 的点质量应为 bad。
	if m["toolOffset"].Quality != string(model.QualityBad) {
		t.Fatalf("toolOffset quality = %q, want bad", m["toolOffset"].Quality)
	}
}
