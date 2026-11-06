// Package opcua 把网关的点位表暴露成一个 OPC UA 服务端地址空间，供
// SCADA / MES / 第三方客户端直接读写。
//
//	Root/Objects
//	└─ {site}
//	   └─ {device}          （目录）
//	      └─ {point}        （变量，NodeId = ns=2;s={site}/{device}/{point}）
//
// 与南向主站的关系是**方向相反的两条链路**：南向是网关去读设备，北向是
// 别的系统来读网关。两者共用 codec/opcua 与同一个服务端内核
// （internal/opcuaserver），所以「值怎么编码」「坏质量怎么表达」在主从、
// 南北之间不会分叉。
//
// 三个刻意的设计选择：
//
//  1. **一点一变量，不做聚合变量**。SCADA 的标签表是按点位建的；把多个
//     点位塞进结构体需要客户端先理解自定义类型，反而增加集成成本。
//  2. **质量码走 DataValue.Status，不走额外的质量变量**。这是 OPC UA
//     表达「值可不可信」的标准位置；多一个 quality 变量会让每个客户端
//     都要写一遍「先读值再读质量」的胶水代码。
//  3. **写入必须真的过南向设备**。返回 Good 代表设备接受了写入，而不是
//     「网关内存里改了个数」。设备没确认就返回 Good，SCADA 会显示一个
//     从未生效的设定值——这类错误在现场极难被发现。
package opcua

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"uic/internal/cache"
	ua "uic/internal/codec/opcua"
	"uic/internal/model"
	"uic/internal/obs"
	"uic/internal/opcuaserver"
)

// namespace 是自定义节点使用的命名空间索引。0 是 OPC UA 标准命名空间，
// 1 留给服务端自身的 URI；业务点位统一放 2，客户端配置一次即可。
const namespace = 2

// writeTimeout 是北向写入等待南向设备确认的上限。太长会让 SCADA 的
// 写操作看起来「卡住」，太短会把正常的慢设备判成失败。
const writeTimeout = 5 * time.Second

// Spec 描述要暴露的一个点位。
type Spec struct {
	Device     string
	DeviceName string
	Point      string
	// Name 是给现场看的显示名；为空时用 Point。
	Name string
	// DataType 必须与南向点位的类型一致：它同时决定 OPC UA 的 DataType
	// 属性与写入类型校验。
	DataType model.DataType
	Writable bool
}

// Seed 是启动时的初始样本。没有对应点位的变量保持 BadNotConnected。
type Seed struct {
	Device string
	Point  string
	Sample model.Sample
}

// Outcome 是一次北向写入的结果。
type Outcome struct {
	OK      bool
	Code    string
	Message string
}

// Source 是北向服务端对网关的依赖。定义在这里而不是直接依赖 runtime，
// 是为了让这个包能用一个假实现单独测试——OPC UA 服务端的失败模式大多在
// 协议状态机里，与网关其余部分无关。
type Source interface {
	OPCUASpecs() []Spec
	OPCUASeeds() []Seed
	Stream(filter func(model.Sample) bool, buffer int) (<-chan cache.Event, func())
	OPCUAWrite(ctx context.Context, device, point string, v model.Value) Outcome
}

// Config 是启动参数。
type Config struct {
	Addr    string
	Site    string
	Source  Source
	Metrics *obs.Metrics
	Log     *slog.Logger
}

// Server 是运行中的北向 OPC UA 服务端。
type Server struct {
	srv     *opcuaserver.Server
	src     Source
	site    string
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	metrics *obs.Metrics
	log     *slog.Logger

	mu     sync.RWMutex
	points map[string]pointRef
	byKey  map[string]ua.NodeID
	closed bool
}

type pointRef struct {
	spec Spec
	node ua.NodeID
}

