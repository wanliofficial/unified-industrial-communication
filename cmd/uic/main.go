// Command uic 是单一可执行入口：run（默认）/ sim / mbread / mcread / finsread / enipread / bacread / iec104read / uaread / s7blocks / s7upload / instantiate / import / token / version。
//
//	uic -config config.json            # 前台运行
//	uic run -config config.json        # 同上
//	uic -check -config config.json     # 只校验配置
//	uic sim -listen 127.0.0.1:15020    # 内置 Modbus TCP 从站
//	uic sim -protocol s7 -listen 127.0.0.1:15102   # 内置 S7 从站
//	uic sim -protocol opcua -listen 127.0.0.1:14840 # 内置 OPC UA 服务端
//	uic sim -protocol mqtt -listen 127.0.0.1:18830  # 内置 MQTT Broker（南向演示）
//	uic sim -protocol dlt645 -listen 127.0.0.1:14001 # 内置电能表从站（645-2007）
//	uic sim -protocol snmp -listen 127.0.0.1:16161   # 内置 SNMP v1/v2c 代理（UDP）
//	uic sim -protocol ads -listen 127.0.0.1:48898    # 内置 ADS/AMS 服务端（TCP）
//	uic mbread -addr 127.0.0.1:1502    # 不依赖第三方工具的从站读写自检（TCP）
//	uic mbread -transport rtu -port /dev/ttyUSB0 -unit 1   # 串口从站自检
//	uic mcread -addr 127.0.0.1:5000 -area d -offset 100 -qty 2   # MC 3E 自检
//	uic finsread -addr 127.0.0.1:9600 -area dm -offset 100 -qty 2 # 欧姆龙 FINS 自检
//	uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG  # EtherNet/IP CIP 自检
//	uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 1  # BACnet/IP 自检
//	uic iec104read -addr 127.0.0.1:2404 -ca 1   # IEC 60870-5-104 自检（总召唤 + 上送对象）
//	uic snmpread -addr 127.0.0.1:16161 -oid 1.3.6.1.2.1.1.1.0   # SNMP v1/v2c 自检
//	uic adsread -addr 127.0.0.1:48898 -probe   # ADS/AMS 自检（设备信息握手）
//	uic uaread -addr 127.0.0.1:4840 -nodes ns=2;s=site/dev/point  # OPC UA 读写自检
//	uic s7blocks -addr 127.0.0.1:15102   # S7 块目录（-type db / -num 1 可细看）
//	uic s7upload -addr 127.0.0.1:15102 -type db -num 1 -out db1.bin  # 块上传备份
//	uic token -name ops -role operator   # 生成用户凭据（明文 + tokenSha256）
//
// 关闭顺序遵循 docs/03-product/02-architecture.md §11.2：
// 停止接收流量 → 停调度 → 排空 Outbox → 关连接，总预算 10 秒。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/config"
	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/opcuaserver"
	uicruntime "uic/internal/runtime"
	adssim "uic/internal/sim/ads"
	bacnetsim "uic/internal/sim/bacnet"
	cjt188sim "uic/internal/sim/cjt188"
	dlt645sim "uic/internal/sim/dlt645"
	enipsim "uic/internal/sim/enip"
	finsim "uic/internal/sim/fins"
	iec104sim "uic/internal/sim/iec104"
	mcsim "uic/internal/sim/mc"
	sim "uic/internal/sim/modbus"
	simmqtt "uic/internal/sim/mqtt"
	sims7 "uic/internal/sim/s7"
	snmpsim "uic/internal/sim/snmp"
)

