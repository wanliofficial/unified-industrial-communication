package snmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	"uic/internal/model"
)

// 本文件是 SNMPv3 的会话层：引擎发现、时间同步、密钥本地化、Report 分类。
//
// 为什么单独成一个文件：v3 的复杂度不在线格式（那是 codec 的事），而在
// 「一条请求要经历哪几步」——它可能与代理往返三四次（发现 → 报告 boots/time
// → 真请求），每一步的失败含义完全不同。把这些步骤摊平在 driver.go 里，会让
// 已经很长的 Read/Write 主流程被安全逻辑打断。
//
// 三步发现的细节：
//
//  1. 发空 engineID 的无认证 GetRequest，代理回 usmStatsUnknownEngineIDs，
//     报文中带 msgAuthoritativeEngineID/Boots/Time——引擎身份一次拿全。
//  2. 有了 engineID 就能派生密钥；用带认证/加密的报文重发。
//  3. 代理若认为时间不在窗口内，回 usmStatsNotInTimeWindows，同样带新的
//     boots/time，刷新缓存后再试一次。

// v3Session 是一次 v3 会话的安全状态。
//
// 缓存 engineBoots/engineTime 而不是每次重新发现：发现要额外两次往返，
// 在轮询周期只有几秒的场景下不可接受。缓存失效的判据是 engineBoots 变化
// （代理重启），由 Report 触发。
type v3Session struct {
	user string
	auth snmpcodec.AuthProtocol
	priv snmpcodec.PrivProtocol

	// 下面两个口令分开存储：USM 允许认证口令与加密口令不同（有些代理要求
	// 这样），用同一份派生会静默连错。只有 privPassword 为空时才回退到
	// authPassword，这个回退是显式的。
	authPassword []byte
	privPassword []byte

	// fixedEngineID 非空表示配置里显式写死了引擎身份。它只用来做一致性
	// 校验，不省掉发现：时间基准只能从 Report 里问出来（见 discover）。
	fixedEngineID []byte

	// msgID 在每条请求上 +1，避免重放。RFC 3412 要求管理器维护它。
	msgID int32

	// engineID/Boots/Time 是发现阶段学到的对端状态。engineID 用原子读写
	// 是因为「检测缓存失效」发生在读路径的锁外。
	engineID    []byte
	engineBoots int
	engineTime  int
	// engineTimeAt 是学 engineTime 时的墙上时钟，用于推算当前值。
	engineTimeAt time.Time

	// keys 是本地化密钥，发现完成后生成一次。口令与 engineID 都不变时
	// 不需要重复派生（SHA-512 的 1 MiB 扩展开销不小）。
	//
	// 密钥是 engineID 的函数：engineID 一变，旧密钥立刻作废。这条不变式
	// 由 setEngine 负责维护（它会把 keysReady 打回未就绪），而不是靠调用
	// 方记得手动清标志——漏清一次的症状是「代理重装后所有请求都认证失败，
	// 重启进程才好」。
	keys *snmpcodec.V3Keys
	// keysReady 表示 keys 已可用。用原子标志而不是判 keys != nil：两者
	// 之间可能因为 engineID 变更而需要重新派生。
	keysReady atomic.Bool

	// mu 保护 keys 的替换（读路径会读它）。会话级的 mu 保护往返时序。
	mu sync.Mutex
}

