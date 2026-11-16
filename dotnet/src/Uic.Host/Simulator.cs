using System.Buffers.Binary;
using System.Diagnostics;
using System.Net;
using Microsoft.Extensions.Logging;
using Uic.Codec.Bacnet;
using Uic.Domain;
using Uic.Sim.Bacnet;
using Uic.Sim.Modbus;
using Uic.Sim.S7;

namespace Uic.Host;

/// <summary>
/// 内置模拟现场：Modbus TCP 从站 + S7 从站 + BACnet/IP 从站 + 数据泵。
///
/// 它的价值在于「一条命令就能看到完整链路」——主站连的是真实网络、走的是真实
/// 协议帧、经真实编解码，只是对端换成了同进程的内存模型。比任何 mock 都更接近现场。
///
/// 三种从站同时起：Modbus、S7 与 BACnet 的地址模型互不相同（寄存器序号 / 字节偏移 /
/// 对象+属性），合成一个模型只会让各边的点位互相迁就。分开三套内存更诚实。
/// </summary>
public sealed class Simulator : IAsyncDisposable
{
    private readonly ModbusSlaveModel _model;
    private readonly S7SlaveModel _s7Model;
    private readonly BacnetSlaveModel _bacnetModel;
    private readonly object _gate = new();
    private readonly ModbusTcpSlaveServer _server;
    private readonly S7TcpSlaveServer _s7Server;
    private readonly BacnetUdpSlaveServer _bacnetServer;
    private readonly CancellationTokenSource _cts = new();
    private readonly ILogger _log;
    private readonly TimeSpan _period;
    private Task? _pump;
    private long _counter;
    private int _disposed;

    public Simulator(int port, int s7Port, int bacnetPort, ILogger log, TimeSpan? period = null)
    {
        _log = log;
        _period = period ?? TimeSpan.FromSeconds(1);
        _model = new ModbusSlaveModel();
        _s7Model = new S7SlaveModel();
        _bacnetModel = new BacnetSlaveModel();

        Seed();
        SeedS7();
        SeedBacnet();

        _server = new ModbusTcpSlaveServer(IPAddress.Loopback, port, _model, _gate);
        _s7Server = new S7TcpSlaveServer(IPAddress.Loopback, s7Port, _s7Model);
        _bacnetServer = BacnetUdpSlaveServer.Start(_bacnetModel, bacnetPort);
        Port = port;
        S7Port = s7Port;
        BacnetPort = _bacnetServer.Port;
    }

    public int Port { get; }
    public int S7Port { get; }
    public int BacnetPort { get; }
    public ModbusSlaveModel Model => _model;
    public S7SlaveModel S7Model => _s7Model;
    public BacnetSlaveModel BacnetModel => _bacnetModel;

    public void Start()
    {
        _server.Start();
        _s7Server.Start();
        _pump = Task.Run(() => PumpAsync(_cts.Token));
        _log.LogInformation("modbus slave listening on 127.0.0.1:{Port}", Port);
        _log.LogInformation("s7 slave listening on 127.0.0.1:{Port}", S7Port);
        _log.LogInformation("bacnet/ip slave listening on 127.0.0.1:{Port}/udp", BacnetPort);
    }

    private void Seed()
    {
        // 与 DemoConfig 的点位布局一一对应：温度/压力是 float64，计数是 uint32，报警是线圈。
        lock (_gate)
        {
            WriteDouble(0, 55.5);
            WriteDouble(4, 101.25);
            WriteFloat(10, 123.4f);
            _model.Holding[8] = 0;
            _model.Holding[9] = 0;
            _model.SeedCoils(3, false);
        }
    }

    private void SeedS7()
    {
        // S7 的点位用字节偏移：DB1 里的温度 0..7、压力 8..15、计数 16..19，
        // 再加一个标志位 M0.0。与 DemoConfig 的 s7-1 设备对应。
        var db = _s7Model.DataBlock(1);
        WriteS7Double(db, 0, 55.5);
        WriteS7Double(db, 8, 101.25);
        BinaryPrimitives.WriteUInt32BigEndian(db.AsSpan(16, 4), 0);
        _s7Model.SeedFlagBit(0, 0, false);
    }

    /// <summary>
    /// BACnet 的对象模型：analog-input:1 的 present-value 是温度、analog-input:2 是压力，
    /// analog-input:3 的 present-value 是计数，binary-input:1 的 present-value 是运行状态。
    /// 与 DemoConfig 的 bacnet-1 设备一一对应。
    ///
    /// 属性号用 `BacnetConstants` 里的常量：present-value=85。对象标识符是
    /// 「10 位类型 + 22 位实例」，所以这里按类型 0（analog-input）/ 3（binary-input）种。
    /// </summary>
    private void SeedBacnet()
    {
        const int analogInput = 0;
        const int binaryInput = 3;

        _bacnetModel.Set(analogInput, 1, BacnetConstants.PropertyPresentValue,
            DataValue.Number(DataType.Float32, 55.5), DataType.Float32);
        _bacnetModel.Set(analogInput, 2, BacnetConstants.PropertyPresentValue,
            DataValue.Number(DataType.Float32, 101.25), DataType.Float32);
        _bacnetModel.Set(analogInput, 3, BacnetConstants.PropertyPresentValue,
            DataValue.Number(DataType.UInt32, 0), DataType.UInt32);
        _bacnetModel.Set(binaryInput, 1, BacnetConstants.PropertyPresentValue,
            DataValue.Boolean(false), DataType.Bool);

        // object-name 也种一份：点名是「给人看的属性」，配错属性号时读它比读 present-value 好排查。
        _bacnetModel.Set(analogInput, 1, BacnetConstants.PropertyObjectName,
            DataValue.String("AHU-1 Supply Temperature"), DataType.String);
    }

