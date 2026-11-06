// Package rbac 是「谁能做什么」的判定表。
//
// 它是纯函数包：没有配置、没有 IO、没有状态。角色与权限的关系写死在代码里，
// 因为这是一条**安全边界**——能被配置文件改写的权限表，等于能被配置文件
// 绕过的权限表。要新增角色就改这个文件，让它经过一次 code review。
//
// 三个内置角色对应现场三种人：
//
//	viewer   值班员/看板：只能看
//	operator 值班长：能写点位、能确认报警、能重启驱动
//	admin    集成商/运维：能改配置、能删数据、能看审计
//
// 权限按**动作**而不是按**页面**划分。页面级权限在 API 上没法执行：一个人
// 打开了「配置页」，但他照样可以直接 POST /config/apply。所有判定都必须落在
// 服务端动作上，页面只是按钮的可见性。
package rbac

import (
	"fmt"
	"sort"
	"strings"
)

// Role 是内置角色。
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Permission 是一个可判定的动作。
type Permission string

const (
	// PermRead 是只读访问：设备、点位、实时值、历史、报警列表。
	PermRead Permission = "read"
	// PermWrite 是写点位。它是唯一会改变物理世界的常规动作。
	PermWrite Permission = "point.write"
	// PermAlarmAck 是确认报警。与写点位分开：确认只改状态，不动设备。
	PermAlarmAck Permission = "alarm.ack"
	// PermCommand 是执行白名单命令（重启驱动、自检、刷队列）。
	PermCommand Permission = "command"
	// PermCommandDanger 是执行不可逆命令（丢队列、回滚配置、改日志级别）。
	PermCommandDanger Permission = "command.danger"
	// PermConfigRead 是读配置（含版本、diff）。凭据已脱敏，但拓扑本身是
	// 攻击面信息，不给 viewer。
	PermConfigRead Permission = "config.read"
	// PermConfigWrite 是应用配置。
	PermConfigWrite Permission = "config.write"
	// PermAuditRead 是查询与校验审计日志。
	PermAuditRead Permission = "audit.read"
	// PermDiagnostics 是下载诊断包。包里含日志与配置，属于敏感数据。
	PermDiagnostics Permission = "diagnostics"
)

// permissions 是角色 → 权限集合。新增角色必须在这里出现，否则 Can 恒为 false。
var permissions = map[Role]map[Permission]bool{
	RoleViewer: {
		PermRead: true,
	},
	RoleOperator: {
		PermRead:        true,
		PermWrite:       true,
		PermAlarmAck:    true,
		PermCommand:     true,
		PermDiagnostics: true,
		PermConfigRead:  true,
	},
	RoleAdmin: {
		PermRead:          true,
		PermWrite:         true,
		PermAlarmAck:      true,
		PermCommand:       true,
		PermCommandDanger: true,
		PermConfigRead:    true,
		PermConfigWrite:   true,
		PermAuditRead:     true,
		PermDiagnostics:   true,
	},
}

// All 返回全部角色（顺序固定，供 UI 与帮助文本使用）。
func All() []Role { return []Role{RoleViewer, RoleOperator, RoleAdmin} }

// Parse 解析角色名。空字符串视为 admin：历史配置里只有 server.token 一个
// 凭据，它一直是「全权」，不能让升级把它降级成只读。
func Parse(s string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(RoleAdmin):
		return RoleAdmin, nil
	case string(RoleViewer):
		return RoleViewer, nil
	case string(RoleOperator):
		return RoleOperator, nil
	}
	return "", fmt.Errorf("rbac: unknown role %q (want viewer/operator/admin)", s)
}

// Valid 报告角色是否已知。
func Valid(r Role) bool {
	_, ok := permissions[r]
	return ok
}

// Can 判定角色是否拥有某个权限。
//
// 未知角色 / 未知权限一律 false：允许「默认通过」的权限系统是最糟的一种，
// 因为它在测试里永远看不出问题——直到有人写错一个字符串。
func Can(r Role, p Permission) bool {
	set, ok := permissions[r]
	if !ok {
		return false
	}
	return set[p]
}

// Grants 返回角色的全部权限（排序，便于日志与 UI 展示）。
func Grants(r Role) []Permission {
	set := permissions[r]
	out := make([]Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CommandPermission 把命令动作映射到所需权限。
//
// 判定表放在这里而不是 runtime：命令白名单在 runtime 里增删时，这里必须
// 同步——两处都在同一个仓库、同一份 review 里，但把「需要什么权限」放在
// 权限包里，能让越权评审只看一个文件。
func CommandPermission(action string) Permission {
	switch action {
	// 不可逆或影响全局的动作。
	case "outbox.drop", "config.rollback", "log.level":
		return PermCommandDanger
	default:
		return PermCommand
	}
}