// newV3Session 从设备参数构造 v3 会话。
//
// 口令校验（≥8 字节、算法组合是否够用）在这里做一次：这些问题在 Open 时报，
// 比在第一轮事务超时后才报要便宜得多。Open 本身不联网，所以发现的时机留给
// Probe。
func newV3Session(dev model.Device, opts Options) (*v3Session, error) {
	user := strings.TrimSpace(opts.Username)
	if user == "" {
		// USM 的用户名允许为空串（代理上的默认用户），但空用户名几乎
		// 总是配置漏了；这里显式拒绝，避免「用户名写错」伪装成「认证失败」。
		return nil, errors.New("v3 requires labels.username (USM security name)")
	}
	s := &v3Session{
		user:          user,
		auth:          opts.AuthProtocol,
		priv:          opts.PrivProtocol,
		authPassword:  []byte(opts.AuthPassword),
		privPassword:  []byte(opts.PrivPassword),
		fixedEngineID: opts.FixedEngineID,
	}
	if s.privPassword == nil || len(s.privPassword) == 0 {
		s.privPassword = s.authPassword
	}
	// authPassword 为空时，v3 允许 noAuthNoPriv：此时不校验口令长度。
	// 这里必须区分「没配认证」与「配了认证但口令太短」——前者是合法的
	// 最弱安全级别，后者是配置错误。
	if s.auth != snmpcodec.AuthNone {
		if err := snmpcodec.ValidateSecret("authPassword", string(s.authPassword)); err != nil {
			return nil, err
		}
	}
	if s.priv != snmpcodec.PrivNone {
		if err := snmpcodec.ValidateSecret("privPassword", string(s.privPassword)); err != nil {
			return nil, err
		}
		// AES-192/256 需要 24/32 字节的本地化密钥，等于认证摘要长度；
		// MD5/SHA-1 只有 16/20 字节，配 AES-256 会「派生成功、加密失败」。
		if need, have := s.priv.KeyLen(), s.auth.KeyLen(); need > have {
			return nil, fmt.Errorf("priv %v needs a %d-byte key but %v localizes to %d bytes; use a longer auth protocol (SHA-256 or better)",
				s.priv, need, s.auth, have)
		}
	}
	if opts.FixedEngineID != nil {
		s.engineID = append([]byte(nil), opts.FixedEngineID...)
	}
	return s, nil
}

// ensureReady 保证发现完成、密钥就绪。返回的错误已分类。
//
// idempotent 由调用方用会话级 mu 保证：同一时刻只有一个 goroutine 在发现，
// 其余在等。发现内部不能再拿会话 mu（会死锁），所以这里只约定「进入时
// 已由调用方持锁」。
func (s *v3Session) ensureReady(ctx context.Context, c *connState) error {
	if s.keysReady.Load() {
		return nil
	}
	if err := s.discover(ctx, c); err != nil {
		return err
	}
	if err := s.deriveKeys(); err != nil {
		return err
	}
	s.keysReady.Store(true)
	return nil
}

// discover 完成引擎发现与时间基准学习，把 engineID/Boots/Time 学全。
//
// 只有一条路径：发一条空 engineID 的无认证探测包，从代理回的 Report 里
// 一次拿全三件事（身份、启动次数、运行秒数）。哪怕配置里已经写了 engineID
// 也照做——因为「身份已知」不等于「时间基准已知」，而时间基准只能问出来。
//
// 为什么不走「用已知 engineID 发一条无认证请求」这条看似更省事的捷径：
// 那条报文带了用户名却没有摘要，代理按该用户的安全策略一看「要求认证却
// 没认证」，回的是 unsupportedSecLevel；而配了 authPriv 的代理对这类报文
// 更常见的处理是直接丢弃。两条路都可能拿不到时间基准，空 engineID 的
// 发现报文却是协议明确要求代理必须回答的。
//
// 代价是多一次往返（发现 → 真请求）。这个代价只在会话建立时付一次，
// 之后 boots/time 都缓存在会话里。
func (s *v3Session) discover(ctx context.Context, c *connState) error {
	return s.discoverEngineID(ctx, c)
}

// resetEngine 丢弃已学的引擎身份，回到「只有配置里固定的那个（可能没有）」。
//
// 什么时候用：代理回了 unknownEngineIDs。这表示它现在报的 engineID 与
// 我们手里这个对不上（常见于代理重装、配置恢复、或我们缓存了一个过期
// 身份）。此时必须回到空 engineID 重做发现，而不是拿旧身份再算一遍密钥
// ——旧身份算出来的密钥只会让代理再回一次同一个 Report。
func (s *v3Session) resetEngine() {
	s.engineID = append([]byte(nil), s.fixedEngineID...)
	s.engineBoots = 0
	s.engineTime = 0
	s.engineTimeAt = time.Time{}
}

