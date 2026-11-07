package bacnet

import (
	"strings"
	"testing"
)

func TestObjectTypeFromName(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"analog-input", ObjectAnalogInput},
		{"Analog-Input", ObjectAnalogInput},
		{"analog_input", ObjectAnalogInput},
		{"  ANALOG INPUT ", ObjectAnalogInput},
		{"ai", ObjectAnalogInput},
		{"analog-value", ObjectAnalogValue},
		{"av", ObjectAnalogValue},
		{"binary-input", ObjectBinaryInput},
		{"bi", ObjectBinaryInput},
		{"multi-state-value", ObjectMultiStateValue},
		{"device", ObjectDevice},
		{"character-string", ObjectCharacterString},
		{"0", 0},
		{"8", ObjectDevice},
		{"1023", 1023},
	}
	for _, tc := range cases {
		got, ok := ObjectTypeFromName(tc.in)
		if !ok || got != tc.want {
			t.Fatalf("ObjectTypeFromName(%q) = %d/%v, want %d", tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "no-such", "1024", "-1"} {
		if _, ok := ObjectTypeFromName(bad); ok {
			t.Fatalf("ObjectTypeFromName(%q) should fail", bad)
		}
	}
}

func TestObjectTypeName(t *testing.T) {
	if got := ObjectTypeName(ObjectAnalogInput); got != "analog-input" {
		t.Fatalf("name = %q", got)
	}
	if got := ObjectTypeName(ObjectBinaryValue); got != "binary-value" {
		t.Fatalf("name = %q", got)
	}
	// 未知类型回退成编号：宁可打印 "63"，也不要打印一个错误的别名。
	if got := ObjectTypeName(63); got != "63" {
		t.Fatalf("unknown name = %q", got)
	}
	names := ObjectTypeNames()
	if len(names) < 10 {
		t.Fatalf("only %d names", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("names not sorted: %v", names)
		}
	}
}

func TestPropertyFromName(t *testing.T) {
	cases := map[string]int{
		"present-value":  PropPresentValue,
		"PresentValue":   PropPresentValue,
		"present_value":  PropPresentValue,
		"pv":             PropPresentValue,
		"object-name":    PropObjectName,
		"name":           PropObjectName,
		"units":          PropUnits,
		"priority-array": PropPriorityArray,
		"out-of-service": PropOutOfService,
		"oos":            PropOutOfService,
		"85":             PropPresentValue,
		"117":            PropUnits,
		"0":              0,
	}
	for in, want := range cases {
		got, ok := PropertyFromName(in)
		if !ok || got != want {
			t.Fatalf("PropertyFromName(%q) = %d/%v, want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "no-such-property", "4194304", "-1", "0x55"} {
		if _, ok := PropertyFromName(bad); ok {
			t.Fatalf("PropertyFromName(%q) should fail", bad)
		}
	}
}

func TestPropertyName(t *testing.T) {
	if got := PropertyName(PropPresentValue); got != "present-value" {
		t.Fatalf("name = %q", got)
	}
	if got := PropertyName(PropObjectName); got != "object-name" {
		t.Fatalf("name = %q", got)
	}
	if got := PropertyName(7); got != "7" {
		t.Fatalf("unknown name = %q", got)
	}
	names := PropertyNames()
	if len(names) < 10 {
		t.Fatalf("only %d names", len(names))
	}
}

func TestKindOfObject(t *testing.T) {
	cases := []struct {
		typ  int
		want ObjectKind
	}{
		{ObjectAnalogInput, KindAnalog},
		{ObjectAnalogOutput, KindAnalog},
		{ObjectAnalogValue, KindAnalog},
		{ObjectLoop, KindAnalog},
		{ObjectBinaryInput, KindBinary},
		{ObjectBinaryOutput, KindBinary},
		{ObjectBinaryValue, KindBinary},
		{ObjectMultiStateInput, KindMultiState},
		{ObjectMultiStateValue, KindMultiState},
		{ObjectMultiStateOutput, KindMultiState},
		{ObjectCharacterString, KindString},
		{ObjectDevice, KindUnknown},
		{ObjectSchedule, KindUnknown},
	}
	for _, tc := range cases {
		if got := KindOfObject(tc.typ); got != tc.want {
			t.Fatalf("KindOfObject(%d) = %d, want %d", tc.typ, got, tc.want)
		}
	}
}

func TestFormatAddress(t *testing.T) {
	cases := []struct {
		objType, instance, property, index int
		want                               string
	}{
		{ObjectAnalogInput, 1, PropPresentValue, 0, "analog-input:1.present-value"},
		{ObjectAnalogValue, 2, PropPriorityArray, 3, "analog-value:2.priority-array[3]"},
		{63, 7, 999, 0, "63:7.999"},
	}
	for _, tc := range cases {
		got := FormatAddress(tc.objType, tc.instance, tc.property, tc.index)
		if got != tc.want {
			t.Fatalf("FormatAddress = %q, want %q", got, tc.want)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Present-Value":   "presentvalue",
		"present_value":   "presentvalue",
		" present value ": "presentvalue",
		"Analog.Input":    "analoginput",
		"":                "",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Fatalf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAliasTablesAreSelfConsistent(t *testing.T) {
	// 每个别名都必须能被解析回它所属的编号：alias 表是反向生成的，
	// 但这条断言能在有人手写覆盖时立刻发现问题。
	for typ, aliases := range objectTypeNames {
		for _, a := range aliases {
			got, ok := ObjectTypeFromName(a)
			if !ok || got != typ {
				t.Fatalf("alias %q resolves to %d/%v, want %d", a, got, ok, typ)
			}
		}
	}
	for prop, aliases := range propertyNames {
		for _, a := range aliases {
			got, ok := PropertyFromName(a)
			if !ok || got != prop {
				t.Fatalf("property alias %q resolves to %d/%v, want %d", a, got, ok, prop)
			}
		}
	}
}

func TestErrorMessagesListAlternatives(t *testing.T) {
	// 错误提示里必须给出可选值，否则现场只能去翻文档。
	if !strings.Contains(strings.Join(ObjectTypeNames(), ","), "analog-input") {
		t.Fatal("object type list must contain analog-input")
	}
	if !strings.Contains(strings.Join(PropertyNames(), ","), "present-value") {
		t.Fatal("property list must contain present-value")
	}
}
