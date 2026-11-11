package iec61850

import (
	"testing"
)

// 这一组测试覆盖「服务端」一侧的构造/解析函数：内置从站与真实主站必须共用
// 同一套字节语义，所以从站产出的响应必须能被主站的解析函数原样读回、主站
// 产出的请求也必须能被从站的解析函数原样读回。只测「我构造的字节含某个
// 标签」不够——那只证明我按自己的理解写了字节，不证明对端能读懂。

// 读响应往返：成功项与失败项混在一起时，逐项保真。
func TestBuildReadResponseRoundTrip(t *testing.T) {
	in := []AccessResult{
		{OK: true, Data: Data{Tag: tagDataFloat, Float: 2.5, FloatWidth: 32}},
		{OK: true, Data: Data{Tag: tagDataBoolean, Bool: true}},
		{OK: false, Error: DataErrObjectNonExistent},
	}
	raw := BuildReadResponse(11, in)
	invoke, tag, err := ConfirmedInvokeID(raw)
	if err != nil {
		t.Fatalf("ConfirmedInvokeID: %v", err)
	}
	if invoke != 11 || tag != tagConfirmedResponse {
		t.Fatalf("invoke=%d tag=0x%02X, want 11/0xA1", invoke, tag)
	}
	resp, err := ParseReadResponse(raw)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(resp.Results))
	}
	if !resp.Results[0].OK || resp.Results[0].Data.Float != 2.5 {
		t.Errorf("float item = %+v", resp.Results[0])
	}
	if !resp.Results[1].OK || !resp.Results[1].Data.Bool {
		t.Errorf("bool item = %+v", resp.Results[1])
	}
	if resp.Results[2].OK || resp.Results[2].Error != DataErrObjectNonExistent {
		t.Errorf("error item = %+v", resp.Results[2])
	}
}

// 值编不出来时单项降级为错误码，整条响应仍可解析——不能让一个坏值把整页读废。
func TestBuildReadResponseDegradesBadValue(t *testing.T) {
	// 浮点宽度非法（0 会按 32 处理，用 7 触发 encodeFloat 的拒绝路径）。
	bad := Data{Tag: tagDataFloat, Float: 1, FloatWidth: 7}
	raw := BuildReadResponse(1, []AccessResult{{OK: true, Data: bad}})
	resp, err := ParseReadResponse(raw)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].OK {
		t.Fatalf("results = %+v, want one failed item", resp.Results)
	}
	if resp.Results[0].Error != DataErrObjectInvalidated {
		t.Errorf("error = %d, want object-invalid", resp.Results[0].Error)
	}
}