// 构建期注入：-ldflags "-X main.version=... -X main.commit=..."
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && (args[0][0] != '-') {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		runCmd(args)
	case "sim":
		simCmd(args)
	case "mcread":
		mcreadCmd(args)
	case "finsread":
		finsreadCmd(args)
	case "enipread":
		enipreadCmd(args)
	case "bacread":
		bacreadCmd(args)
	case "iec104read":
		iec104readCmd(args)
	case "dlt645read":
		dlt645readCmd(args)
	case "cjt188read":
		cjt188readCmd(args)
	case "snmpread":
		snmpreadCmd(args)
	case "adsread":
		adsreadCmd(args)
	case "uaread":
		uareadCmd(args)
	case "s7blocks":
		s7blocksCmd(args)
	case "s7upload":
		s7uploadCmd(args)
	case "instantiate":
		instantiateCmd(args)
	case "import":
		importCmd(args)
	case "token":
		tokenCmd(args)
	case "version", "-v", "--version":
		printVersion()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `uic — 统一工业通信网关

用法:
  uic [run] -config config.json [-listen :8080] [-token TOKEN]
  uic -check -config config.json
  uic sim [-listen 127.0.0.1:15020] [-protocol modbus|s7|opcua|mqtt|mc|fins|enip|bacnet|iec104|dlt645|cjt188|snmp|ads]
  uic mbread -addr 127.0.0.1:1502 -unit 1 -area holding -offset 0 -qty 2
  uic mbread -addr 127.0.0.1:1502 -area holding -offset 5 -write 777
  uic mcread -addr 127.0.0.1:5000 -area d -offset 100 -qty 2
  uic mcread -addr 127.0.0.1:5000 -area m -offset 0 -qty 16 -write on
  uic finsread -addr 127.0.0.1:9600 -area dm -offset 100 -qty 2
  uic finsread -addr 127.0.0.1:9600 -area cio -offset 10 -bit 3 -write on
  uic enipread -addr 127.0.0.1:44818 -tag DINT_TAG [-type dint]
  uic enipread -addr 127.0.0.1:44818 -tag BOOL_TAG -type bool -write on
  uic bacread -addr 127.0.0.1:47808 -type analog-input -instance 1
  uic bacread -addr 127.0.0.1:47808 -type analog-value -instance 2 -write 55.5
  uic iec104read -addr 127.0.0.1:2404 -ca 1 [-ioa 1001,1002] [-json]
  uic iec104read -addr 127.0.0.1:2404 -area single-command -cmd-ioa 4001 -write on
  uic dlt645read -addr 127.0.0.1:14001 -address 000000000042 -di 00000000,02010100
  uic dlt645read -transport serial -port /dev/ttyUSB0 -baud 2400 -address 0042 -probe
  uic dlt645read -addr 127.0.0.1:14001 -address 42 -di 04000101 -write 20261107120000
  uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 -type 10 -di 901F:0,901F:5,901F:10,901F:17
  uic cjt188read -transport serial -port /dev/ttyUSB0 -baud 2400 -address 00000805000001 -probe
  uic cjt188read -addr 127.0.0.1:14001 -address 00000805000001 -di A017 -write close -allow-control
  uic snmpread -addr 127.0.0.1:16161 -oid 1.3.6.1.2.1.1.1.0,1.3.6.1.2.1.1.5.0 [-raw|-json]
  uic snmpread -addr 127.0.0.1:16161 -walk 1.3.6.1.2.1.1 -max-walk 16
  uic snmpread -addr 127.0.0.1:16161 -oid 1.3.6.1.2.1.1.6.0 -set lab-2 -setType string
  uic adsread -addr 127.0.0.1:48898 -probe [-netid 192.168.1.10.1.1] [-port 851]
  uic adsread -addr 127.0.0.1:48898 -symbol MAIN.nCounter:int16,MAIN.sRecipe:string:81 [-json]
  uic adsread -addr 127.0.0.1:48898 -symbol MAIN.nSetpoint:int16 -set 750
  uic adsread -addr 127.0.0.1:48898 -state
  uic adsread -addr 127.0.0.1:48898 -control run -allow-control
  uic mbread -transport rtu|ascii -port /dev/ttyUSB0 [-baud 19200] [-parity even]
  uic uaread -addr 127.0.0.1:4840 -nodes ns=2;s=site/dev/point [-type float64]
  uic uaread -addr 127.0.0.1:4840 -nodes ns=2;s=site/dev/setpoint -write 42.5
  uic s7blocks -addr 127.0.0.1:15102 [-type db [-num 1]] [-json]
  uic s7upload -addr 127.0.0.1:15102 -type db -num 1 [-out db1.bin|-]
  uic instantiate -template meter.json -param index=1 -channel ch-meter [-out meter-1.json]
  uic import -csv points.csv -device press -channel ch-modbus -protocol modbus-tcp
  uic import -csv points.csv -device press -config config.json -out config.new.json [-dry-run]
  uic token [-name ops] [-role viewer|operator|admin] [-count N] [-json]
  uic version

参数:
  -config     配置文件路径（默认 ./config.json）
  -listen     覆盖监听地址（例如 :8080）
  -token      覆盖 API Bearer Token
  -log-level  debug|info|warn|error
  -data       覆盖数据目录（默认取 config.storage.dir）
  -check      只校验配置并退出
  -version    打印版本并退出

uic token 生成的凭据同时给出明文与 tokenSha256：
  明文交给客户端，配置里只放 tokenSha256。明文只打印一次。
`)
}

func printVersion() {
	fmt.Printf("uic %s (commit %s, built %s, %s)\n", version, commit, buildTime, goruntime.Version())
}

