package snmp

import (
	snmpcodec "uic/internal/codec/snmp"
)

// 本文件是内置代理的 USM（v3）侧。
//
// 它必须**真的**做认证与解密，而不是「看一眼用户名就放行」：
//
//   - 认证校验用本地化密钥重算 HMAC，与主站侧完全对称。只有在两边都
//     真正做了派生，「口令/算法配错」才会在仿真里表现为认证失败，
//     而不是在真机上才暴露。
//   - 解密失败与摘要失败分开报（usmStatsDecryptionErrors 与
//     usmStatsWrongDigests）：这两者的现场原因完全不同，合成一个
//     错误码会让排障失去唯一的线索。
//
// 与真实代理刻意保留的差异：真实代理对摘要错误通常**静默丢弃**（避免
// 成为口令预言机），这里默认回 Report，但可以通过 Faults.DropPercent
// 模拟静默丢包。

// handleV3 处理一条 v3 报文；返回 nil 表示静默丢弃。
func (s *Server) handleV3(datagram []byte) []byte {
	if s.usm == nil {
		// 未启用 USM：与 community 不匹配同样静默丢弃。
		return nil
	}
	msg, err := snmpcodec.ParseV3(datagram)
	if err != nil {
		s.countDrop()
		return nil
	}
	if msg.Header.SecModel != snmpcodec.SecurityModelUSM {
		s.countDrop()
		return nil
	}

	s.mu.Lock()
	faults := s.usm.faults
	s.mu.Unlock()
	if faults.DropPercent > 0 && int(hashOf(datagram)%100) < faults.DropPercent {
		s.countDrop()
		return nil
	}

	// 1. 引擎发现：空 engineID、无认证。代理回 Report 告知自己的身份。
	if len(msg.Security.EngineID) == 0 {
		s.mu.Lock()
		engine := append([]byte(nil), s.usm.engine...)
		boots, etime := s.usm.boots, s.usm.etime
		s.mu.Unlock()
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsUnknownEngineIDs, 1, snmpcodec.AuthNone, nil)
	}
	// engineID 必须与我们的一致；不一致视为「客户端在用旧的引擎身份」。
	s.mu.Lock()
	engine := append([]byte(nil), s.usm.engine...)
	boots, etime := s.usm.boots, s.usm.etime
	s.mu.Unlock()
	if string(msg.Security.EngineID) != string(engine) {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsUnknownEngineIDs, 1, snmpcodec.AuthNone, nil)
	}
	if faults.UnsupportedSecLevel {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsUnsupportedSecLevels, 1, snmpcodec.AuthNone, nil)
	}
	if faults.NotInTimeWindow {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsNotInTimeWindows, 1, snmpcodec.AuthNone, nil)
	}
	// 2. 时间窗口：boots 必须相等，engineTime 差不超过窗口。
	// 内置代理的 engineTime 是固定值（不随墙上时钟走），与 sysUpTime
	// 的取舍相同：测试需要稳定的断言，而不是 sleep 赌运气。
	if msg.Security.EngineBoots != boots {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsNotInTimeWindows, 1, snmpcodec.AuthNone, nil)
	}
	if d := msg.Security.EngineTime - etime; d > snmpcodec.TimeWindow || d < -snmpcodec.TimeWindow {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsNotInTimeWindows, 1, snmpcodec.AuthNone, nil)
	}

	// 3. 用户与安全级别。
	s.mu.Lock()
	user, ok := s.usm.cfg.Users[msg.Security.UserName]
	s.mu.Unlock()
	if !ok {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsUnknownUserNames, 1, snmpcodec.AuthNone, nil)
	}
	wantAuth := msg.Header.Flags.HasAuth()
	wantPriv := msg.Header.Flags.HasPriv()
	if wantAuth != (user.Auth != snmpcodec.AuthNone) || wantPriv != (user.Priv != snmpcodec.PrivNone) {
		return s.buildReport(msg, engine, boots, etime,
			snmpcodec.OIDUsmStatsUnsupportedSecLevels, 1, snmpcodec.AuthNone, nil)
	}

	// 4. 认证校验：用与主站相同的派生流程算出本地化密钥。
	var authKey, privKey []byte
	if wantAuth {
		ku, err := snmpcodec.PasswordToKey(user.Auth, user.AuthPassword)
		if err != nil {
			s.countDrop()
			return nil
		}
		authKey, err = snmpcodec.LocalizeKey(user.Auth, ku, engine)
		if err != nil {
			s.countDrop()
			return nil
		}
		if faults.WrongDigest {
			return s.buildReport(msg, engine, boots, etime,
				snmpcodec.OIDUsmStatsWrongDigests, 1, snmpcodec.AuthNone, nil)
		}
		if err := snmpcodec.VerifyV3(datagram, msg.Security, user.Auth, authKey); err != nil {
			// 摘要错：真代理多静默丢弃；这里回 Report，让测试不必等超时。
			// 这个 Report 不签名——摘要都不对，说明我们与对方的密钥
			// 不一致，我们签的东西对方同样验不过。
			return s.buildReport(msg, engine, boots, etime,
				snmpcodec.OIDUsmStatsWrongDigests, 1, snmpcodec.AuthNone, nil)
		}
	}
	if wantPriv {
		ku, err := snmpcodec.PasswordToKey(user.Auth, user.PrivPassword)
		if err != nil {
			s.countDrop()
			return nil
		}
		privKey, err = snmpcodec.LocalizeKey(user.Auth, ku, engine)
		if err != nil {
			s.countDrop()
			return nil
		}
	}

	// 5. 解密。
	if wantPriv {
		plain, err := snmpcodec.DecryptScopedBytes(user.Priv, privKey,
			msg.Security.EngineBoots, msg.Security.EngineTime,
			msg.Security.PrivParameters, msg.RawScoped)
		if err != nil {
			s.countDecryptError()
			// 走到这里说明摘要已经验过（密钥是对的），只有加密口令/
			// 算法不对。这个 Report 必须签名：否则任何人都能伪造一条
			// 「解密失败」让主站误判。
			return s.buildReport(msg, engine, boots, etime,
				snmpcodec.OIDUsmStatsDecryptionErrors, 1, user.Auth, authKey)
		}
		msg.ScopedPDU = plain
	}
	sc, err := snmpcodec.ParseScopedPDU(msg.ScopedPDU)
	if err != nil {
		s.countDrop()
		return nil
	}
	pdu := sc.PDU
	if !pdu.Type.IsRequest() {
		s.countDrop()
		return nil
	}

	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	vbs, status, index := s.dispatch(snmpcodec.V3, pdu)

	respPDU := snmpcodec.PDU{
		Type: snmpcodec.PDUResponse, RequestID: pdu.RequestID,
		ErrorStatus: status, ErrorIndex: index, Varbinds: vbs,
	}
	return s.buildV3Response(msg, respPDU, user, engine, boots, etime, authKey, privKey)
}

