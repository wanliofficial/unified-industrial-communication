package mbus

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	mb "uic/internal/codec/mbus"
)

// 从站的价值在于「能让主站对着一个行为正确的表跑起来」。因此这里的用例
// 全部通过真实字节流（loopback TCP）驱动，而不是直接调 handle()：组帧、
// 地址过滤、FCB 回放、访问号自增、坏帧注入的形状都必须和真表一致。

func dialSim(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// roundTrip 发一帧并读回一帧；从站静默时返回 ok=false。
func roundTrip(t *testing.T, c net.Conn, req []byte, wait time.Duration) (mb.Frame, bool) {
	t.Helper()
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 0, 320)
	tmp := make([]byte, 320)
	for {
		n, err := c.Read(tmp)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return mb.Frame{}, false
			}
			if errors.Is(err, io.EOF) {
				return mb.Frame{}, false
			}
			t.Fatalf("read: %v", err)
		}
		buf = append(buf, tmp[:n]...)
		if fr, _, perr := mb.Parse(buf); perr == nil {
			return fr, true
		}
	}
}

func readFrame(t *testing.T, c net.Conn, wait time.Duration) (mb.Frame, []byte, bool) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 0, 320)
	tmp := make([]byte, 320)
	for {
		n, err := c.Read(tmp)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return mb.Frame{}, nil, false
			}
			if errors.Is(err, io.EOF) {
				return mb.Frame{}, nil, false
			}
			t.Fatalf("read: %v", err)
		}
		buf = append(buf, tmp[:n]...)
		if fr, _, perr := mb.Parse(buf); perr == nil {
			return fr, buf, true
		}
	}
}

func TestSNDNKEAnswersACK(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	fr, ok := roundTrip(t, c, mb.BuildShort(mb.CtrlSNDNKE, srv.Address()), 500*time.Millisecond)
	if !ok {
		t.Fatal("SND_NKE 没有应答")
	}
	if fr.Type != mb.FrameAck {
		t.Fatalf("应答类型 = %v，应为单字符 ACK", fr.Type)
	}
	if srv.NKECount() != 1 {
		t.Fatalf("NKE 计数 = %d", srv.NKECount())
	}
}

func TestWrongAddressStaysSilent(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithAddress(9))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	// 主地址 3 不是它：必须保持静默，替别人的地址应答会让主站张冠李戴。
	if _, ok := roundTrip(t, c, mb.BuildShort(mb.CtrlSNDNKE, 3), 300*time.Millisecond); ok {
		t.Fatal("地址不符时不该应答")
	}
	if srv.Requests() == 0 {
		t.Fatal("请求计数应包含被丢弃的帧（用于诊断总线串扰）")
	}
	// 对地址后必须有应答，证明静默是地址过滤而不是链路坏了。
	if _, ok := roundTrip(t, c, mb.BuildShort(mb.CtrlSNDNKE, 9), 500*time.Millisecond); !ok {
		t.Fatal("正确地址应应答")
	}
}

func TestREQUD2ReturnsWholeTable(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	fr, ok := roundTrip(t, c, mb.BuildShort(mb.ReqUD2Control(true), srv.Address()), 500*time.Millisecond)
	if !ok {
		t.Fatal("REQ_UD2 没有应答")
	}
	resp, err := mb.ParseResponse(fr)
	if err != nil {
		t.Fatalf("解析应答: %v", err)
	}
	if resp.Kind != mb.DataVariable {
		t.Fatalf("数据结构 = %v，应为 variable", resp.Kind)
	}
	if len(resp.Records) != len(defaultRecords()) {
		t.Fatalf("记录数 = %d，应为 %d", len(resp.Records), len(defaultRecords()))
	}
	if resp.Header.IDText != "12345678" || resp.Header.ManufacturerID != "XYZ" {
		t.Fatalf("固定头 = %+v", resp.Header)
	}
	// 值必须与 defaultRecords 一致。
	if rec, ok, _ := mb.FindRecord(resp.Records, "energy"); !ok || rec.Num != 12345678 {
		t.Fatalf("energy = %+v", rec)
	}
}

