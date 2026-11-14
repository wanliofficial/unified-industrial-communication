using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Uic.Domain;

namespace Uic.Obs;

/// <summary>一条审计记录。字段刻意扁平：<c>jq 'select(.action=="config.apply")'</c> 就能筛。</summary>
public sealed record AuditEntry(
    long Seq,
    DateTimeOffset At,
    string Action,
    string Result,
    string Actor,
    string Source,
    string? Remote,
    string? RequestId,
    string? Target,
    JsonNode? Detail);

/// <summary>审计查询条件。<c>From</c>/<c>To</c> 都是闭区间，时间按 UTC 比较。</summary>
public sealed record AuditQuery(
    string? Action,
    string? Actor,
    string? Result,
    string? Target,
    DateTimeOffset From,
    DateTimeOffset To,
    int Limit,
    int Offset = 0);

public sealed record AuditPage(IReadOnlyList<AuditEntry> Items, int Total);

/// <summary>
/// 审计日志：追加写的 NDJSON 文件，按天 + 按大小切分，按数量保留。
///
/// 为什么不用环形日志：环形日志重启即清空，恰好把「谁改的配置」留在了最容易
/// 丢的地方。审计是事后追责的唯一依据，必须落盘，且写入后立即 flush 到磁盘。
///
/// 为什么写失败不阻断业务：审计写不进去（磁盘满、权限错）时，让一次点位写入
/// 或一次配置生效失败，等于用一个观测问题制造一次生产事故。这里保留一条
/// 告警与指标，业务继续；「审计不可用」本身要能被看见，而不是被伪装成成功。
/// </summary>
public sealed class AuditLog : IDisposable
{
    private const string Prefix = "audit-";
    private const string Suffix = ".ndjson";

    private readonly object _gate = new();
    private readonly string _dataDir;

    private AuditOptions _options;
    private string _directory;
    private FileStream? _stream;
    private string? _path;
    private DateOnly _day;
    private long _bytes;
    private long _seq;
    private int _failures;
    private int _warned;
    private int _disposed;

    public AuditLog(string dataDir, AuditOptions? options = null)
    {
        _dataDir = dataDir;
        _options = options ?? new AuditOptions();
        _directory = Resolve(_options.Directory);

        if (_options.Enabled)
        {
            // 目录建不出来（权限、只读挂载）不能把整个进程拦在启动线上：
            // 采集与北向还有救，审计先降级，指标与告警会把它暴露出来。
            try
            {
                Directory.CreateDirectory(_directory);
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                WarnOnce($"audit log degraded: cannot create '{_directory}': {ex.Message}");
            }
        }

        _seq = ReadLastSeq();
    }

    /// <summary>当前文件；未写入过或已禁用时为 null。诊断包与用例用得到。</summary>
    public string? ActivePath
    {
        get { lock (_gate) return _path; }
    }

    public bool Enabled => _options.Enabled;

    /// <summary>本进程写出的记录条数（含禁用时未落盘的）。</summary>
    public long Written => Interlocked.Read(ref _seq);

    /// <summary>写入失败次数。0 表示审计链路健康，指标把它暴露成 <c>uic_audit_write_failures</c>。</summary>
    public int WriteFailures => Volatile.Read(ref _failures);

    /// <summary>热更新审计设置。目录或上限变化时关掉当前文件，下次写入按新设置重开。</summary>
    public void Configure(AuditOptions options)
    {
        lock (_gate)
        {
            if (_disposed != 0) return;

            var directory = Resolve(options.Directory);
            var changed = !string.Equals(directory, _directory, StringComparison.Ordinal) ||
                          options.MaxBytes != _options.MaxBytes ||
                          options.Enabled != _options.Enabled;

            _options = options;
            _directory = directory;

            if (!changed) return;

            CloseLocked();

            if (options.Enabled)
            {
                try
                {
                    Directory.CreateDirectory(_directory);
                }
                catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                {
                    WarnOnce($"audit log degraded: cannot create '{_directory}': {ex.Message}");
                }
            }
        }
    }

    /// <summary>
    /// 追加一条。返回值便于调用方在日志里带上 seq，但调用方不需要处理失败：
    /// 审计写不进去不能改变业务结果。
    /// </summary>
    public AuditEntry Write(
        string action,
        string result,
        string actor,
        string source,
        string? target = null,
        string? remote = null,
        string? requestId = null,
        object? detail = null)
    {
        var entry = new AuditEntry(
            Interlocked.Increment(ref _seq),
            DateTimeOffset.UtcNow,
            action,
            result,
            actor,
            source,
            remote,
            requestId,
            target,
            detail is null ? null : JsonSerializer.SerializeToNode(detail, JsonDefaults.Options));

