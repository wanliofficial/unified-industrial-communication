using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Engine;
using Uic.Northbound;
using Uic.Obs;

namespace Uic.Runtime;

/// <summary>
/// 设备状态观测：把运行态变化转成 MQTT 的 <c>$state</c>，并做两件调度器不该管的事——
/// 采集停滞检测与状态变化日志。
///
/// 为什么用轮询而不是回调：状态变化频率是「秒级」，轮询成本可以忽略，
/// 而给调度器加事件会引入一条「订阅者抛异常怎么办」的新故障路径。
/// 状态对外是 retained 的当前事实，晚一秒推送没有任何影响。
/// </summary>
public sealed class DeviceStateWatcher : IAsyncDisposable
{
    private readonly Scheduler _engine;
    private readonly Func<MqttSink?> _mqtt;
    private readonly ILogger _log;
    private readonly MetricsRegistry _metrics;
    private readonly Dictionary<string, string> _lastState = new(StringComparer.Ordinal);

    /// <summary>每台设备最近一次被判为停滞的单调时刻（去重用，不用墙钟）。</summary>
    private readonly Dictionary<string, long> _staleWarned = new(StringComparer.Ordinal);

    /// <summary>
    /// 每台设备「第一次看到当前这个最新样本」的单调时刻。
    ///
    /// 停滞必须是**观察出来的**，不能只凭墙钟算：NTP 校时把钟往前跳一小时，
    /// 墙钟意义上的年龄会立刻超过阈值，而设备其实一直在采集。
    /// </summary>
    private readonly Dictionary<string, (DateTimeOffset Newest, long SeenAt)> _firstSeen = new(StringComparer.Ordinal);

    private readonly CancellationTokenSource _cts = new();
    private Task? _loop;
    private int _disposed;

    public DeviceStateWatcher(
        Scheduler engine,
        Func<MqttSink?> mqtt,
        ILogger log,
        MetricsRegistry metrics)
    {
        _engine = engine;
        _mqtt = mqtt;
        _log = log;
        _metrics = metrics;
    }

    public TimeSpan Period { get; init; } = TimeSpan.FromSeconds(1);

    /// <summary>连续多久没有新样本判定为采集停滞。取 10 个周期与 30 秒的较大值。</summary>
    public TimeSpan MinimumStaleAge { get; init; } = TimeSpan.FromSeconds(30);

    /// <summary>
    /// 时间来源。默认系统时钟；用例注入可拨的时钟，把「校时跳变」与「设备真的不动」
    /// 分开验证——这两件事在墙钟上看起来一模一样，只看墙钟就分不开。
    /// </summary>
    public TimeProvider Time { get; init; } = TimeProvider.System;

    /// <summary>
    /// 当前被观测的设备数。用例据此确认「下线设备确实被遗忘了」——
    /// 没有这个口子就只能靠反射或间接行为去猜。
    /// </summary>
    public int TrackedDeviceCount => _lastState.Count;

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
                await TickAsync(ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                return;
            }
            catch (Exception ex)
            {
                _log.LogWarning(ex, "device state watcher tick failed");
            }

