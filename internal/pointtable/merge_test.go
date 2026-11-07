package pointtable

import (
	"strings"
	"testing"

	"uic/internal/model"
)

func point(id string, dt model.DataType) model.Point {
	return model.Point{
		ID:       id,
		Address:  model.Address{Kind: model.AddressRegister, Area: "holding", Offset: 0, Length: 1, Bit: -1},
		DataType: dt,
		Enabled:  true,
	}
}

func withOffset(p model.Point, offset int) model.Point {
	p.Address.Offset = offset
	return p
}

func TestMergeNewAndUnchanged(t *testing.T) {
	existing := []model.Point{point("a", model.TypeUint16)}
	incoming := []model.Point{
		point("a", model.TypeUint16),
		withOffset(point("b", model.TypeUint16), 4),
	}
	merged, actions, err := Merge(existing, incoming, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged = %d", len(merged))
	}
	if merged[0].ID != "a" || merged[1].ID != "b" {
		t.Fatalf("merged = %+v", merged)
	}
	// 原切片不能被改动：调用方可能还要拿它做对比或回滚。
	if len(existing) != 1 {
		t.Fatalf("existing mutated: %d", len(existing))
	}
	if actions[0].Kind != ActionUnchanged || actions[1].Kind != ActionNew {
		t.Fatalf("actions = %+v", actions)
	}
	counts := Count(actions)
	if counts[ActionNew] != 1 || counts[ActionUnchanged] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestMergeReplace(t *testing.T) {
	existing := []model.Point{point("a", model.TypeUint16)}
	incoming := []model.Point{withOffset(point("a", model.TypeUint16), 9)}

	merged, actions, err := Merge(existing, incoming, true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged[0].Address.Offset != 9 {
		t.Fatalf("offset = %d", merged[0].Address.Offset)
	}
	if actions[0].Kind != ActionReplaced {
		t.Fatalf("action = %+v", actions[0])
	}
	// 差异描述要指出具体哪一项变了，否则现场无法判断覆盖是否安全。
	if !strings.Contains(actions[0].Reason, "address") {
		t.Fatalf("reason = %q", actions[0].Reason)
	}
	// 输入切片同样不能被改写。
	if existing[0].Address.Offset != 0 {
		t.Fatalf("existing mutated: %+v", existing[0].Address)
	}
}

func TestMergeConflictWithoutReplace(t *testing.T) {
	existing := []model.Point{point("a", model.TypeUint16)}
	incoming := []model.Point{withOffset(point("a", model.TypeUint16), 9)}

	_, actions, err := Merge(existing, incoming, false)
	if err == nil {
		t.Fatal("conflict must fail without replace")
	}
	if !strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), "-replace") {
		t.Fatalf("error = %v", err)
	}
	if actions[0].Kind != ActionConflict {
		t.Fatalf("action = %+v", actions[0])
	}
}

func TestMergeRejectsDuplicateIncoming(t *testing.T) {
	incoming := []model.Point{
		point("a", model.TypeUint16),
		withOffset(point("a", model.TypeUint16), 2),
	}
	if _, _, err := Merge(nil, incoming, false); err == nil {
		t.Fatal("duplicate incoming must fail")
	}
}

func TestDescribeDiffCoversFields(t *testing.T) {
	old := point("a", model.TypeUint16)
	old.Unit = "V"
	old.Scale = 1
	old.Name = "old"
	newer := old
	newer.Unit = "A"
	newer.Scale = 2
	newer.Name = "new"
	newer.DataType = model.TypeFloat32
	newer.ByteOrder = model.OrderCDAB
	newer.Writable = true
	newer.Enabled = false

	diff := describeDiff(old, newer)
	for _, want := range []string{"dataType", "byteOrder", "scale", "unit", "writable", "enabled", "name"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff = %q, missing %q", diff, want)
		}
	}
}

func TestDescribeDiffFallback(t *testing.T) {
	old := point("a", model.TypeUint16)
	newer := old
	newer.Description = "different"
	if got := describeDiff(old, newer); !strings.Contains(got, "configuration differs") {
		t.Fatalf("diff = %q", got)
	}
}
