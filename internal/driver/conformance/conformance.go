// Package conformance 是驱动契约的共享一致性套件：把「每个注册驱动都必须
// 满足的元数据与寻址约束」写成可复跑的检查，任何驱动都能拿它自证合规。
//
// 存在理由：驱动是外部协议的适配层，数量多、由不同时期写成。注册表只校验了
// SDK 版本与「至少声明一种寻址」（driver.Register 里那两条），其余全靠自觉：
// 版本号写成空串、把未声明的传输写成 "bluetooth"、声明 bitAccess 却不支持
// register 寻址、配置表单里两个字段同名——这些都不会让任何测试变红，只会让
// 前端表单少一格、让现场配置在装配期才报错。
//
// 套件只依赖 driver/model 两个包，可被任何「能拿到驱动实例」的测试调用；
// 它自己也有单元测试（conformance_test.go 用故意写坏的驱动证明每一条检查
// 真的会失败），避免出现「套件永远绿」的假安全感。
//
// 边界：这里只校验**元数据契约**。协议的线上行为（编码、超时、重连）归各自的
// 金报文与虚拟设备测试，不在本套件范围。
package conformance

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"uic/internal/driver"
	"uic/internal/model"
)

// Reporter 是检查结果的接收方。*testing.T 天然满足；收窄成接口是为了让
// 套件本身可被单元测试（用一个记录失败的假 Reporter），且不把 testing
// 拉进生产依赖。
type Reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

var (
	protocolIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	versionRe    = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// 配置字段名会作为 JSON key 与前端表单 name，不允许空白与控制字符。
	fieldNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)
)

var (
	validRoles = map[driver.Role]bool{
		driver.RoleMaster: true, driver.RoleSlave: true, driver.RoleBoth: true,
	}
	validCapStates = map[driver.CapState]bool{
		driver.CapSupported:        true,
		driver.CapUnsupported:      true,
		driver.CapRuntimeDependent: true,
		driver.CapExperimental:     true,
	}
	// 传输种类与 model.Channel.Validate 的 switch 保持一致；少一种会让驱动
	// 声明了一个配置永远装配不起来的传输。
	validTransports = map[string]bool{
		"tcp": true, "udp": true, "serial": true, "memory": true,
	}
	validFieldTypes = map[string]bool{
		"string": true, "int": true, "float": true, "bool": true, "enum": true, "duration": true,
	}
	validFieldScopes = map[string]bool{
		"channel": true, "device": true, "point": true,
	}
)

// Check 校验单个驱动的元数据契约。所有失败都通过 r.Errorf 报告，便于一次看到
// 全部问题（而不是修一条跑一次）。
func Check(r Reporter, d driver.Driver) {
	r.Helper()
	if d == nil {
		r.Errorf("driver 为 nil")
		return
	}
	m := d.Meta()
	prefix := "driver " + m.ID

	checkIdentity(r, m, prefix)
	checkRoles(r, m, prefix)
	checkTransports(r, m, prefix)
	checkAddressKinds(r, m, prefix)
	checkCapabilities(r, m, prefix)
	checkFields(r, "configFields", m.ConfigFields, prefix)
	checkFields(r, "pointFields", m.PointFields, prefix)
	checkMetaStable(r, d, prefix)
}

// CheckRegistry 校验全局注册表自身的一致性：排序、可反查、非空。
//
// 「非空」不是凑数：注册表是全局副作用（各驱动的 init 写入），一旦某个包的
// init 被误删或构建标签把驱动排除掉，所有逐驱动的测试都会「因为遍历空列表
// 而通过」——这是最常见的假绿。
func CheckRegistry(r Reporter) {
	r.Helper()
	metas := driver.List()
	if len(metas) == 0 {
		r.Errorf("驱动注册表为空：没有任何驱动注册成功（检查各驱动包的 init 是否被排除）")
		return
	}
	prev := ""
	for i, m := range metas {
		if m.ID == "" {
			r.Errorf("注册表第 %d 项 ID 为空", i)
			continue
		}
		if i > 0 && m.ID <= prev {
			r.Errorf("注册表未按 ID 严格升序：%q 出现在 %q 之后", m.ID, prev)
		}
		prev = m.ID
		d, ok := driver.Get(m.ID)
		if !ok {
			r.Errorf("List 返回了 %q，但 Get 取不到（元数据与实例不一致）", m.ID)
			continue
		}
		if got := d.Meta().ID; got != m.ID {
			r.Errorf("Get(%q).Meta().ID = %q，注册表自相矛盾", m.ID, got)
		}
	}
}

