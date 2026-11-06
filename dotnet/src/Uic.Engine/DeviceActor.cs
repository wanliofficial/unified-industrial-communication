using System.Diagnostics;
using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Engine;

/// <summary>
/// 通道闸门：限制并发，并让写请求优先于读请求。
///
/// 写入是人的操作，延迟可感知；读是后台任务，晚 100ms 无所谓。
/// 这里的优先级是「尽力而为」：已经进入临界区的读不会被抢占，
/// 但新读会等待所有排队中的写完成。
/// </summary>
public sealed class ChannelGate : IDisposable
{
    private readonly SemaphoreSlim _slots;
    private int _writesWaiting;

    public ChannelGate(int maxConcurrency)
    {
        var n = Math.Max(1, maxConcurrency);
        _slots = new SemaphoreSlim(n, n);
    }

    public async Task<IDisposable> EnterReadAsync(CancellationToken ct)
    {
        while (Volatile.Read(ref _writesWaiting) > 0)
        {
            await Task.Delay(2, ct).ConfigureAwait(false);
        }

        await _slots.WaitAsync(ct).ConfigureAwait(false);
        return new Releaser(_slots);
    }

    public async Task<IDisposable> EnterWriteAsync(CancellationToken ct)
    {
        Interlocked.Increment(ref _writesWaiting);
        try
        {
            await _slots.WaitAsync(ct).ConfigureAwait(false);
            return new Releaser(_slots, () => Interlocked.Decrement(ref _writesWaiting));
        }
        catch
        {
            Interlocked.Decrement(ref _writesWaiting);
            throw;
        }
    }

    public void Dispose() => _slots.Dispose();

    private sealed class Releaser : IDisposable
    {
        private readonly SemaphoreSlim _sem;
        private readonly Action? _onDispose;
        private int _done;

        public Releaser(SemaphoreSlim sem, Action? onDispose = null)
        {
            _sem = sem;
            _onDispose = onDispose;
        }

        public void Dispose()
        {
            if (Interlocked.Exchange(ref _done, 1) != 0) return;
            _onDispose?.Invoke();
            _sem.Release();
        }
    }
}

public sealed record SchedulerOptions
{
    /// <summary>调度 tick。50ms 足以把 100 台设备均匀铺开，又不会空转太频繁。</summary>
    public TimeSpan Tick { get; init; } = TimeSpan.FromMilliseconds(50);

    /// <summary>读请求整体超时（含重试）。</summary>
    public TimeSpan ReadTimeout { get; init; } = TimeSpan.FromSeconds(5);

    /// <summary>探活间隔（熔断后）。</summary>
    public TimeSpan ProbeInterval { get; init; } = TimeSpan.FromSeconds(30);
}

/// <summary>
/// 每设备一个 actor：tick 驱动、永不并发采集、代次隔离。
/// 协议会话有状态（事务号、半双工、PDU 协商），并发采集是最常见的「偶发错乱」来源。
/// </summary>
public sealed class DeviceActor : IAsyncDisposable
{
    private readonly Device _device;
    private readonly Channel _channel;
    private readonly IDriver _driver;
    private readonly PointCache _cache;
    private readonly ChannelGate _gate;
    private readonly ILogger _log;
    private readonly SchedulerOptions _options;
    private readonly TimeSpan _startPhase;
    private readonly IReadOnlyList<PointRef> _points;
    private readonly IReadOnlyList<ReadSpan> _spans;
    private readonly Dictionary<string, Point> _pointsById;

    private readonly object _stateLock = new();
    private readonly Queue<double> _durations = new();
    private readonly SemaphoreSlim _connLock = new(1, 1);

    private IConn? _conn;
    private CancellationTokenSource? _cts;
    private Task? _loop;
    private DateTimeOffset _since = DateTimeOffset.UtcNow;
    private RuntimeState _state = RuntimeState.Configured;
    private long _readsTotal;
    private long _readsFailed;
    private string? _lastError;
    private DateTimeOffset? _lastSuccessAt;
    private int _consecutiveFailures;
    private int _generation;
    private int _disposed;

    public DeviceActor(
        Device device,
        Channel channel,
        IDriver driver,
        PointCache cache,
        ChannelGate gate,
        ILogger log,
        SchedulerOptions options,
        TimeSpan? startPhase = null)
    {
        _device = device;
        _channel = channel;
        _driver = driver;
        _cache = cache;
        _gate = gate;
        _log = log;
        _options = options;
        _startPhase = startPhase ?? TimeSpan.Zero;
        _pointsById = device.Points.ToDictionary(p => p.Id, StringComparer.Ordinal);
        _points = device.Points.Where(p => p.Enabled).Select(p => PointRef.Of(device, p)).ToArray();
        _spans = ReadPlanner.Plan(_points);
    }

