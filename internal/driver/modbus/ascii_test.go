package modbus

import (
	"context"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

// ASCII 与 RTU 是两条独立的总线语义：配置里选错协议时，唯一能兜底的
// 就是装配期校验。因此 Meta 必须诚实地只声明 serial。
func TestASCIIMetaDeclaresSerialOnly(t *testing.T) {
	m := NewASCII().Meta()
	if m.ID != "modbus-ascii" {
		t.Fatalf("id = %q", m.ID)
	}
	if len(m.Transports) != 1 || m.Transports[0] != "serial" {
		t.Fatalf("transports = %v, want [serial]", m.Transports)
	}
	if len(m.Roles) != 1 || m.Roles[0] != driver.RoleMaster {
		t.Fatalf("roles = %v", m.Roles)
	}
	if !m.Accepts(model.AddressRegister) {
		t.Fatal("ASCII must accept register addressing")
	}
	if m.Capabilities.Read != driver.CapSupported || m.Capabilities.Write != driver.CapSupported {
		t.Fatalf("caps = %+v", m.Capabilities)
	}
	if m.Capabilities.Subscribe != driver.CapUnsupported {
		t.Fatal("ASCII must not claim subscribe support")
	}
}

func TestASCIIRejectsTCPTransport(t *testing.T) {
	_, err := NewASCII().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "modbus-ascii",
			Transport: model.Transport{Kind: "tcp", Address: "h:502"}},
		Device: model.Device{ID: "d", UnitID: 1},
	})
	if err == nil {
		t.Fatal("ASCII over tcp must be rejected")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("kind = %v, want ErrUnsupported", driver.KindOf(err))
	}
}

func TestASCIIRejectsBadSerialParams(t *testing.T) {
	opus := []model.Transport{
		{Kind: "serial", Port: ""},                                   // 缺端口
		{Kind: "serial", Port: "/dev/x", Baud: 1234},                 // 非法波特率
		{Kind: "serial", Port: "/dev/x", Baud: 9600, Parity: "mark"}, // 非法校验
	}
	for _, tr := range opus {
		_, err := NewASCII().Open(context.Background(), driver.OpenConfig{
			Channel: model.Channel{ID: "ch", Protocol: "modbus-ascii", Transport: tr},
			Device:  model.Device{ID: "d", UnitID: 1},
		})
		if err == nil {
			t.Fatalf("transport %+v must be rejected", tr)
		}
	}
}
