package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	snmpcodec "uic/internal/codec/snmp"
	"uic/internal/driver"
	snmpdrv "uic/internal/driver/snmp"
	"uic/internal/model"
)

// uic snmpread 是 SNMP 代理的自检命令（v1 / v2c / v3-USM）。
//
// SNMP 的「读不到」在现场通常只有一句「超时」，但原因至少有四层：
// community 错、版本错（v1 的代理不认 v2c 报文）、视图 ACL 不允许该
// 子树、OID 少写或多写一个实例号。v3 还要多三层：用户名、认证算法与
// 口令、加密算法与口令。这条命令把各层拆开：
//
//   - `-probe` 只读 sysDescr.0：收到任何应答（包括 noSuchObject）就证明
//     报文到达、团体名/用户凭据正确、版本被接受；
//   - `-oid` 批量 GET：一次读多个 OID，用来确认视图允许这些子树；
//   - `-walk` 从某个子树前缀做 GETNEXT 遍历：用来确认「OID 到底长什么样」
//     （MIB 名字和实例号在文档里经常对不上）；
//   - `-set` 单点写：验证读写权限与目标对象是否可写。
//
// v3 的凭据有三种给法（优先级从低到高）：命令行参数、环境变量、交互式
// 提示。之所以支持环境变量与交互式：口令写在命令行上会进 shell 历史、
// 会出现在 ps 里，而 v3 的口令恰恰是这套协议唯一的信任根。
//
// 输出保留线类型（INTEGER / Gauge32 / OCTET STRING …），因为同一个数值
// 在不同线类型下的含义不同：TimeTicks 是百分之一秒，Counter32 会回绕，
// Counter64 超过 2^53 经 float64 会丢精度。
//
// 边界：单次附着，不做周期轮询、不接收 Trap、不做 MIB 编译（所以只认
// 数字 OID，不认 sysDescr 这类名字）。
func snmpreadCmd(args []string) {
	fs := flag.NewFlagSet("snmpread", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:16161", "代理地址 host:port（省略端口按 161）")
	community := fs.String("community", "public", "v1/v2c 团体名（等同于口令；不要写在共享脚本里）")
	version := fs.String("version", "2c", "版本：1 | 2c | 3")
	oidList := fs.String("oid", "1.3.6.1.2.1.1.1.0,1.3.6.1.2.1.1.3.0,1.3.6.1.2.1.1.5.0,1.3.6.1.2.1.1.6.0",
		"OID 列表，逗号分隔")
	walk := fs.String("walk", "", "从该子树前缀做 GETNEXT 遍历（诊断用；与 -set 互斥）")
	maxWalk := fs.Int("max-walk", 128, "遍历最多返回多少个 varbind（1..1024）")
	probe := fs.Bool("probe", false, "只读 sysDescr.0 验证链路")
	setValue := fs.String("set", "", "写入值（需要恰好一个 -oid）")
	setType := fs.String("setType", "string", "写入类型：bool|int16|uint16|int32|uint32|int64|uint64|string|bytes")
	retries := fs.Int("retries", 2, "超时重发次数 0..10")
	raw := fs.Bool("raw", false, "显示线类型与十六进制原始字节")
	timeout := fs.Duration("timeout", 3*time.Second, "单次事务超时")
	asJSON := fs.Bool("json", false, "以 JSON 输出")

	// v3/USM 专用
	user := fs.String("user", "", "v3 用户名（USM securityName；-version 3 时必填）")
	auth := fs.String("auth", "sha256", "v3 认证算法：none|md5|sha1|sha224|sha256|sha384|sha512")
	authPass := fs.String("authPassword", "", "v3 认证口令（≥8 字节；也可用环境变量 UIC_SNMP_AUTH_PASSWORD）")
	priv := fs.String("priv", "none", "v3 加密算法：none|des|aes|aes192|aes256（aes192/aes256 需要 sha256 及以上）")
	privPass := fs.String("privPassword", "", "v3 加密口令（≥8 字节；留空时回退到认证口令）")
	engineID := fs.String("engineID", "", "v3 引擎标识（十六进制，可用冒号分隔；留空则自动发现）")
	fs.Parse(args)

	ver, err := snmpcodec.ParseVersion(*version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "-timeout 必须大于 0")
		os.Exit(2)
	}
	if *retries < 0 || *retries > 10 {
		fmt.Fprintln(os.Stderr, "-retries 必须在 0..10 之间")
		os.Exit(2)
	}
	if *maxWalk < 1 || *maxWalk > 1024 {
		fmt.Fprintln(os.Stderr, "-max-walk 必须在 1..1024 之间")
		os.Exit(2)
	}
	if *walk != "" && *setValue != "" {
		fmt.Fprintln(os.Stderr, "-walk 与 -set 不能同时使用")
		os.Exit(2)
	}

	labels := map[string]string{
		"community": *community,
		"version":   ver.String(),
		"options":   fmt.Sprintf("retries=%d", *retries),
	}
	if ver == snmpcodec.V3 {
		if strings.TrimSpace(*user) == "" {
			fmt.Fprintln(os.Stderr, "-version 3 需要 -user（USM securityName）")
			os.Exit(2)
		}
		if strings.TrimSpace(*engineID) != "" && len(strings.TrimSpace(*engineID)) < 10 {
			fmt.Fprintln(os.Stderr, "-engineID 至少 5 字节（10 个十六进制字符）；不确定就留空让驱动自动发现")
			os.Exit(2)
		}
		labels["username"] = strings.TrimSpace(*user)
		labels["authProtocol"] = strings.TrimSpace(*auth)
		labels["privProtocol"] = strings.TrimSpace(*priv)
		labels["authPassword"] = secret(*authPass, "UIC_SNMP_AUTH_PASSWORD")
		labels["privPassword"] = secret(*privPass, "UIC_SNMP_PRIV_PASSWORD")
		if strings.TrimSpace(*engineID) != "" {
			labels["engineID"] = strings.TrimSpace(*engineID)
		}
	}

	oids, err := snmpcodec.ParseOIDList(*oidList)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *setValue != "" && len(oids) != 1 {
		fmt.Fprintln(os.Stderr, "-set 只能配一个 -oid（SNMP SET 一次写一个对象）")
		os.Exit(2)
	}
	var walkPrefix snmpcodec.OID
	if *walk != "" {
		walkPrefix, err = snmpcodec.ParseOID(*walk)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
	}

	ch := model.Channel{
		ID: "ch-snmp", Protocol: "snmp",
		Transport: model.Transport{Kind: "udp", Address: *addr},
		Timeout:   model.Duration(*timeout),
	}
	dev := model.Device{
		ID: "selfcheck", ChannelID: ch.ID, Enabled: true,
		Interval: model.Duration(time.Second),
		Timeout:  model.Duration(*timeout),
		Labels:   labels,
	}

	// 自检的总预算：探测 1 次，读 1 次，遍历最多 maxWalk 次；v3 首轮还要
	// 加上「发现 + 可能的重新发现」，所以额外给 4 个事务的余量。
	budget := *timeout * time.Duration(*maxWalk+4)
	if ver == snmpcodec.V3 {
		budget += 4 * *timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	c, err := snmpdrv.New().Open(ctx, driver.OpenConfig{Channel: ch, Device: dev})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "链路探测（GET sysDescr.0）失败: %v\n", err)
		if ver == snmpcodec.V3 {
			fmt.Fprintln(os.Stderr, "  排查顺序：-user → -auth/-authPassword（口令 ≥8 字节且与代理一致）→ -priv/-privPassword → 地址/端口 → 代理的视图 ACL")
			fmt.Fprintln(os.Stderr, "  提示：认证失败会报权限错误而不是超时；超时通常是地址/端口不通，或代理对坏摘要静默丢弃")
		} else {
			fmt.Fprintln(os.Stderr, "  排查顺序：community（v1/v2c 的读写口令）→ 版本（-version 1|2c）→ 地址/端口 → 代理的视图 ACL 是否允许读 system 组")
		}
		os.Exit(1)
	}
	if *probe {
		if *asJSON {
			out := map[string]any{
				"addr": *addr, "version": ver.String(), "link": "up", "probe": "1.3.6.1.2.1.1.1.0",
			}
			if ver == snmpcodec.V3 {
				out["user"] = labels["username"]
				out["securityLevel"] = securityLevelLabel(labels)
			}
			emitJSON(out)
			return
		}
		fmt.Printf("链路正常: %s 版本 %s（GET sysDescr.0 收到应答）\n", *addr, ver)
		if ver == snmpcodec.V3 {
			fmt.Printf("  USM: 用户 %s，安全级别 %s\n", labels["username"], securityLevelLabel(labels))
		}
		return
	}

	if *walk != "" {
		runSNMPWalk(ctx, c, walkPrefix, *maxWalk, *raw, *asJSON)
		return
	}
	if *setValue != "" {
		runSNMPSet(ctx, c, oids[0], *setValue, *setType, *asJSON)
		return
	}
	runSNMPGet(ctx, c, oids, *raw, *asJSON)
}

