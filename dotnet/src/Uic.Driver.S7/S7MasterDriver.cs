using Microsoft.Extensions.Logging;
using Uic.Codec.S7;
using Uic.Domain;
using Uic.Driver;
using Uic.Transport;

namespace Uic.Driver.S7;

/// <summary>
/// S7comm 主站（S7-300/400/1200/1500）。
///
/// 与 Modbus 驱动的结构刻意保持一致：一层分帧（TPKT + COTP）、一层 PDU 编解码、
/// 驱动只做「点位 → 请求项 → 请求项 → 点位」的翻译。差别在于 S7 的会话是有状态的：
/// 必须先完成 COTP 连接与 PDU 协商，之后的每个 Job 才能带 PDU 引用号。
/// 因此连接不是「用的时候现开一条流」，而是与 <see cref="IConn"/> 同生命周期。
///
/// 明确不支持（配置校验或运行期明确报错，不静默降级）：
/// - 符号寻址（S7-1200/1500 的 0xB2 寻址）与 S7Plus（加密）；
/// - 分片 TPDU（PLC 把一个大 PDU 拆成多个 DT 包）；
/// - 多连接并发（一台 PLC 一条会话，串行由调度器保证）。
/// </summary>
public sealed class S7MasterDriver : IDriver
{
    // PDU 引用号在进程内单调，跨设备不重复：同一条链路上抓包时能分清是谁的请求。
    private static int _referenceCounter;

    public DriverMeta Meta { get; } = new()
    {
        Id = "s7",
        Name = "S7comm 主站",
        Version = "1.0.0",
        Roles = new[] { DriverRole.Master },
        Transports = new[] { "tcp" },
        AddressKinds = new[] { AddressKind.Register },
        Capabilities = new Capabilities(
            Read: CapState.Supported,
            Write: CapState.Supported,
            MultiRead: CapState.Supported,
            BitAccess: CapState.Supported),
        Description = "S7comm 主站：DB/IB/QB/MB 区、位访问、PDU 自动协商（240/480/960）。",
        ConfigFields = new[]
        {
            new ConfigField("transport.host", "主机", "string", Required: true,
                Placeholder: "192.168.1.10", Scope: "channel"),
            new ConfigField("transport.port", "端口", "int", Required: true, Default: 102, Scope: "channel"),
            new ConfigField("labels.rack", "机架号", "int", Default: 0, Scope: "device",
                Help: "S7-1200/1500 通常是 0"),
            new ConfigField("labels.slot", "槽位号", "int", Default: 1, Scope: "device",
                Help: "S7-1200/1500 通常是 1；S7-300 常见 2"),
            new ConfigField("labels.pduSize", "请求的 PDU 长度", "int", Default: 480, Scope: "device",
                Help: "以 PLC 返回的值为准；只有联调抓包时才需要改"),
        },
        PointFields = new[]
        {
            new ConfigField("address.area", "区域", "string", Required: true, Scope: "point",
                Placeholder: "db1 / i / q / m",
                Help: "db<N> 表示数据块，i/q/m 表示输入/输出/标志区；也接受 e/a/merker 等别名"),
            new ConfigField("address.offset", "字节偏移", "int", Required: true, Scope: "point",
                Help: "S7 的地址是字节偏移（不是寄存器序号）；位访问另填 address.bit"),
        },
    };

    public IConn Open(OpenContext context) => new S7Conn(context);

    internal static ushort NextReference()
        => (ushort)(Interlocked.Increment(ref _referenceCounter) & 0xFFFF);
}

/// <summary>一条到 PLC 的 S7 会话。</summary>
internal sealed class S7Conn : IConn
{
    private const string Protocol = "s7";

    private readonly OpenContext _ctx;
    private readonly int _rack;
    private readonly int _slot;
    private readonly int _requestedPdu;

    private IByteStream? _stream;
    private int _pduSize = S7Constants.PduSmall;
    private bool _disposed;

    public S7Conn(OpenContext ctx)
    {
        _ctx = ctx;
        _rack = ParseInt(ctx.Device, "rack", 0, 0, 7);
        _slot = ParseInt(ctx.Device, "slot", 1, 0, 31);
        _requestedPdu = ParseInt(ctx.Device, "pduSize", S7Constants.PduMedium, 240, 960);
    }

    /// <summary>协商出来的 PDU 长度。诊断与日志用；未连接时为请求值。</summary>
    public int PduSize => _pduSize;

