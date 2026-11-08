using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Tests;

/// <summary>
/// 一条金报文。字段名与《驱动 SDK 规范》§7.1 保持一致：
/// <c>name / request / response / expect</c> 是必需项，其余为扩展项。
///
/// 扩展项只解决「规范示例没覆盖到」的两件事：
/// - <c>frame</c>：TCP / RTU / ASCII 三种封装各有一份黄金帧；
/// - <c>offset</c>：点位之间留空隙（例如 100、102）时，顺序推导会算错窗口。
/// </summary>
internal sealed class GoldenCase
{
    public string Name { get; init; } = string.Empty;

    /// <summary>read | write。默认 read。</summary>
    public string Kind { get; init; } = "read";

    /// <summary>tcp | rtu | ascii。默认 tcp。</summary>
    public string Frame { get; init; } = "tcp";

    /// <summary>寄存器区。缺省时按功能码推导。</summary>
    public string? Area { get; init; }

    /// <summary>字节序。默认 ABCD。</summary>
    public string ByteOrder { get; init; } = "abcd";

    public string Request { get; init; } = string.Empty;
    public string Response { get; init; } = string.Empty;

    /// <summary>期望抛出的错误分类（timeout/connection/protocol/address/permission/unsupported）。</summary>
    public string? Error { get; init; }

    public GoldenExpect[] Expect { get; init; } = Array.Empty<GoldenExpect>();
}

internal sealed class GoldenExpect
{
    public string Point { get; init; } = "p1";

    /// <summary>数据类型，与线上类型名一致：uint16 / float32 / bool / string ...</summary>
    public string Type { get; init; } = "uint16";

    public double? Num { get; init; }
    public bool? Bool { get; init; }
    public string? Text { get; init; }

    /// <summary>
    /// 数组期望值。写了这个字段就表示该点位是数组：<see cref="Count"/> 由长度推导，
    /// 不必在夹具里再填一遍（填两处迟早不一致）。
    /// </summary>
    public double[]? Array { get; init; }

    /// <summary>占用寄存器数；仅 string / bytes 这类无固定宽度的类型需要。</summary>
    public int? Length { get; init; }

    /// <summary>绝对地址偏移。缺省时按前一个点位的宽度顺延。</summary>
    public int? Offset { get; init; }

    /// <summary>期望质量码。缺省 good。</summary>
    public string Quality { get; init; } = "good";
}

internal static class GoldenCorpus
{
    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web)
    {
        PropertyNameCaseInsensitive = true,
        ReadCommentHandling = JsonCommentHandling.Skip,
        AllowTrailingCommas = true,
        Converters = { new JsonStringEnumConverter(JsonNamingPolicy.CamelCase) },
    };

    /// <summary>
    /// 定位 <c>testdata/golden</c>：从输出目录向上找，直到看见它。
    /// 这样 <c>dotnet run</c> 与 <c>dotnet test</c> 都不需要拷贝数据文件。
    /// </summary>
    public static string Directory()
    {
        var overridePath = Environment.GetEnvironmentVariable("UIC_TESTDATA");
        if (!string.IsNullOrWhiteSpace(overridePath))
        {
            var direct = Path.Combine(overridePath, "golden");
            return System.IO.Directory.Exists(direct) ? direct : overridePath;
        }

        var dir = new DirectoryInfo(AppContext.BaseDirectory);

        while (dir is not null)
        {
            var candidate = Path.Combine(dir.FullName, "testdata", "golden");
            if (System.IO.Directory.Exists(candidate)) return candidate;
            dir = dir.Parent;
        }

        throw new DirectoryNotFoundException(
            "testdata/golden not found above " + AppContext.BaseDirectory +
            "; set UIC_TESTDATA to override");
    }

    public static IReadOnlyList<(string File, GoldenCase Case)> Load()
    {
        var dir = Directory();
        var list = new List<(string, GoldenCase)>();

        foreach (var file in System.IO.Directory.EnumerateFiles(dir, "*.json").OrderBy(f => f, StringComparer.Ordinal))
        {
            var text = File.ReadAllText(file);
            var parsed = JsonSerializer.Deserialize<GoldenCase>(text, Json)
                         ?? throw new InvalidDataException($"{Path.GetFileName(file)}: fixture is empty");

            if (string.IsNullOrWhiteSpace(parsed.Name))
                throw new InvalidDataException($"{Path.GetFileName(file)}: missing 'name'");

            list.Add((Path.GetFileName(file), parsed));
        }

        return list;
    }
}
