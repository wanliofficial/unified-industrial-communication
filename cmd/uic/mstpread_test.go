package main

import (
	"testing"

	bacnetcodec "uic/internal/codec/bacnet"
	"uic/internal/model"
)

// 与其它 *read 自检用例一致：CLI 层只测「命令行输入 → 平台地址/类型」这种
// 纯函数。真正的报文往返由 driver/bacnetmstp 与 sim/bacnetmstp 覆盖，不在
// CLI 层再造一个客户端。

func TestResolveMSTPPoint(t *testing.T) {
	cases := []struct {
		name      string
		objType   string
		instance  int
		property  string
		index     int
		dtype     string
		wantKind  model.AddressKind
		wantType  model.DataType
		wantLabel string
	}{
		{
			name: "analog present-value", objType: "analog-input", instance: 1,
			property: "present-value", wantKind: model.AddressObject,
			wantType: model.TypeFloat32, wantLabel: "analog-input:1.present-value",
		},
		{
			name: "object-name is text", objType: "analog-input", instance: 1,
			property: "object-name", wantKind: model.AddressObject,
			wantType: model.TypeString, wantLabel: "analog-input:1.object-name",
		},
		{
			name: "binary present-value is bool", objType: "binary-input", instance: 1,
			property: "present-value", wantKind: model.AddressObject,
			wantType: model.TypeBool, wantLabel: "binary-input:1.present-value",
		},
		{
			name: "multi-state present-value is uint32", objType: "multi-state-value", instance: 5,
			property: "present-value", wantKind: model.AddressObject,
			wantType: model.TypeUint32, wantLabel: "multi-state-value:5.present-value",
		},
		{
			name: "device falls back to float32", objType: "device", instance: 1,
			property: "units", wantKind: model.AddressObject,
			wantType: model.TypeFloat32, wantLabel: "device:1.units",
		},
		{
			name: "numeric object type", objType: "0", instance: 2,
			property: "85", wantKind: model.AddressObject,
			wantType: model.TypeFloat32, wantLabel: "analog-input:2.present-value",
		},
		{
			name: "array index", objType: "analog-value", instance: 2,
			property: "priority-array", index: 3, wantKind: model.AddressObject,
			wantType: model.TypeFloat32, wantLabel: "analog-value:2.priority-array[3]",
		},
		{
			name: "dtype override", objType: "analog-input", instance: 1,
			property: "present-value", dtype: "float64", wantKind: model.AddressObject,
			wantType: model.TypeFloat64, wantLabel: "analog-input:1.present-value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, label, dt, err := resolveMSTPPoint(tc.objType, tc.instance, tc.property, tc.index, tc.dtype)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if addr.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", addr.Kind, tc.wantKind)
			}
			if addr.Instance != tc.instance || addr.PropIndex != tc.index {
				t.Fatalf("instance/index = %d/%d, want %d/%d",
					addr.Instance, addr.PropIndex, tc.instance, tc.index)
			}
			if dt != tc.wantType {
				t.Fatalf("dataType = %s, want %s", dt, tc.wantType)
			}
			if label != tc.wantLabel {
				t.Fatalf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

func TestResolveMSTPPointRejects(t *testing.T) {
	cases := []struct {
		name     string
		objType  string
		property string
		dtype    string
	}{
		{"unknown object type", "no-such-type", "present-value", ""},
		{"unknown property", "analog-input", "no-such-prop", ""},
		{"unknown dtype", "analog-input", "present-value", "complex128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := resolveMSTPPoint(tc.objType, 1, tc.property, 0, tc.dtype); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// 数值写法（对象类型编号 + 属性编号）解析出的对象类型应保持用户输入的原文，
// 标签则用规范名——两者不冲突：地址用于写配置，标签用于人读。这里锁住
// 「编号写法也能被接受」这一点，避免以后收紧 parseObjectAddress 时回归。
func TestResolveMSTPPointNumericKeepsRawType(t *testing.T) {
	addr, label, _, err := resolveMSTPPoint("0", 1, "85", 0, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr.ObjectType != "0" || addr.Property != "85" {
		t.Fatalf("address kept = %q/%q, want raw 0/85", addr.ObjectType, addr.Property)
	}
	if label != bacnetcodec.FormatAddress(bacnetcodec.ObjectAnalogInput, 1, bacnetcodec.PropPresentValue, 0) {
		t.Fatalf("label = %q", label)
	}
}
