using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Uic.Domain;

namespace Uic.Config;

/// <summary>一份配置版本。内容是文件本身，元数据只是索引。</summary>
public sealed record ConfigVersion(
    string Id,
    DateTimeOffset CreatedAt,
    string? Comment,
    int Size,
    string Path);

/// <summary>一处配置差异。路径用 JSON Pointer 风格，便于前端直接定位到表单字段。</summary>
public sealed record ConfigDiffEntry(string Path, string Kind, string? Before, string? After)
{
    /// <summary>added | removed | changed</summary>
    public string ChangeKind => Kind;
}

/// <summary>
/// 配置存储：内容寻址的版本目录 + current 指针 + 原子写入。
///
/// 为什么不做「就地覆盖」：现场最常见的故障是「改了配置、进程被 kill、文件半截」，
/// 下次启动直接起不来。原子写入 + 版本目录让这种情况最多退回上一个版本。
/// </summary>
public sealed class ConfigStore
{
    private readonly string _root;
    private readonly string _versionsDir;
    private readonly string _currentFile;
    private readonly SemaphoreSlim _lock = new(1, 1);

    public ConfigStore(string dataDir)
    {
        DataDir = dataDir;
        _root = Path.Combine(dataDir, "config");
        _versionsDir = Path.Combine(_root, "versions");
        _currentFile = Path.Combine(_root, "current");
        Directory.CreateDirectory(_versionsDir);
    }

    public string Root => _root;
    public string DataDir { get; }
    public ConfigDocument? Current { get; private set; }
    public string CurrentId { get; private set; } = string.Empty;

    /// <summary>从磁盘加载并校验。非法配置直接抛 <see cref="ConfigException"/>，绝不「尽力而为」地跑。</summary>
    public async Task<ConfigDocument> LoadFileAsync(string path, CancellationToken ct = default)
    {
        if (!File.Exists(path))
            throw new FileNotFoundException($"configuration file '{path}' does not exist", path);

        var text = await File.ReadAllTextAsync(path, ct).ConfigureAwait(false);
        return Parse(text, path);
    }

    public static ConfigDocument Parse(string json, string origin = "<inline>")
    {
        var migration = ConfigMigrations.Upgrade(json);

        ConfigDocument? doc;
        try
        {
            doc = JsonSerializer.Deserialize<ConfigDocument>(migration.Json, JsonDefaults.Options);
        }
        catch (JsonException ex)
        {
            throw new ConfigException(
                $"configuration '{origin}' is not valid JSON: {ex.Message}",
                new[] { new ValidationProblem("$", ex.Message) });
        }

        if (doc is null)
            throw new ConfigException(
                $"configuration '{origin}' is empty",
                new[] { new ValidationProblem("$", "document is null") });

        var problems = ModelValidator.Validate(doc);
        if (problems.Count > 0)
            throw new ConfigException($"configuration '{origin}' is invalid ({problems.Count} problem(s))", problems);

        return doc;
    }

    /// <summary>
    /// 解析并校验，但把问题**返回**而不是抛出。
    ///
    /// 「校验」端点需要的是一次列全所有问题（前端要逐条高亮），
    /// 而 apply/启动需要的是「任何一条不满足就不许生效」。
    /// 两种语义都要，但只有后一种应该抛异常。
    /// </summary>
    public static bool TryParse(
        string json,
        string origin,
        out ConfigDocument? document,
        out IReadOnlyList<ValidationProblem> problems)
    {
        document = null;

        string upgraded;
        try
        {
            upgraded = ConfigMigrations.Upgrade(json).Json;
        }
        catch (ConfigException ex)
        {
            problems = ex.Problems;
            return false;
        }

        try
        {
            document = JsonSerializer.Deserialize<ConfigDocument>(upgraded, JsonDefaults.Options);
        }
        catch (JsonException ex)
        {
            problems = new[] { new ValidationProblem("$", ex.Message) };
            return false;
        }

        if (document is null)
        {
            problems = new[] { new ValidationProblem("$", $"configuration '{origin}' is empty") };
            return false;
        }

        problems = ModelValidator.Validate(document);
        return true;
    }

