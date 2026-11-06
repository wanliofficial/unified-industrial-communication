package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"uic/internal/model"
)

// bitWriteHarness 在标准回环设备上追加两个布尔点：
//   - bit3w：保持寄存器 20 的第 3 位，可写（读-改-写）；
//   - bitnow：保持寄存器 21 的整字布尔，可写但没有位号——写入必须被明确拒绝。
func bitWriteHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(cfg map[string]any) {
		dev := cfg["devices"].([]any)[0].(map[string]any)
		pts := dev["points"].([]any)
		pts = append(pts,
			map[string]any{
				"id": "bit3w", "dataType": "bool", "writable": true, "enabled": true,
				"address": map[string]any{"kind": "register", "area": "holding", "offset": 20, "length": 1, "bit": 3},
			},
			map[string]any{
				"id": "bitnow", "dataType": "bool", "writable": true, "enabled": true,
				"address": map[string]any{"kind": "register", "area": "holding", "offset": 21, "length": 1, "bit": -1},
			},
		)
		dev["points"] = pts
	})
}

// TestWriteRegisterBitEndToEnd 验证「寄存器内位写」的整条链路：
// HTTP → 校验 → 调度 → 驱动读-改-写 → 回读确认 → 缓存。
func TestWriteRegisterBitEndToEnd(t *testing.T) {
	h := bitWriteHarness(t)

	// 先让设备完成一轮采集，避免写入撞上首次连接。
	h.waitPointGood("sim01", "reg0")

	// 高位与相邻位都置上，用来证明位写不会覆盖其它位。
	h.sim.SetHolding(20, 0xF008)

	write := func(v bool) []httpapiWriteResult {
		t.Helper()
		body := `{"writes":[{"point":"bit3w","value":{"type":"bool","bool":` + boolLit(v) + `},"confirm":true}]}`
		code, resp := h.do(http.MethodPost, "/api/v1/points/sim01/write", body)
		if code != 200 {
			t.Fatalf("write = %d %s", code, resp)
		}
		var out struct {
			Results []httpapiWriteResult `json:"results"`
		}
		if err := json.Unmarshal(resp, &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, resp)
		}
		if len(out.Results) != 1 {
			t.Fatalf("results = %+v", out.Results)
		}
		return out.Results
	}

	res := write(false)
	if !res[0].OK || res[0].Quality != string(model.QualityGood) {
		t.Fatalf("clear bit3 = %+v", res[0])
	}
	if got := h.sim.Holding(20); got != 0xF000 {
		t.Fatalf("after clear: holding[20] = %#04x, want 0xF000", got)
	}

	res = write(true)
	if !res[0].OK {
		t.Fatalf("set bit3 = %+v", res[0])
	}
	if got := h.sim.Holding(20); got != 0xF008 {
		t.Fatalf("after set: holding[20] = %#04x, want 0xF008", got)
	}

	// 确认后的回读必须更新缓存：点位值应立刻变成 false/true。
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, ok := h.rt.Point("sim01", "bit3w")
		if ok && s.Quality == model.QualityGood && s.Value.Bool {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bit3w cache never converged: %+v ok=%v", s, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := write(false); !got[0].OK {
		t.Fatalf("second clear = %+v", got[0])
	}
}

// TestWriteRegisterBoolWithoutBitFails 验证没有位号的寄存器布尔写入被明确
// 拒绝，而不是把整字覆盖成 0/1。
func TestWriteRegisterBoolWithoutBitFails(t *testing.T) {
	h := bitWriteHarness(t)
	h.waitPointGood("sim01", "reg0")
	h.sim.SetHolding(21, 0xFF00)

	code, body := h.do(http.MethodPost, "/api/v1/points/sim01/write",
		`{"writes":[{"point":"bitnow","value":{"type":"bool","bool":true}}]}`)
	if code != 200 {
		t.Fatalf("write = %d %s", code, body)
	}
	var out struct {
		Results []httpapiWriteResult `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].OK {
		t.Fatalf("results = %+v", out.Results)
	}
	if !strings.Contains(out.Results[0].Message, "address.bit") {
		t.Fatalf("message should explain the missing bit: %q", out.Results[0].Message)
	}
	if got := h.sim.Holding(21); got != 0xFF00 {
		t.Fatalf("holding[21] changed to %#04x", got)
	}
}

// httpapiWriteResult 是北向写入结果的子集，只取断言需要的字段。
type httpapiWriteResult struct {
	Point   string `json:"point"`
	OK      bool   `json:"ok"`
	Quality string `json:"quality"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func boolLit(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
