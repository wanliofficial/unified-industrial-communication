// Package s7（sim）实现一个最小 S7comm 从站，用于自测、演示与驱动一致性
// 测试。它复用 codec/s7 的编解码函数，避免「主站与从站各写一套」导致
// 的语义漂移——尤其是长度单位（位 / 字节）这种最容易分叉的地方。
//
// 边界：实现连接、Setup、Read Var、Write Var，以及块目录/块属性（USER_DATA）
// 与块上传（Start/Upload/End）。不含报警、下载、符号表、时间同步。它存在
// 的目的是让主站在没有真实 PLC 的机器上也能跑通完整链路，而不是模拟一台
// 真 CPU——因此它不生成真实的 36 字节块头，也不模拟「块不存在」的错误码。
package s7

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"uic/internal/codec/s7"
)

// Server 是一个内存区域模型的 S7 从站。
type Server struct {
	mu       sync.RWMutex
	ln       net.Listener
	pduLen   int
	mem      map[memKey][]byte
	blocks   map[blockKey][]byte
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   bool
	reqCount int
	uploadID byte
}

type memKey struct {
	area byte
	db   int
}

// blockKey 标识一个可上传的块。
type blockKey struct {
	typ byte
	num int
}

// connState 是每条连接的块上传会话。会话 id 只在连接内唯一：PLC 释放
// 连接时所有未完成的上传一并作废，跨连接复用 id 没有意义。
type connState struct {
	uploads map[byte]*uploadState
}

type uploadState struct {
	data []byte
	off  int
}

// NewServer 创建从站；addr 为 "127.0.0.1:0" 时自动分配端口。
// pduLen 是 Setup 阶段返回给主站的协商值（常见 240 / 480 / 960）。
func NewServer(addr string, pduLen int) (*Server, error) {
	if pduLen <= 0 {
		pduLen = 480
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:     ln,
		pduLen: pduLen,
		mem:    map[memKey][]byte{},
		blocks: map[blockKey][]byte{},
		conns:  map[net.Conn]struct{}{},
	}
	// 默认给 M / I / Q / DB1 各 1KB，避免测试每次都要先扩容。
	s.Ensure(s7.AreaMerker, 0, 1024)
	s.Ensure(s7.AreaInput, 0, 1024)
	s.Ensure(s7.AreaOutput, 0, 1024)
	s.Ensure(s7.AreaDB, 1, 1024)
	// 给块目录一个非空起点：OB1 存在但内容为空。DB 块直接从内存区域
	// 反向暴露，因此 DB1 自动可见，且与读写到的内容始终一致。
	s.blocks[blockKey{typ: s7.BlockOB, num: 1}] = nil

	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// PDULen 返回协商的 PDU 长度。
func (s *Server) PDULen() int { return s.pduLen }

// Close 停止监听并等待连接退出。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// Ensure 把某个区域的长度至少扩到 size。
func (s *Server) Ensure(area byte, db, size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memKey{area: area, db: db}
	if len(s.mem[k]) >= size {
		return
	}
	buf := make([]byte, size)
	copy(buf, s.mem[k])
	s.mem[k] = buf
}

// SetBytes 写入一段字节（自动扩容）。
func (s *Server) SetBytes(area byte, db, offset int, data []byte) {
	if offset < 0 {
		return
	}
	s.Ensure(area, db, offset+len(data))
	s.mu.Lock()
	defer s.mu.Unlock()
	copy(s.mem[memKey{area: area, db: db}][offset:], data)
}

// Bytes 读出一段字节。
func (s *Server) Bytes(area byte, db, offset, n int) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.mem[memKey{area: area, db: db}]
	if offset < 0 || offset+n > len(buf) {
		return nil
	}
	return append([]byte(nil), buf[offset:offset+n]...)
}

// SetBit 设置某字节的某一位。
func (s *Server) SetBit(area byte, db, offset, bit int, on bool) {
	if bit < 0 || bit > 7 {
		return
	}
	s.Ensure(area, db, offset+1)
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.mem[memKey{area: area, db: db}]
	mask := byte(1) << uint(bit)
	if on {
		buf[offset] |= mask
	} else {
		buf[offset] &^= mask
	}
}

// Bit 读取某字节的某一位。
func (s *Server) Bit(area byte, db, offset, bit int) bool {
	b := s.Bytes(area, db, offset, 1)
	if len(b) == 0 || bit < 0 || bit > 7 {
		return false
	}
	return b[0]&(1<<uint(bit)) != 0
}