    /// <summary>启动时调用：优先读 current 指针，其次读给定文件，最后用兜底配置。</summary>
    public async Task<ConfigDocument> BootstrapAsync(string file, Func<ConfigDocument> fallback, CancellationToken ct = default)
    {
        var pointer = ReadCurrentPointer();
        if (pointer is not null)
        {
            var doc = LoadVersion(pointer);
            if (doc is not null)
            {
                Current = doc;
                CurrentId = pointer;
                return doc;
            }
        }

        if (File.Exists(file))
        {
            var doc = await LoadFileAsync(file, ct).ConfigureAwait(false);
            Commit(doc, $"loaded from {Path.GetFileName(file)}");
            return doc;
        }

        var seed = fallback();
        ModelValidator.ThrowIfInvalid(seed);
        Commit(seed, "bootstrap default");
        return seed;
    }

    /// <summary>提交一个新版本。先写文件再切指针，任何一步失败都不会让 current 指向半个文件。</summary>
    public string Commit(ConfigDocument doc, string? comment = null)
    {
        ModelValidator.ThrowIfInvalid(doc);

        _lock.Wait();
        try
        {
            var canonical = CanonicalJson.Of(doc);
            var id = ComputeId(canonical);
            var path = Path.Combine(_versionsDir, id + ".json");

            if (!File.Exists(path))
            {
                AtomicWrite(path, Encoding.UTF8.GetBytes(JsonSerializer.Serialize(doc, JsonDefaults.Indented) + "\n"));
                AtomicWrite(Path.Combine(_versionsDir, id + ".meta.json"),
                    Encoding.UTF8.GetBytes(JsonSerializer.Serialize(
                        new { id, createdAt = DateTimeOffset.UtcNow, comment }, JsonDefaults.Options)));
            }

            AtomicWrite(_currentFile, Encoding.UTF8.GetBytes(id));

            Current = doc;
            CurrentId = id;
            return id;
        }
        finally
        {
            _lock.Release();
        }
    }

    public IReadOnlyList<ConfigVersion> Versions()
    {
        if (!Directory.Exists(_versionsDir)) return Array.Empty<ConfigVersion>();

        var list = new List<ConfigVersion>();

        foreach (var file in Directory.EnumerateFiles(_versionsDir, "*.json"))
        {
            if (file.EndsWith(".meta.json", StringComparison.Ordinal)) continue;

            var id = Path.GetFileNameWithoutExtension(file);
            var info = new FileInfo(file);
            DateTimeOffset createdAt = info.LastWriteTimeUtc;
            string? comment = null;

            var metaPath = Path.Combine(_versionsDir, id + ".meta.json");
            if (File.Exists(metaPath))
            {
                try
                {
                    using var meta = JsonDocument.Parse(File.ReadAllText(metaPath));
                    if (meta.RootElement.TryGetProperty("createdAt", out var at) &&
                        at.TryGetDateTimeOffset(out var parsed))
                    {
                        createdAt = parsed;
                    }
                    if (meta.RootElement.TryGetProperty("comment", out var c) &&
                        c.ValueKind == JsonValueKind.String)
                    {
                        comment = c.GetString();
                    }
                }
                catch (JsonException)
                {
                    // 元数据坏了不影响配置本体可用。
                }
            }

            list.Add(new ConfigVersion(id, createdAt, comment, (int)info.Length, file));
        }

        return list.OrderByDescending(v => v.CreatedAt).ToArray();
    }

    public ConfigDocument? LoadVersion(string id)
    {
        var path = Path.Combine(_versionsDir, id + ".json");
        if (!File.Exists(path)) return null;

        try
        {
            return Parse(File.ReadAllText(path), path);
        }
        catch (ConfigException)
        {
            return null;
        }
    }

    /// <summary>回滚到指定版本：把旧内容作为新版本提交，历史只增不改。</summary>
    public string Rollback(string id, string? comment = null)
    {
        var doc = LoadVersion(id)
                  ?? throw new KeyNotFoundException($"config version '{id}' not found or is unreadable");

        return Commit(doc, comment ?? $"rollback to {id}");
    }

    public string? ReadCurrentPointer()
    {
        if (!File.Exists(_currentFile)) return null;

        try
        {
            var id = File.ReadAllText(_currentFile).Trim();
            return id.Length == 0 ? null : id;
        }
        catch (IOException)
        {
            return null;
        }
    }

    /// <summary>两份配置的差异。只比较语义内容，不比较格式。</summary>
    public static IReadOnlyList<ConfigDiffEntry> Diff(ConfigDocument before, ConfigDocument after)
    {
        using var a = JsonDocument.Parse(CanonicalJson.Of(before));
        using var b = JsonDocument.Parse(CanonicalJson.Of(after));

        var entries = new List<ConfigDiffEntry>();
        Walk("", a.RootElement, b.RootElement, entries);
        return entries;
    }

