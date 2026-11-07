using System.Text.Json;
using System.Text.Json.Nodes;
using Uic.Config;
using Uic.Domain;

namespace Uic.Tests;

/// <summary>
/// 线上序列化契约。这是网关与 UI、SCADA、脚本之间唯一的握手点，
/// 一旦悄悄漂移，两侧都「自测通过」却互相读不懂。
///
/// 重点断言三处：时长写法、枚举小写、时间统一 UTC 且带 Z。
/// </summary>
internal static class JsonTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("json/duration_roundtrip", DurationRoundtrip));
        tests.Add(new TestCase("json/duration_accepts_common_spellings", DurationAcceptsCommonSpellings));
        tests.Add(new TestCase("json/duration_rejects_nonsense", DurationRejectsNonsense));
        tests.Add(new TestCase("json/enums_are_lowercase_wire_names", EnumsAreLowercaseWireNames));
        tests.Add(new TestCase("json/datavalue_roundtrip", DataValueRoundtrip));
        tests.Add(new TestCase("json/datavalue_wire_shape_is_stable", DataValueWireShape));
        tests.Add(new TestCase("json/timestamps_are_utc_with_z", TimestampsAreUtcWithZ));
        tests.Add(new TestCase("json/config_roundtrip_is_lossless", ConfigRoundtripIsLossless));
        tests.Add(new TestCase("json/unknown_fields_are_tolerated", UnknownFieldsAreTolerated));
    }

    private static Task DurationRoundtrip(TestContext t)
    {
        foreach (var (text, expected) in new (string, TimeSpan)[]
                 {
                     ("500ms", TimeSpan.FromMilliseconds(500)),
                     ("1s", TimeSpan.FromSeconds(1)),
                     ("2m", TimeSpan.FromMinutes(2)),
                     ("1.5s", TimeSpan.FromMilliseconds(1500)),
                 })
        {
            t.Equal(expected, DurationConverter.Parse(text), $"parse '{text}'");
        }

        // 序列化回线上写法，再读回来必须完全一致。
        var json = JsonSerializer.Serialize(TimeSpan.FromMinutes(2), JsonDefaults.Options);
        t.Equal("\"2m\"", json, "duration is written in the compact form");

        var back = JsonSerializer.Deserialize<TimeSpan>(json, JsonDefaults.Options);
        t.Equal(TimeSpan.FromMinutes(2), back, "duration round-trips");

        // 没有整单位可用时（90 分钟）退化为秒：绝不输出 TimeSpan 的 "01:30:00"，
        // 那种写法与本转换器的语法不一致，写出去就读不回来。
        t.Equal("\"5400s\"", JsonSerializer.Serialize(TimeSpan.FromMinutes(90), JsonDefaults.Options),
            "non-integral durations fall back to seconds");

        return Task.CompletedTask;
    }

    private static Task DurationAcceptsCommonSpellings(TestContext t)
    {
        // 纯数字按毫秒：旧版本配置与手写脚本都这么写。
        var fromNumber = JsonSerializer.Deserialize<TimeSpan>("250", JsonDefaults.Options);
        t.Equal(TimeSpan.FromMilliseconds(250), fromNumber, "a bare number means milliseconds");

        // TimeSpan 的标准写法必须仍能读：配置文件可能由人从别处拷来。
        t.Equal(TimeSpan.FromHours(1), DurationConverter.Parse("01:00:00"), "hh:mm:ss");
        // 空字符串按 0 处理（字段缺省），但不能把一个写错的单位当成 0。
        t.Equal(TimeSpan.Zero,
            JsonSerializer.Deserialize<TimeSpan>("\"   \"", JsonDefaults.Options), "blank means zero");

        return Task.CompletedTask;
    }

    private static Task DurationRejectsNonsense(TestContext t)
    {
        // 写错的单位不能被悄悄当成毫秒——静默兜底会让「3 秒」采集变成 3 毫秒。
        t.Throws<FormatException>(() => DurationConverter.Parse("5 fortnights"), "unknown unit");
        t.Throws<FormatException>(() => DurationConverter.Parse("abc"), "not a duration");

        return Task.CompletedTask;
    }

    private static Task EnumsAreLowercaseWireNames(TestContext t)
    {
        foreach (var type in Enum.GetValues<DataType>())
        {
            var wire = JsonEnum.ToWire(type);
            t.Equal(wire.ToLowerInvariant(), wire, $"{type}: wire name is lowercase");
            t.Equal(type, JsonEnum.ParseDataType(wire), $"{type}: wire name round-trips");

            var json = JsonSerializer.Serialize(type, JsonDefaults.Options);
            t.Equal($"\"{wire}\"", json, $"{type}: serialized as a wire name, not a number");
        }

        foreach (var quality in Enum.GetValues<Quality>())
        {
            var wire = JsonEnum.ToWire(quality);
            t.Equal(quality, JsonEnum.ParseQuality(wire), $"{quality}: wire name round-trips");
        }

        // 数值枚举会诱使消费方写死序号；契约里明确禁止。
        t.Throws<JsonException>(
            () => JsonSerializer.Deserialize<Quality>("1", JsonDefaults.Options),
            "integer enum values are rejected");

        return Task.CompletedTask;
    }

    private static Task DataValueRoundtrip(TestContext t)
    {
        foreach (var value in new[]
                 {
                     DataValue.Number(DataType.UInt16, 42),
                     DataValue.Number(DataType.Float64, -0.125),
                     DataValue.Boolean(true),
                     DataValue.String("运行"),
                     DataValue.DateTime(new DateTimeOffset(2026, 11, 5, 12, 47, 46, 123, TimeSpan.Zero)),
                 })
        {
            var json = JsonSerializer.Serialize(value, JsonDefaults.Options);
            var back = JsonSerializer.Deserialize<DataValue>(json, JsonDefaults.Options);

            t.Ok(back is not null, $"deserialize {json}");
            t.Equal(value.Type, back!.Type, $"{json}: type");
            t.Equal(value.Num, back.Num, $"{json}: num");
            t.Equal(value.Bool, back.Bool, $"{json}: bool");
            t.Equal(value.Text, back.Text, $"{json}: text");
        }

        // 未知数值类型名降级为 uint16 而不是抛错：旧网关发来的新类型不该炸掉 UI。
        var lenient = JsonSerializer.Deserialize<DataValue>("{\"type\":\"quantum\",\"num\":1}", JsonDefaults.Options);
        t.Equal(DataType.UInt16, lenient!.Type, "unknown type falls back to uint16");

        return Task.CompletedTask;
    }

    private static Task DataValueWireShape(TestContext t)
    {
        t.Equal("{\"type\":\"uint16\",\"num\":42}",
            JsonSerializer.Serialize(DataValue.Number(DataType.UInt16, 42), JsonDefaults.Options),
            "numeric shape");

        t.Equal("{\"type\":\"bool\",\"bool\":true}",
            JsonSerializer.Serialize(DataValue.Boolean(true), JsonDefaults.Options),
            "boolean shape");

        // 默认编码器会把非 ASCII 转义。线上契约允许这样（JSON 语义等价），
        // 但编辑器里要能直接读，所以 UI 侧改用 UnsafeRelaxedJsonEscaping。
        t.Equal("{\"type\":\"string\",\"text\":\"\\u8FD0\\u884C\"}",
            JsonSerializer.Serialize(DataValue.String("运行"), JsonDefaults.Options),
            "text is JSON-escaped by default");

        // 未使用的字段不写空值：消费方用「字段存在性」判断类型是常见做法。
        t.Ok(!JsonSerializer.Serialize(DataValue.Boolean(false), JsonDefaults.Options).Contains("num"),
            "no stray num field");

        // 时间值走 text 字段，且固定为 UTC + Z：消费方不必猜时区。
        t.Equal("{\"type\":\"datetime\",\"text\":\"2026-11-05T12:47:46.123Z\"}",
            JsonSerializer.Serialize(
                DataValue.DateTime(new DateTimeOffset(2026, 11, 5, 20, 47, 46, 123, TimeSpan.FromHours(8))),
                JsonDefaults.Options),
            "datetime shape");

        return Task.CompletedTask;
    }

    private static Task TimestampsAreUtcWithZ(TestContext t)
    {
        var local = new DateTimeOffset(2026, 11, 5, 20, 47, 46, 123, TimeSpan.FromHours(8));
        var json = JsonSerializer.Serialize(local, JsonDefaults.Options);

        // 契约只允许一种写法：UTC + 毫秒 + Z。混着 +08:00 迟早被脚本按字面量解析错。
        t.Equal("\"2026-11-05T12:47:46.123Z\"", json, "timestamps are normalized to UTC and end with Z");

        var back = JsonSerializer.Deserialize<DateTimeOffset>(json, JsonDefaults.Options);
        t.Equal(local.UtcDateTime, back.UtcDateTime, "the instant is preserved");

        return Task.CompletedTask;
    }

    private static Task ConfigRoundtripIsLossless(TestContext t)
    {
        var original = DemoConfig.Build(enableLocalModbus: true);
        var json = JsonDefaults.ToJson(original, indented: true);

        t.Contains(json, "\"interval\": \"1s\"", "durations use the compact form");

        var back = ConfigStore.Parse(json, "roundtrip.json");

        // 解析后再序列化必须字节一致：否则每次启动都会「产生一个新版本」。
        t.Equal(json, JsonDefaults.ToJson(back, indented: true), "round-trip is stable");
        t.Equal(original.Devices.Count, back.Devices.Count, "device count");
        t.Equal(original.Devices[0].Points.Count, back.Devices[0].Points.Count, "point count");
        t.Equal(original.Channels[1].Timeout, back.Channels[1].Timeout, "channel timeout");
        t.Equal(original.Devices[1].Interval, back.Devices[1].Interval, "device interval");

        return Task.CompletedTask;
    }

    private static Task UnknownFieldsAreTolerated(TestContext t)
    {
        // 前向兼容：新版本加的字段，旧版本的网关不该因为读不懂就拒绝启动。
        var json = JsonDefaults.ToJson(DemoConfig.Build());
        var node = JsonNode.Parse(json)!.AsObject();
        node["futureKnob"] = 42;

        var parsed = ConfigStore.Parse(node.ToJsonString(), "forward.json");
        t.Equal("demo-site", parsed.Site, "unknown top-level field is ignored");

        return Task.CompletedTask;
    }
}
