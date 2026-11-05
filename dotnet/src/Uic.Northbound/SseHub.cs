using System.Collections.Concurrent;
using System.Text.Json;
using System.Threading.Channels;
using Uic.Domain;

namespace Uic.Northbound;

public sealed record SseEvent(string Event, string Data, string? Id = null);

/// <summary>
/// SSE 扇出中心。
///
/// 三条硬约束：
/// - 每个连接一个有界队列，慢了就丢最旧（<see cref="BoundedChannelFullMode.DropOldest"/>），
///   绝不能因为一个慢客户端把采集主链路堵住；
/// - 值没变就不推（死区在上游已经做过，这里只做质量变化）；
/// - 每条事件带 <c>id: device:point:seq</c>，客户端用它去重；服务端不重放历史，
///   重连后由客户端先拉一次点位快照再吃增量。
/// </summary>
public sealed class SseHub
{
    private const int DefaultBufferPerConnection = 1000;

    private readonly List<Subscriber> _subscribers = new();
    private readonly ConcurrentDictionary<(string Device, string Point), Quality> _lastQuality = new();
    private readonly object _lock = new();

    public int ConnectionCount
    {
        get { lock (_lock) return _subscribers.Count; }
    }

    /// <summary>订阅。patterns 支持 <c>device/*</c> 与 <c>*/point</c>，空表示全部。</summary>
    public Subscription Subscribe(IReadOnlyList<string> patterns, string? qualityFilter, int heartbeatSeconds)
    {
        var channel = System.Threading.Channels.Channel.CreateBounded<SseEvent>(new BoundedChannelOptions(DefaultBufferPerConnection)
        {
            FullMode = BoundedChannelFullMode.DropOldest,
            SingleReader = true,
            SingleWriter = false,
        });

        var subscriber = new Subscriber(channel, patterns, qualityFilter, heartbeatSeconds);
        lock (_lock) _subscribers.Add(subscriber);

        return new Subscription(this, subscriber);
    }

    /// <summary>由采集链路调用。异常一律吞掉：推送失败不得影响采集。</summary>
    public void Publish(Sample sample)
    {
        Subscriber[] subs;
        lock (_lock) subs = _subscribers.ToArray();
        if (subs.Length == 0) return;

        var quality = JsonEnum.ToWire(sample.Quality);

        if (_lastQuality.TryGetValue((sample.DeviceId, sample.PointId), out var previous) && previous != sample.Quality)
        {
            var change = new SseEvent(
                "quality",
                JsonSerializer.Serialize(new
                {
                    device = sample.DeviceId,
                    point = sample.PointId,
                    from = JsonEnum.ToWire(previous),
                    to = quality,
                    at = sample.CollectedAt,
                }, ApiJson.Options),
                $"{sample.DeviceId}:{sample.PointId}:{sample.Seq}");

            foreach (var sub in subs)
            {
                if (sub.Matches(sample.DeviceId, sample.PointId) && sub.AllowsQuality(quality))
                {
                    sub.Queue.Writer.TryWrite(change);
                }
            }
        }

        _lastQuality[(sample.DeviceId, sample.PointId)] = sample.Quality;

        var payload = new SseEvent(
            "sample",
            JsonSerializer.Serialize(SampleDto.From(sample), ApiJson.Options),
            $"{sample.DeviceId}:{sample.PointId}:{sample.Seq}");

        foreach (var sub in subs)
        {
            if (sub.Matches(sample.DeviceId, sample.PointId) && sub.AllowsQuality(quality))
            {
                sub.Queue.Writer.TryWrite(payload);
            }
        }
    }

    private void Remove(Subscriber subscriber)
    {
        lock (_lock) _subscribers.Remove(subscriber);
    }

    public sealed class Subscription : IDisposable
    {
        private readonly SseHub _hub;
        private readonly Subscriber _subscriber;
        private int _disposed;

        internal Subscription(SseHub hub, Subscriber subscriber)
        {
            _hub = hub;
            _subscriber = subscriber;
        }

        public ChannelReader<SseEvent> Reader => _subscriber.Queue.Reader;
        public int HeartbeatSeconds => _subscriber.HeartbeatSeconds;

        public void Dispose()
        {
            if (Interlocked.Exchange(ref _disposed, 1) != 0) return;
            _subscriber.Queue.Writer.TryComplete();
            _hub.Remove(_subscriber);
        }
    }

    internal sealed class Subscriber
    {
        private readonly IReadOnlyList<string> _patterns;
        private readonly string? _quality;

        public Subscriber(
            System.Threading.Channels.Channel<SseEvent> queue,
            IReadOnlyList<string> patterns,
            string? quality,
            int heartbeatSeconds)
        {
            Queue = queue;
            _patterns = patterns;
            _quality = quality;
            HeartbeatSeconds = heartbeatSeconds;
        }

        public System.Threading.Channels.Channel<SseEvent> Queue { get; }
        public int HeartbeatSeconds { get; }

        public bool AllowsQuality(string quality)
            => _quality is null || string.Equals(_quality, quality, StringComparison.OrdinalIgnoreCase);

        public bool Matches(string device, string point)
        {
            if (_patterns.Count == 0) return true;

            foreach (var pattern in _patterns)
            {
                var slash = pattern.IndexOf('/');
                if (slash < 0)
                {
                    if (string.Equals(pattern, device, StringComparison.Ordinal)) return true;
                    continue;
                }

                var dp = pattern[..slash];
                var pp = pattern[(slash + 1)..];
                var deviceOk = dp is "*" || string.Equals(dp, device, StringComparison.Ordinal);
                var pointOk = pp is "*" || string.Equals(pp, point, StringComparison.Ordinal);

                if (deviceOk && pointOk) return true;
            }

            return false;
        }
    }
}