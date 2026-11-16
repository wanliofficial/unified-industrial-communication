using System.Text.Json.Nodes;

namespace Uic.Domain;

/// <summary>
/// 控制台「新建设备」用的配置草稿。
///
/// 页面跑在裁剪后的 WASM 里、只引用 <c>Uic.Domain</c>：它既用不了 <c>Uic.Config</c> 的
/// <c>ConfigStore</c>，也不该在浏览器里再实现一套校验。所以这里只负责把「表单里填的
/// 几个字段」变成配置树上的节点，真正的校验仍由服务端的 <see cref="ModelValidator"/>
/// 完成——这也让「页面拼出来的 JSON 网关认不认」可以在测试里用真的解析器验一遍。
/// </summary>
public static class ConfigDraft
{
    /// <summary>协议决定链路类型。让调用方自己填 transport，迟早出现「tcp 通道没有 host」。</summary>
    public static string TransportFor(string protocol) => protocol switch
    {
        "modbus-tcp" => "tcp",
        "modbus-rtu" or "modbus-ascii" => "serial",
        "virtual" => "none",
        // S7 只有 ISO-on-TCP 一种承载；显式列出来，免得以后有人给别的协议
        // 加传输方式时顺手把 S7 也改了。
        "s7" => "tcp",
        // OPC UA 的端点是 opc.tcp://，与 Modbus TCP 同样是 TCP 承载，
        // 但端口默认值是 4840 而不是 502（见 DefaultPort）。
        "opcua" => "tcp",
        // MQTT 订阅方连的是 Broker，同样是 TCP 承载，端口默认 1883。
        "mqtt" => "tcp",
        // BACnet/IP 跑在 UDP 上（Annex J），是唯一非 TCP 的 IP 承载。
        "bacnet" => "udp",
        _ => "tcp",
    };

    /// <summary>
    /// 协议默认端口。填错端口的现象是「连不上」而不是「协议不对」，
    /// 所以默认值必须按协议来，不能让所有人都去查手册。
    /// </summary>
    public static int DefaultPort(string protocol) => protocol switch
    {
        "modbus-tcp" => 502,
        "s7" => 102,
        "opcua" => 4840,
        "mqtt" => 1883,
        // BACnet/IP 的标准端口是 0xBAC0 = 47808。
        "bacnet" => 47808,
        _ => 502,
    };

    /// <summary>新增一条通道。只写与 transport 相关的字段，其余交给配置默认值。</summary>
    public static JsonObject NewChannel(
        string id,
        string? name,
        string protocol,
        string? host = null,
        int port = 0,
        string? serialPort = null,
        int baud = 9600)
    {
        var transport = TransportFor(protocol);

        var channel = new JsonObject
        {
            ["id"] = id,
            ["protocol"] = protocol,
            ["transport"] = transport,
        };

        if (!string.IsNullOrWhiteSpace(name)) channel["name"] = name.Trim();

        switch (transport)
        {
            case "tcp":
                if (!string.IsNullOrWhiteSpace(host)) channel["host"] = host.Trim();
                if (port > 0) channel["port"] = port;
                break;

            case "serial":
                channel["serial"] = new JsonObject
                {
                    ["port"] = serialPort,
                    ["baud"] = baud,
                };
                break;
        }

        return channel;
    }

    /// <summary>新增一台设备。至少要有一个点位——校验器明确拒绝零点位设备。</summary>
    public static JsonObject NewDevice(
        string id,
        string? name,
        string channelId,
        TimeSpan interval,
        int unitId,
        JsonObject point)
    {
        var device = new JsonObject
        {
            ["id"] = id,
            ["channelId"] = channelId,
            ["interval"] = FormatDuration(interval),
            ["unitId"] = unitId,
            ["points"] = new JsonArray(point),
        };

        if (!string.IsNullOrWhiteSpace(name)) device["name"] = name.Trim();
        return device;
    }