            try { await Task.Delay(Period, ct).ConfigureAwait(false); }
            catch (OperationCanceledException) { return; }
        }
    }

    /// <summary>
    /// 跑一轮观测。公开是为了让用例能确定性地推一轮，而不是等 <see cref="Period"/>；
    /// 生产路径只由 <see cref="LoopAsync"/> 调用。
    /// </summary>
    public async Task TickAsync(CancellationToken ct)
    {
        var sink = _mqtt();
        var wallNow = Time.GetUtcNow();
        var monoNow = Time.GetTimestamp();

        var actors = _engine.Actors;
        var live = new HashSet<string>(actors.Count, StringComparer.Ordinal);

        foreach (var actor in actors)
        {
            live.Add(actor.DeviceId);
            var state = JsonEnum.ToWire(actor.State);
            var previous = _lastState.TryGetValue(actor.DeviceId, out var prev) ? prev : null;

            if (!string.Equals(previous, state, StringComparison.Ordinal))
            {
                _lastState[actor.DeviceId] = state;

                // 首次观测也要推一次：retained 状态必须让新消费者一问即知。
                if (sink is not null)
                {
                    await sink.PublishStateAsync(
                        actor.DeviceId, state, previous, actor.Stats.LastError, ct).ConfigureAwait(false);
                }

                if (previous is not null)
                {
                    _log.LogInformation("device {Device} state {From} -> {To}",
                        actor.DeviceId, previous, state);
                    _metrics.Counter("uic_device_state_changes_total", 1, new Dictionary<string, string>
                    {
                        ["device"] = actor.DeviceId,
                        ["state"] = state,
                    });
                }
            }

            if (actor.State == RuntimeState.Collecting) CheckStale(actor, wallNow, monoNow);
        }

        // 热更新删掉的设备必须一并遗忘：三个字典都按 deviceId 建条目，
        // 不清就一直攒着（反复改配置的部署会持续增长）。更要紧的是两条错语义：
        // 设备删了又原样加回来时，_lastState 里还是旧状态，等于「状态没变」，
        // $state 不会被重推；_staleWarned 里还是旧的告警时刻，
        // 会让新一轮停滞在 5 分钟去重窗口内被吞掉。
        var gone = _lastState.Keys.Where(k => !live.Contains(k)).ToArray();
        if (gone.Length > 0)
        {
            foreach (var device in gone) _lastState.Remove(device);
            Prune(_staleWarned, live);
            Prune(_firstSeen, live);

            // broker 上还留着这台设备的 retained $state：不清掉，新订阅者会读到
            // 一台配置里已经不存在的设备的最后状态。
            if (sink is not null)
            {
                foreach (var device in gone)
                {
                    await sink.ForgetStateAsync(device, ct).ConfigureAwait(false);
                    _log.LogInformation("device {Device} was removed; its retained state is cleared", device);
                }
            }
        }
    }

    private static void Prune<T>(Dictionary<string, T> map, HashSet<string> live)
    {
        foreach (var key in map.Keys.Where(k => !live.Contains(k)).ToArray()) map.Remove(key);
    }

    private void CheckStale(DeviceActor actor, DateTimeOffset wallNow, long monoNow)
    {
        var samples = _engine.Cache.ForDevice(actor.DeviceId);
        if (samples.Count == 0)
        {
            _firstSeen.Remove(actor.DeviceId);
            return;
        }

        var newest = samples.Max(s => s.CollectedAt);

        // 阈值按设备内最慢的点位周期算：一台只挂慢变量的设备按设备周期
        // 判断会把完全正常的采集误报成停滞。
        var threshold = TimeSpan.FromMilliseconds(Math.Max(
            MinimumStaleAge.TotalMilliseconds,
            actor.SlowestInterval.TotalMilliseconds * 10));

        if (!_firstSeen.TryGetValue(actor.DeviceId, out var seen) || seen.Newest != newest)
        {
            // 新样本到了：重置观察起点。首次观测也从这里进，所以刚启动的进程
            // 不会拿「重启前的历史」判停滞——那段时间我们没有观察过。
            seen = (newest, monoNow);
            _firstSeen[actor.DeviceId] = seen;
        }

        var wallAge = wallNow - newest;
        var monoAge = Time.GetElapsedTime(seen.SeenAt, monoNow);

        // 两个时钟都要越过阈值才算停滞：墙钟说「很久没动」可能是刚被校时跳过去，
        // 单调时钟说「很久没动」才是真的没有新样本。
        if (wallAge <= threshold || monoAge <= threshold) return;

        if (_staleWarned.TryGetValue(actor.DeviceId, out var last) &&
            Time.GetElapsedTime(last, monoNow) < TimeSpan.FromMinutes(5)) return;

        _staleWarned[actor.DeviceId] = monoNow;
        _metrics.Counter("uic_watchdog_stale_total", 1, new Dictionary<string, string>
        {
            ["device"] = actor.DeviceId,
        });
        _log.LogWarning("device {Device} reports collecting but newest sample is {Age}s old",
            actor.DeviceId, (int)wallAge.TotalSeconds);
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

        _cts.Dispose();
    }
}
