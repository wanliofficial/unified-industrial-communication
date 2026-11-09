namespace Uic.Northbound;

/// <summary>
/// 命令通道的能力边界。REST 在本机监听、可挂 Bearer Token；MQTT 这一版
/// 没有用户身份（TLS 只保护链路，不认证发布者），所以不能让同一个请求
/// 在两条通道上拥有同等权限。
/// </summary>
[Flags]
public enum CommandScope
{
    Standard = 0,

    /// <summary>config.rollback：会切换整份配置，只允许在 REST 上执行。</summary>
    ConfigRollback = 1,

    /// <summary>outbox.drop：会丢弃待投递数据，只允许在 REST 上执行。</summary>
    OutboxDrop = 2,

    All = ConfigRollback | OutboxDrop,
}

/// <summary>
/// /api/v1/commands 与 MQTT 下行命令共用的执行器。
///
/// 语义只写一份：两边各自 switch 一遍，时间一长必然出现「REST 能回滚、
/// MQTT 悄悄也能回滚」这类分叉。参数缺失、确认要求、未知动作统一抛
/// <see cref="ApiException"/>，REST 由中间件渲染，MQTT 转成 !ok 结果。
/// </summary>
public static class CommandExecutor
{
    public static async Task<CommandResponseDto> ExecuteAsync(
        IGatewayControl control,
        CommandRequestDto request,
        CommandScope scope,
        CancellationToken ct)
    {
        var parameters = request.Params ?? new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);

        string Param(string name) =>
            parameters.TryGetValue(name, out var value) && !string.IsNullOrWhiteSpace(value)
                ? value
                : throw ApiException.BadRequest("request.missing_parameter",
                    $"command '{request.Action}' requires param '{name}'");

        switch (request.Action)
        {
            case "driver.restart":
            {
                var device = Param("device");
                await control.RestartDeviceAsync(device, ct).ConfigureAwait(false);
                return new CommandResponseDto(request.Action, true, $"device '{device}' restarted");
            }

            case "device.selftest":
            {
                var device = Param("device");
                var ok = await control.SelfTestAsync(device, ct).ConfigureAwait(false);
                return new CommandResponseDto(request.Action, ok,
                    ok ? "self test passed" : "self test failed");
            }

            case "outbox.flush":
            {
                // sink 为空表示全部；指定后只推那一条通道，便于「只补 MQTT 不补 journal」。
                parameters.TryGetValue("sink", out var sink);
                sink = string.IsNullOrWhiteSpace(sink) ? null : sink;

                await control.FlushOutboxAsync(sink, ct).ConfigureAwait(false);
                return new CommandResponseDto(request.Action, true,
                    sink is null ? "outbox flushed" : $"outbox flushed for '{sink}'");
            }

            case "diag.bundle":
            {
                var path = await control.GenerateDiagnosticsAsync(ct).ConfigureAwait(false);
                return new CommandResponseDto(request.Action, true, $"diagnostics bundle written to {path}");
            }

            case "outbox.drop":
                Require(scope, CommandScope.OutboxDrop, request.Action);

                if (!request.Confirm)
                    throw ApiException.BadRequest("request.confirm_required",
                        "outbox.drop requires \"confirm\": true");

                return new CommandResponseDto(request.Action, false,
                    "outbox.drop is not implemented: dropping pending records needs a retention policy");

            case "config.rollback":
                Require(scope, CommandScope.ConfigRollback, request.Action);

                if (!request.Confirm)
                    throw ApiException.BadRequest("request.confirm_required",
                        "config.rollback requires \"confirm\": true");

                var version = Param("version");
                var outcome = await control.RollbackAsync(version, ct).ConfigureAwait(false);
                return new CommandResponseDto(request.Action, true,
                    $"rolled back to {version} as {outcome.Version}");

            case "log.level":
            {
                var level = Param("level");
                control.SetLogLevel(level);
                return new CommandResponseDto(request.Action, true, $"log level set to {level}");
            }

            default:
                throw ApiException.BadRequest("request.unknown_action",
                    $"unknown action '{request.Action}'");
        }
    }

    private static void Require(CommandScope scope, CommandScope required, string action)
    {
        if ((scope & required) == 0)
        {
            throw ApiException.Forbidden("command.forbidden",
                $"command '{action}' is not allowed on this channel");
        }
    }
}
