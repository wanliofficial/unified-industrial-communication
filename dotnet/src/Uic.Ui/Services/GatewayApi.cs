using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.Json.Serialization.Metadata;
using Uic.Domain;

namespace Uic.Ui;

/// <summary>
/// 北向 REST 的最小客户端。UI 只通过它取数：不复制采集逻辑、不自留设备状态副本，
/// 浏览器里看到的与 SCADA/REST 调用方看到的必然是同一份数据。
///
/// 序列化全部走 <see cref="UiJson"/>（源生成），因为 WASM 发布启用了裁剪，
/// 运行时反射序列化不可用。时长字段复用网关的 <see cref="DurationConverter"/>，
/// 保证 "500ms" 这类可读写法在两端一致。
/// </summary>
public sealed class GatewayApi
{
    private readonly HttpClient _http;

    public GatewayApi(string? baseUrl = null)
    {
        _http = new HttpClient { Timeout = TimeSpan.FromSeconds(20) };
        _http.BaseAddress = new Uri(baseUrl ?? ResolveBaseUrl(), UriKind.Absolute);

        var token = TokenStore.Load();
        if (!string.IsNullOrEmpty(token))
        {
            _http.DefaultRequestHeaders.Authorization = new("Bearer", token);
        }
    }

    public string BaseUrl => _http.BaseAddress!.ToString().TrimEnd('/');

    /// <summary>
    /// 与 API 同源时用相对路径；不同源（本地调试）回落到 8080。
    /// 写死绝对地址在部署到 80/443 之后会把请求打回本机。
    /// </summary>
    private static string ResolveBaseUrl()
    {
        var origin = BrowserOrigin();
        return origin is null ? "http://127.0.0.1:8080/" : new Uri(new Uri(origin), "/").ToString();
    }

    private static string? BrowserOrigin()
    {
        try
        {
            if (!OperatingSystem.IsBrowser()) return null;

            var origin = Uno.Foundation.WebAssemblyRuntime.InvokeJS("window.location.origin");
            return string.IsNullOrWhiteSpace(origin) || origin == "null" ? null : origin;
        }
        catch
        {
            return null;
        }
    }

    /// <summary>取源生成上下文里登记的元数据。漏登记会在这里直接抛，而不是静默反序列化失败。</summary>
    private static JsonTypeInfo<T> Info<T>()
        => UiJson.Default.GetTypeInfo(typeof(T)) as JsonTypeInfo<T>
           ?? throw new InvalidOperationException(
               $"JSON 类型 {typeof(T).Name} 未登记到 {nameof(UiJson)}，请补 [JsonSerializable]");

    private async Task<T> GetAsync<T>(string path, CancellationToken ct = default)
    {
        using var response = await _http.GetAsync(path, ct).ConfigureAwait(false);
        return await ReadAsync<T>(response, path, ct).ConfigureAwait(false);
    }

    private async Task<T> PostAsync<T>(string path, HttpContent content, CancellationToken ct = default)
    {
        using var request = new HttpRequestMessage(HttpMethod.Post, path) { Content = content };
        using var response = await _http.SendAsync(request, ct).ConfigureAwait(false);
        return await ReadAsync<T>(response, path, ct).ConfigureAwait(false);
    }

    private static async Task<T> ReadAsync<T>(HttpResponseMessage response, string path, CancellationToken ct)
    {
        var text = await response.Content.ReadAsStringAsync(ct).ConfigureAwait(false);

        if (!response.IsSuccessStatusCode)
        {
            throw new GatewayException($"{(int)response.StatusCode} {path}: {Trim(text)}",
                response.StatusCode is System.Net.HttpStatusCode.Unauthorized
                    or System.Net.HttpStatusCode.Forbidden);
        }

        if (string.IsNullOrWhiteSpace(text)) return default!;

        try
        {
            return JsonSerializer.Deserialize(text, Info<T>())
                   ?? throw new GatewayException($"{path} returned an empty body");
        }
        catch (JsonException ex)
        {
            throw new GatewayException($"{path} returned malformed JSON: {ex.Message}");
        }
    }

