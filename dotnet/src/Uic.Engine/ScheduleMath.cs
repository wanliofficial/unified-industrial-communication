using System.Diagnostics;

namespace Uic.Engine;

/// <summary>
/// 调度用的单调时间换算。
///
/// 到期时间必须用 <see cref="Stopwatch"/> 时间戳：<see cref="DateTimeOffset.UtcNow"/>
/// 会被 NTP 校时或手工改钟拉动——往前跳会让设备白等一轮，往后跳则会在同一周期内
/// 连续采集多次。样本里的时间戳仍然用墙上时钟，两者职责不同，不能混用。
/// </summary>
public static class ScheduleMath
{
    /// <summary>把时长换算成 Stopwatch 时间戳单位；下限 1，避免算出一个「立刻又到期」的死循环。</summary>
    public static long ToTimestamp(TimeSpan duration)
    {
        var ticks = (long)Math.Round(duration.TotalSeconds * Stopwatch.Frequency);
        return ticks <= 0 ? 1 : ticks;
    }

    /// <summary>距离到期还有多久；已到期返回零。</summary>
    public static TimeSpan Remaining(long now, long due)
        => due <= now ? TimeSpan.Zero : Stopwatch.GetElapsedTime(now, due);

    /// <summary>
    /// 启动相位：把同周期设备按序号均匀铺开，避免 n 台设备在 t0 同时打满通道。
    /// 相位按设备自己的 interval 归一——100 台 1s 设备就是每 10ms 一台，
    /// 而不是把 100 台挤进一个 50ms tick 里。
    /// </summary>
    public static TimeSpan PhaseOffset(int index, int count, TimeSpan interval)
    {
        if (count <= 0 || interval <= TimeSpan.Zero) return TimeSpan.Zero;

        var slot = Math.Clamp(index, 0, count - 1);
        return TimeSpan.FromMilliseconds(interval.TotalMilliseconds * slot / count);
    }
}
