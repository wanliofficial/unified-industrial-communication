package mtconnect

import (
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mtcodec "uic/internal/codec/mtconnect"
	"uic/internal/driver"
	"uic/internal/model"
	simmtconnect "uic/internal/sim/mtconnect"
)

// driver 测试用 httptest 起真实 HTTP 服务，注入不同响应来覆盖驱动的
// 每条错误分类路径；再叠加一个多设备 Agent 的选择逻辑。

func deviceWithPoints(points ...model.Point) model.Device {
	return model.Device{
		ID: "dev", ChannelID: "ch", Enabled: true,
		Timeout: model.Duration(3 * time.Second),
		Points:  points,
	}
}

func openMTConnect(t *testing.T, addr string, dev model.Device) driver.Conn {
	t.Helper()
	ch := model.Channel{ID: "ch", Protocol: "mtconnect",
		Transport: model.Transport{Kind: "tcp", Address: addr}}
	c, err := New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return c
}

func span(pts ...model.Point) []driver.ReadSpan {
	refs := make([]driver.PointRef, 0, len(pts))
	dev := deviceWithPoints(pts...)
	for _, p := range pts {
		refs = append(refs, driver.PointRefOf(dev, p))
	}
	return []driver.ReadSpan{{ID: "sym:dataItems", Points: refs}}
}

func pt(id, path, dt string) model.Point {
	return model.Point{ID: id, Enabled: true, DataType: model.DataType(dt),
		Address: model.Address{Kind: model.AddressSymbol, Path: path, Index: -1}}
}

func TestReadMultiPoint(t *testing.T) {
	body := `<?xml version="1.0"?><MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
		`<ComponentStream component="Linear" name="X"><Samples>` +
		`<Position dataItemId="Xabs" sequence="1">12.5</Position>` +
		`<Position dataItemId="Zload" sequence="2">3.5</Position>` +
		`</Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MTConnect-Version", "2.0")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := openMTConnect(t, srv.URL, deviceWithPoints(
		pt("p1", "Xabs", "float64"), pt("p2", "Zload", "float64")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("p1", "Xabs", "float64"), pt("p2", "Zload", "float64")))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d samples, want 2", len(out))
	}
	m := map[string]driver.RawSample{}
	for _, s := range out {
		m[s.PointID] = s
	}
	if v := m["p1"].Value; v.Type != model.TypeFloat64 || v.Num != 12.5 {
		t.Fatalf("p1 = %+v", m["p1"])
	}
	if m["p1"].Quality != model.QualityGood {
		t.Fatalf("p1 quality = %q", m["p1"].Quality)
	}
	if m["p2"].Value.Num != 3.5 {
		t.Fatalf("p2 = %+v", m["p2"])
	}
}

func TestReadUnavailableIsBadNotError(t *testing.T) {
	// UNAVAILABLE 是单点 bad，Read 本身成功。
	body := `<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
		`<ComponentStream component="Systems" name="s"><Samples>` +
		`<Tool_Offset dataItemId="toolOffset" sequence="1">UNAVAILABLE</Tool_Offset>` +
		`</Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("t", "toolOffset", "float64")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("t", "toolOffset", "float64")))
	if err != nil {
		t.Fatalf("Read should succeed for an unavailable point: %v", err)
	}
	if len(out) != 1 || out[0].Quality != model.QualityBad {
		t.Fatalf("unavailable point = %+v", out[0])
	}
	if out[0].Err == nil {
		t.Fatal("unavailable point should carry an error description")
	}
}