// 写响应往返：成功（81 00）与失败（80 01 <code>）都要保真。
func TestBuildWriteResponseRoundTrip(t *testing.T) {
	raw := BuildWriteResponse(5, []WriteResult{
		{OK: true},
		{OK: false, Error: DataErrAccessDenied},
	})
	invoke, _, err := ConfirmedInvokeID(raw)
	if err != nil {
		t.Fatalf("ConfirmedInvokeID: %v", err)
	}
	if invoke != 5 {
		t.Errorf("invoke = %d, want 5", invoke)
	}
	resp, err := ParseWriteResponse(raw)
	if err != nil {
		t.Fatalf("ParseWriteResponse: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	if !resp.Results[0].OK {
		t.Errorf("first = %+v, want OK", resp.Results[0])
	}
	if resp.Results[1].OK || resp.Results[1].Error != DataErrAccessDenied {
		t.Errorf("second = %+v, want access-denied", resp.Results[1])
	}
}

// 读请求往返：名字列表、ListName 与 WithResult 三条路径都要能原样读回。
func TestParseReadRequestRoundTrip(t *testing.T) {
	names := []ObjectName{
		DomainObject("simpleIO", "GGIO1$ST$Ind1$stVal"),
		DomainObject("simpleIO", "MMXU1$MX$TotW$mag$f"),
	}
	raw, err := BuildReadRequest(21, ReadRequest{Names: names, WithResult: true})
	if err != nil {
		t.Fatalf("BuildReadRequest: %v", err)
	}
	invoke, req, err := ParseReadRequest(raw)
	if err != nil {
		t.Fatalf("ParseReadRequest: %v", err)
	}
	if invoke != 21 {
		t.Errorf("invoke = %d, want 21", invoke)
	}
	if len(req.Names) != 2 {
		t.Fatalf("names = %d, want 2", len(req.Names))
	}
	for i, want := range names {
		if req.Names[i] != want {
			t.Errorf("name[%d] = %+v, want %+v", i, req.Names[i], want)
		}
	}
	if !req.WithResult {
		t.Error("WithResult lost in the round trip")
	}

	// ListName 路径（命名变量表）。
	list := DomainObject("simpleIO", "LLN0$DataSet1")
	raw, err = BuildReadRequest(22, ReadRequest{ListName: &list})
	if err != nil {
		t.Fatalf("BuildReadRequest(list): %v", err)
	}
	_, req, err = ParseReadRequest(raw)
	if err != nil {
		t.Fatalf("ParseReadRequest(list): %v", err)
	}
	if req.ListName == nil || *req.ListName != list {
		t.Errorf("listName = %+v, want %+v", req.ListName, list)
	}
	if len(req.Names) != 0 {
		t.Errorf("names = %v, want empty for a list read", req.Names)
	}
}

// 写请求往返：变量规格与数据列表都编码成 [0]，只能靠出现顺序区分。
func TestParseWriteRequestRoundTrip(t *testing.T) {
	name := DomainObject("simpleIO", "GGIO1$CO$SPCs$stVal")
	raw, err := BuildWriteRequest(31, WriteRequest{
		Names:  []ObjectName{name},
		Values: []Data{{Tag: tagDataBoolean, Bool: true}},
	})
	if err != nil {
		t.Fatalf("BuildWriteRequest: %v", err)
	}
	invoke, req, err := ParseWriteRequest(raw)
	if err != nil {
		t.Fatalf("ParseWriteRequest: %v", err)
	}
	if invoke != 31 {
		t.Errorf("invoke = %d, want 31", invoke)
	}
	if len(req.Names) != 1 || req.Names[0] != name {
		t.Fatalf("names = %+v, want [%+v]", req.Names, name)
	}
	if len(req.Values) != 1 {
		t.Fatalf("values = %d, want 1", len(req.Values))
	}
	if req.Values[0].Tag != tagDataBoolean || !req.Values[0].Bool {
		t.Errorf("value = %+v, want boolean true", req.Values[0])
	}
}

// 模型浏览请求往返：objectClass、域与续读游标三项各自保真。
func TestParseGetNameListRequestRoundTrip(t *testing.T) {
	raw := BuildGetNameListRequest(41, GetNameListRequest{
		Class: ObjectClassNamedVariable, Domain: "simpleIO", ContinueAfter: "GGIO1$ST$Ind1$stVal",
	})
	invoke, req, err := ParseGetNameListRequest(raw)
	if err != nil {
		t.Fatalf("ParseGetNameListRequest: %v", err)
	}
	if invoke != 41 {
		t.Errorf("invoke = %d, want 41", invoke)
	}
	if req.Class != ObjectClassNamedVariable {
		t.Errorf("class = %d, want %d", req.Class, ObjectClassNamedVariable)
	}
	if req.Domain != "simpleIO" {
		t.Errorf("domain = %q, want simpleIO", req.Domain)
	}
	if req.ContinueAfter != "GGIO1$ST$Ind1$stVal" {
		t.Errorf("continueAfter = %q", req.ContinueAfter)
	}

	// 域为空表示整个 VMD：不能把「没写域」误读成「域名为空串的过滤」。
	raw = BuildGetNameListRequest(42, GetNameListRequest{Class: ObjectClassNamedVariable})
	_, req, err = ParseGetNameListRequest(raw)
	if err != nil {
		t.Fatalf("ParseGetNameListRequest(vmd): %v", err)
	}
	if req.Domain != "" {
		t.Errorf("domain = %q, want empty", req.Domain)
	}
}

// 模型浏览响应的更多标志默认值是 TRUE（省略字段即「还有下一页」）。
func TestBuildGetNameListResponseRoundTrip(t *testing.T) {
	raw := ConfirmedResponse(7, BuildGetNameListResponse([]string{"a", "b"}, true))
	resp, err := ParseGetNameListResponse(raw)
	if err != nil {
		t.Fatalf("ParseGetNameListResponse: %v", err)
	}
	if len(resp.Identifiers) != 2 || resp.Identifiers[0] != "a" || resp.Identifiers[1] != "b" {
		t.Errorf("identifiers = %v", resp.Identifiers)
	}
	if !resp.MoreFollows {
		t.Error("moreFollows = false, want true")
	}

	raw = ConfirmedResponse(8, BuildGetNameListResponse([]string{"c"}, false))
	resp, err = ParseGetNameListResponse(raw)
	if err != nil {
		t.Fatalf("ParseGetNameListResponse: %v", err)
	}
	if resp.MoreFollows {
		t.Error("moreFollows = true, want false")
	}
}

// MMSServiceTag 是内置从站的分流依据：请求与响应两种外层都要认。
func TestMMSServiceTag(t *testing.T) {
	req := ConfirmedRequest(1, appendTLV(nil, ServiceTagRead, appendTLV(nil, contextTag(1, true), nil)))
	tag, err := MMSServiceTag(req)
	if err != nil {
		t.Fatalf("MMSServiceTag(request): %v", err)
	}
	if tag != ServiceTagRead {
		t.Errorf("tag = 0x%02X, want read", tag)
	}

	resp := ConfirmedResponse(1, appendTLV(nil, ServiceTagWrite, []byte{0x81, 0x00}))
	tag, err = MMSServiceTag(resp)
	if err != nil {
		t.Fatalf("MMSServiceTag(response): %v", err)
	}
	if tag != ServiceTagWrite {
		t.Errorf("tag = 0x%02X, want write", tag)
	}

	// 非 confirmed 外层（如 initiate）必须报错，而不是猜一个服务标签。
	if _, err := MMSServiceTag([]byte{0xA8, 0x00}); err == nil {
		t.Error("expected an error for a non-confirmed MMS PDU")
	}
}

// ConfirmedResponse 与 ConfirmedRequest 只在最外层标签上不同，其余字节一致。
func TestConfirmedResponseMirrorsRequest(t *testing.T) {
	svc := appendTLV(nil, ServiceTagRead, appendTLV(nil, contextTag(1, true), nil))
	req := ConfirmedRequest(9, svc)
	resp := ConfirmedResponse(9, svc)
	if req[0] != tagConfirmedRequest {
		t.Fatalf("request tag = 0x%02X", req[0])
	}
	if resp[0] != tagConfirmedResponse {
		t.Fatalf("response tag = 0x%02X", resp[0])
	}
	if len(req) != len(resp) {
		t.Fatalf("lengths differ: %d vs %d", len(req), len(resp))
	}
	for i := 1; i < len(req); i++ {
		if req[i] != resp[i] {
			t.Fatalf("byte %d differs: 0x%02X vs 0x%02X", i, req[i], resp[i])
		}
	}
}

// 请求解析函数遇到错误的服务标签必须报错，不能把别家的报文当成自己的。
func TestParseRequestsRejectWrongService(t *testing.T) {
	writeReq, err := BuildWriteRequest(1, WriteRequest{
		Names:  []ObjectName{DomainObject("ld", "ln$ST$do")},
		Values: []Data{{Tag: tagDataBoolean, Bool: true}},
	})
	if err != nil {
		t.Fatalf("BuildWriteRequest: %v", err)
	}
	if _, _, err := ParseReadRequest(writeReq); err == nil {
		t.Error("ParseReadRequest accepted a write request")
	}

	readReq, err := BuildReadRequest(2, ReadRequest{Names: []ObjectName{DomainObject("ld", "ln$ST$do")}})
	if err != nil {
		t.Fatalf("BuildReadRequest: %v", err)
	}
	if _, _, err := ParseWriteRequest(readReq); err == nil {
		t.Error("ParseWriteRequest accepted a read request")
	}
	if _, _, err := ParseGetNameListRequest(readReq); err == nil {
		t.Error("ParseGetNameListRequest accepted a read request")
	}
}
