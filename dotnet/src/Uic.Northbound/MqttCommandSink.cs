using System.Collections.Concurrent;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Obs;

namespace Uic.Northbound;

/// <summary>
/// MQTT 下行命令通道。
///
/// 契约刻意做成「请求/结果」两主题：命令从 <c>{prefix}/{site}/$command</c> 进入，
/// 每条命令都回一条结果到 <c>.../$command/result</c>，失败也回——
/// 只回成功的通道会让操作员分不清「没执行」和「执行了但结果丢了」。
///
/// 安全边界：这一版 MQTT 命令没有用户身份（TLS 只保护链路），因此只放行
/// 不会改变整份配置、不会丢数据的动作（重启/自检/冲刷/日志级别）；
/// 回滚与丢弃必须走 REST。
///
/// 幂等：发布方必须带 <c>id</c>（或 <c>idempotencyKey</c>）。同一 id 在 5 分钟
/// 窗口内重复到达时不再执行，直接重放首次结果并标 <c>replayed:true</c>。
/// 重连/重投都可能造成重复，这个窗口是操作员的纠正时间，不是持久台账。
/// </summary>
public sealed class MqttCommandSink : IAsyncDisposable
{
    public static readonly TimeSpan DedupWindow = TimeSpan.FromMinutes(5);

    private static readonly TimeSpan ExecutionTimeout = TimeSpan.FromSeconds(20);

    /// <summary>在没有用户身份的 MQTT 通道上允许执行的动作。集合外的动作会得到 command.forbidden。</summary>
    public static IReadOnlyList<string> AllowedActions { get; } = new[]
    {
        "driver.restart",
        "device.selftest",
        "outbox.flush",
        "log.level",
        "diag.bundle",
    };

    private readonly MqttClient _client;
    private readonly IGatewayControl _control;
    private readonly ILogger _log;
    private readonly string _site;
    private readonly AuditLog? _audit;
    private readonly Func<MqttClient, MqttMessage, CancellationToken, Task> _handler;
    private readonly ConcurrentDictionary<string, CacheEntry> _cache = new(StringComparer.Ordinal);
    private int _sweep;
    private int _disposed;

    private sealed record CacheEntry(DateTimeOffset At, TaskCompletionSource<CommandResult> Result);

    private sealed record CommandResult(
        string? Id,
        string Action,
        bool Ok,
        string? Detail,
        string? ErrorCode,
        string? ErrorMessage,
        DateTimeOffset At);

    public MqttCommandSink(
        MqttClient client,
        MqttOptions options,
        string site,
        IGatewayControl control,
        ILogger log,
        AuditLog? audit = null)
    {
        _client = client;
        _control = control;
        _log = log;
        _site = site;
        _audit = audit;

        Topic = MqttSink.Render(NonEmpty(options.CommandTopic, $"{options.TopicPrefix}/{{site}}/$command"),
            options, site);
        ResultTopic = MqttSink.Render(
            NonEmpty(options.CommandResultTopic, $"{options.TopicPrefix}/{{site}}/$command/result"),
            options, site);

        // 订阅必须在 client.Start() 之前登记；运行时装配顺序保证了这一点。
        _handler = HandleAsync;
        client.OnMessage = _handler;
        client.Subscribe(Topic, Math.Clamp(options.Qos, 0, 1));
    }

    public string Topic { get; }

    public string ResultTopic { get; }

