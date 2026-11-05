using System.Buffers;
using System.Runtime.InteropServices;

namespace Uic.Tests;

/// <summary>
/// 一对伪终端（pty）。主站打开 <see cref="SlavePath"/> 当串口用，测试这一端用
/// <see cref="Read"/> / <see cref="Write"/> 扮演从站。
///
/// 为什么不用内存流：内存流没有内核 tty 层，也就没有 termios、没有行规程、
/// 没有真实的 poller。串口专属的缺陷（半双工串帧、deadline 抖动、关闭打断读）
/// 全部长在那一层，用内存流伪造出来的「通过」只会掩盖问题。
///
/// 这里只调四个 libc 函数（posix_openpt / grantpt / unlockpt / ptsname），
/// 不引第三方包，也不需要外部进程。非 Linux 上直接跳过。
/// </summary>
internal sealed class PtyPair : IDisposable
{
    private const int O_RDWR = 0x0002;
    private const int O_NOCTTY = 0x0100;
    private const short PollIn = 0x0001;
    private const int Eintr = 4;

    private int _master = -1;

    private PtyPair(int master, string slavePath)
    {
        _master = master;
        SlavePath = slavePath;
    }

    /// <summary>从站设备路径，例如 <c>/dev/pts/7</c>。交给 <see cref="SerialOptions.Port"/>。</summary>
    public string SlavePath { get; }

    public static PtyPair Open()
    {
        if (!OperatingSystem.IsLinux())
            throw new SkipException("pty pairs need Linux (posix_openpt)");

        var fd = Posix.posix_openpt(O_RDWR | O_NOCTTY);
        if (fd < 0)
            throw new SkipException($"posix_openpt failed (errno {Marshal.GetLastWin32Error()})");

        if (Posix.grantpt(fd) != 0 || Posix.unlockpt(fd) != 0)
        {
            Posix.close(fd);
            throw new SkipException($"grantpt/unlockpt failed (errno {Marshal.GetLastWin32Error()})");
        }

        var name = Marshal.PtrToStringAnsi(Posix.ptsname(fd));
        if (string.IsNullOrEmpty(name))
        {
            Posix.close(fd);
            throw new SkipException("ptsname returned nothing");
        }

        return new PtyPair(fd, name);
    }

    /// <summary>把字节写到「总线」上，等待主站侧读取。</summary>
    public void Write(ReadOnlySpan<byte> data)
    {
        if (data.IsEmpty) return;

        var scratch = ArrayPool<byte>.Shared.Rent(data.Length);
        try
        {
            data.CopyTo(scratch);

            // 直接传数组时无法表达「从第 n 个字节开始」，所以固定住再算指针；
            // 否则部分写入的重试会从头再写一遍，把帧写重。
            var handle = GCHandle.Alloc(scratch, GCHandleType.Pinned);
            try
            {
                var sent = 0;
                while (sent < data.Length)
                {
                    var ptr = Marshal.UnsafeAddrOfPinnedArrayElement(scratch, sent);
                    var n = (int)Posix.write(_master, ptr, data.Length - sent);
                    if (n < 0)
                    {
                        var errno = Marshal.GetLastWin32Error();
                        if (errno == Eintr) continue;
                        throw new IOException($"pty write failed (errno {errno})");
                    }

                    sent += n;
                }
            }
            finally
            {
                handle.Free();
            }
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(scratch);
        }
    }

    /// <summary>等可读。返回 false 表示在 <paramref name="timeout"/> 内没有数据。</summary>
    public bool WaitReadable(TimeSpan timeout)
    {
        // poll 的 revents 是出参：数组必须标 [In, Out]，否则内核写回的是被封送的副本。
        var fds = new[] { new Posix.PollFd { Fd = _master, Events = PollIn } };
        var ms = (int)Math.Clamp(timeout.TotalMilliseconds, 0, int.MaxValue);
        while (true)
        {
            var rc = Posix.poll(fds, 1, ms);
            if (rc < 0 && Marshal.GetLastWin32Error() == Eintr) continue;
            return rc > 0 && (fds[0].Revents & PollIn) != 0;
        }
    }

    /// <summary>读一段。超时返回 0；对端已关闭返回 -1。</summary>
    public int Read(Span<byte> buffer, TimeSpan timeout)
    {
        if (buffer.IsEmpty) return 0;
        if (!WaitReadable(timeout)) return 0;

        var scratch = ArrayPool<byte>.Shared.Rent(buffer.Length);
        try
        {
            var n = Posix.read(_master, scratch, buffer.Length);
            if (n < 0)
            {
                var errno = Marshal.GetLastWin32Error();
                return errno == Eintr ? 0 : -1;
            }

            scratch.AsSpan(0, n).CopyTo(buffer);
            return n;
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(scratch);
        }
    }

    /// <summary>读满 <paramref name="buffer"/>，超时抛 <see cref="TimeoutException"/>。</summary>
    public void ReadExactly(Span<byte> buffer, TimeSpan timeout)
    {
        var filled = 0;
        var deadline = Environment.TickCount64 + (long)timeout.TotalMilliseconds;

        while (filled < buffer.Length)
        {
            var left = deadline - Environment.TickCount64;
            if (left <= 0) throw new TimeoutException($"pty read timed out after {timeout.TotalMilliseconds:0}ms");

            var n = Read(buffer[filled..], TimeSpan.FromMilliseconds(left));
            if (n < 0) throw new IOException("pty closed while reading");
            if (n == 0) continue;
            filled += n;
        }
    }

    /// <summary>丢弃已经到达的字节，避免上一条用例的尾巴被当成这一条的请求。</summary>
    public void Drain()
    {
        Span<byte> scratch = stackalloc byte[256];
        while (Read(scratch, TimeSpan.Zero) > 0)
        {
        }
    }

    public void Dispose()
    {
        var fd = Interlocked.Exchange(ref _master, -1);
        if (fd >= 0) Posix.close(fd);
    }

    private static class Posix
    {
        [StructLayout(LayoutKind.Sequential)]
        public struct PollFd
        {
            public int Fd;
            public short Events;
            public short Revents;
        }

        [DllImport("libc", SetLastError = true)]
        public static extern int posix_openpt(int flags);

        [DllImport("libc", SetLastError = true)]
        public static extern int grantpt(int fd);

        [DllImport("libc", SetLastError = true)]
        public static extern int unlockpt(int fd);

        [DllImport("libc", SetLastError = true)]
        public static extern IntPtr ptsname(int fd);

        [DllImport("libc", SetLastError = true)]
        public static extern int poll([In, Out] PollFd[] fds, nuint nfds, int timeoutMs);

        [DllImport("libc", SetLastError = true)]
        public static extern int read(int fd, byte[] buffer, int count);

        [DllImport("libc", SetLastError = true)]
        public static extern nint write(int fd, IntPtr buffer, nint count);

        [DllImport("libc", SetLastError = true)]
        public static extern int close(int fd);
    }
}