    public async Task ProbeAsync(CancellationToken ct)
    {
        // 探活必须建立完整会话（COTP + PDU 协商），否则「TCP 通」会被当成可用。
        // 协商之后读一个字节，确认读服务也可用，而不只是握手成功。
        await EnsureSessionAsync(ct).ConfigureAwait(false);

        var probe = new S7ReadItem(new S7Address(S7Constants.AreaFlag, 0, 0, -1), 1, false);
        var response = await RoundTripAsync(S7Read.BuildRequest(S7MasterDriver.NextReference(),
            new[] { probe }), ct).ConfigureAwait(false);

        var parsed = S7Read.ParseResponse(S7Frame.Parse(response), 1);
        if (parsed.Count != 1 || !parsed[0].Ok)
            throw DriverException.ProtocolError(Protocol, "probe",
                $"probe read of MB0 returned '{S7Constants.DescribeReturn(parsed[0].ReturnCode)}'");
    }

    public async Task<IReadOnlyList<RawSample>> ReadAsync(IReadOnlyList<ReadSpan> spans, CancellationToken ct)
    {
        var all = new List<RawSample>();

        foreach (var span in spans)
        {
            try
            {
                all.AddRange(await ReadSpanAsync(span, ct).ConfigureAwait(false));
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw;
            }
            catch (DriverException ex)
            {
                // 一个窗口失败不拖垮整轮：与 Modbus 驱动同一语义。
                all.AddRange(span.Points.Select(p =>
                    new RawSample(p.Id, null, QualityMap.From(ex.Kind), null, ex.Message)));
            }
            catch (Exception ex)
            {
                all.AddRange(span.Points.Select(p =>
                    new RawSample(p.Id, null, Quality.Bad, null, ex.Message)));
            }
        }

        return all;
    }

    private async Task<IReadOnlyList<RawSample>> ReadSpanAsync(ReadSpan span, CancellationToken ct)
    {
        await EnsureSessionAsync(ct).ConfigureAwait(false);

        // 窗口内每个点位各占一个请求项：S7 的「一项」本来就能指定区域与任意字节偏移，
        // 不需要像 Modbus 那样把相邻地址合成一个窗口。合并阅读数反而更省字节，
        // 但会让「一个点位越界」连累整窗，按点位逐项请求的失败粒度更诚实。
        var items = span.Points.Select(ToReadItem).ToArray();

        var samples = new List<RawSample>(items.Length);
        var budget = PduBudget();

        // 一项就是 12 字节，一个点表很容易把 PDU 撑爆（240 字节约 19 项）。
        // 这里按协商值切片，而不是让整窗失败：现场一台 PLC 上百个点位是常态，
        // 「一次读太多」应该是驱动自己拆，不是让运维去删点。
        var pending = new List<(int Index, S7ReadItem Item)>(items.Length);

        for (var i = 0; i < items.Length; i++)
        {
            var item = items[i];

            // 单个点位本身就装不下：切不了，只能点名失败。
            if (ItemBytes(item) > budget)
            {
                samples.Add(new RawSample(span.Points[i].Id, null, Quality.Bad, null,
                    $"{item.Address.Describe()}: needs {ItemBytes(item)} bytes of PDU but the PLC negotiated " +
                    $"{_pduSize}; shorten the point length"));
                continue;
            }

            pending.Add((i, item));
        }

        foreach (var chunk in Batch(pending, budget))
        {
            var request = chunk.Select(c => c.Item).ToArray();

            var frame = S7Read.BuildRequest(S7MasterDriver.NextReference(), request);
            var response = await RoundTripAsync(frame, ct).ConfigureAwait(false);
            var results = S7Read.ParseResponse(S7Frame.Parse(response), request.Length);

            for (var i = 0; i < request.Length; i++)
            {
                var point = span.Points[chunk[i].Index];
                var item = chunk[i].Item;
                var (ok, returnCode, data) = results[i];

                if (!ok)
                {
                    var quality = S7Write.QualityFor(returnCode);
                    samples.Add(new RawSample(point.Id, null, quality, null,
                        $"{item.Address.Describe()}: {S7Constants.DescribeReturn(returnCode)}"));
                    continue;
                }

                try
                {
                    samples.Add(Decode(point, item, data));
                }
                catch (Exception ex)
                {
                    samples.Add(new RawSample(point.Id, null, Quality.Bad, null,
                        $"{item.Address.Describe()}: {ex.Message}"));
                }
            }
        }

        return samples;
    }

    /// <summary>请求头 10 字节 + 参数头 2 字节（功能码 + 项数）。</summary>
    private int PduBudget() => Math.Max(12, _pduSize - S7Constants.JobHeaderLength - 2);