// TestConditionWithoutLevelIsBadNotGood 是本驱动最容易犯的错的反向断言：
// Agent 写了非标准的级别元素名时，那条观测仍然是「报警」，不能因为
// 读不出级别就回落成普通文本上报（那会得到 quality=good + value="OVERHEAT"，
// 一条告警被静默吞掉，而且看起来完全正常）。
func TestConditionWithoutLevelIsBadNotGood(t *testing.T) {
	cases := []struct{ name, conds string }{
		// 平铺写法 + 未知元素名：这是**唯一**真正可观测的 fail-open 路径。
		// 文本非空（"OVERHEAT"），若无「条件缺级别」判定，就会按 string 解出
		// 来照常上报 quality=good+value="OVERHEAT"——看起来完全正常。
		{"平铺未知元素名", `<Conditions><Bogus dataItemId="alm" type="SYSTEM" sequence="5">` +
			`OVERHEAT</Bogus></Conditions>`},
		// 两层写法 + 未知内层级别名：文本落在外层（为空），会被「空值」分支
		// 拦成 bad；这里仍要断言 bad，但更要断言错误文案说清是「级别读不出来」
		// 而不是误报成「空值」。
		{"未知级别元素名", `<Conditions><System dataItemId="alm" type="SYSTEM" sequence="5">` +
			`<BogusLevel sequence="5">OVERHEAT</BogusLevel></System></Conditions>`},
		{"外层无内层", `<Conditions><System dataItemId="alm" type="SYSTEM" sequence="5">` +
			`</System></Conditions>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
				`<ComponentStream component="Controller" name="c">` + c.conds +
				`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			c2 := openMTConnect(t, srv.URL, deviceWithPoints(pt("alm", "alm", "string")))
			defer c2.Close()
			out, err := c2.Read(context.Background(), span(pt("alm", "alm", "string")))
			if err != nil {
				t.Fatalf("Read 不应整体报错（单点问题不升级为链路错误）: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("got %d samples", len(out))
			}
			if out[0].Quality == model.QualityGood {
				t.Fatalf("条件缺级别必须判坏，实际 quality=%q value=%v err=%v",
					out[0].Quality, out[0].Value, out[0].Err)
			}
			if out[0].Err == nil {
				t.Fatal("应附带「级别读不出来」的诊断，否则运维只看到 bad 不知原因")
			}
			// 平铺未知元素名这条：文本非空且是合法的 string，若无「条件缺级别」
			// 判定就会一路走到 conditionQuality 并（fail-open 时）返回 good。
			// 额外断言错误文案不要误报成「空值」——那会把排查引向错误方向。
			if c.name == "平铺未知元素名" {
				if out[0].Value.Type == model.TypeString && out[0].Value.Text == "OVERHEAT" &&
					out[0].Quality == model.QualityGood {
					t.Fatal("无法识别的告警被当成正常字符串上报（fail-open）")
				}
				if strings.Contains(out[0].Err.Error(), "empty value") {
					t.Fatalf("错误文案误报成空值：%v", out[0].Err)
				}
			}
		})
	}
}

// TestConditionLevelsMapToQuality 钉住四态映射的两端：Fault→bad、
// Warning→uncertain、Normal→good、Unavailable→bad。
func TestConditionLevelsMapToQuality(t *testing.T) {
	body := `<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
		`<ComponentStream component="Controller" name="c"><Conditions>` +
		`<Fault dataItemId="f" type="SYSTEM" sequence="1">x</Fault>` +
		`<Warning dataItemId="w" type="SYSTEM" sequence="2">x</Warning>` +
		`<Normal dataItemId="n" type="SYSTEM" sequence="3">x</Normal>` +
		`<Unavailable dataItemId="u" type="SYSTEM" sequence="4"/>` +
		`</Conditions></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	points := []model.Point{
		pt("f", "f", "string"), pt("w", "w", "string"),
		pt("n", "n", "string"), pt("u", "u", "string"),
	}
	c := openMTConnect(t, srv.URL, deviceWithPoints(points...))
	defer c.Close()
	out, err := c.Read(context.Background(), span(points...))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := map[string]model.Quality{}
	for _, s := range out {
		got[s.PointID] = s.Quality
	}
	want := map[string]model.Quality{
		"f": model.QualityBad, "w": model.QualityUncertain,
		"n": model.QualityGood, "u": model.QualityBad,
	}
	for id, q := range want {
		if got[id] != q {
			t.Errorf("point %s quality = %q，应为 %q", id, got[id], q)
		}
	}
}

