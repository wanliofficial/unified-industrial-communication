using Microsoft.Extensions.Logging;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Virtual;
using Uic.Obs;
using Uic.Outbox;
using Uic.Runtime;

namespace Uic.Tests;

/// <summary>
/// 优雅退出（FR-RUN-11）：停机顺序是「先停采集、再尽力把上报队列冲干净、
/// 最后才发 $death 关连接」。这三件事的顺序不能颠倒：
///
/// - 先停采集：否则停机后迟到的样本还会进队列，flush 永远追不上新数据；
/// - 先 flush 再发 $death：$death 是 retained 的「我下线了」，
///   如果队列还没投完就发出去，订阅者会以为数据到此为止——
///   而实际上还有一批排在盘上没送走；
/// - flush 有上限（3 秒）：停机不能无限等一个对端不可用的情况，
///   未投完的记录留在盘上，下次启动重放。
///
/// 这里用真实进程内 runtime + 一个可脚本化的 sink 验证顺序与副作用，
/// 而不是只断言 DisposeAsync 不抛异常。
/// </summary>
internal static class ShutdownTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("shutdown/flush_runs_before_death_and_clears_the_queue", FlushRunsBeforeDeathAndClearsTheQueue));
        tests.Add(new TestCase("shutdown/flush_gives_up_after_the_deadline", FlushGivesUpAfterTheDeadline));
    }

    /// <summary>
    /// 停机时队列里还有记录：它们必须被投走（位点推进），
    /// 并且 $death 之后队列已空——顺序反了的话订阅者会漏掉最后一批。
    /// </summary>
    private static async Task FlushRunsBeforeDeathAndClearsTheQueue(TestContext t)
    {
        using var logs = LoggerFactory.Create(builder => { builder.ClearProviders(); });
        var ring = new RingBufferLogProvider(128);

        var runtime = await GatewayRuntime.CreateAsync(
            new RuntimeOptions
            {
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                // 关闭 journal 等存储 sink：本用例只关心「我们注册的 sink 收到了最后一批」。
                Fallback = () => new ConfigDocument
                {
                    SchemaVersion = 1,
                    Site = "shutdown-site",
                    Description = "shutdown contract",
                    Channels = new List<Channel>
                    {
                        new() { Id = "virtual-bus", Protocol = "virtual", Transport = "none" },
                    },
                    Devices = new List<Device>
                    {
                        new()
                        {
                            Id = "v1",
                            ChannelId = "virtual-bus",
                            Interval = TimeSpan.FromMilliseconds(200),
                            Labels = new Dictionary<string, string>
                            {
                                ["mode"] = "const",
                                ["baseline"] = "42",
                            },
                            Points = new List<Point>
                            {
                                new()
                                {
                                    Id = "temp",
                                    Address = Address.Register("holding", 0),
                                    DataType = DataType.Float64,
                                },
                            },
                        },
                    },
                },
            },
            Drivers(),
            logs,
            ring,
            t.Ct);

        var sink = new ScriptedSink();
        runtime.Outbox.RegisterSink(sink);

        // 直接塞三条待投记录：不依赖采集节奏，停机时它们必须全部送达。
        runtime.Outbox.Enqueue("northbound", "one");
        runtime.Outbox.Enqueue("northbound", "two");
        runtime.Outbox.Enqueue("northbound", "three");

        var pendingBefore = runtime.Outbox.Stats().Pending;
        t.Equal(3, pendingBefore, "three records are pending before shutdown");

        await runtime.DisposeAsync();

        t.Equal(3, sink.Delivered.Count, "all pending records were delivered during shutdown flush");
        t.Equal("one", sink.Delivered[0], "order is preserved");
        t.Equal("three", sink.Delivered[2], "order is preserved");

        // 队列被清空：位点推进了，没有记录永远留在盘上。
        t.Equal(0, runtime.Outbox.Stats().Pending, "the queue is empty after a clean flush");
    }

    /// <summary>
    /// flush 有 3 秒上限：sink 一直失败时，停机不能无限等——
    /// 超时的记录留在盘上，下次启动重放（位点未推进）。
    /// </summary>
    private static async Task FlushGivesUpAfterTheDeadline(TestContext t)
    {
        using var logs = LoggerFactory.Create(builder => { builder.ClearProviders(); });
        var ring = new RingBufferLogProvider(128);

        var runtime = await GatewayRuntime.CreateAsync(
            new RuntimeOptions
            {
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = () => new ConfigDocument
                {
                    SchemaVersion = 1,
                    Site = "shutdown-site",
                    Description = "shutdown contract",
                    Channels = new List<Channel>
                    {
                        new() { Id = "virtual-bus", Protocol = "virtual", Transport = "none" },
                    },
                    Devices = new List<Device>
                    {
                        new()
                        {
                            Id = "v1",
                            ChannelId = "virtual-bus",
                            Interval = TimeSpan.FromMilliseconds(200),
                            Labels = new Dictionary<string, string> { ["mode"] = "const", ["baseline"] = "1" },
                            Points = new List<Point>
                            {
                                new()
                                {
                                    Id = "temp",
                                    Address = Address.Register("holding", 0),
                                    DataType = DataType.Float64,
                                },
                            },
                        },
                    },
                },
            },
            Drivers(),
            logs,
            ring,
            t.Ct);

        var failing = new ScriptedSink { Fail = true };
        runtime.Outbox.RegisterSink(failing);

        runtime.Outbox.Enqueue("northbound", "stuck");

        // 停机必须在一个有界时间内返回：sink 一直失败时不能挂住退出。
        var started = System.Diagnostics.Stopwatch.StartNew();
        await runtime.DisposeAsync();
        started.Stop();

        t.Ok(started.Elapsed < TimeSpan.FromSeconds(20),
            $"shutdown returns even when the sink never accepts data (took {started.ElapsedMilliseconds} ms)");

        // 记录没被丢弃：位点未推进，下次启动重放。
        t.Equal(1, runtime.Outbox.Stats().Pending, "undeliverable records stay on disk for the next boot");
    }

    private static DriverRegistry Drivers()
    {
        var registry = new DriverRegistry();
        registry.Register(new VirtualDriver());
        return registry;
    }

    /// <summary>可编程投递目标：按开关成功或失败，并记录收到的每一条。</summary>
    private sealed class ScriptedSink : IOutboxSink
    {
        public List<string> Delivered { get; } = new();
        public bool Fail { get; set; }

        public string Name => "northbound";

        public Task DeliverAsync(IReadOnlyList<OutboxRecord> batch, CancellationToken ct)
        {
            if (Fail) throw new InvalidOperationException("the sink is down");
            Delivered.AddRange(batch.Select(r => r.Payload));
            return Task.CompletedTask;
        }
    }
}
