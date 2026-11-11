using Uic.Domain;
using Uic.Driver;

namespace Uic.Engine;

/// <summary>
/// 数据加工：缩放、偏移、质量传播、死区。
/// 质量不可用时绝不参与死区比较——否则「坏值恰好等于上一个好值」会被误判为无变化。
/// </summary>
public static class Pipeline
{
    public static Sample Process(Device device, Point point, RawSample raw, ulong seq, DateTimeOffset now, long? monoAt = null)
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
                raw.Error,
                monoAt);
        }

        var value = raw.Value;

        if (value.Type.IsNumeric() && (point.Scale != 1 || point.Offset != 0))
        {
            // 缩放后的类型是**点位声明的类型**，不是原始线上类型：
            // 现场常见「uint16 原始码 × 0.1 = 工程量浮点」，结果必须按 float 报出去。
            value = value.Array is { } array
                ? DataValue.Numbers(point.DataType, array.Select(v => Scale(point, v)).ToArray())
                : DataValue.Number(point.DataType, Scale(point, value.AsNumber()));
        }

        return new Sample(device.Id, raw.PointId, value, Quality.Good, point.Unit,
            raw.DeviceTime, now, seq, null, monoAt);
    }

    /// <summary>
    /// 缩放：整型点位按四舍五入落回整数，浮点保留小数。
    ///
    /// 显式用 <see cref="MidpointRounding.AwayFromZero"/>：.NET 默认的银行家舍入会把
    /// 4.5 变成 4、5.5 变成 6，与现场「四舍五入」的直觉不一致，
    /// 而这类差异在趋势图上表现为「偶尔差 1 个刻度」，几乎不可能被追查出来。
    /// </summary>
    private static double Scale(Point point, double raw)
    {
        var scaled = raw * point.Scale + point.Offset;
        return point.DataType is DataType.Float32 or DataType.Float64
            ? scaled
            : Math.Round(scaled, MidpointRounding.AwayFromZero);
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

        // 数组按「元素逐个比、取最大变化」判定：任何一个元素越过阈值就上报。
        // 用最大变化而不是平均值，是因为平均值会把「一个通道跳变、其余不变」
        // 稀释成没变化——那正是最该被看到的情况。
        double delta;
        if (previous.Value.IsArray || next.Value.IsArray)
        {
            if (!previous.Value.IsArray || !next.Value.IsArray ||
                previous.Value.Count != next.Value.Count)
            {
                return true;
            }

            delta = 0;
            for (var i = 0; i < previous.Value.Count; i++)
            {
                delta = Math.Max(delta,
                    Math.Abs(next.Value.NumberAt(i) - previous.Value.NumberAt(i)));
            }
        }
        else
        {
            delta = Math.Abs(next.Value.AsNumber() - previous.Value.AsNumber());
        }

        return Exceeds(delta, deadband.Value, mode, rangeMin, rangeMax, previous, next);
    }

    /// <summary>三种死区模式的统一判定。数组与标量共用，避免两套阈值语义。</summary>
    private static bool Exceeds(
        double delta,
        double deadband,
        DeadbandMode mode,
        double? rangeMin,
        double? rangeMax,
        Sample previous,
        Sample next)
    {
        switch (mode)
        {
            // 百分比死区按量程归一：这样同一个「5%」在 0–100 和 0–10 的量程上
            // 含义一致，点表抄来抄去也不会变成另一种灵敏度。
            case DeadbandMode.Percent when rangeMin is { } min && rangeMax is { } max && max > min:
                return delta > (max - min) * deadband / 100.0;

            // 变化率：Δ/Δt 与阈值比较。时间窗来自两个样本的采集时刻，
            // 不做滑动平均——那属于计算点，不属于死区。
            case DeadbandMode.Rate:
                return ExceedsRate(previous, next, delta, deadband);

            default:
                return delta > deadband;
        }
    }

    private static bool ExceedsRate(Sample previous, Sample next, double delta, double perSecond)
    {
        // Δt 优先用单调时刻：墙钟会被 NTP 校时拨动，rate 死区用 CollectedAt 算速率，
        // 一次前跳会把 1 秒的涨幅压成 1 小时（尖峰被抹平），后跳则把 1 小时压成 1 秒（凭空造峰）。
        // 两条样本都带单调戳时才用它；否则回退墙钟，保持「构造样本没填 MonoAt」的旧语义。
        // MonoAt 已归一化到 TimeSpan.Ticks，因此这里除的是常量，与具体 TimeProvider 频率无关。
        double seconds;
        if (previous.MonoAt is { } prevMono && next.MonoAt is { } nextMono)
            seconds = (nextMono - prevMono) / (double)TimeSpan.TicksPerSecond;
        else
            seconds = (next.CollectedAt - previous.CollectedAt).TotalSeconds;

        // 时间没有前进（时钟回拨、同一时刻的两条样本）时算不出速率。
        // 宁可上报一条，也不要把变化吞掉。
        if (!(seconds > 0)) return true;

        return delta / seconds > perSecond;
    }

    private static bool SameValue(DataValue a, DataValue b)
    {
        if (a.Type != b.Type) return false;

        if (a.Array is not null || b.Array is not null)
        {
            if (a.Array is null || b.Array is null || a.Array.Count != b.Array.Count) return false;

            for (var i = 0; i < a.Array.Count; i++)
            {
                if (Math.Abs(a.Array[i] - b.Array[i]) >
                    double.Epsilon * Math.Max(1, Math.Abs(a.Array[i])))
                {
                    return false;
                }
            }

            return true;
        }

        if (a.Num.HasValue || b.Num.HasValue)
        {
            if (!a.Num.HasValue || !b.Num.HasValue) return false;
            return Math.Abs(a.Num.Value - b.Num.Value) <= double.Epsilon * Math.Max(1, Math.Abs(a.Num.Value));
        }
        if (a.Bool.HasValue || b.Bool.HasValue) return a.Bool == b.Bool;
        return string.Equals(a.Text, b.Text, StringComparison.Ordinal);
    }
}