// buildV3Response 组一条加密/签名的应答。
//
// 应答的安全级别**跟随请求**：请求是 authPriv，应答也必须 authPriv。
// 有些代理允许降级（请求 authPriv、应答 authNoPriv），但主站侧会因此
// 把明文 PDU 当密文解。这里不引入这种「合法但罕见」的行为。
func (s *Server) buildV3Response(req snmpcodec.V3Message, respPDU snmpcodec.PDU,
	user USMUser, engine []byte, boots, etime int, authKey, privKey []byte) []byte {

	scoped, err := snmpcodec.BuildScopedPDU(engine, "", respPDU)
	if err != nil {
		return nil
	}
	flags := snmpcodec.MsgFlags(0)
	if user.Auth != snmpcodec.AuthNone {
		flags |= snmpcodec.FlagAuth
	}
	if user.Priv != snmpcodec.PrivNone {
		flags |= snmpcodec.FlagPriv
	}
	hdr := snmpcodec.HeaderData{
		MsgID:    req.Header.MsgID,
		MaxSize:  req.Header.MaxSize,
		Flags:    flags,
		SecModel: snmpcodec.SecurityModelUSM,
	}
	sec := snmpcodec.USMSecurityParameters{
		EngineID:    engine,
		EngineBoots: boots,
		EngineTime:  etime,
		UserName:    user.Name,
	}
	keys := &snmpcodec.V3Keys{Auth: user.Auth, AuthKey: authKey, Priv: user.Priv, PrivKey: privKey}
	raw, err := snmpcodec.BuildV3(hdr, sec, scoped, keys)
	if err != nil {
		return nil
	}
	return raw
}

// buildReport 组一条 Report PDU。
//
// Report 的身份是「安全模型自己的诊断通道」：它的 varbind 是一个
// usmStats* 计数器，不是业务数据。引擎发现的 Report 必须带 engineID/
// boots/time（否则客户端无从继续）；其它 Report 也带，便于一次修正。
//
// authKey 非空时给 Report 加认证。为什么不是全都加：认证前的 Report
// （未知 engineID、时间窗口、未知用户）发生时我们**还没有**确认对方的
// 密钥，签出来的签名对方也无从验证；而验过摘要之后的 Report（解密失败）
// 若不签名，攻击者就能伪造它让主站误判口令错误。两种情况在实现上必须
// 分开，不能图省事统一成一种。
func (s *Server) buildReport(req snmpcodec.V3Message, engine []byte, boots, etime int,
	counter snmpcodec.OID, value uint64, authProto snmpcodec.AuthProtocol, authKey []byte) []byte {

	s.mu.Lock()
	s.usm.reports++
	s.mu.Unlock()

	pdu := snmpcodec.PDU{
		Type:      snmpcodec.PDUReport,
		RequestID: req.PDU.RequestID,
		Varbinds: []snmpcodec.Varbind{{
			OID:   counter,
			Value: snmpcodec.Unsigned(snmpcodec.TypeCounter32, value),
		}},
	}
	scoped, err := snmpcodec.BuildScopedPDU(engine, "", pdu)
	if err != nil {
		return nil
	}
	hdr := snmpcodec.HeaderData{
		MsgID:    req.Header.MsgID,
		MaxSize:  req.Header.MaxSize,
		Flags:    0,
		SecModel: snmpcodec.SecurityModelUSM,
	}
	sec := snmpcodec.USMSecurityParameters{
		EngineID:    engine,
		EngineBoots: boots,
		EngineTime:  etime,
		UserName:    req.Security.UserName,
	}
	var keys *snmpcodec.V3Keys
	if authProto != snmpcodec.AuthNone && len(authKey) > 0 {
		hdr.Flags = snmpcodec.FlagAuth
		keys = &snmpcodec.V3Keys{Auth: authProto, AuthKey: authKey}
	}
	raw, err := snmpcodec.BuildV3(hdr, sec, scoped, keys)
	if err != nil {
		return nil
	}
	return raw
}

func (s *Server) countDrop() {
	s.mu.Lock()
	s.usm.dropped++
	s.mu.Unlock()
}

func (s *Server) countDecryptError() {
	s.mu.Lock()
	s.usm.decrypted++
	s.mu.Unlock()
}

// hashOf 是故障注入用的稳定哈希（FNV-1a）：同一报文每次得到同一结果，
// 测试不会因为随机数而抖动。
func hashOf(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}