// runCmd 启动完整服务。
func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "./config.json", "配置文件路径")
	listen := fs.String("listen", "", "覆盖监听地址")
	token := fs.String("token", "", "覆盖 API Token")
	logLevel := fs.String("log-level", "", "日志级别 debug|info|warn|error")
	dataDir := fs.String("data", "", "覆盖数据目录")
	check := fs.Bool("check", false, "只校验配置并退出")
	showVer := fs.Bool("version", false, "打印版本并退出")
	_ = fs.Parse(args)

	if *showVer {
		printVersion()
		return
	}
	if *check {
		os.Exit(checkConfig(*cfgPath))
	}

	rt, err := uicruntime.New(uicruntime.Options{
		ConfigPath: *cfgPath,
		DataDir:    *dataDir,
		Listen:     *listen,
		Token:      *token,
		LogLevel:   *logLevel,
		Version:    version,
		Commit:     commit,
		BuildTime:  buildTime,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rt.Start(ctx)
	log := rt.Logger()

	cfg := rt.ConfigManager().Current()
	srv := &http.Server{
		Addr:              rt.ListenAddr(),
		Handler:           rt.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// 不设置 WriteTimeout：SSE 是长连接。
	}

	errCh := make(chan error, 1)
	go func() {
		if cfg.Server.TLS.Enabled && cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "" {
			log.Info("http_listening", "addr", srv.Addr, "tls", true)
			errCh <- srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
			return
		}
		if cfg.Server.TLS.Enabled {
			log.Warn("tls_enabled_without_certificate",
				"detail", "TLS 已开启但未配置 certFile/keyFile，回退到明文 HTTP")
		}
		log.Info("http_listening", "addr", srv.Addr, "tls", false)
		errCh <- srv.ListenAndServe()
	}()

	// 认证状态按「凭据表」报告，而不是按单个 Token 字段：多用户配置下
	// server.token 可能为空，但认证是开着的。反过来，删光所有用户之后
	// 必须明确喊出「open」，否则运维会以为还在鉴权。
	authLine := "open（未配置凭据，任何能连上端口的人都是 admin）"
	if st := rt.AuthStore(); st != nil && !st.Open() {
		authLine = fmt.Sprintf("bearer（%d 个用户）", st.Len())
	}
	auditLine := "关闭"
	if st, ok := rt.AuditStats(); ok {
		auditLine = fmt.Sprintf("开启（%d 条，seq=%d）", st.Records, st.LastSeq)
	}
	fmt.Printf("uic %s | site=%s | config=%s\n", version, cfg.Site, rt.ConfigManager().Version())
	fmt.Printf("  API   http://%s/api/v1\n", srv.Addr)
	fmt.Printf("  SSE   http://%s/api/v1/stream\n", srv.Addr)
	fmt.Printf("  认证  %s\n", authLine)
	fmt.Printf("  审计  %s\n", auditLine)
	fmt.Printf("  设备  %d 台 / 通道 %d 条\n", len(cfg.Devices), len(cfg.Channels))
	fmt.Println("  Ctrl+C 停止")

	select {
	case <-ctx.Done():
		log.Info("shutdown_signal")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http_serve_failed", "error", err)
			_ = rt.Stop(5 * time.Second)
			os.Exit(1)
		}
	}

	// 1. 停止接收新请求。 2. 停调度并排空 Outbox。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http_shutdown_incomplete", "error", err)
	}
	if err := rt.Stop(5 * time.Second); err != nil {
		log.Warn("runtime_stop_incomplete", "error", err)
	}
}

// checkConfig 做纯校验（不写快照、不改状态文件），适合 CI 与部署前检查。
func checkConfig(path string) int {
	if err := uicruntime.RegisterBuiltins(); err != nil {
		fmt.Fprintf(os.Stderr, "驱动注册失败: %v\n", err)
		return 1
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		return 1
	}
	var f config.File
	if err := decodeStrict(raw, &f); err != nil {
		fmt.Fprintf(os.Stderr, "配置解析失败: %v\n", err)
		return 1
	}
	if err := f.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置校验失败: %v\n", err)
		return 1
	}
	// 协议与传输的匹配是静态配置错误，必须和字段校验一样在 CI 里就被拦下。
	// 只做 f.Validate() 会漏掉「modbus-tcp 配到串口上」这类问题，等到
	// 部署后才发现。
	if err := uicruntime.CheckDriverBindings(f.Channels); err != nil {
		fmt.Fprintf(os.Stderr, "配置校验失败: %v\n", err)
		return 1
	}
	canonical, err := f.Canonical()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置序列化失败: %v\n", err)
		return 1
	}

	fmt.Printf("配置有效\n")
	fmt.Printf("  site        %s\n", f.Site)
	fmt.Printf("  version     %s\n", config.VersionOf(canonical))
	fmt.Printf("  schema      %d\n", f.SchemaVersion)
	fmt.Printf("  listen      %s\n", f.Server.Listen)
	fmt.Printf("  storage     %s\n", f.Storage.Dir)
	fmt.Printf("  channels    %d\n", len(f.Channels))
	fmt.Printf("  devices     %d\n", len(f.Devices))
	points := 0
	for _, d := range f.Devices {
		points += len(d.Points)
	}
	fmt.Printf("  points      %d\n", points)
	if len(f.Alarms) > 0 {
		enabled := 0
		for _, a := range f.Alarms {
			if !a.Disabled {
				enabled++
			}
		}
		fmt.Printf("  alarms      %d（启用 %d）\n", len(f.Alarms), enabled)
	}
	if h := f.Storage.History; h.Enabled {
		beat := "关闭"
		if h.Heartbeat != nil && h.Heartbeat.Duration() > 0 {
			beat = h.Heartbeat.String()
		}
		fmt.Printf("  history     %s（上限 %s，保留 %s，心跳 %s）\n",
			h.Dir, humanBytes(h.MaxBytes), h.MaxAge.String(), beat)
	}
	fmt.Printf("  drivers     %d（%s）\n", len(driver.List()), driverIDs())
	if subs := subscribedChannels(f.Channels); len(subs) > 0 {
		fmt.Printf("  subscribe   %v\n", subs)
	}
	sinks := []string{}
	if f.Northbound.Webhook.Enabled {
		sinks = append(sinks, "webhook")
	}
	if f.Storage.Journal {
		sinks = append(sinks, "journal")
	}
	if len(sinks) == 0 {
		sinks = []string{"none（变化事件只进缓存/SSE）"}
	}
	fmt.Printf("  outbox      %v\n", sinks)
	if f.Northbound.MQTT.Enabled {
		fmt.Printf("  mqtt        %s（qos %d）\n", f.Northbound.MQTT.Broker, f.Northbound.MQTT.EffectiveQoS())
	}
	if o := f.Northbound.OPCUA; o.Enabled {
		exposed, writable := opcuaExposed(f)
		fmt.Printf("  opcua       %s（%d 点，可写 %d，匿名）\n", o.Listen, exposed, writable)
	}
	return 0
}

