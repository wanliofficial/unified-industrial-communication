package iec61850

import (
	"fmt"
)

// 本文件把前面几层拼成一次关联：TPKT → COTP → 会话 → 表示 → ACSE → MMS。
//
// 之所以把「拼装」放在 codec 里而不是驱动里：这条封套链的字节序、长度与
// 上下文编号是协议事实，与「用哪个 socket 发」无关。放在 codec 里它就能被
// 单元测试逐字节验证——而这正是最容易出错的一段，因为它没有语义可供自检，
// 只有一个字节一个字节地对。

// BuildConnectPDU 生成建立关联的完整 TPKT 载荷（COTP CR 之后的那一段）。
//
// 一次调用产出四层嵌套：会话 CN { 表示 CP { ACSE AARQ { MMS Initiate } } }。
func BuildConnectPDU(req InitiateRequest, auth *ACSEAuth) ([]byte, error) {
	mms := BuildInitiate(req)
	acse, err := BuildAARQ(mms, auth)
	if err != nil {
		return nil, err
	}
	pres, err := BuildPresentationConnect(DefaultPresentationParams(), acse)
	if err != nil {
		return nil, err
	}
	sess, err := BuildSessionConnect(DefaultSessionParams(), pres)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// ParseConnectResponse 拆开对端的 AC（会话接受）→ CPA → AARE → Initiate 响应。
//
// 返回值里同时带上「对端确认了什么」（协商后的 PDU 大小与嵌套层数）与
// 「关联是否被接受」。三步里任何一步失败都要把层级写进错误：一条
// 「association rejected」而不说是会话层还是 ACSE 拒绝的消息，在现场等于
// 没有信息。
func ParseConnectResponse(sessionPayload []byte) (AARE, InitiateResponse, error) {
	spdu, err := ParseSPDU(sessionPayload)
	if err != nil {
		return AARE{}, InitiateResponse{}, fmt.Errorf("session layer: %w", err)
	}
	if spdu.ID == spduAbort {
		return AARE{}, InitiateResponse{}, fmt.Errorf("session layer: peer aborted the association (reason 0x%02X)", spdu.Reason)
	}
	if spdu.ID == spduRefuse {
		return AARE{}, InitiateResponse{}, fmt.Errorf("session layer: peer refused the association (reason 0x%02X)", spdu.Reason)
	}
	if spdu.ID != spduAccept {
		return AARE{}, InitiateResponse{}, fmt.Errorf("session layer: got %s, want AC", SessionName(spdu.ID))
	}

	pres, err := ParsePresentation(spdu.Payload)
	if err != nil {
		return AARE{}, InitiateResponse{}, fmt.Errorf("presentation layer: %w", err)
	}
	switch pres.Kind {
	case "ab":
		return AARE{}, InitiateResponse{}, fmt.Errorf("presentation layer: peer aborted the association (reason 0x%02X)", pres.Reason)
	case "arp":
		return AARE{}, InitiateResponse{}, fmt.Errorf("presentation layer: peer rejected the presentation context (reason %d)", pres.Reason)
	}
	if pres.UserData == nil {
		return AARE{}, InitiateResponse{}, fmt.Errorf("presentation layer: %s carries no user data", pres.Kind)
	}

	kind, aare, err := ParseACSE(pres.UserData)
	if err != nil {
		return AARE{}, InitiateResponse{}, fmt.Errorf("acse: %w", err)
	}
	if kind == "abrt" {
		return aare, InitiateResponse{}, fmt.Errorf("acse: peer aborted the association")
	}
	if kind != "aare" {
		return AARE{}, InitiateResponse{}, fmt.Errorf("acse: got %s, want AARE", kind)
	}
	if aare.Result != ACSEResultAccepted {
		reason := ACSEReason(aare.Result, aare.Diagnostic)
		if reason == "" {
			reason = "association rejected"
		}
		return aare, InitiateResponse{}, fmt.Errorf("acse: %s", reason)
	}
	if len(aare.UserData) == 0 {
		return aare, InitiateResponse{}, fmt.Errorf("acse: AARE carries no user information (the peer accepted the association but sent no Initiate response)")
	}
	init, err := ParseInitiateResponse(aare.UserData)
	if err != nil {
		return aare, InitiateResponse{}, fmt.Errorf("mms initiate: %w", err)
	}
	return aare, init, nil
}

// WrapData 把一个 MMS PDU 包成可发送的会话 DT 载荷
// （会话 DT { 表示 用户数据 { MMS } }）。
func WrapData(mms []byte) ([]byte, error) {
	pres, err := BuildPresentationUserData(mms)
	if err != nil {
		return nil, err
	}
	return BuildSessionData(pres), nil
}

// UnwrapData 从会话 DT 负载里取出 MMS PDU。
func UnwrapData(sessionPayload []byte) ([]byte, error) {
	spdu, err := ParseSPDU(sessionPayload)
	if err != nil {
		return nil, err
	}
	switch spdu.ID {
	case spduDataTransfer:
	case spduAbort:
		return nil, fmt.Errorf("session layer: peer aborted the association (reason 0x%02X)", spdu.Reason)
	case spduFinish, spduDisconnect:
		return nil, fmt.Errorf("session layer: peer released the association (%s)", SessionName(spdu.ID))
	default:
		return nil, fmt.Errorf("session layer: got %s, want DT", SessionName(spdu.ID))
	}
	pres, err := ParsePresentation(spdu.Payload)
	if err != nil {
		return nil, err
	}
	if pres.Kind == "ab" {
		return nil, fmt.Errorf("presentation layer: peer aborted the association")
	}
	if pres.UserData == nil {
		return nil, fmt.Errorf("presentation layer: %s carries no user data", pres.Kind)
	}
	if pres.ContextID != 0 && pres.ContextID != MMSContextID {
		// 上下文 1 是 ACSE：关联建立之后还收到它，说明对端在做释放或
		// 中止，而不是回答我们的读请求。
		return nil, fmt.Errorf("presentation layer: user data uses context %d, want the MMS context (%d)",
			pres.ContextID, MMSContextID)
	}
	return pres.UserData, nil
}

// BuildReleasePDU 生成有序释放的会话载荷（会话 FN { 表示 ACSE RLRQ }）。
func BuildReleasePDU() ([]byte, error) {
	pres, err := BuildPresentationUserDataACSE(BuildRLRQ())
	if err != nil {
		return nil, err
	}
	return BuildSessionFinish(pres)
}

// BuildAbortPDU 生成异常中止的会话载荷（会话 AB { 表示 ACSE ABRT }）。
func BuildAbortPDU() ([]byte, error) {
	pres, err := BuildPresentationAbort(BuildABRT(false))
	if err != nil {
		return nil, err
	}
	return BuildSessionAbort(pres)
}

// ParseRelease 判断一个会话载荷是否是释放或中止。
//
// 返回 "rlrq"/"rlre"/"abort"/""（不是释放类报文）。
func ParseRelease(sessionPayload []byte) string {
	spdu, err := ParseSPDU(sessionPayload)
	if err != nil {
		return ""
	}
	switch spdu.ID {
	case spduFinish:
		if pres, err := ParsePresentation(spdu.Payload); err == nil {
			if kind, _, err := ParseACSE(pres.UserData); err == nil && kind == "rlrq" {
				return "rlrq"
			}
		}
		return "finish"
	case spduDisconnect:
		return "disconnect"
	case spduAbort:
		return "abort"
	}
	return ""
}

// BuildReleaseResponse 生成对释放请求的响应（会话 FN { 表示 ACSE RLRE }）。
func BuildReleaseResponse() ([]byte, error) {
	pres, err := BuildPresentationUserDataACSE(BuildRLRE())
	if err != nil {
		return nil, err
	}
	return BuildSessionFinish(pres)
}
