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
        tests.Add(new TestCase("pipeline/deadband_reports_change", DeadbandReportsChange));
        tests.Add(new TestCase("pipeline/deadband_ignores_noise_below_threshold", DeadbandIgnoresNoiseBelowThreshold));
        tests.Add(new TestCase("pipeline/deadband_reports_quality_transitions", DeadbandReportsQualityTransitions));
        tests.Add(new TestCase("pipeline/deadband_compares_non_numeric_by_equality", DeadbandComparesNonNumeric));
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
        string? unit = null) => new()
    {
        Id = id,
        Address = Address.Register("holding", 0),
        DataType = type,
        Scale = scale,
        Offset = offset,
        Deadband = deadband,
        Unit = unit,
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

    private static Sample SampleOf(double value, Quality quality) => new(
        "d", "p", DataValue.Number(DataType.Float64, value), quality, null, null, DateTimeOffset.UtcNow, 1);
}