// SetBlock 注册或覆盖一个可上传的块（内容为裸负载，不含块头）。
func (s *Server) SetBlock(typ byte, num int, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[blockKey{typ: typ, num: num}] = append([]byte(nil), data...)
}

// blockData 返回块内容。显式注册的块优先；DB 块没注册时直接从内存区域
// 取，这样「读点位」与「上传块」看到的是同一份数据。
func (s *Server) blockData(typ byte, num int) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.blocks[blockKey{typ: typ, num: num}]; ok {
		return append([]byte(nil), b...), true
	}
	if typ == s7.BlockDB && num != 0 {
		if buf, ok := s.mem[memKey{area: s7.AreaDB, db: num}]; ok {
			return append([]byte(nil), buf...), true
		}
	}
	return nil, false
}

// blockNumbers 返回某类型的所有块号（升序）。
func (s *Server) blockNumbers(typ byte) []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[int]bool{}
	for k := range s.blocks {
		if k.typ == typ {
			seen[k.num] = true
		}
	}
	if typ == s7.BlockDB {
		for k := range s.mem {
			if k.area == s7.AreaDB && k.db != 0 {
				seen[k.db] = true
			}
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// RequestCount 返回累计收到的 S7 业务请求数（不含握手）。
// 用于断言「多个点位被合并进一次事务」。
func (s *Server) RequestCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reqCount
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			_ = c.Close()
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
	st := &connState{uploads: map[byte]*uploadState{}}
	for {
		frame, err := readTPKT(c)
		if err != nil {
			return
		}
		payload, err := s7.ParseTPKT(frame)
		if err != nil || len(payload) < 2 {
			return
		}
		switch payload[1] {
		case s7.COTPCR:
			if _, err := c.Write(ccFrame()); err != nil {
				return
			}
		case s7.COTPDT:
			if len(payload) < 3 {
				return
			}
			resp, ok := s.handlePDU(payload[3:], st)
			if !ok {
				return
			}
			if _, err := c.Write(s7.WrapDT(resp)); err != nil {
				return
			}
		default:
			return
		}
	}
}

func readTPKT(c net.Conn) ([]byte, error) {
	head := make([]byte, 4)
	if _, err := readFull(c, head); err != nil {
		return nil, err
	}
	if head[0] != s7.TPKTVersion {
		return nil, fmt.Errorf("sim-s7: bad tpkt version %d", head[0])
	}
	n := int(binary.BigEndian.Uint16(head[2:4]))
	if n < 4 || n > 8192 {
		return nil, fmt.Errorf("sim-s7: bad tpkt length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		if err != nil {
			return got, err
		}
		got += n
	}
	return got, nil
}

// ccFrame 是连接确认。refs 与实际连接无关，只要类型是 CC 主站就会接受。
func ccFrame() []byte {
	payload := []byte{
		0x12, s7.COTPCC,
		0x00, 0x01, 0x00, 0x01, // refs
		0x00,
		0xC0, 0x02, 0x01, 0x02,
		0xC1, 0x02, 0x01, 0x00,
		0xC2, 0x02, 0x01, 0x00,
	}
	return s7.TPKT(payload)
}

func (s *Server) handlePDU(raw []byte, st *connState) ([]byte, bool) {
	if len(raw) < 10 || raw[0] != s7.S7ProtoID {
		return nil, false
	}
	ref := binary.BigEndian.Uint16(raw[4:6])
	paramLen := int(binary.BigEndian.Uint16(raw[6:8]))
	dataLen := int(binary.BigEndian.Uint16(raw[8:10]))
	if len(raw) < 10+paramLen+dataLen {
		return nil, false
	}
	param := raw[10 : 10+paramLen]
	data := raw[10+paramLen : 10+paramLen+dataLen]
	if len(param) < 1 {
		return nil, false
	}

	// USER_DATA 的参数区第 0 字节是保留位（0x00），不是功能码，因此
	// 必须先按 ROSCTR 分流，否则会被下面的 switch 当成未知功能拒绝。
	if raw[1] == s7.ROSCTRUserData {
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleUserData(ref, param, data), true
	}
	if raw[1] != s7.ROSCTRJob {
		return nil, false
	}

	switch param[0] {
	case s7.FuncSetup:
		// 回显对端请求的长度，但不超过本机配置：这正是协商的含义。
		want := s.pduLen
		if len(param) >= 8 {
			if n := int(binary.BigEndian.Uint16(param[6:8])); n < want {
				want = n
			}
		}
		p := []byte{s7.FuncSetup, 0x00, 0x00, 0x01, 0x00, 0x01,
			byte(want >> 8), byte(want)}
		return s7.BuildPDU(s7.ROSCTRAckData, ref, p, nil), true

	case s7.FuncRead:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleRead(ref, param), true

	case s7.FuncWrite:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleWrite(ref, param, data), true

	case s7.FuncStartUpload:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleStartUpload(ref, param, st)

	case s7.FuncUpload:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleUpload(ref, param, st)

	case s7.FuncEndUpload:
		s.mu.Lock()
		s.reqCount++
		s.mu.Unlock()
		return s.handleEndUpload(ref, param, st)
	}
	return nil, false
}

func (s *Server) handleRead(ref uint16, param []byte) []byte {
	if len(param) < 2 {
		return s7.BuildPDU(s7.ROSCTRAckData, ref, []byte{s7.FuncRead, 0}, nil)
	}
	count := int(param[1])
	outParam := []byte{s7.FuncRead, byte(count)}
	var outData []byte
	for i := 0; i < count; i++ {
		off := 2 + i*12
		if off+12 > len(param) {
			outData = append(outData, 0x05, 0x00, 0x00, 0x00)
			continue
		}
		it, err := parseItem(param[off : off+12])
		if err != nil {
			outData = append(outData, 0x05, 0x00, 0x00, 0x00)
			continue
		}
		buf, code := s.readItem(it)
		if code != s7.CodeOK {
			outData = append(outData, code, 0x00, 0x00, 0x00)
			continue
		}
		ts := byte(s7.RSByteWord)
		length := len(buf) * 8
		if it.BitAccess {
			ts = s7.RSBit
			length = 1
		}
		outData = append(outData, s7.CodeOK, ts,
			byte(length>>8), byte(length))
		outData = append(outData, buf...)
		if len(buf)%2 == 1 {
			outData = append(outData, 0x00)
		}
	}
	return s7.BuildPDU(s7.ROSCTRAckData, ref, outParam, outData)
}

func (s *Server) handleWrite(ref uint16, param, data []byte) []byte {
	if len(param) < 2 {
		return s7.BuildPDU(s7.ROSCTRAckData, ref, []byte{s7.FuncWrite, 0}, nil)
	}
	count := int(param[1])
	outParam := []byte{s7.FuncWrite, byte(count)}
	var outData []byte
	off := 0
	for i := 0; i < count; i++ {
		p := 2 + i*12
		d := off
		if p+12 > len(param) || d+4 > len(data) {
			outData = append(outData, 0x05)
			continue
		}
		it, err := parseItem(param[p : p+12])
		if err != nil {
			outData = append(outData, 0x05)
			continue
		}
		ts := data[d+1]
		length := int(binary.BigEndian.Uint16(data[d+2 : d+4]))
		payload := payloadBytes(ts, length)
		d += 4
		if d+payload > len(data) {
			outData = append(outData, 0x05)
			continue
		}
		code := s.writeItem(it, data[d:d+payload])
		outData = append(outData, code)
		off = d + payload
		if payload%2 == 1 {
			off++
		}
	}
	return s7.BuildPDU(s7.ROSCTRAckData, ref, outParam, outData)
}

// ---------------------------------------------------------------------------
// USER_DATA：块目录与块属性
// ---------------------------------------------------------------------------

// handleUserData 响应块目录查询。模拟器不模拟「块不存在」的错误码：
// 未知块返回一份全零的属性，让主站侧的解析路径仍然完整跑一遍。
func (s *Server) handleUserData(ref uint16, param, data []byte) []byte {
	if len(param) != 8 {
		return s7.BuildUserDataResponse(ref, s7.UserDataGroupBlocks, 0, false, nil)
	}
	group := param[5] & 0x0F
	sub := param[6]
	switch sub {
	case s7.UserDataListAll:
		// ListAll 的数据区是 `0A 00 0000`，不是 `FF 09`，不要走通用解析。
		return s7.BuildUserDataResponse(ref, group, sub, false, s.blockCounts())

	case s7.UserDataListBlocksOfType:
		payload, ok := userDataPayload(data)
		if !ok || len(payload) < 2 || payload[0] != 0x30 {
			return s7.BuildUserDataResponse(ref, group, sub, false, nil)
		}
		return s7.BuildUserDataResponse(ref, group, sub, false, encodeBlockNumbers(s.blockNumbers(payload[1])))

	case s7.UserDataBlockInfo:
		payload, ok := userDataPayload(data)
		if !ok || len(payload) < 8 || payload[0] != 0x30 {
			return s7.BuildUserDataResponse(ref, group, sub, false, nil)
		}
		typ := payload[1]
		num, err := strconv.Atoi(string(payload[2:7]))
		if err != nil {
			return s7.BuildUserDataResponse(ref, group, sub, false, nil)
		}
		body, _ := s.blockData(typ, num)
		return s7.BuildUserDataResponse(ref, group, sub, false, blockInfoPayload(typ, num, body))
	}
	return s7.BuildUserDataResponse(ref, group, sub, false, nil)
}

// userDataPayload 剥离 `FF 09 + 长度` 数据头。
func userDataPayload(data []byte) ([]byte, bool) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0x09 {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(data[2:4]))
	if 4+n > len(data) {
		return nil, false
	}
	return data[4 : 4+n], true
}

