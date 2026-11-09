package dnp3

import (
	"fmt"
	"testing"

	dnp3codec "uic/internal/codec/dnp3"
)

// 从站编码 -> 主站解析 的自洽测试。它不替代与真实设备抓包的对照，但能
// 在「编码少一个字节 / 解析多读一个字节」时立刻失败，而不是等到集成
// 测试里表现成「某个值莫名其妙」。
func TestStaticRoundTrip(t *testing.T) {
	s, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetAutoChange(false)

	s.mu.Lock()
	raw, _ := s.buildRead([]dnp3codec.Header{{Group: dnp3codec.GroupClass, Variation: 1, Qualifier: dnp3codec.QualAllObjects, All: true}})
	s.mu.Unlock()

	objs, err := dnp3codec.ParseObjects(raw)
	if err != nil {
		t.Fatalf("parse own encoding: %v\nraw = % X", err, raw)
	}

	got := map[string]any{}
	for _, o := range objs {
		got[fmt.Sprintf("%d/%d", o.Group, o.Index)] = o.Value
	}
	want := map[string]any{
		"1/0":  true,
		"1/1":  false,
		"1/2":  true,
		"1/3":  false,
		"3/0":  uint64(1),
		"3/1":  uint64(2),
		"10/0": false,
		"10/1": false,
		"20/0": uint64(1000),
		"20/1": uint64(42),
		"30/0": 42.5,
		"30/1": 7.25,
		"30/2": -3.5,
		"30/3": 1234.0,
		"40/0": 0.0,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("missing object %s", k)
		} else if g != v {
			t.Errorf("object %s = %v, want %v", k, g, v)
		}
	}
	if t.Failed() {
		t.Logf("raw = % X", raw)
	}
}