// discoverEngineID 发一条空 engineID 的请求，从 Report 里学引擎身份。
//
// 这条探测包必须无认证、无加密（我们还不知道 engineID，无从派生密钥），
// 也没有摘要可填——USM 把这种报文当作「发现请求」处理，代理会回一条
// unknownEngineIDs 的 Report 而不是丢弃。它的唯一目的是拿到 engineID 与
// boots/time。
//
// 用户名照填：空 engineID + 空用户名会被部分代理当成垃圾包静默丢弃，
// 连 Report 都不回，症状是「首包必超时」。填上用户名后代理才知道该用
// 哪份安全策略来生成这条 Report。
func (s *v3Session) discoverEngineID(ctx context.Context, c *connState) error {
	id := c.nextID()
	hdr := snmpcodec.HeaderData{
		MsgID:    s.nextMsgID(),
		MaxSize:  c.maxSize,
		Flags:    snmpcodec.FlagReportable,
		SecModel: snmpcodec.SecurityModelUSM,
	}
	// engineID 为空：这正是「我在做发现」的信号，代理会回 Report 而不是丢弃。
	sec := snmpcodec.USMSecurityParameters{
		UserName: s.user,
	}
	pdu, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, id, []snmpcodec.OID{sysDescrOID})
	if err != nil {
		return driver.E(driver.ErrInternal, "discover", c.cfg.Device.ID, err.Error(), false, err)
	}
	scoped, err := snmpcodec.BuildScopedPDU(nil, "", pdu)
	if err != nil {
		return driver.E(driver.ErrInternal, "discover", c.cfg.Device.ID, err.Error(), false, err)
	}
	raw, err := snmpcodec.BuildV3(hdr, sec, scoped, nil)
	if err != nil {
		return driver.E(driver.ErrInternal, "discover", c.cfg.Device.ID, err.Error(), false, err)
	}
	// 发现包只发一次：它极短，丢包时重发没有额外收益，反而会把一轮发现
	// 拖成两轮超时。真丢了由 Probe/调度器在下一轮重来（发现不消耗任何
	// 会话状态）。
	if err := c.sendDatagram(ctx, raw, "discover"); err != nil {
		return err
	}
	resp, err := c.readV3Datagram(ctx, "discover")
	if err != nil {
		return err
	}
	msg, err := snmpcodec.ParseV3(resp)
	if err != nil {
		return driver.E(driver.ErrProtocol, "discover", c.cfg.Device.ID,
			"malformed v3 datagram: "+err.Error(), false, err)
	}
	if msg.PDU.Type != snmpcodec.PDUReport {
		return driver.E(driver.ErrProtocol, "discover", c.cfg.Device.ID,
			fmt.Sprintf("discovery expected a Report PDU, got %s", msg.PDU.Type), false, nil)
	}
	if len(msg.Security.EngineID) == 0 {
		return driver.E(driver.ErrProtocol, "discover", c.cfg.Device.ID,
			"agent Report carried no engine ID", false, nil)
	}
	// 配置里手动写了 engineID，代理报的却是另一个：必须明说。
	//
	// 静默改用代理报的 engineID 会让「配置写错了」变成「看起来一切正常」，
	// 运维再也发现不了那份配置是错的；更糟的是，这类配置错往往伴随拓扑
	// 变更（换板、换设备），而静默纠正正好掩盖了那次变更。宁可让这台设备
	// 连不上并给出两个 engineID，也不要做无声的纠正。
	if len(s.fixedEngineID) > 0 && !bytes.Equal(s.fixedEngineID, msg.Security.EngineID) {
		return driver.E(driver.ErrProtocol, "discover", c.cfg.Device.ID, fmt.Sprintf(
			"代理的 engineID 是 %x，与配置里的 %x 不一致；删掉 engineID 让驱动自动发现",
			msg.Security.EngineID, s.fixedEngineID), false, nil)
	}
	s.setEngine(msg.Security.EngineID, msg.Security.EngineBoots, msg.Security.EngineTime)
	return nil
}

