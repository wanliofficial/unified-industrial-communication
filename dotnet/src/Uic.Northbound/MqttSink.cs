using System.Collections.Concurrent;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging;
using Uic.Codec.Mqtt;
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

    public MqttSink(MqttOptions options, string site, ILogger log, string? dataDir = null)
    {
        _options = options;
        _site = site;
        _log = log;
        _client = new MqttClient(options, site, log, dataDir,
            // 北向的 CA 来自配置树；报错时必须指向那一行，而不是协议层的通用路径。
            caField: "northbound.mqtt.tls.caFile");
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
        await DeliverPartialAsync(batch, ct).ConfigureAwait(false);
    }

    /// <summary>
    /// 逐条发布并回报「实际送达 broker 的最后一条 id」。
    ///
    /// Outbox 按 at-least-once 工作：位点只在确认推进。若这里整批一起算成功，
    /// 发到一半断连时已发到 broker 的那几条会被整批回退重发——订阅者收重复、
    /// retained 被覆盖一遍。所以逐条发、逐条记，把真正送达的边界交回队列：
    /// 发到一半失败时抛 <see cref="PartialDeliveryException"/>，让队列确认已送达部分、
    /// 只对未送达部分重试。
    /// </summary>
    public async Task<long> DeliverPartialAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
    {
        var lastId = -1L;
        foreach (var record in batch)
        {
            var (device, point, payload) = Parse(record.Payload);
            var topic = RenderTopic(device, point);

            try
            {
                await _client.PublishAsync(topic, Encoding.UTF8.GetBytes(payload), _options.Retain, ct)
                    .ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                // 已发到 broker 的部分确认掉，未发的留给下一轮。断连只是传输问题，
                // 重试即可，不算数据损坏。
                throw new PartialDeliveryException(lastId,
                    $"mqtt delivery stopped at record {lastId}: {ex.Message}", ex);
            }

            lastId = record.Id;
        }

        return lastId;
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

    /// <summary>
    /// 设备被热更新删掉：清 retained 的 $state，并停止在重连时补发它。
    ///
    /// 只删内存不够——retained 是 broker 上的副本，新订阅者一上来就会读到
    /// 一台已经不存在的设备的「最后状态」，运维会去查一个配置里没有的设备。
    /// 清 retained 的协议动作是往同一主题发一个**空载荷 + retain**。
    /// </summary>
    public async Task ForgetStateAsync(string device, CancellationToken ct)
    {
        _lastStates.TryRemove(device, out _);

        try
        {
            var topic = $"{_options.TopicPrefix}/{MqttClient.Segment(_site)}/{MqttClient.Segment(device)}/$state";
            await _client.PublishAsync(topic, ReadOnlyMemory<byte>.Empty, retain: true, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            // 清不掉不是数据问题，broker 上会留一条过期的 retained；下次重连不会再补发它。
            _log.LogDebug("clearing retained state failed for {Device}: {Error}", device, ex.Message);
        }
    }

    private Task PublishStateRawAsync(string device, string payload, CancellationToken ct)
    {
        var topic = $"{_options.TopicPrefix}/{MqttClient.Segment(_site)}/{MqttClient.Segment(device)}/$state";
        return _client.PublishAsync(topic, Encoding.UTF8.GetBytes(payload), retain: true, ct);
    }

    public string RenderTopic(string device, string point)
        => Render(_options.Topic, _options, _site, device, point);

    /// <summary>
    /// 渲染主题模板。命令通道只用到 {prefix}/{site}，值主题额外用到 {device}/{point}。
    /// 占位符不在模板里时替换是空操作，所以统一走同一个方法。
    /// </summary>
    internal static string Render(string template, MqttOptions options, string site, string? device = null, string? point = null)
    {
        var value = string.IsNullOrWhiteSpace(template)
            ? "uic/{site}/{device}/{point}/value"
            : template;

        value = value
            .Replace("{prefix}", options.TopicPrefix, StringComparison.Ordinal)
            .Replace("{site}", MqttClient.Segment(site), StringComparison.Ordinal);

        if (device is not null)
            value = value.Replace("{device}", MqttClient.Segment(device), StringComparison.Ordinal);

        if (point is not null)
            value = value.Replace("{point}", MqttClient.Segment(point), StringComparison.Ordinal);

        return value;
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