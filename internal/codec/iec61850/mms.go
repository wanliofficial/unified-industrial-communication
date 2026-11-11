package iec61850

import (
	"errors"
	"fmt"
	"strings"
)

// 本文件实现 ISO 9506（MMS）的 PDU 层：Initiate、Read、Write、GetNameList、
// GetVariableAccessAttributes、Conclude，以及 Confirmed-Error 与 Reject。
//
// MMS 的 PDU 有两种外层：confirmed-request（0xA0）与 confirmed-response
// （0xA1），里面第一项永远是 invokeID，第二项是服务体。**invokeID 是本层
// 唯一的配对机制**：一条连接上可以同时有多个在途请求，靠它把响应分回对应
// 的请求。不校验 invokeID 就接受第一个响应，会在「前一次请求超时后迟到的
// 响应」到达时把 A 的值写给 B——那是最难复现的一类事故。
//
// 服务标签（与 ASN.1 的上下文编号一致）：
//
//	0xA1 getNameList            0xA4 read
//	0xA5 write                  0xA6 getVariableAccessAttributes
//	0xA8 initiate               0xA9 initiate-response
//	0xAA conclude               0xAB conclude-response
//	0xA2 confirmed-error        0xA3 reject

// MMS 服务标签。
const (
	ServiceTagGetNameList      = 0xA1
	ServiceTagRead             = 0xA4
	ServiceTagWrite            = 0xA5
	ServiceTagVarAccessAttrs   = 0xA6
	ServiceTagInitiate         = 0xA8
	ServiceTagInitiateResponse = 0xA9
	ServiceTagConclude         = 0xAA
	ServiceTagConcludeResponse = 0xAB
	ServiceTagConfirmedError   = 0xA2
	ServiceTagReject           = 0xA3
)

// confirmed-request / response 的外层标签。
const (
	tagConfirmedRequest  = 0xA0
	tagConfirmedResponse = 0xA1
)

// MMS 协议版本号，固定为 1。
const MMSVersion = 1

// DefaultMaxPDUSize 是本端提议的最大 MMS PDU 尺寸。
//
// 取 65000 而不是更大的值：COTP 的 TPDU 上限是 8192，超过它的 PDU 必须在
// COTP 层分片；本实现不分片，因此实际可用上限由协商出的 TPDU 大小决定。
// 65000 是标准里给出的默认值，真机普遍接受。
const DefaultMaxPDUSize = 65000

// DefaultMaxOutstanding 是本端提议的最大在途请求数。
const DefaultMaxOutstanding = 5

// DefaultNestingLevel 是提议的最大数据嵌套层数。
const DefaultNestingLevel = 10

// parameterCBB 是本端支持的服务参数位图（11 位，含填充字节）。
//
// 0xF1 0x00 覆盖 str1/str2/vnam/valt/vlis（结构、变长数组、变量名、
// 变长整数、变长位串）。这些位声明「我声明自己支持什么类型的参数」，
// 写少了会让对端在需要用到时改用更保守的编码，写多了则可能收到解不开的报文。
var parameterCBB = []byte{0xF1, 0x00}

// servicesSupported 是本端支持的服务位图（85 位，11 字节）。
//
// 位序与服务标签无关，是标准里一张固定的表。这里声明的集合与驱动实际实现
// 的服务一致：读、写、模型浏览、状态、标识、命名变量表。声明一个不实现的
// 服务，对端可能会用它来回应，然后我们只能报「不支持」。
var servicesSupported = []byte{
	0xEE, // status | getNameList | identify | read | write | getVarAccessAttrs
	0x1C, // defineNamedVariableList | getNamedVariableListAttrs | deleteNamedVariableList
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x40, // readJournal
	0x01, // informationReport
	0x10, // conclude
}

// ---------------------------------------------------------------------------
// Initiate
// ---------------------------------------------------------------------------

// InitiateRequest 是 MMS 的 Initiate 请求参数。
type InitiateRequest struct {
	// MaxPDUSize 是本端能接收的最大 PDU。
	MaxPDUSize int
	// MaxOutstandingCalling 是本端允许的对端在途请求数。
	MaxOutstandingCalling int
	// MaxOutstandingCalled 是本端自己会发出的最大在途请求数。
	MaxOutstandingCalled int
	// NestingLevel 是提议的最大嵌套层数。
	NestingLevel int
}

// DefaultInitiateRequest 返回默认参数。
func DefaultInitiateRequest() InitiateRequest {
	return InitiateRequest{
		MaxPDUSize:            DefaultMaxPDUSize,
		MaxOutstandingCalling: DefaultMaxOutstanding,
		MaxOutstandingCalled:  DefaultMaxOutstanding,
		NestingLevel:          DefaultNestingLevel,
	}
}

// BuildInitiate 生成 Initiate-RequestPDU。
func BuildInitiate(req InitiateRequest) []byte {
	if req.MaxPDUSize <= 0 {
		req.MaxPDUSize = DefaultMaxPDUSize
	}
	if req.NestingLevel <= 0 {
		req.NestingLevel = DefaultNestingLevel
	}
	var inner []byte
	inner = appendTLV(inner, contextTag(0, false), encodeUnsigned(uint64(req.MaxPDUSize)))
	inner = appendTLV(inner, contextTag(1, false), encodeUnsigned(uint64(req.MaxOutstandingCalling)))
	inner = appendTLV(inner, contextTag(2, false), encodeUnsigned(uint64(req.MaxOutstandingCalled)))
	inner = appendTLV(inner, contextTag(3, false), []byte{byte(req.NestingLevel)})

	var detail []byte
	detail = appendTLV(detail, contextTag(0, false), []byte{MMSVersion})
	detail = appendTLV(detail, contextTag(1, false), encodeBitString(parameterCBB, 5))
	detail = appendTLV(detail, contextTag(2, false), encodeBitString(servicesSupported, 3))
	inner = appendTLV(inner, contextTag(4, true), detail)

	return appendTLV(nil, ServiceTagInitiate, inner)
}

// InitiateResponse 是解析出来的 Initiate 响应。
type InitiateResponse struct {
	MaxPDUSize            int
	MaxOutstandingCalling int
	MaxOutstandingCalled  int
	NestingLevel          int
	Version               int
}

