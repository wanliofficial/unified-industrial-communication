package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uic/internal/i18n"
)

// 北向错误消息 i18n 的端到端验收（docs/03-product/05-northbound-api-spec.md §4.9）：
//
//  1. `error.code` 在任何语言下都不变——它是客户端的分支依据；
//  2. `error.message` 随语言变化，中文请求拿到中文；
//  3. 没有语言偏好时逐字节等于升级前（默认英语）；
//  4. 响应自述语言（Content-Language）并声明 Vary，避免缓存串味。

// getError 发一个会产生领域错误的请求，返回状态码、解析后的信封与响应头。
func getError(t *testing.T, s *Server, path string, headers map[string]string) (*httptest.ResponseRecorder, errorDetail) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: decode envelope: %v (body=%s)", path, err, rec.Body.String())
	}
	return rec, env.Error
}

// 同一个不存在的点位，换语言只换 message，不换 code / 状态码。
func TestErrorEnvelopeLocalizesMessageNotCode(t *testing.T) {
	s, _ := newServer(t, Options{})
	const path = "/api/v1/points/d1/missing"

	en, enErr := getError(t, s, path, nil)
	zh, zhErr := getError(t, s, path, map[string]string{"Accept-Language": "zh-CN,zh;q=0.9"})

	if en.Code != http.StatusNotFound || zh.Code != http.StatusNotFound {
		t.Fatalf("status en=%d zh=%d", en.Code, zh.Code)
	}
	if enErr.Code != "point.not_found" || zhErr.Code != "point.not_found" {
		t.Fatalf("code changed with locale: en=%q zh=%q", enErr.Code, zhErr.Code)
	}
	if zhErr.Message == enErr.Message {
		t.Fatalf("message did not change with locale: %q", enErr.Message)
	}
	// 默认英语，且与引入 i18n 之前的字面量完全一致。
	if want := `point "missing" not found on device "d1"`; enErr.Message != want {
		t.Fatalf("default message = %q, want %q", enErr.Message, want)
	}
	// 参数照常代入（这是「翻译模板而非翻译字符串」的关键证据）。
	if want := `点位 "missing" 在设备 "d1" 上不存在`; zhErr.Message != want {
		t.Fatalf("zh message = %q, want %q", zhErr.Message, want)
	}

	// 语言自述与缓存声明。
	if got := zh.Header().Get("Content-Language"); got != "zh" {
		t.Fatalf("Content-Language = %q, want zh", got)
	}
	if got := en.Header().Get("Content-Language"); got != "en" {
		t.Fatalf("Content-Language = %q, want en", got)
	}
	if !strings.Contains(zh.Header().Get("Vary"), "Accept-Language") {
		t.Fatalf("Vary = %q, want it to mention Accept-Language", zh.Header().Get("Vary"))
	}
}

// 显式 ?lang= 优先于 Accept-Language，两者都优先于默认值。
func TestErrorEnvelopeLanguagePrecedence(t *testing.T) {
	s, _ := newServer(t, Options{})
	cases := []struct {
		name    string
		query   string
		header  string
		wantLan string
		wantMsg string
	}{
		{
			name: "query zh", query: "?lang=zh",
			wantLan: "zh", wantMsg: `点位 "missing" 在设备 "d1" 上不存在`,
		},
		{
			name: "query en beats zh header", query: "?lang=en",
			header:  "zh-CN,zh;q=0.9",
			wantLan: "en", wantMsg: `point "missing" not found on device "d1"`,
		},
		{
			name: "unsupported query defers to header", query: "?lang=fr",
			header:  "zh-CN,zh;q=0.9",
			wantLan: "zh", wantMsg: `点位 "missing" 在设备 "d1" 上不存在`,
		},
		{
			name: "unsupported everywhere", query: "?lang=fr",
			wantLan: "en", wantMsg: `point "missing" not found on device "d1"`,
		},
		{
			name: "wildcard only", header: "*",
			wantLan: "en", wantMsg: `point "missing" not found on device "d1"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hdr map[string]string
			if tc.header != "" {
				hdr = map[string]string{"Accept-Language": tc.header}
			}
			rec, ed := getError(t, s, "/api/v1/points/d1/missing"+tc.query, hdr)
			if got := rec.Header().Get("Content-Language"); got != tc.wantLan {
				t.Fatalf("Content-Language = %q, want %q", got, tc.wantLan)
			}
			if ed.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", ed.Message, tc.wantMsg)
			}
		})
	}
}

// 认证失败也走同一条渲染路径：401 的 code 与 WWW-Authenticate 不变，文案变中文。
func TestErrorEnvelopeLocalizesAuthFailure(t *testing.T) {
	s, _ := newServer(t, Options{Token: "secret"})
	const path = "/api/v1/health"

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Language", "zh")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "auth.unauthorized" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if want := "缺少 Bearer Token 或 Token 无效"; env.Error.Message != want {
		t.Fatalf("message = %q, want %q", env.Error.Message, want)
	}
	// 无头/错误头都不该因为 i18n 丢掉认证挑战语义。
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
}

// 上下文取消/超时由 fail 的兜底分支渲染，同样要能切语言、且状态码语义不变。
func TestErrorEnvelopeLocalizesContextErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  int
		wantCode2 string
		wantMsg   string
	}{
		{"canceled", context.Canceled, 499, "request.canceled", "请求已取消"},
		{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout, "request.timeout", "请求超时"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, be := newServer(t, Options{})
			be.writeFn = func(context.Context, string, []WriteItem) ([]WriteResult, error) {
				return nil, tc.err
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write?lang=zh",
				strings.NewReader(`{"writes":[{"point":"p1","value":{"type":"uint16","num":1}}]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.Error.Code != tc.wantCode2 {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.wantCode2)
			}
			if env.Error.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", env.Error.Message, tc.wantMsg)
			}
		})
	}
}

