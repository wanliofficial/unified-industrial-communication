using Uic.Domain;
using Uic.Engine;
using Uic.Northbound;
using Uic.Obs;
using Uic.Outbox;
using OutboxQueue = Uic.Outbox.Outbox;

namespace Uic.Runtime;

/// <summary>
/// 采集结果 → 上报队列的绑定。
///
/// 只有 <see cref="PointCache"/> 判定「发生变化」的样本才会走到这里，
/// 死区过滤已经在缓存层完成；这一层只做序列化与分发，不做任何判断。
/// 序列化提前成字符串是因为队列要落盘：写盘时再序列化会把采集线程拖住。
/// </summary>
public sealed class SampleFanout : IDisposable
{
    private readonly PointCache _cache;
    private readonly OutboxQueue _outbox;
    private readonly MetricsRegistry _metrics;
    private string[] _targets;
    private IDisposable? _subscription;

    public SampleFanout(PointCache cache, OutboxQueue outbox, MetricsRegistry metrics, IEnumerable<string> targets)
    {
        _cache = cache;
        _outbox = outbox;
        _metrics = metrics;
        _targets = targets.Distinct(StringComparer.Ordinal).ToArray();
    }

    public IReadOnlyList<string> Targets => _targets;

    public void Start() => _subscription ??= _cache.Subscribe(Publish);

    /// <summary>热更新时切换目标集合。已排队的数据不会因为目标被移除而回滚。</summary>
    public void SetTargets(IEnumerable<string> targets)
        => Volatile.Write(ref _targets, targets.Distinct(StringComparer.Ordinal).ToArray());

    private void Publish(Sample sample)
    {
        var targets = Volatile.Read(ref _targets);
        if (targets.Length == 0) return;

        string payload;
        try
        {
            payload = ApiJson.Serialize(SampleDto.From(sample));
        }
        catch (Exception)
        {
            // 序列化失败是编码缺陷，不能让它把采集线程带崩；计数后跳过这一条。
            _metrics.Counter("uic_sample_encode_errors_total", 1);
            return;
        }

        foreach (var target in targets)
        {
            try
            {
                _outbox.Enqueue(target, payload);
            }
            catch (IOException)
            {
                // 磁盘满或权限错误：丢一条并计数，比让采集停下更合理。
                _metrics.Counter("uic_outbox_enqueue_errors_total",
                    1, new Dictionary<string, string> { ["sink"] = target });
            }
        }

        _metrics.Counter("uic_samples_published_total", 1, new Dictionary<string, string>
        {
            ["device"] = sample.DeviceId,
            ["quality"] = JsonEnum.ToWire(sample.Quality),
        });
    }

    public void Dispose() => _subscription?.Dispose();
}