func TestAccessNumberIncrements(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetAccessNo(0)

	c := dialSim(t, srv)
	var last byte
	for i := 0; i < 3; i++ {
		fcb := i%2 == 0
		fr, ok := roundTrip(t, c, mb.BuildShort(mb.ReqUD2Control(fcb), srv.Address()), 500*time.Millisecond)
		if !ok {
			t.Fatalf("第 %d 次读取没有应答", i)
		}
		resp, err := mb.ParseResponse(fr)
		if err != nil {
			t.Fatalf("第 %d 次解析: %v", i, err)
		}
		// 每次新请求都应加一；FCB 交替正是"新请求"的信号。
		if i > 0 && resp.Header.AccessNo != last+1 {
			t.Fatalf("第 %d 次访问号 = %d，应为 %d", i, resp.Header.AccessNo, last+1)
		}
		last = resp.Header.AccessNo
	}
}

func TestFCBRetransmitReplaysWithoutIncrement(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	req := mb.BuildShort(mb.ReqUD2Control(true), srv.Address())
	fr1, ok := roundTrip(t, c, req, 500*time.Millisecond)
	if !ok {
		t.Fatal("首帧没有应答")
	}
	resp1, _ := mb.ParseResponse(fr1)
	// 完全相同的 FCB 再来一次 = 重传，必须回放同一份字节。
	_, raw2, ok := func() (mb.Frame, []byte, bool) {
		if _, err := c.Write(req); err != nil {
			t.Fatalf("write: %v", err)
		}
		return readFrame(t, c, 500*time.Millisecond)
	}()
	if !ok {
		t.Fatal("重传没有应答")
	}
	fr2, _, err := mb.Parse(raw2)
	if err != nil {
		t.Fatalf("解析重传应答: %v", err)
	}
	resp2, _ := mb.ParseResponse(fr2)
	if resp2.Header.AccessNo != resp1.Header.AccessNo {
		t.Fatalf("重传后访问号 %d 变了（应为 %d）：会造成重复计费",
			resp2.Header.AccessNo, resp1.Header.AccessNo)
	}
	if srv.Reads() != 1 {
		t.Fatalf("成功读取次数 = %d，重传不该计为新读取", srv.Reads())
	}
	// 翻转 FCB 则是新请求，访问号要加一。
	fr3, ok := roundTrip(t, c, mb.BuildShort(mb.ReqUD2Control(false), srv.Address()), 500*time.Millisecond)
	if !ok {
		t.Fatal("翻转 FCB 后没有应答")
	}
	resp3, _ := mb.ParseResponse(fr3)
	if resp3.Header.AccessNo != resp1.Header.AccessNo+1 {
		t.Fatalf("新请求访问号 = %d，应为 %d", resp3.Header.AccessNo, resp1.Header.AccessNo+1)
	}
	if srv.Reads() != 2 {
		t.Fatalf("成功读取次数 = %d，应为 2", srv.Reads())
	}
}

func TestNKEResetsFCBState(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	req := mb.BuildShort(mb.ReqUD2Control(true), srv.Address())
	if _, ok := roundTrip(t, c, req, 500*time.Millisecond); !ok {
		t.Fatal("首帧没有应答")
	}
	// SND_NKE 之后，同样的 FCB 应被当成新请求而不是重传。
	if _, ok := roundTrip(t, c, mb.BuildShort(mb.CtrlSNDNKE, srv.Address()), 500*time.Millisecond); !ok {
		t.Fatal("SND_NKE 没有应答")
	}
	if _, ok := roundTrip(t, c, req, 500*time.Millisecond); !ok {
		t.Fatal("复位后读取没有应答")
	}
	if srv.Reads() != 2 {
		t.Fatalf("成功读取次数 = %d，应为 2（SND_NKE 后同一 FCB 是新请求）", srv.Reads())
	}
}

func TestBadChecksumInjection(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetBadChecksum(true)

	c := dialSim(t, srv)
	if _, err := c.Write(mb.BuildShort(mb.ReqUD2Control(true), srv.Address())); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 320)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 注入只改校验和：帧结构与数据都完好，这样"主站靠校验和发现问题"
	// 这条路径才被真正验证到。
	if _, _, perr := mb.Parse(buf[:n]); !errors.Is(perr, mb.ErrChecksum) {
		t.Fatalf("注入的坏帧应报 ErrChecksum，得到 %v（%x）", perr, buf[:n])
	}
}

