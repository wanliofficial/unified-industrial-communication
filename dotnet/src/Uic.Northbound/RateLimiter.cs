using System.Collections.Concurrent;

namespace Uic.Northbound;

/// <summary>
/// 令牌桶限流。按 key（IP / 操作类别）独立计数。
///
/// 边缘网关通常没有反代，这一层就是唯一的防打爆措施；实现刻意简单：
/// 固定窗口 + 突发额度，够用且不会在压力下变成新的故障点。
/// </summary>
public sealed class RateLimiter
{
    private sealed class Bucket
    {
        public double Tokens;
        public long LastRefillTicks;
        public readonly object Lock = new();
    }

    private readonly ConcurrentDictionary<string, Bucket> _buckets = new(StringComparer.Ordinal);
    private readonly double _capacity;
    private readonly double _refillPerSecond;

    public RateLimiter(double capacity, double refillPerSecond)
    {
        _capacity = Math.Max(1, capacity);
        _refillPerSecond = Math.Max(0.01, refillPerSecond);
    }

    public bool TryTake(string key, double cost = 1)
    {
        var now = System.Diagnostics.Stopwatch.GetTimestamp();
        var bucket = _buckets.GetOrAdd(key, _ => new Bucket
        {
            Tokens = _capacity,
            LastRefillTicks = now,
        });

        lock (bucket.Lock)
        {
            var elapsedSeconds = (now - bucket.LastRefillTicks) / (double)System.Diagnostics.Stopwatch.Frequency;
            if (elapsedSeconds > 0)
            {
                bucket.Tokens = Math.Min(_capacity, bucket.Tokens + elapsedSeconds * _refillPerSecond);
                bucket.LastRefillTicks = now;
            }

            if (bucket.Tokens < cost) return false;

            bucket.Tokens -= cost;
            return true;
        }
    }

    /// <summary>清理长时间未访问的桶，避免按 IP 建桶导致内存缓慢增长。</summary>
    public void Prune(TimeSpan idle)
    {
        var now = System.Diagnostics.Stopwatch.GetTimestamp();
        var idleTicks = (long)(idle.TotalSeconds * System.Diagnostics.Stopwatch.Frequency);

        foreach (var (key, bucket) in _buckets)
        {
            if (now - Interlocked.Read(ref bucket.LastRefillTicks) > idleTicks)
            {
                _buckets.TryRemove(key, out _);
            }
        }
    }
}