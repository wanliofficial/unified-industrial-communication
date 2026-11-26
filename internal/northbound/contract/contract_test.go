package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"uic/internal/model"
)

func sampleFixture() model.Sample {
	ts := time.Date(2026, 11, 26, 8, 30, 0, 0, time.UTC)
	return model.Sample{
		DeviceID:    "sim01",
		PointID:     "reg0",
		Value:       model.Value{Num: 1234},
		Quality:     model.QualityGood,
		DeviceTime:  &ts,
		CollectedAt: ts,
		Seq:         7,
		Unit:        "kWh",
	}
}

func TestEncodeV1IsFlatWithSchemaVersion(t *testing.T) {
	b, err := EncodeV1(sampleFixture())
	if err != nil {
		t.Fatalf("EncodeV1: %v", err)
	}
	// 必须与改造前 publish 的字节形态一致：顶层 schemaVersion，Sample 字段平铺。
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("not json: %v (%s)", err, b)
	}
	if got["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v, want 1", got["schemaVersion"])
	}
	if got["device"] != "sim01" || got["point"] != "reg0" {
		t.Fatalf("flat device/point missing: %s", b)
	}
	if _, ok := got["sample"]; ok {
		t.Fatalf("v1 must not nest under sample: %s", b)
	}
}

func TestEncodeV2NestsSampleWithContractVersion(t *testing.T) {
	b, err := EncodeV2(sampleFixture())
	if err != nil {
		t.Fatalf("EncodeV2: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("not json: %v (%s)", err, b)
	}
	if got["contractVersion"] != float64(2) {
		t.Fatalf("contractVersion = %v, want 2", got["contractVersion"])
	}
	sample, ok := got["sample"].(map[string]any)
	if !ok {
		t.Fatalf("sample not an object: %s", b)
	}
	// v2 用驼峰外部字段名，与 v1 的扁平 snake_case 区分开。
	if sample["deviceId"] != "sim01" || sample["pointId"] != "reg0" {
		t.Fatalf("sample.deviceId/pointId missing: %s", b)
	}
	if sample["unit"] != "kWh" {
		t.Fatalf("sample.unit missing: %s", b)
	}
	// 不应出现 v1 的顶层 device/point。
	if _, ok := got["device"]; ok {
		t.Fatalf("v2 must not use top-level device: %s", b)
	}
}

// v1 与 v2 必须是两份不同的字节：这是「双发过渡期」有意义的前提——若两版
// 完全一样，消费方就无法区分，升版本也就失去了意义。
func TestV1AndV2Differ(t *testing.T) {
	s := sampleFixture()
	v1, err := EncodeV1(s)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := EncodeV2(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(v1) == string(v2) {
		t.Fatalf("v1 and v2 payloads are identical: %s", v1)
	}
}

func TestEncodeV2OmitsEmptyErrorAndDeviceTime(t *testing.T) {
	s := sampleFixture()
	s.Error = ""
	s.DeviceTime = nil
	b, err := EncodeV2(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "deviceTime") {
		t.Fatalf("deviceTime should be omitted when nil: %s", b)
	}
}

func TestIsV2Lane(t *testing.T) {
	cases := map[string]bool{
		"mqtt":       false,
		"mqtt#v2":    true,
		"webhook#v2": true,
		"journal":    false,
		"journal#v2": true,
		"#v2":        true,
		"mqtt#v2#v2": true, // 追加一次后缀仍是 v2 lane（不会有人这么命名）
		"mqt":        false,
		"mqtt#v22":   false,
		"v2mqtt":     false,
	}
	for name, want := range cases {
		if got := IsV2Lane(name); got != want {
			t.Fatalf("IsV2Lane(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBaseLane(t *testing.T) {
	if got := BaseLane("mqtt#v2"); got != "mqtt" {
		t.Fatalf("BaseLane(mqtt#v2) = %q", got)
	}
	if got := BaseLane("webhook#v2"); got != "webhook" {
		t.Fatalf("BaseLane(webhook#v2) = %q", got)
	}
	if got := BaseLane("mqtt"); got != "mqtt" {
		t.Fatalf("BaseLane(mqtt) = %q", got)
	}
}

func TestBatch(t *testing.T) {
	items := []model.Sample{sampleFixture(), sampleFixture()}

	v1, err := Batch(Version1, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != 2 {
		t.Fatalf("len = %d", len(v1))
	}
	for _, b := range v1 {
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["schemaVersion"] != float64(1) {
			t.Fatalf("batch v1 item missing schemaVersion: %s", b)
		}
	}

	v2, err := Batch(Version2, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(v2) != 2 {
		t.Fatalf("len = %d", len(v2))
	}
	for _, b := range v2 {
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["contractVersion"] != float64(2) {
			t.Fatalf("batch v2 item missing contractVersion: %s", b)
		}
	}

	// 未知版本回落到 v1（v1 是兜底默认）。
	fallback, err := Batch(99, items)
	if err != nil {
		t.Fatal(err)
	}
	if string(fallback[0]) != string(v1[0]) {
		t.Fatalf("unknown version should fall back to v1")
	}

	if _, err := Batch(Version1, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

// Batch 的 v2 item 必须与单独 EncodeV2 一致：Batch 只是循环封装，不能悄悄
// 改变单条的字节形态。
func TestBatchMatchesSingleEncode(t *testing.T) {
	s := sampleFixture()
	singles, err := Batch(Version2, []model.Sample{s})
	if err != nil {
		t.Fatal(err)
	}
	one, err := EncodeV2(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(singles[0]) != string(one) {
		t.Fatalf("Batch(Version2) != EncodeV2:\n%s\n%s", singles[0], one)
	}
}
