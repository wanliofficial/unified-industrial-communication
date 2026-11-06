using System.Buffers;
using System.Collections.Concurrent;
using System.Text.Json;
using Microsoft.Extensions.Logging;
using Uic.Domain;

namespace Uic.Obs;

/// <summary>
/// JSON 行文件日志 + 按大小轮转。
///
/// 为什么不是普通文本：现场排障时要按设备、级别过滤，直接 <c>jq</c> 一行一个对象
/// 比正则切多行异常可靠得多。异常堆栈会被压进一个字段里的 <c>\n</c>，
/// 不会把「一条日志」拆成十几行。
///
/// 为什么默认关闭：边缘设备磁盘通常很小，没人显式配置时落盘只会变成磁盘告警。
/// 内存环形日志仍然照常工作，文件日志是给需要留档的现场额外打开的。
/// </summary>
public sealed class JsonLineFileLogProvider : ILoggerProvider
{
    private static readonly JsonWriterOptions WriterOptions = new()
    {
        Indented = false,
        SkipValidation = true,
    };

    private readonly ConcurrentDictionary<string, ILogger> _loggers = new(StringComparer.Ordinal);
    private readonly object _gate = new();

    private FileLogOptions _options = new();
    private string _directory = string.Empty;
    private string? _activePath;
    private FileStream? _stream;
    private long _bytes;
    private int _writeFailed;
    private int _disposed;

    /// <summary>当前活动文件；未启用或尚未配置时为 null。主要给用例和诊断包使用。</summary>
    public string? ActivePath
    {
        get { lock (_gate) return _stream is null ? null : _activePath; }
    }

    /// <summary>
    /// 应用配置。可以在运行期反复调用：路径/大小没变就保留当前文件，
    /// 变了才关闭旧文件并打开新文件；<c>enabled=false</c> 立即停止落盘。
    /// </summary>
    public void Configure(FileLogOptions options, string dataDir)
    {
        var directory = Path.IsPathRooted(options.Directory)
            ? options.Directory
            : Path.Combine(dataDir, options.Directory);

        // 配置里可能是 "logs/../logs"：先规范化，避免同一目录被当成两个不同目标。
        try { directory = Path.GetFullPath(directory); }
        catch (Exception) { directory = Path.Combine(dataDir, options.Directory); }

        lock (_gate)
        {
            if (_disposed != 0) return;

            var sameTarget = _stream is not null &&
                             string.Equals(directory, _directory, StringComparison.Ordinal) &&
                             string.Equals(options.FileName, _options.FileName, StringComparison.Ordinal) &&
                             options.MaxBytes == _options.MaxBytes;

            _options = options;
            _directory = directory;

            if (!options.Enabled)
            {
                CloseLocked();
                return;
            }

            if (sameTarget) return;

            CloseLocked();

            try
            {
                Directory.CreateDirectory(directory);
                var path = Path.Combine(directory, options.FileName);
                OpenLocked(path);
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                // 打不开文件不能拖垮网关：配置错误在启动日志里说明一次即可。
                CloseLocked();
                Console.Error.WriteLine(
                    $"file logging disabled: cannot open '{Path.Combine(directory, options.FileName)}': {ex.Message}");
            }
        }
    }

    public ILogger CreateLogger(string categoryName)
        => _loggers.GetOrAdd(categoryName, name => new FileLogger(this, name));

    internal bool IsEnabled(LogLevel level)
    {
        if (level == LogLevel.None) return false;

        var minimum = ParseLevel(_options.MinimumLevel) ?? LogLevel.Trace;
        return level >= minimum;
    }

    internal void Write(LogLevel level, string category, string message, Exception? exception)
    {
        if (Volatile.Read(ref _disposed) != 0) return;

        var line = Serialize(DateTimeOffset.UtcNow, level, category, message, exception);

        lock (_gate)
        {
            if (_stream is null || !IsEnabled(level)) return;

            // 先判断再写：轮转只发生在新行会超限时，文件不会先超限再切。
            if (_bytes > 0 && _bytes + line.Length + 1 > _options.MaxBytes)
            {
                try { RotateLocked(); }
                catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                {
                    WarnOnce($"file logging rotation failed: {ex.Message}");
                    return;
                }
            }

            try
            {
                _stream.Write(line, 0, line.Length);
                _stream.WriteByte((byte)'\n');
                // 逐行 flush：现场最常见的动作是 tail 日志，缓冲区攒着会让人误判「没日志」。
                _stream.Flush();
                _bytes += line.Length + 1;
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                WarnOnce($"file logging write failed: {ex.Message}");
                CloseLocked();
            }
        }
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        lock (_gate)
        {
            CloseLocked();
        }

        _loggers.Clear();
    }