        if (!_options.Enabled) return entry;

        Append(entry);
        return entry;
    }

    private void Append(AuditEntry entry)
    {
        var line = JsonSerializer.Serialize(entry, JsonDefaults.Options);
        var bytes = Encoding.UTF8.GetBytes(line + "\n");

        lock (_gate)
        {
            if (_disposed != 0) return;

            var now = DateTimeOffset.UtcNow;
            if (!EnsureOpenLocked(now)) return;

            // 先判断再写：只有在「这一行会超限」时才换文件，不产生一个超限文件。
            if (_bytes > 0 && _bytes + bytes.Length > _options.MaxBytes)
            {
                CloseLocked();
                if (!EnsureOpenLocked(now)) return;
            }

            try
            {
                _stream!.Write(bytes, 0, bytes.Length);
                // 审计要能经受突然断电：逐条 flush 到磁盘。写入频率本来就低
                // （配置变更、命令、点位写入），这点开销远小于一次无法追溯的事故。
                _stream.Flush(flushToDisk: true);
                _bytes += bytes.Length;
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                Interlocked.Increment(ref _failures);
                WarnOnce($"audit log write failed: {ex.Message}; auditing is degraded");
                CloseLocked();
            }
        }
    }

    /// <summary>磁盘满时每条都打一行会把真正的原因埋掉，所以只告警一次。</summary>
    private void WarnOnce(string message)
    {
        if (Interlocked.Exchange(ref _warned, 1) == 0)
            Console.Error.WriteLine(message);
    }

    /// <summary>按条件查询，最新在前。游标是偏移量（见 <see cref="EncodeCursor"/>）。</summary>
    /// <remarks>
    /// 这里**不能**说「追加写让游标稳定」：那是升序、只追加的历史查询才成立的性质。
    /// 审计按最新在前排序，翻页期间每落一条新记录，整列向后移一位，用偏移量连续翻会
    /// 重复/漏看边界那条。要完整导出请走一次性 <c>export</c>，而不是翻页。
    /// </remarks>
    public async Task<AuditPage> QueryAsync(AuditQuery query, CancellationToken ct = default)
    {
        var matches = new List<AuditEntry>();

        foreach (var file in FilesNewestFirst())
        {
            ct.ThrowIfCancellationRequested();
            if (!MayContain(file, query.From, query.To)) continue;

            await foreach (var entry in ReadAsync(file, ct).ConfigureAwait(false))
            {
                if (Matches(entry, query)) matches.Add(entry);
            }
        }

        // 文件名只到秒，跨文件的同一秒要靠 At + Seq 兜底。
        matches.Sort(static (a, b) =>
        {
            var byTime = b.At.CompareTo(a.At);
            return byTime != 0 ? byTime : b.Seq.CompareTo(a.Seq);
        });

        var page = matches.Skip(query.Offset).Take(query.Limit).ToArray();
        return new AuditPage(page, matches.Count);
    }

    /// <summary>按同样条件导出成 NDJSON（一行一条，可直接 <c>jq</c> / <c>grep</c>）。</summary>
    public static string Serialize(AuditEntry entry) => JsonSerializer.Serialize(entry, JsonDefaults.Options);

    public IReadOnlyList<string> Files()
        => Directory.Exists(_directory)
            ? Directory.GetFiles(_directory, Prefix + "*" + Suffix).OrderBy(f => f, StringComparer.Ordinal).ToArray()
            : Array.Empty<string>();

    private IEnumerable<string> FilesNewestFirst()
        => Files().Reverse();

    private static async IAsyncEnumerable<AuditEntry> ReadAsync(
        string path,
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken ct = default)
    {
        // 进程被杀时最后一行可能是半条：解析失败就跳过，不能让一条坏行毁掉整个查询。
        await foreach (var line in File.ReadLinesAsync(path, ct).ConfigureAwait(false))
        {
            if (line.Length == 0) continue;

            AuditEntry? entry;
            try
            {
                entry = JsonSerializer.Deserialize<AuditEntry>(line, JsonDefaults.Options);
            }
            catch (JsonException)
            {
                continue;
            }

            if (entry is not null) yield return entry;
        }
    }

    private static bool Matches(AuditEntry entry, AuditQuery query)
    {
        if (entry.At < query.From || entry.At > query.To) return false;
        if (query.Action is not null && !string.Equals(entry.Action, query.Action, StringComparison.OrdinalIgnoreCase))
            return false;
        if (query.Actor is not null && !string.Equals(entry.Actor, query.Actor, StringComparison.OrdinalIgnoreCase))
            return false;
        if (query.Result is not null && !string.Equals(entry.Result, query.Result, StringComparison.OrdinalIgnoreCase))
            return false;
        if (query.Target is not null && !string.Equals(entry.Target, query.Target, StringComparison.OrdinalIgnoreCase))
            return false;

        return true;
    }

