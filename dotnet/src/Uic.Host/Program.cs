using Microsoft.AspNetCore.StaticFiles;
using Microsoft.Extensions.Logging;
using Uic.Config;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Driver.Virtual;
using Uic.Northbound;
using Uic.Obs;
using Uic.Runtime;

namespace Uic.Host;

internal static class Program
{
    private const string GatewayVersion = "0.1.0";

    public static async Task<int> Main(string[] args)
    {
        CliOptions cli;
        try
        {
            cli = CliOptions.Parse(args);
        }
        catch (ArgumentException ex)
        {
            Console.Error.WriteLine(ex.Message);
            Console.Error.WriteLine();
            Console.Error.WriteLine(CliOptions.Usage);
            return 64;
        }

        switch (cli.Command)
        {
            case "help":
                Console.WriteLine(CliOptions.Usage);
                return 0;

            case "version":
                PrintVersion();
                return 0;

            case "check":
                return await CheckAsync(cli).ConfigureAwait(false);

            case "sim":
                return await RunAsync(cli, simulate: true).ConfigureAwait(false);

            default:
                return await RunAsync(cli, simulate: false).ConfigureAwait(false);
        }
    }

    private static void PrintVersion()
    {
        var runtime = Environment.Version.ToString();
        Console.WriteLine($"uic {GatewayVersion}");
        Console.WriteLine($"api {Northbound.RestApi.ApiVersion}  schema {Northbound.RestApi.SchemaVersion}");
        Console.WriteLine($"runtime dotnet {runtime}  os {Environment.OSVersion.VersionString}");
    }

    /// <summary>静态校验：只解析与检查，不启动任何线程。CI 与部署前检查都走这条路径。</summary>
    private static async Task<int> CheckAsync(CliOptions cli)
    {
        var path = cli.ConfigPath ?? "./uic.json";

        if (!File.Exists(path))
        {
            Console.Error.WriteLine($"config file '{path}' does not exist");
            return 3;
        }

        try
        {
            var json = await File.ReadAllTextAsync(path).ConfigureAwait(false);
            var doc = ConfigStore.Parse(json, path);

            Console.WriteLine($"ok: {path}");
            Console.WriteLine($"  site: {doc.Site}");
            Console.WriteLine($"  channels: {doc.Channels.Count}, devices: {doc.Devices.Count}, " +
                              $"points: {doc.Devices.Sum(d => d.Points.Count)}");

            var northbound = new List<string>();
            if (doc.Northbound.Rest.Enabled) northbound.Add($"rest({doc.Northbound.Rest.Listen})");
            if (doc.Northbound.Mqtt.Enabled) northbound.Add($"mqtt({doc.Northbound.Mqtt.Host}:{doc.Northbound.Mqtt.Port})");
            if (doc.Northbound.OpcUa.Enabled) northbound.Add($"opcua({doc.Northbound.OpcUa.Endpoint})");
            Console.WriteLine($"  northbound: {(northbound.Count == 0 ? "none" : string.Join(", ", northbound))}");

            return 0;
        }
        catch (ConfigException ex)
        {
            Console.Error.WriteLine($"invalid: {path}");
            Console.Error.WriteLine(ex.ToDetailedString());
            return 2;
        }
        catch (Exception ex)
        {
            Console.Error.WriteLine($"cannot read '{path}': {ex.Message}");
            return 3;
        }
    }