    private static string Trim(string text)
    {
        text = text.Replace('\n', ' ').Replace('\r', ' ').Trim();
        return text.Length <= 300 ? text : text[..300] + "…";
    }

    // ------------------------------------------------------------------
    // 端点（与 05-northbound-api-spec.md 一一对应）
    // ------------------------------------------------------------------

    public Task<HealthDto> HealthAsync(CancellationToken ct = default)
        => GetAsync<HealthDto>("/api/v1/health", ct);

    public Task<VersionDto> VersionAsync(CancellationToken ct = default)
        => GetAsync<VersionDto>("/api/v1/version", ct);

    public Task<Paged<DeviceSummaryDto>> DevicesAsync(int limit = 200, CancellationToken ct = default)
        => GetAsync<Paged<DeviceSummaryDto>>($"/api/v1/devices?limit={limit}", ct);

    /// <summary>驱动元数据。控制台用它渲染四态能力徽标。</summary>
    public Task<DriversDto> DriversAsync(CancellationToken ct = default)
        => GetAsync<DriversDto>("/api/v1/drivers", ct);

    public Task<DeviceSummaryDto> DeviceAsync(string id, CancellationToken ct = default)
        => GetAsync<DeviceSummaryDto>($"/api/v1/devices/{Uri.EscapeDataString(id)}", ct);

    public Task<Paged<PointDto>> DevicePointsAsync(string device, int limit = 500, CancellationToken ct = default)
        => GetAsync<Paged<PointDto>>(
            $"/api/v1/devices/{Uri.EscapeDataString(device)}/points?limit={limit}", ct);

    public Task<Paged<PointDto>> PointsAsync(string? device = null, int limit = 500, CancellationToken ct = default)
    {
        var query = new StringBuilder($"?limit={limit}");
        if (!string.IsNullOrWhiteSpace(device)) query.Append("&device=").Append(Uri.EscapeDataString(device));

        return GetAsync<Paged<PointDto>>($"/api/v1/points{query}", ct);
    }

    /// <summary>单点写入。请求体是 { "value": {...}, "confirm": true }。</summary>
    public Task<WriteResponseDto> WriteAsync(string device, string point, DataValueDto value,
        CancellationToken ct = default)
        => PostAsync<WriteResponseDto>(
            $"/api/v1/points/{Uri.EscapeDataString(device)}/{Uri.EscapeDataString(point)}/write",
            JsonContent.Create(new WriteRequest(value, Confirm: true), Info<WriteRequest>()),
            ct);

    public Task<Paged<LogEntryDto>> LogsAsync(int limit = 200, string? level = null, string? device = null,
        CancellationToken ct = default)
    {
        var query = new StringBuilder($"?limit={limit}");
        if (!string.IsNullOrWhiteSpace(level)) query.Append("&level=").Append(Uri.EscapeDataString(level));
        if (!string.IsNullOrWhiteSpace(device)) query.Append("&device=").Append(Uri.EscapeDataString(device));

        return GetAsync<Paged<LogEntryDto>>($"/api/v1/logs{query}", ct);
    }

    public Task<ConfigResponseDto> ConfigAsync(CancellationToken ct = default)
        => GetAsync<ConfigResponseDto>("/api/v1/config", ct);

    public Task<Paged<ConfigVersionDto>> VersionsAsync(CancellationToken ct = default)
        => GetAsync<Paged<ConfigVersionDto>>("/api/v1/config/versions", ct);

    public Task<ValidateResponseDto> ValidateAsync(string json, CancellationToken ct = default)
        => PostAsync<ValidateResponseDto>("/api/v1/config/validate",
            JsonContent.Create(new ValidateRequest(json), Info<ValidateRequest>()), ct);

    public Task<ApplyResponseDto> ApplyAsync(string json, string? comment, CancellationToken ct = default)
        => PostAsync<ApplyResponseDto>("/api/v1/config/apply",
            JsonContent.Create(new ApplyRequest(json, comment, Confirm: true), Info<ApplyRequest>()), ct);

