package modbus

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	mbcodec "uic/internal/codec/modbus"
)

// client 是一个极简的主站侧测试客户端，直接收发 MBAP 帧。
type client struct {
	t    *testing.T
	conn net.Conn
	tx   uint16
}

func dial(t *testing.T, s *Server) *client {
	t.Helper()
	c, err := net.DialTimeout("tcp", s.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{t: t, conn: c}
}

func (c *client) roundTrip(pdu []byte) []byte {
	c.t.Helper()
	c.tx++
	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write(mbcodec.BuildTCP(mbcodec.Frame{Transaction: c.tx, Unit: 1, PDU: pdu})); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	head := make([]byte, 7)
	if _, err := io.ReadFull(c.conn, head); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	if got := binary.BigEndian.Uint16(head[0:2]); got != c.tx {
		c.t.Fatalf("transaction echo = %d, want %d", got, c.tx)
	}
	length := int(binary.BigEndian.Uint16(head[4:6]))
	body := make([]byte, length-1)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return body
}

func readRegs(addr, qty int) []byte {
	pdu := make([]byte, 5)
	pdu[0] = mbcodec.FCReadHoldingRegs
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))
	return pdu
}

func newTestServer(t *testing.T, size int) *Server {
	t.Helper()
	s, err := NewServer("127.0.0.1:0", size)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestServerReadHolding(t *testing.T) {
	s := newTestServer(t, 64)
	s.SetHolding(3, 0x1122, 0x3344)
	c := dial(t, s)

	resp := c.roundTrip(readRegs(3, 2))
	if !bytes.Equal(resp, []byte{0x03, 0x04, 0x11, 0x22, 0x33, 0x44}) {
		t.Fatalf("resp = % x", resp)
	}
}

func TestServerReadCoils(t *testing.T) {
	s := newTestServer(t, 64)
	s.SetCoil(0, true)
	s.SetCoil(2, true)
	c := dial(t, s)

	pdu := []byte{mbcodec.FCReadCoils, 0, 0, 0, 4}
	resp := c.roundTrip(pdu)
	// 位 0 与位 2 置位 → 0b0101。
	if len(resp) != 3 || resp[1] != 1 || resp[2] != 0b0101 {
		t.Fatalf("resp = % x", resp)
	}
}

// 越界读必须返回异常码 02，而不是静默返回零值。
func TestServerOutOfRange(t *testing.T) {
	s := newTestServer(t, 16)
	c := dial(t, s)
	resp := c.roundTrip(readRegs(15, 4))
	if len(resp) != 2 || resp[0] != mbcodec.FCReadHoldingRegs|0x80 || resp[1] != 2 {
		t.Fatalf("resp = % x", resp)
	}
}

func TestServerWriteSingleRegisterAndCoil(t *testing.T) {
	s := newTestServer(t, 32)
	c := dial(t, s)

	reg := []byte{mbcodec.FCWriteSingleRegister, 0, 4, 0xAB, 0xCD}
	if resp := c.roundTrip(reg); !bytes.Equal(resp, []byte{mbcodec.FCWriteSingleRegister, 0, 4, 0xAB, 0xCD}) {
		t.Fatalf("reg echo = % x", resp)
	}
	if got := s.Holding(4); got != 0xABCD {
		t.Fatalf("holding[4] = %#x", got)
	}

	coil := []byte{mbcodec.FCWriteSingleCoil, 0, 1, 0xFF, 0x00}
	if resp := c.roundTrip(coil); len(resp) != 5 {
		t.Fatalf("coil echo = % x", resp)
	}
	// 回读确认线圈。
	resp := c.roundTrip([]byte{mbcodec.FCReadCoils, 0, 1, 0, 1})
	if len(resp) != 3 || resp[2]&1 == 0 {
		t.Fatalf("coil read = % x", resp)
	}
}

func TestServerWriteMultipleRegisters(t *testing.T) {
	s := newTestServer(t, 32)
	c := dial(t, s)
	pdu := []byte{mbcodec.FCWriteMultipleRegs, 0, 10, 0, 3, 6,
		0x00, 0x01, 0x00, 0x02, 0x00, 0x03}
	resp := c.roundTrip(pdu)
	if len(resp) != 5 || binary.BigEndian.Uint16(resp[3:5]) != 3 {
		t.Fatalf("resp = % x", resp)
	}
	for i, want := range []uint16{1, 2, 3} {
		if got := s.Holding(10 + i); got != want {
			t.Fatalf("holding[%d] = %d, want %d", 10+i, got, want)
		}
	}
}

func TestServerWriteMultipleCoils(t *testing.T) {
	s := newTestServer(t, 32)
	c := dial(t, s)
	// 写 3 个线圈：位 0 与位 2 置位（0b101）。
	pdu := []byte{mbcodec.FCWriteMultipleCoils, 0, 5, 0, 3, 1, 0b0000_0101}
	if resp := c.roundTrip(pdu); len(resp) != 5 {
		t.Fatalf("resp = % x", resp)
	}
	resp := c.roundTrip([]byte{mbcodec.FCReadCoils, 0, 5, 0, 3})
	if len(resp) != 3 || resp[2]&0b111 != 0b101 {
		t.Fatalf("coil read = % x", resp)
	}
}

// 非法功能码必须回异常 01。
func TestServerUnknownFunction(t *testing.T) {
	s := newTestServer(t, 8)
	c := dial(t, s)
	resp := c.roundTrip([]byte{0x42, 0, 0, 0, 1})
	if len(resp) != 2 || resp[0] != 0xC2 || resp[1] != 1 {
		t.Fatalf("resp = % x", resp)
	}
}

// 畸形帧：服务端应关闭连接，而不是死循环或 panic。
func TestServerMalformedLengthClosesConnection(t *testing.T) {
	s := newTestServer(t, 8)
	c := dial(t, s)
	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	// length=1 非法（最小 2）。
	bad := []byte{0, 1, 0, 0, 0, 1, 1}
	if _, err := c.conn.Write(bad); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(c.conn, make([]byte, 1)); err == nil {
		t.Fatal("expected connection close")
	}
}

// Close 必须关闭在飞连接，否则调用方会卡到读超时。
func TestServerCloseTerminatesActiveConnections(t *testing.T) {
	s := newTestServer(t, 8)
	c := dial(t, s)
	if resp := c.roundTrip(readRegs(0, 1)); len(resp) != 4 {
		t.Fatalf("resp = % x", resp)
	}

	done := make(chan struct{})
	go func() {
		_ = s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on an active connection")
	}
	// 重复 Close 必须幂等。
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// 连接已断，后续读应失败。
	_ = c.conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection should be closed")
	}
}