// blockCounts 生成 28 字节的数量表。顺序固定为 OB/DB/SDB/FC/SFC/FB/SFB，
// 主站不必依赖顺序，但固定顺序能让抓包对比变得容易。
func (s *Server) blockCounts() []byte {
	order := []byte{s7.BlockOB, s7.BlockDB, s7.BlockSDB, s7.BlockFC, s7.BlockSFC, s7.BlockFB, s7.BlockSFB}
	out := make([]byte, 0, 28)
	for _, t := range order {
		n := len(s.blockNumbers(t))
		out = append(out, 0x30, t, byte(n>>8), byte(n))
	}
	return out
}

// encodeBlockNumbers 把块号列表编码成每项 4 字节（块号 + 语言/保留）。
func encodeBlockNumbers(nums []int) []byte {
	out := make([]byte, 0, len(nums)*4)
	for _, n := range nums {
		out = append(out, byte(n>>8), byte(n), 0x00, 0x01)
	}
	return out
}

// blockInfoPayload 生成 78 字节的块属性。字段偏移与 codec 的解析器一致；
// 模拟器只填有意义的字段，日期与校验和留零。
func blockInfoPayload(typ byte, num int, data []byte) []byte {
	p := make([]byte, 78)
	p[0] = 0x70
	p[1] = typ
	p[2] = 0x01
	p[3] = 0x01
	p[9] = 0x01 // 有效块
	if typ == s7.BlockDB {
		p[10] = 0x05
	} else {
		p[10] = 0x01
	}
	binary.BigEndian.PutUint16(p[12:14], uint16(num))
	binary.BigEndian.PutUint32(p[14:18], uint32(len(data)))
	mc7 := len(data)
	if mc7 > 0xFFFF {
		mc7 = 0xFFFF
	}
	binary.BigEndian.PutUint16(p[40:42], uint16(mc7))
	copy(p[42:50], "UIC-SIM")
	copy(p[50:58], "SIM")
	copy(p[58:66], "SIM")
	p[66] = 1
	return p
}