// setEngine 更新学到的引擎状态并记录学习时刻。
//
// 同时把密钥打回未就绪：密钥是 engineID 的函数，engineID 变了就必须重新
// 派生。这条不变式放在这里而不是让每个调用方自己清标志，是因为漏清一次的
// 症状（代理重装后一直认证失败）很难从日志上看出来。
func (s *v3Session) setEngine(id []byte, boots, tm int) {
	s.engineID = append([]byte(nil), id...)
	s.engineBoots = boots
	s.engineTime = tm
	s.engineTimeAt = time.Now()
	s.keysReady.Store(false)
}

// nextMsgID 返回下一个非零 msgID。
func (s *v3Session) nextMsgID() int32 {
	s.msgID++
	if s.msgID == 0 {
		s.msgID = 1
	}
	return s.msgID
}

// deriveKeys 用当前 engineID 派生本地化密钥。
//
// 每次 engineID 变更都要重新派生：密钥 = H(Ku || engineID || Ku)，engineID
// 变了密钥就变了。派生放在这里统一做，读路径只需读一次 keys。
func (s *v3Session) deriveKeys() error {
	keys := &snmpcodec.V3Keys{Auth: s.auth, Priv: s.priv}
	if s.auth != snmpcodec.AuthNone {
		authKu, err := snmpcodec.PasswordToKey(s.auth, string(s.authPassword))
		if err != nil {
			return err
		}
		authKey, err := snmpcodec.LocalizeKey(s.auth, authKu, s.engineID)
		if err != nil {
			return err
		}
		keys.AuthKey = authKey
	}
	if s.priv != snmpcodec.PrivNone {
		// 加密口令用**认证算法**做 1 MiB 扩展与本地化，不用加密算法：
		// DES/AES 本身不是摘要算法，USM 规定隐私密钥由同一族哈希派生，
		// 只是取前 16/24/32 字节。用「哈希口令只为加密」的想法会派生出
		// 一份代理不可能同意的密钥。
		privKey, err := s.localizedPrivKey()
		if err != nil {
			return err
		}
		keys.PrivKey = privKey
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
	return nil
}

// localizedPrivKey 派生隐私密钥。
//
// 关键点：**用的是认证算法的哈希**，不是加密算法。DES/AES 没有「摘要」这个
// 概念，USM 规定隐私口令走与认证口令相同的 1 MiB 扩展 + 本地化流程，然后
// 取前 16/24/32 字节。这也是「AES-256 必须配 SHA-256 及以上」的根因——
// 本地化后的密钥长度等于认证摘要长度，MD5 只给 16 字节，取不出 32 字节。
func (s *v3Session) localizedPrivKey() ([]byte, error) {
	ku, err := snmpcodec.PasswordToKey(s.auth, string(s.privPassword))
	if err != nil {
		return nil, err
	}
	key, err := snmpcodec.LocalizeKey(s.auth, ku, s.engineID)
	if err != nil {
		return nil, err
	}
	if need := s.priv.KeyLen(); len(key) < need {
		return nil, fmt.Errorf("priv %v needs %d bytes but %v gives %d", s.priv, need, s.auth, len(key))
	}
	return key, nil
}

// currentKeys 返回本地化的密钥快照（无锁读）。
func (s *v3Session) currentKeys() *snmpcodec.V3Keys {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys
}

// estimatedEngineTime 用学到的 engineTime 加上经过的秒数推算当前值。
//
// engineTime 是代理自己的秒表，从我们上次读取到现在会走。直接拿旧值去比对
// 必然在几分钟后报「时间窗口外」——这是最常见的假故障。估算不是精确同步
// （我们与代理之间有往返延迟），但 RFC 3414 的窗口有 150 秒，估算误差
// 远小于它。
func (s *v3Session) estimatedEngineTime() int {
	elapsed := int(time.Since(s.engineTimeAt).Seconds())
	return s.engineTime + elapsed
}

// engineTimeWindow 是 RFC 3414 §3.2 规定的时间窗口（秒）。
//
// 它不是「时钟容差」而是一条**防重放**约束：USM 的摘要只保证报文没被改过，
// 不保证它是新的。同一份带正确摘要的旧应答可以被原样重放，只有
// (engineBoots, engineTime) 的窗口能把它挡下来。
const engineTimeWindow = 150

// staleEngineError 表示应答里的引擎状态与本地缓存不符。
//
// 两种成因的处置不同，但都需要先刷新缓存再重试一次：
//   - 代理重启（boots 变了）：缓存的时间基准作废；
//   - 报文是重放（boots 更旧，或 |time-本地| 超过窗口）：本地基准其实是对的。
type staleEngineError struct {
	engineID    []byte
	engineBoots int
	engineTime  int
	reason      string
}

func (e *staleEngineError) Error() string {
	return fmt.Sprintf("v3 response rejected: %s (agent reports engineID %x, boots %d, time %d)",
		e.reason, e.engineID, e.engineBoots, e.engineTime)
}

// staleEngine 按 RFC 3414 §3.2 检查应答的引擎身份与时间窗口。
//
// 为什么要检查：认证摘要只覆盖「报文内容未被改动」，不覆盖「报文是新的」。
// 一条几天前抓到的合法应答可以被原样重放，若不比较 engineID/boots/time，
// 主站会把旧值当成新采集结果写进缓存——对计量、计数类点位就是静默回退。
//
// 返回 nil 表示可以接受。
func (s *v3Session) staleEngine(sec snmpcodec.USMSecurityParameters) *staleEngineError {
	if len(sec.EngineID) == 0 {
		return &staleEngineError{reason: "response carries no engine ID"}
	}
	if !bytes.Equal(sec.EngineID, s.engineID) {
		return &staleEngineError{
			engineID: sec.EngineID, engineBoots: sec.EngineBoots, engineTime: sec.EngineTime,
			reason: fmt.Sprintf("engine ID changed (cached %x)", s.engineID),
		}
	}
	if sec.EngineBoots != s.engineBoots {
		return &staleEngineError{
			engineID: sec.EngineID, engineBoots: sec.EngineBoots, engineTime: sec.EngineTime,
			reason: fmt.Sprintf("engineBoots %d, cached %d", sec.EngineBoots, s.engineBoots),
		}
	}
	if delta := sec.EngineTime - s.estimatedEngineTime(); delta > engineTimeWindow || delta < -engineTimeWindow {
		return &staleEngineError{
			engineID: sec.EngineID, engineBoots: sec.EngineBoots, engineTime: sec.EngineTime,
			reason: fmt.Sprintf("engineTime %d is %+d s from the expected %d (window ±%d s)",
				sec.EngineTime, delta, s.estimatedEngineTime(), engineTimeWindow),
		}
	}
	return nil
}

// probeV3 是 v3 的 Probe：确认链路并（首次）完成发现。
func (c *connState) probeV3(ctx context.Context) error {
	if err := c.v3.ensureReady(ctx, c); err != nil {
		return err
	}
	// 发现成功后发一条真实请求确认认证+加密都通。sysDescr 通常可读，
	// 即便 noSuchObject 也证明安全层通过了。
	id := c.nextID()
	pdu, err := snmpcodec.NewRequestPDU(snmpcodec.PDUGet, id, []snmpcodec.OID{sysDescrOID})
	if err != nil {
		return driver.E(driver.ErrInternal, "probe", c.cfg.Device.ID, err.Error(), false, err)
	}
	if _, err := c.transact(ctx, pdu, "probe"); err != nil {
		if driver.KindOf(err) == driver.ErrTimeout {
			return driver.E(driver.ErrTimeout, "probe", c.cfg.Device.ID,
				"no response from agent; check v3 user and security level", true, err)
		}
		return err
	}
	return nil
}
