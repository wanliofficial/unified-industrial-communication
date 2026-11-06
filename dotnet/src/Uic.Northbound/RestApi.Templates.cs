using System.Text.Json;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Uic.Config;
using Uic.Domain;

namespace Uic.Northbound;

public static partial class RestApi
{
    /// <summary>
    /// 设备模板与批量实例化（FR-CFG-04）。
    ///
    /// 模板保存在 <c>{dataDir}/templates</c>，与配置版本目录分开：
    /// 模板是「怎么生成设备」的规则，配置是「现在有哪些设备」的事实，
    /// 混在一起会让回滚把模板也带回去。
    /// </summary>
    private static void MapTemplates(WebApplication app, NorthboundContext ctx)
    {
        var store = new TemplateStore(ctx.Store.DataDir);

        app.MapGet("/api/v1/templates", () =>
        {
            var templates = store.List()
                .Select(t => new TemplateSummaryDto(
                    t.Id,
                    t.Name,
                    t.Description,
                    t.Protocol,
                    t.Parameters,
                    t.Device["id"]?.GetValue<string>()))
                .ToArray();

            return Results.Json(new PagedResponse<TemplateSummaryDto>(templates, null, templates.Length),
                ApiJson.Options);
        });

        app.MapGet("/api/v1/templates/{id}", (string id) =>
        {
            var template = store.Get(id)
                           ?? throw ApiException.NotFound("template.not_found",
                               $"template '{id}' not found", new { template = id });

            return Results.Json(template, ApiJson.Options);
        });

        app.MapPost("/api/v1/templates", async (HttpContext http, CancellationToken ct) =>
        {
            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var template = ReadJson<DeviceTemplate>(body)
                           ?? throw ApiException.BadRequest("request.invalid", "template body is required");

            var saved = await store.SaveAsync(template, ct).ConfigureAwait(false);
            return Results.Json(saved, ApiJson.Options, statusCode: StatusCodes.Status201Created);
        });

        app.MapDelete("/api/v1/templates/{id}", async (string id, CancellationToken ct) =>
        {
            var removed = await store.DeleteAsync(id, ct).ConfigureAwait(false);
            if (!removed)
                throw ApiException.NotFound("template.not_found", $"template '{id}' not found");

            return Results.NoContent();
        });

        app.MapPost("/api/v1/templates/{id}/instantiate", async (HttpContext http, string id, CancellationToken ct) =>
        {
            var template = store.Get(id)
                           ?? throw ApiException.NotFound("template.not_found",
                               $"template '{id}' not found", new { template = id });

            var body = await ReadBodyAsync(http, ct).ConfigureAwait(false);
            var request = string.IsNullOrWhiteSpace(body)
                ? new InstantiateRequestDto(null, false, null)
                : ReadJson<InstantiateRequestDto>(body)
                  ?? new InstantiateRequestDto(null, false, null);

            var current = ctx.Control.CurrentConfig
                          ?? throw ApiException.NotReady("configuration is not loaded");

            var channels = current.Channels.Select(c => c.Id).ToArray();
            var input = request.Instances is { Count: > 0 }
                ? request.Instances
                : new[] { new Dictionary<string, string?>(StringComparer.OrdinalIgnoreCase) };

            var devices = new List<Device>();
            var problems = new List<ValidationProblem>();

            for (var i = 0; i < input.Count; i++)
            {
                var parameters = new Dictionary<string, string?>(StringComparer.OrdinalIgnoreCase);
                foreach (var (key, value) in input[i]) parameters[key] = value;

                // index 自动注入：批量生成 50 台设备时，调用方不必手写 1..50。
                if (input.Count > 1 && !parameters.ContainsKey("index"))
                    parameters["index"] = (i + 1).ToString(System.Globalization.CultureInfo.InvariantCulture);

                var result = TemplateInstantiator.Instantiate(template, parameters, channels);
                problems.AddRange(result.Problems);

                if (result.Device is not null)
                    devices.Add(result.Device.WithTemplateId(template.Id));
            }

            var duplicate = devices.GroupBy(d => d.Id, StringComparer.Ordinal)
                .Where(g => g.Count() > 1)
                .Select(g => g.Key)
                .ToArray();

            foreach (var idValue in duplicate)
                problems.Add(new ValidationProblem("device.id", $"duplicate instantiated device id '{idValue}'"));

            if (problems.Count > 0)
            {
                throw ApiException.Unprocessable("template.invalid_instance",
                    $"{problems.Count} problem(s) while instantiating '{template.Id}'",
                    problems.Select(p => new { path = p.Path, message = p.Message }));
            }

            if (!request.Apply)
            {
                return Results.Json(new InstantiateResponseDto(
                    template.Id, false, null, devices, Array.Empty<ValidationProblem>()), ApiJson.Options);
            }

            var existingIds = current.Devices.Select(d => d.Id).ToHashSet(StringComparer.Ordinal);
            foreach (var device in devices.Where(d => existingIds.Contains(d.Id)))
                problems.Add(new ValidationProblem("device.id", $"device '{device.Id}' already exists"));

            if (problems.Count > 0)
            {
                throw ApiException.Unprocessable("template.invalid_instance",
                    $"{problems.Count} problem(s) while applying '{template.Id}'",
                    problems.Select(p => new { path = p.Path, message = p.Message }));
            }

            var candidate = current.WithDevices(current.Devices.Concat(devices).ToArray());
            var outcome = await ctx.Control.ApplyConfigAsync(candidate,
                request.Comment ?? $"instantiated {devices.Count} device(s) from template '{template.Id}'", ct)
                .ConfigureAwait(false);

            return Results.Json(new InstantiateResponseDto(
                template.Id, true, outcome.Version, devices, Array.Empty<ValidationProblem>()), ApiJson.Options);
        });
    }
}