// opcuaExposed 统计北向地址空间里的变量数，与 runtime.OPCUASpecs 保持同一
// 口径：只看 enabled 的设备与点位。-check 的输出要和实际暴露的一致，否则
// 「配置说 12 个点、客户端看到 9 个」会成为现场排查的噪音。
func opcuaExposed(f config.File) (exposed, writable int) {
	for _, d := range f.Devices {
		if !d.Enabled {
			continue
		}
		for _, p := range d.Points {
			if !p.Enabled {
				continue
			}
			exposed++
			if p.Writable {
				writable++
			}
		}
	}
	return exposed, writable
}

// humanBytes 用二进制单位打印容量。配置里是字节数（精确），但人读的是 MB。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// subscribedChannels 列出开启了服务端订阅的通道。订阅改变了取数路径
// （推送 vs 轮询），必须在校验阶段就让人看到——它不会体现在点位数、
// 采集周期这些更显眼的字段上。
func subscribedChannels(chs []model.Channel) []string {
	var out []string
	for _, ch := range chs {
		if !ch.Subscribe {
			continue
		}
		desc := ch.ID
		if d := ch.PublishingInterval.Duration(); d > 0 {
			desc += "@" + d.String()
		}
		out = append(out, desc)
	}
	sort.Strings(out)
	return out
}

func driverIDs() string {
	ms := driver.List()
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "none"
	}
	out := ids[0]
	for _, id := range ids[1:] {
		out += ", " + id
	}
	return out
}