    /// <summary>文件名里的日期早于/晚于查询范围时整份跳过，避免为了查一小时把全部归档都读一遍。</summary>
    private static bool MayContain(string path, DateTimeOffset from, DateTimeOffset to)
    {
        var name = Path.GetFileNameWithoutExtension(path);
        if (name.Length < Prefix.Length + 8) return true;

        if (!DateOnly.TryParseExact(name.Substring(Prefix.Length, 8), "yyyyMMdd",
                CultureInfo.InvariantCulture, DateTimeStyles.None, out var day))
        {
            return true;
        }

        return day >= DateOnly.FromDateTime(from.UtcDateTime) &&
               day <= DateOnly.FromDateTime(to.UtcDateTime);
    }

    private bool EnsureOpenLocked(DateTimeOffset now)
    {
        var today = DateOnly.FromDateTime(now.UtcDateTime);
        if (_stream is not null && _day == today) return true;

        CloseLocked();

        try
        {
            Directory.CreateDirectory(_directory);

            var path = UniquePathLocked(now);
            _stream = new FileStream(
                path,
                FileMode.Append,
                FileAccess.Write,
                FileShare.Read | FileShare.Delete,
                bufferSize: 4096,
                useAsync: false);

            _path = path;
            _day = today;
            _bytes = _stream.Length;

            // 新文件的路径已经加入列表，此时按「含当前文件」的语义裁剪才不会多留一个。
            PruneLocked();
            return true;
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            Interlocked.Increment(ref _failures);
            WarnOnce($"audit log degraded: cannot open '{_directory}': {ex.Message}");

            CloseLocked();
            return false;
        }
    }

    private string UniquePathLocked(DateTimeOffset now)
    {
        var stamp = now.UtcDateTime.ToString("yyyyMMdd-HHmmss-fff", CultureInfo.InvariantCulture);
        var path = Path.Combine(_directory, $"{Prefix}{stamp}{Suffix}");

        var suffix = 1;
        while (File.Exists(path))
            path = Path.Combine(_directory, $"{Prefix}{stamp}-{suffix++}{Suffix}");

        return path;
    }

    private void CloseLocked()
    {
        try { _stream?.Dispose(); }
        catch (IOException) { /* 关闭失败没有补救动作 */ }

        _stream = null;
        _path = null;
        _bytes = 0;
    }

    /// <summary>只保留最近的 <see cref="AuditOptions.Retain"/> 个文件；按文件名（时间）排序。</summary>
    private void PruneLocked()
    {
        var keep = Math.Max(1, _options.Retain);
        var files = Files().ToList();

        while (files.Count > keep)
        {
            var victim = files.FirstOrDefault(f =>
                !string.Equals(f, _path, StringComparison.Ordinal));

            // 只有当前文件时宁可多占一个，也不能删掉正在写的文件。
            if (victim is null) return;

            try
            {
                File.Delete(victim);
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                // 删不掉就留着：占空间总比丢掉最新审计更可接受。
                _ = ex;
                return;
            }

            files.Remove(victim);
        }
    }

    /// <summary>
    /// 从最新的文件末尾恢复序号，让 seq 在重启后仍然单调。
    /// 只读最后一个文件的一段尾部：审计常见的量级不需要全量扫描。
    /// </summary>
    private long ReadLastSeq()
    {
        var newest = Files().LastOrDefault();
        if (newest is null) return 0;

        try
        {
            using var stream = new FileStream(newest, FileMode.Open, FileAccess.Read, FileShare.ReadWrite);
            const int window = 64 * 1024;
            var start = Math.Max(0, stream.Length - window);

            stream.Seek(start, SeekOrigin.Begin);
            using var reader = new StreamReader(stream, Encoding.UTF8);
            var text = reader.ReadToEnd();

            var lines = text.Split('\n', StringSplitOptions.RemoveEmptyEntries);
            for (var i = lines.Length - 1; i >= 0; i--)
            {
                try
                {
                    var entry = JsonSerializer.Deserialize<AuditEntry>(lines[i], JsonDefaults.Options);
                    if (entry is not null) return entry.Seq;
                }
                catch (JsonException)
                {
                    // 尾部半行：继续往前找。
                }
            }
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            // 读不到就从头计数：seq 只是排序兜底，不影响可读性。
        }

        return 0;
    }

    private string Resolve(string directory)
        => Path.IsPathRooted(directory)
            ? Path.GetFullPath(directory)
            : Path.GetFullPath(Path.Combine(_dataDir, directory));

    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        lock (_gate)
        {
            CloseLocked();
        }
    }
}
