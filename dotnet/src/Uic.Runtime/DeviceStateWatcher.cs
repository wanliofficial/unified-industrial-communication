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
    private readonly Dictionary<string, DateTimeOffset> _staleWarned = new(StringComparer.Ordinal);
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

    private async Task TickAsync(CancellationToken ct)
    {
        var sink = _mqtt();
        var now = DateTimeOffset.UtcNow;

        foreach (var actor in _engine.Actors)
        {
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

            if (actor.State == RuntimeState.Collecting) CheckStale(actor, now);
        }
    }

    private void CheckStale(DeviceActor actor, DateTimeOffset now)
    {
        var samples = _engine.Cache.ForDevice(actor.DeviceId);
        if (samples.Count == 0) return;

        var newest = samples.Max(s => s.CollectedAt);
        var threshold = TimeSpan.FromMilliseconds(Math.Max(
            MinimumStaleAge.TotalMilliseconds,
            actor.Device.Interval.TotalMilliseconds * 10));

        if (now - newest <= threshold) return;

        if (_staleWarned.TryGetValue(actor.DeviceId, out var last) &&
            now - last < TimeSpan.FromMinutes(5)) return;

        _staleWarned[actor.DeviceId] = now;
        _metrics.Counter("uic_watchdog_stale_total", 1, new Dictionary<string, string>
        {
            ["device"] = actor.DeviceId,
        });
        _log.LogWarning("device {Device} reports collecting but newest sample is {Age}s old",
            actor.DeviceId, (int)(now - newest).TotalSeconds);
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