    public Task<CommandResponseDto> CommandAsync(string action, Dictionary<string, string>? parameters = null,
        CancellationToken ct = default)
        => PostAsync<CommandResponseDto>("/api/v1/commands",
            JsonContent.Create(new CommandRequest(action, parameters, Confirm: true), Info<CommandRequest>()), ct);

    /// <summary>
    /// SSE 订阅。枚举在连接断开时结束，由调用方决定重连节奏
    /// （UI 里是 3 秒退避，并在界面上显示「已断开/重连中」）。
    /// </summary>
    public async IAsyncEnumerable<SseEvent> StreamAsync(
        IEnumerable<string>? points = null,
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken ct = default)
    {
        var query = points is null
            ? string.Empty
            : "?points=" + Uri.EscapeDataString(string.Join(',', points));

        using var request = new HttpRequestMessage(HttpMethod.Get, $"/api/v1/stream{query}");
        request.Headers.Accept.ParseAdd("text/event-stream");

        using var response = await _http
            .SendAsync(request, HttpCompletionOption.ResponseHeadersRead, ct)
            .ConfigureAwait(false);

        if (!response.IsSuccessStatusCode)
        {
            throw new GatewayException($"{(int)response.StatusCode} /api/v1/stream");
        }

        await using var stream = await response.Content.ReadAsStreamAsync(ct).ConfigureAwait(false);
        using var reader = new StreamReader(stream);

        string? type = null;
        var data = new StringBuilder();

        while (!ct.IsCancellationRequested)
        {
            var line = await reader.ReadLineAsync(ct).ConfigureAwait(false);
            if (line is null) yield break;

            if (line.Length == 0)
            {
                if (type is not null) yield return new SseEvent(type, data.ToString());

                type = null;
                data.Clear();
                continue;
            }

            if (line.StartsWith("event:", StringComparison.Ordinal))
            {
                type = line["event:".Length..].Trim();
            }
            else if (line.StartsWith("data:", StringComparison.Ordinal))
            {
                if (data.Length > 0) data.Append('\n');
                data.Append(line["data:".Length..].Trim());
            }
        }
    }
}

public sealed record SseEvent(string Type, string Data);

public sealed class GatewayException : Exception
{
    public GatewayException(string message, bool unauthorized = false) : base(message)
        => Unauthorized = unauthorized;

    public bool Unauthorized { get; }
}

/// <summary>会话 Token 只放 localStorage：刷新保持登录，关标签页即失效。</summary>
public static class TokenStore
{
    private const string Key = "uic.token";

    public static string? Load()
    {
        try
        {
            if (!OperatingSystem.IsBrowser()) return null;

            var value = Uno.Foundation.WebAssemblyRuntime.InvokeJS($"window.localStorage.getItem('{Key}')");
            return string.IsNullOrWhiteSpace(value) || value == "null" ? null : value;
        }
        catch
        {
            return null;
        }
    }

    public static void Save(string? token)
    {
        try
        {
            if (!OperatingSystem.IsBrowser()) return;

            Uno.Foundation.WebAssemblyRuntime.InvokeJS(string.IsNullOrEmpty(token)
                ? $"window.localStorage.removeItem('{Key}')"
                : $"window.localStorage.setItem('{Key}', '{EscapeJs(token)}')");
        }
        catch
        {
            // 隐私模式下 localStorage 不可用：退化为「本次会话有效」，不弹窗打扰。
        }
    }

    /// <summary>令牌里的反斜杠与引号必须先转义，否则拼出来的 JS 字符串会把语句截断。</summary>
    private static string EscapeJs(string value)
        => value.Replace("\\", "\\\\").Replace("'", "\\'")
            .Replace("\r", "\\r").Replace("\n", "\\n");
}

// ----------------------------------------------------------------------
// 请求体（源生成要求显式类型，不能用匿名对象）
// ----------------------------------------------------------------------

public sealed record WriteRequest(DataValueDto Value, bool Confirm);

public sealed record ValidateRequest(string Json);

