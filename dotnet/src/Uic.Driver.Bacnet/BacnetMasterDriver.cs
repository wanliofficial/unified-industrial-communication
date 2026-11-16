using Microsoft.Extensions.Logging;
using Uic.Codec.Bacnet;
using Uic.Domain;
using Uic.Driver;
using Uic.Transport;

namespace Uic.Driver.Bacnet;

/// <summary>
/// BACnet/IP 主站（ASHRAE 135 Annex J）。
///
/// 三层结构与 S7 同构：<see cref="BacnetFrame"/> 管 BVLC/NPDU 封拆帧、
/// <see cref="BacnetApdu"/> 管服务请求/响应、<see cref="BacnetTags"/> 管应用标签，
/// 驱动只做「点位 → 对象标识符 + 属性 → 请求 → 值」的翻译。
///
/// 与 TCP 驱动的关键差别在会话层：BACnet/IP 跑在 UDP 上，没有「连接」，
/// 也没有字节流可以按长度切帧。因此：
/// - 不缓存会话，每次读都发一个独立请求（UDP 无连接，探活与读写走同一条路）；
/// - 一条数据报就是一条完整报文，读完整个数据报再解析；
/// - 用 invoke-id 把响应配对到请求，晚到的上一条响应会被丢弃重读，而不是当成这次的值。
///   这一条是本驱动最重要的正确性保证：配对错了，点位质量仍是 good，读到的是别的点位的值。
///
/// 明确不支持（配置校验或运行期明确报错，不静默降级）：
/// - 分段响应（segmented ComplexAck）：本实现不重组，遇到明确报错；
/// - ReadPropertyMultiple / COV 订阅 / 写优先级与数组索引；
/// - 通过 BACnet 路由器跨网段（NPDU 控制字节固定为 0，即一跳直连）。
/// </summary>
public sealed class BacnetMasterDriver : IDriver
{
    public DriverMeta Meta { get; } = new()
    {
        Id = "bacnet",
        Name = "BACnet/IP 主站",
        Version = "1.0.0",
        Roles = new[] { DriverRole.Master },
        Transports = new[] { "udp" },
        AddressKinds = new[] { AddressKind.Object },
        Capabilities = new Capabilities(
            Read: CapState.Supported,
            Write: CapState.Supported,
            // 对象/属性发现需要 ReadPropertyMultiple 与设备对象浏览，本轮不做。
            Discovery: CapState.Unsupported,
            // 每个点位一个请求，没有「一个请求读多个点位」的能力。
            MultiRead: CapState.Unsupported,
            Subscribe: CapState.Unsupported),
        Description = "BACnet/IP 主站：按对象类型 + 实例 + 属性读单个属性（ReadProperty），支持写（WriteProperty）。",
        ConfigFields = new[]
        {
            new ConfigField("transport.host", "主机", "string", Required: true,
                Placeholder: "192.168.1.20", Scope: "channel"),
            new ConfigField("transport.port", "端口", "int", Required: true, Default: 47808, Scope: "channel",
                Help: "BACnet/IP 的标准端口是 47808（0xBAC0）"),
        },
        PointFields = new[]
        {
            new ConfigField("address.objectType", "对象类型", "int", Required: true, Scope: "point",
                Placeholder: "0",
                Help: "BACnet 对象类型号：0=analog-input，1=analog-output，2=analog-value，3=binary-input，8=device"),
            new ConfigField("address.instance", "实例号", "int", Required: true, Scope: "point",
                Help: "对象实例号（0..4194303）"),
            new ConfigField("address.property", "属性", "string", Required: true, Scope: "point",
                Placeholder: "85",
                Help: "属性标识符：85=present-value，77=object-name，111=status-flags"),
        },
    };

    public IConn Open(OpenContext context) => new BacnetConn(context);
}

/// <summary>一条到 BACnet 设备（或广播域）的链路。</summary>
internal sealed class BacnetConn : IConn
{
    private const string Protocol = "bacnet";

    private readonly OpenContext _ctx;
    private readonly int _deviceInstance;
    private IByteStream? _stream;
    private byte _invokeId;
    private bool _disposed;

    public BacnetConn(OpenContext ctx)
    {
        _ctx = ctx;
        _deviceInstance = ParseInt(ctx.Device, "deviceInstance", -1, -1, BacnetObjectId.MaxInstance);
    }

