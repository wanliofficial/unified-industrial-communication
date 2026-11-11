package iec61850

import (
	"errors"
	"fmt"
)

// 本文件实现 ISO 8823 表示层里 MMS 用到的部分：CP（连接）、CPA（接受）、
// 用户数据（Fully-Encoded-Data）与 AB（中止）。
//
// 表示层在 MMS 里承担一件具体的事：**声明两个表示上下文**。上下文 1 是
// ACSE（应用层关联控制），上下文 3 是 MMS（制造报文规范）。关联建立时，
// ACSE 的 AARQ 走上下文 1，MMS 的 Initiate 走上下文 3——同一个字节流里
// 两种语法靠这个编号区分。写错编号不会立刻报错，而是让对端用错误的语法
// 去解析用户数据，表现为「关联建立后第一次读就解不开」。
//
// 另一个细节：调用侧与被调侧的 P-Selector 必须**成对出现**，哪怕长度为 0
// 也要写出 `81 00` 与 `82 00` 两个字段。省掉它们会让一部分实现在解析
// normal-mode-parameters 时把后一个字段读成前一个。

// 表示层上下文编号。
const (
	// ACSEContextID 是 ACSE 的表示上下文编号。
	ACSEContextID = 1
	// MMSContextID 是 MMS 的表示上下文编号。
	MMSContextID = 3
)

// 抽象语法 OID 的内容字节。
var (
	// asnIDACSE 是 ACSE 的抽象语法 2.2.1.0.1。
	asnIDACSE = []byte{0x52, 0x01, 0x00, 0x01}
	// asnIDMMS 是 MMS 的抽象语法 1.0.9506.2.1。
	asnIDMMS = []byte{0x28, 0xca, 0x22, 0x02, 0x01}
	// berID 是 BER 传输语法 2.1.1。表示层只支持这一种传输语法。
	berID = []byte{0x51, 0x01}
)

// 表示层的固定值。
var (
	// defaultPSel 是 IEC 61850 的默认 P-Selector。
	defaultPSel = []byte{0x00, 0x00, 0x00, 0x01}
	// modeSelectorNormal 是 mode-selector 的 normal-mode（1）。
	modeSelectorNormal = []byte{0xA0, 0x03, 0x80, 0x01, 0x01}
)

// PresentationParams 是表示层选择子。
type PresentationParams struct {
	// CallingPSel 是调用侧 P-Selector。
	CallingPSel []byte
	// CalledPSel 是被调侧 P-Selector。
	CalledPSel []byte
}

// DefaultPresentationParams 返回默认选择子。
func DefaultPresentationParams() PresentationParams {
	return PresentationParams{
		CallingPSel: defaultPSel,
		CalledPSel:  defaultPSel,
	}
}

// encodeContextItem 写一个表示上下文定义项。
//
// 内容 = context-id(INTEGER) + abstract-syntax(OID) + transfer-syntax-list。
func encodeContextItem(dst []byte, ctxID byte, oid []byte) []byte {
	ts := appendTLV(nil, tagSequence, appendTLV(nil, tagOID, berID))
	content := appendTLV(nil, tagInt, []byte{ctxID})
	content = appendTLV(content, tagOID, oid)
	content = append(content, ts...)
	return appendTLV(dst, tagSequence, content)
}

// BuildPresentationConnect 生成 CP PPDU。
func BuildPresentationConnect(p PresentationParams, payload []byte) ([]byte, error) {
	calling := p.CallingPSel
	if calling == nil {
		calling = defaultPSel
	}
	called := p.CalledPSel
	if called == nil {
		called = defaultPSel
	}
	if len(calling) > 16 || len(called) > 16 {
		return nil, errors.New("iec61850: presentation selector longer than 16 bytes")
	}

	ctxList := encodeContextItem(nil, ACSEContextID, asnIDACSE)
	ctxList = encodeContextItem(ctxList, MMSContextID, asnIDMMS)

	// user-data：0x61 { PDV-list 0x30 { context-id, 0xA0 { payload } } }
	pdv := appendTLV(nil, tagInt, []byte{ACSEContextID})
	pdv = appendTLV(pdv, contextTag(0, true), payload)
	userData := appendTLV(nil, 0x61, appendTLV(nil, tagSequence, pdv))

	nmp := append([]byte{0x81, byte(len(calling))}, calling...)
	nmp = append(nmp, 0x82, byte(len(called)))
	nmp = append(nmp, called...)
	nmp = appendTLV(nmp, 0xA4, ctxList)
	nmp = append(nmp, userData...)

	out := append([]byte(nil), modeSelectorNormal...)
	out = appendTLV(out, 0xA2, nmp)
	return appendTLV(nil, tagSequence|0x01, out), nil
}