// ParseInitiateResponse 解析 Initiate-ResponsePDU。
func ParseInitiateResponse(b []byte) (InitiateResponse, error) {
	e, err := parseElement(b)
	if err != nil {
		return InitiateResponse{}, err
	}
	if e.tag != ServiceTagInitiateResponse {
		return InitiateResponse{}, fmt.Errorf("iec61850: mms tag 0x%02X, want initiate-response (0x%02X)", e.tag, ServiceTagInitiateResponse)
	}
	out := InitiateResponse{MaxPDUSize: DefaultMaxPDUSize, NestingLevel: DefaultNestingLevel, Version: MMSVersion}
	rest := e.data
	for len(rest) > 0 {
		f, err := parseElement(rest)
		if err != nil {
			return out, err
		}
		rest = f.rest
		num, constructed, ok := isContext(f.tag)
		if !ok {
			continue
		}
		switch {
		case num == 0 && !constructed:
			v, err := decodeUnsigned(f.data)
			if err != nil {
				return out, err
			}
			out.MaxPDUSize = int(v)
		case num == 1 && !constructed:
			v, err := decodeUnsigned(f.data)
			if err != nil {
				return out, err
			}
			out.MaxOutstandingCalling = int(v)
		case num == 2 && !constructed:
			v, err := decodeUnsigned(f.data)
			if err != nil {
				return out, err
			}
			out.MaxOutstandingCalled = int(v)
		case num == 3 && !constructed:
			if len(f.data) > 0 {
				out.NestingLevel = int(f.data[0])
			}
		case num == 4 && constructed:
			parseInitResponseDetail(f.data, &out)
		}
	}
	if out.MaxPDUSize <= 0 {
		out.MaxPDUSize = DefaultMaxPDUSize
	}
	return out, nil
}

// parseInitResponseDetail 解析 InitResponseDetail。
func parseInitResponseDetail(b []byte, out *InitiateResponse) {
	for len(b) > 0 {
		e, err := parseElement(b)
		if err != nil {
			return
		}
		b = e.rest
		num, constructed, ok := isContext(e.tag)
		if !ok || constructed {
			continue
		}
		switch num {
		case 0:
			if v, err := decodeUnsigned(e.data); err == nil {
				out.Version = int(v)
			}
		case 3:
			if len(e.data) > 0 {
				out.NestingLevel = int(e.data[0])
			}
		}
	}
}

// BuildInitiateResponse 生成 Initiate-ResponsePDU（内置从站用）。
func BuildInitiateResponse(req InitiateRequest) []byte {
	if req.MaxPDUSize <= 0 {
		req.MaxPDUSize = DefaultMaxPDUSize
	}
	if req.NestingLevel <= 0 {
		req.NestingLevel = DefaultNestingLevel
	}
	var inner []byte
	inner = appendTLV(inner, contextTag(0, false), encodeUnsigned(uint64(req.MaxPDUSize)))
	inner = appendTLV(inner, contextTag(1, false), encodeUnsigned(uint64(req.MaxOutstandingCalling)))
	inner = appendTLV(inner, contextTag(2, false), encodeUnsigned(uint64(req.MaxOutstandingCalled)))
	inner = appendTLV(inner, contextTag(3, false), []byte{byte(req.NestingLevel)})

	var detail []byte
	detail = appendTLV(detail, contextTag(0, false), []byte{MMSVersion})
	detail = appendTLV(detail, contextTag(1, false), encodeBitString(parameterCBB, 5))
	detail = appendTLV(detail, contextTag(2, false), encodeBitString(servicesSupported, 3))
	inner = appendTLV(inner, contextTag(4, true), detail)

	return appendTLV(nil, ServiceTagInitiateResponse, inner)
}

// ---------------------------------------------------------------------------
// ObjectName 与 VariableAccessSpecification
// ---------------------------------------------------------------------------

// ObjectName 是 MMS 的对象名。
//
// IEC 61850 的变量名形如 `LD/LN.DO.DA`，在线格式上被拆成 domain
// （`LD`）与 item（`LN$DO$DA`）两部分，分隔符从 `/` 与 `.` 变成 `$`。
// 这个转换是本协议最容易出错的地方之一：写错分隔符不会报错，只会得到
// object-non-existent，而人看到的名字与报文里的名字长得几乎一样。
type ObjectName struct {
	// Kind 是 "vmd" / "domain" / "aa"。
	Kind string
	// Domain 是域（逻辑设备）名。
	Domain string
	// Item 是域内的项名（`LLN0$MX$TotW$mag$f`）。
	Item string
	// Name 是 vmd/aa 形式的整名。
	Name string
}

// DomainObject 构造一个域内对象名。
func DomainObject(domain, item string) ObjectName {
	return ObjectName{Kind: "domain", Domain: domain, Item: item}
}

// String 返回可读的对象名（`LD/LLN0.MX.TotW.mag.f`）。
func (o ObjectName) String() string {
	switch o.Kind {
	case "domain":
		return o.Domain + "/" + strings.ReplaceAll(o.Item, "$", ".")
	case "vmd", "aa":
		return o.Name
	}
	return ""
}

// MMSName 返回报文里的形式（`LD$LLN0$MX$TotW$mag$f`），用于日志对照。
func (o ObjectName) MMSName() string {
	switch o.Kind {
	case "domain":
		return o.Domain + "$" + o.Item
	}
	return o.Name
}

// Encode 写 ObjectName。
func (o ObjectName) Encode() []byte {
	switch o.Kind {
	case "vmd":
		return appendTLV(nil, contextTag(0, false), []byte(o.Name))
	case "aa":
		return appendTLV(nil, contextTag(2, false), []byte(o.Name))
	}
	var inner []byte
	inner = appendTLV(inner, tagVisibleChars, []byte(o.Domain))
	inner = appendTLV(inner, tagVisibleChars, []byte(o.Item))
	return appendTLV(nil, contextTag(1, true), inner)
}

