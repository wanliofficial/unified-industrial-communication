using System.Collections.Concurrent;
using System.Globalization;
using System.Text;
using Uic.Driver;

namespace Uic.Obs;

/// <summary>
/// 指标注册表。
///
/// 只做三件事：计数器、分布（固定桶直方图）、可回调的瞬时值。
/// 刻意不引入第三方指标库——边缘设备上，少一个依赖就少一类启动失败。
/// </summary>
public sealed class MetricsRegistry : IMetrics
{
    private static readonly double[] DefaultBuckets =
    {
        0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
    };

    private readonly ConcurrentDictionary<string, CounterSeries> _counters = new(StringComparer.Ordinal);
    private readonly ConcurrentDictionary<string, HistogramSeries> _histograms = new(StringComparer.Ordinal);
    private readonly ConcurrentDictionary<string, Func<double>> _gauges = new(StringComparer.Ordinal);
    private readonly ConcurrentDictionary<string, string> _help = new(StringComparer.Ordinal);

    public void Counter(string name, double delta, IReadOnlyDictionary<string, string>? labels = null)
        => GetCounter(name, labels).Add(delta);

    public void Increment(string name, IReadOnlyDictionary<string, string>? labels = null)
        => Counter(name, 1, labels);

    public void Observe(string name, double value, IReadOnlyDictionary<string, string>? labels = null)
        => GetHistogram(name, labels, DefaultBuckets).Observe(value);

    /// <summary>注册瞬时值。同名覆盖，便于热更新时替换。</summary>
    public void Gauge(string name, Func<double> read, string? help = null)
    {
        _gauges[name] = read;
        if (help is not null) _help[name] = help;
    }

    public void Describe(string name, string help) => _help[name] = help;

    public double CounterValue(string name, IReadOnlyDictionary<string, string>? labels = null)
        => _counters.TryGetValue(Key(name, labels), out var c) ? c.Value : 0;

    private CounterSeries GetCounter(string name, IReadOnlyDictionary<string, string>? labels)
        => _counters.GetOrAdd(Key(name, labels), _ => new CounterSeries(name, labels));

    private HistogramSeries GetHistogram(string name, IReadOnlyDictionary<string, string>? labels, double[] buckets)
        => _histograms.GetOrAdd(Key(name, labels), _ => new HistogramSeries(name, labels, buckets));

    /// <summary>渲染 Prometheus 文本格式。名称与标签在注册时已规范，输出无需再转义。</summary>
    public string RenderPrometheus()
    {
        var sb = new StringBuilder(4096);
        var emitted = new HashSet<string>(StringComparer.Ordinal);

        foreach (var counter in _counters.Values.OrderBy(c => c.Name, StringComparer.Ordinal))
        {
            if (emitted.Add(counter.Name))
            {
                WriteHelp(sb, counter.Name, "counter");
            }
            sb.Append(counter.Name);
            AppendLabels(sb, counter.Labels);
            sb.Append(' ').Append(Format(counter.Value)).Append('\n');
        }

        foreach (var histogram in _histograms.Values.OrderBy(h => h.Name, StringComparer.Ordinal))
        {
            if (emitted.Add(histogram.Name))
            {
                WriteHelp(sb, histogram.Name, "histogram");
            }

            var snapshot = histogram.Snapshot();

            foreach (var (bound, cumulative) in snapshot.Buckets)
            {
                sb.Append(histogram.Name).Append("_bucket");
                AppendLabels(sb, histogram.Labels, ("le", Format(bound)));
                sb.Append(' ').Append(cumulative.ToString(CultureInfo.InvariantCulture)).Append('\n');
            }

            sb.Append(histogram.Name).Append("_bucket");
            AppendLabels(sb, histogram.Labels, ("le", "+Inf"));
            sb.Append(' ').Append(snapshot.Count.ToString(CultureInfo.InvariantCulture)).Append('\n');

            sb.Append(histogram.Name).Append("_sum");
            AppendLabels(sb, histogram.Labels);
            sb.Append(' ').Append(Format(snapshot.Sum)).Append('\n');

            sb.Append(histogram.Name).Append("_count");
            AppendLabels(sb, histogram.Labels);
            sb.Append(' ').Append(snapshot.Count.ToString(CultureInfo.InvariantCulture)).Append('\n');
        }

        foreach (var (name, read) in _gauges.OrderBy(g => g.Key, StringComparer.Ordinal))
        {
            double value;
            try { value = read(); }
            catch { value = double.NaN; }

            if (emitted.Add(name)) WriteHelp(sb, name, "gauge");
            sb.Append(name).Append(' ').Append(Format(value)).Append('\n');
        }

        return sb.ToString();
    }