func TestReadMissingIDIsAddressError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
			`<ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples>` +
			`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`))
	}))
	defer srv.Close()

	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64"), pt("q", "missingId", "float64")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64"), pt("q", "missingId", "float64")))
	if err != nil {
		t.Fatalf("Read should succeed with one bad point: %v", err)
	}
	var q driver.RawSample
	for _, s := range out {
		if s.PointID == "q" {
			q = s
		}
	}
	if q.Quality != model.QualityAddressError || driver.KindOf(q.Err) != driver.ErrAddress {
		t.Fatalf("missing id = %+v", q)
	}
}

func TestReadNon2xxIsProtocolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`<MTConnectError><Header/><Errors><Error errorCode="INTERNAL_ERROR">boom</Error></Errors></MTConnectError>`))
	}))
	defer srv.Close()

	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	_, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("500 -> err kind = %v, want ErrProtocol", driver.KindOf(err))
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention status: %v", err)
	}
}

func TestReadUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "string")))
	defer c.Close()
	_, err := c.Read(context.Background(), span(pt("p", "Xabs", "string")))
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("401 -> kind = %v, want ErrPermission", driver.KindOf(err))
	}
}

func TestReadNotFoundIsAddressError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "string")))
	defer c.Close()
	_, err := c.Read(context.Background(), span(pt("p", "Xabs", "string")))
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("404 -> kind = %v, want ErrAddress", driver.KindOf(err))
	}
}

func TestReadMalformedXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<MTConnectStreams><Header>"))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "string")))
	defer c.Close()
	_, err := c.Read(context.Background(), span(pt("p", "Xabs", "string")))
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("malformed xml -> kind = %v, want ErrProtocol", driver.KindOf(err))
	}
}

func TestReadHTMLResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><html><body>Agent login</body></html>"))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "string")))
	defer c.Close()
	_, err := c.Read(context.Background(), span(pt("p", "Xabs", "string")))
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("html -> kind = %v, want ErrProtocol", driver.KindOf(err))
	}
	if !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("html hint missing: %v", err)
	}
}

func TestReadConditionPoint(t *testing.T) {
	// 条件点声明为 string，值来自级别。
	body := `<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
		`<ComponentStream component="Controller" name="c"><Conditions><System dataItemId="alm" sequence="1"><Fault sequence="1">OVERHEAT</Fault></System></Conditions>` +
		`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("a", "alm", "string")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("a", "alm", "string")))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out[0].Quality != model.QualityBad {
		t.Fatalf("fault condition quality = %q, want bad", out[0].Quality)
	}
	if out[0].Value.String() != "FAULT" {
		t.Fatalf("condition value = %q, want FAULT", out[0].Value.String())
	}
}

// TestReadUnknownLevelConditionMergedWithNormalIsBad 是「单点问题不升级为链路
// 错误」与「级别缺失判坏」两条规则的交点，也是最隐蔽的一条 fail-open：
//
// 同一 dataItemId 上，一条级别读不出来的条件（Agent 用了非标准级别元素名）与
// 一条 Normal 同时出现时，codec 的合并若让 Normal 胜出，级别就变成 "NORMAL"，
// 驱动侧「级别缺失判坏」分支根本走不到——一条真实告警被上报成 good，而且
// 能解析、字段不空、看不出任何异常。
func TestReadUnknownLevelConditionMergedWithNormalIsBad(t *testing.T) {
	body := `<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
		`<ComponentStream component="Controller" name="c"><Conditions>` +
		`<System dataItemId="alm" type="SYSTEM" sequence="5">` +
		`<BogusLevel sequence="5">OVERHEAT</BogusLevel></System>` +
		`<System dataItemId="alm" type="SYSTEM" sequence="9"><Normal sequence="9">ok</Normal></System>` +
		`</Conditions></ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("alm", "alm", "string")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("alm", "alm", "string")))
	if err != nil {
		t.Fatalf("Read 不应整体报错（单点问题不升级为链路错误）: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d samples", len(out))
	}
	if out[0].Quality == model.QualityGood {
		t.Fatalf("未知级别条件与 Normal 合并后 quality=good（fail-open）：value=%v err=%v",
			out[0].Value, out[0].Err)
	}
	if out[0].Err == nil {
		t.Fatal("应附带「级别读不出来」的诊断")
	}
}

