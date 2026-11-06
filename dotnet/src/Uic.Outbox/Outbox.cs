using System.Collections.Concurrent;
using System.Globalization;
using System.Runtime.ExceptionServices;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Outbox;

public sealed record OutboxRecord(
    [property: JsonPropertyName("id")] long Id,
    [property: JsonPropertyName("payload")] string Payload,
    [property: JsonPropertyName("at")] DateTimeOffset CreatedAt);

public sealed record OutboxStats(long Pending, long DroppedTotal, long Delivered, long Bytes, int Partitions);

/// <summary>
/// 一个投递目标。失败必须抛异常——只有异常才表示「位点不能推进」。
/// </summary>
public interface IOutboxSink
{
    string Name { get; }
    Task DeliverAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct);
}

public sealed record OutboxOptions
{
    public int MaxRecords { get; init; } = 100_000;
    public long MaxBytes { get; init; } = 256L * 1024 * 1024;
    public TimeSpan Ttl { get; init; } = TimeSpan.FromHours(24);
    public int BatchSize { get; init; } = 64;
    public TimeSpan FlushInterval { get; init; } = TimeSpan.FromSeconds(1);

    /// <summary>追加文件超过该大小时，若队列已清空则重写文件，避免无限增长。</summary>
    public long CompactThresholdBytes { get; init; } = 4L * 1024 * 1024;
}

/// <summary>
/// 持久化上报队列。
///
/// 设计要点：
/// - 每个 sink 独立分区、独立确认位点：一个北向通道挂掉不影响其他通道；
/// - 整批成功才推进位点：部分成功不推进，宁可重复不可丢；
/// - 三重上限（条数/字节/TTL）：磁盘不会无限增长，超限丢最旧并计数；
/// - 掉电只可能丢最后半行，加载时跳过即可，不允许整个队列起不来。
/// </summary>
public sealed class Outbox : IAsyncDisposable
{
    private readonly string _root;
    private readonly OutboxOptions _options;
    private readonly ConcurrentDictionary<string, Partition> _partitions = new(StringComparer.Ordinal);
    private readonly ConcurrentDictionary<string, IOutboxSink> _sinks = new(StringComparer.Ordinal);
    private readonly CancellationTokenSource _cts = new();
    private readonly SemaphoreSlim _flushLock = new(1, 1);
    private Task? _loop;
    private long _dropped;
    private int _disposed;

    public Outbox(string dataDir, OutboxOptions? options = null)
    {
        _root = Path.Combine(dataDir, "outbox");
        _options = options ?? new OutboxOptions();
        Directory.CreateDirectory(_root);

        // 启动时就把磁盘上的分区读回来。
        // 如果等到下一次 Enqueue 才建分区，重启后的 Stats 会显示「没有积压」，
        // 而实际上数据还躺在文件里——这是监控最不该撒谎的地方。
        foreach (var dir in Directory.EnumerateDirectories(_root))
        {
            var name = Path.GetFileName(dir);
            if (name.Length == 0) continue;

            _partitions.TryAdd(name, new Partition(dir));
        }
    }

    public OutboxOptions Options => _options;

    public void RegisterSink(IOutboxSink sink) => _sinks[sink.Name] = sink;

    /// <summary>
    /// 摘掉一个投递目标（热更新关闭 MQTT / journal 时用）。
    /// 分区与其中的数据保留：重新启用后继续从原位点投递，不会丢历史。
    /// </summary>
    public bool RemoveSink(string name) => _sinks.TryRemove(name, out _);

    public IReadOnlyList<string> Sinks => _sinks.Keys.OrderBy(k => k, StringComparer.Ordinal).ToArray();

    public void Enqueue(string sink, string payload)
    {
        var partition = _partitions.GetOrAdd(sink, name => new Partition(Path.Combine(_root, Sanitize(name))));
        var dropped = partition.Enqueue(payload, _options);
        if (dropped > 0) Interlocked.Add(ref _dropped, dropped);
    }

    public void Start()
    {
        if (_disposed != 0) return;
        _loop ??= Task.Run(() => LoopAsync(_cts.Token));
    }

