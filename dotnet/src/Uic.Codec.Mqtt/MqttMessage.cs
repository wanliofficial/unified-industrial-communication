namespace Uic.Codec.Mqtt;

/// <summary>一条入站消息。主题已由 Broker 过滤，这里不再做通配匹配。</summary>
public sealed record MqttMessage(string Topic, byte Qos, bool Retain, byte[] Payload);
