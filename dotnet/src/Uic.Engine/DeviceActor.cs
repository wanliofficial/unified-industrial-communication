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

    /// <summary>
    /// 数据目录。需要落本地文件的协议（证书库）通过 <see cref="OpenContext"/> 拿到它，
    /// 而不是去读进程的当前目录——驱动的输入必须全部来自显式参数。
    /// </summary>
    public string? DataDir { get; init; }
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
    private readonly IReadOnlyList<CollectBucket> _buckets;
    private readonly TimeSpan _baseInterval;
    private readonly TimeSpan _slowestInterval;
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

        // 点位周期可以覆盖设备周期：按有效周期分桶，每一轮只读「到期」的桶。
        //
        // 不给每个周期单独开循环，是因为同一设备上的请求必须串行（事务号、
        // 半双工、PDU 协商都经不起交错）；一个循环按到期时间合并读取，
        // 既保住了串行，又让快慢变量各按各的节奏走。
        var buckets = new List<CollectBucket>();
        foreach (var group in device.Points
                     .Where(p => p.Enabled)
                     .GroupBy(p => p.EffectiveInterval(device))
                     .OrderBy(g => g.Key))
        {
            var refs = group.Select(p => PointRef.Of(device, p)).ToArray();
            buckets.Add(new CollectBucket(group.Key, ReadPlanner.Plan(refs)));
        }

        _buckets = buckets;
        _baseInterval = _buckets.Count == 0 ? device.Interval : _buckets[0].Interval;
        _slowestInterval = _buckets.Count == 0 ? device.Interval : _buckets[^1].Interval;
    }

    public string DeviceId => _device.Id;
    public Device Device => _device;
    public string Protocol => _channel.Protocol;
    public int Generation => Volatile.Read(ref _generation);
    public RuntimeState State { get { lock (_stateLock) return _state; } }
    public ConfiguredState ConfiguredState => _device.Enabled ? ConfiguredState.Enabled : ConfiguredState.Disabled;

    /// <summary>
    /// 全设备合并后的读窗口（不区分周期）。协议自检与「整台设备一次读」的调用方用它；
    /// 实际采集按 <see cref="CollectionIntervals"/> 分组各自规划，不再共用这份计划。
    /// </summary>
    public IReadOnlyList<ReadSpan> Spans => _spans;

    /// <summary>
    /// 设备内最慢的点位周期。看门狗用它判断「多久没有新样本才算采集停滞」：
    /// 只有慢变量的设备按最快周期算会把正常状态误报成故障。
    /// </summary>
    public TimeSpan SlowestInterval => _slowestInterval;

    /// <summary>实际生效的采集周期分组，升序。诊断与控制台用。</summary>
    public IReadOnlyList<TimeSpan> CollectionIntervals
        => _buckets.Select(b => b.Interval).ToArray();

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
        var started = Stopwatch.GetTimestamp();
        var phase = ScheduleMath.ToTimestamp(_startPhase);

        // 所有周期分组共用启动相位：设备之间已经铺开，设备内部再错开会让
        // 「快变量每 100ms、慢变量每 10s」的第一次读数毫无必要地晚到。
        foreach (var bucket in _buckets) bucket.NextDue = started + phase;

        while (!ct.IsCancellationRequested)
        {
            if (_buckets.Count == 0)
            {
                // 点位全部被禁用：没有可读的东西，但设备本身是「在跑」的。
                // 保持 Configured 会让控制台把一个正常部署显示成未启动。
                SetState(RuntimeState.Collecting);

                try { await Task.Delay(Timeout.Infinite, ct).ConfigureAwait(false); }
                catch (OperationCanceledException) { return; }
                continue;
            }

            var now = Stopwatch.GetTimestamp();
            var nextDue = _buckets.Min(b => b.NextDue);

            if (now < nextDue)
            {
                var wait = ScheduleMath.Remaining(now, nextDue);
                if (wait > _options.Tick) wait = _options.Tick;

                try { await Task.Delay(wait, ct).ConfigureAwait(false); }
                catch (OperationCanceledException) { return; }

                continue;
            }

            var due = _buckets.Where(b => b.NextDue <= now).ToArray();

            // 到期的桶合并成一次读取：同一设备上的请求必须串行，
            // 分两次调用只会多一轮往返。
            var spans = due.Length == 1
                ? due[0].Spans
                : due.SelectMany(b => b.Spans).ToArray();

            try
            {
                await CollectOnceAsync(spans, ct).ConfigureAwait(false);
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

            var finished = Stopwatch.GetTimestamp();

            if (_consecutiveFailures > 0)
            {
                // 链路有故障时整台设备一起退避：只让快周期继续打，
                // 只会把已经超时的设备压得更死。
                var retryAt = finished + ScheduleMath.ToTimestamp(BackoffDelay());
                foreach (var bucket in _buckets)
                    bucket.NextDue = Math.Max(bucket.NextDue, retryAt);

                continue;
            }

            // 从本轮结束时间起算：采集耗时不额外扣除，但也不补发积压的周期。
            // 补发会让一次卡顿之后立刻来一串突发请求，对现场链路毫无好处。
            foreach (var bucket in due)
                bucket.NextDue = finished + bucket.IntervalTicks;
        }
    }

    /// <summary>
    /// 失败退避：按最快周期的 2^n 增长，上限取恢复间隔。
    /// 基数用最快周期而不是设备周期——点位覆盖后两者可能差几个数量级。
    /// </summary>
    private TimeSpan BackoffDelay()
    {
        var baseMs = _baseInterval.TotalMilliseconds;
        var ceiling = Math.Max(baseMs, _device.RecoverInterval.TotalMilliseconds);
        var backoffMs = Math.Min(baseMs * Math.Pow(2, Math.Min(_consecutiveFailures, 6)), ceiling);
        var jitter = Random.Shared.NextDouble() * 0.1 * backoffMs;
        return TimeSpan.FromMilliseconds(backoffMs + jitter);
    }

    private async Task CollectOnceAsync(IReadOnlyList<ReadSpan> spans, CancellationToken ct)
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

                var raw = await conn.ReadAsync(spans, timeout.Token).ConfigureAwait(false);

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

                    Fail(raw[0].Error ?? "all points returned non-good quality", raw[0].Quality, spans);
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

                Fail(message, Quality.Timeout, spans);
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

                Fail(ex.Message, quality, spans);
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
    ///
    /// 只标本轮真正读过的点位：周期覆盖之后，一次失败可能只影响快变量的那个桶，
    /// 把慢变量也一起打成坏值会凭空制造一片「设备故障」。
    /// </summary>
    private void Fail(string message, Quality quality, IReadOnlyList<ReadSpan> spans)
    {
        // 地址错是点表配置问题，不是链路问题：把它算进熔断计数会让「一个点位写错」
        // 拖垮整台设备，现场看到的是设备离线而不是「这条地址不存在」。
        var countsTowardBreaker = quality != Quality.AddressError;
        if (countsTowardBreaker) _consecutiveFailures++;

        _lastError = message;

        var failed = spans
            .SelectMany(s => s.Points)
            .Select(p => p.Id)
            .ToHashSet(StringComparer.Ordinal);

        var now = DateTimeOffset.UtcNow;
        foreach (var point in _device.Points.Where(p => p.Enabled && failed.Contains(p.Id)))
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

            var ctx = new OpenContext(_channel, _device, _log, NullMetrics.Instance, _options.DataDir);
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
        if (commands.Count == 0) return Array.Empty<WriteResult>();

        // 值形状先校验，再建链。元素个数与点位声明不一致属于请求错误，
        // 既不该占用通道预算，也不能寄希望于驱动「顺手」报错——
        // 进程内的虚拟驱动不会报，真设备则可能写偏半段寄存器，
        // 而回读只比较前几个元素，看起来仍然「写成功」。
        var accepted = new List<WriteCommand>(commands.Count);
        var results = new WriteResult?[commands.Count];

        for (var i = 0; i < commands.Count; i++)
        {
            var command = commands[i];
            var expected = command.Point.Elements;

            if (expected > 1)
            {
                if (command.Value.Array is not { } array)
                {
                    results[i] = Rejected(command.Point.Id,
                        $"value.shape: '{command.Point.Id}' is an array of {expected} elements, got a scalar");
                    continue;
                }

                if (array.Count != expected)
                {
                    results[i] = Rejected(command.Point.Id,
                        $"value.shape: '{command.Point.Id}' needs {expected} elements, got {array.Count}");
                    continue;
                }
            }
            else if (command.Value.Array is not null)
            {
                // 标量点位只接受标量值：允许「单元素数组」会得到两种等价的线上写法，
                // 回读比较与上层展示都得多写一条分支。
                results[i] = Rejected(command.Point.Id,
                    $"value.shape: '{command.Point.Id}' is a scalar point, got an array");
                continue;
            }

            accepted.Add(command);
        }

        if (accepted.Count == 0)
            return results.Select(r => r!).ToArray();

        var conn = await EnsureConnAsync(ct).ConfigureAwait(false);

        using var lease = await _gate.EnterWriteAsync(ct).ConfigureAwait(false);
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
        timeout.CancelAfter(_device.Timeout ?? _channel.Timeout);

        var written = await conn.WriteAsync(accepted, timeout.Token).ConfigureAwait(false);

        // confirm=true 才回读：写命令本身已经成功，回读是额外的读预算；
        // 对「闪一下就复位」的命令寄存器强行回读只会制造假故障，
        // 所以由调用方按点位决定，而不是全局默认打开。
        var confirmed = accepted.Where(c => c.Confirm).ToArray();

        if (confirmed.Length > 0)
            written = await VerifyWriteAsync(conn, confirmed, written, ct, timeout.Token).ConfigureAwait(false);

        // 按请求顺序把驱动结果填回被拒项留下的空位：调用方按下标对齐，不能因为
        // 「中间有一条形状错误」就把后面的结果整体前移。
        //
        // 同一个点位在一次批量里出现多次是合法的（以最后一次写入为准），
        // 所以按 id 排队而不是直接建字典——字典遇到重复键会抛异常，
        // 把一个「值形状错误」升级成整批 500。
        var pending = written
            .GroupBy(r => r.PointId, StringComparer.Ordinal)
            .ToDictionary(g => g.Key, g => new Queue<WriteResult>(g), StringComparer.Ordinal);

        var merged = new List<WriteResult>(commands.Count);
        var cursor = 0;

        foreach (var slot in results)
        {
            if (slot is not null)
            {
                merged.Add(slot);
                continue;
            }

            var id = accepted[cursor++].Point.Id;
            merged.Add(pending.TryGetValue(id, out var queue) && queue.Count > 0
                ? queue.Dequeue()
                : new WriteResult(id, false, Quality.Bad, DateTimeOffset.UtcNow,
                    "write.driver_returned_nothing"));
        }

        return merged;
    }

    private static WriteResult Rejected(string pointId, string error)
        => new(pointId, false, Quality.Bad, DateTimeOffset.UtcNow, error);

    /// <summary>
    /// 写后回读。放在同一个通道租约里完成，回读与写入之间不会插进一轮采集，
    /// 否则读到的是采集循环覆盖后的值，校验就失去了意义。
    /// </summary>
    private async Task<IReadOnlyList<WriteResult>> VerifyWriteAsync(
        IConn conn,
        IReadOnlyList<WriteCommand> confirmed,
        IReadOnlyList<WriteResult> results,
        CancellationToken ct,
        CancellationToken token)
    {
        // 同一个点位在一次批量里出现多次时，以最后一次写入的值为准。
        var expected = new Dictionary<string, DataValue>(StringComparer.Ordinal);
        var refs = new List<PointRef>();
        foreach (var command in confirmed)
        {
            expected[command.Point.Id] = command.Value;
            if (!refs.Any(p => string.Equals(p.Id, command.Point.Id, StringComparison.Ordinal)))
                refs.Add(command.Point);
        }

        var verdicts = new Dictionary<string, (bool Ok, Quality Quality, string? Error)>(StringComparer.Ordinal);

        try
        {
            var samples = await conn.ReadAsync(ReadPlanner.Plan(refs), token).ConfigureAwait(false);
            var byId = samples
                .Where(s => s.Value is not null)
                .GroupBy(s => s.PointId, StringComparer.Ordinal)
                .ToDictionary(g => g.Key, g => g.Last(), StringComparer.Ordinal);

            foreach (var point in refs)
            {
                var wanted = expected[point.Id];

                if (!byId.TryGetValue(point.Id, out var sample))
                {
                    verdicts[point.Id] = (false, Quality.Uncertain,
                        "verify.no_value: device returned no value");
                    continue;
                }

                if (sample.Quality != Quality.Good)
                {
                    verdicts[point.Id] = (false, sample.Quality,
                        $"verify.quality: readback quality is {sample.Quality.ToString().ToLowerInvariant()}");
                    continue;
                }

                if (!sample.Value!.Matches(wanted))
                {
                    verdicts[point.Id] = (false, Quality.Uncertain,
                        $"verify.mismatch: wrote {wanted}, read {sample.Value}");
                    continue;
                }

                verdicts[point.Id] = (true, Quality.Good, null);
            }
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            // 写入已经下发，回读失败只能说明「不确定」，不能谎报成写入失败：
            // 现场处置完全不同——前者查回读链路，后者查写命令。
            var quality = ex is DriverException driver ? QualityMap.From(driver.Kind) : Quality.Uncertain;
            foreach (var point in refs)
            {
                verdicts[point.Id] = (false, quality, $"verify.readback_failed: {ex.Message}");
            }
        }

        var output = new List<WriteResult>(results.Count);
        foreach (var result in results)
        {
            // 写都没成功就没有「回读校验」可言，保留驱动的原始裁决。
            if (!result.Ok || !verdicts.TryGetValue(result.PointId, out var verdict))
            {
                output.Add(result);
                continue;
            }

            output.Add(result with
            {
                Ok = verdict.Ok,
                Quality = verdict.Ok ? Quality.Good : verdict.Quality,
                Error = verdict.Error,
                Verified = verdict.Ok,
            });
        }

        return output;
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

    /// <summary>
    /// 一个采集周期分组：同一有效周期的点位，以及它们的读窗口计划。
    /// <see cref="NextDue"/> 只在采集循环线程里读写，不需要同步。
    /// </summary>
    private sealed class CollectBucket
    {
        public CollectBucket(TimeSpan interval, IReadOnlyList<ReadSpan> spans)
        {
            Interval = interval;
            IntervalTicks = ScheduleMath.ToTimestamp(interval);
            Spans = spans;
        }

        public TimeSpan Interval { get; }

        /// <summary>周期换算成 <see cref="Stopwatch"/> 时间戳单位，避免每轮重复取整。</summary>
        public long IntervalTicks { get; }

        public IReadOnlyList<ReadSpan> Spans { get; }

        /// <summary>下一到期时刻（<see cref="Stopwatch"/> 时间戳）。</summary>
        public long NextDue { get; set; }
    }
}

internal sealed class NullMetrics : IMetrics
{
    public static readonly NullMetrics Instance = new();
    public void Counter(string name, double delta, IReadOnlyDictionary<string, string>? labels = null) { }
    public void Observe(string name, double value, IReadOnlyDictionary<string, string>? labels = null) { }
}