// secret 取口令：命令行参数优先，其次环境变量。
//
// 环境变量这一档不是「方便」而是「安全」：口令写在命令行上会进 shell
// 历史、会出现在 ps 的完整命令行里（同机任何用户都能看到）。v3 的口令
// 是这套协议唯一的信任根，不该用这种方式泄漏。
//
// 两处都没给时返回空串，交给驱动报「口令不足 8 字节」——这条错误比
// 「认证失败」更能说明问题出在配置而不是对端。
func secret(flagValue, envName string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	return ""
}

// securityLevelLabel 把配置里的算法组合翻成 USM 的三档名字。
func securityLevelLabel(labels map[string]string) string {
	auth := strings.ToLower(strings.TrimSpace(labels["authProtocol"]))
	priv := strings.ToLower(strings.TrimSpace(labels["privProtocol"]))
	hasAuth := auth != "" && auth != "none"
	hasPriv := priv != "" && priv != "none"
	switch {
	case hasPriv && hasAuth:
		return "authPriv"
	case hasAuth:
		return "authNoPriv"
	}
	return "noAuthNoPriv"
}

// snmpWalker 是驱动的诊断接口。用类型断言而不是把它塞进 driver.Conn：
// 生产采集只需要 Read/Write，遍历是自检工具的能力。
type snmpWalker interface {
	Walk(context.Context, snmpcodec.OID, int) ([]snmpdrv.GetResult, error)
}