func checkIdentity(r Reporter, m driver.Meta, prefix string) {
	if m.ID == "" {
		r.Errorf("driver 的 ID 为空（配置里无法引用）")
		return
	}
	if !protocolIDRe.MatchString(m.ID) {
		r.Errorf("%s: id %q 不合法（应为小写字母开头、仅含小写字母/数字/连字符）", prefix, m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		r.Errorf("%s: name 为空", prefix)
	}
	if m.Name != strings.TrimSpace(m.Name) {
		r.Errorf("%s: name %q 首尾有空白（前端展示会参差）", prefix, m.Name)
	}
	if m.Version == "" {
		r.Errorf("%s: version 为空（驱动市场与兼容性判断依赖它）", prefix)
	} else if !versionRe.MatchString(m.Version) {
		r.Errorf("%s: version %q 不是 x.y.z 形式", prefix, m.Version)
	}
	if m.SDKVersion != driver.SDKVersion {
		r.Errorf("%s: sdkVersion %q，want %q", prefix, m.SDKVersion, driver.SDKVersion)
	}
}

func checkRoles(r Reporter, m driver.Meta, prefix string) {
	if len(m.Roles) == 0 {
		r.Errorf("%s: roles 为空（至少要说明是主站还是从站）", prefix)
	}
	seen := map[driver.Role]bool{}
	for _, role := range m.Roles {
		if !validRoles[role] {
			r.Errorf("%s: 未知 role %q", prefix, role)
		}
		if seen[role] {
			r.Errorf("%s: role %q 重复", prefix, role)
		}
		seen[role] = true
	}
}

func checkTransports(r Reporter, m driver.Meta, prefix string) {
	if len(m.Transports) == 0 {
		r.Errorf("%s: transports 为空", prefix)
	}
	seen := map[string]bool{}
	for _, t := range m.Transports {
		if !validTransports[t] {
			r.Errorf("%s: 未知 transport %q（合法值 tcp/udp/serial/memory）", prefix, t)
			continue
		}
		if seen[t] {
			r.Errorf("%s: transport %q 重复", prefix, t)
		}
		seen[t] = true
		// 声明的传输必须能通过配置校验，否则用户照着元数据配出来的通道
		// 在装配期才报「unknown transport kind」。
		if err := minimalChannel(m.ID, t).Validate(); err != nil {
			r.Errorf("%s: 声明的 transport %q 无法构造合法通道：%v", prefix, t, err)
		}
	}
}

func checkAddressKinds(r Reporter, m driver.Meta, prefix string) {
	if len(m.AddressKinds) == 0 {
		r.Errorf("%s: addressKinds 为空", prefix)
	}
	seen := map[model.AddressKind]bool{}
	for _, k := range m.AddressKinds {
		if seen[k] {
			r.Errorf("%s: addressKind %q 重复", prefix, k)
		}
		seen[k] = true
		addr, ok := minimalAddress(k)
		if !ok {
			r.Errorf("%s: 未知 addressKind %q", prefix, k)
			continue
		}
		if !m.Accepts(k) {
			r.Errorf("%s: 声明了 addressKind %q 但 Meta.Accepts 返回 false", prefix, k)
		}
		// 声明的寻址范式必须能被平台统一地址模型表达，否则用户无法写出
		// 该驱动的任何点位。
		if err := addr.Validate(); err != nil {
			r.Errorf("%s: addressKind %q 的合法地址无法通过模型校验：%v", prefix, k, err)
		}
		if addr.Canonical() == "" {
			r.Errorf("%s: addressKind %q 的 Canonical() 为空", prefix, k)
		}
	}
}

func checkCapabilities(r Reporter, m driver.Meta, prefix string) {
	caps := map[string]driver.CapState{
		"read": m.Capabilities.Read, "write": m.Capabilities.Write,
		"subscribe": m.Capabilities.Subscribe, "browse": m.Capabilities.Browse,
		"discovery": m.Capabilities.Discovery, "multiRead": m.Capabilities.MultiRead,
		"bitAccess": m.Capabilities.BitAccess, "structTypes": m.Capabilities.StructTypes,
	}
	for name, state := range caps {
		if !validCapStates[state] {
			r.Errorf("%s: capabilities.%s = %q 不是 supported/unsupported/runtime_dependent/experimental 之一",
				prefix, name, state)
		}
	}
	// 平台的存在意义就是读；一个连 read 都不 supported 的驱动无法被任何
	// 设备使用。
	if m.Capabilities.Read != driver.CapSupported {
		r.Errorf("%s: capabilities.read = %q，必须 supported", prefix, m.Capabilities.Read)
	}
	// 其余能力都建立在「能读」之上：不能读却声明能写/能订阅是自相矛盾。
	for name, state := range caps {
		if name == "read" {
			continue
		}
		if state == driver.CapSupported && m.Capabilities.Read != driver.CapSupported {
			r.Errorf("%s: 声明 capabilities.%s=supported 但 read 不可用", prefix, name)
		}
	}
	// 位访问是寄存器地址才有的概念；声明支持却只接受符号/对象寻址，
	// 用户永远用不上它。
	if m.Capabilities.BitAccess == driver.CapSupported && !m.Accepts(model.AddressRegister) {
		r.Errorf("%s: 声明 capabilities.bitAccess=supported，却不接受 register 寻址", prefix)
	}
}

func checkFields(r Reporter, what string, fields []driver.ConfigField, prefix string) {
	byScope := map[string]map[string]bool{}
	for i, f := range fields {
		where := fmt.Sprintf("%s: %s[%d]", prefix, what, i)
		if !fieldNameRe.MatchString(f.Name) {
			r.Errorf("%s: 字段名 %q 不合法（会作为 JSON key 与表单 name）", where, f.Name)
			continue
		}
		if strings.TrimSpace(f.Label) == "" {
			r.Errorf("%s (%s): label 为空（表单没有可显示的标题）", where, f.Name)
		}
		if !validFieldTypes[f.Type] {
			r.Errorf("%s (%s): 未知 type %q", where, f.Name, f.Type)
		}
		if !validFieldScopes[f.Scope] {
			r.Errorf("%s (%s): 未知 scope %q（channel/device/point）", where, f.Name, f.Scope)
		}
		if f.Type == "enum" && len(f.Enum) == 0 {
			r.Errorf("%s (%s): type=enum 却没有可选值", where, f.Name)
		}
		// 名字看起来像凭据却没声明 Secret：前端会把明文口令渲染成普通文本框，
		// GET /config 也不会遮罩。driver.LooksSecret 是名字启发式的单一事实源
		// （覆盖 community/privatekey 这类名字看不出密码字样的凭据），Secret 才是
		// 权威声明——两者不一致说明驱动要么漏标 Secret，要么字段名起得比凭据还
		// 像凭据（后者同样该修，否则脱敏会越依赖猜名字）。
		if !f.Secret && driver.LooksSecret(f.Name) {
			r.Errorf("%s (%s): 字段名看起来是凭据，却未声明 secret:true", where, f.Name)
		}
		if byScope[f.Scope] == nil {
			byScope[f.Scope] = map[string]bool{}
		}
		if byScope[f.Scope][f.Name] {
			r.Errorf("%s: scope=%s 下字段名 %q 重复（表单会互相覆盖）", where, f.Scope, f.Name)
		}
		byScope[f.Scope][f.Name] = true
	}
}

// checkMetaStable 断言 Meta() 是纯函数：同一驱动两次调用必须逐字节一致。
// 元数据是可缓存的不可变对象；如果它每次返回不同内容（比如内部按调用累加、
// 或返回共享切片且调用方被允许修改），前端拿到的能力声明会漂移。
func checkMetaStable(r Reporter, d driver.Driver, prefix string) {
	first, err := json.Marshal(d.Meta())
	if err != nil {
		r.Errorf("%s: Meta() 无法序列化：%v", prefix, err)
		return
	}
	second, err := json.Marshal(d.Meta())
	if err != nil {
		r.Errorf("%s: Meta() 第二次序列化失败：%v", prefix, err)
		return
	}
	if string(first) != string(second) {
		r.Errorf("%s: Meta() 两次调用结果不一致（元数据必须是不可变对象）", prefix)
	}
}

// minimalChannel 为某种传输构造一条最小合法通道，用于验证「元数据声明的
// 传输真的能被配置出来」。
func minimalChannel(protocol, transport string) model.Channel {
	ch := model.Channel{ID: "ch-conformance", Protocol: protocol}
	switch transport {
	case "tcp", "udp":
		ch.Transport = model.Transport{Kind: transport, Address: "127.0.0.1:1"}
	case "serial":
		ch.Transport = model.Transport{
			Kind: "serial", Port: "/dev/ttyS0",
			Baud: 9600, DataBits: 8, Parity: "none", StopBits: 1,
		}
	case "memory":
		ch.Transport = model.Transport{Kind: "memory"}
	}
	return ch
}

// minimalAddress 为每种寻址范式构造一个最小合法地址。返回 false 表示该范式
// 不在平台的地址模型里（驱动声明了模型不认识的东西）。
func minimalAddress(kind model.AddressKind) (model.Address, bool) {
	switch kind {
	case model.AddressRegister:
		return model.Address{Kind: kind, Area: "holding", Offset: 0, Length: 1, Bit: -1}, true
	case model.AddressSymbol:
		return model.Address{Kind: kind, Path: "value", Index: -1}, true
	case model.AddressObject:
		return model.Address{Kind: kind, ObjectType: "analogInput", Property: "presentValue", Instance: 1}, true
	case model.AddressFile:
		return model.Address{Kind: kind, FileID: 1, RecordID: 1}, true
	case model.AddressComputed:
		return model.Address{Kind: kind, Expr: "1+1"}, true
	default:
		return model.Address{}, false
	}
}
