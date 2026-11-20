package mewtocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"uic/internal/codec/mewtocol"
	"uic/internal/driver"
	"uic/internal/model"
)

// 纯函数辅助的单元测试：它们决定读多少字、按什么字节序解、错误归到哪个
// 质量位，出错时不会有任何 I/O 迹象，只能靠这些断言钉住。

func TestPointWords(t *testing.T) {
	cases := []struct {
		name string
		p    driver.PointRef
		want int
	}{
		{"bool is one word", driver.PointRef{DataType: model.TypeBool, Length: 1}, 1},
		{"bool ignores length", driver.PointRef{DataType: model.TypeBool, Length: 5}, 1},
		{"explicit length wins", driver.PointRef{DataType: model.TypeUint16, Length: 3}, 3},
		{"falls back to type width", driver.PointRef{DataType: model.TypeUint32, Length: 0}, 2},
		{"unknown type defaults to one", driver.PointRef{DataType: model.TypeUint16, Length: 0}, 1},
	}
	for _, c := range cases {
		if got := pointWords(c.p); got != c.want {
			t.Errorf("%s: pointWords = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestReqWords(t *testing.T) {
	cases := []struct {
		name string
		r    driver.WriteRequest
		want int
	}{
		{"explicit length", driver.WriteRequest{Address: model.Address{Length: 4}, DataType: model.TypeUint16}, 4},
		{"type width when no length", driver.WriteRequest{Address: model.Address{Length: 0}, DataType: model.TypeFloat64}, 4},
		{"default one", driver.WriteRequest{Address: model.Address{Length: 0}, DataType: model.TypeUint16}, 1},
	}
	for _, c := range cases {
		if got := reqWords(c.r); got != c.want {
			t.Errorf("%s: reqWords = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestOrderFor(t *testing.T) {
	// 未显式配置时按字节数选默认：>=8 字用 DCBA，否则 CDAB；显式配置覆盖。
	if got := orderFor(4, ""); got != model.OrderCDAB {
		t.Errorf("orderFor(4,\"\") = %v, want CDAB", got)
	}
	if got := orderFor(8, ""); got != model.OrderDCBA {
		t.Errorf("orderFor(8,\"\") = %v, want DCBA", got)
	}
	if got := orderFor(2, model.OrderABCD); got != model.OrderABCD {
		t.Errorf("explicit order should win: %v", got)
	}
}

// 错误分类：PLC 回 66H（地址错）必须落到地址类错误 + 地址质量位，
// 65H（写保护）落到权限类。分类错了，采集层会一遍遍重试一个永远写不进的点。
func TestClassify(t *testing.T) {
	cn := &conn{devID: "dev1"}
	cases := []struct {
		code    byte
		wantErr driver.ErrorKind
	}{
		{0x66, driver.ErrAddress},
		{0x65, driver.ErrPermission},
		{0x41, driver.ErrProtocol}, // 格式错：重发有意义
		{0x53, driver.ErrProtocol}, // busy：可重试
		{0x60, driver.ErrAddress},  // 参数错：区码/单元码不受支持，配置问题
	}
	for _, tc := range cases {
		ec := &mewtocol.ErrorCode{Code: tc.code}
		err := cn.classify("write", ec)
		if got := driver.KindOf(err); got != tc.wantErr {
			t.Errorf("%02XH: kind = %v, want %v", tc.code, got, tc.wantErr)
		}
	}
}

func TestQualityOf(t *testing.T) {
	if q := qualityOf(&mewtocol.ErrorCode{Code: 0x66}); q != model.QualityAddressError {
		t.Errorf("0x66 quality = %v, want address error", q)
	}
	if q := qualityOf(&mewtocol.ErrorCode{Code: 0x22}); q != model.QualityCommError {
		t.Errorf("0x22 quality = %v, want comm error", q)
	}
	if q := qualityOf(&mewtocol.ErrorCode{Code: 0x41}); q != model.QualityCommError {
		t.Errorf("0x41 quality = %v, want comm error (retryable format error)", q)
	}
	if q := qualityOf(driver.E(driver.ErrTimeout, "read", "d", "", true, nil)); q != model.QualityTimeout {
		t.Errorf("timeout quality = %v, want timeout", q)
	}
	// 普通协议错（非 ErrorCode）落到 Bad。
	if q := qualityOf(errors.New("generic")); q != model.QualityBad {
		t.Errorf("generic quality = %v, want bad", q)
	}
}

// 质量位必须与错误码自身的 Kind()/Retryable() 一致：手抄的链路码白名单曾
// 漏掉 0x29/2A/2B/2C/2D/2E/2F/31/34/35/37 共 11 个码，把可重试的链路故障
// 显示成 bad。这里遍历整段链路码与全部协议/处理码，逐码钉住派生关系。
func TestQualityOfDerivesFromErrorCode(t *testing.T) {
	// 链路类：0x22..0x38 整段都应是 comm_error（含被旧白名单漏掉的那些）。
	for code := 0x22; code <= 0x38; code++ {
		ec := &mewtocol.ErrorCode{Code: byte(code)}
		if got := qualityOf(ec); got != model.QualityCommError {
			t.Errorf("%02XH (kind=%v): quality = %v, want comm error", code, ec.Kind(), got)
		}
	}
	// 可重试的协议/处理类：同样是通信问题，得 comm_error。
	for _, code := range []byte{0x40, 0x41, 0x42, 0x43, 0x50, 0x51, 0x52, 0x53} {
		ec := &mewtocol.ErrorCode{Code: code}
		if !ec.Retryable() {
			t.Fatalf("%02XH 应是可重试的，测试前提有误", code)
		}
		if got := qualityOf(ec); got != model.QualityCommError {
			t.Errorf("%02XH (retryable=%v): quality = %v, want comm error", code, ec.Retryable(), got)
		}
	}
	// 地址类固定 address_error，不受派生规则影响。
	for _, code := range []byte{0x60, 0x61, 0x66, 0x67} {
		if got := qualityOf(&mewtocol.ErrorCode{Code: code}); got != model.QualityAddressError {
			t.Errorf("%02XH: quality = %v, want address error", code, got)
		}
	}
	// 不可重试的应用类：配置/模式问题，落 bad。
	for _, code := range []byte{0x62, 0x63, 0x64, 0x65, 0x68} {
		ec := &mewtocol.ErrorCode{Code: code}
		if ec.Retryable() {
			t.Fatalf("%02XH 不应是可重试的，测试前提有误", code)
		}
		if got := qualityOf(ec); got != model.QualityBad {
			t.Errorf("%02XH: quality = %v, want bad", code, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 批量写：逐项返回
// ---------------------------------------------------------------------------

// scriptLink 是一个最小 link 实现：Write 记录请求并按「回显命令码」现场
// 生成一条正常完成应答。用它把「批量写怎么逐项汇报」与真实收发分开。
type scriptLink struct {
	requests [][]byte
	reply    []byte
	pos      int
	closed   bool
}

func (l *scriptLink) Write(p []byte) error {
	l.requests = append(l.requests, append([]byte(nil), p...))
	// 应答：`%` + 站号 + `$` + 回显码 + BCC + CR。回显码是请求命令码的前
	// 两个字符（例如 WD… 回显 "WD"），与从站实现同一套规则。
	echo := mewtocol.EchoCode(string(p[4:6]))
	l.reply = mewtocol.AppendBCC([]byte("%01$" + echo))
	l.pos = 0
	return nil
}

func (l *scriptLink) ReadFull(n int) ([]byte, error) {
	if l.pos+n > len(l.reply) {
		return nil, errors.New("script: reply exhausted")
	}
	out := l.reply[l.pos : l.pos+n]
	l.pos += n
	return out, nil
}

func (l *scriptLink) Close() error                { l.closed = true; return nil }
func (l *scriptLink) SetDeadline(time.Time) error { return nil }
func (l *scriptLink) Drain() error                { return nil }
func (l *scriptLink) Lock()                       {}
func (l *scriptLink) Unlock()                     {}

// TestWriteFailureKeepsEarlierSuccessesIntact：批量写里后面的条目失败，不能把
// **已经成功**的条目也标上错误（docs §3.4「逐项返回结果」）。
//
// 早先这里是「把 firstErr 回填进所有 Err==nil 的结果」，后果是调用方会去
// 重发一个已经写下去的点位——对计数类点位就是重复累加。
func TestWriteFailureKeepsEarlierSuccessesIntact(t *testing.T) {
	l := &scriptLink{}
	c := &conn{link: l, station: 1, devID: "plc1", timeout: time.Second}

	res, err := c.Write(context.Background(), []driver.WriteRequest{
		{
			PointID:  "ok",
			Address:  model.Address{Kind: model.AddressRegister, Area: "dt", Offset: 0, Bit: -1},
			DataType: model.TypeUint16,
			Value:    model.NumValue(model.TypeUint16, 0x1234),
		},
		{
			// 未知区名：在发帧之前就判地址错，不会再消耗一帧应答。
			PointID:  "bad",
			Address:  model.Address{Kind: model.AddressRegister, Area: "no-such-area", Offset: 0, Bit: -1},
			DataType: model.TypeUint16,
			Value:    model.NumValue(model.TypeUint16, 1),
		},
	})
	if err == nil {
		t.Fatal("a batch with a failing item must return an error")
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if !res[0].OK || res[0].Err != nil || res[0].Quality != model.QualityGood {
		t.Fatalf("first item succeeded but was reported as %+v", res[0])
	}
	if res[1].OK || res[1].Err == nil {
		t.Fatalf("second item failed but was reported as %+v", res[1])
	}
	if res[1].Quality == "" {
		t.Fatal("a failed write must carry a quality, not the empty string")
	}
	if len(l.requests) != 1 {
		t.Fatalf("frames = %d, want 1 (the bad item must not touch the wire)", len(l.requests))
	}
}

// ---------------------------------------------------------------------------
// 多帧 span：切帧必须按点位边界、拼接后统一解码
// ---------------------------------------------------------------------------

// timeoutErr 让 link 的读错误落进 isTimeout 分支（→ ErrTimeout，链路级）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// spanLink 是一条会「按请求里的起止地址」回真实数据的假链路，并能让第 N
// 个事务失败。用真实帧而不是固定 byte 串，是为了同时验证切帧边界真的落在
// 协议允许的位置上。
type spanLink struct {
	words    map[int]uint16
	failAt   int // 从 0 开始的事务序号；-1 表示不失败
	n        int
	requests []string
	reply    []byte
	pos      int
}

func (l *spanLink) Write(p []byte) error {
	l.requests = append(l.requests, string(p))
	idx := l.n
	l.n++
	if idx == l.failAt {
		l.reply, l.pos = nil, 0
		return nil
	}
	// 用协议自己的解析器取出起止地址：手写下标容易把区码也算进去，
	// 而那样得到的错误是「write failed」，与要验证的东西无关。
	req, err := mewtocol.ParseRequest(p)
	if err != nil {
		return err
	}
	if req.Cmd != mewtocol.CmdReadData || req.Area.Key != "dt" {
		return fmt.Errorf("spanLink only answers RD on dt, got %s on %s", req.Cmd, req.Area.Key)
	}
	var sb strings.Builder
	sb.WriteString("%01$RD")
	for a := req.Start; a <= req.End; a++ {
		v := l.words[a]
		sb.WriteString(fmt.Sprintf("%02X%02X", v&0xFF, v>>8))
	}
	l.reply, l.pos = mewtocol.AppendBCC([]byte(sb.String())), 0
	return nil
}

func (l *spanLink) ReadFull(n int) ([]byte, error) {
	if l.reply == nil {
		return nil, timeoutErr{}
	}
	if l.pos+n > len(l.reply) {
		return nil, errors.New("script: reply exhausted")
	}
	out := l.reply[l.pos : l.pos+n]
	l.pos += n
	return out, nil
}

func (l *spanLink) Close() error                { return nil }
func (l *spanLink) SetDeadline(time.Time) error { return nil }
func (l *spanLink) Drain() error                { return nil }
func (l *spanLink) Lock()                       {}
func (l *spanLink) Unlock()                     {}

func spanConn(l *spanLink) *conn {
	return &conn{link: l, station: 1, devID: "plc1", timeout: time.Second}
}

// TestReadSpanAcrossFrames：30 个连续寄存器超过一帧的 24 字上限，必须切成
// 两帧、拼接后统一解码，且每个点位只出现一次、全部是 good。
//
// 变异验证：把 decodeSpan 的逐帧解码还原（每帧对 span 内全部点位解码），
// 这条测试会看到 8 个坏点位与 24 个重复样本。
func TestReadSpanAcrossFrames(t *testing.T) {
	words := map[int]uint16{}
	points := make([]driver.PointRef, 0, 30)
	for i := 0; i < 30; i++ {
		words[i] = uint16(0x1000 + i)
		points = append(points, driver.PointRef{
			ID:       fmt.Sprintf("d%d", i),
			Address:  model.Address{Kind: model.AddressRegister, Area: "dt", Offset: i, Length: 1, Bit: -1},
			DataType: model.TypeUint16,
		})
	}
	l := &spanLink{words: words, failAt: -1}
	c := spanConn(l)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: points}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(l.requests) != 2 {
		t.Fatalf("frames = %d, want 2 (24 words + 6 words): %q", len(l.requests), l.requests)
	}
	if len(samples) != 30 {
		t.Fatalf("samples = %d, want 30 (one per point, no duplicates)", len(samples))
	}
	seen := map[string]bool{}
	for _, s := range samples {
		if seen[s.PointID] {
			t.Fatalf("point %s appears twice in the result", s.PointID)
		}
		seen[s.PointID] = true
		if s.Err != nil {
			t.Fatalf("%s: %v", s.PointID, s.Err)
		}
		if s.Quality != model.QualityGood {
			t.Fatalf("%s quality = %v", s.PointID, s.Quality)
		}
	}
	// 顺序与值都要对：切帧之后点位顺序不能乱，值不能串位。
	for i, s := range samples {
		if want := float64(0x1000 + i); s.Value.Num != want {
			t.Fatalf("sample %d (%s) = %v, want %v", i, s.PointID, s.Value.Num, want)
		}
	}
}

// TestReadSpanSecondFrameFails：多帧 span 里后排的帧失败，前排已经读到的值
// 必须保留（不重复、不被标坏），后排点位标坏。这是「部分失败」在**一个
// span 内部**的形态，和跨 span 的部分失败同样重要。
func TestReadSpanSecondFrameFails(t *testing.T) {
	words := map[int]uint16{}
	points := make([]driver.PointRef, 0, 30)
	for i := 0; i < 30; i++ {
		words[i] = uint16(i)
		points = append(points, driver.PointRef{
			ID:       fmt.Sprintf("d%d", i),
			Address:  model.Address{Kind: model.AddressRegister, Area: "dt", Offset: i, Length: 1, Bit: -1},
			DataType: model.TypeUint16,
		})
	}
	l := &spanLink{words: words, failAt: 1}
	c := spanConn(l)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: points}})
	if err != nil {
		t.Fatalf("有成功点时 Read 不该返回错误: %v", err)
	}
	if len(samples) != 30 {
		t.Fatalf("samples = %d, want 30", len(samples))
	}
	for i, s := range samples {
		if i < 24 {
			if s.Err != nil || s.Quality != model.QualityGood {
				t.Fatalf("d%d (first frame) should be good: %+v", i, s)
			}
			continue
		}
		if s.Err == nil {
			t.Fatalf("d%d (second frame) should carry the timeout: %+v", i, s)
		}
		if s.Quality != model.QualityTimeout {
			t.Fatalf("d%d quality = %v, want timeout", i, s.Quality)
		}
	}
}

// TestReadSpanLinkFailureStops：第一帧就链路级失败时，后面的帧不再发出去
// （否则一轮采集会被拖成 N 倍超时），且全部点位都带错误——不能从结果里
// 消失，那会被采集层当成「设备安静」。
func TestReadSpanLinkFailureStops(t *testing.T) {
	words := map[int]uint16{}
	points := make([]driver.PointRef, 0, 30)
	for i := 0; i < 30; i++ {
		words[i] = uint16(i)
		points = append(points, driver.PointRef{
			ID:       fmt.Sprintf("d%d", i),
			Address:  model.Address{Kind: model.AddressRegister, Area: "dt", Offset: i, Length: 1, Bit: -1},
			DataType: model.TypeUint16,
		})
	}
	l := &spanLink{words: words, failAt: 0}
	c := spanConn(l)

	samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: points}})
	if err == nil {
		t.Fatal("全部点位失败时必须返回错误，让熔断器看到「设备全坏」")
	}
	if got := driver.KindOf(err); got != driver.ErrTimeout {
		t.Fatalf("kind = %v, want timeout", got)
	}
	if len(l.requests) != 1 {
		t.Fatalf("frames = %d, want 1: a link failure must not be followed by more requests", len(l.requests))
	}
	if len(samples) != 30 {
		t.Fatalf("samples = %d, want 30", len(samples))
	}
	for _, s := range samples {
		if s.Err == nil || s.Quality != model.QualityTimeout {
			t.Fatalf("%s: %+v", s.PointID, s)
		}
	}
}

// TestReadIndexPointConfig：索引寄存器上两类配置错误必须在发帧之前拦下，
// 而不是静默读到别的东西 / 在运行期报「应答过短」。
func TestReadIndexPointConfig(t *testing.T) {
	cases := []struct {
		name string
		ref  driver.PointRef
	}{
		{"ix with offset", driver.PointRef{
			ID: "bad", Address: model.Address{Kind: model.AddressRegister, Area: "ix", Offset: 1, Bit: -1},
			DataType: model.TypeUint16,
		}},
		{"ix as uint32", driver.PointRef{
			ID: "bad", Address: model.Address{Kind: model.AddressRegister, Area: "ix", Offset: 0, Bit: -1},
			DataType: model.TypeUint32,
		}},
	}
	for _, tc := range cases {
		l := &spanLink{words: map[int]uint16{}, failAt: -1}
		c := spanConn(l)
		samples, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{tc.ref}}})
		if err == nil && (len(samples) == 0 || samples[0].Err == nil) {
			t.Fatalf("%s: expected an address error, got %+v / %v", tc.name, samples, err)
		}
		if len(l.requests) != 0 {
			t.Fatalf("%s: a bad config must not touch the wire, sent %q", tc.name, l.requests)
		}
		var e error
		if err != nil {
			e = err
		} else {
			e = samples[0].Err
		}
		if got := driver.KindOf(e); got != driver.ErrAddress {
			t.Fatalf("%s: kind = %v, want address", tc.name, got)
		}
	}
}

// TestWriteIndexWidthCheckedLocally：宽度不符的索引写必须在**主站侧**被拒绝，
// 一个字节都不发。发出去的话，PLC 侧要么回 61H（数据错，看不出哪条配置错），
// 要么在某些固件上照收——只写 I0，I1 留在旧值，却回「写成功」。
func TestWriteIndexWidthCheckedLocally(t *testing.T) {
	cases := []struct {
		name string
		req  driver.WriteRequest
		hint string
	}{
		{
			name: "id as uint16",
			req: driver.WriteRequest{
				PointID:  "id16",
				Address:  model.Address{Kind: model.AddressRegister, Area: "id", Offset: 0, Length: 1, Bit: -1},
				DataType: model.TypeUint16,
				Value:    model.NumValue(model.TypeUint16, 9),
			},
			hint: "uint32",
		},
		{
			name: "ix as uint32",
			req: driver.WriteRequest{
				PointID:  "ix32",
				Address:  model.Address{Kind: model.AddressRegister, Area: "ix", Offset: 0, Length: 2, Bit: -1},
				DataType: model.TypeUint32,
				Value:    model.NumValue(model.TypeUint32, 9),
			},
			hint: "uint16",
		},
	}
	for _, tc := range cases {
		l := &spanLink{words: map[int]uint16{}, failAt: -1}
		c := spanConn(l)
		res, err := c.Write(context.Background(), []driver.WriteRequest{tc.req})
		if err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
		if len(l.requests) != 0 {
			t.Fatalf("%s: a bad width must not touch the wire, sent %q", tc.name, l.requests)
		}
		if got := driver.KindOf(err); got != driver.ErrAddress {
			t.Fatalf("%s: kind = %v, want address", tc.name, got)
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Fatalf("%s: error should name the right type (%s): %v", tc.name, tc.hint, err)
		}
		if len(res) != 1 || res[0].Quality == "" {
			t.Fatalf("%s: failed item must carry a quality: %+v", tc.name, res)
		}
	}
}

// options 解析与 hostlink/ads 同一纪律：非法值、缺值、未知项报错，
// 不悄悄回默认值（旧行为是静默忽略，是缺口审计点名的静默降级）。
func TestParseOptions(t *testing.T) {
	opt, err := parseOptions(model.Device{Labels: map[string]string{"options": "gap=20ms"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opt.Gap != 20*time.Millisecond {
		t.Fatalf("opt = %+v", opt)
	}
	for _, tc := range []struct{ name, options string }{
		{"gap 非法", "gap=99s"},
		{"gap 缺值", "gap"},
		{"未知项", "junk=1"},
	} {
		if _, err := parseOptions(model.Device{ID: "d", Labels: map[string]string{"options": tc.options}}); err == nil {
			t.Fatalf("%s: 应报错", tc.name)
		}
	}
	opt, err = parseOptions(model.Device{})
	if err != nil || opt.Gap != defaultGap {
		t.Fatalf("opt = %+v err = %v", opt, err)
	}
}