// Start 建立地址空间并开始监听。端口被占用时返回错误：让启动失败比
// 「网关起来了但 SCADA 连不上」更容易被发现。
func Start(cfg Config) (*Server, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("opcua northbound: source is nil")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	site := strings.TrimSpace(cfg.Site)
	if site == "" {
		site = "default"
	}
	s := &Server{
		src:     cfg.Source,
		site:    site,
		metrics: cfg.Metrics,
		log:     log.With("northbound", "opcua"),
		points:  map[string]pointRef{},
		byKey:   map[string]ua.NodeID{},
	}

	seeds := map[string]model.Sample{}
	for _, sd := range cfg.Source.OPCUASeeds() {
		seeds[pointKey(sd.Device, sd.Point)] = sd.Sample
	}

	specs := cfg.Source.OPCUASpecs()
	nodes := opcuaserver.StandardNodes()
	objects := ua.Numeric(0, ua.NodeObjectsFolder)

	rootID := ua.StringNode(namespace, site)
	nodes = append(nodes, opcuaserver.Node{
		ID: rootID, Parent: objects,
		BrowseName: site, Namespace: namespace, DisplayName: site,
		Class: ua.NodeClassObject,
	})

	devices := map[string]bool{}
	for _, sp := range specs {
		if !devices[sp.Device] {
			devices[sp.Device] = true
			display := sp.DeviceName
			if display == "" {
				display = sp.Device
			}
			nodes = append(nodes, opcuaserver.Node{
				ID: ua.StringNode(namespace, site+"/"+sp.Device), Parent: rootID,
				BrowseName: sp.Device, Namespace: namespace, DisplayName: display,
				Class: ua.NodeClassObject,
			})
		}

		nid := ua.StringNode(namespace, site+"/"+sp.Device+"/"+sp.Point)
		name := sp.Name
		if name == "" {
			name = sp.Point
		}

		node := opcuaserver.Node{
			ID: nid, Parent: ua.StringNode(namespace, site+"/"+sp.Device),
			BrowseName: sp.Point, Namespace: namespace, DisplayName: name,
			Class: ua.NodeClassVariable, DataType: sp.DataType, Writable: sp.Writable,
			Status: ua.StatusBadNotConnected,
		}
		if sd, ok := seeds[pointKey(sp.Device, sp.Point)]; ok {
			if v, err := ua.EncodeValue(sp.DataType, sd.Value); err == nil {
				node.Value = v
				node.Status = statusOf(sd.Quality)
				node.UpdatedAt = sd.CollectedAt
			} else {
				s.log.Warn("opcua_seed_encode_failed",
					"device", sp.Device, "point", sp.Point, "error", err.Error())
			}
		}
		nodes = append(nodes, node)

		s.mu.Lock()
		s.points[nid.String()] = pointRef{spec: sp, node: nid}
		s.byKey[pointKey(sp.Device, sp.Point)] = nid
		s.mu.Unlock()
	}

	// 排序让地址空间顺序稳定：浏览结果随 map 迭代顺序变化，会让每次
	// 重启后的「设备列表」看起来都不一样，排查时很干扰。
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Parent.String() != nodes[j].Parent.String() {
			return nodes[i].Parent.String() < nodes[j].Parent.String()
		}
		return nodes[i].ID.String() < nodes[j].ID.String()
	})

	srv, err := opcuaserver.New(opcuaserver.Config{
		Addr:  cfg.Addr,
		Nodes: nodes,
		// 端点里带上站点名，SCADA 同时接多个站点时能一眼分清。
		ApplicationName: "UIC Gateway " + site,
		ApplicationURI:  "urn:uic:" + site,
		OnWrite:         s.onWrite,
		Log:             s.log.Debug,
	})
	if err != nil {
		return nil, err
	}
	s.srv = srv

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.pushLoop(ctx)

	if s.metrics != nil {
		s.metrics.Set("uic_opcua_server_up", 1, obs.Labels{"site": site})
	}
	// site 由调用方的 logger 携带（runtime 已经 With("site", ...)），
	// 这里再写一次会在日志里出现两个同名键。
	s.log.Info("opcua_server_listening",
		"addr", srv.Addr(), "points", len(s.points))
	return s, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string {
	if s == nil || s.srv == nil {
		return ""
	}
	return s.srv.Addr()
}

// Close 停止推送并关闭服务端。
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	err := s.srv.Close()
	if s.metrics != nil {
		s.metrics.Set("uic_opcua_server_up", 0, obs.Labels{"site": s.site})
	}
	return err
}

