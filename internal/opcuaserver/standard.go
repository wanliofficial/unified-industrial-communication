package opcuaserver

import (
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/model"
)

// StandardNodes 返回最小标准地址空间：
//
//	Root
//	├─ Objects
//	│  └─ Server
//	│     └─ ServerStatus
//	│        └─ CurrentTime
//	├─ Types
//	└─ Views
//
// 为什么要带这些「用不到」的节点：通用客户端（那些带浏览树的 GUI 工具）
// 在连接后会先读 Objects 与 ServerStatus.CurrentTime，读不到会显示一个
// 大红叉，运维就会认为服务端没起来。多这四个节点换来的是「能被现成工具
// 直接浏览」。
//
// CurrentTime 由服务端每秒自动刷新（见 Server.clockLoop），调用方不必维护。
func StandardNodes() []Node {
	root := ua.Numeric(0, ua.NodeRootFolder)
	objects := ua.Numeric(0, ua.NodeObjectsFolder)
	return []Node{
		{ID: root, BrowseName: "Root", DisplayName: "Root", Class: ua.NodeClassObject},
		{ID: objects, Parent: root, BrowseName: "Objects", DisplayName: "Objects", Class: ua.NodeClassObject},
		{ID: ua.Numeric(0, ua.NodeTypesFolder), Parent: root, BrowseName: "Types", DisplayName: "Types", Class: ua.NodeClassObject},
		{ID: ua.Numeric(0, ua.NodeViewsFolder), Parent: root, BrowseName: "Views", DisplayName: "Views", Class: ua.NodeClassObject},
		{ID: ua.Numeric(0, ua.NodeServer), Parent: objects, BrowseName: "Server", DisplayName: "Server", Class: ua.NodeClassObject},
		{ID: ua.Numeric(0, ua.NodeServerStatus), Parent: ua.Numeric(0, ua.NodeServer), BrowseName: "ServerStatus", DisplayName: "ServerStatus", Class: ua.NodeClassObject},
		{
			ID: ua.Numeric(0, 2258), Parent: ua.Numeric(0, ua.NodeServerStatus),
			BrowseName: "CurrentTime", DisplayName: "CurrentTime",
			Class: ua.NodeClassVariable, DataType: model.TypeString,
			Value: ua.Variant{Type: ua.TypeDateTime, Time: time.Now()},
		},
	}
}

// clockLoop 每秒刷新标准 CurrentTime 节点。
func (s *Server) clockLoop() {
	defer s.wg.Done()
	nid := ua.Numeric(0, 2258)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			now := time.Now()
			s.Update(nid, ua.Variant{Type: ua.TypeDateTime, Time: now}, ua.StatusGood, now)
		case <-s.done():
			return
		}
	}
}

// done 在服务端关闭后返回一个已关闭的 channel。
//
// 复用 ln 关闭作为信号：Close 先置 closed 再 ln.Close，Accept 立即返回，
// 但 clockLoop 并不在 accept 路径上，需要一个独立的信号源。用闭包轮询
// 会引入无谓的唤醒，所以这里放一个惰性 channel。
func (s *Server) done() <-chan struct{} {
	s.mu.RLock()
	ch := s.doneCh
	s.mu.RUnlock()
	return ch
}