func TestReadTypeMismatchIsAddressError(t *testing.T) {
	// 声明 float64 但 Agent 给了文本，应折成地址错误，而不是崩溃。
	body := `<MTConnectStreams><Header/><Streams><DeviceStream name="VMC" uuid="u1">` +
		`<ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">not-a-number</Position></Samples>` +
		`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if err != nil {
		t.Fatalf("Read should succeed: %v", err)
	}
	if out[0].Quality != model.QualityAddressError {
		t.Fatalf("mismatch quality = %q, want address error", out[0].Quality)
	}
}

func TestReadTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`<MTConnectStreams><Header/><Streams><DeviceStream name="A" uuid="A1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`))
	}))
	defer srv.Close()
	dev := deviceWithPoints(pt("p", "Xabs", "float64"))
	dev.Timeout = model.Duration(50 * time.Millisecond)
	ch := model.Channel{ID: "ch", Protocol: "mtconnect", Transport: model.Transport{Kind: "tcp", Address: srv.URL}}
	c, err := New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	_, err = c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if driver.KindOf(err) != driver.ErrTimeout {
		t.Fatalf("slow server -> kind = %v, want ErrTimeout", driver.KindOf(err))
	}
}

func TestReadMultiDeviceRequiresSelection(t *testing.T) {
	body := `<MTConnectStreams><Header/><Streams>` +
		`<DeviceStream name="A" uuid="A1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples></ComponentStream></DeviceStream>` +
		`<DeviceStream name="B" uuid="B1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">2</Position></Samples></ComponentStream></DeviceStream>` +
		`</Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	// 两台设备、无 labels.deviceUuid/deviceName：应报地址错误并列出可选设备。
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	_, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("multi-device unselected -> kind = %v, want ErrAddress", driver.KindOf(err))
	}
	if !strings.Contains(err.Error(), "deviceUuid") {
		t.Fatalf("error should mention deviceUuid selection: %v", err)
	}
}

func TestReadSelectsByUUID(t *testing.T) {
	body := `<MTConnectStreams><Header/><Streams>` +
		`<DeviceStream name="A" uuid="A1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">111</Position></Samples></ComponentStream></DeviceStream>` +
		`<DeviceStream name="B" uuid="B1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="2">222</Position></Samples></ComponentStream></DeviceStream>` +
		`</Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	dev := deviceWithPoints(pt("p", "Xabs", "float64"))
	dev.Labels = map[string]string{"deviceUuid": "B1"}
	c := openMTConnect(t, srv.URL, dev)
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out[0].Value.Num != 222 {
		t.Fatalf("selected B1 -> value = %v, want 222", out[0].Value.Num)
	}
}

func TestProbeFallsBackToCurrent(t *testing.T) {
	// /probe 返回 404，驱动应退回 /current 判定连通性。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/probe") {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`<MTConnectStreams><Header/><Streams><DeviceStream name="A" uuid="A1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe should fall back to /current: %v", err)
	}
}

func TestWriteUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<MTConnectStreams><Header/><Streams/></MTConnectStreams>`))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	res, err := c.Write(context.Background(), []driver.WriteRequest{{
		PointID: "p", Address: model.Address{Kind: model.AddressSymbol, Path: "Xabs", Index: -1},
		DataType: model.TypeFloat64, Value: model.NumValue(model.TypeFloat64, 1),
	}})
	if err == nil {
		t.Fatal("Write should report unsupported")
	}
	if res[0].Quality != model.QualityBad || driver.KindOf(res[0].Err) != driver.ErrUnsupported {
		t.Fatalf("write result = %+v", res[0])
	}
}

func TestReadBaseURLWithPath(t *testing.T) {
	// 基址带子路径，驱动要拼出 /probe 到 base/probe 而不是 root/probe。
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`<MTConnectStreams><Header/><Streams><DeviceStream name="A" uuid="A1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL+"/mtconnect", deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	if _, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64"))); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if gotPath != "/mtconnect/current" {
		t.Fatalf("probe path = %q, want /mtconnect/current", gotPath)
	}
}

func TestParseBaseURL(t *testing.T) {
	cases := []struct{ in, scheme, host, path string }{
		{"192.168.1.20:5000", "http", "192.168.1.20:5000", ""},
		{"http://host:5000/mtconnect", "http", "host:5000", "/mtconnect"},
		{"https://h:443", "https", "h:443", ""},
	}
	for _, tc := range cases {
		u, err := ParseBaseURL(tc.in)
		if err != nil {
			t.Fatalf("ParseBaseURL(%q): %v", tc.in, err)
		}
		if u.Scheme != tc.scheme || u.Host != tc.host || u.Path != tc.path {
			t.Fatalf("ParseBaseURL(%q) = %+v, want scheme=%s host=%s path=%s", tc.in, u, tc.scheme, tc.host, tc.path)
		}
	}
	for _, bad := range []string{"", "ftp://host:5000", "http://u:p@host:5000", "http://host:5000?q=1"} {
		if _, err := ParseBaseURL(bad); err == nil {
			t.Fatalf("ParseBaseURL(%q) should fail", bad)
		}
	}
}

func TestParseBaseURLAddsDefaultPort(t *testing.T) {
	u, err := ParseBaseURL("http://host")
	if err != nil {
		t.Fatalf("ParseBaseURL: %v", err)
	}
	if u.Host != "host:5000" {
		t.Fatalf("default port = %q, want host:5000", u.Host)
	}
}

// ---------------------------------------------------------------------------
// 自检入口（ProbeDevices / Current / Sample / LastVersion / BaseURL）
// ---------------------------------------------------------------------------