// ---------------------------------------------------------------------------
// 块上传：Start → Upload × N → End
// ---------------------------------------------------------------------------

func (s *Server) handleStartUpload(ref uint16, param []byte, st *connState) ([]byte, bool) {
	if len(param) != 18 || param[0] != s7.FuncStartUpload || param[8] != 9 {
		return nil, false
	}
	addr := param[9:]
	if len(addr) != 9 || addr[0] != '_' || addr[1] != '0' || addr[8] != 'A' {
		return nil, false
	}
	num, err := strconv.Atoi(string(addr[3:8]))
	if err != nil {
		return nil, false
	}
	body, ok := s.blockData(addr[2], num)
	if !ok {
		// 模拟器把未知块当成空块：真实 PLC 会返回「对象不存在」，但这里
		// 的目标是让主站的完整流程可跑，而不是复刻错误码。
		body = []byte{}
	}

	s.mu.Lock()
	s.uploadID++
	id := s.uploadID
	s.mu.Unlock()
	st.uploads[id] = &uploadState{data: body}

	p := make([]byte, 16)
	p[0] = s7.FuncStartUpload
	p[7] = id
	copy(p[11:16], fmt.Sprintf("%05d", len(body)))
	return s7.BuildPDU(s7.ROSCTRAckData, ref, p, nil), true
}

