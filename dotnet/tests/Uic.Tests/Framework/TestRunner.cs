using System.Diagnostics;

namespace Uic.Tests;

/// <summary>断言失败。运行器据此记一条失败，其余断言仍然继续跑。</summary>
internal sealed class CheckFailedException : Exception
{
    public CheckFailedException(string message) : base(message) { }
}

/// <summary>
/// 环境不具备执行条件（例如非 Linux 上没有伪终端）。
///
/// 与「失败」严格区分：跳过意味着这条用例在本机根本没有验证过，
/// 输出里必须能看见它，而不是悄悄算进通过数。
/// </summary>
internal sealed class SkipException : Exception
{
    public SkipException(string reason) : base(reason) { }
}

/// <summary>
/// 单个用例的上下文：断言 + 临时目录 + 取消信号。
///
/// 断言直接抛异常而不是返回 bool：失败点就是异常点，不需要每行都写 if。
/// </summary>
internal sealed class TestContext
{
    private readonly string _tempRoot;

    public TestContext(CancellationToken ct)
    {
        Ct = ct;
        _tempRoot = Path.Combine(Path.GetTempPath(), "uic-tests", Guid.NewGuid().ToString("N")[..12]);
        Directory.CreateDirectory(_tempRoot);
    }

    public CancellationToken Ct { get; }

    /// <summary>用例独占的可写目录，用例结束时删除。</summary>
    public string TempDir => _tempRoot;

    public string TempFile(string name) => Path.Combine(_tempRoot, name);

    public void Fail(string what) => throw new CheckFailedException(what);

    public void Ok(bool condition, string what)
    {
        if (!condition) throw new CheckFailedException(what);
    }

    public void Equal<T>(T expected, T actual, string what)
    {
        if (!EqualityComparer<T>.Default.Equals(expected, actual))
            throw new CheckFailedException($"{what}: expected <{Format(expected)}>, got <{Format(actual)}>");
    }

    /// <summary>浮点比较。相对误差，避免把 1499.9999 判成与 1500 不等。</summary>
    public void Close(double expected, double actual, double tolerance, string what)
    {
        var scale = Math.Max(1d, Math.Max(Math.Abs(expected), Math.Abs(actual)));
        if (Math.Abs(expected - actual) > tolerance * scale)
            throw new CheckFailedException(
                $"{what}: expected {expected:R} ± {tolerance:R}, got {actual:R}");
    }

    public void Contains(string haystack, string needle, string what)
    {
        if (!haystack.Contains(needle, StringComparison.Ordinal))
            throw new CheckFailedException($"{what}: '{needle}' not found in <{Shorten(haystack)}>");
    }

    public void Throws<TException>(Action action, string what) where TException : Exception
    {
        try
        {
            action();
        }
        catch (TException)
        {
            return;
        }
        catch (Exception ex)
        {
            throw new CheckFailedException(
                $"{what}: expected {typeof(TException).Name}, got {ex.GetType().Name}: {Shorten(ex.Message)}");
        }

        throw new CheckFailedException($"{what}: expected {typeof(TException).Name}, nothing was thrown");
    }

    public async Task ThrowsAsync<TException>(Func<Task> action, string what) where TException : Exception
    {
        try
        {
            await action().ConfigureAwait(false);
        }
        catch (TException)
        {
            return;
        }
        catch (Exception ex)
        {
            throw new CheckFailedException(
                $"{what}: expected {typeof(TException).Name}, got {ex.GetType().Name}: {Shorten(ex.Message)}");
        }

        throw new CheckFailedException($"{what}: expected {typeof(TException).Name}, nothing was thrown");
    }

    public void Cleanup()
    {
        try
        {
            if (Directory.Exists(_tempRoot)) Directory.Delete(_tempRoot, recursive: true);
        }
        catch (IOException)
        {
            // 临时目录清理失败不影响结论。
        }
    }

    private static string Format<T>(T value) => value switch
    {
        null => "null",
        string s => "\"" + Shorten(s) + "\"",
        double d => d.ToString("R", System.Globalization.CultureInfo.InvariantCulture),
        _ => value.ToString() ?? "null",
    };

    private static string Shorten(string text)
    {
        text = text.Replace('\n', ' ').Replace('\r', ' ').Trim();
        return text.Length <= 160 ? text : text[..160] + "…";
    }
}

