using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Config;
using Uic.Domain;

namespace Uic.Northbound;

public static partial class RestApi
{
    /// <summary>
    /// 点表导入与配置迁移（FR-CFG-05 / FR-CFG-13）。
    ///
    /// 导入默认 dry-run：现场最常见的操作是「先看看会新增/覆盖什么」，
    /// 直接落盘会让一次手滑覆盖掉整张点表。
    /// </summary>
    private static void MapImportExport(WebApplication app, NorthboundContext ctx)
    {
        app.MapPost("/api/v1/devices/{device}/points/import",
            async (HttpContext http, string device, CancellationToken ct) =>
        {
            var current = ctx.Control.CurrentConfig
                          ?? throw ApiException.NotReady("configuration is not loaded");

            var target = current.Devices.FirstOrDefault(d => d.Id == device)
                         ?? throw ApiException.NotFound("device.not_found",
                             $"device '{device}' not found", new { device });

            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);

            string csv;
            var queryDryRun = !string.Equals(http.Request.Query["dryRun"], "false", StringComparison.OrdinalIgnoreCase);
            var dryRun = queryDryRun;
            string? comment = null;

            if (body.TrimStart().StartsWith('{'))
            {
                var request = ReadJson<PointsImportRequestDto>(body)
                              ?? throw ApiException.BadRequest("request.invalid", "request body is required");

                csv = request.Csv ?? string.Empty;

                // 查询串优先于请求体：运维脚本用 ?dryRun=false 表达「我确认要写」，
                // 而请求体里的默认值不应该悄悄把它改回预览。
                var querySpecified = http.Request.Query.ContainsKey("dryRun");
                dryRun = querySpecified ? queryDryRun : request.DryRun ?? true;
                comment = request.Comment;
            }
            else
            {
                // 也接受直接把 CSV 当请求体：curl --data-binary @points.csv 更顺手。
                csv = body;
            }

            var result = PointCsv.Import(csv, target.Points, target.Id);

            if (!result.Ok)
            {
                return Results.Json(new PointsImportResponseDto(
                    target.Id, false, false, null,
                    result.Added, result.Updated, result.Points, result.Problems), ApiJson.Options);
            }

            if (dryRun)
            {
                return Results.Json(new PointsImportResponseDto(
                    target.Id, true, false, null,
                    result.Added, result.Updated, result.Points, Array.Empty<ValidationProblem>()),
                    ApiJson.Options);
            }

            var merged = PointCsv.Merge(target.Points, result.Points);
            var updatedDevice = target.WithPoints(merged);
            var candidate = current.WithDevices(
                current.Devices.Select(d => d.Id == device ? updatedDevice : d).ToArray());

            var outcome = await AuditedAsync(
                ctx, http, "config.points_import",
                _ => device,
                o => new
                {
                    o.Version,
                    added = result.Added,
                    updated = result.Updated,
                    total = merged.Count,
                },
                () => ctx.Control.ApplyConfigAsync(candidate,
                    comment ?? $"imported {result.Points.Count} point(s) into '{device}'", ct))
                .ConfigureAwait(false);

            return Results.Json(new PointsImportResponseDto(
                target.Id, true, true, outcome.Version,
                result.Added, result.Updated, merged, Array.Empty<ValidationProblem>()), ApiJson.Options);
        });

        app.MapGet("/api/v1/config/export", (HttpContext http) =>
        {
            var config = ctx.Control.CurrentConfig
                         ?? throw ApiException.NotReady("configuration is not loaded");

            var redact = !string.Equals(http.Request.Query["redact"], "false", StringComparison.OrdinalIgnoreCase);

            var node = redact
                ? Redact(config)
                : JsonNode.Parse(JsonSerializer.Serialize(config, JsonDefaults.Options));

            // 导出未脱敏配置会把凭据引用（以及站点结构）带出机器，必须留痕。
            if (!redact)
            {
                WriteAudit(ctx, http, "config.export", "ok", target: "config",
                    detail: new { redacted = false, version = ctx.Control.ConfigVersion });
            }

            http.Response.Headers.ContentDisposition =
                $"attachment; filename=\"uic-config-{DateTimeOffset.UtcNow:yyyyMMdd-HHmmss}.json\"";

            return Results.Json(new ConfigExportDto(
                ctx.GatewayVersion,
                DateTimeOffset.UtcNow,
                ctx.Control.ConfigVersion,
                redact,
                node), ApiJson.Options);
        });

        app.MapPost("/api/v1/config/import", async (HttpContext http, CancellationToken ct) =>
        {
            var (text, comment) = await ReadConfigTextAsync(http, ct).ConfigureAwait(false);

            // 默认导出是脱敏的；导入它时同样把 "***" 还原，避免把密钥写成占位符。
            var migration = ConfigMigrations.Upgrade(
                RedactedSecrets.Restore(text, ctx.Control.CurrentConfig));

            var parsed = ConfigStore.TryParse(migration.Json, "import", out var document, out var problems);
            if (!parsed || document is null || problems.Count > 0)
            {
                return Results.Json(new ConfigImportResponseDto(
                    false, false, null, migration.Applied, migration.Root, problems), ApiJson.Options);
            }

            var apply = string.Equals(http.Request.Query["apply"], "true", StringComparison.OrdinalIgnoreCase);

            if (!apply)
            {
                return Results.Json(new ConfigImportResponseDto(
                    true, false, null, migration.Applied, migration.Root, Array.Empty<ValidationProblem>()),
                    ApiJson.Options);
            }

            var outcome = await AuditedAsync(
                ctx, http, "config.import",
                o => o.Version,
                o => new { migrations = migration.Applied, o.RestartedDevices, o.DurationMs },
                () => ctx.Control.ApplyConfigAsync(document,
                    comment ?? "imported configuration", ct)).ConfigureAwait(false);

            return Results.Json(new ConfigImportResponseDto(
                true, true, outcome.Version, migration.Applied, migration.Root,
                Array.Empty<ValidationProblem>()), ApiJson.Options);
        });
    }
}