func (s *Server) handleUpload(ref uint16, param []byte, st *connState) ([]byte, bool) {
	if len(param) != 8 || param[0] != s7.FuncUpload {
		return nil, false
	}
	up, ok := st.uploads[param[7]]
	if !ok {
		return nil, false
	}
	// 分片大小由协商 PDU 推出：12 字节 ACK_DATA 头 + 2 字节参数 +
	// 4 字节数据头（长度 + FB 00）。
	chunk := s.pduLen - 18
	if chunk < 1 {
		chunk = 1
	}
	remain := len(up.data) - up.off
	n := remain
	last := true
	if remain > chunk {
		n = chunk
		last = false
	}
	payload := up.data[up.off : up.off+n]
	up.off += n

	flag := byte(0)
	if !last {
		flag = 1
	}
	data := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint16(data[0:2], uint16(len(payload)))
	binary.BigEndian.PutUint16(data[2:4], 0x00FB)
	copy(data[4:], payload)
	return s7.BuildPDU(s7.ROSCTRAckData, ref, []byte{s7.FuncUpload, flag}, data), true
}

func (s *Server) handleEndUpload(ref uint16, param []byte, st *connState) ([]byte, bool) {
	if len(param) != 8 || param[0] != s7.FuncEndUpload {
		return nil, false
	}
	if _, ok := st.uploads[param[7]]; !ok {
		return nil, false
	}
	delete(st.uploads, param[7])
	return s7.BuildPDU(s7.ROSCTRAckData, ref, []byte{s7.FuncEndUpload}, nil), true
}

// parseItem 解析 S7ANY 变量说明。主站只会发 0x12 0x0A 0x10 这一种。
func parseItem(b []byte) (s7.Item, error) {
	if len(b) < 12 || b[0] != 0x12 || b[2] != 0x10 {
		return s7.Item{}, fmt.Errorf("sim-s7: unsupported item spec % x", b)
	}
	ts := b[3]
	length := int(binary.BigEndian.Uint16(b[4:6]))
	db := int(binary.BigEndian.Uint16(b[6:8]))
	area := b[8]
	addr := int(b[9])<<16 | int(b[10])<<8 | int(b[11])
	it := s7.Item{Area: area, DB: db, ByteAddr: addr / 8, Bit: -1}
	if ts == s7.TSBit {
		it.BitAccess = true
		it.Bit = addr % 8
	} else {
		it.Bytes = length
	}
	return it, nil
}

// payloadBytes 与 codec 侧保持一致：BIT/BYTE/WORD/DWORD 按位，其余按字节。
func payloadBytes(ts byte, length int) int {
	switch ts {
	case s7.RSBit, s7.RSByteWord:
		return (length + 7) / 8
	}
	return length
}

func (s *Server) readItem(it s7.Item) ([]byte, byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf, ok := s.mem[memKey{area: it.Area, db: it.DB}]
	if !ok {
		return nil, s7.CodeObjectNotFound
	}
	if it.ByteAddr < 0 || it.ByteAddr >= len(buf) {
		return nil, s7.CodeInvalidAddr
	}
	if it.BitAccess {
		if it.Bit < 0 || it.Bit > 7 {
			return nil, s7.CodeInvalidAddr
		}
		return []byte{buf[it.ByteAddr] >> uint(it.Bit) & 1}, s7.CodeOK
	}
	if it.Bytes <= 0 {
		return nil, s7.CodeInvalidAddr
	}
	if it.ByteAddr+it.Bytes > len(buf) {
		return nil, s7.CodeInvalidAddr
	}
	return append([]byte(nil), buf[it.ByteAddr:it.ByteAddr+it.Bytes]...), s7.CodeOK
}

func (s *Server) writeItem(it s7.Item, v []byte) byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memKey{area: it.Area, db: it.DB}
	// 输入区是只读的：允许写会让主站误以为写成功了，实际 PLC 上永远
	// 不会生效，是最难查的一类「静默失败」。
	if it.Area == s7.AreaInput {
		return s7.CodeAccessDenied
	}
	buf, ok := s.mem[k]
	if !ok {
		return s7.CodeObjectNotFound
	}
	if it.ByteAddr < 0 || it.ByteAddr >= len(buf) {
		return s7.CodeInvalidAddr
	}
	if it.BitAccess {
		if it.Bit < 0 || it.Bit > 7 || len(v) != 1 {
			return s7.CodeInvalidAddr
		}
		mask := byte(1) << uint(it.Bit)
		if v[0]&1 != 0 {
			buf[it.ByteAddr] |= mask
		} else {
			buf[it.ByteAddr] &^= mask
		}
		return s7.CodeOK
	}
	if it.Bytes <= 0 || len(v) != it.Bytes {
		return s7.CodeInvalidAddr
	}
	if it.ByteAddr+it.Bytes > len(buf) {
		return s7.CodeInvalidAddr
	}
	copy(buf[it.ByteAddr:], v)
	return s7.CodeOK
}