// 多客户端并发读写：内存模型必须在锁内更新。
func TestServerConcurrentClients(t *testing.T) {
	s := newTestServer(t, 512)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer c.Close()
			cl := &client{t: t, conn: c}
			addr := n * 4
			pdu := []byte{mbcodec.FCWriteSingleRegister, 0, 0, 0, 0}
			binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
			binary.BigEndian.PutUint16(pdu[3:5], uint16(n+1))
			cl.roundTrip(pdu)
			if got := s.Holding(addr); got != uint16(n+1) {
				t.Errorf("holding[%d] = %d", addr, got)
			}
			cl.roundTrip(readRegs(addr, 1))
		}(i)
	}
	wg.Wait()
}

func TestServerSettersClamp(t *testing.T) {
	s := newTestServer(t, 4)
	// 越界写入不得 panic。
	s.SetHolding(-1, 1)
	s.SetHolding(3, 9, 10)
	s.SetCoil(-1, true)
	s.SetCoil(99, true)
	if got := s.Holding(3); got != 9 {
		t.Fatalf("holding[3] = %d", got)
	}
	if got := s.Holding(99); got != 0 {
		t.Fatalf("out-of-range read = %d", got)
	}
}

func TestAddrIsUsable(t *testing.T) {
	s := newTestServer(t, 4)
	if s.Addr() == "" {
		t.Fatal("empty addr")
	}
	if _, err := net.ResolveTCPAddr("tcp", s.Addr()); err != nil {
		t.Fatalf("addr not resolvable: %v", err)
	}
}
