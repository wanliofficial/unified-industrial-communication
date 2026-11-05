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
    {
        if (previous is null) return true;

        // 质量变化必须上报，哪怕值没变——「good → timeout」本身就是事件。
        if (previous.Quality != next.Quality) return true;

        if (next.Quality != Quality.Good) return false;

        if (deadband is null or <= 0) return !SameValue(previous.Value, next.Value);

        if (!previous.Value.IsNumeric || !next.Value.IsNumeric)
            return !SameValue(previous.Value, next.Value);

        return Math.Abs(next.Value.AsNumber() - previous.Value.AsNumber()) > deadband.Value;
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