namespace Uic.Host;

/// <summary>
/// 命令行约定。
///
/// 刻意不引入参数解析库：边缘部署常常只能拷一个可执行文件过去，
/// 少一个依赖就少一类「在我机器上能跑」。子命令格式固定为 <c>uic &lt;command&gt; [options]</c>。
/// </summary>
public sealed record CliOptions
{
    public string Command { get; init; } = "run";
    public string? ConfigPath { get; init; }
    public string? DataDir { get; init; }
    public string? Listen { get; init; }
    public string? WebRoot { get; init; }
    public int SimPort { get; init; } = 15020;
    public bool Verbose { get; init; }

    public const string Usage = """
        uic — 工业网关

        用法:
          uic run   [--config <file>] [--data <dir>] [--listen <host:port>] [--web <dir>] [-v]
          uic sim   [--port <modbus-port>] [--data <dir>] [--listen <host:port>] [-v]
          uic check [--config <file>]
          uic version

        选项:
          --config <file>   配置文件路径（默认读取数据目录里的当前版本）
          --data <dir>      数据目录（配置版本、上报队列、日志），默认 ./data
          --listen <addr>   覆盖 northbound.rest.listen
          --web <dir>       覆盖前端静态资源目录
          --port <n>        sim 模式内置 Modbus 从站端口，默认 15020
          -v, --verbose     以 Debug 级别输出日志
          -h, --help        显示本帮助

        退出码:
          0  成功
          2  配置非法
          3  配置无法读取
        """;

    public static CliOptions Parse(string[] args)
    {
        var command = "run";
        string? config = null;
        string? data = null;
        string? listen = null;
        string? web = null;
        var port = 15020;
        var verbose = false;

        for (var i = 0; i < args.Length; i++)
        {
            var arg = args[i];

            switch (arg)
            {
                case "-h" or "--help" or "help":
                    command = "help";
                    break;

                case "-check" or "check":
                    command = "check";
                    break;

                case "run" or "sim" or "version":
                    command = arg;
                    break;

                case "-v" or "--verbose":
                    verbose = true;
                    break;

                case "--config":
                    config = Next(args, ref i, arg);
                    break;

                case "--data":
                    data = Next(args, ref i, arg);
                    break;

                case "--listen":
                    listen = Next(args, ref i, arg);
                    break;

                case "--web":
                    web = Next(args, ref i, arg);
                    break;

                case "--port":
                    port = int.TryParse(Next(args, ref i, arg), out var p) ? p : port;
                    break;

                default:
                    if (arg.StartsWith("--config=", StringComparison.Ordinal)) config = arg["--config=".Length..];
                    else if (arg.StartsWith("--data=", StringComparison.Ordinal)) data = arg["--data=".Length..];
                    else if (arg.StartsWith("--listen=", StringComparison.Ordinal)) listen = arg["--listen=".Length..];
                    else if (arg.StartsWith("--web=", StringComparison.Ordinal)) web = arg["--web=".Length..];
                    else if (arg.StartsWith("--port=", StringComparison.Ordinal))
                        port = int.TryParse(arg["--port=".Length..], out var p2) ? p2 : port;

                    // 拼错的子命令或选项必须立刻失败。
                    // 静默忽略的后果是 `uic simm` 被当成 `uic run` 起了一个网关：
                    // 运维会以为自己启动的是仿真环境，实际连的却是真实设备。
                    else throw new ArgumentException($"unknown argument '{arg}'");
                    break;
            }
        }

        return new CliOptions
        {
            Command = command,
            ConfigPath = config,
            DataDir = data,
            Listen = listen,
            WebRoot = web,
            SimPort = port,
            Verbose = verbose,
        };
    }

    private static string Next(string[] args, ref int i, string name)
    {
        if (i + 1 >= args.Length)
            throw new ArgumentException($"{name} requires a value");

        return args[++i];
    }
}
