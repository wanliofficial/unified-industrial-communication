using Uic.Domain;
using Uic.Driver;
using Uic.Engine;

namespace Uic.Tests;

/// <summary>
/// 数据加工与缓存的断言（规范要求的「缩放/死区/质量传播」三件事）。
///
/// 这一层的错误在产线上极难发现：数值错了但质量是 good，趋势图看起来完全正常。
/// 所以这里对每一处容易写反的地方都留了一条用例。
/// </summary>
internal static class PipelineTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("pipeline/scale_and_offset", ScaleAndOffset));
        tests.Add(new TestCase("pipeline/rounding_for_integer_types", RoundingForIntegerTypes));
        tests.Add(new TestCase("pipeline/bad_quality_skips_scaling", BadQualitySkipsScaling));
        tests.Add(new TestCase("pipeline/bad_quality_preserves_reason", BadQualityPreservesReason));
        tests.Add(new TestCase("pipeline/error_kind_maps_to_quality", ErrorKindMapsToQuality));
        tests.Add(new TestCase("pipeline/deadband_reports_change", DeadbandReportsChange));
        tests.Add(new TestCase("pipeline/deadband_ignores_noise_below_threshold", DeadbandIgnoresNoiseBelowThreshold));
        tests.Add(new TestCase("pipeline/deadband_reports_quality_transitions", DeadbandReportsQualityTransitions));
        tests.Add(new TestCase("pipeline/deadband_compares_non_numeric_by_equality", DeadbandComparesNonNumeric));
        tests.Add(new TestCase("pipeline/deadband_percent_uses_range_span", DeadbandPercentUsesRangeSpan));
        tests.Add(new TestCase("pipeline/deadband_rate_uses_elapsed_time", DeadbandRateUsesElapsedTime));
        tests.Add(new TestCase("pipeline/deadband_rate_ignores_a_clock_jump", DeadbandRateIgnoresAClockJump));
        tests.Add(new TestCase("pipeline/array_scales_element_wise", ArrayScalesElementWise));
        tests.Add(new TestCase("pipeline/deadband_array_uses_largest_change", DeadbandArrayUsesLargestChange));
        tests.Add(new TestCase("cache/publish_configured_applies_point_rule", CachePublishConfiguredAppliesRule));
        tests.Add(new TestCase("cache/publish_sequence_is_monotonic", CacheSequenceIsMonotonic));
        tests.Add(new TestCase("cache/publish_notifies_only_on_change", CacheNotifiesOnlyOnChange));
        tests.Add(new TestCase("cache/remove_device_clears_state", CacheRemoveDeviceClearsState));
        tests.Add(new TestCase("cache/subscriber_failure_does_not_break_publish", CacheSubscriberFailureIsolated));
    }

    private static Device Device(params Point[] points) => new()
    {
        Id = "dev-1",
        ChannelId = "ch-1",
        Interval = TimeSpan.FromMilliseconds(500),
        Points = points.ToList(),
    };

    private static Point Point(
        string id,
        DataType type = DataType.UInt16,
        double scale = 1,
        double offset = 0,
        double? deadband = null,
        string? unit = null,
        DeadbandMode deadbandMode = DeadbandMode.Absolute,
        double? rangeMin = null,
        double? rangeMax = null,
        int count = 0) => new()
    {
        Id = id,
        Address = Address.Register("holding", 0),
        DataType = type,
        Scale = scale,
        Offset = offset,
        Deadband = deadband,
        DeadbandMode = deadbandMode,
        RangeMin = rangeMin,
        RangeMax = rangeMax,
        Unit = unit,
        Count = count,
    };

    private static Task ScaleAndOffset(TestContext t)
    {
        var point = Point("temperature", DataType.Float64, scale: 0.1, offset: -5, unit: "°C");
        var raw = new RawSample(point.Id, DataValue.Number(DataType.UInt16, 250), Quality.Good, null);

        var sample = Pipeline.Process(Device(point), point, raw, 1, DateTimeOffset.UtcNow);

        // 250 * 0.1 - 5 = 20
        t.Close(20, sample.Value.AsNumber(), 1e-9, "scaled value");
        t.Equal(DataType.Float64, sample.Value.Type, "float point stays float after scaling");
        t.Equal("°C", sample.Unit, "unit is carried through");
        t.Equal(1UL, sample.Seq, "seq is carried through");
        t.Equal(Quality.Good, sample.Quality, "quality");

        return Task.CompletedTask;
    }

    private static Task RoundingForIntegerTypes(TestContext t)
    {
        var point = Point("count", DataType.UInt16, scale: 0.5);
        var raw = new RawSample(point.Id, DataValue.Number(DataType.UInt16, 7), Quality.Good, null);

        var sample = Pipeline.Process(Device(point), point, raw, 1, DateTimeOffset.UtcNow);

        // 3.5 四舍五入为 4：整数点位不允许带小数，否则北向的类型契约就形同虚设。
        t.Close(4, sample.Value.AsNumber(), 1e-9, "integer points round half away from zero");
        t.Equal(DataType.UInt16, sample.Value.Type, "type is preserved");

        return Task.CompletedTask;
    }

    private static Task BadQualitySkipsScaling(TestContext t)
    {
        var point = Point("flow", DataType.Float32, scale: 100);
        var raw = new RawSample(point.Id, DataValue.Number(DataType.Float32, 1), Quality.Timeout, null, "read timed out");

        var sample = Pipeline.Process(Device(point), point, raw, 3, DateTimeOffset.UtcNow);

        // 坏值上的缩放没有意义，甚至会误导：把 1 变成 100 再报成坏值，现场只会更困惑。
        t.Close(1, sample.Value.AsNumber(), 1e-9, "value is passed through untouched");
        t.Equal(Quality.Timeout, sample.Quality, "quality is preserved");
        t.Equal("read timed out", sample.Error, "error text is preserved");

        return Task.CompletedTask;
    }

    private static Task BadQualityPreservesReason(TestContext t)
    {
        var point = Point("pressure", DataType.UInt16);
        var raw = new RawSample(point.Id, null, Quality.AddressError, null, "illegal data address");

        var sample = Pipeline.Process(Device(point), point, raw, 1, DateTimeOffset.UtcNow);

        // 没有值时也必须给一个占位值，否则 UI 与北向都要处理 null。
        t.Ok(sample.Value is not null, "a placeholder value is always present");
        t.Equal(Quality.AddressError, sample.Quality, "quality");
        t.Equal("illegal data address", sample.Error, "error");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 驱动异常 → 质量码的统一映射（`QualityMap.From`）：每个 <see cref="ErrorKind"/> 落到哪一档
    /// 质量是「上层据此决定重试/熔断/立即失败」的唯一依据，所有驱动共用这一份实现。
    ///
    /// 此前只有 VirtualDriverTests 顺带钉住了 Address→address_error，其余七档从未被整体断言过：
    /// 把 Connection 写成 Bad（而不是 comm_error）不会让任何现有用例变红，却会让整网设备的断链
    /// 在趋势图与告警里看起来像「值不对」而不是「连不上」——两个结论的处理方式完全不同。
    /// </summary>
    private static Task ErrorKindMapsToQuality(TestContext t)
    {
        // 逐档点名，写清「为什么是这一档」，漂移时一眼看出改坏了哪条语义。
        var expected = new (ErrorKind Kind, Quality Quality, string Why)[]
        {
            (ErrorKind.Timeout, Quality.Timeout, "超时可重试，质量单独成档"),
            (ErrorKind.Connection, Quality.CommError, "连不上是通信错误，不冒充值坏"),
            (ErrorKind.Address, Quality.AddressError, "点表/地址错是配置问题"),
            (ErrorKind.Unsupported, Quality.Disabled, "本实现不做的能力是 disabled"),
            (ErrorKind.Permission, Quality.Disabled, "权限不足与不支持同归 disabled（写不进去）"),
            (ErrorKind.Canceled, Quality.NotConnected, "主动取消意味着会话已断"),
            (ErrorKind.Protocol, Quality.Bad, "协议错是坏值，不归到可重试类"),
            (ErrorKind.Internal, Quality.Bad, "网关内部错也是坏值"),
        };

        foreach (var (kind, quality, why) in expected)
            t.Equal(quality, QualityMap.From(kind), $"{kind} → {quality}：{why}");

        // 反向守卫：Good/Uncertain/Stale 不是由错误折出来的，任何 ErrorKind 都不该映射到它们，
        // 否则「读成功但标记为 stale」这类合法质量会被错误路径污染。
        foreach (ErrorKind kind in Enum.GetValues<ErrorKind>())
        {
            var q = QualityMap.From(kind);
            t.Ok(q is not (Quality.Good or Quality.Uncertain or Quality.Stale),
                $"{kind} 不能折成成功类质量（{q}）");
        }

        return Task.CompletedTask;
    }

    private static Task DeadbandReportsChange(TestContext t)
    {
        var previous = SampleOf(10, Quality.Good);
        var next = SampleOf(12, Quality.Good);

        t.Ok(Pipeline.ExceedsDeadband(previous, next, deadband: 1), "a 2-unit move crosses a 1-unit deadband");
        t.Ok(Pipeline.ExceedsDeadband(null, next, deadband: 1), "the first sample always reports");

        return Task.CompletedTask;
    }

    private static Task DeadbandIgnoresNoiseBelowThreshold(TestContext t)
    {
        var previous = SampleOf(10, Quality.Good);

        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleOf(10.5, Quality.Good), deadband: 1),
            "0.5 of movement is inside a 1-unit deadband");
        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleOf(10, Quality.Good), deadband: 1),
            "an identical value does not report");
        t.Ok(Pipeline.ExceedsDeadband(previous, SampleOf(11.5, Quality.Good), deadband: 1),
            "1.5 of movement is outside a 1-unit deadband");

        return Task.CompletedTask;
    }

    private static Task DeadbandReportsQualityTransitions(TestContext t)
    {
        // good → timeout 必须上报：值可能没变，但「这个点失效了」本身是事件。
        // 反过来，坏值之间不应该反复上报——那不是新信息。
        t.Ok(Pipeline.ExceedsDeadband(SampleOf(10, Quality.Good), SampleOf(10, Quality.Timeout), deadband: null),
            "good -> timeout always reports");
        t.Ok(Pipeline.ExceedsDeadband(SampleOf(10, Quality.Timeout), SampleOf(10, Quality.Good), deadband: null),
            "timeout -> good always reports");
        t.Ok(!Pipeline.ExceedsDeadband(SampleOf(10, Quality.Timeout), SampleOf(99, Quality.Timeout), deadband: 1),
            "a different value under the same bad quality does not report");

        return Task.CompletedTask;
    }

    private static Task DeadbandComparesNonNumeric(TestContext t)
    {
        var a = new Sample("d", "p", DataValue.String("run"), Quality.Good, null, null, DateTimeOffset.UtcNow, 1);
        var b = new Sample("d", "p", DataValue.String("stop"), Quality.Good, null, null, DateTimeOffset.UtcNow, 2);
        var c = new Sample("d", "p", DataValue.String("run"), Quality.Good, null, null, DateTimeOffset.UtcNow, 3);

        // 字符串点位没有「死区」的概念，只能整体比较。
        t.Ok(Pipeline.ExceedsDeadband(a, b, deadband: 5), "different text reports");
        t.Ok(!Pipeline.ExceedsDeadband(a, c, deadband: 5), "identical text does not report");

        var latch = new Sample("d", "p", DataValue.Boolean(true), Quality.Good, null, null, DateTimeOffset.UtcNow, 1);
        var latchOff = new Sample("d", "p", DataValue.Boolean(false), Quality.Good, null, null, DateTimeOffset.UtcNow, 2);

        t.Ok(Pipeline.ExceedsDeadband(latch, latchOff, deadband: 100), "bool changes always report");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 百分比死区按量程归一：5% 在 0–100 上是 5 个单位，在 0–10 上是 0.5。
    /// 点表从别的项目抄过来时，换的是量程而不是灵敏度。
    /// </summary>
    private static Task DeadbandPercentUsesRangeSpan(TestContext t)
    {
        var point = Point("pct", DataType.Float64, deadband: 5,
            deadbandMode: DeadbandMode.Percent, rangeMin: 0, rangeMax: 100);
        var previous = SampleOf(10, Quality.Good);

        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleOf(14.9, Quality.Good), point),
            "4.9 of a 100-unit span is inside 5%");
        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleOf(15, Quality.Good), point),
            "the threshold itself does not report (strict comparison)");
        t.Ok(Pipeline.ExceedsDeadband(previous, SampleOf(15.1, Quality.Good), point),
            "5.1 of a 100-unit span crosses 5%");

        var narrow = Point("pct", DataType.Float64, deadband: 5,
            deadbandMode: DeadbandMode.Percent, rangeMin: 0, rangeMax: 10);
        t.Ok(Pipeline.ExceedsDeadband(previous, SampleOf(10.6, Quality.Good), narrow),
            "the same 5% on a 0-10 span is only 0.5 units");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 变化率死区看的是「单位/秒」而不是「单位」：1 秒内涨 1.9 不报，
    /// 同样 1.9 的涨幅发生在 2 秒里更不该报，2.1/秒才报。
    /// </summary>
    private static Task DeadbandRateUsesElapsedTime(TestContext t)
    {
        var point = Point("rate", DataType.Float64, deadband: 2, deadbandMode: DeadbandMode.Rate);
        var t0 = new DateTimeOffset(2026, 11, 6, 8, 0, 0, TimeSpan.Zero);
        var previous = SampleAt(10, Quality.Good, t0);

        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleAt(11.9, Quality.Good, t0.AddSeconds(1)), point),
            "1.9/s is inside a 2/s threshold");
        t.Ok(Pipeline.ExceedsDeadband(previous, SampleAt(12.1, Quality.Good, t0.AddSeconds(1)), point),
            "2.1/s crosses a 2/s threshold");
        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleAt(13.9, Quality.Good, t0.AddSeconds(2)), point),
            "the same 3.9 units over 2s is only 1.95/s");
        t.Ok(Pipeline.ExceedsDeadband(previous, SampleAt(10.5, Quality.Good, t0), point),
            "zero elapsed time reports instead of hiding a change");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 变化率死区的 Δt 必须用单调时刻，绝不能用墙钟。
    ///
    /// rate 死区本质是「单位时间涨多少」。用墙钟算 Δt，一次 NTP 校时就能伪造结论：
    /// 前跳一小时，1 秒的 2.1 单位涨幅被算成 1 小时，速率趋近 0、尖峰被抹平；
    /// 后跳则把 1 小时压成 1 秒，凭空造出一个尖峰。判定本身不该依赖墙钟有没有被拨过。
    /// </summary>
    private static Task DeadbandRateIgnoresAClockJump(TestContext t)
    {
        var point = Point("rate", DataType.Float64, deadband: 2, deadbandMode: DeadbandMode.Rate);
        var t0 = new DateTimeOffset(2026, 11, 6, 8, 0, 0, TimeSpan.Zero);

        // 单调上真实只过了 1 秒，墙钟却被前跳了一小时。
        var previous = SampleAtMono(10, t0, monoSeconds: 0);
        var next = SampleAtMono(12.1, t0.AddHours(1), monoSeconds: 1);

        t.Ok(Pipeline.ExceedsDeadband(previous, next, point),
            "a forward wall-clock jump must not flatten the rate below threshold");

        // 单调真实过了 1 秒、涨幅只有 0.5，即便墙钟显示「同一时刻」，也不该误报。
        var slow = SampleAtMono(10.5, t0, monoSeconds: 0);
        var slowNext = SampleAtMono(11.0, t0, monoSeconds: 1);
        t.Ok(!Pipeline.ExceedsDeadband(slow, slowNext, point),
            "a sub-threshold move over a real second stays suppressed despite a flat wall clock");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 数组点位的缩放逐元素进行：每个元素各自乘 scale 再落回整数/浮点。
    /// 如果只缩第一个元素（把数组当标量处理的典型退化），其余元素会以原始工程量出现，
    /// 趋势图上看起来仍然「合理」，只有对着设备本地显示才能发现差了一个系数。
    /// </summary>
    private static Task ArrayScalesElementWise(TestContext t)
    {
        var integer = Point("batch", DataType.UInt16, scale: 0.5, count: 3);
        var raw = new RawSample(integer.Id,
            DataValue.Numbers(DataType.UInt16, new double[] { 7, 9, 11 }), Quality.Good, null);

        var sample = Pipeline.Process(Device(integer), integer, raw, 1, DateTimeOffset.UtcNow);

        t.Ok(sample.Value.IsArray, "the scaled value stays an array");
        t.Equal(3, sample.Value.Count, "element count is preserved");
        // 3.5→4、4.5→5（四舍五入远离零）、5.5→6
        t.Close(4, sample.Value.NumberAt(0), 1e-9, "element 0 is scaled and rounded");
        t.Close(5, sample.Value.NumberAt(1), 1e-9, "element 1 is scaled and rounded");
        t.Close(6, sample.Value.NumberAt(2), 1e-9, "element 2 is scaled and rounded");

        var floating = Point("profile", DataType.Float64, scale: 2, offset: -1, count: 2);
        var floatRaw = new RawSample(floating.Id,
            DataValue.Numbers(DataType.Float64, new double[] { 1.5, -0.25 }), Quality.Good, null);

        var floats = Pipeline.Process(Device(floating), floating, floatRaw, 2, DateTimeOffset.UtcNow);

        // 1.5*2-1 = 2；-0.25*2-1 = -1.5，浮点元素不做取整。
        t.Close(2, floats.Value.NumberAt(0), 1e-9, "float element 0");
        t.Close(-1.5, floats.Value.NumberAt(1), 1e-9, "float element 1 keeps its fraction");

        // 无缩放时数组原样透传，不做任何逐元素拷贝以外的加工。
        var plain = Point("plain", DataType.UInt16, count: 2);
        var untouched = Pipeline.Process(Device(plain), plain,
            new RawSample(plain.Id, DataValue.Numbers(DataType.UInt16, new double[] { 3, 4 }),
                Quality.Good, null), 3, DateTimeOffset.UtcNow);

        t.Close(3, untouched.Value.NumberAt(0), 1e-9, "unscaled element 0");
        t.Close(4, untouched.Value.NumberAt(1), 1e-9, "unscaled element 1");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 数组死区按「逐元素比、取最大变化」判定。
    ///
    /// 用最大而不是平均值是刻意的：平均值会把「一个通道跳变、其余不动」稀释成没变化，
    /// 而那正是最该被看到的情况。这条用例同时锁住反向：所有元素都在阈值内时不上报。
    /// </summary>
    private static Task DeadbandArrayUsesLargestChange(TestContext t)
    {
        var point = Point("batch", DataType.UInt16, deadband: 1, count: 3);

        var previous = SampleArray(new double[] { 10, 10, 10 }, 1);
        var noisy = SampleArray(new double[] { 10.4, 10.4, 10.4 }, 2);

        t.Ok(!Pipeline.ExceedsDeadband(previous, noisy, point),
            "all elements inside the threshold do not report");

        // 只有第三个元素跳了 1.5，平均变化只有 0.5——按平均值会被吞掉。
        var oneJump = SampleArray(new double[] { 10, 10, 11.5 }, 3);
        t.Ok(Pipeline.ExceedsDeadband(previous, oneJump, point),
            "a single element crossing the threshold reports");

        // 元素个数都变了说明读取窗口已经不对，必须上报而不是比较。
        var wrongLength = SampleArray(new double[] { 10, 10 }, 4);
        t.Ok(Pipeline.ExceedsDeadband(previous, wrongLength, point),
            "a changed element count always reports");

        // 标量与数组互相比同样视为变化：类型/维度变化本身就是事件。
        t.Ok(Pipeline.ExceedsDeadband(previous, SampleOf(10, Quality.Good), point),
            "scalar vs array always reports");

        // 关闭死区（null）时数组按内容比较，相同内容不上报。
        t.Ok(!Pipeline.ExceedsDeadband(previous, SampleArray(new double[] { 10, 10, 10 }, 5), deadband: null),
            "identical arrays do not report without a deadband");
        t.Ok(Pipeline.ExceedsDeadband(previous, noisy, deadband: null),
            "any element change reports without a deadband");

        return Task.CompletedTask;
    }

    private static Sample SampleArray(double[] values, ulong seq) => new(
        "d", "p", DataValue.Numbers(DataType.UInt16, values), Quality.Good, null, null,
        DateTimeOffset.UtcNow, seq);

    private static Task CachePublishConfiguredAppliesRule(TestContext t)
    {
        var cache = new PointCache();
        var point = Point("pct", DataType.Float64, deadband: 10,
            deadbandMode: DeadbandMode.Percent, rangeMin: 0, rangeMax: 100);

        t.Ok(cache.PublishConfigured(SampleOf(10, Quality.Good), point), "the first sample reports");
        // 死区比较的是「相邻两次采集」，不是「上次上报」：15 距 10 只有 5，被吞掉；
        // 26 距 15 是 11，越过 10% 的阈值。
        t.Ok(!cache.PublishConfigured(SampleOf(15, Quality.Good), point),
            "5 of a 100-unit span stays inside 10%");
        t.Ok(cache.PublishConfigured(SampleOf(26, Quality.Good), point),
            "11 of a 100-unit span crosses 10%");

        return Task.CompletedTask;
    }

    private static Task CacheSequenceIsMonotonic(TestContext t)
    {
        var cache = new PointCache();

        t.Equal(1UL, cache.NextSeq("d", "p"), "first seq");
        t.Equal(2UL, cache.NextSeq("d", "p"), "second seq");
        t.Equal(1UL, cache.NextSeq("d", "q"), "seq is per point");
        t.Equal(1UL, cache.NextSeq("e", "p"), "seq is per device");

        return Task.CompletedTask;
    }

    private static Task CacheNotifiesOnlyOnChange(TestContext t)
    {
        var cache = new PointCache();
        var seen = new List<double>();

        using var subscription = cache.Subscribe(s => seen.Add(s.Value.AsNumber()));

        t.Ok(cache.Publish(SampleOf(10, Quality.Good), deadband: null), "first publish reports");
        t.Ok(!cache.Publish(SampleOf(10, Quality.Good), deadband: null), "same value does not report");
        t.Ok(cache.Publish(SampleOf(11, Quality.Good), deadband: null), "changed value reports");

        // 不通知不代表不缓存：最后一个值仍必须是可读的。
        t.Equal(11d, cache.Get("d", "p")!.Value.AsNumber(), "cache always holds the latest value");
        t.Equal(2, seen.Count, "subscriber saw exactly the reporting samples");

        return Task.CompletedTask;
    }

    private static Task CacheRemoveDeviceClearsState(TestContext t)
    {
        var cache = new PointCache();

        cache.Publish(new Sample("dev-a", "p", DataValue.Number(DataType.UInt16, 1), Quality.Good,
            null, null, DateTimeOffset.UtcNow, 1), null);
        cache.Publish(new Sample("dev-b", "p", DataValue.Number(DataType.UInt16, 2), Quality.Good,
            null, null, DateTimeOffset.UtcNow, 1), null);

        cache.RemoveDevice("dev-a");

        t.Ok(cache.Get("dev-a", "p") is null, "removed device is gone");
        t.Ok(cache.Get("dev-b", "p") is not null, "other devices are untouched");
        t.Equal(1UL, cache.NextSeq("dev-a", "p"), "seq restarts for a re-added device");

        return Task.CompletedTask;
    }

    private static Task CacheSubscriberFailureIsolated(TestContext t)
    {
        var cache = new PointCache();
        var reached = 0;

        using var bad = cache.Subscribe(_ => throw new InvalidOperationException("subscriber is broken"));
        using var good = cache.Subscribe(_ => reached++);

        // 订阅者故障（北向推送挂了）绝不能影响采集主链路。
        t.Ok(cache.Publish(SampleOf(1, Quality.Good), null), "publish still reports");
        t.Equal(1, reached, "the healthy subscriber still received the sample");

        return Task.CompletedTask;
    }

    private static Sample SampleOf(double value, Quality quality)
        => SampleAt(value, quality, DateTimeOffset.UtcNow);

    private static Sample SampleAt(double value, Quality quality, DateTimeOffset at) => new(
        "d", "p", DataValue.Number(DataType.Float64, value), quality, null, null, at, 1);

    /// <summary>
    /// 同时指定墙钟与单调时刻的样本：验证「Δt 用单调、墙钟可随意跳」。
    /// <paramref name="monoSeconds"/> 是单调秒数，乘上 <see cref="TimeSpan.TicksPerSecond"/>
    /// 归一化到 ticks——与生产侧 <c>Sample.MonoAt</c> 的单位一致。
    /// </summary>
    private static Sample SampleAtMono(double value, DateTimeOffset wall, double monoSeconds) => new(
        "d", "p", DataValue.Number(DataType.Float64, value), Quality.Good, null, null, wall, 1,
        null, (long)(monoSeconds * TimeSpan.TicksPerSecond));
}
