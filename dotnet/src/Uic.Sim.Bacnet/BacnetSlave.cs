using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using Uic.Codec.Bacnet;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Sim.Bacnet;

/// <summary>
/// BACnet 从站内存模型：对象标识（类型 + 实例）→ 属性 → 值。
///
/// 用「属性 → 已编码的应用标签值」而不是强类型的对象类：本轮只需要
/// ReadProperty/WriteProperty 两件事，值在从站内部保持「线上形态」最省事，
/// 也让「从站回了一个什么字节」在用例里能逐字节断言。
/// </summary>
public sealed class BacnetSlaveModel
{
    private readonly ConcurrentDictionary<(int Type, int Instance, int Property), byte[]> _values = new();

    /// <summary>设备实例号（I-Am 里声明的 device 对象实例）。</summary>
    public int DeviceInstance { get; set; } = 1001;

    /// <summary>已处理的请求数。用例用它断言「确实走到从站了」。</summary>
    public int RequestCount;

    public void Set(int objectType, int instance, int property, DataValue value, DataType dataType)
        => _values[(objectType, instance, property)] = BacnetTags.EncodeValue(value, dataType);

    /// <summary>直接放一个已编码的值（用例里断言字节级行为时用）。</summary>
    public void SetRaw(int objectType, int instance, int property, ReadOnlySpan<byte> valueWithTag)
        => _values[(objectType, instance, property)] = valueWithTag.ToArray();

    public bool TryGet(int objectType, int instance, int property, out byte[] valueWithTag)
        => _values.TryGetValue((objectType, instance, property), out valueWithTag!);

    public void Remove(int objectType, int instance, int property)
        => _values.TryRemove((objectType, instance, property), out _);

    /// <summary>对象是否存在（任取一个属性）。用于区分「对象不存在」与「属性不存在」。</summary>
    public bool ObjectExists(int objectType, int instance)
        => _values.Keys.Any(k => k.Type == objectType && k.Instance == instance);
}

/// <summary>
/// 从站报文处理：纯函数，输入一条完整报文，输出要回写到对端的报文（或 null = 不回）。
///
/// 纯函数是为了让「一条请求 → 一条响应」的语义能在用例里直接断言，
/// 不必先起一个套接字。套接字相关的部分见 <see cref="BacnetUdpSlaveServer"/>。
/// </summary>
public static class BacnetSlaveHandler
{
    /// <summary>
    /// 处理一条报文。
    ///
    /// <paramref name="reply"/> 为 null 表示「正常但不需要回」——例如收到一条
    /// 不是给本机设备的确认请求，静默丢弃比回错误更接近真实设备的做法。
    /// </summary>
    public static byte[]? Handle(BacnetSlaveModel model, ReadOnlySpan<byte> frame, out string? trace)
    {
        trace = null;

        var apdu = BacnetFrame.Payload(frame);
        if (apdu.IsEmpty) return null;

        Interlocked.Increment(ref model.RequestCount);

        var pduType = (byte)(apdu[0] & 0xF0);

        // 未确认请求：Who-Is 是唯一需要响应的。
        if (pduType == BacnetConstants.ApduUnconfirmedRequest)
        {
            if (apdu.Length < 2)
            {
                trace = "unconfirmed request shorter than 2 bytes";
                return null;
            }

            if (apdu[1] != BacnetConstants.ServiceWhoIs)
            {
                trace = $"unconfirmed service 0x{apdu[1]:x2} ignored";
                return null;
            }

            trace = "who-is -> i-am";
            return BacnetFrame.Build(BacnetApdu.BuildIAm(model.DeviceInstance), broadcast: false);
        }

        if (pduType != BacnetConstants.ApduConfirmedRequest) return null;

        if (apdu.Length < 4)
        {
            trace = "confirmed request shorter than its header";
            return null;
        }

        var invokeId = apdu[2];
        var service = apdu[3];

        // 确认请求的响应（成功/错误/拒绝）一律是裸 APDU，在这里统一封帧。
        // 分开放在各自分支会出现「成功封了帧、错误忘了封帧」的不一致——
        // 驱动会把裸 APDU 当成非 BACnet 流量丢掉，现象是「对象写错却超时」而不是 address_error。
        var response = service switch
        {
            BacnetConstants.ServiceReadProperty => HandleReadProperty(model, apdu, invokeId, ref trace),
            BacnetConstants.ServiceWriteProperty => HandleWriteProperty(model, apdu, invokeId, ref trace),
            _ => Reject(invokeId, service, reason: 0, ref trace),
        };

        return BacnetFrame.Build(response, broadcast: false);
    }

    private static byte[] HandleReadProperty(
        BacnetSlaveModel model, ReadOnlySpan<byte> apdu, byte invokeId, ref string? trace)
    {
        var i = 4;

        if (!TryReadObjectId(apdu, ref i, out var objectId))
        {
            trace = "read-property has no object identifier";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceReadProperty,
                errorClass: 2, errorCode: 9); // property/invalid-data-type
        }

