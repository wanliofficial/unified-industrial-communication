using System.Collections.Concurrent;

namespace Uic.Northbound;

/// <summary>
/// 令牌桶限流。按 key（IP / 操作类别）独立计数。
///
/// 边缘网关通常没有反代，这一层就是唯一的防打爆措施；实现刻意简单：
/// 固定窗口 + 突发额度，够用且不会在压力下变成新的故障点。
///
/// 桶必须能被清掉：key 是来源 IP，全网扫描器每天能贡献几十万个不同地址，
/// 每个地址一个永久桶就是无上限的内存增长——限流器本身变成拒绝服务的手段。
/// 因此按时间（不是按请求数）定期清扫空闲桶：请求量越大清扫越便宜，
/// 而空闲桶的判定用单调时钟，校时跳变不会把活跃桶误判成空闲、也不会让清扫永不触发。
/// </summary>
public sealed class RateLimiter
{
    /// <summary>清扫周期。按时间触发而不是「每 N 次请求」：桶多的时候按请求触发会把 O(桶数) 摊到每次调用上。</summary>
    private static readonly TimeSpan PruneInterval = TimeSpan.FromMinutes(1);

    /// <summary>多久没被访问就算空闲。必须远大于「桶自行回满」的时间（容量 / 补充速率 = 1 秒）。</summary>
    private static readonly TimeSpan DefaultIdleLifetime = TimeSpan.FromMinutes(10);

    private sealed class Bucket
    {
        public double Tokens;
        public long LastRefillTicks;
        public readonly object Lock = new();
    }

    private readonly ConcurrentDictionary<string, Bucket> _buckets = new(StringComparer.Ordinal);
    private readonly double _capacity;
    private readonly double _refillPerSecond;
    private readonly TimeProvider _time;
    private readonly TimeSpan _idleLifetime;
    private long _lastPruneTicks;

    /// <param name="time">时间来源。只用于「过了多久」的判定，因此必须是单调时钟。</param>
    /// <param name="idleLifetime">空闲多久后丢弃桶；默认 10 分钟。</param>
    public RateLimiter(double capacity, double refillPerSecond, TimeProvider? time = null,
        TimeSpan? idleLifetime = null)
    {
        _capacity = Math.Max(1, capacity);
        _refillPerSecond = Math.Max(0.01, refillPerSecond);
        _time = time ?? TimeProvider.System;
        _idleLifetime = idleLifetime ?? DefaultIdleLifetime;

        // 空闲判定必须比「空桶回满」长，否则会丢掉一个本该仍受限的桶，
        // 相当于替对方把配额立刻刷新了。
        var refillToFull = TimeSpan.FromSeconds(_capacity / _refillPerSecond);
        if (_idleLifetime <= refillToFull) _idleLifetime = refillToFull + refillToFull;

        _lastPruneTicks = _time.GetTimestamp();
    }

    /// <summary>当前未清扫的桶数。仅供测试观察内存是否被回收。</summary>
    internal int BucketCount => _buckets.Count;

    /// <summary>丢弃全部桶（仅供测试：静态限流器跨用例共享，配额不能带着上一个用例的余量进来）。</summary>
    internal void Clear()
    {
        _buckets.Clear();
        Interlocked.Exchange(ref _lastPruneTicks, _time.GetTimestamp());
    }

    public bool TryTake(string key, double cost = 1)
    {
        var now = _time.GetTimestamp();

        var bucket = _buckets.GetOrAdd(key, _ => new Bucket
        {
            Tokens = _capacity,
            LastRefillTicks = now,
        });

        bool allowed;

        lock (bucket.Lock)
        {
            var elapsedSeconds = _time.GetElapsedTime(bucket.LastRefillTicks, now).TotalSeconds;
            if (elapsedSeconds > 0)
            {
                bucket.Tokens = Math.Min(_capacity, bucket.Tokens + elapsedSeconds * _refillPerSecond);
                bucket.LastRefillTicks = now;
            }

            allowed = bucket.Tokens >= cost;
            if (allowed) bucket.Tokens -= cost;
        }

        // 清扫放在锁外：它要遍历所有桶并逐个加锁，拿着桶锁去扫很容易与另一条
        // 路径形成反向加锁顺序。
        MaybePrune(now);
        return allowed;
    }

    /// <summary>
    /// 至多每 <see cref="PruneInterval"/> 清扫一次。多个线程同时判定时只有一个真的去走字典，
    /// 其余直接放行——清扫慢一点没关系，不能让它成为并发下的锁竞争点。
    /// </summary>
    private void MaybePrune(long now)
    {
        var last = Interlocked.Read(ref _lastPruneTicks);
        if (_time.GetElapsedTime(last, now) < PruneInterval) return;
        if (Interlocked.CompareExchange(ref _lastPruneTicks, now, last) != last) return;

        Prune(_idleLifetime);
    }

    /// <summary>清理长时间未访问的桶，避免按 IP 建桶导致内存缓慢增长。</summary>
    public void Prune(TimeSpan idle)
    {
        var now = _time.GetTimestamp();

        foreach (var (key, bucket) in _buckets)
        {
            // 与 TryTake 用同一把锁读时间戳：LastRefillTicks 是普通字段，
            // 无锁读可能读到撕裂的中间值，把一个刚被用过的桶判定成空闲。
            bool idleNow;
            lock (bucket.Lock)
            {
                idleNow = _time.GetElapsedTime(bucket.LastRefillTicks, now) > idle;
            }

            if (idleNow) _buckets.TryRemove(key, out _);
        }
    }
}