public sealed record ApplyRequest(string Json, string? Comment, bool Confirm);

public sealed record CommandRequest(string Action,
    [property: JsonPropertyName("params")] Dictionary<string, string>? Params, bool Confirm);

// ----------------------------------------------------------------------
// 契约投影（字段名与 ApiModels.cs 保持一致）
// ----------------------------------------------------------------------

public sealed record Paged<T>(T[] Items, string? NextCursor, int Total);

public sealed record HealthDto(string Status, long UptimeSeconds, DevicesHealthDto Devices,
    PointsHealthDto Points, OutboxHealthDto Outbox, ProblemDto[] Problems);

public sealed record DevicesHealthDto(int Total, int Collecting, int Degraded, int Faulted);

public sealed record PointsHealthDto(int Total, int Good, int Stale, int Bad);

public sealed record OutboxHealthDto(long Pending, long DroppedLastHour);

public sealed record ProblemDto(string Device, string State, string? Reason, DateTimeOffset Since);

public sealed record VersionDto(string Name, string Version, string ApiVersion, string SchemaVersion,
    string Site, string Runtime, DateTimeOffset StartedAt, string ConfigVersion);

public sealed record DriversDto(DriverDto[] Items);

public sealed record DriverDto(string Id, string Name, string Version, string SdkVersion,
    string[] Roles, string[] Transports, string[] AddressKinds, CapabilitiesDto Capabilities,
    string? Description);

public sealed record CapabilitiesDto(string Read, string Write, string Subscribe, string Browse,
    string Discovery, string MultiRead, string BitAccess, string StructTypes);

public sealed record DeviceSummaryDto(string Id, string? Name, string Channel, string Protocol,
    string ConfiguredState, string RuntimeState, DateTimeOffset Since, int Generation,
    [property: JsonConverter(typeof(DurationConverter))] TimeSpan Interval,
    DeviceStatsDto Stats, PointsSummaryDto Points);

public sealed record DeviceStatsDto(long ReadsTotal, long ReadsFailed, double SuccessRate,
    double P95DurationMs, string? LastError, DateTimeOffset? LastSuccessAt);

public sealed record PointsSummaryDto(int Total, int Good, int Bad);

public sealed record PointDto(string Device, string Point, string? Name, DataValueDto Value,
    string Quality, string? Unit, DateTimeOffset? DeviceTime, DateTimeOffset CollectedAt, ulong Seq,
    bool Writable);

public sealed record DataValueDto(string Type, double? Num, bool? Bool, string? Text)
{
    [JsonIgnore]
    public string Display => Type switch
    {
        "bool" => Bool == true ? "true" : Bool == false ? "false" : "—",
        "string" or "bytes" => Text ?? "—",
        _ => Num is { } n
            ? Math.Abs(n - Math.Round(n)) < 1e-9
                ? n.ToString("0", System.Globalization.CultureInfo.InvariantCulture)
                : n.ToString("0.###", System.Globalization.CultureInfo.InvariantCulture)
            : "—",
    };
}

public sealed record WriteResponseDto(WriteResultDto[] Results);

public sealed record WriteResultDto(string Point, bool Ok, string Quality, DateTimeOffset AppliedAt,
    string? Error, bool? Verified = null);

public sealed record LogEntryDto(long Seq, DateTimeOffset At, string Level, string Category,
    string Message, string? Device, string? Exception);

public sealed record ConfigResponseDto(string Version, JsonElement Config);

public sealed record ConfigVersionDto(string Id, DateTimeOffset CreatedAt, string? Comment, int Size,
    bool Current);

public sealed record ValidateResponseDto(bool Valid, string[] Problems);

public sealed record ApplyResponseDto(string Version, bool Applied, ChangedCountsDto Changed,
    string[] RestartedDevices, long DurationMs);

public sealed record ChangedCountsDto(int DevicesAdded, int DevicesModified, int DevicesRemoved,
    int PointsAdded, int PointsModified, int PointsRemoved);

public sealed record CommandResponseDto(string Action, bool Ok, string? Detail);