    /// <summary>
    /// 寄存器点位。地址只取「区域 + 偏移[.位]」：区域来自下拉框，
    /// 这样浏览器里不必再抄一遍 <c>40001</c> 这类参考号的解析规则。
    /// </summary>
    public static JsonObject NewRegisterPoint(
        string id,
        string? name,
        string dataType,
        string area,
        int offset,
        int bit = -1)
    {
        var address = new JsonObject
        {
            ["kind"] = "register",
            ["area"] = area,
            ["offset"] = offset,
        };

        if (bit >= 0) address["bit"] = bit;

        var point = new JsonObject
        {
            ["id"] = id,
            ["address"] = address,
            ["dataType"] = dataType,
        };

        if (!string.IsNullOrWhiteSpace(name)) point["name"] = name.Trim();
        return point;
    }

    /// <summary>
    /// 符号点位（目前只有 OPC UA 用）。地址是一段 NodeId 文本，形如
    /// <c>ns=2;s=Demo.Temp</c>；省略 <c>ns=</c> 时用 <paramref name="index"/> 作命名空间。
    ///
    /// 不把 NodeId 拆成「命名空间 + 标识符」两个表单字段：规范文本是运维在客户端里
    /// 看到的原样，拆开再拼回去只会多一个出错的地方。
    /// </summary>
    public static JsonObject NewSymbolPoint(
        string id,
        string? name,
        string dataType,
        string path,
        int index = 0)
    {
        var address = new JsonObject
        {
            ["kind"] = "symbol",
            ["path"] = path.Trim(),
        };

        if (index > 0) address["index"] = index;

        var point = new JsonObject
        {
            ["id"] = id,
            ["address"] = address,
            ["dataType"] = dataType,
        };

        if (!string.IsNullOrWhiteSpace(name)) point["name"] = name.Trim();
        return point;
    }

    /// <summary>把通道挂到配置树上。返回 null 表示成功，否则是给人看的原因。</summary>
    public static string? AddChannel(JsonObject config, JsonObject channel)
    {
        var id = IdOf(channel);
        if (string.IsNullOrWhiteSpace(id)) return "通道 ID 不能为空";

        var channels = ArrayOf(config, "channels");
        if (channels.Any(item => IdOf(item) == id))
            return $"通道 '{id}' 已存在，请换一个 ID";

        channels.Add(channel);
        return null;
    }

    /// <summary>
    /// 把设备挂到配置树上。先查重、再确认引用的通道存在：
    /// 这两条错误最常见，在这里说清楚比让网关回一个 422 更好读。
    /// </summary>
    public static string? AddDevice(JsonObject config, JsonObject device)
    {
        var id = IdOf(device);
        if (string.IsNullOrWhiteSpace(id)) return "设备 ID 不能为空";

        var devices = ArrayOf(config, "devices");
        if (devices.Any(item => IdOf(item) == id))
            return $"设备 '{id}' 已存在，请换一个 ID";

        var channelId = device["channelId"]?.ToString();
        if (string.IsNullOrWhiteSpace(channelId)) return "设备必须关联一条通道";

        if (!ArrayOf(config, "channels").Any(item => IdOf(item) == channelId))
            return $"通道 '{channelId}' 不存在";

        devices.Add(device);
        return null;
    }

    /// <summary>把时长写成配置文件里的短写法（<c>500ms</c> / <c>1s</c> / <c>2m</c>）。</summary>
    public static string FormatDuration(TimeSpan value)
    {
        if (value <= TimeSpan.Zero) return "0ms";

        if (value.TotalSeconds < 60 && value.TotalSeconds % 1 == 0)
            return $"{(int)value.TotalSeconds}s";

        if (value.TotalMinutes < 60 && value.TotalMinutes % 1 == 0)
            return $"{(int)value.TotalMinutes}m";

        return $"{(long)value.TotalMilliseconds}ms";
    }

    private static JsonArray ArrayOf(JsonObject config, string key)
    {
        if (config[key] is JsonArray existing) return existing;

        var created = new JsonArray();

        // JsonNode 只能有一个父节点：先挂到树上，后续 Add 才会真正生效。
        config[key] = created;
        return created;
    }

    // 配置可能来自旧文件或人手编辑：不是对象的条目直接当作「没有 id」，不要在这里抛异常。
    private static string? IdOf(JsonNode? node)
        => node is JsonObject obj ? obj["id"]?.ToString() : null;
}
