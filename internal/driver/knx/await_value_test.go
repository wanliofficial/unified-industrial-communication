package knx

import (
	"context"
	"errors"
	"testing"
	"time"

	knxcodec "uic/internal/codec/knx"
	"uic/internal/driver"
)

// failReadDatagram 的 Read 总是返回一个「非超时」的底层错误（例如已连接
// UDP 套接字上的 ECONNREFUSED）。readFrame 遇到这类错误会 dropLink()，把
// c.link 置空。
type failReadDatagram struct {
	closed bool
}

func (d *failReadDatagram) Write([]byte) error { return nil }
func (d *failReadDatagram) Read() ([]byte, error) {
	return nil, errors.New("udp: read: connection refused")
}
func (d *failReadDatagram) SetDeadline(time.Time) error { return nil }
func (d *failReadDatagram) Close() error                { d.closed = true; return nil }

// awaitValue 在等待总线应答时遇到底层读错误，readFrame 已经把 c.link 置空；
// 若 defer 里仍用 c.link.SetDeadline，会对 nil 接口调用方法而 panic。
// 驱动在任何情况下都不许 panic——必须把链路错误作为 error 返回。
func TestAwaitValueLinkErrorReturnsInsteadOfPanicking(t *testing.T) {
	link := &failReadDatagram{}
	c := &conn{devID: "d", timeout: time.Second, link: link}

	err := c.awaitValue(context.Background(), "read", 100*time.Millisecond,
		func(knxcodec.LData) (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("want a link error, got nil")
	}
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("kind = %v (err=%v)", driver.KindOf(err), err)
	}
	if c.link != nil {
		t.Fatal("link should have been dropped after a read error")
	}
	if !link.closed {
		t.Fatal("underlying datagram should have been closed")
	}
}
