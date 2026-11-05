using System.Collections.Concurrent;
using Microsoft.Extensions.Logging;

namespace Uic.Obs;

/// <summary>一条结构化日志。字段刻意扁平：控制台与 API 都不需要再解析字符串。</summary>
public sealed record LogEntry(
    long Seq,
    DateTimeOffset At,
    string Level,
    string Category,
    string Message,
    string? Exception,
    string? DeviceId);

/// <summary>
/// 环形缓冲日志。
///
/// 边缘设备上「翻日志」是最常见的排障动作，但没人愿意为此部署一套日志系统。
/// 保留最近 N 条在内存里，API 直接可查，进程重启即清空——够用且不占磁盘。
/// </summary>
public sealed class RingBufferLogProvider : ILoggerProvider
{
    private readonly LogEntry[] _buffer;
    private readonly ConcurrentDictionary<string, ILogger> _loggers = new(StringComparer.Ordinal);
    private readonly object _lock = new();
    private long _seq;
    private int _head;
    private int _count;

    public RingBufferLogProvider(int capacity = 500)
    {
        _buffer = new LogEntry[Math.Max(16, capacity)];
    }

    public int Capacity => _buffer.Length;

    public ILogger CreateLogger(string categoryName)
        => _loggers.GetOrAdd(categoryName, name => new RingLogger(this, name));

    /// <summary>按时间倒序返回最近条目。筛选在锁内完成，保证快照一致。</summary>
    public IReadOnlyList<LogEntry> Snapshot(string? level = null, string? category = null, string? deviceId = null, int limit = 200)
    {
        var result = new List<LogEntry>(Math.Min(limit, _buffer.Length));

        lock (_lock)
        {
            var minLevel = ParseLevel(level);

            for (var i = 0; i < _count && result.Count < limit; i++)
            {
                var idx = (_head - 1 - i + _buffer.Length * 2) % _buffer.Length;
                var entry = _buffer[idx];
                if (entry is null) continue;

                if (minLevel is not null && Severity(entry.Level) < Severity(minLevel)) continue;
                if (category is not null && !entry.Category.Contains(category, StringComparison.OrdinalIgnoreCase)) continue;
                if (deviceId is not null && !string.Equals(entry.DeviceId, deviceId, StringComparison.Ordinal)) continue;

                result.Add(entry);
            }
        }

        return result;
    }

    internal void Add(LogEntry entry)
    {
        lock (_lock)
        {
            _buffer[_head] = entry;
            _head = (_head + 1) % _buffer.Length;
            if (_count < _buffer.Length) _count++;
        }
    }

    internal long NextSeq() => Interlocked.Increment(ref _seq);

    public void Dispose() => _loggers.Clear();

    private static string? ParseLevel(string? level) => level?.ToLowerInvariant() switch
    {
        null or "" => null,
        "trace" => "Trace",
        "debug" => "Debug",
        "info" or "information" => "Information",
        "warn" or "warning" => "Warning",
        "error" => "Error",
        "critical" or "fatal" => "Critical",
        _ => null,
    };

    private static int Severity(string level) => level switch
    {
        "Trace" => 0,
        "Debug" => 1,
        "Information" => 2,
        "Warning" => 3,
        "Error" => 4,
        "Critical" => 5,
        _ => 2,
    };

    private sealed class RingLogger : ILogger
    {
        private readonly RingBufferLogProvider _owner;
        private readonly string _category;

        public RingLogger(RingBufferLogProvider owner, string category)
        {
            _owner = owner;
            _category = category;
        }

        public IDisposable? BeginScope<TState>(TState state) where TState : notnull => null;

        public bool IsEnabled(LogLevel logLevel) => logLevel != LogLevel.None;

        public void Log<TState>(
            LogLevel logLevel,
            EventId eventId,
            TState state,
            Exception? exception,
            Func<TState, Exception?, string> formatter)
        {
            if (!IsEnabled(logLevel)) return;

            var message = formatter(state, exception);

            _owner.Add(new LogEntry(
                _owner.NextSeq(),
                DateTimeOffset.UtcNow,
                logLevel.ToString(),
                _category,
                message,
                exception?.ToString(),
                ExtractDeviceId(_category, message)));
        }

        /// <summary>从分类名或消息里提取设备 id，供 API 按设备过滤。</summary>
        private static string? ExtractDeviceId(string category, string message)
        {
            const string prefix = "uic.device.";
            if (category.StartsWith(prefix, StringComparison.Ordinal))
            {
                var rest = category[prefix.Length..];
                var dot = rest.IndexOf('.');
                return dot < 0 ? rest : rest[..dot];
            }

            return null;
        }
    }
}