    private async Task HandleAsync(MqttClient client, MqttMessage message, CancellationToken ct)
    {
        if (!string.Equals(message.Topic, Topic, StringComparison.Ordinal))
        {
            _log.LogDebug("ignoring inbound message on unexpected topic {Topic}", message.Topic);
            return;
        }

        if (!TryParse(message.Payload, out var request, out var id, out var action, out var parseError))
        {
            _log.LogWarning("rejected malformed mqtt command: {Error}", parseError);
            await PublishResultAsync(
                new CommandResult(id, action, false, null, "request.invalid", parseError,
                    DateTimeOffset.UtcNow),
                replayed: false, ct).ConfigureAwait(false);
            return;
        }

        var key = id!;
        var now = DateTimeOffset.UtcNow;
        var entry = new CacheEntry(now,
            new TaskCompletionSource<CommandResult>(TaskCreationOptions.RunContinuationsAsynchronously));

        if (_cache.TryAdd(key, entry))
        {
            Sweep(now);

            CommandResult result;
            try
            {
                result = await ExecuteAsync(request!, id!, now, ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                // 停机/断链：不缓存半成品，重连后同一 id 可以真正重试。
                _cache.TryRemove(key, out _);
                throw;
            }
            catch (Exception ex)
            {
                result = new CommandResult(id, action, false, null,
                    "command.failed", ex.Message, DateTimeOffset.UtcNow);
            }

            entry.Result.TrySetResult(result);

            _log.LogInformation("mqtt command {Action} ({Id}) -> ok={Ok}{Detail}",
                action, id, result.Ok, result.Detail is null ? "" : $": {result.Detail}");

            await PublishResultAsync(result, replayed: false, ct).ConfigureAwait(false);
            return;
        }

        // 重复 id：等首次执行的结果，原样重放。绝不第二次执行命令。
        if (!_cache.TryGetValue(key, out var existing))
        {
            // 理论上只会在窗口清理时发生；按未知处理而不是再执行一次。
            await PublishResultAsync(
                new CommandResult(id, action, false, null, "command.duplicate_unavailable",
                    "the original command is no longer cached; resend with a new id",
                    DateTimeOffset.UtcNow),
                replayed: false, ct).ConfigureAwait(false);
            return;
        }

        CommandResult cached;
        try
        {
            cached = await existing.Result.Task.WaitAsync(ExecutionTimeout, ct).ConfigureAwait(false);
        }
        catch (TimeoutException)
        {
            await PublishResultAsync(
                new CommandResult(id, action, false, null, "command.in_flight",
                    "the original command is still running", DateTimeOffset.UtcNow),
                replayed: true, ct).ConfigureAwait(false);
            return;
        }

        _log.LogInformation("mqtt command {Action} ({Id}) replayed (duplicate id)", action, id);
        await PublishResultAsync(cached, replayed: true, ct).ConfigureAwait(false);
    }

    private async Task<CommandResult> ExecuteAsync(
        CommandRequestDto request, string id, DateTimeOffset at, CancellationToken ct)
    {
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
        timeout.CancelAfter(ExecutionTimeout);

        try
        {
            var response = await CommandExecutor
                .ExecuteAsync(_control, request, CommandScope.Standard, timeout.Token)
                .ConfigureAwait(false);

            // MQTT 这一版没有账号，actor 与 REST 无令牌时一样记 anonymous，由 source 区分通道；
            // 命令参数里可能有设备名，详情原样记下，方便回答「这条命令当时到底发了什么」。
            _audit?.Write(
                request.Action,
                response.Ok ? "ok" : "failed",
                actor: "anonymous",
                source: "mqtt",
                target: id,
                detail: new { id, response.Detail, request.Params });

            return new CommandResult(id, request.Action, response.Ok, response.Detail, null, null, at);
        }
        catch (ApiException ex)
        {
            _audit?.Write(request.Action, "failed", "anonymous", "mqtt", target: id,
                detail: new { id, error = ex.Code, ex.Message, request.Params });

            return new CommandResult(id, request.Action, false, null, ex.Code, ex.Message, at);
        }
        catch (KeyNotFoundException ex)
        {
            _audit?.Write(request.Action, "failed", "anonymous", "mqtt", target: id,
                detail: new { id, error = "request.not_found", ex.Message, request.Params });

            return new CommandResult(id, request.Action, false, null,
                "request.not_found", ex.Message, at);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (OperationCanceledException)
        {
            _audit?.Write(request.Action, "failed", "anonymous", "mqtt", target: id,
                detail: new { id, error = "command.timeout", request.Params });

            return new CommandResult(id, request.Action, false, null,
                "command.timeout", $"command did not finish within {ExecutionTimeout.TotalSeconds:0}s", at);
        }
        catch (Exception ex)
        {
            _audit?.Write(request.Action, "failed", "anonymous", "mqtt", target: id,
                detail: new { id, error = "command.failed", ex.Message, request.Params });

            return new CommandResult(id, request.Action, false, null,
                "command.failed", ex.Message, at);
        }
    }

    private async Task PublishResultAsync(CommandResult result, bool replayed, CancellationToken ct)
    {
        var payload = Render(result, replayed);

        try
        {
            await _client.PublishAsync(ResultTopic, Encoding.UTF8.GetBytes(payload), retain: false, ct)
                .ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            // 结果发不出去不能影响后续命令，但必须留痕：操作员会一直等这条结果。
            _log.LogWarning("failed to publish mqtt command result for {Id}: {Error}",
                result.Id ?? "(no id)", ex.Message);
        }
    }

    private string Render(CommandResult result, bool replayed)
    {
        var node = new JsonObject
        {
            ["schemaVersion"] = 1,
            ["id"] = result.Id,
            ["action"] = result.Action,
            ["ok"] = result.Ok,
            ["detail"] = result.Detail,
            ["replayed"] = replayed,
            ["at"] = result.At,
            ["site"] = _site,
        };

        if (!result.Ok)
        {
            node["error"] = new JsonObject
            {
                ["code"] = result.ErrorCode,
                ["message"] = result.ErrorMessage,
            };
        }

        return node.ToJsonString(ApiJson.Options);
    }

    private static bool TryParse(
        byte[] payload,
        out CommandRequestDto? request,
        out string? id,
        out string action,
        out string error)
    {
        request = null;
        id = null;
        action = "";
        error = "";

        try
        {
            if (JsonNode.Parse(payload) is not JsonObject body)
            {
                error = "command payload must be a JSON object";
                return false;
            }

            // id 是幂等键；兼容常见的 idempotencyKey 命名。
            id = body["id"]?.GetValue<string>() ?? body["idempotencyKey"]?.GetValue<string>();
            action = body["action"]?.GetValue<string>() ?? "";

            if (string.IsNullOrWhiteSpace(id))
            {
                error = "command payload requires a non-empty 'id'";
                return false;
            }

            if (string.IsNullOrWhiteSpace(action))
            {
                error = "command payload requires a non-empty 'action'";
                return false;
            }

            var parameters = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            if (body["params"] is JsonObject values)
            {
                foreach (var (name, value) in values)
                {
                    if (value is null) continue;
                    parameters[name] = value.GetValueKind() == JsonValueKind.String
                        ? value.GetValue<string>()
                        : value.ToJsonString();
                }
            }

            var confirm = body["confirm"]?.GetValue<bool>() ?? false;

            request = new CommandRequestDto(action, parameters, confirm);
            return true;
        }
        catch (Exception ex) when (ex is JsonException or FormatException or InvalidOperationException or NotSupportedException)
        {
            error = ex.Message;
            request = null;
            return false;
        }
    }

    /// <summary>窗口清理只在有命令进来时做，不额外起计时器——命令通道本来就是低频的。</summary>
    private void Sweep(DateTimeOffset now)
    {
        if ((Interlocked.Increment(ref _sweep) & 0x1F) != 0) return;

        foreach (var (key, entry) in _cache)
        {
            if (now - entry.At > DedupWindow) _cache.TryRemove(key, out _);
        }
    }

    private static string NonEmpty(string? value, string fallback)
        => string.IsNullOrWhiteSpace(value) ? fallback : value;

    public ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return ValueTask.CompletedTask;

        // 连接与读循环归 MqttSink/MqttClient 所有；这里只放开回调引用。
        if (ReferenceEquals(_client.OnMessage, _handler)) _client.OnMessage = null;

        return ValueTask.CompletedTask;
    }
}
