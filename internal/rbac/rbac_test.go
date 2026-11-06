package rbac

import (
	"testing"
)

// TestPermissionMatrix 把「谁能做什么」钉成一张表。这张表就是权限系统的
// 规格：任何改动都会在这里体现，而不是藏在某个 handler 的 if 里。
func TestPermissionMatrix(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		// viewer：只看。
		{RoleViewer, PermRead, true},
		{RoleViewer, PermWrite, false},
		{RoleViewer, PermAlarmAck, false},
		{RoleViewer, PermCommand, false},
		{RoleViewer, PermConfigRead, false},
		{RoleViewer, PermConfigWrite, false},
		{RoleViewer, PermAuditRead, false},
		{RoleViewer, PermDiagnostics, false},

		// operator：能手动作业，但不动配置。
		{RoleOperator, PermRead, true},
		{RoleOperator, PermWrite, true},
		{RoleOperator, PermAlarmAck, true},
		{RoleOperator, PermCommand, true},
		{RoleOperator, PermCommandDanger, false},
		{RoleOperator, PermConfigRead, true},
		{RoleOperator, PermConfigWrite, false},
		{RoleOperator, PermAuditRead, false},
		{RoleOperator, PermDiagnostics, true},

		// admin：全部。
		{RoleAdmin, PermRead, true},
		{RoleAdmin, PermWrite, true},
		{RoleAdmin, PermAlarmAck, true},
		{RoleAdmin, PermCommand, true},
		{RoleAdmin, PermCommandDanger, true},
		{RoleAdmin, PermConfigRead, true},
		{RoleAdmin, PermConfigWrite, true},
		{RoleAdmin, PermAuditRead, true},
		{RoleAdmin, PermDiagnostics, true},
	}
	for _, c := range cases {
		if got := Can(c.role, c.perm); got != c.want {
			t.Errorf("Can(%s, %s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

// TestUnknownRoleDeniesEverything 是这套权限系统最重要的一条不变量：
// 角色名写错时必须是「全拒」，不能是「全过」。
func TestUnknownRoleDeniesEverything(t *testing.T) {
	for _, r := range []Role{"", "root", "Admin", "superuser", "viewer "} {
		for _, p := range []Permission{PermRead, PermWrite, PermConfigWrite} {
			if Can(r, p) {
				t.Fatalf("Can(%q, %s) = true; unknown roles must deny everything", r, p)
			}
		}
	}
}

// TestUnknownPermissionDenies 保证权限名拼错也是「拒」。
func TestUnknownPermissionDenies(t *testing.T) {
	for _, p := range []Permission{"", "write", "point.writes", "POINT.WRITE"} {
		if Can(RoleAdmin, p) {
			t.Fatalf("admin granted unknown permission %q", p)
		}
	}
}

// TestParseDefaultsToAdmin 兼容老配置：没有 role 字段的老部署一直是全权，
// 升级后不能突然降级成只读。
func TestParseDefaultsToAdmin(t *testing.T) {
	for _, in := range []string{"", "   ", "admin", "ADMIN"} {
		r, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", in, err)
		}
		if r != RoleAdmin {
			t.Fatalf("Parse(%q) = %s, want admin", in, r)
		}
	}
}

func TestParseAcceptsBuiltins(t *testing.T) {
	for _, want := range All() {
		got, err := Parse(string(want))
		if err != nil || got != want {
			t.Fatalf("Parse(%q) = %s, %v", want, got, err)
		}
	}
}

func TestParseRejectsUnknown(t *testing.T) {
	for _, in := range []string{"root", "super", "readonly", "viewer2"} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) succeeded", in)
		}
	}
}

// TestAllIsStable 保证角色顺序固定（UI 与帮助文本依赖它）。
func TestAllIsStable(t *testing.T) {
	got := All()
	want := []Role{RoleViewer, RoleOperator, RoleAdmin}
	if len(got) != len(want) {
		t.Fatalf("All() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("All()[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestValid(t *testing.T) {
	for _, r := range All() {
		if !Valid(r) {
			t.Fatalf("Valid(%s) = false", r)
		}
	}
	for _, r := range []Role{"", "root", "Admin"} {
		if Valid(r) {
			t.Fatalf("Valid(%q) = true", r)
		}
	}
}

// TestGrantsAreSortedAndComplete 保证 Grants 与实际权限一致。
func TestGrantsAreSortedAndComplete(t *testing.T) {
	for _, role := range All() {
		grants := Grants(role)
		for i := 1; i < len(grants); i++ {
			if grants[i-1] >= grants[i] {
				t.Fatalf("%s grants not sorted: %v", role, grants)
			}
		}
		// 逐个权限对照 Can。
		for _, p := range []Permission{
			PermRead, PermWrite, PermAlarmAck, PermCommand, PermCommandDanger,
			PermConfigRead, PermConfigWrite, PermAuditRead, PermDiagnostics,
		} {
			has := false
			for _, g := range grants {
				if g == p {
					has = true
				}
			}
			if has != Can(role, p) {
				t.Fatalf("%s: Grants says %v for %s, Can says %v", role, has, p, Can(role, p))
			}
		}
	}
}

// TestViewerIsSubsetOfOperatorIsSubsetOfAdmin 验证角色是包含关系。
// 如果某天 viewer 有了一项 operator 没有的权限，这套角色就失去了可解释性。
func TestRoleNesting(t *testing.T) {
	for _, p := range []Permission{
		PermRead, PermWrite, PermAlarmAck, PermCommand, PermCommandDanger,
		PermConfigRead, PermConfigWrite, PermAuditRead, PermDiagnostics,
	} {
		if Can(RoleViewer, p) && !Can(RoleOperator, p) {
			t.Fatalf("viewer has %s but operator does not", p)
		}
		if Can(RoleOperator, p) && !Can(RoleAdmin, p) {
			t.Fatalf("operator has %s but admin does not", p)
		}
	}
}

// TestCommandPermission 验证高危命令需要高危权限。
func TestCommandPermission(t *testing.T) {
	danger := []string{"outbox.drop", "config.rollback", "log.level"}
	for _, a := range danger {
		if got := CommandPermission(a); got != PermCommandDanger {
			t.Fatalf("CommandPermission(%q) = %s, want %s", a, got, PermCommandDanger)
		}
	}
	safe := []string{"driver.restart", "device.selftest", "outbox.flush", "alarm.ack", ""}
	for _, a := range safe {
		if got := CommandPermission(a); got != PermCommand {
			t.Fatalf("CommandPermission(%q) = %s, want %s", a, got, PermCommand)
		}
	}
}

// TestDangerCommandsRequireAdmin 把上一条接到角色上：operator 不能丢队列。
func TestDangerCommandsRequireAdmin(t *testing.T) {
	for _, a := range []string{"outbox.drop", "config.rollback", "log.level"} {
		if Can(RoleOperator, CommandPermission(a)) {
			t.Fatalf("operator can run %q", a)
		}
		if !Can(RoleAdmin, CommandPermission(a)) {
			t.Fatalf("admin cannot run %q", a)
		}
	}
}
