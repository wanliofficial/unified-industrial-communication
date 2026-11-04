package runtime

import (
	"errors"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
)

// 装配期校验：协议与传输不匹配属于静态配置错误，必须在启动时就暴露，
// 而不是让设备一直显示「连接失败」。现场排查「为什么连不上」的成本远
// 高于「启动时报一行错」。
func TestCheckTransportSupport(t *testing.T) {
	tcp := model.Channel{ID: "ch", Protocol: "modbus-tcp",
		Transport: model.Transport{Kind: "tcp", Address: "h:502"}}
	serial := model.Channel{ID: "ch", Protocol: "modbus-rtu",
		Transport: model.Transport{Kind: "serial", Port: "/dev/ttyUSB0", Baud: 9600}}
	mem := model.Channel{ID: "ch", Protocol: "virtual",
		Transport: model.Transport{Kind: "memory"}}

	md := driver.Meta{ID: "modbus-tcp", Transports: []string{"tcp"}}
	if err := checkTransportSupport(md, tcp); err != nil {
		t.Fatalf("tcp channel: %v", err)
	}
	if err := checkTransportSupport(md, serial); err == nil {
		t.Fatal("modbus-tcp must reject serial transport")
	}

	mr := driver.Meta{ID: "modbus-rtu", Transports: []string{"serial"}}
	if err := checkTransportSupport(mr, serial); err != nil {
		t.Fatalf("serial channel: %v", err)
	}
	if err := checkTransportSupport(mr, tcp); err == nil {
		t.Fatal("modbus-rtu must reject tcp transport")
	}

	mv := driver.Meta{ID: "virtual", Transports: []string{"memory"}}
	if err := checkTransportSupport(mv, mem); err != nil {
		t.Fatalf("memory channel: %v", err)
	}
}

// 错误必须带可识别代码，客户端才知道这是配置问题而不是运行期故障。
func TestCheckTransportSupportErrorCode(t *testing.T) {
	md := driver.Meta{ID: "modbus-tcp", Transports: []string{"tcp"}}
	err := checkTransportSupport(md, model.Channel{ID: "ch", Protocol: "modbus-tcp",
		Transport: model.Transport{Kind: "serial", Port: "/dev/x"}})

	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err should be *httpapi.APIError: %v", err)
	}
	if apiErr.Code != "config.unsupported_transport" {
		t.Fatalf("code = %q", apiErr.Code)
	}
	if apiErr.Status != 422 {
		t.Fatalf("status = %d", apiErr.Status)
	}
}

// `uic -check` 与 runtime 装配必须共用同一套绑定校验，否则会出现
// 「CI 通过但启动失败」的不一致。
func TestCheckDriverBindings(t *testing.T) {
	if err := RegisterBuiltins(); err != nil {
		t.Fatalf("register: %v", err)
	}
	good := []model.Channel{
		{ID: "a", Protocol: "modbus-tcp", Transport: model.Transport{Kind: "tcp", Address: "h:502"}},
		{ID: "b", Protocol: "modbus-rtu", Transport: model.Transport{Kind: "serial", Port: "/dev/ttyUSB0", Baud: 9600}},
		{ID: "c", Protocol: "modbus-ascii", Transport: model.Transport{Kind: "serial", Port: "/dev/ttyUSB0", Baud: 9600}},
		{ID: "d", Protocol: "virtual", Transport: model.Transport{Kind: "memory"}},
	}
	if err := CheckDriverBindings(good); err != nil {
		t.Fatalf("valid channels rejected: %v", err)
	}

	bad := []model.Channel{
		{ID: "x", Protocol: "modbus-tcp", Transport: model.Transport{Kind: "serial", Port: "/dev/ttyS0"}},
		// ASCII 也只能走串口；配成 tcp 同样是静态错误。
		{ID: "z", Protocol: "modbus-ascii", Transport: model.Transport{Kind: "tcp", Address: "h:502"}},
	}
	if err := CheckDriverBindings(bad); err == nil {
		t.Fatal("protocol/transport mismatch must be rejected")
	}

	unknown := []model.Channel{
		{ID: "y", Protocol: "no-such-protocol", Transport: model.Transport{Kind: "tcp", Address: "h:1"}},
	}
	if err := CheckDriverBindings(unknown); err == nil {
		t.Fatal("unknown protocol must be rejected")
	}
}
