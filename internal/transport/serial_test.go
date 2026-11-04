package transport

import (
	"strings"
	"testing"
)

func TestSerialConfigDefaults(t *testing.T) {
	c := SerialConfig{Port: "/dev/ttyS0"}.Normalized()
	if c.Baud != 9600 || c.DataBits != 8 || c.StopBits != 1 || c.Parity != "none" {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestSerialConfigValidate(t *testing.T) {
	bad := []SerialConfig{
		{},                                      // 没有端口
		{Port: "x", Baud: 12345},                // 非法波特率
		{Port: "x", Baud: 9600, DataBits: 9},    // 数据位越界
		{Port: "x", Baud: 9600, DataBits: 4},    // 数据位越界
		{Port: "x", Baud: 9600, Parity: "mark"}, // 不支持的校验
		{Port: "x", Baud: 9600, StopBits: 3},    // 停止位非法
		{Port: "x", Baud: 9600, StopBits: -1},   // 停止位非法
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d (%+v) should fail", i, c)
		}
	}

	good := []SerialConfig{
		{Port: "x", Baud: 9600},
		{Port: "x", Baud: 19200, DataBits: 8, Parity: "even", StopBits: 1},
		{Port: "x", Baud: 921600, DataBits: 7, Parity: "odd", StopBits: 2},
		{Port: "x", Baud: 1200},
	}
	for i, c := range good {
		if err := c.Validate(); err != nil {
			t.Fatalf("case %d (%+v): %v", i, c, err)
		}
	}
}

func TestSerialConfigString(t *testing.T) {
	c := SerialConfig{Port: "/dev/ttyUSB0", Baud: 9600, DataBits: 8, Parity: "none", StopBits: 1}
	got := c.String()
	if !strings.Contains(got, "/dev/ttyUSB0") || !strings.Contains(got, "9600") {
		t.Fatalf("string = %q", got)
	}
}

func TestSerialFromTransport(t *testing.T) {
	// 省略字段必须补成协议最常见的 9600-8-N-1，而不是留零值。
	c := SerialFromTransport("/dev/ttyS1", 0, 0, "", 0)
	if c.Baud != 9600 || c.DataBits != 8 || c.Parity != "none" || c.StopBits != 1 {
		t.Fatalf("normalized = %+v", c)
	}
	c = SerialFromTransport("/dev/ttyS1", 115200, 7, "even", 2)
	if c.Baud != 115200 || c.DataBits != 7 || c.Parity != "even" || c.StopBits != 2 {
		t.Fatalf("explicit = %+v", c)
	}
}
