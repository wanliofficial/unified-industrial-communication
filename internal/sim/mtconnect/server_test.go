package mtconnect

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	mtcodec "uic/internal/codec/mtconnect"
)

// 内置 Agent 的测试：起一个真实 HTTP 服务，用 net/http 打三个资源，再用
// codec 解析回来验证。故障注入通过 Options.Faults 与 Set* 方法覆盖。

func startSim(t *testing.T, opt Options) *Server {
	t.Helper()
	srv, err := NewServerWithOptions("127.0.0.1:0", opt)
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestSimProbeAndCurrent(t *testing.T) {
	srv := startSim(t, Options{})
	base := srv.URL()

	// /probe 返回设备模型。
	st, body := get(t, base+mtcodec.PathProbe)
	if st != 200 {
		t.Fatalf("probe status = %d", st)
	}
	devs, err := mtcodec.ParseDevices(body)
	if err != nil {
		t.Fatalf("parse probe: %v", err)
	}
	if len(devs.Devices) != 1 || devs.Devices[0].Name != defaultDeviceName {
		t.Fatalf("probe devices = %+v", devs.Devices)
	}

	// /current 返回快照，含全部默认数据项。
	st, body = get(t, base+mtcodec.PathCurrent)
	if st != 200 {
		t.Fatalf("current status = %d", st)
	}
	streams, err := mtcodec.ParseStreams(body)
	if err != nil {
		t.Fatalf("parse current: %v", err)
	}
	idx := streams.Index()
	// toolOffset 初始为 UNAVAILABLE。
	if it, _ := idx.Lookup("toolOffset"); !it.Unavailable() {
		t.Fatalf("toolOffset should be unavailable by default: %+v", it)
	}
	if it, _ := idx.Lookup("Xabs"); it.Value() != "12.3456" {
		t.Fatalf("Xabs = %q", it.Value())
	}
}

func TestSimSetValueAndUnavailable(t *testing.T) {
	srv := startSim(t, Options{})
	base := srv.URL()

	if !srv.SetValue("Xabs", "99.9") {
		t.Fatal("SetValue existing id returned false")
	}
	if srv.SetValue("nope", "1") {
		t.Fatal("SetValue unknown id returned true")
	}
	_, body := get(t, base+mtcodec.PathCurrent)
	streams, _ := mtcodec.ParseStreams(body)
	if it, _ := streams.Index().Lookup("Xabs"); it.Value() != "99.9" {
		t.Fatalf("Xabs = %q after SetValue", it.Value())
	}

	srv.SetUnavailable("Yabs")
	_, body = get(t, base+mtcodec.PathCurrent)
	streams, _ = mtcodec.ParseStreams(body)
	if it, _ := streams.Index().Lookup("Yabs"); !it.Unavailable() {
		t.Fatalf("Yabs should be unavailable after SetUnavailable: %+v", it)
	}
}

func TestSimCondition(t *testing.T) {
	srv := startSim(t, Options{})
	base := srv.URL()

	srv.SetCondition("alm", mtcodec.ConditionFault, "OVERHEAT")
	_, body := get(t, base+mtcodec.PathCurrent)
	streams, _ := mtcodec.ParseStreams(body)
	it, ok := streams.Index().Lookup("alm")
	if !ok {
		t.Fatal("alm missing")
	}
	if !it.IsCondition() || it.Condition != mtcodec.ConditionFault {
		t.Fatalf("alm = %+v, want FAULT", it)
	}
}

func TestSimFaultInjection(t *testing.T) {
	// HTML：返回 HTML 页面。
	srv := startSim(t, Options{Faults: Faults{HTML: true}})
	st, body := get(t, srv.URL()+mtcodec.PathCurrent)
	if st != 200 || !strings.HasPrefix(strings.TrimSpace(string(body)), "<!doctype html") {
		t.Fatalf("HTML fault: status=%d body=%.40q", st, body)
	}

	// Malformed：返回无法解析的 XML。
	srv = startSim(t, Options{Faults: Faults{Malformed: true}})
	_, body = get(t, srv.URL()+mtcodec.PathCurrent)
	if _, err := mtcodec.ParseStreams(body); err == nil {
		t.Fatal("malformed fault should not parse")
	}

	// FailProbe：/probe 返回 500 + MTConnectError。
	srv = startSim(t, Options{Faults: Faults{FailProbe: true}})
	st, body = get(t, srv.URL()+mtcodec.PathProbe)
	if st != 500 {
		t.Fatalf("FailProbe status = %d", st)
	}
	if _, ok := mtcodec.ParseError(body); !ok {
		t.Fatalf("FailProbe body is not an error document: %.60q", body)
	}

	// Unavailable：全部数据项变 UNAVAILABLE。
	srv = startSim(t, Options{Faults: Faults{Unavailable: true}})
	_, body = get(t, srv.URL()+mtcodec.PathCurrent)
	streams, _ := mtcodec.ParseStreams(body)
	if it, _ := streams.Index().Lookup("Xabs"); !it.Unavailable() {
		t.Fatalf("Unavailable fault: Xabs = %+v", it)
	}

	// Condition：alm 变 FAULT，消息取 ConditionText。
	srv = startSim(t, Options{Faults: Faults{Condition: true, ConditionText: "spindle overload"}})
	_, body = get(t, srv.URL()+mtcodec.PathCurrent)
	streams, _ = mtcodec.ParseStreams(body)
	it, _ := streams.Index().Lookup("alm")
	if it.Condition != mtcodec.ConditionFault {
		t.Fatalf("Condition fault: alm = %+v", it)
	}
	if !strings.Contains(it.Text, "spindle overload") {
		t.Fatalf("Condition fault text = %q", it.Text)
	}

	// Omit：Xabs 从快照里消失。
	srv = startSim(t, Options{Faults: Faults{Omit: true}})
	_, body = get(t, srv.URL()+mtcodec.PathCurrent)
	streams, _ = mtcodec.ParseStreams(body)
	if _, ok := streams.Index().Lookup("Xabs"); ok {
		t.Fatal("Omit fault: Xabs should be absent")
	}
}

func TestSimGzip(t *testing.T) {
	srv := startSim(t, Options{Gzip: true})
	// 用 DisableCompression 的客户端：net/http 默认会自己加 Accept-Encoding
	// 并透明解压、抹掉 Content-Encoding。关掉它才能看到服务端真实的头与字节。
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Get(srv.URL() + mtcodec.PathCurrent)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	body, _ := io.ReadAll(resp.Body)
	// 手动按魔数解压，验证确实是 gzip 且内容可解析。
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	if _, err := mtcodec.ParseStreams(plain); err != nil {
		t.Fatalf("gzip body did not decompress/parse: %v", err)
	}
}

func TestSimPathPrefix(t *testing.T) {
	srv := startSim(t, Options{PathPrefix: "/mtconnect"})
	if !strings.HasPrefix(srv.URL(), "http://127.0.0.1:") || !strings.HasSuffix(srv.URL(), "/mtconnect") {
		t.Fatalf("URL = %q", srv.URL())
	}
	st, body := get(t, srv.URL()+mtcodec.PathProbe)
	if st != 200 {
		t.Fatalf("prefixed probe status = %d", st)
	}
	if _, err := mtcodec.ParseDevices(body); err != nil {
		t.Fatalf("prefixed probe parse: %v", err)
	}
	// 根路径返回 404（不是 Agent）。
	st, _ = get(t, srv.URL()+"/")
	if st != 404 {
		t.Fatalf("root status = %d, want 404", st)
	}
}

func TestSimSampleWindow(t *testing.T) {
	srv := startSim(t, Options{})
	// 压入几个序号。
	seqs := []uint64{srv.Tick(), srv.Tick(), srv.Tick()}

	// from = 第二个：应只回后两条。
	resp, err := http.Get(srv.URL() + mtcodec.PathSample + "?from=" + itoa(seqs[1]))
	if err != nil {
		t.Fatalf("GET sample: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	streams, err := mtcodec.ParseStreams(body)
	if err != nil {
		t.Fatalf("parse sample: %v", err)
	}
	// 每个序号是一段 DeviceStream；段数应等于命中的观测条数。
	if len(streams.Devices) < 1 {
		t.Fatalf("sample returned no devices")
	}
	seen := map[uint64]bool{}
	for _, d := range streams.Devices {
		for _, c := range d.Components {
			for _, it := range c.Items {
				seen[it.Sequence] = true
			}
		}
	}
	if !seen[seqs[1]] || !seen[seqs[2]] || seen[seqs[0]] {
		t.Fatalf("sample window wrong: seen=%v want seqs[1],seqs[2]", seen)
	}
}

func TestSimDelay(t *testing.T) {
	srv := startSim(t, Options{Faults: Faults{Delay: 150 * time.Millisecond}})
	start := time.Now()
	get(t, srv.URL()+mtcodec.PathCurrent)
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("Delay fault not applied: elapsed %v", elapsed)
	}
}

func TestSimCloseStopsServer(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	addr := srv.Addr()
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 关闭后再打请求应当失败。
	client := &http.Client{Timeout: 500 * time.Millisecond}
	if _, err := client.Get("http://" + addr + mtcodec.PathCurrent); err == nil {
		t.Fatal("expected request to fail after Close")
	}
}

// itoa 是一个小的 uint64 → string，避免在测试里为了一个转换引入 strconv。
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