// BuildPresentationAccept 生成 CPA PPDU。
//
// 结果列表按 CP 里提出的上下文顺序回答：先 ACSE 后 MMS，每个都是
// 「接受 + BER 传输语法」。
func BuildPresentationAccept(payload []byte) ([]byte, error) {
	// 每一项：30 07 { 80 01 00, 81 02 51 01 }
	resultItem := []byte{tagSequence, 0x07, 0x80, 0x01, 0x00, 0x81, 0x02, berID[0], berID[1]}
	results := append([]byte(nil), resultItem...)
	results = append(results, resultItem...)
	resultList := appendTLV(nil, 0xA5, results)

	pdv := appendTLV(nil, tagInt, []byte{ACSEContextID})
	pdv = appendTLV(pdv, contextTag(0, true), payload)
	userData := appendTLV(nil, 0x61, appendTLV(nil, tagSequence, pdv))

	nmp := []byte{0x83, 0x04, 0x00, 0x00, 0x00, 0x01}
	nmp = append(nmp, resultList...)
	nmp = append(nmp, userData...)

	out := append([]byte(nil), modeSelectorNormal...)
	out = appendTLV(out, 0xA2, nmp)
	return appendTLV(nil, tagSequence|0x01, out), nil
}

// BuildPresentationUserData 生成带 MMS 上下文编号的用户数据。
func BuildPresentationUserData(payload []byte) ([]byte, error) {
	return buildPresentationUserData(MMSContextID, payload)
}

// BuildPresentationUserDataACSE 生成带 ACSE 上下文编号的用户数据。
func BuildPresentationUserDataACSE(payload []byte) ([]byte, error) {
	return buildPresentationUserData(ACSEContextID, payload)
}

func buildPresentationUserData(ctxID byte, payload []byte) ([]byte, error) {
	pdv := appendTLV(nil, tagInt, []byte{ctxID})
	pdv = appendTLV(pdv, contextTag(0, true), payload)
	return appendTLV(nil, 0x61, appendTLV(nil, tagSequence, pdv)), nil
}

// BuildPresentationAbort 生成 AB PPDU。
func BuildPresentationAbort(payload []byte) ([]byte, error) {
	inner, err := BuildPresentationUserDataACSE(payload)
	if err != nil {
		return nil, err
	}
	return appendTLV(nil, contextTag(0, true), inner), nil
}

// PresentationData 是解析出来的表示层报文。
type PresentationData struct {
	// Kind 是 "cp" / "cpa" / "ab" / "user-data" / "arp" / "unknown"。
	Kind string
	// UserData 是用户的编码数据（CP/CPA 里只有 ACSE 那一段）。
	UserData []byte
	// ContextID 是 user-data 里的表示上下文编号。
	ContextID byte
	// HasACSE / HasMMS 表示 CP 里提出了哪些上下文。
	HasACSE bool
	HasMMS  bool
	// Reason 是 ARP（提供方拒绝）里的原因码。
	Reason byte
}

// ParsePresentation 解析一个表示层报文。
//
// 只做「取出用户数据」这一件事：CP 里的上下文定义只记录「有没有 MMS」，
// 不逐项校验传输语法。真正的协商结果对客户端没有更多用处——只要对端接了
// 关联，上下文就是可用的；拒绝会直接体现在 AARE 的结果里。
func ParsePresentation(b []byte) (PresentationData, error) {
	e, err := parseElement(b)
	if err != nil {
		return PresentationData{}, err
	}
	switch e.tag {
	case tagSequence | 0x01: // 0x31：CP 与 CPA 共用
		return parseConnectPresentation(e.data)
	case contextTag(0, true): // 0xA0：AB（以及一部分实现的 ARP）
		return parsePresentationAbort(e.data)
	case contextTag(1, true): // 0xA1：ARP（提供方拒绝）
		out := PresentationData{Kind: "arp"}
		if n, _, ok := isContext(firstTag(e.data)); ok {
			out.Reason = n
		}
		return out, nil
	case 0x61: // 部分对端把用户数据直接作为顶层
		return parseFullyEncodedData(e.data)
	}
	return PresentationData{Kind: "unknown"}, nil
}

