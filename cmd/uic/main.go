// Command uic 是单一可执行入口：run（默认）/ sim / mbread / mcread / finsread / uaread / s7blocks / s7upload / instantiate / import / token / version。
//
//	uic -config config.json            # 前台运行
//	uic run -config config.json        # 同上
//	uic -check -config config.json     # 只校验配置
//	uic sim -listen 127.0.0.1:15020    # 内置 Modbus TCP 从站
//	uic sim -protocol s7 -listen 127.0.0.1:15102   # 内置 S7 从站
//	uic sim -protocol opcua -listen 127.0.0.1:14840 # 内置 OPC UA 服务端
//	uic sim -protocol mqtt -listen 127.0.0.1:18830  # 内置 MQTT Broker（南向演示）
//	uic mbread -addr 127.0.0.1:1502    # 不依赖第三方工具的从站读写自检（TCP）
//	uic mbread -transport rtu -port /dev/ttyUSB0 -unit 1   # 串口从站自检
//	uic mcread -addr 127.0.0.1:5000 -area d -offset 100 -qty 2   # MC 3E 自检
//	uic finsread -addr 127.0.0.1:9600 -area dm -offset 100 -qty 2 # 欧姆龙 FINS 自检
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
	finsim "uic/internal/sim/fins"
	mcsim "uic/internal/sim/mc"
	sim "uic/internal/sim/modbus"
	simmqtt "uic/internal/sim/mqtt"
	sims7 "uic/internal/sim/s7"
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
  uic sim [-listen 127.0.0.1:15020] [-protocol modbus|s7|opcua|mqtt|mc|fins]
  uic mbread -addr 127.0.0.1:1502 -unit 1 -area holding -offset 0 -qty 2
  uic mbread -addr 127.0.0.1:1502 -area holding -offset 5 -write 777
  uic mcread -addr 127.0.0.1:5000 -area d -offset 100 -qty 2
  uic mcread -addr 127.0.0.1:5000 -area m -offset 0 -qty 16 -write on
  uic finsread -addr 127.0.0.1:9600 -area dm -offset 100 -qty 2
  uic finsread -addr 127.0.0.1:9600 -area cio -offset 10 -bit 3 -write on
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
	proto := fs.String("protocol", "modbus", "从站协议：modbus | s7 | opcua | mqtt | mc | fins")
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
	case "modbus":
	default:
		fmt.Fprintf(os.Stderr, "未知协议 %q（支持 modbus | s7 | opcua | mqtt | mc | fins）\n", *proto)
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