    /// <summary>
    /// 探活：Who-Is 广播 + 等 I-Am。
    ///
    /// 为什么不直接读一个属性——属性读要指定对象，而配置里没有「一定存在」的对象；
    /// Who-Is 只要求对面是个 BACnet 设备（这是协议规定必须响应的服务）。
    /// 配了 deviceInstance 时核对实例号：同网段常有多台设备，回错一台同样是「通」。
    /// </summary>
    public async Task ProbeAsync(CancellationToken ct)
    {
        var stream = await EnsureStreamAsync(ct).ConfigureAwait(false);

        await stream.WriteAsync(
            BacnetFrame.Build(BacnetApdu.BuildWhoIs(), broadcast: true), ct).ConfigureAwait(false);

        var deadline = DateTimeOffset.UtcNow + Timeout;

        while (DateTimeOffset.UtcNow < deadline)
        {
            var remaining = deadline - DateTimeOffset.UtcNow;
            var apdu = await ReceiveApduAsync(stream, remaining, ct).ConfigureAwait(false);
            if (apdu is null) continue;

            if (BacnetApdu.IsIAm(apdu))
            {
                var instance = BacnetApdu.ParseIAmDeviceInstance(apdu);
                if (_deviceInstance >= 0 && instance != _deviceInstance)
                {
                    _ctx.Log.LogDebug(
                        "bacnet: ignoring I-Am from device {Seen}, configured deviceInstance is {Expected}",
                        instance, _deviceInstance);
                    continue;
                }

                _ctx.Log.LogDebug("bacnet: probe reached device {Instance} at {Remote}", instance, stream.Remote);
                return;
            }

            // 广播域上还可能有别的报文（其它主站的请求）。按协议类型跳过，不当成失败。
            _ctx.Log.LogTrace("bacnet: ignoring {Length}-byte non-I-Am payload during probe", apdu.Length);
        }

        throw DriverException.Timeout(Protocol, "probe",
            _deviceInstance >= 0
                ? $"no I-Am from device {_deviceInstance} at {stream.Remote} within {Timeout.TotalMilliseconds:0}ms"
                : $"no I-Am from any device at {stream.Remote} within {Timeout.TotalMilliseconds:0}ms; " +
                  "check that the port is 47808/UDP and that a BACnet device answers Who-Is on this subnet");
    }

    public async Task<IReadOnlyList<RawSample>> ReadAsync(IReadOnlyList<ReadSpan> spans, CancellationToken ct)
    {
        var all = new List<RawSample>();

        foreach (var span in spans)
        {
            foreach (var point in span.Points)
            {
                try
                {
                    all.Add(await ReadPointAsync(point, ct).ConfigureAwait(false));
                }
                catch (OperationCanceledException) when (ct.IsCancellationRequested)
                {
                    throw;
                }
                catch (DriverException ex)
                {
                    // 一个点位失败不拖垮整轮：与 Modbus/S7 同一语义。
                    all.Add(new RawSample(point.Id, null, QualityMap.From(ex.Kind), null, ex.Message));
                }
                catch (Exception ex)
                {
                    all.Add(new RawSample(point.Id, null, Quality.Bad, null, ex.Message));
                }
            }
        }

        return all;
    }

    private async Task<RawSample> ReadPointAsync(PointRef point, CancellationToken ct)
    {
        var id = BacnetObjectId.From(point.Address);
        var property = BacnetObjectId.PropertyOf(point.Address);
        var label = $"{id.Describe()} {BacnetConstants.DescribeProperty(property)}";

        var stream = await EnsureStreamAsync(ct).ConfigureAwait(false);

        var invokeId = NextInvokeId();
        var request = BacnetFrame.Build(
            BacnetApdu.BuildReadProperty(id, property, invokeId), broadcast: false);

        await stream.WriteAsync(request, ct).ConfigureAwait(false);

        var response = await AwaitResponseAsync(stream, invokeId, label, "read", ct).ConfigureAwait(false);
        var ack = BacnetApdu.ParseReadProperty(response, invokeId);

        if (!ack.Ok && ack.Kind != ErrorKind.Protocol)
            throw new DriverException(ack.Kind, Protocol, "read", $"{label}: {ack.Describe}");

        if (!ack.Ok)
            throw DriverException.ProtocolError(Protocol, "read", $"{label}: {ack.Describe}");

        var (tag, body, _) = BacnetTags.DecodeTag(ack.ValueWithTag);
        var value = BacnetTags.DecodeValue(tag, body, point.DataType);

        return new RawSample(point.Id, value, Quality.Good, null);
    }

