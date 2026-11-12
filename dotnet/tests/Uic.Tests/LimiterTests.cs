using Uic.Northbound;

namespace Uic.Tests;

/// <summary>
/// 限流器的行为。看起来只是「数个数」，但两个地方一旦写错就会变成故障源：
///
/// 1. 配额按「过了多久」恢复，必须用单调时钟——用墙钟算，一次 NTP 前跳会让
///    配额瞬间回满（限流形同虚设），后跳会让配额永不恢复（网关把自己锁在门外）；
/// 2. 桶按来源 IP 建，必须能被清掉。全网扫描器每天贡献几十万个地址，
///    每个地址一个永久桶就是无上限的内存增长，限流器自己成了拒绝服务手段。
///
/// 用 <see cref="FakeClock"/> 而不是真的等：这些是纯时长逻辑，
/// 真等 10 分钟只会让用例变慢，还测不了「校时跳变」。
/// </summary>
internal static class LimiterTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("limits/burst_then_refill", BurstThenRefill));
        tests.Add(new TestCase("limits/wall_clock_jump_does_not_refill", WallClockJumpDoesNotRefill));
        tests.Add(new TestCase("limits/prune_drops_idle_buckets", PruneDropsIdleBuckets));
        tests.Add(new TestCase("limits/prune_keeps_active_buckets", PruneKeepsActiveBuckets));
        tests.Add(new TestCase("limits/idle_window_cannot_undercut_a_refill", IdleWindowCannotUndercutARefill));
        tests.Add(new TestCase("limits/too_many_requests_uses_one_wire_code", TooManyRequestsUsesOneWireCode));
    }

    private static Task TooManyRequestsUsesOneWireCode(TestContext t)
    {
        // 429 有两个来源：通用限流与登录锁定。它们必须用**同一个** code，
        // 否则客户端得写两套分支才能认出「被限流了」——而文档里只写一个。
        // 这两处曾经漂过：实现统一返回 request.rate_limited，
        // 文档（以及由此生成的客户端错误表）却写成 request.too_many_requests。
        var ex = ApiException.TooManyRequests();

        t.Equal(429, ex.Status, "the status is 429");
        t.Equal("request.rate_limited", ex.Code, "the limiter and the login lockout share one wire code");

        return Task.CompletedTask;
    }

    private static Task BurstThenRefill(TestContext t)
    {
        var clock = new FakeClock();
        var limiter = new RateLimiter(capacity: 2, refillPerSecond: 1, clock);

        t.Ok(limiter.TryTake("a"), "the first request in the burst is allowed");
        t.Ok(limiter.TryTake("a"), "the second request in the burst is allowed");
        t.Ok(!limiter.TryTake("a"), "the burst is capped at the capacity");
        t.Ok(limiter.TryTake("b"), "another key has its own bucket");

        // 恢复是连续的：过 1.5 s 就回 1.5 个令牌，不按整秒跳变。
        clock.Advance(TimeSpan.Zero, TimeSpan.FromMilliseconds(1500));
        t.Ok(limiter.TryTake("a"), "a token comes back after the refill interval");
        t.Ok(!limiter.TryTake("a"), "half a token is not two tokens");

        return Task.CompletedTask;
    }

    private static Task WallClockJumpDoesNotRefill(TestContext t)
    {
        var clock = new FakeClock();
        var limiter = new RateLimiter(capacity: 1, refillPerSecond: 1, clock);

        t.Ok(limiter.TryTake("a"), "the burst token is spent");
        t.Ok(!limiter.TryTake("a"), "the bucket is empty");

        // 只拨墙钟：真实上只过了几毫秒，配额不能因此回满，
        // 否则「改一下系统时间」就成了绕过限流的最短路径。
        clock.Advance(TimeSpan.FromHours(1), TimeSpan.Zero);
        t.Ok(!limiter.TryTake("a"), "a wall-clock jump must not refill the bucket");

        // 单调时钟真的走过去了才恢复。
        clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(1));
        t.Ok(limiter.TryTake("a"), "real elapsed time does refill the bucket");

        return Task.CompletedTask;
    }

    private static Task PruneDropsIdleBuckets(TestContext t)
    {
        var clock = new FakeClock();
        var limiter = new RateLimiter(capacity: 2, refillPerSecond: 2, clock,
            idleLifetime: TimeSpan.FromSeconds(30));

        for (var i = 0; i < 50; i++) limiter.TryTake("scanner-" + i);
        t.Equal(50, limiter.BucketCount, "one bucket per source address");

        // 清扫按时间触发，而不是「每 N 次请求」：桶多的时候按请求触发会把
        // 遍历成本摊到每次调用上。
        clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(29));
        limiter.TryTake("scanner-0");
        t.Equal(50, limiter.BucketCount, "nothing is pruned before the idle window");

        clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(40));
        limiter.TryTake("newcomer");

        // 上一个存活的是 scanner-0（在 29 s 处用过），它此刻已空闲 40 s，也会被清掉；
        // 只剩 newcomer。
        t.Equal(1, limiter.BucketCount, "idle buckets are dropped, memory does not grow forever");
        t.Ok(limiter.BucketCount is 1, "the live bucket survives the sweep");

        return Task.CompletedTask;
    }

    private static Task PruneKeepsActiveBuckets(TestContext t)
    {
        var clock = new FakeClock();
        var limiter = new RateLimiter(capacity: 2, refillPerSecond: 2, clock,
            idleLifetime: TimeSpan.FromSeconds(30));

        limiter.TryTake("busy");
        limiter.TryTake("quiet");

        // busy 每 10 s 用一次，quiet 一直不用；清扫在 61 s 时触发。
        for (var i = 0; i < 6; i++)
        {
            clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(10));
            limiter.TryTake("busy");
        }

        t.Equal(1, limiter.BucketCount, "the busy key keeps its bucket, the quiet one is dropped");

        // 丢掉活跃桶等于替对方把配额立刻刷新：busy 刚用完，仍然应当被限。
        limiter.TryTake("busy");
        t.Ok(!limiter.TryTake("busy"), "the surviving bucket kept its token accounting");

        return Task.CompletedTask;
    }

    private static Task IdleWindowCannotUndercutARefill(TestContext t)
    {
        // 调用方给的空闲窗口比「空桶回满」还短时，必须被抬到回满时间之上：
        // 否则每次清扫都会丢掉一个本该仍受限的桶，限流形同虚设。
        var clock = new FakeClock();
        var limiter = new RateLimiter(capacity: 100, refillPerSecond: 1, clock,
            idleLifetime: TimeSpan.FromSeconds(1));

        for (var i = 0; i < 100; i++) t.Ok(limiter.TryTake("a"), "the full burst is allowed");
        t.Ok(!limiter.TryTake("a"), "the bucket is empty");

        // 越过清扫周期（1 分钟）触发一次清扫：此时 "a" 只空闲了 61 s，
        // 但它要 100 s 才能回满，仍必须被保留。
        clock.Advance(TimeSpan.Zero, TimeSpan.FromSeconds(61));
        limiter.TryTake("probe");

        t.Equal(2, limiter.BucketCount,
            "a bucket that still needs ~39 s to refill is not treated as idle after 61 s");

        return Task.CompletedTask;
    }
}