    private void OpenLocked(string path)
    {
        var stream = new FileStream(
            path,
            FileMode.Append,
            FileAccess.Write,
            FileShare.Read | FileShare.Delete,
            bufferSize: 4096,
            useAsync: false);

        _stream = stream;
        _activePath = path;
        _bytes = stream.Length;

        // 上次退出时文件已经写满：启动即先归档，避免第一行就把文件顶过上限。
        if (_bytes >= _options.MaxBytes)
        {
            try { RotateLocked(); }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                WarnOnce($"file logging rotation on open failed: {ex.Message}");
            }
        }
    }

    private void CloseLocked()
    {
        try { _stream?.Dispose(); }
        catch (IOException) { /* 关闭失败没有补救动作 */ }

        _stream = null;
        _activePath = null;
        _bytes = 0;
    }

    private void RotateLocked()
    {
        var active = _activePath;
        if (active is null) return;

        var directory = Path.GetDirectoryName(active) ?? _directory;
        var name = Path.GetFileNameWithoutExtension(active);
        var extension = Path.GetExtension(active);

        CloseLocked();

        var stamp = DateTimeOffset.UtcNow.ToString("yyyyMMdd-HHmmss-fff");
        var archive = Path.Combine(directory, $"{name}-{stamp}{extension}");

        // 同一毫秒内连续轮转（用例或极端流量）时追加序号，不能覆盖已有归档。
        var suffix = 1;
        while (File.Exists(archive))
        {
            archive = Path.Combine(directory, $"{name}-{stamp}-{suffix++}{extension}");
        }

        File.Move(active, archive);
        PruneLocked(directory, name, extension);
        OpenLocked(active);
    }

    /// <summary>只保留最近 <see cref="FileLogOptions.Retain"/> 份归档；当前文件不参与计数。</summary>
    private void PruneLocked(string directory, string name, string extension)
    {
        var keep = Math.Max(1, _options.Retain);
        var prefix = name + "-";

        var archives = Directory
            .EnumerateFiles(directory)
            .Where(p =>
            {
                var file = Path.GetFileName(p);
                return file.StartsWith(prefix, StringComparison.Ordinal) &&
                       file.EndsWith(extension, StringComparison.OrdinalIgnoreCase);
            })
            .OrderByDescending(File.GetLastWriteTimeUtc)
            .ToArray();

        foreach (var stale in archives.Skip(keep))
        {
            try { File.Delete(stale); }
            catch (IOException) { /* 被 tail 占用时下次再删 */ }
            catch (UnauthorizedAccessException) { }
        }
    }

    private static byte[] Serialize(
        DateTimeOffset at,
        LogLevel level,
        string category,
        string message,
        Exception? exception)
    {
        var buffer = new ArrayBufferWriter<byte>(512);
        using var writer = new Utf8JsonWriter(buffer, WriterOptions);

        writer.WriteStartObject();
        writer.WriteString("ts", at);
        writer.WriteString("level", level.ToString());
        writer.WriteString("category", category);
        writer.WriteString("msg", message);

        var device = LogFields.DeviceId(category);
        if (device is not null) writer.WriteString("device", device);

        if (exception is not null) writer.WriteString("exception", exception.ToString());
        writer.WriteEndObject();
        writer.Flush();

        return buffer.WrittenSpan.ToArray();
    }

    private void WarnOnce(string message)
    {
        if (Interlocked.Exchange(ref _writeFailed, 1) == 0)
        {
            Console.Error.WriteLine(message);
        }
    }

    private static LogLevel? ParseLevel(string? level) => level?.ToLowerInvariant() switch
    {
        "trace" => LogLevel.Trace,
        "debug" => LogLevel.Debug,
        "information" or "info" => LogLevel.Information,
        "warning" or "warn" => LogLevel.Warning,
        "error" => LogLevel.Error,
        "critical" => LogLevel.Critical,
        _ => null,
    };

    private sealed class FileLogger : ILogger
    {
        private readonly JsonLineFileLogProvider _owner;
        private readonly string _category;

        public FileLogger(JsonLineFileLogProvider owner, string category)
        {
            _owner = owner;
            _category = category;
        }

        public IDisposable? BeginScope<TState>(TState state) where TState : notnull => null;

        public bool IsEnabled(LogLevel logLevel) => _owner.IsEnabled(logLevel);

        public void Log<TState>(
            LogLevel logLevel,
            EventId eventId,
            TState state,
            Exception? exception,
            Func<TState, Exception?, string> formatter)
        {
            if (!IsEnabled(logLevel)) return;

            _owner.Write(logLevel, _category, formatter(state, exception), exception);
        }
    }
}

/// <summary>日志字段口径。环形缓冲与文件日志共用，避免两处对设备 id 的提取规则不一致。</summary>
internal static class LogFields
{
    private const string DevicePrefix = "uic.device.";

    /// <summary>分类名形如 <c>uic.device.meter-1</c> 时取设备 id；其余返回 null。</summary>
    public static string? DeviceId(string category)
    {
        if (!category.StartsWith(DevicePrefix, StringComparison.Ordinal)) return null;

        var rest = category[DevicePrefix.Length..];
        var dot = rest.IndexOf('.');
        return dot < 0 ? rest : rest[..dot];
    }
}
