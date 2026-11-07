using Microsoft.Extensions.Logging;

namespace Uic.Obs;

/// <summary>资源水位告警的类型。两个水位可能同时触发，因此不能用布尔表达。</summary>
public enum ResourcePressure
{
    None,
    Disk,
    Memory,
    DiskAndMemory,
}

/// <summary>
/// 降级阈值。全部为 0 表示不检查——边缘设备的磁盘/内存差异太大，
/// 没有一个默认值能同时适用于 1GB 内存的盒子和机房虚拟机。
/// </summary>
public sealed record ResourceLimits
{
    /// <summary>剩余空间低于该值时进入降级；0 表示不检查磁盘。</summary>
    public long MinFreeBytes { get; init; }

    /// <summary>恢复到该值以上才解除降级；0 表示取 <see cref="MinFreeBytes"/> 的两倍。</summary>
    public long ResumeFreeBytes { get; init; }

    /// <summary>工作集超过该值时进入降级；0 表示不检查内存。</summary>
    public long MaxMemoryBytes { get; init; }

    /// <summary>检查间隔。检查本身只是两次系统调用，不需要更密。</summary>
    public TimeSpan Interval { get; init; } = TimeSpan.FromSeconds(10);

    /// <summary>
    /// 把配置转成阈值。恢复线默认是进入线的两倍：没有回差会在阈值附近反复
    /// 「降级→恢复→降级」，日志和指标都会被刷屏。
    /// </summary>
    public static ResourceLimits FromStorage(Uic.Domain.StorageOptions storage) => new()
    {
        MinFreeBytes = storage.MinFreeBytes,
        ResumeFreeBytes = storage.ResumeFreeBytes > 0
            ? storage.ResumeFreeBytes
            : SafeDouble(storage.MinFreeBytes),
        MaxMemoryBytes = storage.MaxMemoryBytes,
        Interval = storage.ResourceCheckInterval,
    };

    public bool Enabled => MinFreeBytes > 0 || MaxMemoryBytes > 0;

    private static long SafeDouble(long value)
        => value <= 0 || value > long.MaxValue / 2 ? (value > 0 ? long.MaxValue : 0) : value * 2;
}

/// <summary>一次资源采样。读不到的项用 null 表示，和「读到 0」必须区分开。</summary>
public sealed record ResourceReading(
    long? FreeBytes,
    long? DiskTotalBytes,
    long? WorkingSetBytes,
    DateTimeOffset At);

/// <summary>当前资源判定结果。Since 是这一次进入降级的时间，恢复后保持不变直到下一次进入。</summary>
public sealed record ResourceStatus(
    bool DiskLow,
    bool MemoryHigh,
    ResourceReading Reading,
    DateTimeOffset Since)
{
    public ResourcePressure Pressure => (DiskLow, MemoryHigh) switch
    {
        (true, true) => ResourcePressure.DiskAndMemory,
        (true, false) => ResourcePressure.Disk,
        (false, true) => ResourcePressure.Memory,
        _ => ResourcePressure.None,
    };

    public bool Degraded => DiskLow || MemoryHigh;
}

/// <summary>北向层只需要读状态，不参与判定。</summary>
public interface IResourceStatusSource
{
    ResourceStatus Status { get; }
}

/// <summary>
/// 资源水位看门狗：数据目录所在分区剩余空间或进程工作集低于阈值时进入降级，
/// 回到恢复线以上后自动解除。
///
/// 为什么用「进入线/恢复线」两条线：单阈值会在边界抖动，每次抖动都要停/开
/// 文件日志与历史通道，产生的日志比要防的问题还多。
///
/// 为什么探针可注入：磁盘满与内存超限在自测里没法稳定制造，
/// 而这段判定逻辑恰恰是最容易写错的部分（null、回差、同时触发）。
/// </summary>
public sealed class ResourceMonitor : IResourceStatusSource, IAsyncDisposable
{
    private readonly Func<ResourceReading> _probe;
    private readonly ILogger _log;
    private readonly MetricsRegistry? _metrics;
    private readonly object _gate = new();
    private readonly CancellationTokenSource _cts = new();

    private ResourceLimits _limits;
    private ResourceStatus _status;
    private Task? _loop;
    private int _disposed;

    public ResourceMonitor(
        Func<ResourceReading> probe,
        ILogger log,
        MetricsRegistry? metrics = null,
        ResourceLimits? limits = null)
    {
        _probe = probe;
        _log = log;
        _metrics = metrics;
        _limits = limits ?? new ResourceLimits();

        var at = DateTimeOffset.UtcNow;
        _status = new ResourceStatus(false, false, new ResourceReading(null, null, null, at), at);
    }

    /// <summary>状态变化时触发，只在进入/解除降级时各触发一次。</summary>
    public event Action<ResourceStatus>? Changed;

    public ResourceLimits Limits
    {
        get { lock (_gate) return _limits; }
    }

    public ResourceStatus Status
    {
        get { lock (_gate) return _status; }
    }

    /// <summary>运行期换阈值（配置热更新）。新阈值立刻生效，不必等下一个检查周期。</summary>
    public void Configure(ResourceLimits limits)
    {
        lock (_gate)
        {
            if (_disposed != 0) return;
            _limits = limits;
        }

        Check();
        EnsureLoop();
    }

