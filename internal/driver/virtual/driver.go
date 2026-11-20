// Package virtual 实现一个不依赖任何硬件与网络的虚拟设备驱动。
//
// 它用于三件事：无真机开发、CI 回归、故障注入。波形与故障均由设备标签
// 控制，因此同一个驱动可以模拟正弦/斜坡/随机/常量，以及丢包、超时、
// 地址错误等异常。
package virtual

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
)

// Driver 是虚拟设备驱动。
type Driver struct{}

// New 返回虚拟驱动。
func New() *Driver { return &Driver{} }

// Meta 声明虚拟驱动能力。
//
// 能力位是「实际存在的行为」的声明，不是配置意图：Conn 契约只有
// Probe/Read/Write/Close，本驱动没有任何订阅/浏览代码路径，Subscribe 与
// Browse 如实声明 unsupported——虚标 supported 会经 driver.List() 暴露给
// API/UI，让配置界面出现两个永远点不动的按钮。
func (d *Driver) Meta() driver.Meta {
	return driver.Meta{
		ID:           "virtual",
		Name:         "虚拟设备",
		Version:      "0.1.0",
		SDKVersion:   driver.SDKVersion,
		Roles:        []driver.Role{driver.RoleBoth},
		Transports:   []string{"memory"},
		AddressKinds: []model.AddressKind{model.AddressRegister, model.AddressSymbol},
		Capabilities: driver.Capabilities{
			Read: driver.CapSupported, Write: driver.CapSupported,
			Subscribe: driver.CapUnsupported, Browse: driver.CapUnsupported,
			Discovery: driver.CapUnsupported, MultiRead: driver.CapSupported,
			BitAccess: driver.CapSupported, StructTypes: driver.CapUnsupported,
		},
		ConfigFields: []driver.ConfigField{
			{Name: "labels.mode", Label: "波形", Type: "enum", Scope: "device",
				Enum: []string{"sine", "ramp", "random", "const"}, Default: "sine"},
			{Name: "labels.fault", Label: "故障注入", Type: "enum", Scope: "device",
				Enum: []string{"none", "drop", "timeout", "bad", "address", "disconnect"}, Default: "none"},
			{Name: "labels.faultAt", Label: "第几次请求注入", Type: "int", Scope: "device", Default: 0},
		},
		Description: "内置虚拟设备：正弦/斜坡/随机/常量波形 + 故障注入",
	}
}

// Open 创建虚拟连接。
func (d *Driver) Open(_ context.Context, cfg driver.OpenConfig) (driver.Conn, error) {
	if cfg.Channel.Transport.Kind != "memory" {
		return nil, driver.E(driver.ErrUnsupported, "open", cfg.Device.ID,
			"virtual driver requires memory transport", false, nil)
	}
	return &conn{
		dev:     cfg.Device,
		mode:    label(cfg.Device, "mode", "sine"),
		fault:   label(cfg.Device, "fault", "none"),
		faultAt: atoiDefault(label(cfg.Device, "faultAt", "0"), 0),
		started: time.Now(),
		written: map[string]model.Value{},
		rand:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

type conn struct {
	mu      sync.Mutex
	dev     model.Device
	mode    string
	fault   string
	faultAt int
	started time.Time
	probes  int
	reads   int
	writes  int
	written map[string]model.Value
	rand    *rand.Rand
}

func (c *conn) Probe(context.Context) error {
	c.mu.Lock()
	c.probes++
	n := c.probes
	c.mu.Unlock()
	if c.hit(n) {
		return driver.E(driver.ErrDisconnected, "probe", c.dev.ID, "injected disconnect", true, nil)
	}
	return nil
}

// hit 报告「第 n 次请求是否命中注入点」。所有请求计数共享同一序列
// （probe/read/write 都从它取号），因此「第 n 次请求注入」对三个入口
// 含义一致：到点就断，与请求来自哪个方法无关。
func (c *conn) hit(n int) bool {
	return c.fault != "none" && c.faultAt > 0 && n == c.faultAt
}

func (c *conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	c.mu.Lock()
	c.reads++
	n := c.reads
	c.mu.Unlock()

	if c.hit(n) {
		if err := c.injectFault(); err != nil {
			return nil, err
		}
	}
	select {
	case <-ctx.Done():
		return nil, driver.E(driver.ErrTimeout, "read", c.dev.ID, "context done", true, ctx.Err())
	default:
	}

	out := make([]driver.RawSample, 0, 16)
	now := time.Since(c.started)
	for _, span := range spans {
		for _, p := range span.Points {
			out = append(out, c.sample(p, now))
		}
	}
	return out, nil
}

func (c *conn) Write(_ context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.mu.Lock()
	c.writes++
	n := c.writes
	c.mu.Unlock()
	if c.hit(n) {
		if err := c.injectFault(); err != nil {
			return nil, err
		}
	}
	out := make([]driver.WriteResult, 0, len(reqs))
	for _, r := range reqs {
		c.written[r.PointID] = r.Value
		out = append(out, driver.WriteResult{PointID: r.PointID, OK: true, Quality: model.QualityGood})
	}
	return out, nil
}

func (c *conn) Close() error { return nil }

// injectFault 返回当前故障对应的错误（调用方已确认命中注入点）。
// 门限判定（faultAt）由 hit 负责，这里只负责「命中之后错成什么样」。
func (c *conn) injectFault() error {
	switch c.fault {
	case "drop":
		return driver.E(driver.ErrTimeout, "read", c.dev.ID, "injected packet drop", true, nil)
	case "timeout":
		return driver.E(driver.ErrTimeout, "read", c.dev.ID, "injected timeout", true, nil)
	case "bad":
		return driver.E(driver.ErrProtocol, "read", c.dev.ID, "injected protocol error", false, nil)
	case "address":
		return driver.E(driver.ErrAddress, "read", c.dev.ID, "injected address error", false, nil)
	case "disconnect":
		// 断连是临时性故障：collector 会重连后重试，不应因此熔断整台设备。
		return driver.E(driver.ErrDisconnected, "read", c.dev.ID, "injected disconnect", true, nil)
	}
	return nil
}

func (c *conn) sample(p driver.PointRef, elapsed time.Duration) driver.RawSample {
	c.mu.Lock()
	if v, ok := c.written[p.ID]; ok {
		c.mu.Unlock()
		return driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood}
	}
	c.mu.Unlock()

	if p.DataType == model.TypeBool {
		period := 2 * time.Second
		return driver.RawSample{PointID: p.ID, Value: model.BoolValue(int(elapsed/period)%2 == 0), Quality: model.QualityGood}
	}
	if !p.DataType.IsNumeric() {
		return driver.RawSample{PointID: p.ID, Value: model.TextValue(p.DataType, "RUN"), Quality: model.QualityGood}
	}
	phase := float64(hash(p.ID)%1000) / 1000.0 * 2 * math.Pi
	secs := elapsed.Seconds()
	var v float64
	switch c.mode {
	case "ramp":
		v = math.Mod(secs*10+phase*10, 100)
	case "random":
		c.mu.Lock()
		v = c.rand.Float64() * 100
		c.mu.Unlock()
	case "const":
		v = 42
	default: // sine
		v = 50 + 40*math.Sin(secs/5+phase)
	}
	return driver.RawSample{PointID: p.ID, Value: model.NumValue(p.DataType, v), Quality: model.QualityGood}
}

func label(d model.Device, key, def string) string {
	if d.Labels == nil {
		return def
	}
	if v, ok := d.Labels[key]; ok && v != "" {
		return v
	}
	return def
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