    public async Task<IReadOnlyList<WriteResult>> WriteAsync(
        IReadOnlyList<WriteCommand> commands, CancellationToken ct)
    {
        var results = new List<WriteResult>(commands.Count);

        foreach (var cmd in commands)
        {
            var now = DateTimeOffset.UtcNow;

            try
            {
                await WritePointAsync(cmd, ct).ConfigureAwait(false);
                results.Add(new WriteResult(cmd.Point.Id, true, Quality.Good, now));
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw;
            }
            catch (DriverException ex)
            {
                results.Add(new WriteResult(cmd.Point.Id, false, QualityMap.From(ex.Kind), now, ex.Message));
            }
            catch (Exception ex)
            {
                results.Add(new WriteResult(cmd.Point.Id, false, Quality.Bad, now, ex.Message));
            }
        }

        return results;
    }

    private async Task WritePointAsync(WriteCommand cmd, CancellationToken ct)
    {
        var point = cmd.Point;
        var id = BacnetObjectId.From(point.Address);
        var property = BacnetObjectId.PropertyOf(point.Address);
        var label = $"{id.Describe()} {BacnetConstants.DescribeProperty(property)}";

        // 数组点位需要「属性标识符 + 数组索引」，本实现不带索引，写下去会写到整个数组上，
        // 设备要么报错要么只改第一个元素——两种结果都不是调用方想要的。
        if (point.IsArray)
            throw DriverException.Unsupported(Protocol, "encode",
                $"{label}: writing arrays is not supported by this build; write a scalar point instead");

        var valueWithTag = BacnetTags.EncodeValue(cmd.Value, point.DataType);

        var stream = await EnsureStreamAsync(ct).ConfigureAwait(false);

        var invokeId = NextInvokeId();
        var request = BacnetFrame.Build(
            BacnetApdu.BuildWriteProperty(id, property, valueWithTag, invokeId), broadcast: false);

        await stream.WriteAsync(request, ct).ConfigureAwait(false);

        var response = await AwaitResponseAsync(stream, invokeId, label, "write", ct).ConfigureAwait(false);
        var ack = BacnetApdu.ParseSimpleAck(response, invokeId, BacnetConstants.ServiceWriteProperty);

        if (!ack.Ok && ack.Kind != ErrorKind.Protocol)
            throw new DriverException(ack.Kind, Protocol, "write", $"{label}: {ack.Describe}");

        if (!ack.Ok)
            throw DriverException.ProtocolError(Protocol, "write", $"{label}: {ack.Describe}");
    }

    /// <summary>
    /// 等到一条属于 <paramref name="invokeId"/> 的响应。
    ///
    /// 迟到的响应必须丢弃重读，不能直接返回：UDP 上「上一条请求超时、它的响应晚到」
    /// 是常态，把它当成这次的响应就会把 A 点的值写到 B 点上，而质量仍是 good。
    /// 超时用一条请求的时延预算，不是「等到某条匹配为止」——否则一台不回包的设备
    /// 会让整轮采集挂在这里。
    /// </summary>
    private async Task<byte[]> AwaitResponseAsync(
        IByteStream stream, byte invokeId, string label, string op, CancellationToken ct)
    {
        var deadline = DateTimeOffset.UtcNow + Timeout;
        var seenStray = 0;

        while (DateTimeOffset.UtcNow < deadline)
        {
            var remaining = deadline - DateTimeOffset.UtcNow;
            var apdu = await ReceiveApduAsync(stream, remaining, ct).ConfigureAwait(false);
            if (apdu is null) continue;

            if (MatchesInvokeId(apdu, invokeId)) return apdu;

            seenStray++;
            _ctx.Log.LogDebug("bacnet: dropping stray response while waiting for invoke-id {InvokeId} ({Label})",
                invokeId, label);
        }

        throw DriverException.Timeout(Protocol, op,
            seenStray == 0
                ? $"{label}: no response within {Timeout.TotalMilliseconds:0}ms"
                : $"{label}: only {seenStray} response(s) with a different invoke-id arrived within " +
                  $"{Timeout.TotalMilliseconds:0}ms");
    }