        if (!TryReadPropertyId(apdu, ref i, out var property))
        {
            trace = "read-property has no property identifier";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceReadProperty,
                errorClass: 2, errorCode: 9);
        }

        var label = $"{objectId.Describe()} {BacnetConstants.DescribeProperty(property)}";

        if (!model.ObjectExists(objectId.Type, objectId.Instance))
        {
            trace = $"read {label}: unknown-object";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceReadProperty,
                errorClass: 1, errorCode: 31); // object/unknown-object
        }

        if (!model.TryGet(objectId.Type, objectId.Instance, property, out var value))
        {
            trace = $"read {label}: unknown-property";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceReadProperty,
                errorClass: 2, errorCode: 32); // property/unknown-property
        }

        trace = $"read {label} -> {value.Length} byte(s)";

        return BacnetApdu.BuildReadPropertyAck(invokeId, objectId, property, value);
    }

    private static byte[] HandleWriteProperty(
        BacnetSlaveModel model, ReadOnlySpan<byte> apdu, byte invokeId, ref string? trace)
    {
        var i = 4;

        if (!TryReadObjectId(apdu, ref i, out var objectId))
        {
            trace = "write-property has no object identifier";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 2, 9);
        }

        if (!TryReadPropertyId(apdu, ref i, out var property))
        {
            trace = "write-property has no property identifier";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 2, 9);
        }

        var label = $"{objectId.Describe()} {BacnetConstants.DescribeProperty(property)}";

        if (!model.ObjectExists(objectId.Type, objectId.Instance))
        {
            trace = $"write {label}: unknown-object";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 1, 31);
        }

        // [3] 属性值：打开标签 + 应用标签值 + 关闭标签。
        if (i >= apdu.Length || apdu[i] != BacnetConstants.ContextTagOpening(3))
        {
            trace = "write-property has no open tag for the property value";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 2, 9);
        }
        i++;

        // 值是不透明的应用标签字节：按标签头给出的确切长度跳过，不逐字节找关闭标签。
        // 否则写进像 real 1.25（44 3F A0 00 00）这种载荷里带 0x3F 的值会被从中间截断。
        var start = i;
        var (_, valueLen) = TaggedSpanLength(apdu, i, out var lengthError);
        if (lengthError is not null)
        {
            trace = $"write-property {lengthError}";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 2, 9);
        }
        i += valueLen;

        if (i >= apdu.Length || !BacnetConstants.IsContextClosing(apdu[i]))
        {
            trace = "write-property value is not closed";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 2, 9);
        }

        if (valueLen == 0)
        {
            trace = "write-property carries an empty value";
            return BacnetApdu.BuildError(invokeId, BacnetConstants.ServiceWriteProperty, 2, 9);
        }

        model.SetRaw(objectId.Type, objectId.Instance, property, apdu.Slice(start, valueLen));
        trace = $"write {label} <- {valueLen} byte(s)";

        return BacnetApdu.BuildSimpleAck(invokeId, BacnetConstants.ServiceWriteProperty);
    }

    private static byte[] Reject(byte invokeId, byte service, byte reason, ref string? trace)
    {
        trace = $"service 0x{service:x2} rejected";
        return new[] { BacnetConstants.ApduReject, invokeId, service, reason };
    }

    /// <summary>[0] 对象标识符：上下文标签 0、定长 4 字节。</summary>
    private static bool TryReadObjectId(ReadOnlySpan<byte> apdu, ref int i, out BacnetObjectId id)
    {
        id = default;
        if (i + 5 > apdu.Length) return false;
        if (apdu[i] != BacnetConstants.ContextTagHeader(0, 4)) return false;

        id = BacnetObjectId.FromWire(
            System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(apdu.Slice(i + 1, 4)));
        i += 5;
        return true;
    }

    /// <summary>[1] 属性标识符：上下文标签 1、长度 1..4。</summary>
    private static bool TryReadPropertyId(ReadOnlySpan<byte> apdu, ref int i, out int property)
    {
        property = 0;
        if (i >= apdu.Length) return false;
        if ((apdu[i] & BacnetConstants.ContextTagClassBit) == 0) return false;
        if ((apdu[i] >> 4) != 1) return false;

        var length = apdu[i] & 0x07;
        if (length is 0 or > 4) return false;
        if (i + 1 + length > apdu.Length) return false;

        var value = 0;
        for (var n = 0; n < length; n++) value = (value << 8) | apdu[i + 1 + n];

        property = value;
        i += 1 + length;
        return true;
    }

    /// <summary>
    /// 量出「应用标签 + 载荷」占用的总字节数，不解读值本身。
    ///
    /// 写请求里的属性值是一段不透明的应用标签字节：标签头给出长度，据此跳过恰好
    /// 这么多字节就是关闭标签的位置。绝不能在值里逐字节找关闭标签——载荷可能撞上
    /// 任意字节序列（例如 real 1.25 = 44 3F A0 00 00 里的 0x3F）。
    /// </summary>
    private static (int Tag, int Length) TaggedSpanLength(
        ReadOnlySpan<byte> apdu, int offset, out string? error)
    {
        error = null;
        if (offset >= apdu.Length)
        {
            error = "value is empty";
            return (0, 0);
        }

        var header = apdu[offset];
        if ((header & BacnetConstants.ContextTagClassBit) != 0)
        {
            error = "value is a context tag, not an application value";
            return (0, 0);
        }

        var tag = header >> 4;
        var lvt = header & 0x07;
        var cursor = offset + 1;

        // boolean 与 null 的值就在标签头里：没有载荷字节，不能按「1 + lvt」算。
        if (tag is BacnetConstants.TagNull or BacnetConstants.TagBoolean)
            return (tag, 1);

        if (lvt < 5)
            return (tag, 1 + lvt); // 头 1 字节 + 短载荷

        if (cursor >= apdu.Length)
        {
            error = "value declares an extended length but it is missing";
            return (0, 0);
        }

        var first = apdu[cursor++];
        int length;
        int consumed;

        if (first <= 253)
        {
            length = first;
            consumed = 0;
        }
        else if (first == 254)
        {
            if (cursor + 2 > apdu.Length)
            {
                error = "value declares a 2-byte extended length but it is truncated";
                return (0, 0);
            }
            length = System.Buffers.Binary.BinaryPrimitives.ReadUInt16BigEndian(apdu.Slice(cursor, 2));
            consumed = 2;
        }
        else
        {
            if (cursor + 4 > apdu.Length)
            {
                error = "value declares a 4-byte extended length but it is truncated";
                return (0, 0);
            }
            length = (int)System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(apdu.Slice(cursor, 4));
            consumed = 4;
        }

        cursor += consumed;

        if (cursor + length > apdu.Length)
        {
            error = $"value declares {length} payload byte(s) but only {apdu.Length - cursor} remain";
            return (0, 0);
        }

        return (tag, cursor - offset + length);
    }
}