func firstTag(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// parseConnectPresentation 解析 CP/CPA 的内容。
func parseConnectPresentation(body []byte) (PresentationData, error) {
	out := PresentationData{Kind: "cp"}
	// 内容 = mode-selector + normal-mode-parameters(0xA2)。
	for len(body) > 0 {
		e, err := parseElement(body)
		if err != nil {
			return PresentationData{}, err
		}
		body = e.rest
		if e.tag != 0xA2 {
			continue
		}
		if err := parseNormalModeParams(e.data, &out); err != nil {
			return PresentationData{}, err
		}
	}
	return out, nil
}

// parseNormalModeParams 解析 normal-mode-parameters。
func parseNormalModeParams(body []byte, out *PresentationData) error {
	for len(body) > 0 {
		e, err := parseElement(body)
		if err != nil {
			return err
		}
		body = e.rest
		switch e.tag {
		case 0xA4: // presentation-context-definition-list
			parseContextList(e.data, out)
		case 0xA5: // presentation-context-definition-result-list
			out.Kind = "cpa"
			if res := firstResultRejected(e.data); res {
				// 有上下文被拒：关联本身也许建立了，但 MMS 用不了。
				// 把原因带出去，比让后续每次读都超时有用。
				out.Reason = 1
			}
		case 0x61: // user-data
			d, err := parseFullyEncodedData(e.data)
			if err != nil {
				return err
			}
			out.UserData = d.UserData
			out.ContextID = d.ContextID
		}
	}
	return nil
}

// parseContextList 记录 CP 里提出了哪些上下文。
func parseContextList(body []byte, out *PresentationData) {
	for len(body) > 0 {
		e, err := parseElement(body)
		if err != nil {
			return
		}
		body = e.rest
		if e.tag != tagSequence {
			continue
		}
		inner := e.data
		for len(inner) > 0 {
			f, err := parseElement(inner)
			if err != nil {
				return
			}
			inner = f.rest
			if f.tag != tagInt || len(f.data) == 0 {
				continue
			}
			switch f.data[0] {
			case ACSEContextID:
				out.HasACSE = true
			case MMSContextID:
				out.HasMMS = true
			}
		}
	}
}

// firstResultRejected 判断结果列表里是否有「拒绝」项。
//
// 接受项形如 30 07 80 01 00 …, 拒绝项的第一个字段是 80 01 02。只看第一项
// 就够：上下文被拒时对端不会假装接了一个又拒一个。
func firstResultRejected(body []byte) bool {
	e, err := parseElement(body)
	if err != nil || e.tag != tagSequence || len(e.data) < 3 {
		return false
	}
	return e.data[0] == 0x80 && e.data[1] == 0x01 && e.data[2] != 0x00
}

// parseFullyEncodedData 解析 0x61 里的 PDV 列表。
func parseFullyEncodedData(body []byte) (PresentationData, error) {
	out := PresentationData{Kind: "user-data"}
	e, err := parseElement(body)
	if err != nil {
		return out, err
	}
	if e.tag != tagSequence {
		return out, fmt.Errorf("iec61850: presentation pdv-list tag 0x%02X, want SEQUENCE", e.tag)
	}
	inner := e.data
	for len(inner) > 0 {
		f, err := parseElement(inner)
		if err != nil {
			return out, err
		}
		inner = f.rest
		switch {
		case f.tag == tagInt:
			if len(f.data) > 0 {
				out.ContextID = f.data[0]
			}
		case f.tag == contextTag(0, true):
			out.UserData = f.data
		}
	}
	return out, nil
}

// parsePresentationAbort 解析 AB PPDU。
func parsePresentationAbort(body []byte) (PresentationData, error) {
	e, err := parseElement(body)
	if err != nil {
		return PresentationData{}, err
	}
	if e.tag != 0x61 {
		return PresentationData{Kind: "ab"}, nil
	}
	d, err := parseFullyEncodedData(e.data)
	if err != nil {
		return PresentationData{}, err
	}
	out := PresentationData{Kind: "ab", UserData: d.UserData, ContextID: d.ContextID}
	// AB 的用户数据是 ACSE 的 AARQ/AARE 形态，里面可能有诊断码。这里
	// 不做深入解析：调用方需要的是「对端中止了关联」这个事实。
	if len(out.UserData) >= 2 {
		out.Reason = out.UserData[1]
	}
	return out, nil
}
