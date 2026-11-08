using System.Globalization;
using Uic.Domain;
using Uic.Driver;

namespace Uic.Driver.Virtual;

public enum Waveform
{
    Sine,
    Ramp,
    Random,
    Const,
}

public enum InjectedFault
{
    None,
    Drop,
    Timeout,
    Bad,
    Address,
    Disconnect,
}

/// <summary>
/// 虚拟设备驱动：不依赖硬件与网络，用于无真机开发、CI 回归与故障注入。
/// 波形与故障由设备标签控制，同一个驱动可以模拟正弦/斜坡/随机/常量，
/// 以及丢包、超时、坏值、地址错、断连五类异常。
/// </summary>
public sealed class VirtualDriver : IDriver
{
    public DriverMeta Meta { get; } = new()
    {
        Id = "virtual",
        Name = "虚拟设备",
        Version = "1.0.0",
        Roles = new[] { DriverRole.Both },
        Transports = new[] { "none" },
        AddressKinds = new[] { AddressKind.Register, AddressKind.Symbol },
        Capabilities = new Capabilities(
            Read: CapState.Supported,
            Write: CapState.Supported,
            Subscribe: CapState.Supported,
            Browse: CapState.Supported,
            MultiRead: CapState.Supported,
            BitAccess: CapState.Supported),
        Description = "内置虚拟设备：正弦/斜坡/随机/常量波形 + 五类故障注入。",
        ConfigFields = new[]
        {
            new ConfigField("labels.mode", "波形", "enum", Scope: "device",
                Enum: new[] { "sine", "ramp", "random", "const" }, Default: "sine"),
            new ConfigField("labels.fault", "故障注入", "enum", Scope: "device",
                Enum: new[] { "none", "drop", "timeout", "bad", "address", "disconnect" }, Default: "none"),
            new ConfigField("labels.faultAt", "第几次请求注入", "int", Scope: "device", Default: 0),
            new ConfigField("labels.periodSeconds", "周期（秒）", "float", Scope: "device", Default: 60.0),
            new ConfigField("labels.amplitude", "幅值", "float", Scope: "device", Default: 100.0),
            new ConfigField("labels.baseline", "基值", "float", Scope: "device", Default: 50.0),
        },
    };

    public IConn Open(OpenContext context) => new VirtualConn(context);
}

internal sealed class VirtualConn : IConn
{
    private readonly Device _device;
    private readonly Waveform _mode;
    private readonly InjectedFault _fault;
    private readonly int _faultAt;
    private readonly double _period;
    private readonly double _amplitude;
    private readonly double _baseline;
    private readonly long _started = System.Diagnostics.Stopwatch.GetTimestamp();
    private readonly Dictionary<string, DataValue> _written = new(StringComparer.Ordinal);
    private readonly object _lock = new();
    private int _reads;
    private bool _disposed;

    public VirtualConn(OpenContext ctx)
    {
        _device = ctx.Device;
        _mode = ParseMode(ctx.Device);
        _fault = ParseFault(ctx.Device);
        _faultAt = ParseInt(ctx.Device, "faultAt", 0);
        _period = ParseDouble(ctx.Device, "periodSeconds", 60);
        if (_period <= 0) _period = 60;
        _amplitude = ParseDouble(ctx.Device, "amplitude", 100);
        _baseline = ParseDouble(ctx.Device, "baseline", 50);
    }

    public Task ProbeAsync(CancellationToken ct)
    {
        if (_fault == InjectedFault.Disconnect)
            throw DriverException.Connection("virtual", "probe", "injected disconnect");
        return Task.CompletedTask;
    }

    public Task<IReadOnlyList<RawSample>> ReadAsync(IReadOnlyList<ReadSpan> spans, CancellationToken ct)
    {
        if (_disposed) throw DriverException.Connection("virtual", "read", "connection is disposed");

        int n;
        lock (_lock) n = ++_reads;

        if (_faultAt > 0 && n == _faultAt)
        {
            switch (_fault)
            {
                case InjectedFault.Drop:
                case InjectedFault.Timeout:
                    throw DriverException.Timeout("virtual", "read", "injected timeout");
                case InjectedFault.Disconnect:
                    throw DriverException.Connection("virtual", "read", "injected disconnect");
                case InjectedFault.Address:
                    throw DriverException.Address("virtual", "read", "injected address error");
            }
        }

        if (_fault == InjectedFault.Disconnect)
            throw DriverException.Connection("virtual", "read", "injected disconnect");

        var elapsed = System.Diagnostics.Stopwatch.GetElapsedTime(_started).TotalSeconds;
        var now = DateTimeOffset.UtcNow;
        var result = new List<RawSample>();

        lock (_lock)
        {
            foreach (var span in spans)
            {
                foreach (var p in span.Points)
                {
                    if (_fault == InjectedFault.Bad)
                    {
                        result.Add(new RawSample(p.Id, null, Quality.Bad, null, "injected bad value"));
                        continue;
                    }

                    if (_mode == Waveform.Const && _written.TryGetValue(p.Id, out var written))
                    {
                        result.Add(new RawSample(p.Id, written, Quality.Good, now));
                        continue;
                    }

                    result.Add(new RawSample(p.Id, Sample(p, elapsed), Quality.Good, now));
                }
            }
        }

        return Task.FromResult<IReadOnlyList<RawSample>>(result);
    }