func TestProbeDevices(t *testing.T) {
	probeBody := `<MTConnectDevices><Header instanceId="7"/>` +
		`<Devices><Device id="D1" name="VMC" uuid="VMC-001">` +
		`<Description>desc</Description><Manufacturer>ACME</Manufacturer>` +
		`<DataItems><DataItem category="EVENT" type="AVAILABILITY" id="avail"/></DataItems>` +
		`<Components><Linear id="X" name="X" type="LINEAR">` +
		`<DataItems><DataItem category="SAMPLE" type="POSITION" id="Xabs" units="MILLIMETER"/></DataItems>` +
		`</Linear></Components></Device></Devices></MTConnectDevices>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MTConnect-Version", "2.1")
		w.Write([]byte(probeBody))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()

	doc, version, err := c.(interface {
		ProbeDevices(context.Context) (*mtcodec.Devices, string, error)
	}).ProbeDevices(context.Background())
	if err != nil {
		t.Fatalf("ProbeDevices: %v", err)
	}
	if version != "2.1" {
		t.Fatalf("version = %q, want 2.1 (from response header)", version)
	}
	if len(doc.Devices) != 1 || doc.Devices[0].UUID != "VMC-001" {
		t.Fatalf("devices = %+v", doc.Devices)
	}
	// 自检发现的版本会记住，LastVersion 能读到。
	if c.(interface{ LastVersion() string }).LastVersion() != "2.1" {
		t.Fatalf("LastVersion not remembered")
	}
}

func TestProbeDevicesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`<MTConnectError><Header/><Errors><Error errorCode="X">boom</Error></Errors></MTConnectError>`))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	_, _, err := c.(interface {
		ProbeDevices(context.Context) (*mtcodec.Devices, string, error)
	}).ProbeDevices(context.Background())
	if driver.KindOf(err) != driver.ErrProtocol {
		t.Fatalf("ProbeDevices 500 -> kind = %v, want ErrProtocol", driver.KindOf(err))
	}
}

func TestCurrentAndSample(t *testing.T) {
	body := `<MTConnectStreams><Header version="2.0" firstSequence="100" lastSequence="102" nextSequence="103"/>` +
		`<Streams><DeviceStream name="VMC" uuid="u1"><ComponentStream component="Linear" name="X"><Samples>` +
		`<Position dataItemId="Xabs" sequence="102">50.5</Position></Samples></ComponentStream>` +
		`</DeviceStream></Streams></MTConnectStreams>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()

	cur, ver, err := c.(interface {
		Current(context.Context) (*mtcodec.Streams, string, error)
	}).Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if ver != "2.0" || cur.Header.LastSequence != 102 {
		t.Fatalf("Current header = %+v ver=%q", cur.Header, ver)
	}
	if it, _ := cur.Index().Lookup("Xabs"); it.Value() != "50.5" {
		t.Fatalf("Current Xabs = %q", it.Value())
	}

	smp, _, err := c.(interface {
		Sample(context.Context, uint64, int) (*mtcodec.Streams, string, error)
	}).Sample(context.Background(), 100, 10)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(smp.Items()) == 0 {
		t.Fatal("Sample returned no items")
	}
}

func TestSampleRejectsBadCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<MTConnectStreams><Header/><Streams/></MTConnectStreams>`))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	_, _, err := c.(interface {
		Sample(context.Context, uint64, int) (*mtcodec.Streams, string, error)
	}).Sample(context.Background(), 1, 0)
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("Sample count=0 -> kind = %v, want ErrUnsupported", driver.KindOf(err))
	}
	_, _, err = c.(interface {
		Sample(context.Context, uint64, int) (*mtcodec.Streams, string, error)
	}).Sample(context.Background(), 1, 99999)
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("Sample count too large -> kind = %v, want ErrUnsupported", driver.KindOf(err))
	}
}

func TestProbeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<MTConnectStreams><Header/><Streams><DeviceStream name="A" uuid="A1"><ComponentStream component="Linear" name="X"><Samples><Position dataItemId="Xabs" sequence="1">1</Position></Samples></ComponentStream></DeviceStream></Streams></MTConnectStreams>`))
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe should succeed: %v", err)
	}
}

func TestBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL+"/mtconnect", deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	if got := c.(interface{ BaseURL() string }).BaseURL(); got != srv.URL+"/mtconnect" {
		t.Fatalf("BaseURL = %q", got)
	}
}

func TestOpenRejectsNonTCP(t *testing.T) {
	_, err := New().Open(context.Background(), driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Protocol: "mtconnect", Transport: model.Transport{Kind: "serial", Address: "/dev/x"}},
		Device:  deviceWithPoints(),
	})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("non-tcp transport -> kind = %v, want ErrUnsupported", driver.KindOf(err))
	}
}

func TestReadEmptySpans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	out, err := c.Read(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty spans should not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty spans returned %d samples", len(out))
	}
}

