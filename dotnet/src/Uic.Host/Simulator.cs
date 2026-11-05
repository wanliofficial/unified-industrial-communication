using System.Buffers.Binary;
using System.Diagnostics;
using System.Net;
using Microsoft.Extensions.Logging;
using Uic.Sim.Modbus;

namespace Uic.Host;

/// <summary>
/// 内置模拟现场：Modbus TCP 从站 + 数据泵。
///
/// 它的价值在于「一条命令就能看到完整链路」——主站连的是真实 TCP、走的是真实
/// MBAP 帧、经真实编解码，只是对端换成了同进程的内存模型。比任何 mock 都更接近现场。
/// </summary>
public sealed class Simulator : IAsyncDisposable
{
    private readonly ModbusSlaveModel _model;
    private readonly object _gate = new();
    private readonly ModbusTcpSlaveServer _server;
    private readonly CancellationTokenSource _cts = new();
    private readonly ILogger _log;
    private readonly TimeSpan _period;
    private Task? _pump;
    private long _counter;
    private int _disposed;

    public Simulator(int port, ILogger log, TimeSpan? period = null)
    {
        _log = log;
        _period = period ?? TimeSpan.FromSeconds(1);
        _model = new ModbusSlaveModel();

        Seed();

        _server = new ModbusTcpSlaveServer(IPAddress.Loopback, port, _model, _gate);
        Port = port;
    }

    public int Port { get; }
    public ModbusSlaveModel Model => _model;

    public void Start()
    {
        _server.Start();
        _pump = Task.Run(() => PumpAsync(_cts.Token));
        _log.LogInformation("modbus slave listening on 127.0.0.1:{Port}", Port);
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

    private async Task PumpAsync(CancellationToken ct)
    {
        var started = Stopwatch.GetTimestamp();
        var period = _period;

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
            }

            try { await Task.Delay(period, ct).ConfigureAwait(false); }
            catch (OperationCanceledException) { return; }
        }
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

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        await _cts.CancelAsync().ConfigureAwait(false);

        if (_pump is not null)
        {
            try { await _pump.ConfigureAwait(false); } catch { /* 关闭期异常忽略 */ }
        }

        await _server.DisposeAsync().ConfigureAwait(false);
        _cts.Dispose();
    }
}
