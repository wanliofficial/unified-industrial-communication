using Uic.Domain;
using Uic.Driver;

namespace Uic.Engine;

/// <summary>
/// 数据加工：缩放、偏移、质量传播、死区。
/// 质量不可用时绝不参与死区比较——否则「坏值恰好等于上一个好值」会被误判为无变化。
/// </summary>
public static class Pipeline
{
    public static Sample Process(Device device, Point point, RawSample raw, ulong seq, DateTimeOffset now)
    {
        if (raw.Value is null || raw.Quality != Quality.Good)
        {
            return new Sample(
                device.Id,
                raw.PointId,
                raw.Value ?? DataValue.Number(point.DataType, 0),
                raw.Quality,
                point.Unit,
                raw.DeviceTime,
                now,
                seq,
                raw.Error);
        }

        var value = raw.Value;

        if (value.Type.IsNumeric() && (point.Scale != 1 || point.Offset != 0))
        {
            var scaled = value.AsNumber() * point.Scale + point.Offset;
            value = point.DataType switch
            {
                DataType.Float32 or DataType.Float64 => DataValue.Number(point.DataType, scaled),
                _ => DataValue.Number(point.DataType, Math.Round(scaled)),
            };
        }

        return new Sample(device.Id, raw.PointId, value, Quality.Good, point.Unit,
            raw.DeviceTime, now, seq);
    }

    /// <summary>死区判定。返回 true 表示「变化超过阈值，应当上报」。</summary>
    public static bool ExceedsDeadband(Sample? previous, Sample next, double? deadband)
        => ExceedsDeadband(previous, next, deadband, DeadbandMode.Absolute, null, null);

    /// <summary>按点位配置的死区方式判定。量程与模式缺一不可的情况在配置校验里已经拦住。</summary>
    public static bool ExceedsDeadband(Sample? previous, Sample next, Point point)
        => ExceedsDeadband(previous, next, point.Deadband, point.DeadbandMode,
            point.RangeMin, point.RangeMax);

    /// <summary>指定死区方式与量程的完整重载（百分比/变化率需要它们）。</summary>
    public static bool ExceedsDeadband(
        Sample? previous,
        Sample next,
        double? deadband,
        DeadbandMode mode,
        double? rangeMin = null,
        double? rangeMax = null)
    {
        if (previous is null) return true;

        // 质量变化必须上报，哪怕值没变——「good → timeout」本身就是事件。
        if (previous.Quality != next.Quality) return true;

        if (next.Quality != Quality.Good) return false;

        if (deadband is null or <= 0) return !SameValue(previous.Value, next.Value);

        if (!previous.Value.IsNumeric || !next.Value.IsNumeric)
            return !SameValue(previous.Value, next.Value);

        var delta = Math.Abs(next.Value.AsNumber() - previous.Value.AsNumber());

        return mode switch
        {
            // 百分比死区按量程归一：这样同一个「5%」在 0–100 和 0–10 的量程上
            // 含义一致，点表抄来抄去也不会变成另一种灵敏度。
            DeadbandMode.Percent when rangeMin is { } min && rangeMax is { } max && max > min
                => delta > (max - min) * deadband.Value / 100.0,

            // 变化率：Δ/Δt 与阈值比较。时间窗来自两个样本的采集时刻，
            // 不做滑动平均——那属于计算点，不属于死区。
            DeadbandMode.Rate => ExceedsRate(previous, next, delta, deadband.Value),

            _ => delta > deadband.Value,
        };
    }

    private static bool ExceedsRate(Sample previous, Sample next, double delta, double perSecond)
    {
        var seconds = (next.CollectedAt - previous.CollectedAt).TotalSeconds;

        // 时间没有前进（时钟回拨、同一时刻的两条样本）时算不出速率。
        // 宁可上报一条，也不要把变化吞掉。
        if (!(seconds > 0)) return true;

        return delta / seconds > perSecond;
    }

    private static bool SameValue(DataValue a, DataValue b)
    {
        if (a.Type != b.Type) return false;
        if (a.Num.HasValue || b.Num.HasValue)
        {
            if (!a.Num.HasValue || !b.Num.HasValue) return false;
            return Math.Abs(a.Num.Value - b.Num.Value) <= double.Epsilon * Math.Max(1, Math.Abs(a.Num.Value));
        }
        if (a.Bool.HasValue || b.Bool.HasValue) return a.Bool == b.Bool;
        return string.Equals(a.Text, b.Text, StringComparison.Ordinal);
    }
}