// DecodeObjectName 解析 ObjectName。
func DecodeObjectName(b []byte) (ObjectName, error) {
	e, err := parseElement(b)
	if err != nil {
		return ObjectName{}, err
	}
	num, constructed, ok := isContext(e.tag)
	if !ok {
		return ObjectName{}, fmt.Errorf("iec61850: object name tag 0x%02X is not a context tag", e.tag)
	}
	switch {
	case num == 0 && !constructed:
		return ObjectName{Kind: "vmd", Name: string(e.data)}, nil
	case num == 2 && !constructed:
		return ObjectName{Kind: "aa", Name: string(e.data)}, nil
	case num == 1 && constructed:
		rest := e.data
		var domain, item string
		for len(rest) > 0 {
			f, err := parseElement(rest)
			if err != nil {
				return ObjectName{}, err
			}
			rest = f.rest
			if f.tag != tagVisibleChars {
				continue
			}
			if domain == "" {
				domain = string(f.data)
			} else {
				item = string(f.data)
			}
		}
		if domain == "" || item == "" {
			return ObjectName{}, errors.New("iec61850: domain-specific object name is missing a part")
		}
		return ObjectName{Kind: "domain", Domain: domain, Item: item}, nil
	}
	return ObjectName{}, fmt.Errorf("iec61850: unsupported object name tag 0x%02X", e.tag)
}

// ReadRequest 是一次读请求：一组变量或一个命名变量表。
type ReadRequest struct {
	// Names 是变量名列表（listOfVariable）。
	Names []ObjectName
	// ListName 是命名变量表（variableListName）。非空时 Names 被忽略。
	ListName *ObjectName
	// WithResult 表示希望响应里回显访问规格（仅命名变量表路径有意义）。
	WithResult bool
}

// encodeVariableAccessSpec 写 VariableAccessSpecification。
func encodeVariableAccessSpec(dst []byte, names []ObjectName, listName *ObjectName) []byte {
	if listName != nil {
		return appendTLV(dst, contextTag(1, true), listName.Encode())
	}
	var inner []byte
	for _, n := range names {
		var seq []byte
		seq = appendTLV(seq, contextTag(0, true), n.Encode())
		inner = append(inner, appendTLV(nil, tagSequence, seq)...)
	}
	return appendTLV(dst, contextTag(0, true), inner)
}

// ---------------------------------------------------------------------------
// confirmed 外层
// ---------------------------------------------------------------------------

// ConfirmedRequest 组装一个 confirmed-RequestPDU。
func ConfirmedRequest(invokeID uint32, service []byte) []byte {
	var inner []byte
	inner = appendTLV(inner, tagInt, encodeUnsigned(uint64(invokeID)))
	inner = append(inner, service...)
	return appendTLV(nil, tagConfirmedRequest, inner)
}

// ConfirmedResponse 组装一个 confirmed-ResponsePDU（内置从站用）。
//
// 与 ConfirmedRequest 对称：外层标签换成 0xA1，invokeID 仍在第一项。从站
// 回读/写响应都从这里出，保证 invokeID 的位置与请求侧一致——主站靠它配对。
func ConfirmedResponse(invokeID uint32, service []byte) []byte {
	var inner []byte
	inner = appendTLV(inner, tagInt, encodeUnsigned(uint64(invokeID)))
	inner = append(inner, service...)
	return appendTLV(nil, tagConfirmedResponse, inner)
}

// BuildReadRequest 生成一个完整的读请求。
func BuildReadRequest(invokeID uint32, req ReadRequest) ([]byte, error) {
	if len(req.Names) == 0 && req.ListName == nil {
		return nil, errors.New("iec61850: read request has no variable")
	}
	var body []byte
	if req.WithResult {
		body = appendTLV(body, contextTag(0, false), []byte{0xFF})
	}
	body = encodeVariableAccessSpec(body, req.Names, req.ListName)
	return ConfirmedRequest(invokeID, appendTLV(nil, ServiceTagRead, body)), nil
}

// ParseReadRequest 解析一个读请求，返回 invokeID 与请求体（内置从站用）。
//
// 与 BuildReadRequest 对称：剥掉 confirmed-request 外层，取 read 服务体，再把
// listOfVariable / variableListName 拆出来。单项里带不带 WithResult 不影响从站
// 取值，因此这里只记录、不据此改变行为。
func ParseReadRequest(b []byte) (uint32, ReadRequest, error) {
	invoke, service, err := splitConfirmed(b, tagConfirmedRequest)
	if err != nil {
		return 0, ReadRequest{}, err
	}
	if service.tag != ServiceTagRead {
		return 0, ReadRequest{}, fmt.Errorf("iec61850: mms service 0x%02X, want read (0x%02X)", service.tag, ServiceTagRead)
	}
	var req ReadRequest
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return 0, ReadRequest{}, err
		}
		rest = e.rest
		num, constructed, ok := isContext(e.tag)
		if !ok {
			continue
		}
		switch {
		case num == 0 && !constructed && len(e.data) == 1 && e.data[0] == 0xFF:
			req.WithResult = true
		case num == 0 && constructed:
			// listOfVariable：SEQUENCE OF { SEQUENCE { [0] ObjectName } }。
			items := e.data
			for len(items) > 0 {
				seq, err := parseElement(items)
				if err != nil {
					return 0, ReadRequest{}, err
				}
				items = seq.rest
				inner := seq.data
				for len(inner) > 0 {
					f, err := parseElement(inner)
					if err != nil {
						return 0, ReadRequest{}, err
					}
					inner = f.rest
					if n, _, ok := isContext(f.tag); ok && n == 0 {
						name, err := DecodeObjectName(f.data)
						if err != nil {
							return 0, ReadRequest{}, err
						}
						req.Names = append(req.Names, name)
					}
				}
			}
		case num == 1 && constructed:
			// variableListName [1] { ObjectName }。
			name, err := DecodeObjectName(e.data)
			if err != nil {
				return 0, ReadRequest{}, err
			}
			req.ListName = &name
		}
	}
	return invoke, req, nil
}

// AccessResult 是一次读的结果：要么有值，要么有数据访问错误。
type AccessResult struct {
	// OK 表示这一项读成功。
	OK bool
	// Data 是成功时的值。
	Data Data
	// Error 是失败时的数据访问错误码。
	Error uint8
	// Item 是出错的对象名（由调用方按请求顺序回填）。
	Item string
}