// 透传类错误：前缀翻成中文，底层错误文本（这里是一段 JSON 解析错误）保持原样
// ——翻译器没有能力也不该改写第三方/标准库的错误详情。
func TestErrorEnvelopeKeepsWrappedDetail(t *testing.T) {
	s, _ := newServer(t, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/points/d1/write?lang=zh",
		strings.NewReader(`{"writes":[`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "request.bad_json" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, "JSON 请求体非法: ") {
		t.Fatalf("message = %q, want the Chinese prefix", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "unexpected EOF") &&
		!strings.Contains(env.Error.Message, "unexpected end of JSON input") {
		t.Fatalf("wrapped detail was lost: %q", env.Error.Message)
	}
}

// APIError 的两副面孔：Message 永远是源语言（审计日志、进程内调用方依赖它），
// Localized 才随请求语言变化。二者不能互相污染。
func TestAPIErrorSourceMessageIsStable(t *testing.T) {
	err := Errf(http.StatusNotFound, "device.not_found", "device %q not found", "plc-01")
	if want := `device "plc-01" not found`; err.Message != want {
		t.Fatalf("Message = %q, want %q", err.Message, want)
	}
	if want := `device "plc-01" not found`; err.Error() != "device.not_found: "+want {
		t.Fatalf("Error() = %q", err.Error())
	}
	if want := `设备 "plc-01" 不存在`; err.Localized(i18n.ZH) != want {
		t.Fatalf("Localized(zh) = %q, want %q", err.Localized(i18n.ZH), want)
	}
	if got := err.Localized(i18n.EN); got != err.Message {
		t.Fatalf("Localized(en) = %q, want the source message %q", got, err.Message)
	}
	// 参数切片是拷贝进来的：调用方事后改自己的 args 不能污染已构造的错误。
	args := []any{"plc-01"}
	err = Errf(http.StatusNotFound, "device.not_found", "device %q not found", args...)
	args[0] = "mutated"
	if got := err.Localized(i18n.ZH); got != `设备 "plc-01" 不存在` {
		t.Fatalf("args were not copied: %q", got)
	}
}

// 手工构造（不经 Errf）的 APIError 没有模板，只能回退原文——不能变成空串。
func TestAPIErrorWithoutTemplateFallsBackToMessage(t *testing.T) {
	err := &APIError{Status: http.StatusTeapot, Code: "custom.handmade", Message: "handmade message"}
	if got := err.Localized(i18n.ZH); got != "handmade message" {
		t.Fatalf("Localized = %q", got)
	}
	var nilErr *APIError
	if got := nilErr.Localized(i18n.ZH); got != "" {
		t.Fatalf("nil Localized = %q", got)
	}
}

// 成功响应没有可读文案，不该被塞上语言头（它会让「这条响应依赖语言」变成谎言，
// 进而让中间缓存把所有 200 也按语言切碎）。
func TestSuccessResponseCarriesNoLanguageHeader(t *testing.T) {
	s, _ := newServer(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/points/d1/p1?lang=zh", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Language"); got != "" {
		t.Fatalf("unexpected Content-Language on success: %q", got)
	}
}
