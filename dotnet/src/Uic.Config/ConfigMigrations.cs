using System.Text.Json;
using System.Text.Json.Nodes;
using Uic.Domain;

namespace Uic.Config;

/// <summary>一次迁移的结果。<see cref="Applied"/> 为空表示原样通过。</summary>
public sealed record MigrationResult(JsonObject Root, string Json, IReadOnlyList<string> Applied);

/// <summary>
/// 配置迁移链。
///
/// 现场升级程序时，配置文件通常还是旧版本写出来的。原则（域模型 §10.2）：
/// 1. 等于当前版本 → 直接用；
/// 2. 小于当前版本 → 按链迁移，迁移结果就是新配置；
/// 3. 大于当前版本 → 拒绝启动，不能「猜」新字段的语义。
///
/// 这一版只有 v1，但 v0 时代留下的 <c>server.listen</c> 与
/// <c>northbound.mqtt.broker</c> 仍然要能读进来——它们是文档里出现过的真实写法。
/// </summary>
public static class ConfigMigrations
{
    public const int CurrentVersion = 1;

    public static MigrationResult Upgrade(string json)
    {
        JsonNode? node;

        try
        {
            node = JsonNode.Parse(json, documentOptions: new JsonDocumentOptions
            {
                CommentHandling = JsonCommentHandling.Skip,
                AllowTrailingCommas = true,
            });
        }
        catch (JsonException ex)
        {
            throw new ConfigException(
                $"configuration is not valid JSON: {ex.Message}",
                new[] { new ValidationProblem("$", ex.Message) });
        }

        if (node is not JsonObject root)
        {
            throw new ConfigException(
                "configuration must be a JSON object",
                new[] { new ValidationProblem("$", "expected an object") });
        }

        var version = ReadVersion(root);
        if (version > CurrentVersion)
        {
            throw new ConfigException(
                $"configuration schema version {version} is newer than this build ({CurrentVersion})",
                new[]
                {
                    new ValidationProblem("schemaVersion",
                        "upgrade the gateway binary; newer configuration is not downgraded"),
                });
        }

        var applied = new List<string>();

        // v0 的 server.listen / server.token → northbound.rest.*。
        if (root["server"] is JsonObject server)
        {
            var listen = ReadLegacyString(server, "listen");

            if (!string.IsNullOrWhiteSpace(listen))
            {
                var rest = EnsureRest(root);
                if (rest["listen"] is null)
                {
                    rest["listen"] = listen;
                    applied.Add("v0.server.listen -> northbound.rest.listen");
                }
            }

            var token = ReadLegacyString(server, "token");

            if (!string.IsNullOrWhiteSpace(token))
            {
                var rest = EnsureRest(root);
                if (rest["token"] is null)
                {
                    rest["token"] = token;
                    applied.Add("v0.server.token -> northbound.rest.token");
                }
            }

            root.Remove("server");
        }

        MigrateChannels(root, applied);
        MigrateDevices(root, applied);

        // v0 的 storage.dir → storage.dataDir。
        if (root["storage"] is JsonObject storage &&
            storage["dir"] is JsonValue dirValue &&
            dirValue.TryGetValue<string>(out var dir) &&
            !string.IsNullOrWhiteSpace(dir))
        {
            storage["dataDir"] ??= dir;
            storage.Remove("dir");
            applied.Add("v0.storage.dir -> storage.dataDir");
        }

        // v0 的 mqtt.broker（tcp://host:port）→ host + port。
        if (root["northbound"] is JsonObject nb &&
            nb["mqtt"] is JsonObject mqtt &&
            mqtt["broker"] is JsonValue brokerValue &&
            brokerValue.TryGetValue<string>(out var broker))
        {
            var (host, port) = SplitBroker(broker);
            mqtt["host"] ??= host;
            if (port is not null) mqtt["port"] ??= port;
            mqtt.Remove("broker");
            applied.Add("v0.northbound.mqtt.broker -> host/port");
        }

        if (version != CurrentVersion)
        {
            root["schemaVersion"] = CurrentVersion;
            applied.Add($"schemaVersion {version} -> {CurrentVersion}");
        }

        var upgraded = root.ToJsonString(new JsonSerializerOptions { WriteIndented = true });
        return new MigrationResult(root, upgraded, applied);
    }

    /// <summary>确保 <c>northbound.rest</c> 存在；返回同一个对象，调用方直接写字段。</summary>
    private static JsonObject EnsureRest(JsonObject root)
    {
        var northbound = root["northbound"] as JsonObject ?? new JsonObject();
        root["northbound"] = northbound;

        var rest = northbound["rest"] as JsonObject ?? new JsonObject();
        northbound["rest"] = rest;

        return rest;
    }

