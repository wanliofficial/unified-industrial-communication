// Package snmp 是内置的 SNMP v1/v2c 代理（测试/演示用）。
//
// 它模拟一台带 system 组与两块网卡的设备：GET / GETNEXT / GETBULK / SET
// 的线格式与真实代理一致，community 不匹配时**静默丢弃**（真实代理就是
// 这样，不会回 authorizationError——那条错误只在 v3 的 USM 里出现）。
//
// 与真实代理刻意保留的三个差异：
//
//  1. MIB 只有一张扁平表，没有真正的树结构；GETNEXT 按 OID 字典序找
//     后继，效果与遍历 MIB 树一致。
//  2. sysUpTime 不随墙上时钟增长（固定值）。测试里需要「稳定的值」，
//     一个会自己变的计数器会让断言变成 sleep 赌运气。
//  3. 不做视图/ACL，只做 community 匹配。
package snmp

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"

	snmpcodec "uic/internal/codec/snmp"
)

// DefaultCommunity 是内置代理默认接受的团体名。
const DefaultCommunity = "public"

// entry 是一个 OID 的当前值。
type entry struct {
	value    snmpcodec.Value
	writable bool
}

// Options 是内置代理的可调参数。
type Options struct {
	// Community 为空时使用 public。
	Community string
	// MaxBulk 是 GetBulk 一次最多返回的 varbind 数（默认 64）。
	// 真实代理用 tooBig 兜底；这里直接截断，避免测试用例为「大响应」
	// 准备几十个 OID。
	MaxBulk int
}

// Server 是内置 SNMP 代理。
type Server struct {
	conn      *net.UDPConn
	community string
	maxBulk   int

	mu       sync.Mutex
	table    map[string]entry
	sorted   []snmpcodec.OID
	requests int
	writes   int
	closed   bool
}

// NewServer 在 addr（如 "127.0.0.1:0"）上监听 UDP。
func NewServer(addr string) (*Server, error) {
	return NewServerWithOptions(addr, Options{})
}

// NewServerWithOptions 带参数启动代理。
func NewServerWithOptions(addr string, opt Options) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("snmp sim: resolve %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, fmt.Errorf("snmp sim: listen %q: %w", addr, err)
	}
	community := opt.Community
	if community == "" {
		community = DefaultCommunity
	}
	maxBulk := opt.MaxBulk
	if maxBulk <= 0 {
		maxBulk = 64
	}
	s := &Server{conn: conn, community: community, maxBulk: maxBulk, table: map[string]entry{}}
	s.loadDefaults()
	go s.serve()
	return s, nil
}

// Addr 返回实际监听地址（端口 0 时用得到）。
func (s *Server) Addr() string { return s.conn.LocalAddr().String() }

// Requests 返回收到的合法请求数。
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Writes 返回成功执行的 SET 项数。
func (s *Server) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// Get 读取一个 OID 的当前值。
func (s *Server) Get(oid string) (snmpcodec.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.table[oid]
	return e.value, ok
}

// Set 设置/新增一个 OID 的值（测试夹具用；真实代理只允许通过 SET 改
// 标记为可写的项）。
func (s *Server) Set(oid string, v snmpcodec.Value) error {
	o, err := snmpcodec.ParseOID(oid)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table[oid] = entry{value: v, writable: true}
	s.rebuild(o)
	return nil
}

// SetWritable 调整某个 OID 是否允许 SET（不存在时返回错误）。
func (s *Server) SetWritable(oid string, writable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.table[oid]
	if !ok {
		return fmt.Errorf("snmp sim: OID %s not found", oid)
	}
	e.writable = writable
	s.table[oid] = e
	return nil
}

// Close 停止代理。可重复调用。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}

// rebuild 重建有序 OID 列表。调用方必须持锁。
func (s *Server) rebuild(o snmpcodec.OID) {
	for _, existing := range s.sorted {
		if existing.Compare(o) == 0 {
			return
		}
	}
	s.sorted = append(s.sorted, o.Clone())
	sort.Slice(s.sorted, func(i, j int) bool { return s.sorted[i].Compare(s.sorted[j]) < 0 })
}