    public Task<IReadOnlyList<WriteResult>> WriteAsync(IReadOnlyList<WriteCommand> commands, CancellationToken ct)
    {
        if (_fault == InjectedFault.Disconnect)
            throw DriverException.Connection("virtual", "write", "injected disconnect");

        var results = new List<WriteResult>(commands.Count);
        lock (_lock)
        {
            foreach (var cmd in commands)
            {
                _written[cmd.Point.Id] = cmd.Value;
                results.Add(new WriteResult(cmd.Point.Id, true, Quality.Good, DateTimeOffset.UtcNow));
            }
        }
        return Task.FromResult<IReadOnlyList<WriteResult>>(results);
    }

    private DataValue Sample(PointRef p, double t)
    {
        if (!p.IsArray)
        {
            var single = Wave(p, t, 0);
            return p.DataType switch
            {
                DataType.Bool => DataValue.Boolean(single > _baseline),
                DataType.String => DataValue.String($"dev-{_device.Id}-{p.Id}"),
                // 时间型点位给当前时刻：让北向消费者能直接看到「最近一次采集时间」这类值。
                DataType.DateTime => DataValue.DateTime(DateTimeOffset.UtcNow),
                DataType.Float32 or DataType.Float64 => DataValue.Number(p.DataType, single),
                _ => DataValue.Number(p.DataType, Math.Round(single)),
            };
        }

        var values = new double[p.Elements];
        for (var i = 0; i < values.Length; i++)
        {
            values[i] = p.DataType is DataType.Float32 or DataType.Float64
                ? Wave(p, t, i)
                : Math.Round(Wave(p, t, i));
        }

        return DataValue.Numbers(p.DataType, values);
    }

    /// <summary>
    /// 第 <paramref name="index"/> 个元素的波形值。数组的每个元素多走 1/10 个周期的相位差：
    /// 全部元素完全同步的话，看不出「数组被展开渲染」还是「只取了第一个元素」。
    /// </summary>
    private double Wave(PointRef p, double t, int index)
    {
        var phase = p.Address.Offset * 0.1 + index * 0.1;

        return _mode switch
        {
            Waveform.Sine => _baseline + _amplitude * Math.Sin(2 * Math.PI * t / _period + phase),
            Waveform.Ramp => _baseline + _amplitude * (((t / _period) + phase) % 1.0),
            Waveform.Random => _baseline + _amplitude * Random.Shared.NextDouble(),
            _ => _baseline,
        };
    }

    public ValueTask DisposeAsync()
    {
        _disposed = true;
        return ValueTask.CompletedTask;
    }

    // 设备参数以 label 形式承载（与驱动 SDK 的 configFields.scope=device 对应）。
    private static Waveform ParseMode(Device d) => (d.Label("mode") ?? "sine").ToLowerInvariant() switch
    {
        "ramp" => Waveform.Ramp,
        "random" => Waveform.Random,
        "const" => Waveform.Const,
        _ => Waveform.Sine,
    };

    private static InjectedFault ParseFault(Device d) => (d.Label("fault") ?? "none").ToLowerInvariant() switch
    {
        "drop" => InjectedFault.Drop,
        "timeout" => InjectedFault.Timeout,
        "bad" => InjectedFault.Bad,
        "address" => InjectedFault.Address,
        "disconnect" => InjectedFault.Disconnect,
        _ => InjectedFault.None,
    };

    private static int ParseInt(Device d, string key, int fallback)
        => int.TryParse(d.Label(key), NumberStyles.Integer, CultureInfo.InvariantCulture, out var v) ? v : fallback;

    private static double ParseDouble(Device d, string key, double fallback)
        => double.TryParse(d.Label(key), NumberStyles.Float, CultureInfo.InvariantCulture, out var v) ? v : fallback;
}