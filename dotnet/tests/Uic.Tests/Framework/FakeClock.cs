namespace Uic.Tests;

/// <summary>
/// 可拨的时钟：墙钟与单调时间分开推进。
///
/// 现场这两者由不同机制提供——墙钟会被 NTP / 手工校时拨动，单调时钟只随真实流逝走。
/// 用例把它们绑在一起就永远分不开「校时」与「真的过了那么久」，而这类缺陷
/// （停滞误判、TTL 误清、uptime 跳变）恰好全长在两者的差别上。
/// </summary>
internal sealed class FakeClock : TimeProvider
{
    private DateTimeOffset _wall = DateTimeOffset.UtcNow;
    private long _stamp;

    public override DateTimeOffset GetUtcNow() => _wall;

    public override long TimestampFrequency => TimeSpan.TicksPerSecond;

    public override long GetTimestamp() => _stamp;

    // 不重写 GetElapsedTime：基类按 TimestampFrequency 换算，把频率设成
    // 「每秒的 tick 数」后，(end-start) 个 tick 恰好换算回真实时长。

    /// <summary>分别推进墙钟与单调时钟，模拟校时跳变或真实流逝。</summary>
    public void Advance(TimeSpan wall, TimeSpan monotonic)
    {
        _wall += wall;
        _stamp += monotonic.Ticks;
    }
}