type snmpRawReader interface {
	GetRaw(context.Context, []snmpcodec.OID) ([]snmpdrv.GetResult, error)
}

// snmpRow 是自检输出的一行。
type snmpRow struct {
	OID     string `json:"oid"`
	Type    string `json:"type,omitempty"`
	Value   string `json:"value,omitempty"`
	Hex     string `json:"hex,omitempty"`
	Quality string `json:"quality,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (r snmpRow) display() string {
	if r.Error != "" {
		return "错误: " + r.Error
	}
	if r.Hex != "" {
		return r.Value + "  (0x" + r.Hex + ")"
	}
	return r.Value
}

func snmpRowOf(res snmpdrv.GetResult, showRaw bool) snmpRow {
	row := snmpRow{OID: res.OID.String()}
	if res.Err != nil {
		row.Quality = string(snmpQuality(res.Err))
		row.Error = res.Err.Error()
		return row
	}
	row.Quality = string(model.QualityGood)
	row.Type = res.Value.Type.String()
	row.Value = res.Value.String()
	if showRaw && len(res.Value.Bytes) > 0 {
		row.Hex = hex.EncodeToString(res.Value.Bytes)
	}
	return row
}

func snmpQuality(err error) model.Quality {
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}

func runSNMPGet(ctx context.Context, c driver.Conn, oids []snmpcodec.OID, showRaw, asJSON bool) {
	reader, ok := c.(snmpRawReader)
	if !ok {
		fmt.Fprintln(os.Stderr, "驱动不支持原始读取")
		os.Exit(1)
	}
	results, err := reader.GetRaw(ctx, oids)
	if err != nil && len(results) == 0 {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}

	rows := make([]snmpRow, 0, len(results))
	for _, res := range results {
		rows = append(rows, snmpRowOf(res, showRaw))
	}
	if asJSON {
		emitJSON(map[string]any{"read": len(rows), "rows": rows})
		return
	}
	if showRaw {
		printRow([]int{40, 14, 42, 12}, "OID", "类型", "值", "质量")
		for _, r := range rows {
			printRow([]int{40, 14, 42, 12}, r.OID, r.Type, r.display(), r.Quality)
		}
		return
	}
	printRow([]int{40, 44}, "OID", "值")
	for _, r := range rows {
		printRow([]int{40, 44}, r.OID, r.display())
	}
}

func runSNMPWalk(ctx context.Context, c driver.Conn, prefix snmpcodec.OID, max int, showRaw, asJSON bool) {
	w, ok := c.(snmpWalker)
	if !ok {
		fmt.Fprintln(os.Stderr, "驱动不支持遍历")
		os.Exit(1)
	}
	results, err := w.Walk(ctx, prefix, max)
	if err != nil && len(results) == 0 {
		fmt.Fprintf(os.Stderr, "遍历失败: %v\n", err)
		os.Exit(1)
	}

	rows := make([]snmpRow, 0, len(results))
	for _, res := range results {
		rows = append(rows, snmpRowOf(res, showRaw))
	}
	if asJSON {
		emitJSON(map[string]any{"walk": prefix.String(), "count": len(rows), "rows": rows, "error": errText(err)})
		return
	}
	if showRaw {
		printRow([]int{40, 14, 42, 12}, "OID", "类型", "值", "质量")
	} else {
		printRow([]int{40, 44}, "OID", "值")
	}
	for _, r := range rows {
		if showRaw {
			printRow([]int{40, 14, 42, 12}, r.OID, r.Type, r.display(), r.Quality)
		} else {
			printRow([]int{40, 44}, r.OID, r.display())
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "遍历中断: %v\n", err)
		os.Exit(1)
	}
}

func runSNMPSet(ctx context.Context, c driver.Conn, oid snmpcodec.OID, raw, typ string, asJSON bool) {
	val, err := parseSNMPSetValue(raw, typ)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	res, werr := c.Write(ctx, []driver.WriteRequest{{
		PointID:  oid.String(),
		Address:  model.Address{Kind: model.AddressSymbol, Path: oid.String(), Index: -1},
		DataType: model.DataType(typ),
		Value:    val,
	}})
	if werr != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", werr)
		os.Exit(1)
	}
	if len(res) == 1 && res[0].OK {
		if asJSON {
			emitJSON(map[string]any{"oid": oid.String(), "type": typ, "value": val.String(), "ok": true})
			return
		}
		fmt.Printf("写入 %s = %s（%s）完成\n", oid, val.String(), typ)
		return
	}
	var reason error
	if len(res) == 1 {
		reason = res[0].Err
	}
	if asJSON {
		emitJSON(map[string]any{"oid": oid.String(), "type": typ, "ok": false, "error": errText(reason)})
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "写入被拒绝: %v\n", reason)
	os.Exit(1)
}

// parseSNMPSetValue 把命令行字符串按指定类型解析成平台值。
//
// 整数用 base 0：现场既有写十进制 42 的，也有照文件写 0x2A 的；
// 只接受一种写法会让另一半人以为命令坏了。
func parseSNMPSetValue(raw, typ string) (model.Value, error) {
	raw = strings.TrimSpace(raw)
	switch model.DataType(typ) {
	case model.TypeBool:
		switch strings.ToLower(raw) {
		case "true", "1", "on", "yes", "开":
			return model.BoolValue(true), nil
		case "false", "0", "off", "no", "关":
			return model.BoolValue(false), nil
		}
		return model.Value{}, fmt.Errorf("布尔值 %q 不合法（true/false、1/0、on/off）", raw)
	case model.TypeInt16:
		n, err := strconv.ParseInt(raw, 0, 16)
		if err != nil {
			return model.Value{}, fmt.Errorf("int16 值 %q 不合法或越界（-32768..32767）", raw)
		}
		return model.NumValue(model.TypeInt16, float64(n)), nil
	case model.TypeInt32:
		n, err := strconv.ParseInt(raw, 0, 32)
		if err != nil {
			return model.Value{}, fmt.Errorf("int32 值 %q 不合法或越界", raw)
		}
		return model.NumValue(model.TypeInt32, float64(n)), nil
	case model.TypeInt64:
		n, err := strconv.ParseInt(raw, 0, 64)
		if err != nil {
			return model.Value{}, fmt.Errorf("int64 值 %q 不合法或越界", raw)
		}
		return model.NumValue(model.TypeInt64, float64(n)), nil
	case model.TypeUint16:
		n, err := strconv.ParseUint(raw, 0, 16)
		if err != nil {
			return model.Value{}, fmt.Errorf("uint16 值 %q 不合法或越界（0..65535）", raw)
		}
		return model.NumValue(model.TypeUint16, float64(n)), nil
	case model.TypeUint32:
		n, err := strconv.ParseUint(raw, 0, 32)
		if err != nil {
			return model.Value{}, fmt.Errorf("uint32 值 %q 不合法或越界（0..4294967295）", raw)
		}
		return model.NumValue(model.TypeUint32, float64(n)), nil
	case model.TypeUint64:
		n, err := strconv.ParseUint(raw, 0, 64)
		if err != nil {
			return model.Value{}, fmt.Errorf("uint64 值 %q 不合法或越界", raw)
		}
		return model.NumValue(model.TypeUint64, float64(n)), nil
	case model.TypeString:
		return model.TextValue(model.TypeString, raw), nil
	case model.TypeBytes:
		return model.TextValue(model.TypeBytes, raw), nil
	case model.TypeFloat32, model.TypeFloat64, model.TypeBCD16, model.TypeBCD32:
		return model.Value{}, fmt.Errorf("SNMP 没有标准浮点/BCD 可写类型（%s）；请用整型或 string", typ)
	}
	return model.Value{}, fmt.Errorf("未知写入类型 %q（支持 bool|int16|uint16|int32|uint32|int64|uint64|string|bytes）", typ)
}