// simCmd 启动内置从站。-protocol 选择 Modbus TCP / S7 / OPC UA。
func simCmd(args []string) {
	fs := flag.NewFlagSet("sim", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:15020", "监听地址")
	size := fs.Int("size", 1024, "寄存器数量")
	proto := fs.String("protocol", "modbus", "从站协议：modbus | s7 | opcua | mqtt | mc | fins | enip | bacnet | iec104 | dlt645 | cjt188 | snmp | ads")
	pdu := fs.Int("pdu", 480, "S7 协商 PDU 长度（240/480/960）")
	fs.Parse(args)

	switch *proto {
	case "s7":
		simS7(*addr, *pdu)
		return
	case "opcua":
		simOPCUA(*addr)
		return
	case "mqtt":
		simMQTT(*addr)
		return
	case "mc":
		simMC(*addr)
		return
	case "fins":
		simFINS(*addr)
		return
	case "enip":
		simENIP(*addr)
		return
	case "bacnet":
		simBACnet(*addr)
		return
	case "iec104":
		simIEC104(*addr)
		return
	case "dlt645":
		simDLT645(*addr)
		return
	case "cjt188":
		simCJT188(*addr)
		return
	case "snmp":
		simSNMP(*addr)
		return
	case "ads":
		simADS(*addr)
		return
	case "modbus":
	default:
		fmt.Fprintf(os.Stderr, "未知协议 %q（支持 modbus | s7 | opcua | mqtt | mc | fins | enip | bacnet | iec104 | dlt645 | cjt188 | snmp | ads）\n", *proto)
		os.Exit(2)
	}

	srv, err := sim.NewServer(*addr, *size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动模拟从站失败: %v\n", err)
		os.Exit(1)
	}
	// 与 examples/modbus-loopback.json 对应的初始值。
	srv.SetHolding(0, 1234, 5678, 42, 7)
	srv.SetHolding(5, 0)
	srv.SetCoil(0, true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("modbus tcp slave listening on %s\n", srv.Addr())
	fmt.Println("  holding[0]=1234  [1]=5678  [2]=42  [3]=7  [5]=writable")
	fmt.Println("  coil[0]=true")
	fmt.Println("  holding[10] 每秒自增，可用于观察 SSE")
	fmt.Println("  Ctrl+C 停止")

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var n uint16
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				srv.SetHolding(10, n)
			}
		}
	}()

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simMC 启动内置 MC 3E 从站。初始值与 examples/mc.json 对应，便于
// `./uic sim -protocol mc -listen 127.0.0.1:5000 &` 之后直接跑 `./uic -config
// examples/mc.json` 观察链路。
func simMC(addr string) {
	srv, err := mcsim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 MC 从站失败: %v\n", err)
		os.Exit(1)
	}
	srv.SetWord("d", 0, 1234)
	srv.SetWord("d", 1, 5678)
	// float32 48.0 的 IEEE 大端是 42 40 00 00；MC 按「低字在前」存储，
	// 因此低字 0x0000 放 d2、高字 0x4240 放 d3，网关按 CDAB 读回 48.0。
	srv.SetWord("d", 2, 0x0000)
	srv.SetWord("d", 3, 0x4240)
	srv.SetBit("m", 0, true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("mc 3e slave listening on %s\n", srv.Addr())
	fmt.Println("  d[0]=1234  d[1]=5678  d[2..3]=float32(48.0, CDAB)  m[0]=true")
	fmt.Println("  Ctrl+C 停止")

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var n uint16
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				srv.SetWord("d", 10, n)
			}
		}
	}()

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simFINS 启动内置 FINS/TCP 从站。初始值与 examples/fins.json 对应，
// 便于 `./uic sim -protocol fins -listen 127.0.0.1:9600 &` 之后直接跑
// `./uic -config examples/fins.json` 观察链路。
func simFINS(addr string) {
	srv, err := finsim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 FINS 从站失败: %v\n", err)
		os.Exit(1)
	}
	srv.SetWord("dm", 0, 1234)
	srv.SetWord("dm", 1, 5678)
	// float32 48.0 = 0x42400000；FINS 低字在前：DM2=0x0000，DM3=0x4240。
	srv.SetWord("dm", 2, 0x0000)
	srv.SetWord("dm", 3, 0x4240)
	srv.SetBit("cio", 0, 0, true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("fins/tcp slave listening on %s\n", srv.Addr())
	fmt.Println("  DM0=1234  DM1=5678  DM2..3=float32(48.0, low word first)  CIO0.0=true")
	fmt.Println("  DM10 每秒自增，可用于观察 SSE")
	fmt.Println("  Ctrl+C 停止")

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var n uint16
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				srv.SetWord("dm", 10, n)
			}
		}
	}()

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simS7 启动内置 S7 从站。初始值刻意覆盖常见类型：REAL/INT/DINT/
// STRING/位/字，这样 examples/s7.json 一跑起来就能看到每一类都对。
func simS7(addr string, pduLen int) {
	srv, err := sims7.NewServer(addr, pduLen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 S7 模拟从站失败: %v\n", err)
		os.Exit(1)
	}
	srv.SetBytes(0x84, 1, 0, float32Bytes(12.5))
	srv.SetBytes(0x84, 1, 4, []byte{0xFF, 0xFD})             // INT16 -3
	srv.SetBytes(0x84, 1, 6, []byte{0x00, 0x01, 0x86, 0xA0}) // DINT 100000
	srv.SetBytes(0x84, 1, 10, []byte("PUMP-01\x00"))
	srv.SetBytes(0x84, 2, 0, []byte{0xBE, 0xEF})
	srv.SetBytes(0x81, 0, 0, []byte{0x00, 0x2A})
	srv.SetBit(0x83, 0, 0, 3, true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("s7 slave listening on %s (pdu=%d)\n", srv.Addr(), srv.PDULen())
	fmt.Println("  DB1.DBD0=12.5  DB1.DBW4=-3  DB1.DBD6=100000  DB1.DBB10=\"PUMP-01\"")
	fmt.Println("  DB2.DBW0=0xBEEF  IW0=42  M0.3=true")
	fmt.Println("  DB1.DBD0 每秒变化，可用于观察 SSE")
	fmt.Println("  Ctrl+C 停止")

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				i++
				srv.SetBytes(0x84, 1, 0, float32Bytes(float32(12.5+float64(i%10)/10)))
			}
		}
	}()

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

func float32Bytes(v float32) []byte {
	bits := math.Float32bits(v)
	return []byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}
}

