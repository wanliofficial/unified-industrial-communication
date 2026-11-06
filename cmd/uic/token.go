package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"uic/internal/auth"
	"uic/internal/config"
	"uic/internal/rbac"
)

// token 子命令生成用户凭据。
//
// 存在的理由不是「生成一个随机串」——openssl rand 也能做——而是保证
// **生成的哈希与配置文件期望的格式完全一致**。手工算 sha256 时最常见的
// 三个坑是：把换行一起哈希了、输出大写十六进制、对 base64 后的串再哈希。
// 这三件事都会在启动时表现为「配置有效但登录 401」，而且没有报错。
//
// 输出刻意同时给出明文与哈希：
//   - 明文给客户端（写进 SCADA/脚本的 Authorization 头）；
//   - 哈希给配置文件（tokenSha256），这样配置文件泄露不等于凭据泄露。
func tokenCmd(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	name := fs.String("name", "ops", "用户名（写入 server.users[].name）")
	role := fs.String("role", string(rbac.RoleOperator), "角色：viewer|operator|admin")
	nbytes := fs.Int("bytes", auth.DefaultTokenBytes, "随机字节数（至少 12）")
	count := fs.Int("count", 1, "一次生成的凭据条数")
	asJSON := fs.Bool("json", false, "输出可直接粘贴的 server.users JSON 数组")
	fs.Parse(args)

	r, err := rbac.Parse(*role)
	if err != nil {
		fmt.Fprintf(os.Stderr, "角色无效: %v（可选 %s）\n", err, joinRoles())
		os.Exit(2)
	}
	if *count < 1 || *count > 50 {
		fmt.Fprintln(os.Stderr, "-count 必须在 1..50 之间")
		os.Exit(2)
	}
	if strings.TrimSpace(*name) == "" {
		fmt.Fprintln(os.Stderr, "-name 不能为空")
		os.Exit(2)
	}

	type issued struct {
		Name   string `json:"name"`
		Role   string `json:"role"`
		Token  string `json:"-"`
		Sha256 string `json:"tokenSha256"`
	}
	out := make([]issued, 0, *count)
	for i := 0; i < *count; i++ {
		tok, err := auth.GenerateToken(*nbytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "生成失败: %v\n", err)
			os.Exit(1)
		}
		// 生成后立刻自检：如果这里过不了，写进配置也会启动失败。
		if err := auth.ValidateToken(tok); err != nil {
			fmt.Fprintf(os.Stderr, "生成的 Token 未通过校验（这是一个 bug）: %v\n", err)
			os.Exit(1)
		}
		n := *name
		if *count > 1 {
			n = fmt.Sprintf("%s-%d", *name, i+1)
		}
		out = append(out, issued{Name: n, Role: string(r), Token: tok, Sha256: auth.HashToken(tok)})
	}

	if *asJSON {
		users := make([]config.User, len(out))
		for i, u := range out {
			users[i] = config.User{Name: u.Name, Role: u.Role, TokenSha256: u.Sha256}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(users); err != nil {
			fmt.Fprintf(os.Stderr, "输出失败: %v\n", err)
			os.Exit(1)
		}
		// 明文只写 stderr，避免被 `> users.json` 重定向进仓库。
		fmt.Fprintln(os.Stderr, "警告: 明文 Token 只打印在下面，不会写入上面的 JSON；请立刻保存到密码管理器。")
		for _, u := range out {
			fmt.Fprintf(os.Stderr, "  %s  %s\n", u.Name, u.Token)
		}
		return
	}

	fmt.Println("# 明文 Token 只显示这一次。客户端用它做 `Authorization: Bearer <token>`；")
	fmt.Println("# 配置文件只保存 tokenSha256（下面 json 片段里的值）。")
	fmt.Println()
	for _, u := range out {
		fmt.Printf("用户    %s\n", u.Name)
		fmt.Printf("角色    %s\n", u.Role)
		fmt.Printf("Token   %s\n", u.Token)
		fmt.Printf("sha256  %s\n", u.Sha256)
		fmt.Println()
	}

	users := make([]config.User, len(out))
	for i, u := range out {
		users[i] = config.User{Name: u.Name, Role: u.Role, TokenSha256: u.Sha256}
	}
	raw, err := json.MarshalIndent(users, "  ", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "输出失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("把下面这段放进 config.json 的 server.users：")
	fmt.Println()
	fmt.Printf("  \"users\": %s\n", raw)
	fmt.Println()
	fmt.Println("（数组里的元素在用 -json 时可以直接粘贴；上面的缩进只是为了适配 server 段落。）")
}

func joinRoles() string {
	all := rbac.All()
	parts := make([]string, len(all))
	for i, r := range all {
		parts[i] = string(r)
	}
	return strings.Join(parts, "|")
}
