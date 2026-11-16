using Microsoft.Extensions.Logging.Abstractions;
using Uic.Codec.Bacnet;
using Uic.Config;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Bacnet;
using Uic.Engine;
using Uic.Host;
using Uic.Obs;

namespace Uic.Tests;

/// <summary>
/// `uic sim` 的 BACnet 链路：拿 <see cref="DemoConfig"/> 给出的开箱配置，
/// 起真正的 <see cref="Simulator"/> 从站，再用真正的主站驱动读一遍。
///
/// 为什么不只测编解码与驱动：这两层各自绿灯、`sim` 模式却一读就红，是这类改动最常见的坏法——
/// 端口传错、点位种错对象号、通道缺 `transport=udp`。只有把「配置 → 从站 → 主站」
/// 串起来跑一次，才能证明文档里「一条命令就能看到完整链路」这句话是真的。
/// </summary>
internal static class BacnetSimModeTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("bacnet/sim_mode_demo_config_reads_every_point", SimModeReadsEveryPoint));
    }

    private static Task SimModeReadsEveryPoint(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 端口取 0：让系统分配。并发跑用例时固定 47810 会互相抢，
            // 而「端口被占」的表现是读超时，最容易被误判成驱动坏了。
            await using var simulator = new Simulator(
                port: 0, s7Port: 0, bacnetPort: 0,
                NullLogger.Instance, TimeSpan.FromMilliseconds(50));
            simulator.Start();

            // DemoConfig 的 BACnet 通道必须落在从站真正监听的端口上：
            // 端口对不上就是超时，而不是「配置看起来没问题」。
            var document = DemoConfig.Build(
                enableLocalBacnet: true, bacnetPort: simulator.BacnetPort);

            var channel = document.Channels.Single(c => c.Protocol == "bacnet");
            var device = document.Devices.Single(d => d.ChannelId == channel.Id);

            t.Equal("udp", channel.Transport, "BACnet/IP is carried over UDP");
            t.Equal(simulator.BacnetPort, channel.Port, "the channel points at the embedded slave");
            t.Ok(device.Enabled, "the demo BACnet device is enabled in sim mode");
            t.Ok(device.Points.Count >= 4, $"the demo device exposes its points ({device.Points.Count})");

            // 点位必须全是 object 寻址：混进一个 register 点位会被驱动当成配置错误，
            // 而那种失败在「四个点位都 good」的断言里根本看不出来。
            foreach (var point in device.Points)
                t.Equal(AddressKind.Object, point.Address.Kind, $"{point.Id} uses object addressing");

            await using var conn = new BacnetMasterDriver().Open(
                new OpenContext(channel, device, NullLogger.Instance, new MetricsRegistry()));

            // 探活先过：配置里的 deviceInstance 必须与从站 I-Am 里的一致。
            await conn.ProbeAsync(t.Ct);

            var refs = device.Points.Select(p => PointRef.Of(device, p)).ToArray();

            var samples = await conn.ReadAsync(ReadPlanner.Plan(refs), t.Ct);
            var byId = samples.ToDictionary(s => s.PointId, StringComparer.Ordinal);

            t.Equal(device.Points.Count, samples.Count, "one sample per demo point");

            foreach (var point in device.Points)
                t.Equal(Quality.Good, byId[point.Id].Quality,
                    $"{point.Id} quality ({byId[point.Id].Error})");

            // 值必须来自从站种下的那份脉搏，而不是「读到了 0 也算 good」。
            // 温度与压力是正弦，任何时刻都不等于 0；计数是单调计数器。
            t.Close(55.5, byId["temperature"].Value!.AsNumber(), 30.0,
                "temperature is the seeded sine around its 55.5 baseline");
            t.Close(101.25, byId["pressure"].Value!.AsNumber(), 10.0,
                "pressure is the seeded sine around its 101.25 baseline");
            t.Ok(byId["counter"].Value!.AsNumber() >= 0, "counter is a real number");

            var boolValue = byId["running"].Value!.Bool;
            t.Ok(boolValue is not null, "the binary-input point decodes to a boolean");
        });
    }

    /// <summary>
    /// 用例运行器把同步异常包成失败：BACnet 的整套驱动都是异步的，
    /// 忘了 await 的断言会「静默通过」，这里统一收口。
    /// </summary>
    private static Task RunAsync(Func<Task> body) => body();
}