    private async Task PumpAsync(CancellationToken ct)
    {
        var started = Stopwatch.GetTimestamp();
        var period = _period;
        var db = _s7Model.DataBlock(1);

        while (!ct.IsCancellationRequested)
        {
            var t = Stopwatch.GetElapsedTime(started).TotalSeconds;

            lock (_gate)
            {
                // 30 秒一个完整正弦周期，幅值 30，基值 55.5。
                WriteDouble(0, 55.5 + 30 * Math.Sin(2 * Math.PI * t / 30));
                WriteDouble(4, 101.25 + 10 * Math.Sin(2 * Math.PI * t / 45));
                WriteFloat(10, (float)(100 + 40 * Math.Sin(2 * Math.PI * t / 60)));
                _counter = (long)(t * 10);
                _model.Holding[8] = (ushort)(_counter >> 16);
                _model.Holding[9] = (ushort)(_counter & 0xFFFF);
                _model.SeedCoils(3, _counter % 20 < 3);

                // S7 侧同一份脉搏：现场联调时两条链路的曲线应当长得一模一样，
                // 否则看曲线分不清「协议读错了」还是「模拟源不同」。
                WriteS7Double(db, 0, 55.5 + 30 * Math.Sin(2 * Math.PI * t / 30));
                WriteS7Double(db, 8, 101.25 + 10 * Math.Sin(2 * Math.PI * t / 45));
                BinaryPrimitives.WriteUInt32BigEndian(db.AsSpan(16, 4), (uint)_counter);
                _s7Model.SeedFlagBit(0, 0, _counter % 20 < 3);

                // BACnet 侧同样跟这条脉搏：三个协议的曲线一模一样，
                // 「哪条链路读错了」才一眼看得出。
                PumpBacnet(t);
            }

            try { await Task.Delay(period, ct).ConfigureAwait(false); }
            catch (OperationCanceledException) { return; }
        }
    }

    private void PumpBacnet(double t)
    {
        const int analogInput = 0;
        const int binaryInput = 3;

        _bacnetModel.Set(analogInput, 1, BacnetConstants.PropertyPresentValue,
            DataValue.Number(DataType.Float32, (float)(55.5 + 30 * Math.Sin(2 * Math.PI * t / 30))),
            DataType.Float32);
        _bacnetModel.Set(analogInput, 2, BacnetConstants.PropertyPresentValue,
            DataValue.Number(DataType.Float32, (float)(101.25 + 10 * Math.Sin(2 * Math.PI * t / 45))),
            DataType.Float32);
        _bacnetModel.Set(analogInput, 3, BacnetConstants.PropertyPresentValue,
            DataValue.Number(DataType.UInt32, _counter), DataType.UInt32);
        _bacnetModel.Set(binaryInput, 1, BacnetConstants.PropertyPresentValue,
            DataValue.Boolean(_counter % 20 < 3), DataType.Bool);
    }

    /// <summary>按 Modbus 的寄存器顺序（大端）写入一个 double，占 4 个寄存器。</summary>
    private void WriteDouble(int offset, double value)
    {
        Span<byte> buffer = stackalloc byte[8];
        BinaryPrimitives.WriteDoubleBigEndian(buffer, value);

        for (var i = 0; i < 4; i++)
        {
            _model.Holding[offset + i] = (ushort)((buffer[i * 2] << 8) | buffer[i * 2 + 1]);
        }
    }

    /// <summary>按 Modbus 的寄存器顺序（大端）写入一个 float，占 2 个寄存器。</summary>
    private void WriteFloat(int offset, float value)
    {
        Span<byte> buffer = stackalloc byte[4];
        BinaryPrimitives.WriteSingleBigEndian(buffer, value);

        for (var i = 0; i < 2; i++)
        {
            _model.Holding[offset + i] = (ushort)((buffer[i * 2] << 8) | buffer[i * 2 + 1]);
        }
    }

    /// <summary>S7 直接按字节写：DB 本来就是字节数组，不需要寄存器换算。</summary>
    private static void WriteS7Double(byte[] block, int offset, double value)
        => BinaryPrimitives.WriteDoubleBigEndian(block.AsSpan(offset, 8), value);

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        await _cts.CancelAsync().ConfigureAwait(false);

        if (_pump is not null)
        {
            try { await _pump.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        await _server.DisposeAsync().ConfigureAwait(false);
        await _s7Server.DisposeAsync().ConfigureAwait(false);
        await _bacnetServer.DisposeAsync().ConfigureAwait(false);
        _cts.Dispose();
    }
}