    private void WriteHelp(StringBuilder sb, string name, string type)
    {
        if (_help.TryGetValue(name, out var help))
        {
            sb.Append("# HELP ").Append(name).Append(' ').Append(help).Append('\n');
        }
        sb.Append("# TYPE ").Append(name).Append(' ').Append(type).Append('\n');
    }

    private static void AppendLabels(StringBuilder sb, IReadOnlyDictionary<string, string>? labels, params (string Key, string Value)[] extra)
    {
        if ((labels is null || labels.Count == 0) && extra.Length == 0) return;

        sb.Append('{');
        var first = true;

        if (labels is not null)
        {
            foreach (var (k, v) in labels.OrderBy(x => x.Key, StringComparer.Ordinal))
            {
                if (!first) sb.Append(',');
                first = false;
                sb.Append(k).Append("=\"").Append(Escape(v)).Append('"');
            }
        }

        foreach (var (k, v) in extra)
        {
            if (!first) sb.Append(',');
            first = false;
            sb.Append(k).Append("=\"").Append(Escape(v)).Append('"');
        }

        sb.Append('}');
    }

    private static string Escape(string v) => v.Replace("\\", "\\\\", StringComparison.Ordinal)
        .Replace("\"", "\\\"", StringComparison.Ordinal)
        .Replace("\n", "\\n", StringComparison.Ordinal);

    private static string Format(double v) =>
        double.IsNaN(v) || double.IsInfinity(v) ? "0" : v.ToString("R", CultureInfo.InvariantCulture);

    /// <summary>标签集合的稳定键：排序后拼接，保证同一组标签只产生一个时间序列。</summary>
    private static string Key(string name, IReadOnlyDictionary<string, string>? labels)
    {
        if (labels is null || labels.Count == 0) return name;

        var sb = new StringBuilder(name);
        foreach (var (k, v) in labels.OrderBy(x => x.Key, StringComparer.Ordinal))
        {
            sb.Append('\u001f').Append(k).Append('=').Append(v);
        }
        return sb.ToString();
    }

    private sealed class CounterSeries
    {
        private double _value;

        public CounterSeries(string name, IReadOnlyDictionary<string, string>? labels)
        {
            Name = name;
            Labels = labels;
        }

        public string Name { get; }
        public IReadOnlyDictionary<string, string>? Labels { get; }

        public double Value => Volatile.Read(ref _value);

        public void Add(double delta)
        {
            double initial, computed;
            do
            {
                initial = Volatile.Read(ref _value);
                computed = initial + delta;
            } while (Interlocked.CompareExchange(ref _value, computed, initial) != initial);
        }
    }

    private sealed class HistogramSeries
    {
        private readonly double[] _buckets;
        private readonly long[] _counts;
        private long _sumBits;
        private long _count;

        public HistogramSeries(string name, IReadOnlyDictionary<string, string>? labels, double[] buckets)
        {
            Name = name;
            Labels = labels;
            _buckets = buckets;
            _counts = new long[buckets.Length + 1];
        }

        public string Name { get; }
        public IReadOnlyDictionary<string, string>? Labels { get; }

        public void Observe(double value)
        {
            var idx = Array.FindIndex(_buckets, b => value <= b);
            if (idx < 0) idx = _buckets.Length;

            Interlocked.Increment(ref _counts[idx]);
            Interlocked.Increment(ref _count);

            // double 累加用 CAS 保证不丢；直方图基数在整个进程内是常数。
            while (true)
            {
                var current = Interlocked.Read(ref _sumBits);
                var next = BitConverter.DoubleToInt64Bits(BitConverter.Int64BitsToDouble(current) + value);
                if (Interlocked.CompareExchange(ref _sumBits, next, current) == current) break;
            }
        }

        public (List<(double Bound, long Cumulative)> Buckets, double Sum, long Count) Snapshot()
        {
            var buckets = new List<(double, long)>(_buckets.Length);
            long cumulative = 0;

            for (var i = 0; i < _buckets.Length; i++)
            {
                cumulative += Interlocked.Read(ref _counts[i]);
                buckets.Add((_buckets[i], cumulative));
            }

            // +Inf 桶由 Count 表达，不在这里重复累加。
            var sum = BitConverter.Int64BitsToDouble(Interlocked.Read(ref _sumBits));
            return (buckets, sum, Interlocked.Read(ref _count));
        }
    }
}