    public string DeviceId => _device.Id;
    public Device Device => _device;
    public string Protocol => _channel.Protocol;
    public int Generation => Volatile.Read(ref _generation);
    public RuntimeState State { get { lock (_stateLock) return _state; } }
    public ConfiguredState ConfiguredState => _device.Enabled ? ConfiguredState.Enabled : ConfiguredState.Disabled;
    public IReadOnlyList<ReadSpan> Spans => _spans;

    /// <summary>启动相位。调度器按设备序号铺开，避免同周期设备在同一毫秒醒来。</summary>
    public TimeSpan StartPhase => _startPhase;

    public DeviceStats Stats
    {
        get
        {
            lock (_stateLock)
            {
                var total = Interlocked.Read(ref _readsTotal);
                var failed = Interlocked.Read(ref _readsFailed);
                var success = total == 0 ? 1.0 : (total - failed) / (double)total;
                return new DeviceStats(
                    total,
                    failed,
                    Math.Round(success, 4),
                    P95(),
                    _lastError,
                    _lastSuccessAt,
                    _generation);
            }
        }
    }

    public void Start()
    {
        if (Interlocked.CompareExchange(ref _disposed, 0, 0) != 0) return;

        if (!_device.Enabled)
        {
            SetState(RuntimeState.Disabled);
            return;
        }

        _cts = new CancellationTokenSource();
        var token = _cts.Token;
        _loop = Task.Run(() => LoopAsync(token), token);
    }

    /// <summary>重启采集：递增代次，旧代次的结果会被丢弃。</summary>
    public async Task RestartAsync()
    {
        await StopAsync().ConfigureAwait(false);
        Interlocked.Increment(ref _generation);
        SetState(RuntimeState.Configured);
        Start();
    }

