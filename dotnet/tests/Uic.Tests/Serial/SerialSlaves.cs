using System.Diagnostics;
using System.Text;
using Uic.Codec.Modbus;
using Uic.Sim.Modbus;

namespace Uic.Tests;

/// <summary>
/// 伪终端另一端的 Modbus RTU 从站。
///
/// 它做的事和真实从站一样：收一帧、算响应、回一帧。中间刻意留一段
/// <see cref="ReplyDelay"/> 的「处理时间」，并在那段时间里盯着总线——
/// 如果主站在响应回来之前又发了一帧，说明半双工被破坏（T15 要抓的正是这个）。
/// </summary>
internal sealed class RtuSlave : IDisposable
{
    private static readonly TimeSpan FrameWait = TimeSpan.FromMilliseconds(500);

    private readonly PtyPair _pty;
    private readonly Dictionary<byte, ModbusSlaveHandler> _units;
    private readonly CancellationTokenSource _cts = new();
    private readonly Thread _worker;
    private readonly List<byte[]> _requests = new();
    private readonly List<long> _requestStamps = new();
    private readonly object _lock = new();

    private int _disposed;

    public RtuSlave(PtyPair pty, params (byte Unit, ModbusSlaveModel Model)[] units)
    {
        _pty = pty;
        _units = units.ToDictionary(u => u.Unit, u => new ModbusSlaveHandler(u.Model));
        _worker = new Thread(Run) { IsBackground = true, Name = "pty-rtu-slave" };
    }

    /// <summary>从站「处理」一帧要多久；这段时间内总线上不应出现新请求。</summary>
    public TimeSpan ReplyDelay { get; init; } = TimeSpan.FromMilliseconds(20);

    /// <summary>沉默从站：收到帧也不回。用来验证 timeout 与 deadline。</summary>
    public bool Silent { get; init; }

    /// <summary>响应还没发出时又收到新帧（半双工被破坏）。</summary>
    public bool Overlapped { get; private set; }

    public int RequestCount
    {
        get { lock (_lock) return _requests.Count; }
    }

    public IReadOnlyList<byte[]> Requests
    {
        get { lock (_lock) return _requests.ToArray(); }
    }

    /// <summary>每帧读完的时刻（单调时钟），用来断言帧间静默。</summary>
    public IReadOnlyList<long> RequestStamps
    {
        get { lock (_lock) return _requestStamps.ToArray(); }
    }

    /// <summary>后台线程的异常。线程里未捕获异常会直接杀进程，所以记下来交给用例断言。</summary>
    public string? Fault { get; private set; }

    public void Start() => _worker.Start();

    private void Run()
    {
        try
        {
            RunCore();
        }
        catch (Exception ex)
        {
            Fault = ex.ToString();
        }
    }

    private void RunCore()
    {
        while (!_cts.IsCancellationRequested)
        {
            var frame = TryReadFrame();
            if (frame is null) continue;
            lock (_lock)
            {
                _requests.Add(frame);
                _requestStamps.Add(Stopwatch.GetTimestamp());
            }

            if (Silent) continue;

            byte unit;
            byte[] pdu;
            try
            {
                (unit, pdu) = ModbusRtuFrame.Parse(frame);
            }
            catch
            {
                // 帧被截断或 CRC 不对：真实从站同样会丢弃，然后等主站超时。
                continue;
            }

            if (!_units.TryGetValue(unit, out var handler))
            {
                // 总线上没有这个站号：保持沉默，这正是 T16 要复现的场景。
                continue;
            }

            // 「处理」这段时间里总线必须是安静的。
            if (_pty.WaitReadable(ReplyDelay))
            {
                Overlapped = true;
                _pty.Drain();
            }

            var response = handler.Handle(pdu);
            _pty.Write(ModbusRtuFrame.Build(unit, response));
        }
    }