func (s *Server) loadDefaults() {
	set := func(oid string, v snmpcodec.Value, writable bool) {
		s.table[oid] = entry{value: v, writable: writable}
		s.sorted = append(s.sorted, snmpcodec.MustParseOID(oid))
	}
	// system 组（RFC 1213 的前几个标量）。
	set("1.3.6.1.2.1.1.1.0", snmpcodec.String("UIC SNMP agent (simulated)"), false)
	set("1.3.6.1.2.1.1.2.0", snmpcodec.OIDValue(snmpcodec.MustParseOID("1.3.6.1.4.1.99999.1")), false)
	set("1.3.6.1.2.1.1.3.0", snmpcodec.Unsigned(snmpcodec.TypeTimeTicks, 123456), false)
	set("1.3.6.1.2.1.1.4.0", snmpcodec.String("ops@example.invalid"), true)
	set("1.3.6.1.2.1.1.5.0", snmpcodec.String("uic-sim"), true)
	set("1.3.6.1.2.1.1.6.0", snmpcodec.String("lab"), true)
	set("1.3.6.1.2.1.1.7.0", snmpcodec.Integer(72), false)
	set("1.3.6.1.2.1.2.1.0", snmpcodec.Integer(2), false)

	// 两块网卡：descr / type / mtu / speed / phys / admin / oper / counters。
	names := []string{"eth0", "eth1"}
	macs := [][]byte{{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}, {0x02, 0x00, 0x00, 0x00, 0x00, 0x02}}
	for i := 0; i < 2; i++ {
		idx := i + 1
		base := "1.3.6.1.2.1.2.2.1"
		set(fmt.Sprintf("%s.1.%d", base, idx), snmpcodec.Integer(int64(idx)), false)
		set(fmt.Sprintf("%s.2.%d", base, idx), snmpcodec.String(names[i]), false)
		set(fmt.Sprintf("%s.3.%d", base, idx), snmpcodec.Integer(6), false) // ethernetCsmacd
		set(fmt.Sprintf("%s.4.%d", base, idx), snmpcodec.Integer(1500), false)
		set(fmt.Sprintf("%s.5.%d", base, idx), snmpcodec.Unsigned(snmpcodec.TypeGauge32, uint64(100000000)), false)
		set(fmt.Sprintf("%s.6.%d", base, idx), snmpcodec.Octets(macs[i]), false)
		set(fmt.Sprintf("%s.7.%d", base, idx), snmpcodec.Integer(1), true)  // ifAdminStatus 可写
		set(fmt.Sprintf("%s.8.%d", base, idx), snmpcodec.Integer(1), false) // ifOperStatus 只读
		set(fmt.Sprintf("%s.10.%d", base, idx), snmpcodec.Unsigned(snmpcodec.TypeCounter32, uint64(1000*idx)), false)
		set(fmt.Sprintf("%s.16.%d", base, idx), snmpcodec.Unsigned(snmpcodec.TypeCounter32, uint64(2000*idx)), false)
	}
	sort.Slice(s.sorted, func(i, j int) bool { return s.sorted[i].Compare(s.sorted[j]) < 0 })
}

func (s *Server) serve() {
	buf := make([]byte, snmpcodec.MaxDatagram)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		// 拷贝一份：buf 会被下一次 ReadFromUDP 覆盖。
		datagram := append([]byte(nil), buf[:n]...)
		if resp := s.handle(datagram); resp != nil {
			_, _ = s.conn.WriteToUDP(resp, from)
		}
	}
}