// pushLoop 把缓存变更写进地址空间。
//
// 只推送「地址空间里有的点位」：过滤器在订阅时就生效，避免每来一条报警
// 或计算点事件都走一遍查找。
func (s *Server) pushLoop(ctx context.Context) {
	defer s.wg.Done()
	events, cancel := s.src.Stream(func(sample model.Sample) bool {
		s.mu.RLock()
		_, ok := s.byKey[pointKey(sample.DeviceID, sample.PointID)]
		s.mu.RUnlock()
		return ok
	}, 1024)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			// 质量迁移事件同样要推：只推值变化会让「设备断了」在 SCADA 上
			// 表现为「值不动了」——两者在画面上完全一样，含义完全不同。
			s.publish(ev.Sample)
		}
	}
}

func (s *Server) publish(sample model.Sample) {
	s.mu.RLock()
	nid, ok := s.byKey[pointKey(sample.DeviceID, sample.PointID)]
	ref, has := s.points[nid.String()]
	s.mu.RUnlock()
	if !ok || !has {
		return
	}
	v, err := ua.EncodeValue(ref.spec.DataType, sample.Value)
	if err != nil {
		// 值类型与配置不符：把它标成配置错误，而不是丢掉这次更新。
		// 丢掉会让 SCADA 永远显示上一次的值，看起来像设备卡住了。
		s.log.Warn("opcua_value_encode_failed",
			"device", sample.DeviceID, "point", sample.PointID,
			"want", ref.spec.DataType, "got", sample.Value.Type, "error", err.Error())
		s.srv.Update(nid, ua.Variant{}, ua.StatusBadTypeMismatch, sample.CollectedAt)
		return
	}
	s.srv.Update(nid, v, statusOf(sample.Quality), sample.CollectedAt)
}

func (s *Server) onWrite(n opcuaserver.Node, v model.Value, _ ua.Variant) ua.StatusCode {
	s.mu.RLock()
	ref, ok := s.points[n.ID.String()]
	s.mu.RUnlock()
	if !ok {
		return ua.StatusBadNodeIDUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	res := s.src.OPCUAWrite(ctx, ref.spec.Device, ref.spec.Point, v)
	if s.metrics != nil {
		result := "ok"
		if !res.OK {
			result = "error"
		}
		s.metrics.Inc("uic_opcua_server_writes_total", obs.Labels{
			"device": ref.spec.Device, "point": ref.spec.Point, "result": result,
		})
	}
	if res.OK {
		return ua.StatusGood
	}
	return writeStatus(res)
}

// writeStatus 把网关的写入错误码映射成 OPC UA 状态码。
//
// 映射的原则是「让客户端能区分该重试还是该改配置」：设备离线是 Bad 且
// 可重试，点位只读/类型不对是永久错误。
func writeStatus(res Outcome) ua.StatusCode {
	switch res.Code {
	case "point.not_found":
		return ua.StatusBadNodeIDUnknown
	case "point.not_writable", "point.disabled":
		return ua.StatusBadNotWritable
	case "value.type_mismatch":
		return ua.StatusBadTypeMismatch
	case "device.not_found", "device.not_ready", "service.not_ready":
		return ua.StatusBadNoCommunication
	}
	// 未分类的错误走 BadInternalError：让客户端知道「这次没成，但不是配置问题」，
	// 而不是猜一个更「合理」的状态码把问题指向错误的方向。
	return ua.StatusBadInternalError
}

// statusOf 把网关质量码映射到 OPC UA 状态码。
//
// stale 用 Uncertain 而不是 Bad：值本身是可信的，只是不再新鲜，源时间戳
// 会如实反映它的年龄。判成 Bad 会让客户端把「设备安静」显示成「设备掉线」。
func statusOf(q model.Quality) ua.StatusCode {
	switch q {
	case model.QualityGood:
		return ua.StatusGood
	case model.QualityUncertain, model.QualityStale:
		return ua.StatusUncertain
	case model.QualityTimeout:
		return ua.StatusBadTimeout
	case model.QualityCommError:
		return ua.StatusBadNoCommunication
	case model.QualityAddressError:
		return ua.StatusBadNodeIDUnknown
	case model.QualityNotConnected, model.QualityDisabled:
		return ua.StatusBadNotConnected
	}
	return ua.StatusBadNoData
}

func pointKey(device, point string) string { return device + "\x00" + point }