// Err 把失败的结果转成 DataError。
func (a AccessResult) Err() error {
	if a.OK {
		return nil
	}
	return &DataError{Code: a.Error, Item: a.Item}
}

// ReadResponse 是解析出来的读响应。
type ReadResponse struct {
	Results []AccessResult
}

// ParseReadResponse 解析读响应。
//
// 单项失败**不是**整个响应失败：MMS 允许一部分变量成功、一部分带错误码。
// 把整条响应判失败会让「一个点位配错了」变成「整轮采集全丢」。
func ParseReadResponse(b []byte) (ReadResponse, error) {
	invoke, service, err := splitConfirmed(b, tagConfirmedResponse)
	if err != nil {
		return ReadResponse{}, err
	}
	_ = invoke
	if service.tag != ServiceTagRead {
		return ReadResponse{}, fmt.Errorf("iec61850: response service 0x%02X, want read (0x%02X)", service.tag, ServiceTagRead)
	}
	var out ReadResponse
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return ReadResponse{}, err
		}
		rest = e.rest
		// [1] listOfAccessResult，IMPLICIT SEQUENCE OF
		if num, constructed, ok := isContext(e.tag); !ok || num != 1 || !constructed {
			continue
		}
		items := e.data
		for len(items) > 0 {
			item, n, err := parseAccessResult(items)
			if err != nil {
				return ReadResponse{}, err
			}
			out.Results = append(out.Results, item)
			items = items[n:]
		}
	}
	if len(out.Results) == 0 {
		return ReadResponse{}, errors.New("iec61850: read response carries no access result")
	}
	return out, nil
}

// BuildReadResponse 生成一个读响应（内置从站用）。
//
// 结构：confirmed-response { invokeID, read [A4] { [1] listOfAccessResult } }。
// 每一项要么是成功 Data，要么是 `[0] <code>` 失败项。单项失败不使整个响应
// 失败——这与请求侧的 ParseReadResponse 对称。
func BuildReadResponse(invokeID uint32, results []AccessResult) []byte {
	var list []byte
	for _, r := range results {
		if r.OK {
			enc, err := r.Data.Encode()
			if err != nil {
				// 值编不出来等价于这一项读失败：用 object-invalid 表达，比让
				// 整个响应构造失败更贴近「一个点位坏了」的语义。
				list = append(list, contextTag(0, false), 0x01, DataErrObjectInvalidated)
				continue
			}
			list = append(list, enc...)
		} else {
			list = appendTLV(list, contextTag(0, false), []byte{r.Error})
		}
	}
	body := appendTLV(nil, contextTag(1, true), list) // [1] listOfAccessResult
	return ConfirmedResponse(invokeID, appendTLV(nil, ServiceTagRead, body))
}
func parseAccessResult(b []byte) (AccessResult, int, error) {
	if len(b) > 0 && b[0] == contextTag(0, false) {
		// 失败：0x80 <len> <code>
		e, err := parseElement(b)
		if err != nil {
			return AccessResult{}, 0, err
		}
		if len(e.data) == 0 {
			return AccessResult{}, 0, errors.New("iec61850: data access error without a code")
		}
		return AccessResult{OK: false, Error: e.data[0]}, len(b) - len(e.rest), nil
	}
	d, n, err := DecodeData(b)
	if err != nil {
		return AccessResult{}, 0, err
	}
	return AccessResult{OK: true, Data: d}, n, nil
}

// WriteRequest 是一次写请求。
//
// 统一用 listOfVariable 形式：命名变量表的写虽然更省字节，但它的语义是
// 「整表一起写」，而点表侧通常是单个点位写入。
type WriteRequest struct {
	Names  []ObjectName
	Values []Data
}

// BuildWriteRequest 生成一个完整的写请求。
func BuildWriteRequest(invokeID uint32, req WriteRequest) ([]byte, error) {
	if len(req.Names) == 0 {
		return nil, errors.New("iec61850: write request has no variable")
	}
	if len(req.Names) != len(req.Values) {
		return nil, fmt.Errorf("iec61850: write request has %d names but %d values",
			len(req.Names), len(req.Values))
	}
	body := encodeVariableAccessSpec(nil, req.Names, nil)
	var data []byte
	for _, v := range req.Values {
		enc, err := v.Encode()
		if err != nil {
			return nil, err
		}
		data = append(data, enc...)
	}
	body = appendTLV(body, contextTag(0, true), data)
	return ConfirmedRequest(invokeID, appendTLV(nil, ServiceTagWrite, body)), nil
}

// ParseWriteRequest 解析一个写请求，返回 invokeID、变量名与值（内置从站用）。
//
// 写请求体里 listOfVariable 与 listOfData **都**编码成 `[0]`（0xA0），只能按出现
// 顺序区分：第一个 `[0]` 是变量规格，第二个 `[0]` 是数据列表。这里按顺序归位，
// 与 BuildWriteRequest 的产出严格对称。
func ParseWriteRequest(b []byte) (uint32, WriteRequest, error) {
	invoke, service, err := splitConfirmed(b, tagConfirmedRequest)
	if err != nil {
		return 0, WriteRequest{}, err
	}
	if service.tag != ServiceTagWrite {
		return 0, WriteRequest{}, fmt.Errorf("iec61850: mms service 0x%02X, want write (0x%02X)", service.tag, ServiceTagWrite)
	}
	var req WriteRequest
	var sawSpec, sawData bool
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return 0, WriteRequest{}, err
		}
		rest = e.rest
		num, constructed, ok := isContext(e.tag)
		if !ok || !constructed || num != 0 {
			continue
		}
		if !sawSpec {
			// listOfVariable：SEQUENCE OF { SEQUENCE { [0] ObjectName } }。
			items := e.data
			for len(items) > 0 {
				seq, err := parseElement(items)
				if err != nil {
					return 0, WriteRequest{}, err
				}
				items = seq.rest
				inner := seq.data
				for len(inner) > 0 {
					f, err := parseElement(inner)
					if err != nil {
						return 0, WriteRequest{}, err
					}
					inner = f.rest
					if n, _, ok := isContext(f.tag); ok && n == 0 {
						name, err := DecodeObjectName(f.data)
						if err != nil {
							return 0, WriteRequest{}, err
						}
						req.Names = append(req.Names, name)
					}
				}
			}
			sawSpec = true
			continue
		}
		// listOfData：一连串 Data，逐个 DecodeData。
		data := e.data
		for len(data) > 0 {
			d, n, err := DecodeData(data)
			if err != nil {
				return 0, WriteRequest{}, err
			}
			req.Values = append(req.Values, d)
			data = data[n:]
		}
		sawData = true
	}
	_ = sawData
	return invoke, req, nil
}