// handle 处理一条报文；返回 nil 表示静默丢弃。
func (s *Server) handle(datagram []byte) []byte {
	msg, err := snmpcodec.Parse(datagram)
	if err != nil {
		return nil
	}
	// community 不匹配：真实代理不回任何东西（避免成为口令探测的
	// 预言机）。这条语义必须与真实设备一致，否则驱动的「超时 →
	// 检查 community」提示就无法在仿真的设备上复现。
	if msg.Community != s.community {
		return nil
	}
	if !msg.PDU.Type.IsRequest() {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++

	var vbs []snmpcodec.Varbind
	status, index := 0, 0
	switch msg.PDU.Type {
	case snmpcodec.PDUGet:
		vbs, status, index = s.get(msg)
	case snmpcodec.PDUGetNext:
		vbs, status, index = s.getNext(msg)
	case snmpcodec.PDUGetBulk:
		vbs = s.getBulk(msg)
	case snmpcodec.PDUSet:
		vbs, status, index = s.set(msg)
	}
	resp, err := snmpcodec.Build(snmpcodec.Message{
		Version: msg.Version, Community: msg.Community,
		PDU: snmpcodec.PDU{
			Type: snmpcodec.PDUResponse, RequestID: msg.PDU.RequestID,
			ErrorStatus: status, ErrorIndex: index, Varbinds: vbs,
		},
	})
	if err != nil {
		return nil
	}
	return resp
}

func (s *Server) get(msg snmpcodec.Message) ([]snmpcodec.Varbind, int, int) {
	vbs := make([]snmpcodec.Varbind, len(msg.PDU.Varbinds))
	for i, want := range msg.PDU.Varbinds {
		e, ok := s.table[want.OID.String()]
		if !ok {
			if msg.Version == snmpcodec.V1 {
				return msg.PDU.Varbinds, int(snmpcodec.ErrNoSuchName), i + 1
			}
			vbs[i] = snmpcodec.Varbind{OID: want.OID, Value: snmpcodec.Exception(snmpcodec.TypeNoSuchObject)}
			continue
		}
		vbs[i] = snmpcodec.Varbind{OID: want.OID, Value: e.value}
	}
	return vbs, 0, 0
}

// successor 返回严格大于 oid 的下一个 OID。调用方必须持锁。
func (s *Server) successor(oid snmpcodec.OID) (snmpcodec.OID, snmpcodec.Value, bool) {
	i := sort.Search(len(s.sorted), func(i int) bool { return s.sorted[i].Compare(oid) > 0 })
	if i >= len(s.sorted) {
		return nil, snmpcodec.Value{}, false
	}
	next := s.sorted[i]
	return next, s.table[next.String()].value, true
}

func (s *Server) getNext(msg snmpcodec.Message) ([]snmpcodec.Varbind, int, int) {
	vbs := make([]snmpcodec.Varbind, len(msg.PDU.Varbinds))
	for i, want := range msg.PDU.Varbinds {
		next, v, ok := s.successor(want.OID)
		if !ok {
			if msg.Version == snmpcodec.V1 {
				return msg.PDU.Varbinds, int(snmpcodec.ErrNoSuchName), i + 1
			}
			vbs[i] = snmpcodec.Varbind{OID: want.OID, Value: snmpcodec.Exception(snmpcodec.TypeEndOfMibView)}
			continue
		}
		vbs[i] = snmpcodec.Varbind{OID: next, Value: v}
	}
	return vbs, 0, 0
}

// getBulk 按 v2c 语义：前 non-repeaters 个各取一个后继，其余每个
// 复制 max-repetitions 次、每次从上一个结果继续向后走。
func (s *Server) getBulk(msg snmpcodec.Message) []snmpcodec.Varbind {
	nr := msg.PDU.NonRepeaters
	if nr < 0 {
		nr = 0
	}
	if nr > len(msg.PDU.Varbinds) {
		nr = len(msg.PDU.Varbinds)
	}
	mr := msg.PDU.MaxRepetitions
	if mr < 0 {
		mr = 0
	}

	var out []snmpcodec.Varbind
	for _, want := range msg.PDU.Varbinds[:nr] {
		next, v, ok := s.successor(want.OID)
		if !ok {
			out = append(out, snmpcodec.Varbind{OID: want.OID, Value: snmpcodec.Exception(snmpcodec.TypeEndOfMibView)})
			continue
		}
		out = append(out, snmpcodec.Varbind{OID: next, Value: v})
	}
	cursors := make([]snmpcodec.OID, len(msg.PDU.Varbinds)-nr)
	for i, want := range msg.PDU.Varbinds[nr:] {
		cursors[i] = want.OID
	}
	for rep := 0; rep < mr; rep++ {
		if len(out) >= s.maxBulk {
			break
		}
		for i := range cursors {
			next, v, ok := s.successor(cursors[i])
			if !ok {
				out = append(out, snmpcodec.Varbind{OID: cursors[i], Value: snmpcodec.Exception(snmpcodec.TypeEndOfMibView)})
				continue
			}
			out = append(out, snmpcodec.Varbind{OID: next, Value: v})
			cursors[i] = next
			if len(out) >= s.maxBulk {
				break
			}
		}
	}
	return out
}

func (s *Server) set(msg snmpcodec.Message) ([]snmpcodec.Varbind, int, int) {
	// 先全部校验再落地：SNMP 的 SET 是原子的——第二个 varbind 不合法
	// 时第一个也不能生效。
	next := make([]entry, len(msg.PDU.Varbinds))
	for i, vb := range msg.PDU.Varbinds {
		e, ok := s.table[vb.OID.String()]
		if !ok {
			return msg.PDU.Varbinds, s.setErr(msg.Version, snmpcodec.ErrNoSuchName, snmpcodec.ErrNoCreation), i + 1
		}
		if !e.writable {
			return msg.PDU.Varbinds, s.setErr(msg.Version, snmpcodec.ErrReadOnly, snmpcodec.ErrNotWritable), i + 1
		}
		if e.value.Type != vb.Value.Type {
			return msg.PDU.Varbinds, s.setErr(msg.Version, snmpcodec.ErrBadValue, snmpcodec.ErrWrongType), i + 1
		}
		e.value = vb.Value
		next[i] = e
	}
	for i, vb := range msg.PDU.Varbinds {
		s.table[vb.OID.String()] = next[i]
		s.writes++
	}
	return msg.PDU.Varbinds, 0, 0
}

func (s *Server) setErr(version snmpcodec.Version, v1, v2 snmpcodec.ErrorStatus) int {
	if version == snmpcodec.V1 {
		return int(v1)
	}
	return int(v2)
}
