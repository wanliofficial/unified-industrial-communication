using System.Text.Json;
using System.Text.Json.Nodes;
using Uic.Config;
using Uic.Domain;
using Uic.Northbound;

namespace Uic.Tests;

/// <summary>
/// 配置侧能力的单元验收：模板实例化、点表 CSV、迁移链。
///
/// 这三块最容易「看起来有了」：接口能返回 200、但参数替换多了一对引号、
/// CSV 的 40001 被当成偏移 40001、旧配置被静默丢掉一个字段。
/// 所以这里直接断言产出的领域对象，而不是只看 HTTP 状态码。
/// </summary>
internal static class ConfigFeatureTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("template/instantiate_fifty_devices", InstantiateFiftyDevices));
        tests.Add(new TestCase("template/rejects_bad_parameters", RejectsBadParameters));
        tests.Add(new TestCase("template/store_rejects_unknown_placeholder", StoreRejectsUnknownPlaceholder));
        tests.Add(new TestCase("csv/address_expressions", AddressExpressions));
        tests.Add(new TestCase("csv/import_merges_and_reports_conflicts", ImportMergesAndReportsConflicts));
        tests.Add(new TestCase("csv/imports_deadband_mode_and_range", ImportDeadbandModeAndRange));
        tests.Add(new TestCase("csv/imports_point_interval", ImportPointInterval));
        tests.Add(new TestCase("csv/imports_array_count", ImportArrayCount));
        tests.Add(new TestCase("config/deadband_and_range_rules", DeadbandAndRangeRules));
        tests.Add(new TestCase("config/array_count_rules", ArrayCountRules));
        tests.Add(new TestCase("config/migration_is_explicit_and_versioned", MigrationIsExplicitAndVersioned));
        tests.Add(new TestCase("config/migrates_v0_legacy_shapes", MigratesLegacyShapes));
        tests.Add(new TestCase("config/composite_durations_parse", CompositeDurationsParse));
        tests.Add(new TestCase("config/redacted_secrets_are_restored", RedactedSecretsAreRestored));
        tests.Add(new TestCase("config/repository_examples_validate", RepositoryExamplesValidate));
        tests.Add(new TestCase("config/console_draft_builds_a_valid_document", ConsoleDraftBuildsValidDocument));
        tests.Add(new TestCase("config/four_address_kinds_validate", FourAddressKindsValidate));
        tests.Add(new TestCase("template/one_template_targets_modbus_and_s7", OneTemplateTargetsModbusAndS7));
    }

    private static DeviceTemplate Template(string json)
        => System.Text.Json.JsonSerializer.Deserialize<DeviceTemplate>(json, JsonDefaults.Options)!;

    private const string MeterTemplate = """
        {
          "schemaVersion": 1,
          "id": "meter",
          "parameters": [
            { "name": "index", "type": "int", "required": true },
            { "name": "base", "type": "int", "default": "0" },
            { "name": "label", "type": "string", "default": "m" }
          ],
          "device": {
            "id": "meter-{index}",
            "name": "{label}-{index}",
            "channelId": "bus",
            "points": [
              {
                "id": "voltage",
                "address": { "kind": "register", "area": "holding", "offset": "{base}", "length": 0, "bit": -1 },
                "dataType": "float32"
              }
            ]
          }
        }
        """;

    /// <summary>FR-CFG-04 的验收口径：一个模板生成 ≥ 50 个实例，且每个实例都通过校验。</summary>
    private static Task InstantiateFiftyDevices(TestContext t)
    {
        var template = Template(MeterTemplate);
        var ids = new HashSet<string>(StringComparer.Ordinal);

        for (var i = 1; i <= 50; i++)
        {
            var result = TemplateInstantiator.Instantiate(template, new Dictionary<string, string?>
            {
                ["index"] = i.ToString(),
                ["base"] = (i * 10).ToString(),
            }, new[] { "bus" });

            t.Ok(result.Ok, $"instance {i} is valid: {string.Join("; ", result.Problems)}");

            var device = result.Device!;
            t.Equal($"meter-{i}", device.Id, "the id placeholder is rendered");
            t.Equal($"m-{i}", device.Name, "a placeholder inside a longer string is rendered");
            t.Equal(i * 10, device.Points[0].Address.Offset, "an int placeholder becomes a JSON number");
            t.Equal(DataType.Float32, device.Points[0].DataType, "the rest of the skeleton is preserved");

            t.Ok(ids.Add(device.Id), $"device id {device.Id} is unique");
        }

        t.Equal(50, ids.Count, "fifty distinct devices were produced");
        return Task.CompletedTask;
    }

    private static Task RejectsBadParameters(TestContext t)
    {
        var template = Template(MeterTemplate);

        var missing = TemplateInstantiator.Instantiate(template,
            new Dictionary<string, string?>(), new[] { "bus" });

        t.Ok(!missing.Ok, "a missing required parameter fails");
        t.Contains(string.Join("; ", missing.Problems), "required", "the problem explains what is missing");

        var notAnInt = TemplateInstantiator.Instantiate(template, new Dictionary<string, string?>
        {
            ["index"] = "abc",
        }, new[] { "bus" });

        t.Ok(!notAnInt.Ok, "a non-integer int parameter fails");
        t.Contains(string.Join("; ", notAnInt.Problems), "not an integer", "the problem names the type");

        var unknown = TemplateInstantiator.Instantiate(template, new Dictionary<string, string?>
        {
            ["index"] = "1",
            ["typo"] = "x",
        }, new[] { "bus" });

        t.Ok(!unknown.Ok, "an unknown parameter fails instead of being ignored");

        var noChannel = TemplateInstantiator.Instantiate(template, new Dictionary<string, string?>
        {
            ["index"] = "1",
        }, new[] { "another-bus" });

        t.Ok(!noChannel.Ok, "a device pointing at a non-existent channel fails");
        t.Contains(string.Join("; ", noChannel.Problems), "does not exist", "the problem names the channel");

        return Task.CompletedTask;
    }

    private static Task StoreRejectsUnknownPlaceholder(TestContext t)
    {
        var template = Template("""
            {
              "schemaVersion": 1,
              "id": "bad-template",
              "parameters": [ { "name": "index", "type": "int" } ],
              "device": {
                "id": "d-{index}",
                "channelId": "bus",
                "points": [
                  {
                    "id": "p",
                    "address": { "kind": "register", "area": "holding", "offset": "{base}" },
                    "dataType": "uint16"
                  }
                ]
              }
            }
            """);

        var problems = TemplateStore.Validate(template);

        t.Ok(problems.Count > 0, "an undeclared placeholder is rejected at save time");
        t.Contains(string.Join("; ", problems), "{base}", "the problem names the placeholder");
        return Task.CompletedTask;
    }

    private static Task AddressExpressions(TestContext t)
    {
        var holding = PointCsv.ParseAddress("40001", null, null, null, out var error);
        t.Ok(holding is not null, $"40001 parses: {error}");
        t.Equal("holding", holding!.Area, "40001 is a holding register");
        t.Equal(0, holding.Offset, "Modbus references are one-based");

        var bit = PointCsv.ParseAddress("40001.3", null, null, null, out error);
        t.Ok(bit is not null, $"40001.3 parses: {error}");
        t.Equal(3, bit!.Bit, "the bit suffix is preserved");

        var input = PointCsv.ParseAddress("30010", null, null, null, out error);
        t.Ok(input is not null, $"30010 parses: {error}");
        t.Equal("input", input!.Area, "30010 is an input register");
        t.Equal(9, input.Offset, "the reference is converted to a zero-based offset");

        var coil = PointCsv.ParseAddress("coil:5.2", null, null, null, out error);
        t.Ok(coil is not null, $"coil:5.2 parses: {error}");
        t.Equal("coil", coil!.Area, "the area prefix is honoured");

        var split = PointCsv.ParseAddress(null, "holding", "100", "7", out error);
        t.Ok(split is not null, $"split columns parse: {error}");
        t.Equal(100, split!.Offset, "the offset column is used");
        t.Equal(7, split.Bit, "the bit column is used");

        var bad = PointCsv.ParseAddress("nonsense", null, null, null, out error);
        t.Ok(bad is null && error is not null, "an unparseable expression reports an error");
        return Task.CompletedTask;
    }

    private static Task ImportMergesAndReportsConflicts(TestContext t)
    {
        var existing = new List<Point>
        {
            new()
            {
                Id = "keep",
                Address = Address.Register("holding", 0),
                DataType = DataType.UInt16,
            },
            new()
            {
                Id = "replace",
                Address = Address.Register("holding", 1),
                DataType = DataType.UInt16,
            },
        };

        var csv = """
                  id,name,address,dataType,unit,scale,writable
                  replace,替换,40002,uint16,V,0.1,true
                  added,新增,holding:20,float32,degC,1,false
                  """;

        var result = PointCsv.Import(csv, existing, "d1");

        t.Ok(result.Ok, $"the csv is valid: {string.Join("; ", result.Problems)}");
        t.Equal(1, result.Added.Count, "one point is new");
        t.Equal("added", result.Added[0], "the new point is reported");
        t.Equal(1, result.Updated.Count, "one point is replaced");
        t.Equal("replace", result.Updated[0], "the replaced point is reported");

        var merged = PointCsv.Merge(existing, result.Points);
        t.Equal(3, merged.Count, "merge keeps points the csv does not mention");
        t.Ok(merged.Any(p => p.Id == "keep"), "the unmentioned point survives");
        t.Close(0.1, merged.First(p => p.Id == "replace").Scale, 1e-9, "the replacement wins");
        t.Equal(false, merged.First(p => p.Id == "added").Writable, "booleans are parsed");

        var duplicate = PointCsv.Import("""
                                        id,address
                                        dup,holding:0
                                        dup,holding:1
                                        """, Array.Empty<Point>(), "d1");

        t.Ok(!duplicate.Ok, "a duplicate id inside the file is rejected");
        t.Contains(string.Join("; ", duplicate.Problems), "duplicate", "the problem explains the conflict");

        var broken = PointCsv.Import("""
                                     id,address
                                     bad,not-an-address
                                     """, Array.Empty<Point>(), "d1");

        t.Ok(!broken.Ok, "a bad address is rejected");
        t.Contains(string.Join("; ", broken.Problems), "line 2", "the problem points at the row");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 百分比死区与量程必须能通过 CSV 导入：现场点表就是 Excel，
    /// 只支持 JSON 等于逼人手工改配置。
    /// </summary>
    private static Task ImportDeadbandModeAndRange(TestContext t)
    {
        var csv = """
                  id,address,dataType,deadband,deadbandMode,rangeMin,rangeMax
                  pct,holding:0,float32,5,percent,0,100
                  rate,holding:2,float32,2,rate,,
                  """;

        var result = PointCsv.Import(csv, Array.Empty<Point>(), "d1");
        t.Ok(result.Ok, $"the csv is valid: {string.Join("; ", result.Problems)}");

        var pct = result.Points.Single(p => p.Id == "pct");
        t.Equal(DeadbandMode.Percent, pct.DeadbandMode, "percent mode is parsed");
        t.Close(5, pct.Deadband!.Value, 1e-9, "the threshold is a percentage");
        t.Close(0, pct.RangeMin!.Value, 1e-9, "rangeMin is parsed");
        t.Close(100, pct.RangeMax!.Value, 1e-9, "rangeMax is parsed");

        var rate = result.Points.Single(p => p.Id == "rate");
        t.Equal(DeadbandMode.Rate, rate.DeadbandMode, "rate mode is parsed");
        t.Ok(rate.RangeMin is null && rate.RangeMax is null, "rate does not require a range");

        // 拼错的模式必须当场拒绝：静默退回绝对值会让死区悄悄失效。
        var broken = PointCsv.Import("""
                                     id,address,deadband,deadbandMode
                                     bad,holding:0,5,percentage
                                     """, Array.Empty<Point>(), "d1");
        t.Ok(!broken.Ok, "an unknown deadband mode is rejected");
        t.Contains(string.Join("; ", broken.Problems), "line 2", "the problem points at the row");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 点位周期也要能从 CSV 进来：现场点表是 Excel，让运维为了一个 100ms
    /// 去手改 JSON 不现实。空单元格表示继承设备周期，非法值必须当场拒绝。
    /// </summary>
    private static Task ImportPointInterval(TestContext t)
    {
        var csv = """
                  id,address,dataType,interval
                  fast,holding:0,uint16,100ms
                  slow,holding:1,uint16,
                  """;

        var result = PointCsv.Import(csv, Array.Empty<Point>(), "d1");
        t.Ok(result.Ok, $"the csv is valid: {string.Join("; ", result.Problems)}");

        t.Equal(TimeSpan.FromMilliseconds(100), result.Points.Single(p => p.Id == "fast").Interval,
            "the interval column is parsed as a duration");
        t.Equal(null, result.Points.Single(p => p.Id == "slow").Interval,
            "an empty cell means the device period is inherited");

        // 低于下限和拼错都要拒绝：静默退回设备周期会让「已经改了」变成假象。
        var tooFast = PointCsv.Import("""
                                       id,address,interval
                                       fast,holding:0,50ms
                                       """, Array.Empty<Point>(), "d1");
        t.Ok(!tooFast.Ok, "an interval below 100ms is rejected");
        t.Contains(string.Join("; ", tooFast.Problems), "interval", "the problem names the field");

        var garbage = PointCsv.Import("""
                                      id,address,interval
                                      fast,holding:0,soon
                                      """, Array.Empty<Point>(), "d1");
        t.Ok(!garbage.Ok, "a non-duration is rejected");
        t.Contains(string.Join("; ", garbage.Problems), "line 2", "the problem points at the row");

        return Task.CompletedTask;
    }

    private const string TestPointAddress =
        "\"address\": { \"kind\": \"register\", \"area\": \"holding\", \"offset\": 0 }";

    /// <summary>
    /// 数组维度的配置规则。
    ///
    /// 三条都必须拒绝：非数值元素类型（bool 按位、string 靠 length、datetime 线上不是数字）、
    /// 超过 125 个寄存器（Modbus 单次读的硬限制，读不回来的配置不该被接受）、
    /// 负的元素个数（没有语义，只能是填错）。
    /// </summary>
    private static Task ArrayCountRules(TestContext t)
    {
        static string Document(string point) => """
            {
              "schemaVersion": 1,
              "site": "arrays",
              "channels": [{ "id": "ch", "protocol": "virtual", "transport": "none" }],
              "devices": [{
                "id": "d1", "channelId": "ch", "interval": "1s",
                "points": [ { POINT } ]
              }]
            }
            """.Replace("POINT", point);

        static string PointJson(string extra) => $"\"id\": \"p\", {TestPointAddress}{extra}";

        void Expect(string point, string path, string message)
        {
            try
            {
                ConfigStore.Parse(Document(point), "inline.json");
                t.Fail($"{message}: invalid point was accepted");
            }
            catch (ConfigException ex)
            {
                t.Contains(string.Join("; ", ex.Problems), path, message);
            }
        }

        // 标量点位的默认 count 是 0：不填不需要任何额外字段。
        var scalar = ConfigStore.Parse(
            Document(PointJson(", \"dataType\": \"float32\"")), "inline.json");
        t.Equal(0, scalar.Devices[0].Points[0].Count, "count defaults to 0 (scalar)");

        // 合法的 float32 × 4 数组：8 个寄存器，在读窗口上限之内。
        var array = ConfigStore.Parse(
            Document(PointJson(", \"dataType\": \"float32\", \"count\": 4")), "inline.json");
        var point = array.Devices[0].Points[0];
        t.Equal(4, point.Count, "count round-trips");
        t.Equal(8, point.RegisterCount(), "float32 x 4 occupies 8 registers");

        // uint16 数组按元素个数线性放大，与元素宽度无关地保持「一个元素一份」的语义。
        var words = ConfigStore.Parse(
            Document(PointJson(", \"dataType\": \"uint16\", \"count\": 3")), "inline.json");
        t.Equal(3, words.Devices[0].Points[0].RegisterCount(), "uint16 x 3 occupies 3 registers");

        // 125 是上限本身，允许；126 必须拒绝。
        ConfigStore.Parse(
            Document(PointJson(", \"dataType\": \"uint16\", \"count\": 125")), "inline.json");

        Expect(PointJson(", \"dataType\": \"uint16\", \"count\": 126"),
            "count", "an array beyond 125 registers is rejected");

        // float32 的每个元素占 2 个寄存器，所以 63 个元素已经 126 个寄存器。
        Expect(PointJson(", \"dataType\": \"float32\", \"count\": 63"),
            "count", "the register limit accounts for the element width");

        Expect(PointJson(", \"dataType\": \"string\", \"length\": 8, \"count\": 4"),
            "count", "string cannot be an array");
        Expect(PointJson(", \"dataType\": \"bool\", \"count\": 4"),
            "count", "bool cannot be an array");
        Expect(PointJson(", \"dataType\": \"datetime\", \"count\": 2"),
            "count", "datetime cannot be an array");
        Expect(PointJson(", \"dataType\": \"uint16\", \"count\": -1"),
            "count", "a negative count is rejected");

        return Task.CompletedTask;
    }

    /// <summary>
    /// CSV 的 <c>count</c> 列：现场点表在 Excel 里，数组维度必须能跟着点表一起导入，
    /// 否则每个数组点位都要手工改 JSON。
    /// </summary>
    private static Task ImportArrayCount(TestContext t)
    {
        var csv = """
                  id,address,dataType,count
                  profile,holding:0,float32,4
                  batch,holding:8,uint16,3
                  plain,holding:11,uint16,
                  """;

        var result = PointCsv.Import(csv, Array.Empty<Point>(), "d1");
        t.Ok(result.Ok, $"the csv is valid: {string.Join("; ", result.Problems)}");

        t.Equal(4, result.Points.Single(p => p.Id == "profile").Count, "count is parsed");
        t.Equal(8, result.Points.Single(p => p.Id == "profile").RegisterCount(),
            "the register window scales with the element count");
        t.Equal(3, result.Points.Single(p => p.Id == "batch").Count, "a second array is parsed");
        t.Equal(0, result.Points.Single(p => p.Id == "plain").Count,
            "an empty cell means a scalar point");

        // 导入器与启动校验共用一套规则：能导入的配置必须能启动。
        var tooBig = PointCsv.Import("""
                                     id,address,dataType,count
                                     huge,holding:0,uint16,126
                                     """, Array.Empty<Point>(), "d1");

        t.Ok(!tooBig.Ok, "an over-wide array is rejected at import time");
        t.Contains(string.Join("; ", tooBig.Problems), "count", "the problem names the count field");

        var garbage = PointCsv.Import("""
                                      id,address,dataType,count
                                      bad,holding:0,uint16,many
                                      """, Array.Empty<Point>(), "d1");

        t.Ok(!garbage.Ok, "a non-numeric count is rejected");
        t.Contains(string.Join("; ", garbage.Problems), "line 2", "the problem points at the row");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 量程与死区组合的静态规则。写错的死区不会报错、只会让数据看起来「一直没变化」，
    /// 所以这些组合必须在启动前被拒绝。
    /// </summary>
    private static Task DeadbandAndRangeRules(TestContext t)
    {
        static string Document(string point) => """
            {
              "schemaVersion": 1,
              "site": "rules",
              "channels": [{ "id": "ch", "protocol": "virtual", "transport": "none" }],
              "devices": [{
                "id": "d1", "channelId": "ch", "interval": "1s",
                "points": [ { POINT } ]
              }]
            }
            """.Replace("POINT", point);

        static string PointJson(string extra) => $"\"id\": \"p\", {TestPointAddress}{extra}";

        void Expect(string point, string path, string message)
        {
            try
            {
                ConfigStore.Parse(Document(point), "inline.json");
                t.Fail($"{message}: invalid point was accepted");
            }
            catch (ConfigException ex)
            {
                t.Contains(string.Join("; ", ex.Problems), path, message);
            }
        }

        Expect(PointJson(", \"rangeMin\": 0"),
            "rangeMin", "a lone rangeMin is rejected");
        Expect(PointJson(", \"rangeMin\": 10, \"rangeMax\": 10"),
            "rangeMax", "rangeMax must be greater than rangeMin");
        Expect(PointJson(", \"deadband\": 5, \"deadbandMode\": \"percent\""),
            "rangeMin", "percent mode requires a range");
        Expect(PointJson(", \"deadband\": 150, \"deadbandMode\": \"percent\", \"rangeMin\": 0, \"rangeMax\": 100"),
            "deadband", "percent above 100 is a typo, not a threshold");
        Expect(PointJson(", \"deadband\": 0, \"deadbandMode\": \"rate\""),
            "deadband", "rate mode requires a positive threshold");

        var valid = ConfigStore.Parse(
            Document(PointJson(", \"deadband\": 5, \"deadbandMode\": \"percent\", \"rangeMin\": 0, \"rangeMax\": 100")),
            "inline.json");

        var point = valid.Devices[0].Points[0];
        t.Equal(DeadbandMode.Percent, point.DeadbandMode, "the mode round-trips");
        t.Close(100, point.RangeMax!.Value, 1e-9, "the range round-trips");

        return Task.CompletedTask;
    }

    private static Task MigrationIsExplicitAndVersioned(TestContext t)
    {
        const string legacy = """
                              {
                                "schemaVersion": 1,
                                "site": "plant-a",
                                "server": { "listen": "0.0.0.0:9090" },
                                "northbound": {
                                  "mqtt": { "enabled": true, "broker": "mqtt://broker.local:1884" }
                                }
                              }
                              """;

        var migration = ConfigMigrations.Upgrade(legacy);

        t.Ok(migration.Applied.Count >= 2, "both legacy shapes are reported");
        t.Equal("0.0.0.0:9090",
            migration.Root["northbound"]!["rest"]!["listen"]!.GetValue<string>(),
            "server.listen is moved");
        t.Equal("broker.local",
            migration.Root["northbound"]!["mqtt"]!["host"]!.GetValue<string>(),
            "the broker host is extracted");
        t.Equal(1884,
            migration.Root["northbound"]!["mqtt"]!["port"]!.GetValue<int>(),
            "the broker port is extracted");
        t.Ok(migration.Root["server"] is null, "the legacy section is removed");

        t.Throws<ConfigException>(
            () => ConfigMigrations.Upgrade("""{ "schemaVersion": 99 }"""),
            "a configuration from the future is refused");

        t.Throws<ConfigException>(
            () => ConfigMigrations.Upgrade("not json"),
            "malformed JSON is a configuration error, not a crash");
        return Task.CompletedTask;
    }

    /// <summary>
    /// 仓库里 examples/ 下的示例配置必须能被本构建直接 check 通过：
    /// 它们是「开箱即跑」的承诺，如果迁移漏了一种旧写法，等于示例变成死文件。
    /// </summary>
    private static Task RepositoryExamplesValidate(TestContext t)
    {
        var dir = FindRepoRoot();
        t.Ok(dir is not null, "found the repository root above the test output directory");

        foreach (var file in Directory.EnumerateFiles(dir!, "*.json"))
        {
            var parsed = ConfigStore.TryParse(File.ReadAllText(file), file, out var document, out var problems);

            t.Ok(parsed && problems.Count == 0 && document is not null,
                $"{Path.GetFileName(file)} validates ({string.Join("; ", problems)})");
        }

        return Task.CompletedTask;
    }

    /// <summary>
    /// 控制台「新建设备」拼出来的配置必须是网关认得的配置。
    ///
    /// 页面在裁剪后的 WASM 里跑，既不能用 <c>ConfigStore</c> 也不能预演一次解析，
    /// 所以这段拼装逻辑放在域层、由这里用真的解析器过一遍：如果字段名或时长写法漂了，
    /// 现场表现会是「点创建就 422」，而不是编译错误。
    /// </summary>
    private static Task ConsoleDraftBuildsValidDocument(TestContext t)
    {
        var root = JsonNode.Parse(JsonDefaults.ToJson(DemoConfig.Build())) as JsonObject;
        t.Ok(root is not null, "the demo configuration serializes to an object");

        var channel = ConfigDraft.NewChannel("modbus-tcp-2", "二号链路", "modbus-tcp", "127.0.0.1", 15021);
        t.Ok(ConfigDraft.AddChannel(root!, channel) is null, "a new channel is appended");

        var device = ConfigDraft.NewDevice(
            "meter-2", "电表 2", "modbus-tcp-2", TimeSpan.FromMilliseconds(500), 3,
            ConfigDraft.NewRegisterPoint("voltage", "电压", "float32", "holding", 20));

        t.Ok(ConfigDraft.AddDevice(root!, device) is null, "the device references the new channel");

        // 真正的断言：页面拼出来的 JSON 交给网关自己的解析器仍然合法。
        var document = ConfigStore.Parse(root!.ToJsonString(JsonDefaults.Indented), "console-draft");

        var added = document.Devices.SingleOrDefault(d => d.Id == "meter-2");
        t.Ok(added is not null, "the new device survives the round trip");
        t.Equal(TimeSpan.FromMilliseconds(500), added!.Interval, "the duration keeps its value");
        t.Equal(3, added.UnitId, "the unit id is kept");
        t.Equal(1, added.Points.Count, "the first point is present");
        t.Equal(DataType.Float32, added.Points[0].DataType, "the data type is parsed");
        t.Equal("holding", added.Points[0].Address.Area, "the area is kept");
        t.Equal(20, added.Points[0].Address.Offset, "the offset is kept");

        var created = document.Channels.SingleOrDefault(c => c.Id == "modbus-tcp-2");
        t.Ok(created is not null, "the channel is created");
        t.Equal("tcp", created!.Transport, "the transport follows the protocol");

        // 重名与悬空引用在这里就说清楚，而不是等网关回 422。
        t.Contains(ConfigDraft.AddDevice(root, device) ?? string.Empty, "已存在",
            "a duplicate device id is rejected");
        t.Contains(
            ConfigDraft.AddDevice(root, ConfigDraft.NewDevice(
                "meter-3", null, "missing-bus", TimeSpan.FromSeconds(1), 1,
                ConfigDraft.NewRegisterPoint("p", null, "uint16", "holding", 0))) ?? string.Empty,
            "不存在", "an unknown channel is rejected");

        var serial = ConfigDraft.NewChannel("rtu-1", null, "modbus-rtu", serialPort: "/dev/ttyS0", baud: 19200);
        t.Equal("serial", serial["transport"]?.ToString(), "RTU uses the serial transport");
        t.Equal("19200", serial["serial"]?["baud"]?.ToString(), "the baud rate is written");

        t.Equal("none", ConfigDraft.NewChannel("v-1", null, "virtual")["transport"]?.ToString(),
            "virtual needs no transport");

        // S7：协议决定默认端口（102）与区域下拉内容。这两件事都在域层，
        // 页面只是照抄；写错的话现场表现是「点创建就 422」或「连 502 超时」。
        t.Equal("tcp", ConfigDraft.TransportFor("s7"), "S7 runs over ISO-on-TCP");
        t.Equal(102, ConfigDraft.DefaultPort("s7"), "S7 defaults to port 102");
        t.Equal(502, ConfigDraft.DefaultPort("modbus-tcp"), "Modbus still defaults to 502");

        var s7Channel = ConfigDraft.NewChannel("s7-1", "S7 链路", "s7", "127.0.0.1", 102);
        t.Equal("tcp", s7Channel["transport"]?.ToString(), "the S7 channel uses the TCP transport");
        t.Equal("102", s7Channel["port"]?.ToString(), "the S7 port is written");
        t.Ok(ConfigDraft.AddChannel(root!, s7Channel) is null, "the S7 channel is appended");

        var s7Device = ConfigDraft.NewDevice(
            "plc-2", "PLC 2", "s7-1", TimeSpan.FromMilliseconds(500), 1,
            ConfigDraft.NewRegisterPoint("count", "计数", "uint32", "db1", 16));
        t.Ok(ConfigDraft.AddDevice(root!, s7Device) is null, "the S7 device is appended");

        var s7Document = ConfigStore.Parse(root!.ToJsonString(JsonDefaults.Indented), "s7-draft");
        var plc = s7Document.Devices.Single(d => d.Id == "plc-2");
        t.Equal("db1", plc.Points[0].Address.Area, "the S7 area is kept verbatim (db<N>)");
        t.Equal(16, plc.Points[0].Address.Offset, "the S7 offset is a byte offset");

        // 位点位：bit 必须落进连线的地址里，否则 bool 点位读不出单个位。
        var bitPoint = ConfigDraft.NewRegisterPoint("motor", "电机", "bool", "m", 4, bit: 5);
        t.Equal(5, ConfigDraft.NewDevice("plc-3", null, "s7-1", TimeSpan.FromSeconds(1), 1, bitPoint)
            ["points"]![0]!["address"]!["bit"]!.GetValue<int>(), "the bit index is written");

        // OPC UA：NodeId 文本是地址，端口默认 4840。控制台必须拼出符号寻址，
        // 而不是把 UA 点位写成 holding/0 —— 那种点位连上去只会读回 BadNodeIdUnknown。
        t.Equal("tcp", ConfigDraft.TransportFor("opcua"), "OPC UA runs over TCP");
        t.Equal(4840, ConfigDraft.DefaultPort("opcua"), "OPC UA defaults to port 4840");
        t.Equal(502, ConfigDraft.DefaultPort("modbus-rtu"), "serial protocols keep the Modbus default port");

        var uaChannel = ConfigDraft.NewChannel("ua-1", "UA 链路", "opcua", "127.0.0.1", 4840);
        t.Equal("tcp", uaChannel["transport"]?.ToString(), "the UA channel uses the TCP transport");
        t.Ok(ConfigDraft.AddChannel(root!, uaChannel) is null, "the UA channel is appended");

        var uaDevice = ConfigDraft.NewDevice(
            "plc-ua", "UA 服务端", "ua-1", TimeSpan.FromMilliseconds(500), 1,
            ConfigDraft.NewSymbolPoint("temp", "温度", "float64", "ns=2;s=Demo.Temp"));

        t.Ok(ConfigDraft.AddDevice(root!, uaDevice) is null, "the UA device is appended");

        var uaDocument = ConfigStore.Parse(root!.ToJsonString(JsonDefaults.Indented), "opcua-draft");
        var uaPoint = uaDocument.Devices.Single(d => d.Id == "plc-ua").Points[0];

        t.Equal(AddressKind.Symbol, uaPoint.Address.Kind, "the UA point uses symbol addressing");
        t.Equal("ns=2;s=Demo.Temp", uaPoint.Address.Path, "the NodeId text is kept verbatim");
        t.Equal(0, uaPoint.Address.Index, "no namespace index is written when it is zero");

        var indexed = ConfigDraft.NewSymbolPoint("p", null, "uint16", "i=85", index: 3);
        t.Equal(3, indexed["address"]!["index"]!.GetValue<int>(), "a non-zero default namespace is written");

        // 符号点位不写 area/offset：写了会被静态校验当成寄存器地址来算长度。
        t.Ok(indexed["address"]!.AsObject().ContainsKey("area") is false,
            "symbol addressing carries no register area");

        // MQTT 南向：主题就是符号地址，端口默认 1883。控制台拼出来的点位必须能被
        // 网关解析成 topic 过滤器，而不是落回 holding/0 那种寄存器地址。
        t.Equal("tcp", ConfigDraft.TransportFor("mqtt"), "MQTT runs over TCP");
        t.Equal(1883, ConfigDraft.DefaultPort("mqtt"), "MQTT defaults to port 1883");

        var mqttChannel = ConfigDraft.NewChannel("mqtt-1", "边缘 Broker", "mqtt", "127.0.0.1", 1883);
        t.Equal("tcp", mqttChannel["transport"]?.ToString(), "the MQTT channel uses the TCP transport");
        t.Ok(ConfigDraft.AddChannel(root!, mqttChannel) is null, "the MQTT channel is appended");

        var mqttDevice = ConfigDraft.NewDevice(
            "edge-1", "边缘数据", "mqtt-1", TimeSpan.FromSeconds(1), 1,
            ConfigDraft.NewSymbolPoint("temp", "温度", "float64", "plant/line1/temp"));

        t.Ok(ConfigDraft.AddDevice(root!, mqttDevice) is null, "the MQTT device is appended");

        var mqttDocument = ConfigStore.Parse(root!.ToJsonString(JsonDefaults.Indented), "mqtt-draft");
        var mqttPoint = mqttDocument.Devices.Single(d => d.Id == "edge-1").Points[0];

        t.Equal(AddressKind.Symbol, mqttPoint.Address.Kind, "the MQTT point uses symbol addressing");
        t.Equal("plant/line1/temp", mqttPoint.Address.Path, "the topic filter is kept verbatim");
        t.Ok(mqttPoint.Address.Property is null,
            "no field override is written when the device default applies");

        t.Equal("500ms", ConfigDraft.FormatDuration(TimeSpan.FromMilliseconds(500)),
            "sub-second durations stay readable");
        t.Equal("2m", ConfigDraft.FormatDuration(TimeSpan.FromMinutes(2)),
            "minutes use the short form");

        return Task.CompletedTask;
    }

    /// <summary>
    /// FR-CFG-01 的模型一半：四类寻址共用同一个 <see cref="Address"/>，各自必填/禁止的
    /// 字段由 <see cref="Address.Validate"/> 收口。
    ///
    /// 这里断言的是「模型真的认这四种、且拒绝混填」，不是「已经有驱动去读对象/文件」：
    /// object 要等 BACnet/CIP，file 要等带文件记录语义的协议。把它们混填进寄存器点位
    /// 是跨协议复制粘贴最常见的错误，必须在配置阶段就指出具体字段。
    /// </summary>
    private static Task FourAddressKindsValidate(TestContext t)
    {
        static string Document(string address) => """
            {
              "schemaVersion": 1,
              "site": "kinds",
              "channels": [{ "id": "ch", "protocol": "virtual", "transport": "none" }],
              "devices": [{
                "id": "d1", "channelId": "ch", "interval": "1s",
                "points": [ { "id": "p", "address": ADDRESS, "dataType": "float32" } ]
              }]
            }
            """.Replace("ADDRESS", address);

        var register = ConfigStore.Parse(Document(
            """{ "kind": "register", "area": "holding", "offset": 4 }"""), "register.json");
        t.Equal(AddressKind.Register, register.Devices[0].Points[0].Address.Kind,
            "register addressing is accepted");

        var symbol = ConfigStore.Parse(Document(
            """{ "kind": "symbol", "path": "ns=2;s=Demo.Temp" }"""), "symbol.json");
        t.Equal("ns=2;s=Demo.Temp", symbol.Devices[0].Points[0].Address.Path,
            "symbol addressing keeps the path verbatim");

        var obj = ConfigStore.Parse(Document("""
            { "kind": "object", "objectType": "analogInput", "instance": 3, "property": "presentValue" }
            """), "object.json");
        t.Equal(AddressKind.Object, obj.Devices[0].Points[0].Address.Kind,
            "object addressing carries type/instance/property");
        t.Equal("analogInput", obj.Devices[0].Points[0].Address.ObjectType, "the object type survives");
        t.Equal(3, obj.Devices[0].Points[0].Address.Instance, "the instance survives");

        var file = ConfigStore.Parse(Document(
            """{ "kind": "file", "fileId": 2, "recordId": 7 }"""), "file.json");
        t.Equal(2, file.Devices[0].Points[0].Address.FileId, "file addressing carries file and record ids");

        // 稳定键决定批量规划的分组：四种寻址不能算出同一个键。
        var keys = new[] { register, symbol, obj, file }
            .Select(d => d.Devices[0].Points[0].Address.StableKey())
            .ToArray();
        t.Equal(4, keys.Distinct(StringComparer.Ordinal).Count(), "each kind has its own stable key");

        // 混填必须在配置阶段被拒绝，并指出是哪一类多出来的字段。
        void ExpectRejected(string address, string path, string what)
        {
            try
            {
                ConfigStore.Parse(Document(address), "mixed.json");
                t.Fail($"{what}: a mixed address was accepted");
            }
            catch (ConfigException ex)
            {
                t.Contains(string.Join("; ", ex.Problems), path, what);
            }
        }

        ExpectRejected("""{ "kind": "register", "area": "holding", "path": "ns=2;s=X" }""",
            "address.path", "a path on a register address is rejected");
        ExpectRejected("""{ "kind": "symbol", "path": "s=X", "area": "holding" }""",
            "address.area", "an area on a symbol address is rejected");
        ExpectRejected("""{ "kind": "object", "instance": 1, "property": "p" }""",
            "address.objectType", "an object address needs its type");
        ExpectRejected("""{ "kind": "file", "fileId": 1, "recordId": 2, "area": "db1" }""",
            "address.area", "an area on a file address is rejected");

        return Task.CompletedTask;
    }

    /// <summary>
    /// FR-CFG-01 的验收口径：**同一套模板可生成 Modbus 与 S7 设备**。
    ///
    /// 两者线上都是「寄存器 + 区名 + 偏移」，差别只在区名（holding / db1）与偏移单位
    /// （字 / 字节）。模板里把这两个值参数化，就能用同一份骨架产出两台设备——这正是
    /// 「四类寻址统一模型」要买到的东西：模板不必为每个协议各写一份。
    /// </summary>
    private static Task OneTemplateTargetsModbusAndS7(TestContext t)
    {
        // 区名与偏移都来自参数：Modbus 用字偏移，S7 用字节偏移。
        var template = Template("""
            {
              "schemaVersion": 1,
              "id": "meter",
              "parameters": [
                { "name": "id", "type": "string", "required": true },
                { "name": "channel", "type": "string", "required": true },
                { "name": "area", "type": "string", "required": true },
                { "name": "offset", "type": "int", "required": true }
              ],
              "device": {
                "id": "{id}",
                "channelId": "{channel}",
                "interval": "1s",
                "points": [
                  {
                    "id": "voltage",
                    "address": { "kind": "register", "area": "{area}", "offset": "{offset}" },
                    "dataType": "float32"
                  }
                ]
              }
            }
            """);

        var channels = new[] { "modbus-bus", "s7-bus" };

        var modbus = TemplateInstantiator.Instantiate(template, new Dictionary<string, string?>
        {
            ["id"] = "meter-1",
            ["channel"] = "modbus-bus",
            ["area"] = "holding",
            ["offset"] = "20",
        }, channels);
        t.Ok(modbus.Ok, $"the Modbus instance is valid: {string.Join("; ", modbus.Problems)}");

        var s7 = TemplateInstantiator.Instantiate(template, new Dictionary<string, string?>
        {
            ["id"] = "plc-1",
            ["channel"] = "s7-bus",
            ["area"] = "db1",
            ["offset"] = "16",
        }, channels);
        t.Ok(s7.Ok, $"the S7 instance is valid: {string.Join("; ", s7.Problems)}");

        // 两份实例除了参数值之外完全同构：区名与偏移来自模板，而不是按协议写的分支。
        t.Equal("modbus-bus", modbus.Device!.ChannelId, "the Modbus instance keeps its channel");
        t.Equal("holding", modbus.Device.Points[0].Address.Area, "the Modbus area comes from the parameter");
        t.Equal(20, modbus.Device.Points[0].Address.Offset, "the Modbus offset is a word offset");

        t.Equal("s7-bus", s7.Device!.ChannelId, "the S7 instance keeps its channel");
        t.Equal("db1", s7.Device.Points[0].Address.Area, "the S7 area comes from the same parameter");
        t.Equal(16, s7.Device.Points[0].Address.Offset, "the S7 offset is a byte offset");

        // 真正的断言：两台设备放进同一份配置后仍然合法，而不是「各自能反序列化」。
        var root = new JsonObject
        {
            ["schemaVersion"] = 1,
            ["site"] = "mixed",
            ["channels"] = new JsonArray
            {
                new JsonObject
                {
                    ["id"] = "modbus-bus", ["protocol"] = "modbus-tcp", ["transport"] = "tcp",
                    ["host"] = "127.0.0.1", ["port"] = 1502,
                },
                new JsonObject
                {
                    ["id"] = "s7-bus", ["protocol"] = "s7", ["transport"] = "tcp",
                    ["host"] = "127.0.0.1", ["port"] = 102,
                },
            },
            ["devices"] = new JsonArray(),
        };

        t.Ok(ConfigDraft.AddDevice(root,
                JsonSerializer.SerializeToNode(modbus.Device, JsonDefaults.Options)!.AsObject()) is null,
            "the Modbus device is appended");
        t.Ok(ConfigDraft.AddDevice(root,
                JsonSerializer.SerializeToNode(s7.Device, JsonDefaults.Options)!.AsObject()) is null,
            "the S7 device is appended");

        var document = ConfigStore.Parse(root.ToJsonString(JsonDefaults.Indented), "one-template.json");
        t.Equal(2, document.Devices.Count, "both protocol variants landed in one document");

        var plc = document.Devices.Single(d => d.Id == "plc-1");
        t.Equal("db1", plc.Points[0].Address.Area, "the document keeps the S7 area");
        t.Equal(0, ModelValidator.Validate(document).Count,
            "the document produced by one template passes validation for both protocols");

        return Task.CompletedTask;
    }

    /// <summary>
    /// v0 时代留下的形状：transport 是对象、concurrency、channel（而不是 channelId）、
    /// storage.dir、server.token。这些写法在旧示例里大量存在，必须能读进来。
    /// </summary>
    private static Task MigratesLegacyShapes(TestContext t)
    {
        const string legacy = """
                              {
                                "schemaVersion": 1,
                                "site": "legacy",
                                "server": {
                                  "listen": "0.0.0.0:9090",
                                  "token": "legacy-token"
                                },
                                "storage": { "dir": "./data-legacy" },
                                "channels": [
                                  {
                                    "id": "ch-modbus",
                                    "protocol": "modbus-tcp",
                                    "transport": { "kind": "tcp", "address": "10.0.0.5:15020" },
                                    "concurrency": 3,
                                    "timeout": "2s"
                                  },
                                  {
                                    "id": "ch-rtu",
                                    "protocol": "modbus-rtu",
                                    "transport": {
                                      "kind": "serial",
                                      "port": "/dev/ttyUSB0",
                                      "baud": 9600,
                                      "dataBits": 8,
                                      "parity": "none",
                                      "stopBits": 1
                                    },
                                    "concurrency": 1,
                                    "timeout": "1s"
                                  }
                                ],
                                "devices": [
                                  {
                                    "id": "d1",
                                    "channel": "ch-modbus",
                                    "unitId": 1,
                                    "interval": "1s",
                                    "points": [
                                      { "id": "p1", "address": { "kind": "register", "area": "holding", "offset": 0 }, "dataType": "uint16" }
                                    ]
                                  },
                                  {
                                    "id": "d2",
                                    "channel": "ch-rtu",
                                    "unitId": 1,
                                    "interval": "1s",
                                    "points": [
                                      { "id": "p2", "address": { "kind": "register", "area": "holding", "offset": 0 }, "dataType": "uint16" }
                                    ]
                                  }
                                ]
                              }
                              """;

        var migration = ConfigMigrations.Upgrade(legacy);

        var config = migration.Root;
        var channels = config["channels"]!.AsArray();
        var devices = config["devices"]!.AsArray();

        t.Ok(channels.Count == 2, "both channels survived the migration");

        var tcp = channels[0]!;
        t.Equal("tcp", tcp["transport"]!.GetValue<string>(), "a tcp transport object becomes a string");
        t.Equal("10.0.0.5", tcp["host"]!.GetValue<string>(), "the address is split into host");
        t.Equal(15020, tcp["port"]!.GetValue<int>(), "the address is split into port");
        t.Equal(3, tcp["maxConcurrency"]!.GetValue<int>(), "concurrency becomes maxConcurrency");
        t.Ok(tcp["concurrency"] is null, "the legacy field is removed");

        var rtu = channels[1]!;
        t.Equal("serial", rtu["transport"]!.GetValue<string>(), "a serial transport object becomes serial");
        t.Equal("one", rtu["serial"]!.AsObject()["stopBits"]!.GetValue<string>(),
            "a numeric stop bit is spelled out");
        t.Equal("/dev/ttyUSB0", rtu["serial"]!.AsObject()["port"]!.GetValue<string>(), "the serial port is flattened");

        t.Equal("legacy-token", config["northbound"]!.AsObject()["rest"]!.AsObject()["token"]!.GetValue<string>(),
            "server.token moves to the northbound token");
        t.Equal("0.0.0.0:9090", config["northbound"]!.AsObject()["rest"]!.AsObject()["listen"]!.GetValue<string>(),
            "server.listen moves to northbound.rest");
        t.Ok(config["server"] is null, "the legacy section is removed");
        t.Equal("./data-legacy", config["storage"]!.AsObject()["dataDir"]!.GetValue<string>(),
            "storage.dir becomes dataDir");

        t.Equal("ch-modbus", devices[0]!.AsObject()["channelId"]!.GetValue<string>(),
            "the device points at the migrated channel id");
        t.Ok(devices[0]!.AsObject()["channel"] is null, "the legacy device field is removed");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 控制台读到的配置是脱敏的；把 <c>"***"</c> 原样提交回去时必须还原成当前密钥。
    /// 否则一次无关的编辑（加设备、改名）就会把 API 令牌改成字面量，等于锁死入口。
    /// </summary>
    private static Task RedactedSecretsAreRestored(TestContext t)
    {
        var node = JsonNode.Parse(JsonDefaults.ToJson(DemoConfig.Build()))!.AsObject();
        node["northbound"]!["rest"]!["token"] = "api-token";
        node["northbound"]!["mqtt"]!["username"] = "mqtt-user";
        node["northbound"]!["mqtt"]!["password"] = "mqtt-secret";

        var current = ConfigStore.Parse(node.ToJsonString());

        var redacted = RestApi.Redact(current);
        t.Equal(RedactedSecrets.Placeholder, redacted["northbound"]!["rest"]!["token"]!.GetValue<string>(),
            "the read-back token is a placeholder");

        // 模拟控制台改一个与安全无关的字段，其余（含占位符）原样提交。
        redacted["devices"]![0]!["name"] = "改过的设备名";

        var restored = ConfigStore.Parse(
            RedactedSecrets.Restore(redacted.ToJsonString(), current));

        t.Equal("api-token", restored.Northbound.Rest.Token, "the API token is restored");
        t.Equal("mqtt-secret", restored.Northbound.Mqtt.Password, "the MQTT password is restored");
        t.Equal("mqtt-user", restored.Northbound.Mqtt.Username, "non-secret fields are untouched");
        t.Equal("改过的设备名", restored.Devices[0].Name, "the intended edit survives");

        // 显式清空口令不能被「还原」回来，否则用户永远删不掉一个泄漏的凭据。
        redacted["northbound"]!["rest"]!["token"] = "";
        var cleared = ConfigStore.Parse(
            RedactedSecrets.Restore(redacted.ToJsonString(), current));
        t.Equal(string.Empty, cleared.Northbound.Rest.Token ?? string.Empty,
            "an explicitly cleared token stays cleared");

        return Task.CompletedTask;
    }

    /// <summary>Go 风格的复合时长（"1h0m0s"、"1h30m"）要能读回来，否则旧示例直接起不来。</summary>
    private static Task CompositeDurationsParse(TestContext t)
    {
        t.Equal(TimeSpan.FromHours(1), DurationConverter.Parse("1h0m0s"), "hour/minute/second spelling");
        t.Equal(TimeSpan.FromMinutes(90), DurationConverter.Parse("1h30m"), "minute-only spelling");
        t.Equal(TimeSpan.FromSeconds(150), DurationConverter.Parse("2m30s"), "compound minutes and seconds");
        t.Equal(TimeSpan.FromMilliseconds(500), DurationConverter.Parse("500ms"), "single unit still works");
        t.Equal(TimeSpan.FromMilliseconds(250), DurationConverter.Parse("250"), "a bare number is milliseconds");
        t.Equal(TimeSpan.FromDays(3), DurationConverter.Parse("3d"), "days");
        return Task.CompletedTask;
    }

    private static string? FindRepoRoot()
    {
        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
        {
            if (Directory.Exists(Path.Combine(dir.FullName, "examples")) &&
                File.Exists(Path.Combine(dir.FullName, "dotnet", "global.json")))
            {
                return Path.Combine(dir.FullName, "examples");
            }
        }

        return null;
    }
}
