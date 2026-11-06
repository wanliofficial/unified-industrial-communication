package opcuaserver

import (
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/model"
)

// NewSimulator 起一个带演示地址空间的服务端，用于自测、演示与驱动一致性
// 测试。它与主站共用 codec/opcua，所以「Variant 怎么编码」「状态码怎么
// 解释」这两件事不会在主从之间分叉。
//
//	Root/Objects/Plant
//	├─ Counter      int32  每秒自增（由调用方的计时器驱动）
//	├─ Temperature  double 21.5
//	├─ Running      bool   true
//	├─ Label        string 可写
//	└─ Setpoint     double 可写
//
// 变量都挂在 ns=2 下、用字符串 NodeId：现场点位几乎都是这种形式，用数值
// NodeId 演示会让 examples 与真实配置差得太远。
func NewSimulator(addr string) (*Server, error) {
	nodes := StandardNodes()
	plant := ua.StringNode(2, "Plant")
	nodes = append(nodes,
		Node{ID: plant, Parent: ua.Numeric(0, ua.NodeObjectsFolder),
			BrowseName: "Plant", Namespace: 2, DisplayName: "Plant", Class: ua.NodeClassObject},
		variable(plant, 2, "Plant.Counter", "Counter", model.TypeInt32,
			ua.Variant{Type: ua.TypeInt32, Int: 0}, false),
		variable(plant, 2, "Plant.Temperature", "Temperature", model.TypeFloat64,
			ua.Variant{Type: ua.TypeDouble, Float: 21.5}, false),
		variable(plant, 2, "Plant.Running", "Running", model.TypeBool,
			ua.Variant{Type: ua.TypeBoolean, Bool: true}, false),
		variable(plant, 2, "Plant.Label", "Label", model.TypeString,
			ua.Variant{Type: ua.TypeString, Str: "SIM-01"}, true),
		variable(plant, 2, "Plant.Setpoint", "Setpoint", model.TypeFloat64,
			ua.Variant{Type: ua.TypeDouble, Float: 60}, true),
	)
	return New(Config{
		Addr:            addr,
		Nodes:           nodes,
		ApplicationName: "UIC Simulator",
		ApplicationURI:  "urn:uic:sim",
	})
}

// variable 构造一个变量节点。放在这里而不是导出，是因为「节点长什么样」
// 属于内部约定：调用方只需要 Node 结构，不需要这个快捷函数的签名。
func variable(parent ua.NodeID, ns uint16, id, name string, t model.DataType, v ua.Variant, writable bool) Node {
	return Node{
		ID: ua.StringNode(ns, id), Parent: parent,
		BrowseName: name, Namespace: ns, DisplayName: name,
		Class: ua.NodeClassVariable, DataType: t, Value: v, Writable: writable,
		UpdatedAt: time.Now(),
	}
}