// TestBadCIIsWellFormed 钉住 SetBadCI 的关键性质：换掉 CI 之后**必须重算
// 校验和**，否则帧会先在分帧/校验阶段就被判坏，失败原因退化成"校验和错"
// （可重发），而测不到"CI 不受支持不重发"这条语义。
func TestBadCIIsWellFormed(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	const badCI = 0x5A
	srv.SetBadCI(badCI)

	c := dialSim(t, srv)
	if _, err := c.Write(mb.BuildShort(mb.ReqUD2Control(true), srv.Address())); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 320)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 第一步：帧本身必须**完好**——能过 Parse（含校验和），CI 就是注入值。
	fr, _, perr := mb.Parse(buf[:n])
	if perr != nil {
		t.Fatalf("SetBadCI 注入的帧应能正常分帧（校验和需重算），得到 %v（%x）", perr, buf[:n])
	}
	if fr.CIField != badCI {
		t.Fatalf("CI = %02X，应为注入的 %02X", fr.CIField, badCI)
	}
	// 第二步：在业务层被拒为 ErrFrame——这才是"从站在正常应答、只是我们
	// 理解错了"的那一类，驱动据此报不可重发。
	if _, perr := mb.ParseResponse(fr); !errors.Is(perr, mb.ErrFrame) {
		t.Fatalf("不受支持的 CI 应在 ParseResponse 报 ErrFrame，得到 %v", perr)
	}
}

// TestReqCountSeparatesRetries 钉住 ReqCount 与 Reads 的口径差异：主站重发
// 同一 FCB 时从站回放旧应答，Reads 不增而 ReqCount 增。判断"某类错误该不该
// 重发"只能靠这一对计数。
func TestReqCountSeparatesRetries(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	req := mb.BuildShort(mb.ReqUD2Control(true), srv.Address())
	for i := 0; i < 3; i++ {
		if _, err := c.Write(req); err != nil {
			t.Fatalf("write #%d: %v", i, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 320)
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("read #%d: %v", i, err)
		}
	}
	if got := srv.ReqCount(); got != 3 {
		t.Fatalf("ReqCount = %d，应为 3（每次请求都计，含回放）", got)
	}
	if got := srv.Reads(); got != 1 {
		t.Fatalf("Reads = %d，应为 1（同 FCB 重放不生成新应答）", got)
	}
}

func TestSilentMode(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetSilent(true)

	c := dialSim(t, srv)
	if _, ok := roundTrip(t, c, mb.BuildShort(mb.CtrlSNDNKE, srv.Address()), 300*time.Millisecond); ok {
		t.Fatal("静默模式不该应答")
	}
}

func TestStatusErrorBit(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetStatusError(true)

	c := dialSim(t, srv)
	fr, ok := roundTrip(t, c, mb.BuildShort(mb.ReqUD2Control(true), srv.Address()), 500*time.Millisecond)
	if !ok {
		t.Fatal("没有应答")
	}
	resp, err := mb.ParseResponse(fr)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !resp.Header.StatusError() {
		t.Fatalf("状态字 = %02X，应带应用错误位", resp.Header.Status)
	}
}

func TestMoreRecordsFlag(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", WithMoreRecords(true))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	fr, ok := roundTrip(t, c, mb.BuildShort(mb.ReqUD2Control(true), srv.Address()), 500*time.Millisecond)
	if !ok {
		t.Fatal("没有应答")
	}
	resp, err := mb.ParseResponse(fr)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !resp.MoreRecordsFollow {
		t.Fatal("未带 DIF=1F 的后续报文标志")
	}
}

func TestSetValueChangesReply(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if err := srv.SetValue("energy", 42); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if v, ok := srv.Value("energy"); !ok || v != 42 {
		t.Fatalf("Value = %d,%v", v, ok)
	}
	if err := srv.SetValue("nope", 1); err == nil {
		t.Fatal("未知物理量应报错")
	}
	// 归一化：下划线/大小写与点表地址同规则。
	if err := srv.SetValue("Volume_Flow", 7); err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if v, _ := srv.Value("volume-flow"); v != 7 {
		t.Fatalf("volume-flow = %d", v)
	}
}

