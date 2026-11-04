package runtime

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	mb "uic/internal/codec/modbus"
)

// 从站端到端：真实 Modbus TCP 从站 + 真实采集链路。
//
// 这里刻意不复用从站包内部的测试夹具：要验证的是「配置 → 运行时 →
// 缓存 → 线上字节」整条链，任何一环自己造数据都会让测试失去意义。

type mbClient struct {
	t    *testing.T
	conn net.Conn
	tx   uint16
}

func dialSlave(t *testing.T, addr string) *mbClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial slave %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &mbClient{t: t, conn: c}
}

func (c *mbClient) roundTrip(unit byte, pdu []byte) []byte {
	c.t.Helper()
	c.tx++
	frame := mb.BuildTCP(mb.Frame{Transaction: c.tx, Unit: unit, PDU: pdu})
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, 7)
	if _, err := io.ReadFull(c.conn, head); err != nil {
		c.t.Fatalf("read head: %v", err)
	}
	n := int(binary.BigEndian.Uint16(head[4:6]))
	body := make([]byte, n-1)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return body
}

func (c *mbClient) readRegs(unit byte, addr, qty int) []uint16 {
	c.t.Helper()
	pdu := make([]byte, 5)
	pdu[0] = mb.FCReadHoldingRegs
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	resp := c.roundTrip(unit, pdu)
	if len(resp) < 2 || resp[0] != mb.FCReadHoldingRegs {
		c.t.Fatalf("read %d@%d: % x", qty, addr, resp)
	}
	out := make([]uint16, qty)
	for i := range out {
		out[i] = binary.BigEndian.Uint16(resp[2+i*2:])
	}
	return out
}

func (c *mbClient) readCoils(unit byte, addr, qty int) byte {
	c.t.Helper()
	pdu := make([]byte, 5)
	pdu[0] = mb.FCReadCoils
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	resp := c.roundTrip(unit, pdu)
	if len(resp) != 3 || resp[0] != mb.FCReadCoils {
		c.t.Fatalf("read coils: % x", resp)
	}
	return resp[2]
}

// withSlave 在基础配置上挂一个 Modbus TCP 从站。
func withSlave(mutate func(cfg map[string]any)) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		cfg["slaves"] = []any{map[string]any{
			"id": "scada", "listen": "127.0.0.1:0",
			"units": []any{map[string]any{
				"unitId": 1, "byteOrder": "ABCD",
				"mappings": []any{
					map[string]any{"point": "sim01/reg0", "area": "holding", "offset": 0},
					map[string]any{"point": "sim01/reg10", "area": "holding", "offset": 1},
					map[string]any{"point": "sim01/write_me", "area": "holding", "offset": 2, "writable": true},
					map[string]any{"point": "sim01/coil0", "area": "coil", "offset": 0},
					map[string]any{"point": "sim01/bit3", "area": "discrete", "offset": 0},
				},
			}},
		}}
		if mutate != nil {
			mutate(cfg)
		}
	}
}

// TestSlaveServesCacheAndForwardsWrites 是这套功能的核心验收：
// 上位机读从站 → 拿到的是网关采集到的值；上位机写从站 → 值真的落到设备。
func TestSlaveServesCacheAndForwardsWrites(t *testing.T) {
	h := newHarness(t, withSlave(nil))
	h.waitPointGood("sim01", "reg0")
	h.waitPointGood("sim01", "reg10")

	addrs := h.rt.Slaves()
	addr, ok := addrs["scada"]
	if !ok || addr == "" {
		t.Fatalf("slave not listening: %v", addrs)
	}
	c := dialSlave(t, addr)

	// holding[0] 在模拟从站上初始化为 1234，holding[10] 为 0；两者都来自
	// 真实链路，而不是测试自己塞进缓存的假值。
	regs := c.readRegs(1, 0, 2)
	if regs[0] != 1234 || regs[1] != 0 {
		t.Fatalf("holding = %v, want [1234 0] (values from the Modbus device)", regs)
	}
	if b := c.readCoils(1, 0, 1); b&0x01 != 1 {
		t.Fatalf("coil0 = %#b, want bit0 set", b)
	}

	// 写 setpoint：必须穿透到真实的 Modbus 从站（sim 的 holding[5]）。
	pdu := []byte{mb.FCWriteSingleRegister, 0, 2, 0x03, 0x09}
	resp := c.roundTrip(1, pdu)
	if len(resp) != 5 || resp[0] != mb.FCWriteSingleRegister {
		t.Fatalf("write response = % x", resp)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.sim.Holding(5) == 777 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.sim.Holding(5); got != 777 {
		t.Fatalf("device holding[5] = %d, want 777", got)
	}

	// 读回：缓存会在下一轮采集后更新，从站读到的也应是 777。
	h.waitPointGood("sim01", "write_me")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := c.readRegs(1, 2, 1)[0]; v == 777 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v := c.readRegs(1, 2, 1)[0]; v != 777 {
		t.Fatalf("slave readback = %d, want 777", v)
	}
}

func TestSlaveRejectsWriteToReadOnlyMapping(t *testing.T) {
	h := newHarness(t, withSlave(nil))
	h.waitPointGood("sim01", "reg0")
	c := dialSlave(t, h.rt.Slaves()["scada"])

	pdu := []byte{mb.FCWriteSingleRegister, 0, 0, 0x00, 0x01} // offset 0 只读
	resp := c.roundTrip(1, pdu)
	if len(resp) != 2 || resp[0] != mb.FCWriteSingleRegister|0x80 || resp[1] != 0x02 {
		t.Fatalf("response = % x, want exception 02", resp)
	}
}

func TestSlaveUnknownUnit(t *testing.T) {
	h := newHarness(t, withSlave(nil))
	h.waitPointGood("sim01", "reg0")
	c := dialSlave(t, h.rt.Slaves()["scada"])

	pdu := []byte{mb.FCReadHoldingRegs, 0, 0, 0, 1}
	resp := c.roundTrip(9, pdu)
	if len(resp) != 2 || resp[1] != 0x0B {
		t.Fatalf("response = % x, want gateway error 0B", resp)
	}
}

// 映射变化必须重建从站，否则外部主站会一直读到旧表——配置「已生效」
// 但行为没变，是最难发现的一类故障。
func TestSlaveRebuildsOnConfigChange(t *testing.T) {
	h := newHarness(t, withSlave(nil))
	h.waitPointGood("sim01", "reg0")
	oldAddr := h.rt.Slaves()["scada"]

	// 直接改当前配置里的映射偏移，再走一遍 ApplyConfig（与 REST 同一条路径）。
	cur := h.rt.cfg.Current()
	cur.Slaves[0].Units[0].Mappings[0].Offset = 10
	raw, err := json.Marshal(cur)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := h.rt.ApplyConfig(raw, "test"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	newAddr := h.rt.Slaves()["scada"]
	if newAddr == "" {
		t.Fatal("slave disappeared after reload")
	}
	// 新地址（:0 重新分配）上必须能读到新映射。
	c := dialSlave(t, newAddr)
	if regs := c.readRegs(1, 10, 1); regs[0] != 1234 {
		t.Fatalf("reg at new offset = %v, want 1234", regs)
	}
	if oldAddr == newAddr {
		t.Log("listener address reused; mapping still verified")
	}
}