    /// <summary>把差异压成一份可直接贴进工单的文本。</summary>
    public static string DiffText(IReadOnlyList<ConfigDiffEntry> entries, int max = 200)
    {
        var sb = new StringBuilder();
        foreach (var e in entries.Take(max))
        {
            sb.Append(e.Kind switch
            {
                "added" => $"+ {e.Path} = {e.After}",
                "removed" => $"- {e.Path} (was {e.Before})",
                _ => $"~ {e.Path}: {e.Before} -> {e.After}",
            }).Append('\n');
        }

        if (entries.Count > max) sb.Append($"... {entries.Count - max} more\n");
        return sb.ToString();
    }

    private static void Walk(string path, JsonElement a, JsonElement b, List<ConfigDiffEntry> entries)
    {
        if (a.ValueKind != b.ValueKind)
        {
            entries.Add(new ConfigDiffEntry(path, "changed", Render(a), Render(b)));
            return;
        }

        switch (a.ValueKind)
        {
            case JsonValueKind.Object:
            {
                var aProps = a.EnumerateObject().ToDictionary(p => p.Name, p => p.Value, StringComparer.Ordinal);
                var bProps = b.EnumerateObject().ToDictionary(p => p.Name, p => p.Value, StringComparer.Ordinal);

                foreach (var name in aProps.Keys.Union(bProps.Keys, StringComparer.Ordinal).OrderBy(x => x, StringComparer.Ordinal))
                {
                    var child = path.Length == 0 ? name : $"{path}.{name}";

                    var hasA = aProps.TryGetValue(name, out var av);
                    var hasB = bProps.TryGetValue(name, out var bv);

                    if (hasA && hasB) Walk(child, av, bv, entries);
                    else if (hasA) entries.Add(new ConfigDiffEntry(child, "removed", Render(av), null));
                    else entries.Add(new ConfigDiffEntry(child, "added", null, Render(bv)));
                }
                return;
            }

            case JsonValueKind.Array:
            {
                // 数组按索引逐项比较：配置里的数组都是有序的（通道、设备、点位），
                // 用集合语义比较会掩盖「顺序变化」这类真实差异。
                var aItems = a.EnumerateArray().ToArray();
                var bItems = b.EnumerateArray().ToArray();
                var max = Math.Max(aItems.Length, bItems.Length);

                for (var i = 0; i < max; i++)
                {
                    var child = $"{path}[{i}]";
                    if (i < aItems.Length && i < bItems.Length) Walk(child, aItems[i], bItems[i], entries);
                    else if (i < aItems.Length) entries.Add(new ConfigDiffEntry(child, "removed", Render(aItems[i]), null));
                    else entries.Add(new ConfigDiffEntry(child, "added", null, Render(bItems[i])));
                }
                return;
            }

            default:
                if (a.GetRawText() != b.GetRawText())
                    entries.Add(new ConfigDiffEntry(path, "changed", Render(a), Render(b)));
                return;
        }
    }

    private static string Render(JsonElement e) => e.ValueKind switch
    {
        JsonValueKind.String => e.GetString() ?? string.Empty,
        JsonValueKind.Null or JsonValueKind.Undefined => "null",
        JsonValueKind.True => "true",
        JsonValueKind.False => "false",
        _ => e.GetRawText(),
    };

    private static string ComputeId(string canonical)
    {
        var hash = SHA256.HashData(Encoding.UTF8.GetBytes(canonical));
        return "v" + Convert.ToHexString(hash)[..12].ToLowerInvariant();
    }

    /// <summary>写临时文件 + fsync + 原子替换。同目录保证 rename 是原子的。</summary>
    private static void AtomicWrite(string path, byte[] bytes)
    {
        var dir = Path.GetDirectoryName(path);
        if (!string.IsNullOrEmpty(dir)) Directory.CreateDirectory(dir);

        var tmp = path + ".tmp";

        using (var fs = new FileStream(tmp, FileMode.Create, FileAccess.Write, FileShare.None, 64 * 1024, FileOptions.WriteThrough))
        {
            fs.Write(bytes, 0, bytes.Length);
            fs.Flush(flushToDisk: true);
        }

        File.Move(tmp, path, overwrite: true);
    }
}