    private byte[]? TryReadFrame()
    {
        var head = new byte[2];
        try
        {
            _pty.ReadExactly(head, FrameWait);
        }
        catch (TimeoutException)
        {
            return null;
        }
        catch (IOException)
        {
            return null;
        }

        var fc = head[1];
        var frame = new List<byte>(64) { head[0], head[1] };

        try
        {
            if (fc is ModbusConstants.FcWriteMultipleCoils or ModbusConstants.FcWriteMultipleRegs)
            {
                // addr(2) qty(2) byteCount(1) + data + CRC(2)
                var mid = new byte[5];
                _pty.ReadExactly(mid, FrameWait);
                frame.AddRange(mid);

                var tail = new byte[mid[4] + 2];
                _pty.ReadExactly(tail, FrameWait);
                frame.AddRange(tail);
            }
            else if (fc is ModbusConstants.FcReadFileRecord or ModbusConstants.FcWriteFileRecord)
            {
                // 文件记录请求自带长度：功能码后是「字节数」，再往后那么多字节 + CRC(2)。
                var bc = new byte[1];
                _pty.ReadExactly(bc, FrameWait);
                frame.AddRange(bc);

                var tail = new byte[bc[0] + 2];
                _pty.ReadExactly(tail, FrameWait);
                frame.AddRange(tail);
            }
            else
            {
                // 读功能码与单次写：addr(2) + qty/value(2) + CRC(2)
                var rest = new byte[6];
                _pty.ReadExactly(rest, FrameWait);
                frame.AddRange(rest);
            }
        }
        catch (TimeoutException)
        {
            return null;
        }
        catch (IOException)
        {
            return null;
        }

        return frame.ToArray();
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;
        _cts.Cancel();
        _worker.Join(TimeSpan.FromSeconds(2));
        _cts.Dispose();
    }
}

/// <summary>伪终端另一端的 Modbus ASCII 从站：读到 CRLF 为止，回一行 ':' 开头的文本。</summary>
internal sealed class AsciiSlave : IDisposable
{
    private static readonly TimeSpan FrameWait = TimeSpan.FromMilliseconds(500);

    private readonly PtyPair _pty;
    private readonly Dictionary<byte, ModbusSlaveHandler> _units;
    private readonly CancellationTokenSource _cts = new();
    private readonly Thread _worker;
    private readonly List<string> _requests = new();
    private readonly object _lock = new();
    private int _disposed;

    public AsciiSlave(PtyPair pty, params (byte Unit, ModbusSlaveModel Model)[] units)
    {
        _pty = pty;
        _units = units.ToDictionary(u => u.Unit, u => new ModbusSlaveHandler(u.Model));
        _worker = new Thread(Run) { IsBackground = true, Name = "pty-ascii-slave" };
    }

    public int RequestCount
    {
        get { lock (_lock) return _requests.Count; }
    }

    public IReadOnlyList<string> Requests
    {
        get { lock (_lock) return _requests.ToArray(); }
    }

    /// <summary>后台线程的异常，含义同 <see cref="RtuSlave.Fault"/>。</summary>
    public string? Fault { get; private set; }

    public void Start() => _worker.Start();

    private void Run()
    {
        try
        {
            RunCore();
        }
        catch (Exception ex)
        {
            Fault = ex.ToString();
        }
    }

    private void RunCore()
    {
        var line = new StringBuilder(64);
        var one = new byte[1];

        while (!_cts.IsCancellationRequested)
        {
            if (_pty.Read(one, FrameWait) <= 0) continue;

            line.Append((char)one[0]);

            if (line.Length > 512)
            {
                line.Clear();
                continue;
            }

            if (one[0] != (byte)'\n') continue;

            var frame = line.ToString();
            line.Clear();

            if (!frame.StartsWith(':') || !frame.EndsWith("\r\n", StringComparison.Ordinal)) continue;

            lock (_lock) _requests.Add(frame);

            byte unit;
            byte[] pdu;
            try
            {
                (unit, pdu) = ModbusAsciiFrame.Parse(frame);
            }
            catch
            {
                continue;
            }

            if (!_units.TryGetValue(unit, out var handler)) continue;

            var response = ModbusAsciiFrame.Build(unit, handler.Handle(pdu));
            _pty.Write(Encoding.ASCII.GetBytes(response));
        }
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;
        _cts.Cancel();
        _worker.Join(TimeSpan.FromSeconds(2));
        _cts.Dispose();
    }
}