/// <summary>一个用例。<paramref name="Flag"/> 非空时必须显式传参才会执行（例如 --opcua）。</summary>
internal sealed record TestCase(string Name, Func<TestContext, Task> Body, string? Flag = null);

/// <summary>
/// 极简测试运行器。
///
/// 只做四件事：过滤、逐个执行（带超时）、打印结果、给出退出码。
/// 不并行：这些用例会绑本地端口、写临时目录，串行更接近现场。
/// </summary>
internal static class TestRunner
{
    public static async Task<int> RunAsync(IReadOnlyList<TestCase> cases, string[] args)
    {
        if (args.Contains("--list"))
        {
            foreach (var c in cases) Console.WriteLine(c.Name);
            return 0;
        }

        var filter = ValueOf(args, "--filter");
        var flags = args.Where(a => a.StartsWith("--", StringComparison.Ordinal))
            .Select(a => a[2..])
            .ToHashSet(StringComparer.OrdinalIgnoreCase);

        var stopOnFail = flags.Contains("stop-on-fail");

        var selected = cases
            .Where(c => filter is null || c.Name.Contains(filter, StringComparison.OrdinalIgnoreCase))
            .ToArray();

        Console.WriteLine($"uic tests · {selected.Length} case(s){(filter is null ? string.Empty : $" matching '{filter}'")}");

        var clock = Stopwatch.StartNew();
        var failed = new List<(string Name, string Error)>();
        var skipped = 0;

        foreach (var test in selected)
        {
            if (test.Flag is not null && !flags.Contains(test.Flag))
            {
                skipped++;
                Console.WriteLine($"  skip  {test.Name}  (requires --{test.Flag})");
                continue;
            }

            // 单个用例硬超时：挂死比失败更难排查，宁可让运行器替它判负。
            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(180));
            var ctx = new TestContext(cts.Token);
            var started = Stopwatch.StartNew();

            try
            {
                var body = test.Body(ctx);
                var finished = await Task.WhenAny(body, Task.Delay(Timeout.Infinite, cts.Token)).ConfigureAwait(false);

                if (finished != body)
                {
                    throw new CheckFailedException("timed out after 180s");
                }

                await body.ConfigureAwait(false);
                Console.WriteLine($"  ok    {test.Name}  ({started.ElapsedMilliseconds} ms)");
            }
            catch (OperationCanceledException) when (cts.IsCancellationRequested)
            {
                failed.Add((test.Name, "timed out after 180s"));
                Console.WriteLine($"  FAIL  {test.Name}  (timeout)");
                if (stopOnFail) break;
            }
            catch (SkipException ex)
            {
                skipped++;
                Console.WriteLine($"  skip  {test.Name}  ({ex.Message})");
            }
            catch (CheckFailedException ex)
            {
                failed.Add((test.Name, ex.Message));
                Console.WriteLine($"  FAIL  {test.Name}");
                Console.WriteLine($"        {ex.Message}");
                if (stopOnFail) break;
            }
            catch (Exception ex)
            {
                failed.Add((test.Name, $"{ex.GetType().Name}: {ex.Message}"));
                Console.WriteLine($"  FAIL  {test.Name}");
                Console.WriteLine($"        {ex.GetType().Name}: {ex.Message}");
                if (stopOnFail) break;
            }
            finally
            {
                ctx.Cleanup();
            }
        }

        clock.Stop();

        var passed = selected.Length - failed.Count - skipped;
        Console.WriteLine();
        Console.WriteLine($"{passed} passed, {failed.Count} failed, {skipped} skipped · {clock.Elapsed.TotalSeconds:0.0}s");

        if (failed.Count == 0) return 0;

        Console.WriteLine("failures:");
        foreach (var (name, error) in failed) Console.WriteLine($"  - {name}: {error}");

        return 1;
    }

    private static string? ValueOf(string[] args, string name)
    {
        for (var i = 0; i < args.Length; i++)
        {
            if (args[i] == name && i + 1 < args.Length) return args[i + 1];
            if (args[i].StartsWith(name + "=", StringComparison.Ordinal)) return args[i][(name.Length + 1)..];
        }

        return null;
    }
}