    public void Start()
    {
        if (_disposed != 0) return;
        Check();
        EnsureLoop();
    }

    private void EnsureLoop()
    {
        if (_disposed != 0 || _loop is not null || !Limits.Enabled) return;

        lock (_gate)
        {
            if (_disposed != 0 || _loop is not null || !_limits.Enabled) return;
            _loop = Task.Run(() => LoopAsync(_cts.Token));
        }
    }

    private async Task LoopAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            TimeSpan delay;
            lock (_gate) delay = _limits.Interval;

            try { await Task.Delay(delay, ct).ConfigureAwait(false); }
            catch (OperationCanceledException) { return; }

            try { Check(); }
            catch (Exception ex)
            {
                // 判定逻辑或执行动作出错不能终止看门狗：下一轮还要继续判定。
                _log.LogWarning(ex, "resource check failed");
            }
        }
    }

    /// <summary>执行一次判定。自测直接调用它，不必等定时器。</summary>
    public ResourceStatus Check()
    {
        ResourceStatus status;
        var changed = false;
        var previous = ResourcePressure.None;

        lock (_gate)
        {
            if (_disposed != 0) return _status;

            var limits = _limits;
            var reading = Read();
            previous = _status.Pressure;

            // 已经进入降级时只看「是否回到恢复线」，未进入时才看「是否低于进入线」；
            // 单阈值会在边界反复跳，每次跳都要停/开文件日志。
            var diskLow = _status.DiskLow;
            if (limits.MinFreeBytes > 0 && reading.FreeBytes is long free)
            {
                diskLow = diskLow ? free < limits.ResumeFreeBytes : free < limits.MinFreeBytes;
            }
            else if (limits.MinFreeBytes == 0)
            {
                diskLow = false;
            }

            var memoryHigh = _status.MemoryHigh;
            if (limits.MaxMemoryBytes > 0 && reading.WorkingSetBytes is long used)
            {
                // 内存同样留 10% 回差：释放后立刻再判定，避免在阈值上反复抖动。
                var resume = limits.MaxMemoryBytes - limits.MaxMemoryBytes / 10;
                memoryHigh = memoryHigh ? used > resume : used > limits.MaxMemoryBytes;
            }
            else if (limits.MaxMemoryBytes == 0)
            {
                memoryHigh = false;
            }

            changed = diskLow != _status.DiskLow || memoryHigh != _status.MemoryHigh;
            var since = changed ? reading.At : _status.Since;

            status = new ResourceStatus(diskLow, memoryHigh, reading, since);
            _status = status;
        }

        if (!changed) return status;

        Record(status, previous);

        var handler = Changed;
        if (handler is null) return status;

        try { handler(status); }
        catch (Exception ex)
        {
            // 降级动作失败（比如文件正在被占用）不能反过来把看门狗带停。
            _log.LogError(ex, "resource pressure handler failed");
        }

        return status;
    }

    private ResourceReading Read()
    {
        try { return _probe(); }
        catch (Exception ex)
        {
            // 拿不到信息时按「未知」处理，而不是按「告急」处理：
            // 容器里 DriveInfo 可能不可用，误报会造成不必要的停写。
            _log.LogDebug(ex, "resource probe failed");
            return new ResourceReading(null, null, null, DateTimeOffset.UtcNow);
        }
    }

    private void Record(ResourceStatus status, ResourcePressure previous)
    {
        var reading = status.Reading;

        if (status.Degraded)
        {
            _metrics?.Increment("uic_resource_pressure_total",
                new Dictionary<string, string> { ["kind"] = status.Pressure.ToString().ToLowerInvariant() });

            if (status.DiskLow)
            {
                _log.LogWarning(
                    "data directory free space {Free} bytes is below the watermark; history and file logging are paused",
                    reading.FreeBytes);
            }

            if (status.MemoryHigh)
            {
                _log.LogWarning(
                    "working set {Used} bytes exceeds the limit; health is degraded",
                    reading.WorkingSetBytes);
            }
        }
        else
        {
            _log.LogInformation(
                "resource pressure {Previous} cleared: free {Free} bytes, working set {Used} bytes",
                previous.ToString().ToLowerInvariant(), reading.FreeBytes, reading.WorkingSetBytes);
        }
    }

    /// <summary>默认探针：数据目录所在分区的可用空间 + 当前进程工作集。</summary>
    public static ResourceReading Sample(string dataDir)
    {
        long? free = null;
        long? total = null;

        try
        {
            var full = Path.GetFullPath(dataDir);
            var root = Path.GetPathRoot(full);
            if (!string.IsNullOrEmpty(root))
            {
                var drive = new DriveInfo(root);
                if (drive.IsReady)
                {
                    free = drive.AvailableFreeSpace;
                    total = drive.TotalSize;
                }
            }
        }
        catch (Exception)
        {
            // 平台可能不提供磁盘信息（或目录被删/被挂载覆盖）；交给调用方按未知处理。
        }

        return new ResourceReading(free, total, Environment.WorkingSet, DateTimeOffset.UtcNow);
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
