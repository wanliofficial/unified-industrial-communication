using System.Collections.Concurrent;
using System.Text.Json;
using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Engine;

/// <summary>
/// 调度器：管理每设备 actor 的生命周期与热更新。
/// 改一台设备只重启这一台，其余设备的采集不受影响。
/// </summary>
public sealed class Scheduler : IAsyncDisposable
{
    private readonly DriverRegistry _drivers;
    private readonly PointCache _cache;
    private readonly ILoggerFactory _logs;
    private readonly ILogger _log;
    private readonly SchedulerOptions _options;
    private readonly ConcurrentDictionary<string, DeviceActor> _actors = new(StringComparer.Ordinal);
    private readonly ConcurrentDictionary<string, ChannelGate> _gates = new(StringComparer.Ordinal);
    private readonly long _startedStamp;
    private readonly SemaphoreSlim _applyLock = new(1, 1);
    private ConfigDocument _config = new();
    private int _disposed;

    public Scheduler(
        DriverRegistry drivers,
        PointCache cache,
        ILoggerFactory logs,
        SchedulerOptions? options = null)
    {
        _drivers = drivers;
        _cache = cache;
        _logs = logs;
        _log = logs.CreateLogger("uic.scheduler");
        _options = options ?? new SchedulerOptions();
        _startedStamp = _options.Time.GetTimestamp();
    }

    public PointCache Cache => _cache;
    public ConfigDocument Config => _config;

    /// <summary>已注册的驱动。北向的 /drivers 端点直接读它，不另存一份元数据。</summary>
    public DriverRegistry Drivers => _drivers;

    /// <summary>
    /// 进程已运行时长。
    ///
    /// 用单调时钟算，不用 <c>UtcNow - 启动时刻</c>：NTP 校时往前跳一小时，
    /// 墙钟算法会让 uptime 瞬间变成 3600 秒；往后跳甚至可能算出负数。
    /// <c>/health</c> 里的 uptime 是排障时判断「是不是刚重启过」的依据，
    /// 它不能跟着系统时钟一起撒谎。
    /// </summary>
    public TimeSpan Uptime => _options.Time.GetElapsedTime(_startedStamp);

    public IReadOnlyList<DeviceActor> Actors => _actors.Values.OrderBy(a => a.DeviceId, StringComparer.Ordinal).ToArray();

    public DeviceActor? Find(string deviceId) => _actors.TryGetValue(deviceId, out var a) ? a : null;

    public Channel? ChannelOf(string deviceId)
        => _config.Devices.FirstOrDefault(d => d.Id == deviceId) is { } dev
            ? _config.Channels.FirstOrDefault(c => c.Id == dev.ChannelId)
            : null;

    public void Start(ConfigDocument config)
    {
        _config = config;

        foreach (var channel in config.Channels)
        {
            _gates.GetOrAdd(channel.Id, _ => new ChannelGate(channel.MaxConcurrency));
        }

        for (var i = 0; i < config.Devices.Count; i++)
        {
            var device = config.Devices[i];
            var actor = CreateActor(config, device, i, config.Devices.Count);
            _actors[device.Id] = actor;
            actor.Start();
        }

        _log.LogInformation("scheduler started: {Devices} device(s), {Channels} channel(s)",
            config.Devices.Count, config.Channels.Count);
    }

    /// <summary>应用新配置：按设备粒度 diff，只重启受影响的设备。</summary>
    public async Task<ApplyResult> ApplyAsync(ConfigDocument next)
    {
        await _applyLock.WaitAsync().ConfigureAwait(false);
        try
        {
            var added = new List<string>();
            var removed = new List<string>();
            var restarted = new List<string>();
            var unchanged = new List<string>();

            var before = _config;
            var beforeById = before.Devices.ToDictionary(d => d.Id, StringComparer.Ordinal);
            var nextById = next.Devices.ToDictionary(d => d.Id, StringComparer.Ordinal);

            // 下线的设备先停：先减后加，避免同一串口被新旧两个 actor 同时持有。
            foreach (var (id, actor) in _actors.ToArray())
            {
                if (nextById.ContainsKey(id)) continue;

                _actors.TryRemove(id, out _);
                await actor.DisposeAsync().ConfigureAwait(false);
                _cache.RemoveDevice(id);
                removed.Add(id);
            }

            foreach (var channel in next.Channels)
            {
                _gates.GetOrAdd(channel.Id, _ => new ChannelGate(channel.MaxConcurrency));
            }

            for (var i = 0; i < next.Devices.Count; i++)
            {
                var device = next.Devices[i];
                var changed = !beforeById.TryGetValue(device.Id, out var old) || !SameDevice(old, device);
                var channelChanged = ChannelChangedForDevice(before, next, device);

                if (!changed && !channelChanged)
                {
                    unchanged.Add(device.Id);
                    continue;
                }

                if (_actors.TryRemove(device.Id, out var existing))
                {
                    await existing.DisposeAsync().ConfigureAwait(false);
                    restarted.Add(device.Id);
                }
                else
                {
                    added.Add(device.Id);
                }

                var actor = CreateActor(next, device, i, next.Devices.Count);
                _actors[device.Id] = actor;
                actor.Start();
            }

            // 移除不再使用的通道闸门。
            var nextChannels = next.Channels.Select(c => c.Id).ToHashSet(StringComparer.Ordinal);
            foreach (var id in _gates.Keys.Where(k => !nextChannels.Contains(k)).ToArray())
            {
                if (_gates.TryRemove(id, out var gate)) gate.Dispose();
            }

            _config = next;

            _log.LogInformation(
                "config applied: +{Added} ~{Restarted} -{Removed} ={Unchanged}",
                added.Count, restarted.Count, removed.Count, unchanged.Count);

            return new ApplyResult(added, restarted, removed, unchanged);
        }
        finally
        {
            _applyLock.Release();
        }
    }

