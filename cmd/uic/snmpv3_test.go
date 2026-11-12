package main

import (
	"os"
	"testing"
)

// TestSNMPSecurityLevelLabel 钉住自检输出里的三档名字。
//
// 这三档名字在现场沟通里是通用词汇（authPriv / authNoPriv / noAuthNoPriv），
// 输出里写「加密+认证」反而对不上别人给的文档。
func TestSNMPSecurityLevelLabel(t *testing.T) {
	cases := []struct {
		auth, priv, want string
	}{
		{"sha256", "aes", "authPriv"},
		{"SHA-256", "AES-256", "authPriv"},
		{"md5", "none", "authNoPriv"},
		{"sha1", "", "authNoPriv"},
		{"none", "none", "noAuthNoPriv"},
		{"", "", "noAuthNoPriv"},
	}
	for _, c := range cases {
		got := securityLevelLabel(map[string]string{"authProtocol": c.auth, "privProtocol": c.priv})
		if got != c.want {
			t.Errorf("auth=%q priv=%q = %q, want %q", c.auth, c.priv, got, c.want)
		}
	}
}

// TestSNMPSecretResolution：命令行优先于环境变量；都没有时返回空串。
func TestSNMPSecretResolution(t *testing.T) {
	const env = "UIC_SNMP_TEST_SECRET"
	t.Setenv(env, "")
	if got := secret("flagpw", env); got != "flagpw" {
		t.Fatalf("flag value = %q", got)
	}
	t.Setenv(env, "envpw")
	if got := secret("", env); got != "envpw" {
		t.Fatalf("env value = %q", got)
	}
	if got := secret("flagpw", env); got != "flagpw" {
		t.Fatalf("flag should win over env, got %q", got)
	}
	os.Unsetenv(env)
	if got := secret("", env); got != "" {
		t.Fatalf("unset = %q", got)
	}
}