// WriteResult 是一项写的结果。
type WriteResult struct {
	// OK 表示写成功。
	OK bool
	// Error 是失败时的数据访问错误码。
	Error uint8
	// Item 是出错的对象名。
	Item string
}

// Err 把失败的写结果转成 DataError。
func (r WriteResult) Err() error {
	if r.OK {
		return nil
	}
	return &DataError{Code: r.Error, Item: r.Item}
}

// WriteResponse 是解析出来的写响应。
type WriteResponse struct {
	Results []WriteResult
}

// ParseWriteResponse 解析写响应。
//
// 成功的编码是 `81 00`（CONTEXT [1] 长度 0），失败是 `80 01 <code>`。
// 两者都是单字节标签 + 单字节长度，长度 0 的那个不能因为「内容为空」
// 被当成解析失败——那恰好是成功。
func ParseWriteResponse(b []byte) (WriteResponse, error) {
	_, service, err := splitConfirmed(b, tagConfirmedResponse)
	if err != nil {
		return WriteResponse{}, err
	}
	if service.tag != ServiceTagWrite {
		return WriteResponse{}, fmt.Errorf("iec61850: response service 0x%02X, want write (0x%02X)", service.tag, ServiceTagWrite)
	}
	var out WriteResponse
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return WriteResponse{}, err
		}
		rest = e.rest
		num, _, ok := isContext(e.tag)
		if !ok {
			continue
		}
		switch num {
		case 1: // success
			out.Results = append(out.Results, WriteResult{OK: true})
		case 0: // failure
			code := uint8(0)
			if len(e.data) > 0 {
				code = e.data[0]
			}
			out.Results = append(out.Results, WriteResult{OK: false, Error: code})
		}
	}
	return out, nil
}

// BuildWriteResponse 生成一个写响应（内置从站用）。
//
// 结构：confirmed-response { invokeID, write [A5] }，里面按请求顺序平铺一项一项
// 的结果——成功是 `[1]`（`81 00`），失败是 `[0] <code>`（`80 01 <code>`）。
//
// 与请求侧 ParseWriteResponse 读到的扁平结构严格对称：本平台的编解码自成一系，
// 从站产出的字节必须能被自家请求侧原样读回，否则驱动拿不到自己刚写的结果。
func BuildWriteResponse(invokeID uint32, results []WriteResult) []byte {
	var body []byte
	for _, r := range results {
		if r.OK {
			body = appendTLV(body, contextTag(1, false), nil) // 81 00
		} else {
			body = appendTLV(body, contextTag(0, false), []byte{r.Error}) // 80 01 <code>
		}
	}
	return ConfirmedResponse(invokeID, appendTLV(nil, ServiceTagWrite, body))
}

// ---------------------------------------------------------------------------
// GetNameList
// ---------------------------------------------------------------------------

// 对象类（objectClass）。
const (
	ObjectClassNamedVariable     = 0
	ObjectClassNamedVariableList = 2
	ObjectClassJournal           = 8
	ObjectClassDomain            = 9
)

// GetNameListRequest 是模型浏览请求。
type GetNameListRequest struct {
	Class int
	// Domain 为空表示整个 VMD。
	Domain string
	// ContinueAfter 续读游标。
	ContinueAfter string
}

// BuildGetNameListRequest 生成一个完整的模型浏览请求。
func BuildGetNameListRequest(invokeID uint32, req GetNameListRequest) []byte {
	var body []byte
	body = appendTLV(body, contextTag(0, true), appendTLV(nil, contextTag(0, false), []byte{byte(req.Class)}))

	var scope []byte
	if req.Domain == "" {
		scope = appendTLV(scope, contextTag(0, false), nil)
	} else {
		scope = appendTLV(scope, contextTag(1, false), []byte(req.Domain))
	}
	body = appendTLV(body, contextTag(1, true), scope)

	if req.ContinueAfter != "" {
		body = appendTLV(body, contextTag(2, false), []byte(req.ContinueAfter))
	}
	return ConfirmedRequest(invokeID, appendTLV(nil, ServiceTagGetNameList, body))
}

// ParseGetNameListRequest 解析模型浏览请求（内置从站用）。
//
// objectClass [0] { [0] INTEGER }、objectScope [1] { [0] | [1] VisibleString }、
// continueAfter [2] VisibleString（可选）。域为空表示整个 VMD。
func ParseGetNameListRequest(b []byte) (uint32, GetNameListRequest, error) {
	invoke, service, err := splitConfirmed(b, tagConfirmedRequest)
	if err != nil {
		return 0, GetNameListRequest{}, err
	}
	if service.tag != ServiceTagGetNameList {
		return 0, GetNameListRequest{}, fmt.Errorf("iec61850: mms service 0x%02X, want getNameList (0x%02X)", service.tag, ServiceTagGetNameList)
	}
	var req GetNameListRequest
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return 0, GetNameListRequest{}, err
		}
		rest = e.rest
		num, constructed, ok := isContext(e.tag)
		if !ok {
			continue
		}
		switch {
		case num == 0 && constructed:
			inner := e.data
			for len(inner) > 0 {
				f, err := parseElement(inner)
				if err != nil {
					return 0, GetNameListRequest{}, err
				}
				inner = f.rest
				if n, _, ok := isContext(f.tag); ok && n == 0 && len(f.data) > 0 {
					req.Class = int(f.data[0])
				}
			}
		case num == 1 && constructed:
			inner := e.data
			for len(inner) > 0 {
				f, err := parseElement(inner)
				if err != nil {
					return 0, GetNameListRequest{}, err
				}
				inner = f.rest
				if n, _, ok := isContext(f.tag); ok && n == 1 {
					req.Domain = string(f.data)
				}
			}
		case num == 2 && !constructed:
			req.ContinueAfter = string(e.data)
		}
	}
	return invoke, req, nil
}

