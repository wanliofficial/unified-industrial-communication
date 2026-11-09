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
    /// <summary>
    /// 会话失效（401）时触发。挂在类型上而不是实例上：页面各自持有同一个
    /// <see cref="GatewayApi"/>，但「被踢回登录页」是 Shell 的职责，只该有一个订阅者。
    /// </summary>
    public static event Action? SessionExpired;

    private readonly HttpClient _http;
    private string? _token;

    public GatewayApi(string? baseUrl = null)
    {
        _http = new HttpClient { Timeout = TimeSpan.FromSeconds(20) };
        _http.BaseAddress = new Uri(baseUrl ?? ResolveBaseUrl(), UriKind.Absolute);

        SetToken(TokenStore.Load());
    }

    /// <summary>
    /// 换用另一个会话 Token。登录、改密、退出都走这里，
    /// 避免「保存了 localStorage 但当前 HttpClient 还在用旧令牌」这类分裂状态。
    /// </summary>
    public void SetToken(string? token)
    {
        _token = string.IsNullOrEmpty(token) ? null : token;

        _http.DefaultRequestHeaders.Authorization = _token is null
            ? null
            : new System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", _token);
    }

    public string? Token => _token;

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

    private async Task<T> PutAsync<T>(string path, HttpContent content, CancellationToken ct = default)
    {
        using var request = new HttpRequestMessage(HttpMethod.Put, path) { Content = content };
        using var response = await _http.SendAsync(request, ct).ConfigureAwait(false);
        return await ReadAsync<T>(response, path, ct).ConfigureAwait(false);
    }

    /// <summary>没有响应体的动作（DELETE / 204）也要走同一套错误映射。</summary>
    private async Task ThrowOnErrorAsync(HttpResponseMessage response, string path, CancellationToken ct)
    {
        var text = await response.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
        if (response.IsSuccessStatusCode) return;

        throw Error(response, path, text);
    }

    private async Task<T> ReadAsync<T>(HttpResponseMessage response, string path, CancellationToken ct)
    {
        var text = await response.Content.ReadAsStringAsync(ct).ConfigureAwait(false);

        if (!response.IsSuccessStatusCode) throw Error(response, path, text);

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

    /// <summary>
    /// 把错误响应翻成异常，并解析契约里的 <c>error.code</c>。
    /// 只有已经带着 Token 的请求被 401 才算「会话失效」：
    /// 登录接口本身返回 401 是口令错误，不能顺手把用户踢出登录页。
    /// </summary>
    private GatewayException Error(HttpResponseMessage response, string path, string text)
    {
        var status = (int)response.StatusCode;
        var (code, message) = ParseError(text);

        if (status == 401 && _token is not null) SessionExpired?.Invoke();

        return new GatewayException(
            message ?? $"{(int)response.StatusCode} {path}: {Trim(text)}",
            unauthorized: status is 401 or 403,
            status: status,
            code: code);
    }

    private static (string? Code, string? Message) ParseError(string text)
    {
        if (string.IsNullOrWhiteSpace(text)) return (null, null);

        try
        {
            using var document = JsonDocument.Parse(text);
            if (!document.RootElement.TryGetProperty("error", out var error)) return (null, null);

            var code = error.TryGetProperty("code", out var c) ? c.GetString() : null;
            var message = error.TryGetProperty("message", out var m) ? m.GetString() : null;
            return (code, message);
        }
        catch (JsonException)
        {
            return (null, null);
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

    public Task<MeDto> MeAsync(CancellationToken ct = default)
        => GetAsync<MeDto>("/api/v1/auth/me", ct);

    public Task<LoginResponseDto> LoginAsync(string username, string password, CancellationToken ct = default)
        => PostAsync<LoginResponseDto>("/api/v1/auth/login",
            JsonContent.Create(new LoginRequestDto(username, password), Info<LoginRequestDto>()), ct);

    public async Task LogoutAsync(CancellationToken ct = default)
    {
        using var request = new HttpRequestMessage(HttpMethod.Post, "/api/v1/auth/logout")
        {
            Content = new StringContent("{}", Encoding.UTF8, "application/json"),
        };

        using var response = await _http.SendAsync(request, ct).ConfigureAwait(false);
        // 会话可能已经过期；退出动作本身不需要向用户报错。
    }

    public Task<PasswordChangeResponseDto> ChangePasswordAsync(
        string currentPassword, string newPassword, CancellationToken ct = default)
        => PostAsync<PasswordChangeResponseDto>("/api/v1/auth/password",
            JsonContent.Create(new PasswordChangeRequestDto(currentPassword, newPassword),
                Info<PasswordChangeRequestDto>()), ct);

    public Task<Paged<UserDto>> UsersAsync(CancellationToken ct = default)
        => GetAsync<Paged<UserDto>>("/api/v1/users", ct);

    public Task<CreateUserResponseDto> CreateUserAsync(
        string id, string? displayName, string? role, string? password, CancellationToken ct = default)
        => PostAsync<CreateUserResponseDto>("/api/v1/users",
            JsonContent.Create(new CreateUserRequestDto(id, displayName, role, password),
                Info<CreateUserRequestDto>()), ct);

    public Task<UserDto> UpdateUserAsync(
        string id, string? displayName, string? role, bool? enabled, CancellationToken ct = default)
        => PutAsync<UserDto>($"/api/v1/users/{Uri.EscapeDataString(id)}",
            JsonContent.Create(new UpdateUserRequestDto(displayName, role, enabled),
                Info<UpdateUserRequestDto>()), ct);

    public Task<ResetPasswordResponseDto> ResetPasswordAsync(
        string id, string? password = null, CancellationToken ct = default)
        => PostAsync<ResetPasswordResponseDto>(
            $"/api/v1/users/{Uri.EscapeDataString(id)}/reset-password",
            JsonContent.Create(new ResetPasswordRequestDto(password), Info<ResetPasswordRequestDto>()), ct);

    public async Task DeleteUserAsync(string id, CancellationToken ct = default)
    {
        using var response = await _http
            .DeleteAsync($"/api/v1/users/{Uri.EscapeDataString(id)}", ct)
            .ConfigureAwait(false);

        await ThrowOnErrorAsync(response, "/api/v1/users/" + id, ct).ConfigureAwait(false);
    }

    public Task<Paged<AuditEntryDto>> AuditAsync(
        string? action = null,
        string? actor = null,
        string? result = null,
        int limit = 100,
        CancellationToken ct = default)
    {
        var query = new StringBuilder($"?limit={limit}");
        if (!string.IsNullOrWhiteSpace(action)) query.Append("&action=").Append(Uri.EscapeDataString(action));
        if (!string.IsNullOrWhiteSpace(actor)) query.Append("&actor=").Append(Uri.EscapeDataString(actor));
        if (!string.IsNullOrWhiteSpace(result)) query.Append("&result=").Append(Uri.EscapeDataString(result));

        return GetAsync<Paged<AuditEntryDto>>("/api/v1/audit" + query, ct);
    }

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
    public GatewayException(
        string message,
        bool unauthorized = false,
        int status = 0,
        string? code = null) : base(message)
    {
        Unauthorized = unauthorized;
        Status = status;
        Code = code;
    }

    public bool Unauthorized { get; }

    /// <summary>HTTP 状态码；非 HTTP 来源（如 SSE 断流）为 0。</summary>
    public int Status { get; }

    /// <summary>契约错误码，例如 <c>auth.password_change_required</c>。</summary>
    public string? Code { get; }
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
    PointsHealthDto Points, OutboxHealthDto Outbox, ProblemDto[] Problems,
    ResourcesHealthDto? Resources = null);

/// <summary>资源水位。null 读取值表示平台没提供，不是 0。</summary>
public sealed record ResourcesHealthDto(string Status, bool DiskLow, bool MemoryHigh,
    long? FreeBytes, long? DiskTotalBytes, long? WorkingSetBytes, DateTimeOffset Since);

public sealed record DevicesHealthDto(int Total, int Collecting, int Degraded, int Faulted);

public sealed record PointsHealthDto(int Total, int Good, int Stale, int Bad);

public sealed record OutboxHealthDto(long Pending, long DroppedLastHour);

public sealed record ProblemDto(string Device, string State, string? Reason, DateTimeOffset Since);

public sealed record VersionDto(string Name, string Version, string ApiVersion, string SchemaVersion,
    string Site, string Runtime, DateTimeOffset StartedAt, string ConfigVersion);

// ----------------------------------------------------------------------
// 认证与账号
// ----------------------------------------------------------------------

public sealed record LoginRequestDto(string Username, string Password);

public sealed record PasswordChangeRequestDto(string CurrentPassword, string NewPassword);

public sealed record CreateUserRequestDto(string Id, string? DisplayName, string? Role, string? Password);

public sealed record UpdateUserRequestDto(string? DisplayName, string? Role, bool? Enabled);

public sealed record ResetPasswordRequestDto(string? Password);

public sealed record MeDto(string Id, string? DisplayName, string Role, string[] Permissions,
    bool Enabled, bool MustChangePassword, string Kind);

public sealed record UserDto(string Id, string? DisplayName, string Role, string[] Permissions,
    bool Enabled, bool MustChangePassword, DateTimeOffset CreatedAt, DateTimeOffset? LastLoginAt = null);

public sealed record LoginResponseDto(string Token, DateTimeOffset ExpiresAt, UserDto User);

public sealed record PasswordChangeResponseDto(string Token, DateTimeOffset ExpiresAt, UserDto User);

public sealed record CreateUserResponseDto(UserDto User, string? TemporaryPassword);

public sealed record ResetPasswordResponseDto(string User, string? TemporaryPassword,
    bool MustChangePassword);

// ----------------------------------------------------------------------
// 审计
// ----------------------------------------------------------------------

/// <summary>
/// 审计条目。Detail 保持为原始 JSON：不同动作的细节结构不同，
/// 前端只把它当一段折叠文本展示，没必要为每种动作各建一个 DTO。
/// </summary>
public sealed record AuditEntryDto(long Seq, DateTimeOffset At, string Action, string Result,
    string Actor, string Source, string? Remote, string? RequestId, string? Target,
    JsonElement? Detail = null);

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
    bool Writable,
    // 点位周期：Interval 只在覆盖设备周期时出现；EffectiveInterval 始终有值，
    // 操作员据此判断「这个点位多久没更新算过期」，不必去翻设备配置。
    [property: JsonConverter(typeof(DurationConverter))] TimeSpan? Interval = null,
    [property: JsonConverter(typeof(DurationConverter))] TimeSpan EffectiveInterval = default,
    // 数组点位元素个数；0 表示标量。
    int Count = 0);

public sealed record DataValueDto(string Type, double? Num, bool? Bool, string? Text,
    double[]? Array = null)
{
    [JsonIgnore]
    public bool IsArray => Array is { Length: > 0 };

    [JsonIgnore]
    public string Display
    {
        get
        {
            // 数组在控制台里横向展开，不折叠成「Array」：操作员要一眼看到每个元素，
            // 而不是先点开再确认「哪一位跳了」。
            if (Array is { Length: > 0 } items)
            {
                return "[" + string.Join(", ", items.Select(Format)) + "]";
            }

            return Type switch
            {
                "bool" => Bool == true ? "true" : Bool == false ? "false" : "—",
                "string" or "bytes" or "datetime" => Text ?? "—",
                _ => Num is { } n ? Format(n) : "—",
            };
        }
    }

    private static string Format(double n)
        => Math.Abs(n - Math.Round(n)) < 1e-9
            ? n.ToString("0", System.Globalization.CultureInfo.InvariantCulture)
            : n.ToString("0.###", System.Globalization.CultureInfo.InvariantCulture);
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