    public async Task StopAsync()
    {
        var cts = Interlocked.Exchange(ref _cts, null);
        if (cts is not null)
        {
            await cts.CancelAsync().ConfigureAwait(false);
        }

        var loop = Interlocked.Exchange(ref _loop, null);
        if (loop is not null)
        {
            try { await loop.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        cts?.Dispose();
        await CloseConnAsync().ConfigureAwait(false);
        SetState(RuntimeState.Disabled);
    }

    private async Task LoopAsync(CancellationToken ct)
    {
        // 单调时间戳，不受 NTP 校时/手工改钟影响。
        var nextDue = Stopwatch.GetTimestamp() + ScheduleMath.ToTimestamp(_startPhase);

        while (!ct.IsCancellationRequested)
        {
            var now = Stopwatch.GetTimestamp();

            if (now < nextDue)
            {
                var wait = ScheduleMath.Remaining(now, nextDue);
                if (wait > _options.Tick) wait = _options.Tick;

                try { await Task.Delay(wait, ct).ConfigureAwait(false); }
                catch (OperationCanceledException) { return; }

                continue;
            }

            try
            {
                await CollectOnceAsync(ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                return;
            }
            catch (Exception ex)
            {
                // 兜底：采集循环不能因为任何单点异常而退出。
                _log.LogError(ex, "device {Device} collection loop caught unexpected error", _device.Id);
            }

            // 退避：失败越多，下次尝试越晚；上限为设备恢复间隔。
            var backoff = _consecutiveFailures == 0
                ? _device.Interval
                : TimeSpan.FromMilliseconds(Math.Min(
                    _device.Interval.TotalMilliseconds * Math.Pow(2, Math.Min(_consecutiveFailures, 6)),
                    Math.Max(_device.Interval.TotalMilliseconds, _device.RecoverInterval.TotalMilliseconds)));

            var jitterMs = Random.Shared.NextDouble() * 0.1 * backoff.TotalMilliseconds;
            var delay = backoff + TimeSpan.FromMilliseconds(jitterMs);
            nextDue = Stopwatch.GetTimestamp() + ScheduleMath.ToTimestamp(delay);
        }
    }

    private async Task CollectOnceAsync(CancellationToken ct)
    {
        var started = Stopwatch.GetTimestamp();

        // 重试只针对「临时性」失败：超时、连接断开。地址越界、协议错、权限不足
        // 重试一百次也是同样的结果，只会把故障拖得更久。
        var attempts = Math.Clamp(_device.Retries, 0, 10) + 1;

        for (var attempt = 1; ; attempt++)
        {
            try
            {
                var conn = await EnsureConnAsync(ct).ConfigureAwait(false);

                using var lease = await _gate.EnterReadAsync(ct).ConfigureAwait(false);
                using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
                timeout.CancelAfter(_device.Timeout ?? _channel.Timeout);

                var raw = await conn.ReadAsync(_spans, timeout.Token).ConfigureAwait(false);

                // 全部点位都坏 = 这一轮失败；只按「请求是否成功」统计会掩盖协议层问题。
                // 先判断再发布：需要重试时就把这批坏值丢掉，否则界面会闪一下 Bad 再变回 Good。
                var allBad = raw.Count > 0 && raw.All(r => r.Quality != Quality.Good);

                if (allBad && IsTemporary(raw[0].Quality) && attempt < attempts)
                {
                    await RetryAsync(attempt, attempts, raw[0].Error, ct).ConfigureAwait(false);
                    continue;
                }

                var generation = Volatile.Read(ref _generation);
                var now = DateTimeOffset.UtcNow;

                foreach (var sample in raw)
                {
                    // 代次隔离：设备重启/重连后，旧连接迟到的结果一律丢弃。
                    if (generation != Volatile.Read(ref _generation)) break;

                    if (!_pointsById.TryGetValue(sample.PointId, out var point)) continue;
                    if (!point.Enabled) continue;

                    var seq = _cache.NextSeq(_device.Id, sample.PointId);
                    var processed = Pipeline.Process(_device, point, sample, seq, now);
                    _cache.PublishConfigured(processed, point);
                }

                if (allBad)
                {
                    Interlocked.Increment(ref _readsTotal);
                    Interlocked.Increment(ref _readsFailed);
                    RecordDuration(Stopwatch.GetElapsedTime(started).TotalMilliseconds);

                    Fail(raw[0].Error ?? "all points returned non-good quality", raw[0].Quality);
                    return;
                }

                Interlocked.Increment(ref _readsTotal);
                RecordDuration(Stopwatch.GetElapsedTime(started).TotalMilliseconds);

                _consecutiveFailures = 0;
                _lastError = null;
                _lastSuccessAt = now;
                SetState(RuntimeState.Collecting);
                return;
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw;
            }
            catch (OperationCanceledException)
            {
                // 设备级 deadline 到期，而传输层没先报自己的超时（常见于串口读超时
                // 配得比设备 timeout 长）。语义上这就是超时：可重试，质量码用 timeout，
                // 而不是含糊的 bad。
                var budget = _device.Timeout ?? _channel.Timeout;
                var message = $"read deadline exceeded after {budget.TotalMilliseconds:0}ms";

                if (attempt < attempts)
                {
                    await RetryAsync(attempt, attempts, message, ct).ConfigureAwait(false);
                    continue;
                }

                Interlocked.Increment(ref _readsTotal);
                Interlocked.Increment(ref _readsFailed);
                RecordDuration(Stopwatch.GetElapsedTime(started).TotalMilliseconds);

                Fail(message, Quality.Timeout);
                return;
            }
            catch (Exception ex)
            {
                var driverError = ex as DriverException;
                var quality = driverError is null ? Quality.Bad : QualityMap.From(driverError.Kind);
                var temporary = driverError?.Temporary == true;

                if (temporary && attempt < attempts)
                {
                    await RetryAsync(attempt, attempts, ex.Message, ct).ConfigureAwait(false);
                    continue;
                }

                Interlocked.Increment(ref _readsTotal);
                Interlocked.Increment(ref _readsFailed);
                RecordDuration(Stopwatch.GetElapsedTime(started).TotalMilliseconds);

                Fail(ex.Message, quality);
                return;
            }
        }
    }

    /// <summary>
    /// 重试前先断开：超时后链路上可能还有迟到的响应，复用连接会把旧帧当成新请求的回复。
    /// 重连的代价（一次 Probe）远小于「拿到一个错的值」。
    /// </summary>
    private async Task RetryAsync(int attempt, int attempts, string? error, CancellationToken ct)
    {
        _log.LogDebug("device {Device} retrying ({Attempt}/{Total}) after: {Error}",
            _device.Id, attempt, attempts - 1, error);

        await CloseConnAsync().ConfigureAwait(false);
        ct.ThrowIfCancellationRequested();
    }

    private static bool IsTemporary(Quality quality)
        => quality is Quality.Timeout or Quality.CommError;

    /// <summary>
    /// 失败时把本轮点位全部标成对应质量。
    /// 不这么做，界面上会保留上一次的好值，看起来一切正常——这是最危险的假象。
    /// </summary>
    private void Fail(string message, Quality quality)
    {
        // 地址错是点表配置问题，不是链路问题：把它算进熔断计数会让「一个点位写错」
        // 拖垮整台设备，现场看到的是设备离线而不是「这条地址不存在」。
        var countsTowardBreaker = quality != Quality.AddressError;
        if (countsTowardBreaker) _consecutiveFailures++;

        _lastError = message;

        var now = DateTimeOffset.UtcNow;
        foreach (var point in _device.Points.Where(p => p.Enabled))
        {
            var seq = _cache.NextSeq(_device.Id, point.Id);
            var previous = _cache.Get(_device.Id, point.Id);
            var sample = new Sample(
                _device.Id,
                point.Id,
                previous?.Value ?? DataValue.Number(point.DataType, 0),
                quality,
                point.Unit,
                previous?.DeviceTime,
                now,
                seq,
                message);

            _cache.PublishConfigured(sample, point);
        }

        if (!countsTowardBreaker)
        {
            // 链路本身是通的，状态回到采集：错误只体现在点位质量上。
            // 这里不能直接 return，否则首轮就遇到地址错的设备会永远停在 connecting。
            SetState(RuntimeState.Collecting);
            return;
        }

        if (_consecutiveFailures >= _device.FailureThreshold)
        {
            SetState(RuntimeState.Faulted);
            _log.LogWarning("device {Device} faulted after {Failures} failures: {Error}",
                _device.Id, _consecutiveFailures, message);
            _ = CloseConnAsync();
        }
        else
        {
            SetState(RuntimeState.Degraded);
            _log.LogDebug("device {Device} degraded ({Failures}): {Error}",
                _device.Id, _consecutiveFailures, message);
        }
    }

    private async Task<IConn> EnsureConnAsync(CancellationToken ct)
    {
        await _connLock.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            if (_conn is not null) return _conn;

            SetState(RuntimeState.Connecting);
            var generation = Interlocked.Increment(ref _generation);

            var ctx = new OpenContext(_channel, _device, _log, NullMetrics.Instance);
            var conn = _driver.Open(ctx);

            try
            {
                using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
                timeout.CancelAfter(_channel.ConnectTimeout);
                await conn.ProbeAsync(timeout.Token).ConfigureAwait(false);
            }
            catch
            {
                await conn.DisposeAsync().ConfigureAwait(false);
                throw;
            }

            // 连接建立期间若又被要求重启，本次连接直接作废。
            if (generation != Volatile.Read(ref _generation))
            {
                await conn.DisposeAsync().ConfigureAwait(false);
                throw DriverException.Connection(_channel.Protocol, "connect", "connection superseded by restart");
            }

            _conn = conn;
            return conn;
        }
        finally
        {
            _connLock.Release();
        }
    }

    private async Task CloseConnAsync()
    {
        await _connLock.WaitAsync().ConfigureAwait(false);
        try
        {
            var conn = _conn;
            _conn = null;
            if (conn is null) return;

            // 递增代次：正在飞行中的读取结果全部作废。
            Interlocked.Increment(ref _generation);
            try { await conn.DisposeAsync().ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }
        finally
        {
            _connLock.Release();
        }
    }

    public async Task<IReadOnlyList<WriteResult>> WriteAsync(IReadOnlyList<WriteCommand> commands, CancellationToken ct)
    {
        var conn = await EnsureConnAsync(ct).ConfigureAwait(false);

        using var lease = await _gate.EnterWriteAsync(ct).ConfigureAwait(false);
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
        timeout.CancelAfter(_device.Timeout ?? _channel.Timeout);

        return await conn.WriteAsync(commands, timeout.Token).ConfigureAwait(false);
    }

    public async Task<bool> SelfTestAsync(CancellationToken ct)
    {
        try
        {
            var conn = await EnsureConnAsync(ct).ConfigureAwait(false);
            await conn.ProbeAsync(ct).ConfigureAwait(false);
            return true;
        }
        catch
        {
            return false;
        }
    }

    public DeviceView ToView(int pointsGood, int pointsBad)
    {
        var stats = Stats;
        return new DeviceView(
            _device.Id,
            _device.Name,
            _device.ChannelId,
            Protocol,
            ConfiguredState,
            State,
            _since,
            stats.Generation,
            _device.Interval,
            stats,
            _device.Points.Count,
            pointsGood,
            pointsBad);
    }

    private void SetState(RuntimeState state)
    {
        lock (_stateLock)
        {
            if (_state == state) return;
            _state = state;
            _since = DateTimeOffset.UtcNow;
        }
    }

    private void RecordDuration(double ms)
    {
        lock (_stateLock)
        {
            _durations.Enqueue(ms);
            while (_durations.Count > 256) _durations.Dequeue();
        }
    }

    private double P95()
    {
        lock (_stateLock)
        {
            if (_durations.Count == 0) return 0;
            var arr = _durations.OrderBy(x => x).ToArray();
            var idx = (int)Math.Ceiling(arr.Length * 0.95) - 1;
            return Math.Round(arr[Math.Clamp(idx, 0, arr.Length - 1)], 1);
        }
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;
        await StopAsync().ConfigureAwait(false);
        _connLock.Dispose();
    }
}

internal sealed class NullMetrics : IMetrics
{
    public static readonly NullMetrics Instance = new();
    public void Counter(string name, double delta, IReadOnlyDictionary<string, string>? labels = null) { }
    public void Observe(string name, double value, IReadOnlyDictionary<string, string>? labels = null) { }
}