    /// <summary>
    /// v0 的通道形状：<c>transport</c> 是一个对象（<c>{kind,address}</c> / <c>{kind,port,baud,…}</c>），
    /// 并发字段叫 <c>concurrency</c>。扁平化之后才能被强类型模型直接读。
    /// 这些写法在早期示例配置里大量存在，不迁移等于「升级即起不来」。
    /// </summary>
    private static void MigrateChannels(JsonObject root, List<string> applied)
    {
        if (root["channels"] is not JsonArray channels) return;

        foreach (var item in channels)
        {
            if (item is not JsonObject channel) continue;

            if (channel["transport"] is JsonObject transport)
            {
                var kind = ReadLegacyString(transport, "kind");

                channel["transport"] = kind switch
                {
                    "memory" => "none",
                    "serial" => "serial",
                    _ => kind ?? "tcp",
                };

                if (transport["address"] is JsonValue addressValue &&
                    addressValue.TryGetValue<string>(out var address))
                {
                    var (host, port) = SplitBroker(address);
                    channel["host"] ??= host;
                    if (port is not null) channel["port"] ??= port;
                }

                var transportKind = channel["transport"]!.GetValue<string>();
                if (transportKind == "serial")
                {
                    var serial = channel["serial"] as JsonObject ?? new JsonObject();
                    channel["serial"] = serial;

                    foreach (var key in new[] { "port", "baud", "dataBits", "parity", "stopBits" })
                    {
                        if (transport[key] is JsonValue value) serial[key] ??= value.DeepClone();
                    }

                    // 旧示例把停止位写成 1 / 1.5 / 2，现在的契约是 one / onePointFive / two。
                    if (serial["stopBits"] is JsonValue stopBits)
                    {
                        var normalized = stopBits.ToString() switch
                        {
                            "1" or "1.0" => "one",
                            "1.5" => "onePointFive",
                            "2" or "2.0" => "two",
                            _ => null,
                        };

                        if (normalized is not null) serial["stopBits"] = normalized;
                    }
                }

                if (channel["host"] is null && transport["host"] is JsonValue hostValue)
                    channel["host"] = hostValue.DeepClone();

                if (channel["port"] is null && transport["port"] is JsonValue portValue &&
                    transportKind != "serial")
                {
                    channel["port"] = portValue.DeepClone();
                }

                applied.Add($"v0.channels[].transport -> flat fields ({channel["id"]})");
            }

            if (channel["concurrency"] is JsonValue concurrency && channel["maxConcurrency"] is null)
            {
                channel["maxConcurrency"] = concurrency.DeepClone();
                channel.Remove("concurrency");
                applied.Add($"v0.channels[].concurrency -> maxConcurrency ({channel["id"]})");
            }
        }
    }

    /// <summary>v0 的设备用 <c>channel</c> 指向通道；现在的字段名是 <c>channelId</c>。</summary>
    private static void MigrateDevices(JsonObject root, List<string> applied)
    {
        if (root["devices"] is not JsonArray devices) return;

        foreach (var item in devices)
        {
            if (item is not JsonObject device) continue;
            if (device["channel"] is not JsonValue channel) continue;
            if (device["channelId"] is not null) continue;

            device["channelId"] = channel.DeepClone();
            device.Remove("channel");
            applied.Add($"v0.devices[].channel -> channelId ({device["id"]})");
        }
    }

    private static int ReadVersion(JsonObject root)
    {
        if (root["schemaVersion"] is not JsonValue value) return 1;

        if (value.TryGetValue<int>(out var number)) return number;

        if (value.TryGetValue<string>(out var text) && int.TryParse(text, out number)) return number;

        throw new ConfigException(
            "schemaVersion must be an integer",
            new[] { new ValidationProblem("schemaVersion", "must be an integer") });
    }

    /// <summary>
    /// 读取一个 v0 旧字段的字符串值。字段缺失返回 null；存在但类型不是字符串则报
    /// <see cref="ConfigException"/>。
    ///
    /// 为什么必须显式判类型而不是直接 <c>GetValue&lt;string&gt;()</c>：后者在遇到
    /// 数字 / 布尔 / 对象时抛的是 <see cref="InvalidOperationException"/>，而调用方
    /// （ConfigStore.Parse / TryParse）只按 ConfigException 分类「配置非法」。裸异常会
    /// 冒到 REST 最外层兜底 catch，把一份客户端配置错误渲染成 500——和把请求体语法
    /// 错误冒成 500 同一类问题。旧字段写错了类型，本就等同于配置非法。
    /// </summary>
    private static string? ReadLegacyString(JsonObject node, string field)
    {
        var value = node[field];
        if (value is null) return null;

        // 只认字符串标量：数字 / 布尔 / 对象 / 数组都算形状不符。之前用
        // GetValue<string>() 时，标量非字符串会抛裸 InvalidOperationException，
        // 而对象 / 数组走的是另一条路（它们不是 JsonValue），若只用 is JsonValue
        // 判类型就会把「token 写成 {...}」当成缺失而静默放过，等于半迁移。
        if (value is JsonValue scalar && scalar.TryGetValue<string>(out var text))
            return text;

        throw new ConfigException(
            $"'{field}' must be a string",
            new[] { new ValidationProblem(field, "must be a string") });
    }

    private static (string Host, int? Port) SplitBroker(string broker)
    {
        var text = broker.Trim();

        if (text.Contains("://", StringComparison.Ordinal))
            text = text[(text.IndexOf("://", StringComparison.Ordinal) + 3)..];

        var slash = text.IndexOf('/');
        if (slash >= 0) text = text[..slash];

        var colon = text.LastIndexOf(':');
        if (colon > 0 && int.TryParse(text[(colon + 1)..], out var port))
            return (text[..colon], port);

        return (text, null);
    }
}