// GetNameListResponse 是解析出来的模型浏览响应。
type GetNameListResponse struct {
	// Identifiers 是本页的名字列表。
	Identifiers []string
	// MoreFollows 表示还有下一页（用最后一项做 ContinueAfter 续读）。
	MoreFollows bool
}

// ParseGetNameListResponse 解析模型浏览响应。
func ParseGetNameListResponse(b []byte) (GetNameListResponse, error) {
	_, service, err := splitConfirmed(b, tagConfirmedResponse)
	if err != nil {
		return GetNameListResponse{}, err
	}
	if service.tag != ServiceTagGetNameList {
		return GetNameListResponse{}, fmt.Errorf("iec61850: response service 0x%02X, want getNameList (0x%02X)",
			service.tag, ServiceTagGetNameList)
	}
	out := GetNameListResponse{MoreFollows: true}
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return GetNameListResponse{}, err
		}
		rest = e.rest
		num, constructed, ok := isContext(e.tag)
		if !ok {
			continue
		}
		switch {
		case num == 0 && constructed: // listOfIdentifier
			items := e.data
			for len(items) > 0 {
				f, err := parseElement(items)
				if err != nil {
					return GetNameListResponse{}, err
				}
				items = f.rest
				if f.tag == tagVisibleChars {
					out.Identifiers = append(out.Identifiers, string(f.data))
				}
			}
		case num == 1 && !constructed: // moreFollows，省略时为 TRUE
			out.MoreFollows = len(e.data) > 0 && e.data[0] != 0
		}
	}
	return out, nil
}

// BuildGetNameListResponse 生成模型浏览响应（内置从站用）。
func BuildGetNameListResponse(ids []string, moreFollows bool) []byte {
	var list []byte
	for _, id := range ids {
		list = appendTLV(list, tagVisibleChars, []byte(id))
	}
	var body []byte
	body = appendTLV(body, contextTag(0, true), list)
	if !moreFollows {
		body = appendTLV(body, contextTag(1, false), []byte{0x00})
	}
	return appendTLV(nil, ServiceTagGetNameList, body)
}

// ---------------------------------------------------------------------------
// GetVariableAccessAttributes
// ---------------------------------------------------------------------------

// VarAccessAttrsResponse 是解析出来的变量属性。
type VarAccessAttrsResponse struct {
	// Deletable 表示该变量可以由客户端删除。
	Deletable bool
	// Type 是类型声明。
	Type TypeSpec
	// Address 是厂商私有地址（一般不关心）。
	Address []byte
}

// ParseVarAccessAttrsResponse 解析变量属性响应。
func ParseVarAccessAttrsResponse(b []byte) (VarAccessAttrsResponse, error) {
	_, service, err := splitConfirmed(b, tagConfirmedResponse)
	if err != nil {
		return VarAccessAttrsResponse{}, err
	}
	if service.tag != ServiceTagVarAccessAttrs {
		return VarAccessAttrsResponse{}, fmt.Errorf("iec61850: response service 0x%02X, want getVariableAccessAttributes (0x%02X)",
			service.tag, ServiceTagVarAccessAttrs)
	}
	var out VarAccessAttrsResponse
	rest := service.data
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return VarAccessAttrsResponse{}, err
		}
		rest = e.rest
		num, constructed, ok := isContext(e.tag)
		if !ok {
			continue
		}
		switch {
		case num == 0 && !constructed:
			out.Deletable = len(e.data) > 0 && e.data[0] != 0
		case num == 2 && constructed:
			ts, _, err := DecodeTypeSpec(e.data)
			if err != nil {
				return VarAccessAttrsResponse{}, err
			}
			out.Type = ts
		case num == 4 && constructed:
			out.Address = append([]byte(nil), e.data...)
		}
	}
	return out, nil
}

// BuildVarAccessAttrsResponse 生成变量属性响应（内置从站用）。
func BuildVarAccessAttrsResponse(deletable bool, ts TypeSpec) ([]byte, error) {
	enc, err := EncodeTypeSpec(ts)
	if err != nil {
		return nil, err
	}
	var body []byte
	b := byte(0)
	if deletable {
		b = 0xFF
	}
	body = appendTLV(body, contextTag(0, false), []byte{b})
	body = appendTLV(body, contextTag(2, true), enc)
	return appendTLV(nil, ServiceTagVarAccessAttrs, body), nil
}

