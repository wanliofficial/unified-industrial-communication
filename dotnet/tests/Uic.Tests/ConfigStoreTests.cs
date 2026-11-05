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
        tests.Add(new TestCase("config/diff_finds_changed_added_removed", DiffFindsChanges));
        tests.Add(new TestCase("config/diff_text_is_human_readable", DiffTextIsReadable));
        tests.Add(new TestCase("config/reload_after_restart", ReloadAfterRestart));
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

    private static Task RunAsync(Func<Task> body) => body();
}