    /// <summary>
    /// 判断一条响应是否属于本次请求。I-Am 与 Reject 没有 invoke-id 配对语义
    /// （Reject 有，但拒绝本身就说明请求没被接受，交给上层解析报错更清楚）。
    /// </summary>
    private static bool MatchesInvokeId(ReadOnlySpan<byte> apdu, byte invokeId)
    {
        if (apdu.Length < 2) return false;

        var pduType = (byte)(apdu[0] & 0xF0);
        return pduType is BacnetConstants.ApduComplexAck
            or BacnetConstants.ApduSimpleAck
            or BacnetConstants.ApduError
            ? apdu[1] == invokeId
            : pduType == BacnetConstants.ApduReject;
    }

    /// <summary>
    /// 收一条数据报并取出 APDU。
    ///
    /// UDP 一次读就是一条完整报文，所以缓冲区必须给到数据报上限；给小了内核会静默丢弃
    /// 多余字节，解析出来的是一条被截断的帧——长度校验会通过（BVLC 长度是报文自己写的），
    /// 但属性值已经少了一截。
    ///
    /// 读超时由调用方的时延预算（<paramref name="budget"/>）决定，不由流层：这样超时后
    /// 抛出的是驱动按错误类配好的诊断消息，而不是流层一条泛化的「no datagram」。
    /// </summary>
    private static async Task<byte[]?> ReceiveApduAsync(
        IByteStream stream, TimeSpan budget, CancellationToken ct)
    {
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        cts.CancelAfter(budget);

        var buffer = new byte[UdpByteStream.MaxDatagram];
        int received;
        try
        {
            received = await stream.ReadAsync(buffer, cts.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            // 预算内没等到数据报：交给上层的 deadline 循环按错误类/诊断消息收尾。
            return null;
        }

        if (received <= 0) return null;

        var frame = buffer.AsSpan(0, received);

        // 广播域上会收到别的协议流量（BVLL 类型不是 0x81）。这不是错误，跳过即可。
        if (frame.Length < BacnetConstants.BvllHeaderLength || frame[0] != BacnetConstants.BvllTypeBacnetIp)
            return null;

        var payload = BacnetFrame.Payload(frame);
        return payload.IsEmpty ? null : payload.ToArray();
    }

    // ------------------------------------------------------------------
    // 链路
    // ------------------------------------------------------------------

    private TimeSpan Timeout => _ctx.Channel.Timeout;

    /// <summary>
    /// 取一条可用链路。UDP 没有会话状态，这里只处理「流被关了要重开」，
    /// 不做握手——探活就是发 Who-Is，读写直接发请求。
    /// </summary>
    private async Task<IByteStream> EnsureStreamAsync(CancellationToken ct)
    {
        if (_stream is { IsOpen: true }) return _stream;

        await CloseStreamAsync().ConfigureAwait(false);

        _stream = await ByteStreams.OpenAsync(_ctx.Channel, ct).ConfigureAwait(false);
        return _stream;
    }

    /// <summary>
    /// 取下一个 invoke-id。BACnet 的 invoke-id 是 0..255 的序号，只要求「在途请求内唯一」。
    /// 这里每请求递增；<c>0</c> 也合法，不跳过——跳过会让抓包时更难对齐。
    /// </summary>
    private byte NextInvokeId() => _invokeId++;

    private async Task CloseStreamAsync()
    {
        var stream = Interlocked.Exchange(ref _stream, null);
        if (stream is null) return;

        try { await stream.DisposeAsync().ConfigureAwait(false); }
        catch { /* 关闭期异常忽略 */ }
    }

    public async ValueTask DisposeAsync()
    {
        if (_disposed) return;
        _disposed = true;
        await CloseStreamAsync().ConfigureAwait(false);
    }

    private static int ParseInt(Device device, string key, int fallback, int min, int max)
    {
        var raw = device.Label(key);
        if (raw is null) return fallback;

        if (!int.TryParse(raw, out var value) || value < min || value > max)
            throw DriverException.Unsupported(Protocol, "open",
                $"labels.{key} must be {min}..{max}, got '{raw}'");

        return value;
    }
}
