using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Virtual;
using Uic.Obs;

namespace Uic.Tests;

/// <summary>
/// 虚拟设备驱动的故障注入（FR-COL-07）。
///
/// 为什么值得单独立一组：`labels.fault` / `labels.faultAt` 是**面向用户的配置项**，
/// 写在 configuration.md 的参数表里，含意是「验收坏值怎么显示、断连怎么恢复，
/// 不需要拔网线」。这类开关最典型的坏法是「配了不生效」——驱动照常返回好值，
/// 用例照常全绿，而现场照着文档配了一台故障设备，却等不到任何故障。
///
/// 所以这里断言的是**配置到行为的映射本身**，而不是「虚拟设备能读数」。
/// 走 <see cref="VirtualDriver.Open"/> 而不是自己 new 一个 <c>VirtualConn</c>：
/// label 解析（大小写、缺省、非法值）是这条映射里最容易写错的一段。
/// </summary>
internal static class VirtualDriverTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("virtual/fault_timeout_is_a_temporary_timeout", FaultTimeoutIsTemporary));
        tests.Add(new TestCase("virtual/fault_drop_also_surfaces_as_timeout", FaultDropAlsoSurfacesAsTimeout));
        tests.Add(new TestCase("virtual/fault_address_is_permanent", FaultAddressIsPermanent));
        tests.Add(new TestCase("virtual/fault_bad_keeps_the_sample_and_the_reason", FaultBadKeepsSample));
        tests.Add(new TestCase("virtual/fault_disconnect_is_persistent", FaultDisconnectIsPersistent));
        tests.Add(new TestCase("virtual/fault_at_injects_exactly_once", FaultAtInjectsExactlyOnce));
        tests.Add(new TestCase("virtual/fault_is_none_by_default", FaultIsNoneByDefault));
        tests.Add(new TestCase("virtual/const_mode_writes_read_back", ConstModeWritesReadBack));
    }

    /// <summary>
    /// 超时必须是**临时性**错误：上层据此重试并在重试耗尽后降级。
    /// 如果它被归类成永久错误，一次抖动就会把设备钉进 faulted，且不重试。
    /// </summary>
    private static async Task FaultTimeoutIsTemporary(TestContext t)
    {
        await using var conn = Open(("fault", "timeout"), ("faultAt", "1"));
        var (_, points) = Model();

        var ex = await t.ThrowsAsync<DriverException>(
            () => conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct), "注入超时");

        t.Equal(ErrorKind.Timeout, ex.Kind, "fault=timeout 归为 Timeout");
        t.Equal(true, ex.Temporary, "超时是临时性错误，允许重试");
        t.Contains(ex.Message, "injected timeout", "错误消息说明是注入的，便于排障时区分真假");
    }

    /// <summary>
    /// 文档把 `drop`（丢包）与 `timeout` 列成两类。在驱动这一层两者的表现必然相同——
    /// 「没收到响应」和「等超时」从主站看就是一件事，差别在线上而不是在这里。
    /// 钉住这一点：`drop` 不许变成「照常返回好值」，那才是真正的事故。
    /// </summary>
    private static async Task FaultDropAlsoSurfacesAsTimeout(TestContext t)
    {
        await using var conn = Open(("fault", "drop"), ("faultAt", "1"));
        var (_, points) = Model();

        var ex = await t.ThrowsAsync<DriverException>(
            () => conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct), "注入丢包");

        t.Equal(ErrorKind.Timeout, ex.Kind, "fault=drop 从主站看等于没收到响应");
        t.Equal(true, ex.Temporary, "丢包同样是临时性错误");
    }

    /// <summary>
    /// 地址错是**永久性**错误：重试再多次也还是同一个地址，必须立刻停下并转成
    /// address_error 质量码，而不是把设备拖进降级/熔断。
    /// </summary>
    private static async Task FaultAddressIsPermanent(TestContext t)
    {
        await using var conn = Open(("fault", "address"), ("faultAt", "1"));
        var (_, points) = Model();

        var ex = await t.ThrowsAsync<DriverException>(
            () => conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct), "注入地址错");

        t.Equal(ErrorKind.Address, ex.Kind, "fault=address 归为 Address");
        t.Equal(false, ex.Temporary, "地址错重试没有意义");
        t.Equal(Quality.AddressError, QualityMap.From(ex.Kind), "上层据此落 address_error 质量码");
    }

    /// <summary>
    /// 坏值不走异常：采样必须留在结果里，质量是 <see cref="Quality.Bad"/>，
    /// 并带上原因。抛异常会让「这一批里只有一个点坏了」变成整批失败，
    /// 其余好点跟着被重试——现场看到的就是「一个坏点拖垮整台设备」。
    /// </summary>
    private static async Task FaultBadKeepsSample(TestContext t)
    {
        await using var conn = Open(("fault", "bad"));
        var (_, points) = Model();

        var samples = await conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct);

        t.Equal(points.Count, samples.Count, "每个点都有一条采样，不会被吞掉或合并");
        foreach (var sample in samples)
        {
            t.Equal(Quality.Bad, sample.Quality, $"{sample.PointId} 的质量是 bad");
            t.Equal(null, sample.Value, $"{sample.PointId} 不该带一个看起来可用的值");
            t.Contains(sample.Error ?? "", "injected", $"{sample.PointId} 带上原因，排障时能区分注入与真实坏值");
        }
    }

    /// <summary>
    /// 断连是**持续**状态，不是「第 N 次抖一下」：探活与读都必须失败，
    /// 而且第二次读还要失败。只抖一次的话，上层看到的是一条「自己恢复了」的链路，
    /// 于是断开期间的告警与重连逻辑根本不会被走到。
    /// </summary>
    private static async Task FaultDisconnectIsPersistent(TestContext t)
    {
        await using var conn = Open(("fault", "disconnect"));
        var (_, points) = Model();

        var probe = await t.ThrowsAsync<DriverException>(() => conn.ProbeAsync(t.Ct), "断连时探活失败");
        t.Equal(ErrorKind.Connection, probe.Kind, "探活报连接错误");
        t.Equal(true, probe.Temporary, "连接错误允许重连");

        for (var attempt = 1; attempt <= 2; attempt++)
        {
            var read = await t.ThrowsAsync<DriverException>(
                () => conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct), $"第 {attempt} 次读");
            t.Equal(ErrorKind.Connection, read.Kind, $"第 {attempt} 次读仍然报连接错误");
        }
    }

    /// <summary>
    /// <c>faultAt</c> 的语义是「只在第 N 次请求注入」。两个方向都要钉住：
    /// 第 N 次必须真的坏（否则开关无效），前后两次必须真的好（否则是「配一次坏到底」）。
    /// 只测「第 N 次坏了」的话，一个把 faultAt 忽略、每次都注入的实现同样能通过。
    /// </summary>
    private static async Task FaultAtInjectsExactlyOnce(TestContext t)
    {
        await using var conn = Open(("fault", "timeout"), ("faultAt", "2"));
        var (_, points) = Model();
        var span = new[] { new ReadSpan("s", points) };

        var first = await conn.ReadAsync(span, t.Ct);
        t.Equal(points.Count, first.Count, "第 1 次读正常返回");

        await t.ThrowsAsync<DriverException>(() => conn.ReadAsync(span, t.Ct),
            "第 2 次读按 faultAt 注入");

        var third = await conn.ReadAsync(span, t.Ct);
        t.Equal(points.Count, third.Count, "第 3 次读恢复正常，故障只注入一次");
        t.Ok(third.All(s => s.Quality == Quality.Good), "恢复后质量是 good，不是残留的坏值");
    }

    /// <summary>
    /// 没配 `fault` 时必须一条都不注入。这条看起来平庸，但它挡住的是
    /// 「默认值写错」这一类最贵的 bug：默认非 none 会让整个测试套件与演示配置
    /// 都在随机报故障，而根因在一个 label 的缺省值上。
    /// </summary>
    private static async Task FaultIsNoneByDefault(TestContext t)
    {
        await using var conn = Open();
        var (_, points) = Model();
        var span = new[] { new ReadSpan("s", points) };

        // 连读三次：faultAt 缺省是 0（不注入），任何一次注入都会在这里暴露。
        for (var i = 1; i <= 3; i++)
        {
            var samples = await conn.ReadAsync(span, t.Ct);
            t.Ok(samples.All(s => s.Quality == Quality.Good), $"第 {i} 次读全为 good");
        }

        await conn.ProbeAsync(t.Ct);
        t.Ok(true, "默认配置下探活不抛异常");
    }

    /// <summary>
    /// 常量波形写后回读。这不是协议行为，但演示与 `uic sim` 都靠它：
    /// 写进去的值必须读得出来，否则「控制回路演示」只能看到波形自说自话。
    /// </summary>
    private static async Task ConstModeWritesReadBack(TestContext t)
    {
        await using var conn = Open(("mode", "const"), ("baseline", "5"));
        var (_, points) = Model();
        var point = points[0];

        var before = await conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct);
        t.Equal(5d, before[0].Value!.AsNumber(), "常量波形取 baseline");

        var results = await conn.WriteAsync(
            new[] { new WriteCommand(point, DataValue.Number(point.DataType, 42)) }, t.Ct);
        t.Equal(true, results[0].Ok, "写入被接受");

        var after = await conn.ReadAsync(new[] { new ReadSpan("s", points) }, t.Ct);
        t.Equal(42d, after[0].Value!.AsNumber(), "回读到刚写入的值");
        t.Equal(5d, after[1].Value!.AsNumber(), "另一个点不受影响");
    }

    // ------------------------------------------------------------------
    // 夹具
    // ------------------------------------------------------------------

    /// <summary>
    /// 用一对点位建模型：两个点都声明成 holding 寄存器，
    /// 第二个点用来验证「一个点的故障不影响另一个点」。
    /// </summary>
    private static (Device Device, List<PointRef> Points) Model()
    {
        var device = new Device
        {
            Id = "virt-1",
            ChannelId = "ch-1",
            Interval = TimeSpan.FromMilliseconds(100),
            Points = new List<Point>
            {
                new() { Id = "a", Address = Address.Register("holding", 0), DataType = DataType.UInt16 },
                new() { Id = "b", Address = Address.Register("holding", 1), DataType = DataType.UInt16 },
            },
        };

        return (device, device.Points.Select(p => PointRef.Of(device, p)).ToList());
    }

    /// <summary>
    /// 按 label 打开一个连接。label 走的是与真实配置完全相同的那条解析路径。
    /// </summary>
    private static IConn Open(params (string Key, string Value)[] labels)
    {
        var channel = new Channel { Id = "ch-1", Protocol = "virtual", Transport = "none" };
        var (device, _) = Model();

        var deviceWithLabels = new Device
        {
            Id = device.Id,
            ChannelId = device.ChannelId,
            Interval = device.Interval,
            Points = device.Points,
            Labels = labels.ToDictionary(x => x.Key, x => x.Value, StringComparer.Ordinal),
        };

        return new VirtualDriver().Open(new OpenContext(
            channel, deviceWithLabels, NullLogger.Instance, new MetricsRegistry()));
    }
}
