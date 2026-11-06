using System.Text.Json.Nodes;
using Uic.Config;
using Uic.Domain;

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
        tests.Add(new TestCase("config/deadband_and_range_rules", DeadbandAndRangeRules));
        tests.Add(new TestCase("config/migration_is_explicit_and_versioned", MigrationIsExplicitAndVersioned));
        tests.Add(new TestCase("config/migrates_v0_legacy_shapes", MigratesLegacyShapes));
        tests.Add(new TestCase("config/composite_durations_parse", CompositeDurationsParse));
        tests.Add(new TestCase("config/repository_examples_validate", RepositoryExamplesValidate));
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

    private const string TestPointAddress =
        "\"address\": { \"kind\": \"register\", \"area\": \"holding\", \"offset\": 0 }";

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