/// <summary>
/// 跑在真实 UDP 端口上的从站，供驱动回环测试与手工联调使用。
///
/// 端口取 0 表示由系统分配：并发跑用例时固定端口会互相抢，而「端口被占」
/// 的表现是连接超时——最容易被误判成「驱动有问题」。
/// </summary>
public sealed class BacnetUdpSlaveServer : IAsyncDisposable
{
    private readonly UdpClient _socket;
    private readonly BacnetSlaveModel _model;
    private readonly CancellationTokenSource _cts = new();
    private readonly Task _loop;
    private int _closed;

    private BacnetUdpSlaveServer(UdpClient socket, BacnetSlaveModel model)
    {
        _socket = socket;
        _model = model;
        _loop = Task.Run(() => ServeAsync(_cts.Token));
    }

    /// <summary>本机监听的端口。配置驱动时把它填进 channel.port。</summary>
    public int Port { get; private set; }

    public BacnetSlaveModel Model => _model;

    /// <summary>最近一次处理的请求摘要。联调时比抓包快。</summary>
    public string? LastTrace { get; private set; }

    public static BacnetUdpSlaveServer Start(BacnetSlaveModel model, int port = 0)
    {
        var socket = new UdpClient(new IPEndPoint(IPAddress.Loopback, port));
        // 收到超过 512 字节的报文时需要显式放宽，否则会被截断。
        socket.Client.ReceiveBufferSize = 1 << 16;

        return new BacnetUdpSlaveServer(socket, model)
        {
            Port = ((IPEndPoint)socket.Client.LocalEndPoint!).Port,
        };
    }

    private async Task ServeAsync(CancellationToken ct)
    {
        while (!ct.IsCancellationRequested)
        {
            UdpReceiveResult received;
            try
            {
                received = await _socket.ReceiveAsync(ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                return;
            }
            catch (ObjectDisposedException)
            {
                return;
            }
            catch (SocketException)
            {
                // 从站不因单条坏包退出：一次 ICMP 差错会让下一次 Receive 抛错。
                continue;
            }

            try
            {
                var reply = BacnetSlaveHandler.Handle(_model, received.Buffer, out var trace);
                LastTrace = trace;

                if (reply is not null)
                    await _socket.SendAsync(reply, received.RemoteEndPoint, ct).ConfigureAwait(false);
            }
            catch (Exception)
            {
                // 从站是测试夹具：坏包丢弃，不改变协议状态。
            }
        }
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _closed, 1) != 0) return;

        await _cts.CancelAsync().ConfigureAwait(false);
        _socket.Dispose();

        try { await _loop.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        _cts.Dispose();
    }
}