// EncodeTypeSpec 把 TypeSpec 编成线格式。
func EncodeTypeSpec(t TypeSpec) ([]byte, error) {
	switch t.Kind {
	case "boolean":
		return appendTLV(nil, tagDataBoolean, nil), nil
	case "utc-time":
		return appendTLV(nil, tagDataUTCTime, nil), nil
	case "bit-string":
		return appendTLV(nil, tagDataBitString, encodeInt(int64(t.Bits))), nil
	case "integer":
		return appendTLV(nil, tagDataInteger, encodeInt(int64(t.Bits))), nil
	case "unsigned":
		return appendTLV(nil, tagDataUnsigned, encodeInt(int64(t.Bits))), nil
	case "float":
		fw, ew := t.FloatWidth, t.ExponentWidth
		if fw == 0 {
			fw = 32
		}
		if ew == 0 {
			if fw == 64 {
				ew = 11
			} else {
				ew = 8
			}
		}
		var inner []byte
		inner = appendTLV(inner, tagInt, encodeInt(int64(fw)))
		inner = appendTLV(inner, tagInt, encodeInt(int64(ew)))
		return appendTLV(nil, 0xA7, inner), nil
	case "octet-string":
		return appendTLV(nil, tagDataOctetString, encodeInt(int64(t.Bits))), nil
	case "visible-string":
		return appendTLV(nil, tagDataVisibleString, encodeInt(int64(t.Bits))), nil
	case "mms-string":
		return appendTLV(nil, tagDataMMSString, encodeInt(int64(t.Bits))), nil
	case "binary-time":
		return appendTLV(nil, tagDataBinaryTime, []byte{0x00}), nil
	case "array":
		var inner []byte
		count := t.Count
		if count < 0 {
			count = 0
		}
		inner = appendTLV(inner, contextTag(1, false), encodeUnsigned(uint64(count)))
		elem := t.Elem
		if elem == nil {
			elem = &TypeSpec{Kind: "boolean"}
		}
		enc, err := EncodeTypeSpec(*elem)
		if err != nil {
			return nil, err
		}
		inner = appendTLV(inner, contextTag(2, true), enc)
		return appendTLV(nil, tagDataArray, inner), nil
	case "structure":
		var comps []byte
		for _, c := range t.Components {
			enc, err := EncodeTypeSpec(c)
			if err != nil {
				return nil, err
			}
			// StructComponent 的 type 字段是 [1] EXPLICIT。
			comps = append(comps, appendTLV(nil, tagSequence, appendTLV(nil, contextTag(1, true), enc))...)
		}
		return appendTLV(nil, tagDataStructure, appendTLV(nil, contextTag(1, true), comps)), nil
	case "named":
		// 内置从站不用抽象类型名；写空对象名会得到一个无意义的引用，
		// 宁可直接拒绝。
		return nil, errors.New("iec61850: cannot encode a named abstract type")
	}
	return nil, fmt.Errorf("iec61850: unsupported type specification %q", t.Kind)
}

// ---------------------------------------------------------------------------
// 错误与拒绝
// ---------------------------------------------------------------------------

// ConfirmedError 是 MMS 的 confirmed-ErrorPDU。
type ConfirmedError struct {
	InvokeID uint32
	// Class 是错误类别（如 access、definition）。
	Class string
	// Code 是类别内的错误码。
	Code int
}

func (e *ConfirmedError) Error() string {
	return fmt.Sprintf("mms confirmed error (invokeID=%d): class %s, code %d", e.InvokeID, e.Class, e.Code)
}

// ParseConfirmedError 解析 confirmed-ErrorPDU。
func ParseConfirmedError(b []byte) (ConfirmedError, error) {
	e, err := parseElement(b)
	if err != nil {
		return ConfirmedError{}, err
	}
	if e.tag != ServiceTagConfirmedError {
		return ConfirmedError{}, fmt.Errorf("iec61850: mms tag 0x%02X, want confirmed-error (0x%02X)",
			e.tag, ServiceTagConfirmedError)
	}
	out := ConfirmedError{}
	rest := e.data
	for len(rest) > 0 {
		f, err := parseElement(rest)
		if err != nil {
			return out, err
		}
		rest = f.rest
		if f.tag == tagInt {
			v, err := decodeUnsigned(f.data)
			if err != nil {
				return out, err
			}
			out.InvokeID = uint32(v)
			continue
		}
		if num, constructed, ok := isContext(f.tag); ok && num == 2 && constructed {
			out.Class, out.Code = parseServiceError(f.data)
		}
	}
	return out, nil
}

// parseServiceError 解析 serviceError 的类别与码值。
func parseServiceError(b []byte) (class string, code int) {
	rest := b
	for len(rest) > 0 {
		e, err := parseElement(rest)
		if err != nil {
			return "", 0
		}
		rest = e.rest
		switch e.tag {
		case tagSequence:
			class, code = parseServiceError(e.data)
		default:
			num, _, ok := isContext(e.tag)
			if !ok {
				continue
			}
			switch num {
			case 0:
				class = "vmd-state"
			case 1:
				class = "application-reference"
			case 2:
				class = "definition"
			case 3:
				class = "resource"
			case 4:
				class = "service"
			case 5:
				class = "service-preempt"
			case 6:
				class = "time-resolution"
			case 7:
				class = "access"
			case 8:
				class = "initiate"
			case 9:
				class = "conclude"
			case 10:
				class = "cancel"
			case 11:
				class = "file"
			case 12:
				class = "others"
			}
			if v, err := decodeUnsigned(e.data); err == nil {
				code = int(v)
			}
		}
	}
	return class, code
}

// Reject 是 MMS 的 RejectPDU。
type Reject struct {
	Reason int
	// Type 是被拒 PDU 的外层标签，便于定位是哪一类报文出错。
	Type byte
}

func (r *Reject) Error() string {
	return fmt.Sprintf("mms reject: %s (reason %d, pdu tag 0x%02X)", rejectReasonName(r.Reason), r.Reason, r.Type)
}

// rejectReasonName 返回拒绝原因的可读名。
func rejectReasonName(v int) string {
	switch v {
	case 0:
		return "unrecognized-service"
	case 1:
		return "unrecognized-type"
	case 2:
		return "unrecognized-structure"
	case 3:
		return "unrecognized-data"
	case 4:
		return "unrecognized-invoke-id"
	case 5:
		return "unrecognized-argument"
	case 6:
		return "unrecognized-modifier"
	case 7:
		return "invalid-request"
	case 8:
		return "invalid-result"
	case 9:
		return "invalid-parameter"
	case 10:
		return "other"
	}
	return "unknown"
}

// ParseReject 解析 RejectPDU。
func ParseReject(b []byte) (*Reject, error) {
	e, err := parseElement(b)
	if err != nil {
		return nil, err
	}
	if e.tag != ServiceTagReject {
		return nil, fmt.Errorf("iec61850: mms tag 0x%02X, want reject (0x%02X)", e.tag, ServiceTagReject)
	}
	out := &Reject{}
	rest := e.data
	for len(rest) > 0 {
		f, err := parseElement(rest)
		if err != nil {
			return out, err
		}
		rest = f.rest
		n, _, ok := isContext(f.tag)
		if !ok {
			continue
		}
		switch n {
		case 0: // rejectReason
			if len(f.data) > 0 {
				if v, err := decodeUnsigned(f.data); err == nil {
					out.Reason = int(v)
				}
			}
		case 1: // rejectedPDU [1] IMPLICIT RejectedPDU
			// RejectedPDU 是 CHOICE：pdu-error [0] ConfirmationFailedPDU，其中
			// ConfirmationFailedPDU 又含被拒报文的外层类型。编码是
			// [1]( [0]( <type> ) )——type 埋了两层。逐层剥到那个裸标签字节。
			inner := f.data
			for len(inner) > 0 {
				g, gerr := parseElement(inner)
				if gerr != nil {
					break
				}
				inner = g.rest
				// context 标签的内容里若只有一个字节，那就是要的类型。
				if len(g.data) == 1 {
					out.Type = g.data[0]
					break
				}
				inner = g.data
			}
			if out.Type == 0 && len(f.data) > 0 {
				out.Type = f.data[0]
			}
		}
	}
	return out, nil
}