func TestMeta(t *testing.T) {
	m := New().Meta()
	if m.ID != "mtconnect" {
		t.Fatalf("id = %q", m.ID)
	}
	c := m.Capabilities
	if c.Read != driver.CapSupported || c.Write != driver.CapUnsupported {
		t.Fatalf("read/write caps = %v/%v", c.Read, c.Write)
	}
	if c.MultiRead != driver.CapSupported {
		t.Fatalf("MultiRead = %v, want supported (one /current covers all points)", c.MultiRead)
	}
	if c.Subscribe != driver.CapUnsupported || c.Browse != driver.CapUnsupported || c.Discovery != driver.CapUnsupported {
		t.Fatalf("subscribe/browse/discovery should be unsupported: %+v", c)
	}
	if !m.Accepts(model.AddressSymbol) || m.Accepts(model.AddressRegister) {
		t.Fatalf("address kinds = %v, want symbol only", m.AddressKinds)
	}
}

func TestPlanPointRejects(t *testing.T) {
	cases := []struct {
		name string
		ref  driver.PointRef
		want string
	}{
		{"register", driver.PointRef{ID: "p", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressRegister}}, "symbol"},
		{"empty path", driver.PointRef{ID: "p", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressSymbol, Path: "  "}}, "required"},
		{"space in id", driver.PointRef{ID: "p", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressSymbol, Path: "a b"}}, "dataItemId"},
		{"selector", driver.PointRef{ID: "p", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressSymbol, Path: "x", Selector: "s"}}, "selector"},
		{"index", driver.PointRef{ID: "p", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressSymbol, Path: "x", Index: 2}}, "index"},
		{"length", driver.PointRef{ID: "p", DataType: model.TypeFloat64,
			Address: model.Address{Kind: model.AddressSymbol, Path: "x", Length: 4}}, "length"},
		{"bad datatype", driver.PointRef{ID: "p", DataType: model.DataType("nope"),
			Address: model.Address{Kind: model.AddressSymbol, Path: "x"}}, "dataType"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planPoint(tc.ref)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestBuildPlansDuplicate(t *testing.T) {
	dev := deviceWithPoints(pt("dup", "Xabs", "float64"))
	// 两个点位用了同一个 id。
	dev.Points = []model.Point{pt("dup", "Xabs", "float64"), pt("dup", "Yabs", "float64")}
	if _, err := buildPlans(dev); err == nil {
		t.Fatal("duplicate point id should error")
	}
}

func TestParseSettings(t *testing.T) {
	dev := deviceWithPoints()
	dev.Timeout = model.Duration(2 * time.Second)
	// 合法：labels.timeout 在 device.timeout 之内。
	dev.Labels = map[string]string{"timeout": "1s"}
	if _, err := parseSettings(driver.OpenConfig{Device: dev}); err != nil {
		t.Fatalf("valid settings: %v", err)
	}
	// 非法：超过 device.timeout。
	dev.Labels = map[string]string{"timeout": "10s"}
	if _, err := parseSettings(driver.OpenConfig{Device: dev}); err == nil {
		t.Fatal("timeout > device.timeout should error")
	}
	// 非法格式。
	dev.Labels = map[string]string{"timeout": "abc"}
	if _, err := parseSettings(driver.OpenConfig{Device: dev}); err == nil {
		t.Fatal("bad duration should error")
	}
}

func TestReadConnectionRefused(t *testing.T) {
	// 拿一个已经关掉的监听地址，连接会被拒绝。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ch := model.Channel{ID: "ch", Protocol: "mtconnect", Transport: model.Transport{Kind: "tcp", Address: addr}}
	c, err := New().Open(context.Background(), driver.OpenConfig{Channel: ch, Device: deviceWithPoints(pt("p", "Xabs", "float64"))})
	if err != nil {
		t.Fatalf("open should not dial until Read: %v", err)
	}
	defer c.Close()
	_, err = c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("connection refused -> kind = %v, want ErrDisconnected", driver.KindOf(err))
	}
}