    /// <summary>
    /// 一项在线上占多少字节（请求 + 响应）。
    ///
    /// 两边都要算：只按请求切片，读回一批长字符串时响应仍会超长，而 PLC 对超长响应
    /// 的处理同样可能是「直接不回」——现象是超时，不是错误。
    /// </summary>
    private static int ItemBytes(S7ReadItem item)
    {
        const int requestBytes = 12;
        var dataBytes = item.IsBit ? 1 : Math.Max(1, item.ByteCount);
        var responseBytes = 4 + dataBytes + (dataBytes % 2);
        return requestBytes + responseBytes;
    }

    /// <summary>按 PDU 预算把请求项切片，保持原有的点位顺序。</summary>
    private static IEnumerable<List<(int Index, S7ReadItem Item)>> Batch(
        List<(int Index, S7ReadItem Item)> items, int budget)
    {
        var chunk = new List<(int, S7ReadItem)>();
        var used = 0;

        foreach (var entry in items)
        {
            var need = ItemBytes(entry.Item);
            if (chunk.Count > 0 && used + need > budget)
            {
                yield return chunk;
                chunk = new List<(int, S7ReadItem)>();
                used = 0;
            }

            chunk.Add(entry);
            used += need;
        }

        if (chunk.Count > 0) yield return chunk;
    }

    /// <summary>
    /// 一个点位 → 一个请求项。
    ///
    /// 位访问单独成项（传输尺寸 BIT，个数 1）；字访问按「需要几个字节」请求。
    /// 数组点位在这里天然生效：<see cref="PointRef.ByteLength"/> 已经按元素个数乘过。
    /// </summary>
    private static S7ReadItem ToReadItem(PointRef point)
    {
        var address = S7Address.Parse(point.Address);

        if (point.DataType == DataType.Bool && address.IsBit)
            return new S7ReadItem(address, 1, true);

        if (point.DataType == DataType.Bool && point.Address.Bit == -1)
            throw DriverException.Address(Protocol, "encode",
                $"{address.Describe()}: bool points need address.bit (0..7), or use a byte/word type");

        // 位访问但类型不是 bool：S7 的一位读回来只能是布尔，没有别的解释。
        if (address.IsBit && point.DataType != DataType.Bool)
            throw DriverException.Address(Protocol, "encode",
                $"{address.Describe()}: address.bit requires dataType bool, got {point.DataType}");

        return new S7ReadItem(address, Math.Max(1, point.ByteLength), false);
    }

