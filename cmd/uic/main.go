// Command uic 是单一可执行入口：run（默认）/ sim / mbread / version。
//
//	uic -config config.json            # 前台运行
//	uic run -config config.json        # 同上
//	uic -check -config config.json     # 只校验配置
//	uic sim -listen 127.0.0.1:15020    # 内置 Modbus TCP 从站
//	uic sim -protocol s7 -listen 127.0.0.1:15102   # 内置 S7 从站
//	uic sim -protocol opcua -listen 127.0.0.1:14840 # 内置 OPC UA 服务端
//	uic mbread -addr 127.0.0.1:1502    # 不依赖第三方工具的从站读写自检
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
	"syscall"
	"time"

	ua "uic/internal/codec/opcua"
	"uic/internal/config"
	"uic/internal/driver"
	uicruntime "uic/internal/runtime"
	sim "uic/internal/sim/modbus"
	simopcua "uic/internal/sim/opcua"
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
	case "mbread":
		mbreadCmd(args)
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
  uic sim [-listen 127.0.0.1:15020] [-protocol modbus|s7|opcua]
  uic mbread -addr 127.0.0.1:1502 -unit 1 -area holding -offset 0 -qty 2
  uic mbread -addr 127.0.0.1:1502 -area holding -offset 5 -write 777
  uic version

参数:
  -config     配置文件路径（默认 ./config.json）
  -listen     覆盖监听地址（例如 :8080）
  -token      覆盖 API Bearer Token
  -log-level  debug|info|warn|error
  -data       覆盖数据目录（默认取 config.storage.dir）
  -check      只校验配置并退出
  -version    打印版本并退出
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

	auth := "disabled（仅限开发）"
	if rt.Token() != "" {
		auth = "bearer"
	}
	fmt.Printf("uic %s | site=%s | config=%s\n", version, cfg.Site, rt.ConfigManager().Version())
	fmt.Printf("  API   http://%s/api/v1\n", srv.Addr)
	fmt.Printf("  SSE   http://%s/api/v1/stream\n", srv.Addr)
	fmt.Printf("  认证  %s\n", auth)
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
	fmt.Printf("  drivers     %d（%s）\n", len(driver.List()), driverIDs())
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
		fmt.Printf("  mqtt        已配置（本骨架未实现投递）\n")
	}
	return 0
}

func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
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
	proto := fs.String("protocol", "modbus", "从站协议：modbus | s7 | opcua")
	pdu := fs.Int("pdu", 480, "S7 协商 PDU 长度（240/480/960）")
	fs.Parse(args)

	switch *proto {
	case "s7":
		simS7(*addr, *pdu)
		return
	case "opcua":
		simOPCUA(*addr)
		return
	case "modbus":
	default:
		fmt.Fprintf(os.Stderr, "未知协议 %q（支持 modbus | s7 | opcua）\n", *proto)
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
	srv, err := simopcua.NewServer(addr)
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