func TestReadBodyGzipMagicFallback(t *testing.T) {
	// 服务端给了 gzip 字节但不写 Content-Encoding：驱动的魔数兜底要解压。
	plain := []byte(`<MTConnectStreams><Header/><Streams><DeviceStream name="A" uuid="A1">` +
		`<ComponentStream component="Linear" name="X"><Samples>` +
		`<Position dataItemId="Xabs" sequence="1">7.5</Position></Samples>` +
		`</ComponentStream></DeviceStream></Streams></MTConnectStreams>`)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(plain)
	zw.Close()
	gz := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故意不设 Content-Encoding：Transport 只在自己加了 Accept-Encoding
		// 且响应带该头时才解压，所以这里会把原始 gzip 字节交给驱动。
		w.Header().Set("Content-Type", "application/xml")
		w.Write(gz)
	}))
	defer srv.Close()
	c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "float64")))
	defer c.Close()
	out, err := c.Read(context.Background(), span(pt("p", "Xabs", "float64")))
	if err != nil {
		t.Fatalf("gzip magic fallback: %v", err)
	}
	if out[0].Value.Num != 7.5 {
		t.Fatalf("decompressed value = %v", out[0].Value.Num)
	}
}

func TestStatusErrorVariants(t *testing.T) {
	cases := []struct {
		status int
		kind   driver.ErrorKind
	}{
		{405, driver.ErrUnsupported},
		{429, driver.ErrProtocol},
		{503, driver.ErrProtocol},
		{408, driver.ErrTimeout},
		{403, driver.ErrPermission},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := openMTConnect(t, srv.URL, deviceWithPoints(pt("p", "Xabs", "string")))
		_, err := c.Read(context.Background(), span(pt("p", "Xabs", "string")))
		c.Close()
		srv.Close()
		if driver.KindOf(err) != tc.kind {
			t.Fatalf("status %d -> kind = %v, want %v", tc.status, driver.KindOf(err), tc.kind)
		}
	}
}

// TestIntegrationAgainstSim 用真实的内置 Agent 跑一遍驱动：这是唯一一条
// 「渲染 → HTTP → 解析 → 解码」全链路都走真实代码的用例。其余 driver 用例
// 用 httptest 注入精心构造的响应；这里验证两边的默认设备模型对得上。
func TestIntegrationAgainstSim(t *testing.T) {
	srv, err := simmtconnect.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start sim: %v", err)
	}
	defer srv.Close()

	points := []model.Point{
		pt("x", "Xabs", "float64"),
		pt("exec", "exec", "string"),
		pt("tool", "toolOffset", "float64"),
		pt("alarm", "alm", "string"),
	}
	dev := deviceWithPoints(points...)
	dev.Labels = map[string]string{"deviceUuid": "VMC-3AXIS-001"}
	c := openMTConnect(t, srv.URL(), dev)
	defer c.Close()

	// Probe 应成功（/probe 存在）。
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	out, err := c.Read(context.Background(), span(points...))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	m := map[string]driver.RawSample{}
	for _, s := range out {
		m[s.PointID] = s
	}
	if m["x"].Quality != model.QualityGood || m["x"].Value.Num != 12.3456 {
		t.Fatalf("x = %+v", m["x"])
	}
	if m["exec"].Value.String() != "ACTIVE" {
		t.Fatalf("exec = %+v", m["exec"])
	}
	// toolOffset 初始 UNAVAILABLE → bad 质量。
	if m["tool"].Quality != model.QualityBad {
		t.Fatalf("tool = %+v, want bad (UNAVAILABLE)", m["tool"])
	}
	// alm 初始 NORMAL → good 质量，值是级别。
	if m["alarm"].Quality != model.QualityGood || m["alarm"].Value.String() != "NORMAL" {
		t.Fatalf("alarm = %+v", m["alarm"])
	}

	// 改一个值再读，确认真的走了 HTTP 而非缓存。
	srv.SetValue("Xabs", "77.7")
	out, _ = c.Read(context.Background(), span(points...))
	for _, s := range out {
		if s.PointID == "x" && s.Value.Num != 77.7 {
			t.Fatalf("x after SetValue = %v, want 77.7", s.Value.Num)
		}
	}
}