func TestShortFrameWriteIgnored(t *testing.T) {
	// SND_UD 的错误用法（发成短帧）不该得到应答：标准里没有定义，
	// 回一个 E5 会让主站以为写成功了。
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c := dialSim(t, srv)
	// C=53h 是 SND_UD（主站、FCV=1、FCB=1）；短帧形式在本从站里不应答。
	req := mb.BuildShort(0x53, srv.Address())
	if _, ok := roundTrip(t, c, req, 300*time.Millisecond); ok {
		t.Fatal("未实现的 SND_UD 不该应答")
	}
}

func TestServerCloseIsIdempotent(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("二次 close 应幂等: %v", err)
	}
	// 关闭后不再接受连接。
	if c, err := net.DialTimeout("tcp", srv.Addr(), 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("关闭后仍能建立连接")
	}
}

func TestDefaultRecordsAreAddressable(t *testing.T) {
	// defaultRecords 是 examples/mbus.json 的来源；每个量名都必须能被
	// `q:` 寻址命中，否则示例配置里的点位会静默读到"地址错误"。
	h := mb.Header{IDText: "12345678", ManufacturerID: "XYZ", Medium: mb.MediumHeat}
	data, err := mb.BuildVariableData(h, defaultRecords())
	if err != nil {
		t.Fatalf("组帧: %v", err)
	}
	vd, err := mb.ParseVariableData(data)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(vd.Records) != len(defaultRecords()) {
		t.Fatalf("记录数 = %d，应为 %d", len(vd.Records), len(defaultRecords()))
	}
	for i, rec := range vd.Records {
		if mb.NormalizeQuantity(rec.Quantity) == "" {
			t.Fatalf("第 %d 条记录 VIF %02X 没有物理量名", i, rec.VIF)
		}
	}
	// RecordNames 的输出必须能被 FindRecord 直接吃回去。
	for _, ref := range strings.Split(mb.RecordNames(vd.Records), ", ") {
		name := ref
		if j := strings.IndexByte(name, '('); j >= 0 {
			name = name[:j]
		}
		name = strings.TrimPrefix(name, "q:")
		got, ok, err := mb.FindRecord(vd.Records, name)
		if !ok {
			t.Fatalf("按 %q 反查失败: %v（候选：%s）", name, err, mb.RecordNames(vd.Records))
		}
		if rec := vd.Records[got.Index]; mb.NormalizeQuantity(rec.Quantity) != mb.NormalizeQuantity(got.Quantity) {
			t.Fatalf("按 %q 命中 %d，量名 %q 与记录 %q 不符", name, got.Index, got.Quantity, rec.Quantity)
		}
	}
}

func TestTCPStreamShapes(t *testing.T) {
	// 一帧 250 字节附近的长报文必须能完整穿过 loopback：分片读写是
	// 主站侧分帧最容易出问题的地方。
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if err := srv.SetValue("energy", 1); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	c := dialSim(t, srv)
	fr, ok := roundTrip(t, c, mb.BuildShort(mb.ReqUD2Control(true), srv.Address()), 500*time.Millisecond)
	if !ok {
		t.Fatal("没有应答")
	}
	if fr.Type != mb.FrameLong {
		t.Fatalf("帧类型 = %v，应为长帧", fr.Type)
	}
	// 帧必须能被重新组帧成同样的字节（组帧/解帧互逆）。
	rebuild := mb.BuildLong(fr.C, fr.A, fr.CIField, fr.Data)
	if !bytes.Equal(rebuild, func() []byte {
		b := []byte{mb.LongStart, byte(len(fr.Data) + 3), byte(len(fr.Data) + 3), mb.LongStart,
			fr.C, fr.A, fr.CIField}
		b = append(b, fr.Data...)
		b = append(b, mb.Checksum(fr.C, fr.A, fr.CIField, fr.Data), mb.Stop)
		return b
	}()) {
		t.Fatal("组帧/解帧不互逆")
	}
}
