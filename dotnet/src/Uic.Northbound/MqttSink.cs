using System.Collections.Concurrent;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Outbox;

namespace Uic.Northbound;

/// <summary>
/// MQTT 投递 sink。
///
/// 与 Outbox 的分工：Outbox 负责「不丢」，这里只负责「发出去并拿到 PUBACK」。
/// 任何一次发布失败都必须抛出，让 Outbox 保留位点重试。
/// </summary>
public sealed class MqttSink : IOutboxSink, IAsyncDisposable
{
    private readonly MqttClient _client;
    private readonly MqttOptions _options;
    private readonly string _site;
    private readonly ILogger _log;
    private readonly ConcurrentDictionary<string, string> _lastStates = new(StringComparer.Ordinal);

    public MqttSink(MqttOptions options, string site, ILogger log)
    {
        _options = options;
        _site = site;
        _log = log;
        _client = new MqttClient(options, site, log);
        _client.OnConnected = OnConnectedAsync;
    }

    public string Name => "mqtt";

    public MqttClient Client => _client;

    public void Start() => _client.Start();

    private async Task OnConnectedAsync(MqttClient client, CancellationToken ct)
    {
        // Broker 重启会清空 retained，所以每次重连都要补发 $birth 与全部已知状态。
        await PublishBirthAsync(ct).ConfigureAwait(false);

        foreach (var (device, state) in _lastStates)
        {
            await PublishStateRawAsync(device, state, ct).ConfigureAwait(false);
        }
    }

    public async Task DeliverAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
    {
        foreach (var record in batch)
        {
            var (device, point, payload) = Parse(record.Payload);
            var topic = RenderTopic(device, point);

            await _client.PublishAsync(topic, Encoding.UTF8.GetBytes(payload), _options.Retain, ct)
                .ConfigureAwait(false);
        }
    }

    public Task PublishBirthAsync(CancellationToken ct)
    {
        var payload = JsonSerializer.Serialize(new
        {
            schemaVersion = 1,
            online = true,
            startedAt = DateTimeOffset.UtcNow,
        }, ApiJson.Options);

        var topic = $"{_options.TopicPrefix}/{MqttClient.Segment(_site)}/$birth";
        return _client.PublishAsync(topic, Encoding.UTF8.GetBytes(payload), retain: true, ct);
    }

    public async Task PublishDeathAsync(CancellationToken ct)
    {
        var payload = JsonSerializer.Serialize(new { schemaVersion = 1, online = false }, ApiJson.Options);
        var topic = $"{_options.TopicPrefix}/{MqttClient.Segment(_site)}/$death";

        try
        {
            await _client.PublishAsync(topic, Encoding.UTF8.GetBytes(payload), retain: true, ct)
                .ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            // 停机路径上发不出去不能影响退出码，但要留痕。
            _log.LogWarning("failed to publish $death: {Error}", ex.Message);
        }
    }

    public async Task PublishStateAsync(string device, string state, string? previous, string? error, CancellationToken ct)
    {
        var payload = JsonSerializer.Serialize(new
        {
            schemaVersion = 1,
            state,
            previous,
            at = DateTimeOffset.UtcNow,
            error,
        }, ApiJson.Options);

        _lastStates[device] = payload;

        try
        {
            await PublishStateRawAsync(device, payload, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _log.LogDebug("state publish failed for {Device}: {Error}", device, ex.Message);
        }
    }

    private Task PublishStateRawAsync(string device, string payload, CancellationToken ct)
    {
        var topic = $"{_options.TopicPrefix}/{MqttClient.Segment(_site)}/{MqttClient.Segment(device)}/$state";
        return _client.PublishAsync(topic, Encoding.UTF8.GetBytes(payload), retain: true, ct);
    }

    public string RenderTopic(string device, string point)
    {
        var template = string.IsNullOrWhiteSpace(_options.Topic)
            ? "uic/{site}/{device}/{point}/value"
            : _options.Topic;

        return template
            .Replace("{prefix}", _options.TopicPrefix, StringComparison.Ordinal)
            .Replace("{site}", MqttClient.Segment(_site), StringComparison.Ordinal)
            .Replace("{device}", MqttClient.Segment(device), StringComparison.Ordinal)
            .Replace("{point}", MqttClient.Segment(point), StringComparison.Ordinal);
    }

    /// <summary>
    /// 从队列载荷里取出 device/point。
    /// 载荷本身就是契约里的 Sample JSON，所以不需要额外的路由元数据。
    /// </summary>
    private static (string Device, string Point, string Payload) Parse(string payload)
    {
        try
        {
            var node = JsonNode.Parse(payload) as JsonObject;

            var device = node?["device"]?.GetValue<string>();
            var point = node?["point"]?.GetValue<string>();

            if (string.IsNullOrEmpty(device) || string.IsNullOrEmpty(point))
                throw new FormatException("payload is missing device/point");

            // 附加 schemaVersion：消费方据此判断契约版本，而不必看配置。
            node!["schemaVersion"] = 1;

            return (device, point, node.ToJsonString(ApiJson.Options));
        }
        catch (Exception ex) when (ex is JsonException or FormatException or InvalidOperationException)
        {
            throw new InvalidOperationException($"malformed outbox payload for mqtt: {ex.Message}", ex);
        }
    }

    public ValueTask DisposeAsync() => _client.DisposeAsync();
}