    private static bool ChannelChangedForDevice(ConfigDocument before, ConfigDocument next, Device device)
    {
        var oldDevice = before.Devices.FirstOrDefault(d => d.Id == device.Id);
        var oldChannelId = oldDevice?.ChannelId ?? device.ChannelId;

        var a = before.Channels.FirstOrDefault(c => c.Id == oldChannelId);
        var b = next.Channels.FirstOrDefault(c => c.Id == device.ChannelId);

        if (a is null || b is null) return true;
        return JsonSerializer.Serialize(a, JsonDefaults.Options) != JsonSerializer.Serialize(b, JsonDefaults.Options);
    }

    private DeviceActor CreateActor(ConfigDocument config, Device device, int index, int total)
    {
        var channel = config.Channels.FirstOrDefault(c => c.Id == device.ChannelId)
                      ?? throw new InvalidOperationException(
                          $"device '{device.Id}' references unknown channel '{device.ChannelId}'");

        if (!_drivers.TryGet(channel.Protocol, out var driver))
            throw new InvalidOperationException(
                $"protocol '{channel.Protocol}' is not registered for device '{device.Id}'");

        var gate = _gates.GetOrAdd(channel.Id, _ => new ChannelGate(channel.MaxConcurrency));
        var log = _logs.CreateLogger($"uic.device.{device.Id}");

        // 相位在 actor 启动时生效：新加/重启的设备也不会和老设备挤在同一个 tick。
        // 按设备内最快的周期铺开：若有 100ms 的点位，按 10s 的设备周期铺相会让它
        // 白等好几秒才出第一个值（而现场正是靠这个快值判断设备是否上线）。
        var phase = ScheduleMath.PhaseOffset(index, total, FastestInterval(device));

        return new DeviceActor(device, channel, driver, _cache, gate, log, _options, phase);
    }

    /// <summary>设备内实际最快的采集周期（点位覆盖优先）。</summary>
    private static TimeSpan FastestInterval(Device device)
    {
        var fastest = device.Interval;

        foreach (var point in device.Points)
        {
            if (!point.Enabled) continue;
            var effective = point.EffectiveInterval(device);
            if (effective > TimeSpan.Zero && effective < fastest) fastest = effective;
        }

        return fastest;
    }

    private static bool SameDevice(Device a, Device b)
        => JsonSerializer.Serialize(a, JsonDefaults.Options) == JsonSerializer.Serialize(b, JsonDefaults.Options);

    public async Task<IReadOnlyList<WriteResult>> WriteAsync(
        string deviceId,
        IReadOnlyList<WriteCommand> commands,
        CancellationToken ct)
    {
        var actor = Find(deviceId) ?? throw new KeyNotFoundException($"device '{deviceId}' not found");
        return await actor.WriteAsync(commands, ct).ConfigureAwait(false);
    }

    public async Task<bool> SelfTestAsync(string deviceId, CancellationToken ct)
    {
        var actor = Find(deviceId) ?? throw new KeyNotFoundException($"device '{deviceId}' not found");
        return await actor.SelfTestAsync(ct).ConfigureAwait(false);
    }

    /// <summary>设备视图列表。点位数直接来自缓存，不额外维护计数器。</summary>
    public IReadOnlyList<DeviceView> DeviceViews()
    {
        var views = new List<DeviceView>();

        foreach (var actor in Actors)
        {
            var samples = _cache.ForDevice(actor.DeviceId);
            var good = samples.Count(s => s.Quality == Quality.Good);
            var bad = samples.Count(s => s.Quality != Quality.Good);
            views.Add(actor.ToView(good, bad));
        }

        return views;
    }

    /// <summary>健康聚合。status 只有 ok / degraded / faulted 三态，便于告警规则直接匹配。</summary>
    public HealthView Health(long outboxPending, long outboxDropped)
    {
        var views = DeviceViews();
        var samples = _cache.All();

        var collecting = views.Count(v => v.RuntimeState == RuntimeState.Collecting);
        var degraded = views.Count(v => v.RuntimeState is RuntimeState.Degraded or RuntimeState.Connecting);
        var faulted = views.Count(v => v.RuntimeState == RuntimeState.Faulted);

        var status = faulted > 0 ? "faulted" : degraded > 0 ? "degraded" : "ok";

        return new HealthView(
            status,
            (long)Uptime.TotalSeconds,
            views.Count,
            collecting,
            degraded,
            faulted,
            samples.Count,
            samples.Count(s => s.Quality == Quality.Good),
            samples.Count(s => s.Quality == Quality.Stale),
            samples.Count(s => s.Quality is not (Quality.Good or Quality.Stale)),
            outboxPending,
            outboxDropped);
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        foreach (var actor in _actors.Values)
        {
            await actor.DisposeAsync().ConfigureAwait(false);
        }
        _actors.Clear();

        foreach (var gate in _gates.Values) gate.Dispose();
        _gates.Clear();

        _applyLock.Dispose();
    }
}

public sealed record ApplyResult(
    IReadOnlyList<string> Added,
    IReadOnlyList<string> Restarted,
    IReadOnlyList<string> Removed,
    IReadOnlyList<string> Unchanged)
{
    public bool AnyChange => Added.Count + Restarted.Count + Removed.Count > 0;
}