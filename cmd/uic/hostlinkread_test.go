package main

import (
	"strings"
	"testing"

	"uic/internal/model"
)

// 自检命令的写路径是「现场第一次就用到」的一层，而且它最危险的错法不是报错，
// 是**悄悄改写用户没点名的位/字**。这里的用例都钉在这一点上。

func TestHostLinkWriteRequestBitIgnoresAreaCapability(t *testing.T) {
	// EM 当前区没有原生位访问码（codec/fins 里 em 只有字码 0x98）。早先 CLI
	// 用 `bit >= 0 && a.BitCapable()` 判断，于是 `-area em -bit 3` 掉进字写
	// 分支：`-write on` 直接报「非法数字」，`-write 1` 则发整字写把其余 15
	// 位清零。位写与否只看用户给没给 -bit。
	for _, area := range []string{"em", "em3", "cio", "dm"} {
		req, err := hostLinkWriteRequest(area, 5, 3, 1, "on")
		if err != nil {
			t.Fatalf("area %s: %v", area, err)
		}
		if req.DataType != model.TypeBool {
			t.Fatalf("area %s: DataType = %s，给了 -bit 就必须按位写", area, req.DataType)
		}
		if req.Address.Bit != 3 || req.Address.Offset != 5 {
			t.Fatalf("area %s: address = %+v", area, req.Address)
		}
		if req.Address.Length != 1 {
			t.Fatalf("area %s: Length = %d，位写的宽度恒为 1", area, req.Address.Length)
		}
		if !req.Value.Bool {
			t.Fatalf("area %s: value = %+v，on 应解成 true", area, req.Value)
		}
	}
}

func TestHostLinkWriteRequestRejectsQtyOnWordWrite(t *testing.T) {
	// 字写带 -qty 4 会把 dm10..dm13 一起写：多出的字补零，且 8 字节宽度触发
	// DCBA 字节序，连 dm10 本身都写成 0x0500 而不是 5。
	for _, qty := range []int{0, 2, 4, -1} {
		_, err := hostLinkWriteRequest("dm", 10, -1, qty, "5")
		if err == nil {
			t.Fatalf("qty %d 必须被拒（字写一次只写一个字）", qty)
		}
		if !strings.Contains(err.Error(), "qty") {
			t.Fatalf("qty %d: 错误信息应提到 -qty，实际 %v", qty, err)
		}
	}
}

func TestHostLinkWriteRequestWordWrite(t *testing.T) {
	req, err := hostLinkWriteRequest("dm", 10, -1, 1, "0xBEEF")
	if err != nil {
		t.Fatalf("write request: %v", err)
	}
	if req.DataType != model.TypeUint16 {
		t.Fatalf("DataType = %s", req.DataType)
	}
	if req.Address.Length != 1 || req.Address.Bit != -1 {
		t.Fatalf("address = %+v，字写的长度必须是 1、位号必须是 -1", req.Address)
	}
	if req.Value.Num != 0xBEEF {
		t.Fatalf("value = %v", req.Value)
	}
}

func TestHostLinkWriteRequestRejectsBadValue(t *testing.T) {
	if _, err := hostLinkWriteRequest("dm", 0, -1, 1, "abc"); err == nil {
		t.Fatal("非数字字写值必须被拒")
	}
	if _, err := hostLinkWriteRequest("cio", 0, 3, 1, "maybe"); err == nil {
		t.Fatal("非法布尔量必须被拒")
	}
}