// BuildReject 生成 RejectPDU（内置从站用）。
func BuildReject(reason int, pduType byte) []byte {
	var body []byte
	body = appendTLV(body, contextTag(0, false), encodeInt(int64(reason)))
	body = appendTLV(body, contextTag(1, true), appendTLV(nil, contextTag(0, false), []byte{pduType}))
	return appendTLV(nil, ServiceTagReject, body)
}

// BuildConfirmedError 生成 confirmed-ErrorPDU（内置从站用）。
//
// classTag 是 serviceError 里的类别标签（7 = access）。
func BuildConfirmedError(invokeID uint32, classTag byte, code int) []byte {
	var body []byte
	body = appendTLV(body, tagInt, encodeUnsigned(uint64(invokeID)))
	svcErr := appendTLV(nil, contextTag(classTag, false), encodeInt(int64(code)))
	body = appendTLV(body, contextTag(2, true), appendTLV(nil, tagSequence, svcErr))
	return appendTLV(nil, ServiceTagConfirmedError, body)
}

// ---------------------------------------------------------------------------
// 外层拆解
// ---------------------------------------------------------------------------

// splitConfirmed 拆开 confirmed-request/response，返回 invokeID 与服务体。
func splitConfirmed(b []byte, want byte) (uint32, element, error) {
	e, err := parseElement(b)
	if err != nil {
		return 0, element{}, err
	}
	if e.tag != want {
		return 0, element{}, fmt.Errorf("iec61850: mms pdu tag 0x%02X, want 0x%02X", e.tag, want)
	}
	rest := e.data
	first, err := parseElement(rest)
	if err != nil {
		return 0, element{}, err
	}
	if first.tag != tagInt {
		return 0, element{}, errors.New("iec61850: mms pdu does not start with an invokeID")
	}
	id, err := decodeUnsigned(first.data)
	if err != nil {
		return 0, element{}, err
	}
	svc, err := parseElement(first.rest)
	if err != nil {
		return 0, element{}, err
	}
	return uint32(id), svc, nil
}

// MMSServiceTag 取一个 MMS confirmed PDU 的服务标签（read/write/getNameList/...）。
//
// 内置从站用它分流：自己剥 confirmed-request 外层（invokeID 之后那一段就是服务），
// 不必在 sim 里重抄一遍 ASN.1 结构。
func MMSServiceTag(mms []byte) (byte, error) {
	e, err := parseElement(mms)
	if err != nil {
		return 0, err
	}
	if e.tag != tagConfirmedRequest && e.tag != tagConfirmedResponse {
		return 0, fmt.Errorf("iec61850: mms pdu tag 0x%02X, want confirmed (0x%02X/0x%02X)", e.tag, tagConfirmedRequest, tagConfirmedResponse)
	}
	id, err := parseElement(e.data)
	if err != nil {
		return 0, err
	}
	svc, err := parseElement(id.rest)
	if err != nil {
		return 0, err
	}
	return svc.tag, nil
}

// ConfirmedInvokeID 取出一个 confirmed PDU 的 invokeID 与它的外层标签。
//
// 用于「先看这条报文是不是我在等的那个 invokeID」——不等的话应当丢弃并继续
// 等，而不是把它当成当前请求的响应去解析。
func ConfirmedInvokeID(b []byte) (invokeID uint32, tag byte, err error) {
	e, err := parseElement(b)
	if err != nil {
		return 0, 0, err
	}
	switch e.tag {
	case tagConfirmedRequest, tagConfirmedResponse:
		first, err := parseElement(e.data)
		if err != nil {
			return 0, e.tag, err
		}
		if first.tag != tagInt {
			return 0, e.tag, errors.New("iec61850: confirmed pdu does not start with an invokeID")
		}
		v, err := decodeUnsigned(first.data)
		if err != nil {
			return 0, e.tag, err
		}
		return uint32(v), e.tag, nil
	case ServiceTagConfirmedError:
		first, err := parseElement(e.data)
		if err != nil {
			return 0, e.tag, err
		}
		if first.tag == tagInt {
			if v, err := decodeUnsigned(first.data); err == nil {
				return uint32(v), e.tag, nil
			}
		}
		return 0, e.tag, nil
	}
	return 0, e.tag, nil
}

// ServiceName 返回 MMS 服务标签的可读名。
func ServiceName(tag byte) string {
	switch tag {
	case ServiceTagGetNameList:
		return "getNameList"
	case ServiceTagRead:
		return "read"
	case ServiceTagWrite:
		return "write"
	case ServiceTagVarAccessAttrs:
		return "getVariableAccessAttributes"
	case ServiceTagInitiate:
		return "initiate"
	case ServiceTagInitiateResponse:
		return "initiate-response"
	case ServiceTagConclude:
		return "conclude"
	case ServiceTagConcludeResponse:
		return "conclude-response"
	case ServiceTagConfirmedError:
		return "confirmed-error"
	case ServiceTagReject:
		return "reject"
	}
	return fmt.Sprintf("service(0x%02X)", tag)
}

// BuildConclude 生成 Conclude-RequestPDU。
func BuildConclude() []byte { return []byte{ServiceTagConclude, 0x00} }

// BuildConcludeResponse 生成 Conclude-ResponsePDU。
func BuildConcludeResponse() []byte { return []byte{ServiceTagConcludeResponse, 0x00} }
