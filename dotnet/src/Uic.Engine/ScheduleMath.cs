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

    /// <summary>
    /// 失败退避：按最快周期的 2^n 增长，上限取恢复间隔；再叠加最多 10% 的正向抖动。
    ///
    /// 为什么是纯函数：这条公式是「设备彻底离线后多久试一次」的唯一决定因素，
    /// 却被熔在 actor 里，只能靠「等两次重连的间隔」间接猜——那个量既受机器负载
    /// 影响，又要跑上好几秒才能观察一档。抽成纯函数后，2^n 增长、上限封顶、
    /// 抖动落在 [0, 10%] 三个性质都能直接断言，而不必去赌时序。
    ///
    /// 抖动只加不减：退避是「最少等这么久」，减抖动等于允许比约定更早去打一台
    /// 已经出问题的设备；正向抖动只是把同时失败的设备错开，避免整批同时重连。
    /// </summary>
    /// <param name="failures">连续失败次数（已按上限 6 档截断，避免 2^n 溢出）。</param>
    /// <param name="baseInterval">基数：设备/最快点位的采集周期。</param>
    /// <param name="recoverInterval">上限：退避不会超过它（与基数取较大者，防止基数大于上限时退避反而变短）。</param>
    /// <param name="jitterSample">[0,1) 的随机采样；传 0 表示不要抖动（便于断言基数）。</param>
    public static TimeSpan Backoff(int failures, TimeSpan baseInterval, TimeSpan recoverInterval, double jitterSample)
    {
        var baseMs = baseInterval.TotalMilliseconds;
        if (baseMs <= 0) return TimeSpan.Zero;

        var ceiling = Math.Max(baseMs, recoverInterval.TotalMilliseconds);
        var scaled = baseMs * Math.Pow(2, Math.Clamp(failures, 0, 6));
        var backoffMs = Math.Min(scaled, ceiling);

        var jitter = Math.Clamp(jitterSample, 0, 0.999_999) * 0.1 * backoffMs;
        return TimeSpan.FromMilliseconds(backoffMs + jitter);
    }
}