    private RawSample Decode(PointRef point, S7ReadItem item, byte[] data)
    {
        if (item.IsBit)
        {
            // 位读取的数据项固定 1 字节，bit0 有效；其余位是填充。
            var on = data.Length > 0 && (data[0] & 0x01) != 0;
            return new RawSample(point.Id, DataValue.Boolean(on), Quality.Good, null);
        }

        var expected = Math.Max(1, point.ByteLength);
        if (data.Length < expected)
            throw DriverException.ProtocolError(Protocol, "read",
                $"got {data.Length} byte(s), expected {expected}");

        var value = ValueCodec.Decode(data.AsSpan(0, expected), point.DataType, point.ByteOrder,
            point.IsArray ? point.Elements : 1);

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
                await EnsureSessionAsync(ct).ConfigureAwait(false);

                var item = ToWriteItem(cmd);
                var frame = S7Write.BuildRequest(S7MasterDriver.NextReference(), new[] { item });
                GuardPduSize(frame, cmd.Point.Id);

                var response = await RoundTripAsync(frame, ct).ConfigureAwait(false);
                var codes = S7Write.ParseResponse(S7Frame.Parse(response), 1);

                var quality = S7Write.QualityFor(codes[0]);
                var ok = quality == Quality.Good;

                results.Add(new WriteResult(cmd.Point.Id, ok, quality, now,
                    ok ? null : S7Constants.DescribeReturn(codes[0])));
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

    private static S7WriteItem ToWriteItem(WriteCommand cmd)
    {
        var point = cmd.Point;
        var address = S7Address.Parse(point.Address);

        if (point.DataType == DataType.Bool)
        {
            if (!address.IsBit)
                throw DriverException.Address(Protocol, "encode",
                    $"{address.Describe()}: writing a bool needs address.bit (0..7)");

            return new S7WriteItem(address, true, new[] { cmd.Value.AsBool() ? (byte)0x01 : (byte)0x00 });
        }

        if (address.IsBit)
            throw DriverException.Address(Protocol, "encode",
                $"{address.Describe()}: address.bit can only be written as bool");

        var bytes = ValueCodec.Encode(cmd.Value, point.DataType, point.ByteOrder,
            Math.Max(1, point.ByteLength / 2));
        return new S7WriteItem(address, false, bytes);
    }

    // ------------------------------------------------------------------
    // 会话
    // ------------------------------------------------------------------

    private async Task EnsureSessionAsync(CancellationToken ct)
    {
        if (_stream is { IsOpen: true }) return;

        await CloseStreamAsync().ConfigureAwait(false);

        var stream = await ByteStreams.OpenAsync(_ctx.Channel, ct).ConfigureAwait(false);
        _stream = stream;

        try
        {
            await HandshakeAsync(stream, ct).ConfigureAwait(false);
        }
        catch
        {
            // 握手失败必须把流关掉：留着一条「TCP 通但会话没建」的连接，
            // 下一轮会以为可以复用，错误就从「连接失败」退化成各种莫名其妙的协议错。
            await CloseStreamAsync().ConfigureAwait(false);
            throw;
        }
    }

    private async Task HandshakeAsync(IByteStream stream, CancellationToken ct)
    {
        var connect = Cotp.BuildConnectRequest(_rack, _slot);
        await stream.WriteAsync(connect, ct).ConfigureAwait(false);

        var confirmFrame = await ReadFrameAsync(stream, ct).ConfigureAwait(false);
        var cotp = Tpkt.Payload(confirmFrame);
        Cotp.ExpectConnectConfirm(cotp);

        var tpdu = Cotp.NegotiatedTpduSize(cotp);
        if (tpdu > 0 && tpdu < S7Constants.PduSmall)
            throw DriverException.Unsupported(Protocol, "connect",
                $"PLC only accepts {tpdu}-byte TPDUs, the S7 minimum is {S7Constants.PduSmall}");

        var setup = S7Setup.BuildRequest(S7MasterDriver.NextReference(), (ushort)_requestedPdu);
        await stream.WriteAsync(Cotp.BuildData(setup), ct).ConfigureAwait(false);

        var setupFrame = await ReadFrameAsync(stream, ct).ConfigureAwait(false);
        var setupPdu = S7Frame.Parse(Cotp.ExpectData(Tpkt.Payload(setupFrame)));
        _pduSize = S7Setup.ParseResponse(setupPdu);

        _ctx.Log.LogDebug("s7 session to {Remote}: rack {Rack} slot {Slot} negotiated PDU {Pdu}",
            stream.Remote, _rack, _slot, _pduSize);
    }

    private async Task<byte[]> RoundTripAsync(byte[] s7pdu, CancellationToken ct)
    {
        var stream = _stream ?? throw DriverException.Connection(Protocol, "write", "connection is not open");

        await stream.WriteAsync(Cotp.BuildData(s7pdu), ct).ConfigureAwait(false);
        var frame = await ReadFrameAsync(stream, ct).ConfigureAwait(false);
        return Cotp.ExpectData(Tpkt.Payload(frame)).ToArray();
    }

    /// <summary>读一条完整的 TPKT 帧：先读 4 字节头算长度，再读剩余部分。</summary>
    private static async Task<byte[]> ReadFrameAsync(IByteStream stream, CancellationToken ct)
    {
        var header = new byte[S7Constants.TpktHeaderLength];
        await stream.ReadExactlyAsync(header, ct).ConfigureAwait(false);

        var total = Tpkt.TotalLength(header);
        if (total is < 7 or > 8192)
            throw DriverException.ProtocolError(Protocol, "read", $"implausible TPKT length {total}");

        var frame = new byte[total];
        header.CopyTo(frame, 0);
        if (total > header.Length)
            await stream.ReadExactlyAsync(frame.AsMemory(header.Length), ct).ConfigureAwait(false);

        return frame;
    }

    /// <summary>
    /// 请求超过协商出的 PDU 长度时提前失败。
    ///
    /// 正常路径不会走到这里——读请求已经按 PDU 切过片。能超的只有「单个点位本身就装不下」
    /// （例如一次读 400 字节的字符串块）。不靠 PLC 回错误：有些型号对超长请求直接不回，
    /// 运维看到的会是「网络慢」，而不是「这个点位太大」。
    /// </summary>
    private void GuardPduSize(byte[] s7pdu, ReadSpan span)
    {
        if (s7pdu.Length <= _pduSize) return;

        throw DriverException.ProtocolError(Protocol, "encode",
            $"a single request item of span '{span.Id}' needs {s7pdu.Length} bytes but the PLC negotiated {_pduSize}; " +
            "split the point (shorter length or an array of smaller items)");
    }

    private void GuardPduSize(byte[] s7pdu, string pointId)
    {
        if (s7pdu.Length <= _pduSize) return;

        throw DriverException.ProtocolError(Protocol, "encode",
            $"write of '{pointId}' needs {s7pdu.Length} bytes but the PLC negotiated {_pduSize}");
    }

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