// simOPCUA 启动内置 OPC UA 服务端。它实现的是「无安全策略 + 匿名」这一
// 最小可用面，正好覆盖现场最常见的一类服务端（老 PLC 网关、测试桩、
// 演示服务器），也是驱动侧当前支持的范围。
//
// 初始值刻意做成混合类型（double / int32 / bool / string），
// 这样 examples/opcua.json 一跑起来就能逐类核对类型映射是否正确。
func simOPCUA(addr string) {
	srv, err := opcuaserver.NewSimulator(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 OPC UA 模拟服务端失败: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("opc ua server listening on %s\n", srv.Addr())
	fmt.Println("  ns=2;s=Plant.Temperature=21.5 (double, 只读)")
	fmt.Println("  ns=2;s=Plant.Counter=0        (int32, 每秒自增)")
	fmt.Println("  ns=2;s=Plant.Running=true     (bool, 只读)")
	fmt.Println("  ns=2;s=Plant.Label=\"SIM-01\"   (string, 可写)")
	fmt.Println("  ns=2;s=Plant.Setpoint=60      (double, 可写)")
	fmt.Println("  ns=0;i=2258 CurrentTime 每秒刷新，可用于观察 SSE")
	fmt.Println("  Ctrl+C 停止")

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var n int32
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				srv.Set("ns=2;s=Plant.Counter", ua.Variant{Type: ua.TypeInt32, Int: int64(n)})
				srv.Set("ns=0;i=2258", ua.Variant{Type: ua.TypeDateTime, Time: time.Now()})
			}
		}
	}()

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simMQTT 启动内置 MQTT Broker 并周期性发布一组演示点位。
//
// 与 Modbus/S7/OPC UA 的内置从站同一个用途：让 examples/mqtt-southbound.json
// 不用先装 mosquitto 就能跑通「订阅 → 采集 → REST/SSE」整条链。
//
// 演示载荷刻意分两种形态，因为南向驱动必须同时支持：
//   - 纯文本标量（大量存量设备就是这么发的）；
//   - JSON 对象（新一点的边缘网关会一次发多个字段）。
//
// setpoint 模拟「可写点位」：Broker 收到网关发来的 PUBLISH 就更新本地状态，
// 下一次周期上报时把新值报回去。真实设备的闭环也是这样——写入只是发命令，
// 值是否生效要看设备下一次上报。没有这一步回显，示例里的写穿透永远只能停在
// uncertain，看的人会误以为写入坏了。
func simMQTT(addr string) {
	var (
		mu       sync.Mutex
		setpoint = 60.0
	)

	// echo 是「设备采纳写入后立刻上报」的回调。真实设备两种做法都有：
	// 等下一个上报周期（确认会晚一拍）、或变化即报（确认立刻成功）。这里
	// 用后者，好让示例里的写穿透能当场闭环。
	var echo func(string, float64)

	srv, err := simmqtt.New(addr, simmqtt.Options{
		OnMessage: func(m simmqtt.Message) {
			if m.Topic != "plant/line1/setpoint" {
				return
			}
			v, perr := strconv.ParseFloat(strings.TrimSpace(string(m.Payload)), 64)
			if perr != nil {
				return
			}
			mu.Lock()
			setpoint = v
			mu.Unlock()
			if echo != nil {
				echo(m.Topic, v)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 MQTT 模拟 Broker 失败: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	echo = func(topic string, v float64) {
		srv.Publish(topic, []byte(strconv.FormatFloat(v, 'g', -1, 64)), 1, false)
	}

	fmt.Printf("mqtt broker listening on %s (tcp://%s)\n", srv.Addr(), srv.Addr())
	fmt.Println("  周期发布（每秒一次）：")
	fmt.Println("    plant/line1/temp     \"21.0\"~\"21.9\"  纯文本标量")
	fmt.Println("    plant/line1/counter  \"N\"               每秒自增")
	fmt.Println("    plant/line1/state    {\"running\":true,\"mode\":\"auto\",\"cycle\":N}  JSON 对象")
	fmt.Println("    plant/line1/setpoint \"60\"              可写：向该主题发布即被采纳")
	fmt.Println("  Ctrl+C 停止")

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var n int
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				mu.Lock()
				sp := setpoint
				mu.Unlock()
				srv.Publish("plant/line1/temp",
					[]byte(fmt.Sprintf("%.1f", 21.0+float64(n%10)*0.1)), 1, false)
				srv.Publish("plant/line1/counter", []byte(strconv.Itoa(n)), 1, false)
				srv.Publish("plant/line1/state",
					[]byte(fmt.Sprintf(`{"running":true,"mode":"auto","cycle":%d}`, n)), 1, false)
				srv.Publish("plant/line1/setpoint",
					[]byte(strconv.FormatFloat(sp, 'g', -1, 64)), 1, false)
			}
		}
	}()

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simENIP 启动内置 EtherNet/IP（CIP 显式报文）从站。初始值与
// examples/enip.json 对应，便于 `./uic sim -protocol enip -listen
// 127.0.0.1:44818 &` 之后直接跑 `./uic -config examples/enip.json`。
func simENIP(addr string) {
	srv, err := enipsim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 EtherNet/IP 从站失败: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("ethernet/ip cip slave listening on %s\n", srv.Addr())
	fmt.Println("  DINT_TAG=1234  INT_TAG=5678  REAL_TAG=48.0  BOOL_TAG=true")
	fmt.Println("  STRING_TAG=\"PUMP-01\"  DINT_ARRAY=[10,20,30,40]")
	fmt.Println("  COUNTER 每秒自增，可用于观察 SSE")
	fmt.Println("  Ctrl+C 停止")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simIEC104 启动内置 IEC 60870-5-104（TCP）从站。
//
// 默认对象表覆盖了监视方向的主要类型（短浮点、标度化、单点、双点、累计量、
// 带时标）与控制方向的三种命令（单点、短浮点、标度化），初始值与
// examples/iec104.json 一一对应，便于「起从站 → 跑网关 → 看点位」。
func simIEC104(addr string) {
	srv, err := iec104sim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 IEC 104 从站失败: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("iec 60870-5-104 slave listening on %s (CA=1)\n", srv.Addr())
	fmt.Println("  监视方向：1001=42.5  1002=7.25  1003=1234(标度化)  1004=0.5(取代)")
	fmt.Println("            1005=-3.5(带时标)  2001=on  2002=off  2003=double 2(可遥控)")
	fmt.Println("            2004=double 1  3001=1000(累计量，每秒自增)")
	fmt.Println("  控制方向：4001=单点  4002=短浮点设定值  4003=标度化设定值（可写）")
	fmt.Println("  Ctrl+C 停止")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simDLT645 启动内置电能表从站（DL/T 645-2007）。
//
// 用 TCP 而不是串口，是因为自检的目的在于验证「主站的帧、DI、时序」是否
// 正确，而不是验证 USB 转 485 的驱动装没装。现场把串口服务器当透传用时
// 走的也是同一条字节流，因此这里的行为与真表一致：地址不匹配静默、数据
// 项只读、超长数据块分帧、广播命令执行但不应答。
//
// 初始值与 examples/dlt645.json 一一对应：先起这个从站，再跑
// `./uic -config examples/dlt645.json`，就能看到点位被正常采集。
func simDLT645(addr string) {
	srv, err := dlt645sim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 DL/T 645 从站失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("dl/t 645-2007 meter slave listening on %s (表地址 %s)\n", srv.Addr(), srv.Address())
	fmt.Println("  电能量：00000000=123456.78 组合有功总电能  00010000=123456.78 正向有功总电能")
	fmt.Println("         00020000=0 反向有功总电能  00030000=43.21 组合无功1总电能")
	fmt.Println("  瞬时量：02010100=220.0V A 相  02020100=5.123A A 相  02030000=1.2345kW 总有功")
	fmt.Println("         02060000=0.985 功率因数  02800002=50.00Hz 频率")
	fmt.Println("  需量：  01010000=12.3456kW（11-07 12:00）")
	fmt.Println("  可写：  04000101 日期时间（密码 P0..P2 全 0）  04000401 通信地址  04000B01 第 1 结算日")
	fmt.Println("  注意：  只应答本表地址；广播校时应答静默但生效；地址不匹配时静默")
	fmt.Println("  Ctrl+C 停止")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simCJT188 启动内置户用计量仪表从站（CJ/T 188-2004 水表）。
//
// 用 TCP 而不是串口：自检要验证的是「主站的帧、DI、字段偏移」是否正确，
// 而不是 USB 转 485 装没装。现场把串口服务器当透传用时走的也是同一条
// 字节流，因此从站行为与真表一致：地址不匹配静默、仪表类型决定 901F 的
// 字段布局、阀门与表地址可写、其余 DI 回异常应答。
//
// 初始值与 examples/cjt188.json 一一对应：先起这个从站，再跑
// `./uic -config examples/cjt188.json`，就能看到点位被正常采集。
func simCJT188(addr string) {
	srv, err := cjt188sim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 CJ/T 188 从站失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("cjt/t 188-2004 meter slave listening on %s (表地址 %s, 类型 %02X)\n",
		srv.Addr(), srv.Address(), srv.Type())
	fmt.Println("  901F:0=1234.56 m³ 当前累积流量  901F:5=1200.00 m³ 结算日累积流量")
	fmt.Println("  901F:10=实时时间（随从站时钟）  901F:17=状态字（随阀门状态）")
	fmt.Println("  D120..D12B=上 1..12 月结算日量（1200-n*10 m³）")
	fmt.Println("  可写：A017 阀门（0x55 开 / 0x99 关）  A018 表地址（14 位）")
	fmt.Println("  注意：只应答本表地址；读地址用 T/地址全 AA；其余 DI 回异常应答")
	fmt.Println("  Ctrl+C 停止")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simSNMP 启动内置 SNMP v1/v2c 代理（UDP）。
//
// 默认监听 16161 而不是 161：161 是特权端口，非 root 起不来；改成
// 16161 后，下面的自检命令与 examples/snmp.json 都能在开发机上直接跑。
// 真实设备的端口由 transport.address 决定，驱动不写死端口。
//
// 代理的初始值与 examples/snmp.json 对应：sysDescr/sysUpTime/sysName
// 只读，sysContact/sysLocation 可写，ifTable 里有两块网卡，ifAdminStatus
// 可写。community 不匹配时静默丢弃——这条语义与真实代理一致，也正是
// 「超时先查 community」这条排查建议的依据。
func simSNMP(addr string) {
	srv, err := snmpsim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 SNMP 代理失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("snmp v1/v2c agent listening on %s (community %q)\n", srv.Addr(), snmpsim.DefaultCommunity)
	fmt.Println("  system 组：1.3.6.1.2.1.1.1.0=sysDescr  1.3.6.1.2.1.1.3.0=sysUpTime(TimeTicks)")
	fmt.Println("            1.3.6.1.2.1.1.5.0=sysName  1.3.6.1.2.1.1.7.0=sysServices(INTEGER 72)")
	fmt.Println("  可写：    1.3.6.1.2.1.1.4.0=sysContact  1.3.6.1.2.1.1.6.0=sysLocation")
	fmt.Println("  ifTable： 1.3.6.1.2.1.2.2.1.{1,2,3,4,5,6,7,8,10,16}.{1,2}（两块网卡）")
	fmt.Println("            其中 .7.{1,2}=ifAdminStatus 可写，其余只读")
	fmt.Println("  v1 差异： 缺 OID 回 noSuchName（无 per-varbind 异常），SET 错误码映射到 v1 子集")
	fmt.Println("  Ctrl+C 停止")
	fmt.Println("  自检：   uic snmpread -addr " + srv.Addr() + " -probe")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simADS 启动内置 ADS/AMS 服务端（TCP）。
//
// 默认监听 48898（AMS/TCP 的标准端口），NetId 用 127.0.0.1.1.1、AMS 端口
// 851——与 examples/ads.json、adsread 的默认值一致。变量表见
// internal/sim/ads.loadDefaults：INT16/REAL/LREAL/BOOL/STRING/数组各一份，
// 其中 nSetpoint / fSpeed / bRun / sName / arr[2] 可写。
//
// 仿真端不实现符号表上传与通知订阅（本驱动也不做），未知 IndexGroup 一律
// 回 invalid group；但句柄、只读标记、长度校验与错误码都与真实设备一致。
func simADS(addr string) {
	srv, err := adssim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 ADS 服务端失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("ads/ams server listening on %s (NetId %s, AMS port %d)\n", srv.Addr(), srv.NetID(), srv.Port())
	fmt.Println("  可读：MAIN.nCounter INT16=1234  MAIN.fTemperature REAL=23.5  MAIN.bEnable BOOL=1")
	fmt.Println("        MAIN.sRecipe STRING(80)=\"batch-42\"  MAIN.arr[0..3] INT16=10/20/30/40")
	fmt.Println("  可写：MAIN.nSetpoint INT16=500  MAIN.fSpeed LREAL=1450.75  MAIN.bRun BOOL=0")
	fmt.Println("        MAIN.sName STRING(80)=\"uic-sim\"  MAIN.arr[2]（其余数组元素只读）")
	fmt.Println("  句柄：按变量名分配，PLC 重启（本仿真里是 InvalidateHandles）后失效")
	fmt.Println("  Ctrl+C 停止")
	fmt.Println("  自检：   uic adsread -addr " + srv.Addr() + " -probe")
	fmt.Println("  读点位： uic adsread -addr " + srv.Addr() + " -symbol MAIN.nCounter:int16,MAIN.sRecipe:string:81")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}

// simBACnet 启动内置 BACnet/IP（UDP）从站。初始值与 examples/bacnet.json
// 对应，便于 `./uic sim -protocol bacnet -listen 127.0.0.1:47808 &` 之后
// 直接跑 `./uic -config examples/bacnet.json`。
func simBACnet(addr string) {
	srv, err := bacnetsim.NewServer(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 BACnet/IP 从站失败: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("bacnet/ip slave listening on %s\n", srv.Addr())
	fmt.Println("  analog-input:1.present-value=42.5  object-name=\"AI-1\"  units=62")
	fmt.Println("  analog-value:2.present-value=0（可写）  priority-array 16 级")
	fmt.Println("  binary-input:1.present-value=1（active）  character-string:1=\"PUMP-01\"（可写）")
	fmt.Println("  analog-value:3 每秒自增，可用于观察 SSE")
	fmt.Println("  Ctrl+C 停止")

	<-ctx.Done()
	_ = srv.Close()
	slog.Info("sim_stopped")
}
