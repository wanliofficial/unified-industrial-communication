using System.Collections.Concurrent;
using Uic.Domain;

namespace Uic.Engine;

/// <summary>当前值的唯一事实源。任何模块都通过它读写，不允许自留副本。</summary>
public sealed class PointCache
{
    private readonly ConcurrentDictionary<(string Device, string Point), Sample> _values = new();
    private readonly ConcurrentDictionary<(string Device, string Point), long> _seq = new();
    private readonly List<Action<Sample>> _subscribers = new();
    private readonly object _subLock = new();

    public Sample? Get(string deviceId, string pointId)
        => _values.TryGetValue((deviceId, pointId), out var s) ? s : null;

    public IReadOnlyList<Sample> All() => _values.Values.ToArray();

    public IReadOnlyList<Sample> ForDevice(string deviceId)
        => _values.Where(kv => kv.Key.Device == deviceId).Select(kv => kv.Value).ToArray();

    /// <summary>设备内单调递增序号。北向按 seq 判断「是否新样本」。</summary>
    public ulong NextSeq(string deviceId, string pointId)
        => (ulong)_seq.AddOrUpdate((deviceId, pointId), 1, (_, v) => v + 1);

    /// <summary>写入并返回「是否发生变化且应上报」。订阅者按顺序收到通知。</summary>
    public bool Publish(Sample sample, double? deadband)
    {
        var key = (sample.DeviceId, sample.PointId);
        var previous = _values.TryGetValue(key, out var p) ? p : null;

        var changed = Pipeline.ExceedsDeadband(previous, sample, deadband);
        _values[key] = sample;

        if (!changed) return false;

        Action<Sample>[] subs;
        lock (_subLock) subs = _subscribers.ToArray();

        foreach (var sub in subs)
        {
            try { sub(sample); }
            catch { /* 订阅者故障不得影响采集主链路 */ }
        }

        return true;
    }

    /// <summary>只更新值，不判定变化也不通知订阅者（用于内部状态修复）。</summary>
    public void Set(Sample sample) => _values[(sample.DeviceId, sample.PointId)] = sample;

    public IDisposable Subscribe(Action<Sample> handler)
    {
        lock (_subLock) _subscribers.Add(handler);
        return new Subscription(this, handler);
    }

    public void RemoveDevice(string deviceId)
    {
        foreach (var key in _values.Keys.Where(k => k.Device == deviceId).ToArray())
        {
            _values.TryRemove(key, out _);
            _seq.TryRemove(key, out _);
        }
    }

    private sealed class Subscription : IDisposable
    {
        private readonly PointCache _cache;
        private readonly Action<Sample> _handler;
        private bool _disposed;

        public Subscription(PointCache cache, Action<Sample> handler)
        {
            _cache = cache;
            _handler = handler;
        }

        public void Dispose()
        {
            if (_disposed) return;
            _disposed = true;
            lock (_cache._subLock) _cache._subscribers.Remove(_handler);
        }
    }
}