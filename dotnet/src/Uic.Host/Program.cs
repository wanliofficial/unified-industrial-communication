using System.Security.Cryptography.X509Certificates;
using Microsoft.AspNetCore.StaticFiles;
using Microsoft.Extensions.Logging;
using Uic.Config;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Driver.Virtual;
using Uic.Northbound;
using Uic.Northbound.Security;
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

            case "secret":
                return Secret(cli);

            case "user":
                return User(cli);

            case "sim":
                return await RunAsync(cli, simulate: true).ConfigureAwait(false);

            default:
                return await RunAsync(cli, simulate: false).ConfigureAwait(false);
        }
    }

    // ------------------------------------------------------------------
    // 凭据工具
    // ------------------------------------------------------------------

    /// <summary>
    /// <c>uic secret</c>：生成密封密钥、把明文封成 <c>enc:v1:</c> 密文。
    ///
    /// 刻意不提供「打印明文」的反向命令：能打印一次，就会有人把它贴进工单与聊天记录。
    /// 需要验证密文能否解开时用 <c>uic check</c>。
    /// </summary>
    private static int Secret(CliOptions cli)
    {
        switch (cli.SecretAction)
        {
            case "keygen":
                return SecretKeygen(cli);

            case "seal":
                return SecretSeal(cli);

            default:
                Console.Error.WriteLine("uic secret needs an action: keygen | seal");
                Console.Error.WriteLine();
                Console.Error.WriteLine(CliOptions.Usage);
                return 64;
        }
    }

    /// <summary>
    /// <c>uic user</c>：账号的离线兜底。REST 需要有人能登录，控制台需要有人能进去；
    /// 忘记管理员口令时，这台机器上的操作员必须还能恢复访问。
    ///
    /// 重置口令走与 REST 相同的存储与派生实现，不另写一份「命令行专用格式」——
    /// 两份实现迟早会分叉，然后在最需要登录的时候互相不认。
    /// </summary>
    private static int User(CliOptions cli)
    {
        var dataDir = cli.DataDir ?? "./data";

        UserStore store;
        try
        {
            store = new UserStore(dataDir, message => Console.Error.WriteLine("warning: " + message));
        }
        catch (ConfigException ex)
        {
            Console.Error.WriteLine($"cannot open user store under '{dataDir}':");
            Console.Error.WriteLine(ex.ToDetailedString());
            return 3;
        }

        switch (cli.UserAction)
        {
            case "list":
                Console.WriteLine($"{"id",-20} {"role",-10} {"enabled",-8} {"must-change",-12} last-login");
                foreach (var user in store.All().OrderBy(u => u.Id, StringComparer.Ordinal))
                {
                    Console.WriteLine(
                        $"{user.Id,-20} {user.Role,-10} {(user.Enabled ? "yes" : "no"),-8} " +
                        $"{(user.MustChangePassword ? "yes" : "no"),-12} " +
                        $"{user.LastLoginAt?.ToString("u") ?? "-"}");
                }

                return 0;

            case "reset-password":
            {
                if (string.IsNullOrWhiteSpace(cli.User))
                {
                    Console.Error.WriteLine("uic user reset-password requires --user <id>");
                    return 64;
                }

                var user = store.Find(cli.User);
                if (user is null)
                {
                    Console.Error.WriteLine($"user '{cli.User}' not found");
                    return 3;
                }

                var password = UserStore.GeneratePassword();
                store.SetPassword(user.Id, password, mustChange: true);
                store.ClearInitialPasswordFile();

                // 口令走 stdout，提示走 stderr：方便脚本 `PASSWORD=$(uic user reset-password ...)`。
                Console.WriteLine(password);
                Console.Error.WriteLine(
                    $"temporary password for '{user.Id}' (must be changed at next login)");
                return 0;
            }

            default:
                Console.Error.WriteLine("uic user needs an action: list | reset-password");
                Console.Error.WriteLine();
                Console.Error.WriteLine(CliOptions.Usage);
                return 64;
        }
    }

    private static int SecretKeygen(CliOptions cli)
    {
        var text = Secrets.EncodeKey(Secrets.GenerateKey());

        if (cli.OutPath is null)
        {
            Console.WriteLine(text);
            Console.Error.WriteLine(
                $"save this to a file only the owner can read, then export {Secrets.KeyFileVariable}=<file>");
            return 0;
        }

        var full = Path.GetFullPath(cli.OutPath);
        var dir = Path.GetDirectoryName(full);
        if (!string.IsNullOrEmpty(dir)) Directory.CreateDirectory(dir);

        File.WriteAllText(full, text + "\n");
        RestrictPermissions(full);

        Console.WriteLine($"key written to {full}");
        Console.WriteLine($"export {Secrets.KeyFileVariable}={full}");
        return 0;
    }

    private static int SecretSeal(CliOptions cli)
    {
        var lookup = cli.KeyFile is null
            ? SecretLookup.Default
            : new SecretLookup
            {
                KeyText = () => File.Exists(cli.KeyFile) ? File.ReadAllText(cli.KeyFile) : null,
            };

        if (!Secrets.TryParseKey(lookup.KeyText(), out var key, out var keyError))
        {
            Console.Error.WriteLine(keyError);
            return 2;
        }

        var plaintext = Console.IsInputRedirected ? Console.In.ReadToEnd() : ReadSecretFromConsole();

        // 行尾换行是 shell 与编辑器的副产物，不属于口令本身。
        plaintext = plaintext.TrimEnd('\r', '\n');

        if (plaintext.Length == 0)
        {
            Console.Error.WriteLine("no secret on stdin; pipe the value in or type it");
            return 64;
        }

        Console.WriteLine(Secrets.Seal(plaintext, key));
        return 0;
    }

    /// <summary>交互式输入不回显：口令出现在终端回滚缓冲区里同样是泄露。</summary>
    private static string ReadSecretFromConsole()
    {
        Console.Error.Write("secret value: ");
        var buffer = new System.Text.StringBuilder();

        while (true)
        {
            var key = Console.ReadKey(intercept: true);

            if (key.Key == ConsoleKey.Enter) break;
            if (key.Key == ConsoleKey.Backspace)
            {
                if (buffer.Length > 0) buffer.Length--;
                continue;
            }

            if (!char.IsControl(key.KeyChar)) buffer.Append(key.KeyChar);
        }

        Console.Error.WriteLine();
        return buffer.ToString();
    }

    private static void RestrictPermissions(string path)
    {
        if (OperatingSystem.IsWindows()) return;

        const UnixFileMode ownerOnly = UnixFileMode.UserRead | UnixFileMode.UserWrite;

        try
        {
            File.SetUnixFileMode(path, ownerOnly);

            // 有些文件系统（exFAT、部分 FUSE/NTFS 挂载）根本不记录权限位。
            // 这时静默成功等于谎报：密钥文件实际对所有用户可读。
            if (File.GetUnixFileMode(path) != ownerOnly)
            {
                Console.Error.WriteLine(
                    $"warning: {path} does not keep unix permissions (filesystem ignores them); " +
                    "move the key to a filesystem that does, or mount it as a container secret");
            }
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or PlatformNotSupportedException)
        {
            Console.Error.WriteLine($"warning: could not restrict permissions on {path}: {ex.Message}");
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

            // 凭据引用要真的解析得出来。语法、环境变量、密钥文件任一不对，
            // 这里就失败——部署前的静态检查必须比启动更早发现问题。
            var secretProblems = Secrets.Validate(doc);
            if (secretProblems.Count > 0)
            {
                Console.Error.WriteLine($"invalid: {path}");
                Console.Error.WriteLine($"  credentials are not usable ({secretProblems.Count} problem(s))");
                foreach (var problem in secretProblems)
                    Console.Error.WriteLine($"    {problem}");

                return 2;
            }

            Console.WriteLine($"ok: {path}");
            Console.WriteLine($"  site: {doc.Site}");
            Console.WriteLine($"  channels: {doc.Channels.Count}, devices: {doc.Devices.Count}, " +
                              $"points: {doc.Devices.Sum(d => d.Points.Count)}");

            foreach (var line in Secrets.Describe(doc))
                Console.WriteLine($"  secret {line}");

            var northbound = new List<string>();
            // 协议前缀写出来：check 的输出是部署评审的依据，
            // 「监听 8443」和「监听 8443 且真的是 HTTPS」是两件事。
            if (doc.Northbound.Rest.Enabled)
            {
                var scheme = doc.Northbound.Rest.Tls.Enabled ? "https" : "http";
                northbound.Add($"rest({scheme}://{doc.Northbound.Rest.Listen})");
            }

            if (doc.Northbound.Mqtt.Enabled)
            {
                var transport = doc.Northbound.Mqtt.Tls.Enabled ? "mqtts" : "mqtt";
                northbound.Add($"{transport}({doc.Northbound.Mqtt.Host}:{doc.Northbound.Mqtt.Port})");
            }

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
        var fileLog = new JsonLineFileLogProvider();

        using var loggerFactory = LoggerFactory.Create(builder =>
        {
            builder.SetMinimumLevel(LogLevel.Trace);
            builder.AddProvider(ring);
            builder.AddProvider(fileLog);
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

        // 文件日志的开关来自配置，而配置要读出来才知道：先配置、后写文件，
        // 启动阶段之前的日志只在控制台与环形缓冲里，不会因为「还没配好」而丢一条。
        fileLog.Configure(bootstrap.Logging.File, dataDir);
        if (bootstrap.Logging.File.Enabled)
        {
            log.LogInformation("structured file logging: {Path} (max {MaxBytes} bytes, retain {Retain})",
                fileLog.ActivePath, bootstrap.Logging.File.MaxBytes, bootstrap.Logging.File.Retain);
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
        builder.Logging.AddProvider(fileLog);
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

        // 监听地址要等运行时读出配置（并应用 --listen 覆盖）后才定，
        // 因此 UseUrls 与证书都放到后面，这里只做准备。

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
                    FileLog = fileLog,
                },
                drivers,
                loggerFactory,
                ring).ConfigureAwait(false);
        }
        catch (ConfigException ex)
        {
            // 配置类失败是「运维改一下就能好」的错误，不该以 1（未预期异常）退出：
            // 部署脚本靠退出码区分「配置非法」与「进程崩溃」。
            Console.Error.WriteLine("invalid configuration:");
            Console.Error.WriteLine(ex.ToDetailedString());
            if (simulator is not null) await simulator.DisposeAsync().ConfigureAwait(false);
            return 2;
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
            // RestOptions 是 record：克隆一份只改监听地址，新增字段不会被漏拷。
            rest = rest with { Listen = cli.Listen };
        }

        // TLS 证书必须在 Build 之前交给 Kestrel：它属于监听配置，不是请求管道。
        var scheme = "http";
        if (rest.Tls.Enabled)
        {
            X509Certificate2 certificate;
            try
            {
                certificate = Tls.LoadServerCertificate(rest, dataDir, runtime.SecretValues);
            }
            catch (ConfigException ex)
            {
                Console.Error.WriteLine("invalid TLS configuration:");
                Console.Error.WriteLine(ex.ToDetailedString());
                return 2;
            }

            builder.WebHost.ConfigureKestrel(options =>
                options.ConfigureHttpsDefaults(https => https.ServerCertificate = certificate));
            scheme = "https";
        }

        builder.WebHost.UseUrls(listen.StartsWith("http", StringComparison.OrdinalIgnoreCase)
            ? listen
            : $"{scheme}://{listen}");

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

        PrintBanner(log, scheme, listen, dataDir, runtime, hasWeb ? webRoot : null, simulate, cli.SimPort);

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
        string scheme,
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

        log.LogInformation("listening on {Scheme}://{Listen}  (UI: {Ui})",
            scheme, listen, webRoot ?? "not bundled");

        if (simulate)
        {
            log.LogInformation("simulation mode: embedded Modbus TCP slave on 127.0.0.1:{Port}", simPort);
        }

        // 首次启动会生成 admin 与一个随机初始口令。口令本身不进日志（日志会被收走），
        // 只提示文件位置；用户改密成功后文件会被删除。
        var initial = runtime.Auth.Users.InitialPasswordPath;
        if (runtime.Auth.Users.CreatedInitialAdmin)
        {
            log.LogWarning(
                "first start: created user 'admin' with a random password; read it from {Path} " +
                "and change it after the first login (the file is removed once changed)",
                initial);
        }
        else if (initial is not null && File.Exists(initial))
        {
            log.LogWarning(
                "the initial admin password has not been changed yet; read it from {Path}", initial);
        }

        if (string.IsNullOrWhiteSpace(config.Northbound.Rest.Token))
        {
            log.LogInformation(
                "no static API token configured; scripts must log in with an account " +
                "(local accounts are always required for the API)");
        }

        if (!config.Northbound.OpcUa.Enabled)
        {
            log.LogDebug("opc ua northbound is disabled");
        }
    }
}