    private static async Task<int> RunAsync(CliOptions cli, bool simulate)
    {
        var dataDir = cli.DataDir ?? "./data";
        var level = new LogLevelSwitch();
        if (cli.Verbose) level.Level = LogLevel.Debug;

        var ring = new RingBufferLogProvider(capacity: 2000);

        using var loggerFactory = LoggerFactory.Create(builder =>
        {
            builder.SetMinimumLevel(LogLevel.Trace);
            builder.AddProvider(ring);
            builder.AddSimpleConsole(options =>
            {
                options.SingleLine = true;
                options.TimestampFormat = "HH:mm:ss.fff ";
                options.UseUtcTimestamp = false;
            });
            builder.AddFilter((_, logLevel) => logLevel >= level.Level && logLevel != LogLevel.None);
        });

        var log = loggerFactory.CreateLogger("uic.host");

        ConfigDocument Fallback() => DemoConfig.Build(simulate, cli.SimPort);

        // 先读一次配置：监听地址、静态目录都来自配置，必须在 Build() 之前确定。
        ConfigDocument bootstrap;
        try
        {
            var store = new ConfigStore(dataDir);
            bootstrap = await store.BootstrapAsync(cli.ConfigPath ?? string.Empty, Fallback).ConfigureAwait(false);
        }
        catch (ConfigException ex)
        {
            log.LogCritical("configuration is invalid{NewLine}{Details}", Environment.NewLine, ex.ToDetailedString());
            return 2;
        }
        catch (Exception ex)
        {
            log.LogCritical(ex, "cannot bootstrap configuration from {DataDir}", dataDir);
            return 3;
        }

        var listen = cli.Listen ?? bootstrap.Northbound.Rest.Listen;
        var webRoot = cli.WebRoot
                      ?? bootstrap.Northbound.Rest.StaticRoot
                      ?? Path.Combine(AppContext.BaseDirectory, "wwwroot");

        var hasWeb = Directory.Exists(webRoot) && File.Exists(Path.Combine(webRoot, "index.html"));

        var builder = WebApplication.CreateBuilder(new WebApplicationOptions
        {
            Args = Array.Empty<string>(),
            WebRootPath = hasWeb ? Path.GetFullPath(webRoot) : null,
        });

        builder.Logging.ClearProviders();
        builder.Logging.SetMinimumLevel(LogLevel.Trace);
        builder.Logging.AddProvider(ring);
        builder.Logging.AddSimpleConsole(options =>
        {
            options.SingleLine = true;
            options.TimestampFormat = "HH:mm:ss.fff ";
        });
        builder.Logging.AddFilter((category, logLevel) =>
        {
            if (logLevel == LogLevel.None) return false;

            var minimum = level.Level;

            // 框架自身的 Info 日志噪音太大（每条请求都要打一行），默认压到 Warning。
            if ((category ?? string.Empty).StartsWith("Microsoft.", StringComparison.Ordinal) &&
                minimum < LogLevel.Warning)
            {
                minimum = LogLevel.Warning;
            }

            return logLevel >= minimum;
        });

        builder.WebHost.UseUrls(listen.StartsWith("http", StringComparison.OrdinalIgnoreCase)
            ? listen
            : $"http://{listen}");

        var drivers = new DriverRegistry();
        drivers.Register(ModbusMasterDriver.Tcp(), ModbusMasterDriver.Rtu(), ModbusMasterDriver.Ascii());
        drivers.Register(new VirtualDriver());

        Simulator? simulator = null;
        if (simulate)
        {
            simulator = new Simulator(cli.SimPort, loggerFactory.CreateLogger("uic.sim"));
            simulator.Start();
        }

        GatewayRuntime runtime;
        try
        {
            runtime = await GatewayRuntime.CreateAsync(
                new RuntimeOptions
                {
                    DataDir = dataDir,
                    ConfigPath = cli.ConfigPath,
                    GatewayVersion = GatewayVersion,
                    Level = level,
                    Fallback = Fallback,
                },
                drivers,
                loggerFactory,
                ring).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            log.LogCritical(ex, "gateway failed to start");
            if (simulator is not null) await simulator.DisposeAsync().ConfigureAwait(false);
            return 1;
        }

        var rest = runtime.CurrentConfig.Northbound.Rest;
        if (!string.IsNullOrWhiteSpace(cli.Listen))
        {
            rest = new RestOptions
            {
                Enabled = rest.Enabled,
                Listen = cli.Listen,
                Token = rest.Token,
                StaticRoot = rest.StaticRoot,
            };
        }

        var app = builder.Build();

        app.UseNorthbound(runtime.CreateNorthboundContext(rest));

        if (hasWeb)
        {
            // WASM 产物里有 .dat / .blat / .wasm / .pdb 这些 ASP.NET Core 默认不认识的扩展名，
            // 缺 Content-Type 时静态文件中间件直接 404，浏览器端表现为「runtime 加载失败」。
            var contentTypes = new FileExtensionContentTypeProvider();
            contentTypes.Mappings[".dat"] = "application/octet-stream";
            contentTypes.Mappings[".blat"] = "application/octet-stream";
            contentTypes.Mappings[".wasm"] = "application/wasm";
            contentTypes.Mappings[".pdb"] = "application/octet-stream";
            contentTypes.Mappings[".br"] = "application/octet-stream";
            contentTypes.Mappings[".symbols"] = "application/octet-stream";

            app.UseDefaultFiles();
            app.UseStaticFiles(new StaticFileOptions
            {
                ContentTypeProvider = contentTypes,
                // 产物文件名带内容哈希，可以长时间缓存；index.html 由 UseDefaultFiles 返回，
                // 这里不设 OnPrepareResponse 一律缓存，避免旧 index.html 指向已删除的包目录。
                OnPrepareResponse = ctx =>
                {
                    if (!ctx.File.Name.Equals("index.html", StringComparison.OrdinalIgnoreCase))
                    {
                        ctx.Context.Response.Headers.CacheControl = "public, max-age=86400";
                    }
                },
            });
            app.MapFallbackToFile("index.html");
        }
        else
        {
            app.MapGet("/", () => Results.Text(
                $"uic {GatewayVersion} is running.\n" +
                $"UI assets not found under '{Path.GetFullPath(webRoot)}'.\n" +
                "Build the UI (dotnet publish src/Uic.Ui) or pass --web <dir>.\n" +
                "API: /api/v1/health\n",
                "text/plain; charset=utf-8"));
        }

        PrintBanner(log, listen, dataDir, runtime, hasWeb ? webRoot : null, simulate, cli.SimPort);

        try
        {
            await app.RunAsync().ConfigureAwait(false);
            return 0;
        }
        catch (OperationCanceledException)
        {
            return 0;
        }
        finally
        {
            await runtime.DisposeAsync().ConfigureAwait(false);
            if (simulator is not null) await simulator.DisposeAsync().ConfigureAwait(false);
        }
    }

    private static void PrintBanner(
        ILogger log,
        string listen,
        string dataDir,
        GatewayRuntime runtime,
        string? webRoot,
        bool simulate,
        int simPort)
    {
        var config = runtime.CurrentConfig;

        log.LogInformation("uic {Version} starting: site={Site} config={Config} data={DataDir}",
            GatewayVersion, config.Site, runtime.ConfigVersion, Path.GetFullPath(dataDir));

        log.LogInformation("listening on http://{Listen}  (UI: {Ui})",
            listen, webRoot ?? "not bundled");

        if (simulate)
        {
            log.LogInformation("simulation mode: embedded Modbus TCP slave on 127.0.0.1:{Port}", simPort);
        }

        if (string.IsNullOrWhiteSpace(config.Northbound.Rest.Token))
        {
            log.LogWarning(
                "northbound.rest.token is empty; the API accepts unauthenticated requests. " +
                "Set a token before exposing this port to a network.");
        }

        if (!config.Northbound.OpcUa.Enabled)
        {
            log.LogDebug("opc ua northbound is disabled");
        }
    }
}
