using System.IO.Compression;
using System.Text;
using System.Text.Json;
using Uic.Domain;

namespace Uic.Northbound;

/// <summary>
/// 诊断包的唯一构建入口。
///
/// GET /api/v1/diagnostics/bundle 与命令 <c>diag.bundle</c> 必须产出同样的东西：
/// 两份实现迟早会出现「网页下载的包里有健康数据、远程命令生成的没有」。
/// 内容一律脱敏后写入，包本身可以交给外部团队。
/// </summary>
public static class DiagnosticsBundle
{
    public static byte[] Build(NorthboundContext ctx)
    {
        var config = ctx.Control.CurrentConfig;
        var health = RestApi.BuildHealth(ctx);

        using var buffer = new MemoryStream();

        using (var zip = new ZipArchive(buffer, ZipArchiveMode.Create, leaveOpen: true))
        {
            WriteEntry(zip, "manifest.json", JsonSerializer.Serialize(new
            {
                generatedAt = DateTimeOffset.UtcNow,
                gateway = ctx.GatewayVersion,
                apiVersion = RestApi.ApiVersion,
                schemaVersion = RestApi.SchemaVersion,
                site = ctx.Site,
                configVersion = ctx.Control.ConfigVersion,
            }, JsonDefaults.Indented));

            WriteEntry(zip, "config.redacted.json",
                config is null ? "{}" : JsonSerializer.Serialize(RestApi.Redact(config), JsonDefaults.Indented));

            WriteEntry(zip, "metrics.txt", ctx.Metrics.RenderPrometheus());
            WriteEntry(zip, "health.json", JsonSerializer.Serialize(health, JsonDefaults.Indented));
            WriteEntry(zip, "devices.json",
                JsonSerializer.Serialize(ctx.Engine.DeviceViews().Select(RestApi.ToSummary), JsonDefaults.Indented));

            var logs = ctx.Logs.Snapshot(limit: 5000)
                .OrderBy(e => e.Seq)
                .Select(e => $"{e.At:O} [{e.Level}] {e.Category}: {e.Message}")
                .ToArray();
            WriteEntry(zip, "logs/uic.log", string.Join('\n', logs));
        }

        return buffer.ToArray();
    }

    public static string FileName(DateTimeOffset at) => $"uic-diagnostics-{at:yyyyMMdd-HHmmss}.zip";

    private static void WriteEntry(ZipArchive zip, string name, string content)
    {
        var entry = zip.CreateEntry(name, CompressionLevel.Optimal);
        using var stream = entry.Open();
        var bytes = Encoding.UTF8.GetBytes(content);
        stream.Write(bytes, 0, bytes.Length);
    }
}
