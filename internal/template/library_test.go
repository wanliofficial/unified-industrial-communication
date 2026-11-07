package template

import (
	"path/filepath"
	"testing"
	"time"

	"uic/internal/model"
)

// 模板库测试：examples/templates 下的每一份模板都必须能实例化出通过完整
// 校验的设备。模板是「交付资产」，不是示例代码——它坏了不会有人编译报错，
// 只会在现场第一次使用时报一串难以定位的错误。
func TestTemplateLibrary(t *testing.T) {
	paths, err := filepath.Glob("../../examples/templates/*.json")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) < 10 {
		t.Fatalf("template library has %d templates, want >= 10", len(paths))
	}
	seen := map[string]string{}
	for _, path := range paths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			def, err := Load(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if prev, dup := seen[def.ID]; dup {
				t.Fatalf("duplicate template id %q in %s and %s", def.ID, prev, path)
			}
			seen[def.ID] = path

			params := map[string]string{}
			for _, p := range def.Parameters {
				if !p.Required {
					continue
				}
				switch p.Type {
				case ParamString:
					params[p.Name] = "01"
				case ParamInt:
					params[p.Name] = "1"
				case ParamFloat:
					params[p.Name] = "1.5"
				case ParamBool:
					params[p.Name] = "true"
				}
			}
			dev, err := def.Instantiate(params)
			if err != nil {
				t.Fatalf("instantiate: %v", err)
			}
			dev.ChannelID = "ch-test"
			if dev.Interval == 0 {
				dev.Interval = model.Duration(time.Second)
			}
			if err := dev.Validate(); err != nil {
				t.Fatalf("instantiated device is invalid: %v", err)
			}
			if dev.TemplateID != def.ID {
				t.Fatalf("templateId = %q, want %q", dev.TemplateID, def.ID)
			}
			if len(dev.Points) == 0 {
				t.Fatal("template produced no points")
			}
			// 每个点位都必须带完整地址：模板漏字段在 JSON 层面是合法的，
			// 到驱动里才会变成 address_error。
			for _, p := range dev.Points {
				if p.Address.Kind == "" {
					t.Fatalf("point %q has no address kind", p.ID)
				}
				if p.DataType == "" {
					t.Fatalf("point %q has no dataType", p.ID)
				}
			}
		})
	}
}

// 模板里的参数名与占位符必须逐字符一致：`{unitId}` 不能写成 `{unitid}`。
func TestTemplateLibraryPlaceholdersDeclared(t *testing.T) {
	paths, err := filepath.Glob("../../examples/templates/*.json")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range paths {
		def, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, err := def.Placeholders(); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}
