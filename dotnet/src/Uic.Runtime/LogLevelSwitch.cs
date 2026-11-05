using Microsoft.Extensions.Logging;

namespace Uic.Runtime;

/// <summary>
/// 运行期可切换的日志级别。
///
/// 现场排障的常态是「出问题了，把日志开到 Debug 看一眼，看完调回来」，
/// 而重启进程往往意味着丢掉当前故障现场。因此级别做成可写字段，
/// 由宿主的过滤器读取，而不是挂在 ILoggerFactory 上重建。
/// </summary>
public sealed class LogLevelSwitch
{
    private int _level = (int)LogLevel.Information;

    public LogLevel Level
    {
        get => (LogLevel)Volatile.Read(ref _level);
        set => Volatile.Write(ref _level, (int)value);
    }

    public bool IsEnabled(LogLevel level) => level >= Level && level != LogLevel.None;

    /// <summary>接受 trace/debug/info/warn/error/critical 与常见别名；非法值返回 false。</summary>
    public bool TrySet(string text)
    {
        var parsed = text.Trim().ToLowerInvariant() switch
        {
            "trace" => LogLevel.Trace,
            "debug" => LogLevel.Debug,
            "info" or "information" => LogLevel.Information,
            "warn" or "warning" => LogLevel.Warning,
            "error" => LogLevel.Error,
            "critical" or "fatal" => LogLevel.Critical,
            "none" or "off" => LogLevel.None,
            _ => (LogLevel?)null,
        };

        if (parsed is null) return false;

        Level = parsed.Value;
        return true;
    }
}
