package opcua

// 断线重连必须释放上一条订阅连接，且要先关旧、再拨新。
//
// 历史缺陷：run 在 publishUntilError 出错后直接调 establish，而 establish
// 只用 s.open 建一条新连接并把它赋给 s.conn——上一条连接从未 Close。错误
// 不一定意味着 socket 已死：订阅状态为 Bad、或一条通知解不开时，连接本身
// 仍然活着。于是每次重连都泄漏一条 TCP fd 与服务端会话，跑久了表现为
// 「fd 用尽 / 服务端会话暴涨」。conn 的 read/write 错误路径只返回错误、
// 不自关传输，所以泄漏不会被下层顺手兜住。
//
// 顺序同样重要：若先拨新连接再关旧的，会有一段窗口里服务端同时挂着两条
// 会话（旧订阅的 DeleteSubscriptions 也依赖旧连接还开着）。因此断言
// 「establish 调用 open 的那一刻，旧连接已经被关」。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/model"
	"uic/internal/transport"
)

// closeTrackingStream 只记录自己是否被关闭，读写一律失败。
type closeTrackingStream struct {
	closed atomic.Bool
}

func (s *closeTrackingStream) Write([]byte) error           { return errors.New("closed") }
func (s *closeTrackingStream) ReadFull(int) ([]byte, error) { return nil, errors.New("closed") }
func (s *closeTrackingStream) Close() error {
	s.closed.Store(true)
	return nil
}

// fakeConn 返回一条「已打开、但底层必然失败」的连接，用于观察它是否被关。
// 零值 sessionID/channelID 让 conn.Close 跳过 CloseSession/CloseSecureChannel，
// 只关底层 stream。
func fakeConn(deviceID string, st *closeTrackingStream) *conn {
	var s transport.Stream = st
	c := &conn{deviceID: deviceID, stream: s}
	c.streamBox.Store(&s)
	return c
}

func TestEstablishClosesPreviousConnectionBeforeReopen(t *testing.T) {
	openErr := errors.New("server down")

	oldStream := &closeTrackingStream{}
	old := fakeConn("dev", oldStream)

	// 拨新连接时探一次旧流的状态：必须已经关闭。
	var oldClosedAtOpen atomic.Bool
	points := []subPoint{{id: "p", node: ua.StringNode(2, "P"), dt: model.TypeFloat64}}
	s := newSubState("dev", points, 100*time.Millisecond, 100*time.Millisecond,
		func(context.Context) (*conn, error) {
			oldClosedAtOpen.Store(oldStream.closed.Load())
			return nil, openErr // 失败即可，无需协议交互
		},
		nil, func(string, ...any) {})

	// 模拟 publishUntilError 失败后残留的上一条订阅连接。
	s.mu.Lock()
	s.conn = old
	s.subID = 0 // 0 让 closeCurrent 跳过 DeleteSubscriptions 网络调用
	s.mu.Unlock()

	if err := s.establish(context.Background()); err == nil {
		t.Fatal("establish must fail when open fails")
	}
	if !oldClosedAtOpen.Load() {
		t.Fatal("establish dialed the replacement before closing the previous connection")
	}
	if !oldStream.closed.Load() {
		t.Fatal("previous subscription connection leaked: establish did not close it")
	}

	s.mu.RLock()
	cur := s.conn
	s.mu.RUnlock()
	if cur != nil {
		t.Fatal("a failed establish must not leave a stale connection in s.conn")
	}
}