    private async Task LoopAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            try
            {
                await FlushAsync(ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                return;
            }
            catch
            {
                // 投递失败不终止循环：下一轮继续，位点未推进，数据仍在。
            }

            try { await Task.Delay(_options.FlushInterval, ct).ConfigureAwait(false); }
            catch (OperationCanceledException) { return; }
        }
    }

    /// <summary>立刻冲刷一轮。API 的「立即上报」与测试都直接调它。</summary>
    public Task FlushAsync(CancellationToken ct) => FlushAsync(sink: null, ct);

    /// <summary>
    /// 只冲刷指定 sink（API 的 <c>outbox.flush</c> 支持 <c>sink?</c>）。
    /// 指定不存在的 sink 必须报错而不是静默成功——操作员会以为积压已经推走了。
    /// </summary>
    public async Task FlushAsync(string? sink, CancellationToken ct)
    {
        if (sink is not null && !_sinks.ContainsKey(sink))
            throw new KeyNotFoundException($"outbox sink '{sink}' is not registered");

        await _flushLock.WaitAsync(ct).ConfigureAwait(false);

        // 一个分区失败不得拖住其他分区：分区存在的理由就是「互不影响」。
        // 但失败必须如实上报，否则调用方会以为这一轮全都成功了。
        Exception? firstFailure = null;

        try
        {
            foreach (var (name, partition) in _partitions)
            {
                if (sink is not null && name != sink) continue;
                if (!_sinks.TryGetValue(name, out var sinkImpl)) continue;

                try
                {
                    while (!ct.IsCancellationRequested)
                    {
                        var batch = partition.Peek(_options.BatchSize);
                        if (batch.Count == 0)
                        {
                            partition.CompactIfEmpty(_options.CompactThresholdBytes);
                            break;
                        }

                        await sinkImpl.DeliverAsync(batch, ct).ConfigureAwait(false);
                        partition.Ack(batch[^1].Id);
                    }
                }
                catch (OperationCanceledException) when (ct.IsCancellationRequested)
                {
                    throw;
                }
                catch (Exception ex)
                {
                    firstFailure ??= ex;
                }
            }
        }
        finally
        {
            _flushLock.Release();
        }

        if (firstFailure is not null) ExceptionDispatchInfo.Capture(firstFailure).Throw();
    }

    public OutboxStats Stats()
    {
        long pending = 0, delivered = 0, bytes = 0;

        foreach (var p in _partitions.Values)
        {
            var s = p.Stats();
            pending += s.Pending;
            delivered += s.Delivered;
            bytes += s.Bytes;
        }

        return new OutboxStats(pending, Interlocked.Read(ref _dropped), delivered, bytes, _partitions.Count);
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        await _cts.CancelAsync().ConfigureAwait(false);

        var loop = _loop;
        if (loop is not null)
        {
            try { await loop.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        foreach (var p in _partitions.Values) p.Dispose();
        _cts.Dispose();
        _flushLock.Dispose();
    }

    private static string Sanitize(string name)
    {
        var sb = new StringBuilder(name.Length);
        foreach (var c in name)
        {
            sb.Append(char.IsLetterOrDigit(c) || c is '-' or '_' ? c : '_');
        }
        return sb.Length == 0 ? "sink" : sb.ToString();
    }

    /// <summary>单个 sink 的追加日志 + 确认位点。</summary>
    private sealed class Partition : IDisposable
    {
        private static readonly JsonSerializerOptions Json = new()
        {
            PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        };

        private readonly string _dir;
        private readonly string _dataFile;
        private readonly string _ackFile;
        private readonly object _lock = new();
        private readonly List<OutboxRecord> _pending = new();
        private FileStream? _stream;
        private long _nextId = 1;
        private long _acked;
        private long _delivered;
        private long _dropped;

        public Partition(string dir)
        {
            _dir = dir;
            Directory.CreateDirectory(dir);
            _dataFile = Path.Combine(dir, "records.ndjson");
            _ackFile = Path.Combine(dir, "acked.id");
            Load();
        }

        private void Load()
        {
            if (File.Exists(_ackFile) &&
                long.TryParse(File.ReadAllText(_ackFile), NumberStyles.Integer, CultureInfo.InvariantCulture, out var acked))
            {
                _acked = acked;
            }

            if (!File.Exists(_dataFile)) return;

            foreach (var line in File.ReadLines(_dataFile))
            {
                if (string.IsNullOrWhiteSpace(line)) continue;

                try
                {
                    var rec = JsonSerializer.Deserialize<OutboxRecord>(line, Json);
                    if (rec is null) continue;

                    _nextId = Math.Max(_nextId, rec.Id + 1);
                    if (rec.Id > _acked) _pending.Add(rec);
                }
                catch (JsonException)
                {
                    // 半行（掉电）直接跳过：宁可丢最后一条，也不让整个队列起不来。
                }
            }
        }

        public int Enqueue(string payload, OutboxOptions options)
        {
            lock (_lock)
            {
                var rec = new OutboxRecord(_nextId++, payload, DateTimeOffset.UtcNow);
                _pending.Add(rec);
                Append(rec);

                var dropped = 0;
                var cutoff = DateTimeOffset.UtcNow - options.Ttl;

                while (_pending.Count > options.MaxRecords || EstimateBytesLocked() > options.MaxBytes)
                {
                    _pending.RemoveAt(0);
                    dropped++;
                }

                while (_pending.Count > 0 && _pending[0].CreatedAt < cutoff)
                {
                    _pending.RemoveAt(0);
                    dropped++;
                }

                if (dropped > 0)
                {
                    _dropped += dropped;
                    _acked = _pending.Count > 0 ? _pending[0].Id - 1 : _nextId - 1;
                    PersistAck();
                }

                return dropped;
            }
        }

        public IReadOnlyList<OutboxRecord> Peek(int max)
        {
            lock (_lock) return _pending.Take(max).ToArray();
        }

        public void Ack(long id)
        {
            lock (_lock)
            {
                var removed = _pending.RemoveAll(r => r.Id <= id);
                if (removed == 0) return;

                _acked = Math.Max(_acked, id);
                _delivered += removed;
                PersistAck();
            }
        }

        public (long Pending, long Dropped, long Delivered, long Bytes) Stats()
        {
            lock (_lock) return (_pending.Count, _dropped, _delivered, EstimateBytesLocked());
        }

        /// <summary>队列清空且追加文件过大时重写文件，把已确认的历史真正抹掉。</summary>
        public void CompactIfEmpty(long thresholdBytes)
        {
            lock (_lock)
            {
                if (_pending.Count > 0) return;

                try
                {
                    var info = new FileInfo(_dataFile);
                    if (!info.Exists || info.Length < thresholdBytes) return;

                    _stream?.Dispose();
                    _stream = null;

                    var tmp = _dataFile + ".tmp";
                    File.WriteAllText(tmp, string.Empty);
                    File.Move(tmp, _dataFile, overwrite: true);
                }
                catch (IOException)
                {
                    // 压缩失败不影响正确性，下一轮再试。
                }
            }
        }

        private long EstimateBytesLocked()
        {
            long total = 0;
            foreach (var r in _pending) total += r.Payload.Length + 48L;
            return total;
        }

        private void Append(OutboxRecord rec)
        {
            _stream ??= new FileStream(_dataFile, FileMode.Append, FileAccess.Write, FileShare.Read);

            var line = JsonSerializer.Serialize(rec, Json) + "\n";
            var bytes = Encoding.UTF8.GetBytes(line);

            _stream.Write(bytes, 0, bytes.Length);
            _stream.Flush(flushToDisk: false);
        }

        private void PersistAck()
        {
            var tmp = _ackFile + ".tmp";
            File.WriteAllText(tmp, _acked.ToString(CultureInfo.InvariantCulture));
            File.Move(tmp, _ackFile, overwrite: true);
        }

        public void Dispose()
        {
            lock (_lock)
            {
                _stream?.Dispose();
                _stream = null;
            }
        }
    }
}

/// <summary>本地 journal 兜底：断网留痕、开发联调、CI 断言。它不是北向协议。</summary>
public sealed class JournalSink : IOutboxSink, IAsyncDisposable
{
    private readonly string _path;
    private readonly SemaphoreSlim _lock = new(1, 1);
    private FileStream? _stream;

    public JournalSink(string dataDir)
    {
        Directory.CreateDirectory(dataDir);
        _path = Path.Combine(dataDir, "journal.ndjson");
    }

    public string Name => "journal";
    public string FilePath => _path;

    public async Task DeliverAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
    {
        await _lock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            _stream ??= new FileStream(_path, FileMode.Append, FileAccess.Write, FileShare.Read);

            foreach (var rec in batch)
            {
                var bytes = Encoding.UTF8.GetBytes(rec.Payload + "\n");
                await _stream.WriteAsync(bytes, ct).ConfigureAwait(false);
            }

            await _stream.FlushAsync(ct).ConfigureAwait(false);
        }
        finally
        {
            _lock.Release();
        }
    }

    public ValueTask DisposeAsync()
    {
        _stream?.Dispose();
        _stream = null;
        _lock.Dispose();
        return ValueTask.CompletedTask;
    }
}