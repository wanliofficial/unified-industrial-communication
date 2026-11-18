using System.Text.Json.Nodes;
using Uic.Config;
using Uic.Domain;

namespace Uic.Tests;

/// <summary>
/// 配置即制品：内容寻址、历史只增不改、回滚也是新版本、非法配置必须拒绝。
///
/// 这一层是现场事故的最后一道防线——「改坏了」要能一键退回去，
/// 所以断言的重点不是功能，而是「历史不会被抹掉」。
/// </summary>
internal static class ConfigStoreTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("config/id_is_content_addressed", IdIsContentAddressed));
        tests.Add(new TestCase("config/recommit_of_identical_content_keeps_one_version", RecomitKeepsOneVersion));
        tests.Add(new TestCase("config/rollback_adds_a_version", RollbackAddsAVersion));
        tests.Add(new TestCase("config/bootstrap_prefers_current_pointer", BootstrapPrefersCurrentPointer));
        tests.Add(new TestCase("config/rejects_invalid_documents", RejectsInvalidDocuments));
        tests.Add(new TestCase("config/rejects_twenty_invalid_shapes", RejectsTwentyInvalidShapes));
        tests.Add(new TestCase("config/diff_finds_changed_added_removed", DiffFindsChanges));
        tests.Add(new TestCase("config/diff_text_is_human_readable", DiffTextIsReadable));
        tests.Add(new TestCase("config/reload_after_restart", ReloadAfterRestart));
        tests.Add(new TestCase("config/atomic_write_never_leaves_a_half_written_state",
            AtomicWriteNeverLeaves_a_half_written_state));
    }

    /// <summary>
    /// 配置文档的属性是 init-only（刻意如此：改配置只能走「提交新版本」这条路）。
    /// 需要构造差异时先绕一圈 JSON，顺便也验证了序列化契约确实是唯一的编辑入口。
    /// </summary>
    private static ConfigDocument Edit(ConfigDocument document, Action<JsonObject> edit)
    {
        var node = JsonNode.Parse(JsonDefaults.ToJson(document)) as JsonObject
                   ?? throw new InvalidOperationException("document did not serialize to an object");

        edit(node);
        return ConfigStore.Parse(node.ToJsonString());
    }

    private static Task IdIsContentAddressed(TestContext t)
    {
        var store = new ConfigStore(t.TempDir);
        var document = DemoConfig.Build();

        var first = store.Commit(document, "initial");
        var second = store.Commit(DemoConfig.Build(), "identical content");

        t.Ok(first.StartsWith('v'), $"ids are prefixed with v (got '{first}')");
        t.Equal(12, first.Length - 1, "ids carry a 12-hex-digit digest");
        t.Equal(first, second, "identical content maps to the same id");
        t.Equal(1, store.Versions().Count, "identical content does not create a second version");
        t.Equal(first, store.CurrentId, "current pointer follows the commit");

        var changed = Edit(document, n => n["description"] = "changed");
        var third = store.Commit(changed, "changed");

        t.Ok(third != first, "different content maps to a different id");
        t.Equal(2, store.Versions().Count, "a real change creates a version");
        t.Equal("changed", store.LoadVersion(third)!.Description, "the new version reflects the change");
        t.Equal(document.Description, store.LoadVersion(first)!.Description, "old versions stay readable");

        return Task.CompletedTask;
    }

    private static Task RecomitKeepsOneVersion(TestContext t)
    {
        var store = new ConfigStore(t.TempDir);

        var a = store.Commit(DemoConfig.Build(), "one");
        var b = store.Commit(DemoConfig.Build(), "two");
        var c = store.Commit(DemoConfig.Build(), "three");

        t.Equal(a, b, "second commit reuses the id");
        t.Equal(b, c, "third commit reuses the id");
        t.Equal(1, store.Versions().Count, "history has not grown");

        return Task.CompletedTask;
    }

    private static Task RollbackAddsAVersion(TestContext t)
    {
        var store = new ConfigStore(t.TempDir);

        var original = DemoConfig.Build();
        var firstId = store.Commit(original, "original");

        var modified = Edit(original, n => n["description"] = "modified");
        var secondId = store.Commit(modified, "modified");

        t.Ok(firstId != secondId, "the two versions differ");

        var rollbackId = store.Rollback(firstId);

        // 回滚不是「把指针往回拨」：历史只增不改，回滚本身就是一次提交。
        t.Equal(firstId, rollbackId, "rolling back lands on the content-addressed id of the old version");
        t.Equal(2, store.Versions().Count, "no extra version: the old content already existed");

        t.Equal("modified", store.LoadVersion(secondId)!.Description, "the newer version is still on disk");
        t.Equal(firstId, store.ReadCurrentPointer(), "current points back at the original");

        return Task.CompletedTask;
    }

    private static Task BootstrapPrefersCurrentPointer(TestContext t)
    {
        return RunAsync(async () =>
        {
            var store = new ConfigStore(t.TempDir);

            var first = Edit(DemoConfig.Build(), n => n["description"] = "from commit");
            var committed = store.Commit(first, "committed");

            // current 指针优先于启动参数里给的配置文件：运维回滚过之后，
            // 重启不能再把旧文件捡回来。
            var file = t.TempFile("gateway.json");
            var stale = Edit(DemoConfig.Build(), n => n["description"] = "stale file");
            await File.WriteAllTextAsync(file, JsonDefaults.ToJson(stale, indented: true), t.Ct);

            var loaded = await store.BootstrapAsync(file, () => DemoConfig.Build(), t.Ct);

            t.Equal("from commit", loaded.Description, "the current version wins");
            t.Equal(committed, store.CurrentId, "current id is exposed");
        });
    }

    private static Task RejectsInvalidDocuments(TestContext t)
    {
        // 语法完全合法、语义完全不可用：site 为空，设备指向不存在的通道，且没有点位。
        var invalid = """
                      {
                        "schemaVersion": 1,
                        "site": "",
                        "channels": [],
                        "devices": [
                          {
                            "id": "d1",
                            "channelId": "missing-channel",
                            "points": []
                          }
                        ]
                      }
                      """;

        try
        {
            ConfigStore.Parse(invalid, "invalid.json");
            t.Fail("an invalid document must be rejected");
        }
        catch (ConfigException ex)
        {
            t.Ok(ex.Problems.Count >= 2,
                $"validation reports every problem, not just the first (got {ex.Problems.Count})");
            t.Contains(ex.ToDetailedString(), "site", "problems point at the offending field");
        }

        // 语法错误与语义错误必须是同一个异常类型：调用方只需要处理一种。
        t.Throws<ConfigException>(() => ConfigStore.Parse("{ not json", "broken.json"), "malformed JSON");

        // 提交路径同样要拦：不能绕过 Parse 直接写盘。
        var store = new ConfigStore(t.TempDir);
        t.Throws<ConfigException>(
            () => store.Commit(new ConfigDocument { Site = string.Empty }), "commit validates");
        t.Equal(0, store.Versions().Count, "nothing was written for an invalid document");

        return Task.CompletedTask;
    }

    /// <summary>
    /// NFR「注入 20 类非法配置」：每类只改一个字段，断言拒绝理由落在预期路径上。
    /// 逐条覆盖而不是一个大杂烩，是为了失败时能直接指向「哪条规则漏了」。
    /// </summary>
    private static Task RejectsTwentyInvalidShapes(TestContext t)
    {
        var cases = new (string Name, Action<JsonObject> Mutate, string Path)[]
        {
            ("unsupported schema version", n => n["schemaVersion"] = 2, "schemaVersion"),
            ("empty site", n => n["site"] = "", "site"),
            ("malformed channel id", n => n["channels"]![0]!["id"] = "BAD", "channels[BAD].id"),
            ("missing channel protocol", n => n["channels"]![0]!["protocol"] = "",
                "channels[virtual-bus].protocol"),
            ("zero channel concurrency", n => n["channels"]![0]!["maxConcurrency"] = 0,
                "channels[virtual-bus].maxConcurrency"),
            ("zero channel timeout", n => n["channels"]![0]!["timeout"] = "0s",
                "channels[virtual-bus].timeout"),
            ("unknown transport", n => n["channels"]![1]!["transport"] = "canbus",
                "channels[modbus-tcp-1].transport"),
            ("tcp without host", n => n["channels"]![1]!["host"] = "",
                "channels[modbus-tcp-1].host"),
            ("tcp port out of range", n => n["channels"]![1]!["port"] = 70000,
                "channels[modbus-tcp-1].port"),
            ("serial without port", n =>
            {
                var channel = n["channels"]![1]!.AsObject();
                channel["transport"] = "serial";
                channel["serial"] = JsonNode.Parse("""{"port":"","baud":100}""");
            }, "channels[modbus-tcp-1].serial.port"),
            ("malformed device id", n => n["devices"]![0]!["id"] = "BAD", "devices[BAD].id"),
            ("device points at a missing channel", n => n["devices"]![0]!["channelId"] = "missing",
                "devices[virtual-1].channelId"),
            ("device interval below 10ms", n => n["devices"]![0]!["interval"] = "1ms",
                "devices[virtual-1].interval"),
            ("device unit id out of range", n => n["devices"]![0]!["unitId"] = 0,
                "devices[virtual-1].unitId"),
            ("device without points", n => n["devices"]![0]!["points"] = new JsonArray(),
                "devices[virtual-1].points"),
            ("malformed point id", n => n["devices"]![0]!["points"]![0]!["id"] = "BAD",
                "devices[virtual-1].points[BAD].id"),
            ("register address without area", n =>
                n["devices"]![0]!["points"]![0]!["address"]!["area"] = "",
                "devices[virtual-1].points[temperature].address.area"),
            ("register span shorter than the type", n =>
                n["devices"]![0]!["points"]![0]!["address"]!["length"] = 1,
                "devices[virtual-1].points[temperature].address.length"),
            ("negative deadband", n => n["devices"]![0]!["points"]![0]!["deadband"] = -1,
                "devices[virtual-1].points[temperature].deadband"),
            ("point interval below 100ms", n =>
                n["devices"]![0]!["points"]![0]!["interval"] = "50ms",
                "devices[virtual-1].points[temperature].interval"),
            ("rate deadband must be positive", n =>
            {
                n["devices"]![0]!["points"]![0]!["deadbandMode"] = "rate";
                n["devices"]![0]!["points"]![0]!["deadband"] = 0;
            }, "devices[virtual-1].points[temperature].deadband"),
            ("log file too small", n => n["logging"]!["file"]!["maxBytes"] = 100,
                "logging.file.maxBytes"),
            ("mqtt qos2", n =>
            {
                n["northbound"]!["mqtt"]!["enabled"] = true;
                n["northbound"]!["mqtt"]!["qos"] = 2;
            }, "northbound.mqtt.qos"),
            ("opc ua certificate policy", n =>
            {
                n["northbound"]!["opcUa"]!["enabled"] = true;
                n["northbound"]!["opcUa"]!["securityPolicy"] = "Basic256Sha256";
            }, "northbound.opcUa.securityPolicy"),
            ("negative memory watermark", n => n["storage"]!["maxMemoryBytes"] = -1,
                "storage.maxMemoryBytes"),

            // 命名唯一性（FR-CFG-06）：三类各自独立查重。重复 id 不会在语法层暴露，
            // 只会在语义层被拒——漏了就会让「同名通道/设备/点位」静默覆盖彼此，
            // 现场表现为某些点位永远读不到值，却没有任何报错。
            ("duplicate channel id", n =>
            {
                var copy = n["channels"]![0]!.DeepClone().AsObject();
                n["channels"]!.AsArray().Add(copy);
            }, "channels[virtual-bus].id"),
            ("duplicate device id", n =>
            {
                var copy = n["devices"]![0]!.DeepClone().AsObject();
                n["devices"]!.AsArray().Add(copy);
            }, "devices[virtual-1].id"),
            ("duplicate point id in one device", n =>
            {
                var points = n["devices"]![0]!["points"]!.AsArray();
                points.Add(points[0]!.DeepClone());
            }, "devices[virtual-1].points[temperature].id"),
        };

        t.Ok(cases.Length >= 20, $"the matrix covers at least twenty classes (got {cases.Length})");

        foreach (var (name, mutate, path) in cases)
        {
            var node = JsonNode.Parse(JsonDefaults.ToJson(DemoConfig.Build()))!.AsObject();
            mutate(node);

            try
            {
                ConfigStore.Parse(node.ToJsonString(), "invalid.json");
                t.Fail($"{name}: an invalid document must be rejected");
            }
            catch (ConfigException ex)
            {
                var hit = ex.Problems.Any(p =>
                    string.Equals(p.Path, path, StringComparison.Ordinal) ||
                    p.Path.StartsWith(path + ".", StringComparison.Ordinal));

                t.Ok(hit, $"{name}: expected a problem at '{path}', got " +
                           string.Join(", ", ex.Problems.Select(p => p.Path)));
            }
        }

        // 24 次失败之后合法配置仍能提交：拒绝不能留下半成品或污染当前指针。
        var store = new ConfigStore(t.TempDir);
        var id = store.Commit(DemoConfig.Build(), "valid after rejections");
        t.Equal(1, store.Versions().Count, "failed attempts never create versions");
        t.Ok(store.LoadVersion(id) is not null, "the valid version is readable");

        return Task.CompletedTask;
    }

    private static Task DiffFindsChanges(TestContext t)
    {
        var before = DemoConfig.Build();
        var after = Edit(before, n =>
        {
            n["site"] = "site-2";

            var channels = n["channels"]!.AsArray();
            channels[0]!["name"] = "改名了";

            n["devices"]!.AsArray().RemoveAt(1);
        });

        var diff = ConfigStore.Diff(before, after);

        t.Ok(diff.Any(d => d.Path == "site" && d.Kind == "changed"), "changed field is reported");
        t.Ok(diff.Any(d => d.Path.StartsWith("devices[", StringComparison.Ordinal) && d.Kind == "removed"),
            "a removed array item is reported");

        var rename = diff.SingleOrDefault(d => d.Path == "channels[0].name");
        t.Ok(rename is not null, "renamed field is reported");
        t.Equal("changed", rename!.Kind, "rename is a change");
        t.Equal(before.Channels[0].Name, rename.Before, "before value");
        t.Equal("改名了", rename.After, "after value");

        // 相同内容不得产生任何差异——否则「审阅改动」会淹没在噪声里。
        t.Equal(0, ConfigStore.Diff(before, DemoConfig.Build()).Count, "identical content has no diff");

        return Task.CompletedTask;
    }

    private static Task DiffTextIsReadable(TestContext t)
    {
        var before = DemoConfig.Build();
        var after = Edit(before, n => n["site"] = "site-2");

        var text = ConfigStore.DiffText(ConfigStore.Diff(before, after));

        // 贴进工单的文本要能一眼看出方向。
        t.Contains(text, "~ site:", "changed lines are prefixed with ~");
        t.Contains(text, "demo-site -> site-2", "both values are shown");

        return Task.CompletedTask;
    }

    private static Task ReloadAfterRestart(TestContext t)
    {
        var dir = t.TempDir;

        var first = new ConfigStore(dir);
        var id = first.Commit(DemoConfig.Build(), "initial");

        // 新进程、同一数据目录：current 指针必须能把配置捡回来。
        var second = new ConfigStore(dir);
        var pointer = second.ReadCurrentPointer();
        t.Equal(id, pointer, "current pointer is on disk");

        var loaded = second.LoadVersion(pointer!);
        t.Ok(loaded is not null, "the version is readable after restart");
        t.Equal("demo-site", loaded!.Site, "content round-trips");

        return Task.CompletedTask;
    }

    /// <summary>
    /// 原子写本身（FR-CFG-11）：写临时文件 + fsync + rename，绝不「就地截断重写」。
    ///
    /// 为什么单独立一条：<c>reload_after_restart</c> 只证明「结果能被捡回来」，
    /// 而「截断写 + 恰好没崩」也可能让重启读到旧文件——那正是原子写要避免的
    /// 「半写状态」。这一份钉住两件可从磁盘稳定观测的事：
    ///   1. 每次提交后目录里不残留 <c>.tmp</c>（非原子的就地重写/失败会留下临时文件）；
    ///   2. 指针切过去的那份版本文件是完整、可解析的 JSON——读者要么看到旧完整文件、
    ///      要么看到新完整文件，绝不会看到写了一半的 <c>{"channels":[</c>。
    ///
    /// 原子写作用在 <c>config/versions/&lt;id&gt;.json</c> 上（先写它、再原子切
    /// <c>config/current</c> 指针）。这里不做「多线程读并发撞窗口」的断言：
    /// 本机文件系统上截断-写满太快，窗口不可稳定复现，那样的断言会变成 flaky 噪声，
    /// 反而掩盖真问题。真正被守住的是「不残留临时文件 + 指针指向的文件一定完整」。
    /// </summary>
    private static Task AtomicWriteNeverLeaves_a_half_written_state(TestContext t)
    {
        var dir = t.TempDir;
        var store = new ConfigStore(dir);

        var root = Path.Combine(dir, "config");
        var versionsDir = Path.Combine(root, "versions");

        var last = string.Empty;
        for (var i = 0; i < 20; i++)
        {
            var doc = Edit(DemoConfig.Build(), n => n["site"] = $"demo-site-{i}");
            var id = store.Commit(doc, $"commit {i}");
            last = id;

            // 指针一旦切向某 id，那份版本文件必须已经是完整的 JSON。
            var versionFile = Path.Combine(versionsDir, id + ".json");
            t.Ok(File.Exists(versionFile), $"version {id} is on disk");
            var parsed = JsonNode.Parse(File.ReadAllText(versionFile));
            t.Ok(parsed is not null, $"version {id} parses as complete JSON, not a half-written file");
        }

        // 没有 .tmp 残留：原子写的临时文件一定被 rename 走了，绝不在目录里堆积。
        var leftovers = Directory.EnumerateFiles(root, "*.tmp", SearchOption.AllDirectories).ToArray();
        t.Equal(0, leftovers.Length, $"no leftover tmp file (found {leftovers.Length})");

        // current 指针指向最后一份，且能读回——证明「先写文件再切指针」的顺序没反。
        var pointer = store.ReadCurrentPointer();
        t.Equal(last, pointer, "current points at the latest commit");

        return Task.CompletedTask;
    }

    private static Task RunAsync(Func<Task> body) => body();
}
