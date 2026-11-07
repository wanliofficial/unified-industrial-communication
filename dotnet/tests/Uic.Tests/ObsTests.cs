using System.Text.Json;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using Uic.Domain;
using Uic.Obs;

namespace Uic.Tests;

/// <summary>
/// 可观测性用例（FR-OPS-11 / NFR 日志字段）。
///
/// 文件日志的价值全在「现场能按行解析」和「不会把磁盘写爆」两件事上，
/// 所以这里不测实现细节，只测：字段齐不齐、轮转有没有丢行、归档有没有超量。
/// </summary>
internal static class ObsTests
{
    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("logs/json_line_fields_are_complete", JsonLineFieldsAreComplete));
        tests.Add(new TestCase("logs/file_rotates_without_losing_lines", FileRotatesWithoutLosingLines));
        tests.Add(new TestCase("logs/retention_prunes_old_archives", RetentionPrunesOldArchives));
        tests.Add(new TestCase("logs/disabled_provider_writes_nothing", DisabledProviderWritesNothing));
        tests.Add(new TestCase("logs/reconfigure_stops_writing", ReconfigureStopsWriting));
        tests.Add(new TestCase("logs/invalid_file_config_is_rejected", InvalidFileConfigIsRejected));
        tests.Add(new TestCase("logs/ring_buffer_returns_newest_first", RingBufferReturnsNewestFirst));
        tests.Add(new TestCase("resources/disk_pressure_has_hysteresis", DiskPressureHasHysteresis));
        tests.Add(new TestCase("resources/memory_pressure_has_hysteresis", MemoryPressureHasHysteresis));
        tests.Add(new TestCase("resources/unreadable_probe_is_not_pressure", UnreadableProbeIsNotPressure));
        tests.Add(new TestCase("resources/file_log_pauses_during_pressure", FileLogPausesDuringPressure));
        tests.Add(new TestCase("resources/invalid_watermarks_are_rejected", InvalidWatermarksAreRejected));
    }

    /// <summary>一行的字段口径：ts/level/category/msg/device/exception。</summary>
    private static Task JsonLineFieldsAreComplete(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "logs");

        using var provider = new JsonLineFileLogProvider();
        provider.Configure(new FileLogOptions
        {
            Enabled = true,
            Directory = dir,
            FileName = "uic.log",
            MaxBytes = 1 << 20,
            Retain = 2,
        }, t.TempDir);

        var path = provider.ActivePath;
        t.Ok(path is not null, "the provider opens a file once enabled");
        t.Equal(Path.Combine(Path.GetFullPath(dir), "uic.log"), path, "the active path follows the config");

        var logger = provider.CreateLogger("uic.device.meter-1");

        logger.LogInformation("read failed for {Point}", "temp");
        logger.LogError(new InvalidOperationException("boom"), "device stopped");

        provider.Dispose();

        var lines = File.ReadAllLines(path!);
        t.Equal(2, lines.Length, "one JSON object per log call");

        using var first = JsonDocument.Parse(lines[0]);
        var root = first.RootElement;

        t.Ok(root.TryGetProperty("ts", out var ts) && ts.TryGetDateTimeOffset(out _),
            "ts is a parseable timestamp");
        t.Equal("Information", root.GetProperty("level").GetString(), "level is the wire name");
        t.Equal("uic.device.meter-1", root.GetProperty("category").GetString(), "category is kept");
        t.Equal("read failed for temp", root.GetProperty("msg").GetString(), "the message is rendered");
        t.Equal("meter-1", root.GetProperty("device").GetString(), "the device id is extracted from the category");
        t.Ok(!root.TryGetProperty("exception", out _), "a clean entry has no exception field");

        using var second = JsonDocument.Parse(lines[1]);
        var failed = second.RootElement;
        t.Equal("Error", failed.GetProperty("level").GetString(), "the second line is an error");
        t.Contains(failed.GetProperty("exception").GetString()!, "InvalidOperationException",
            "the exception type is preserved");
        t.Contains(failed.GetProperty("exception").GetString()!, "boom",
            "the exception message is preserved");

        return Task.CompletedTask;
    }

    private static Task FileRotatesWithoutLosingLines(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "logs");
        const int total = 120;
        var padding = new string('x', 80);

        var provider = new JsonLineFileLogProvider();
        provider.Configure(new FileLogOptions
        {
            Enabled = true,
            Directory = dir,
            FileName = "uic.log",
            // 4096 是校验下限；每条日志约 160 字节，120 条必然触发多次轮转。
            MaxBytes = 4096,
            // 留够归档，验证「轮转本身不丢行」；归档裁剪由下一条用例单独验证。
            Retain = 20,
        }, t.TempDir);

        var path = provider.ActivePath!;
        var logger = provider.CreateLogger("uic.runtime");

        for (var i = 0; i < total; i++)
        {
            logger.LogInformation("entry {Index} {Padding}", i, padding);
        }

        provider.Dispose();

        var files = Directory.GetFiles(dir).OrderBy(p => p, StringComparer.Ordinal).ToArray();
        var archives = files.Where(p => !string.Equals(p, path, StringComparison.Ordinal)).ToArray();

        t.Ok(File.Exists(path), "the active file still exists");
        t.Ok(archives.Length >= 2, "the volume was large enough to rotate at least twice");
        t.Ok(archives.Length <= 20, "retention is an upper bound, not a target");

        var lines = files.SelectMany(File.ReadAllLines).ToArray();
        t.Equal(total, lines.Length, "rotation never drops a line");

        var indexes = new HashSet<int>();
        foreach (var line in lines)
        {
            using var document = JsonDocument.Parse(line);
            var message = document.RootElement.GetProperty("msg").GetString()!;
            var start = message.IndexOf("entry ", StringComparison.Ordinal) + "entry ".Length;
            var end = message.IndexOf(' ', start);
            indexes.Add(int.Parse(message[start..end]));
        }

        t.Equal(total, indexes.Count, "every entry appears exactly once");
        t.Ok(indexes.Contains(0) && indexes.Contains(total - 1), "both the first and last entries survived");

        return Task.CompletedTask;
    }

    private static Task RetentionPrunesOldArchives(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "logs");
        var padding = new string('y', 80);

        using var provider = new JsonLineFileLogProvider();
        provider.Configure(new FileLogOptions
        {
            Enabled = true,
            Directory = dir,
            FileName = "uic.log",
            MaxBytes = 4096,
            Retain = 2,
        }, t.TempDir);

        var path = provider.ActivePath!;
        var logger = provider.CreateLogger("uic.runtime");

        for (var i = 0; i < 120; i++)
        {
            logger.LogInformation("entry {Index} {Padding}", i, padding);
        }

        provider.Dispose();

        var files = Directory.GetFiles(dir);
        var archives = files.Where(p => !string.Equals(p, path, StringComparison.Ordinal)).ToArray();

        t.Equal(2, archives.Length, "retain caps the number of archived files");
        t.Ok(File.Exists(path), "the active file is never pruned");

        // 保留的归档必须是完整、可解析的：裁剪只能删整份，不能留下半行。
        foreach (var file in files)
        {
            foreach (var line in File.ReadAllLines(file))
            {
                using var document = JsonDocument.Parse(line);
                t.Equal("Information", document.RootElement.GetProperty("level").GetString(),
                    "every retained line is complete JSON");
            }
        }

        return Task.CompletedTask;
    }

    private static Task DisabledProviderWritesNothing(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "logs");

        using var provider = new JsonLineFileLogProvider();
        provider.Configure(new FileLogOptions { Enabled = false, Directory = dir }, t.TempDir);

        t.Equal(null, provider.ActivePath, "a disabled provider holds no file");

        provider.CreateLogger("uic.runtime").LogInformation("this must not reach disk");

        t.Ok(!Directory.Exists(dir) || Directory.GetFiles(dir).Length == 0,
            "no directory or file is created while disabled");

        return Task.CompletedTask;
    }

    private static Task ReconfigureStopsWriting(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "logs");

        using var provider = new JsonLineFileLogProvider();
        provider.Configure(new FileLogOptions
        {
            Enabled = true,
            Directory = dir,
            FileName = "uic.log",
            MaxBytes = 1 << 20,
            Retain = 2,
        }, t.TempDir);

        var path = provider.ActivePath!;
        var logger = provider.CreateLogger("uic.runtime");

        logger.LogInformation("before disable");
        provider.Configure(new FileLogOptions { Enabled = false, Directory = dir }, t.TempDir);
        logger.LogInformation("after disable");

        provider.Dispose();

        var lines = File.ReadAllLines(path);
        t.Equal(1, lines.Length, "reconfiguring to disabled stops further writes");
        t.Contains(lines[0], "before disable", "the line before the switch is kept");
        t.Ok(!lines.Any(l => l.Contains("after disable", StringComparison.Ordinal)),
            "the line after the switch is not written");

        return Task.CompletedTask;
    }

    private static Task InvalidFileConfigIsRejected(TestContext t)
    {
        var doc = new ConfigDocument
        {
            Site = "log-test",
            Channels =
            {
                new Channel { Id = "ch", Protocol = "virtual", Transport = "none" },
            },
            Devices =
            {
                new Device
                {
                    Id = "dev",
                    ChannelId = "ch",
                    Interval = TimeSpan.FromSeconds(1),
                    Points =
                    {
                        new Point
                        {
                            Id = "p",
                            Address = Address.Register("holding", 0),
                            DataType = DataType.UInt16,
                        },
                    },
                },
            },
            Logging = new LoggingOptions
            {
                File = new FileLogOptions
                {
                    Directory = "",
                    FileName = "sub/dir.log",
                    MaxBytes = 1024,
                    Retain = 0,
                    MinimumLevel = "verbose",
                },
            },
        };

        var problems = ModelValidator.Validate(doc);
        var paths = problems.Select(p => p.Path).ToHashSet(StringComparer.Ordinal);

        t.Ok(paths.Contains("logging.file.directory"), "an empty directory is rejected");
        t.Ok(paths.Contains("logging.file.fileName"), "path separators in fileName are rejected");
        t.Ok(paths.Contains("logging.file.maxBytes"), "a size below 4 KiB is rejected");
        t.Ok(paths.Contains("logging.file.retain"), "zero retention is rejected");
        t.Ok(paths.Contains("logging.file.minimumLevel"), "an unknown level name is rejected");

        return Task.CompletedTask;
    }

    private static Task RingBufferReturnsNewestFirst(TestContext t)
    {
        using var ring = new RingBufferLogProvider(capacity: 16);
        var logger = ring.CreateLogger("uic.device.meter-1");

        for (var i = 0; i < 20; i++)
        {
            logger.LogWarning("line {Index}", i);
        }

        var recent = ring.Snapshot(limit: 3);
        t.Equal(3, recent.Count, "limit is honored");
        t.Contains(recent[0].Message, "line 19", "the newest entry comes first");
        t.Contains(recent[1].Message, "line 18", "entries descend by time");
        t.Equal("meter-1", recent[0].DeviceId, "the ring shares the device id convention");
        t.Equal("Warning", recent[0].Level, "the ring keeps the level name");

        var byDevice = ring.Snapshot(deviceId: "meter-1", limit: 100);
        t.Equal(16, byDevice.Count, "the ring never grows past capacity and filters by device");
        t.Equal(0, ring.Snapshot(deviceId: "other", limit: 100).Count, "unknown devices return nothing");

        return Task.CompletedTask;
    }

    /// <summary>可变的假探针；资源水位用例靠改这三个字段模拟磁盘/内存变化。</summary>
    private sealed class Probe
    {
        public long? Free = 10_000;
        public long? Total = 100_000;
        public long? WorkingSet = 1_000;

        public ResourceReading Read() => new(Free, Total, WorkingSet, DateTimeOffset.UtcNow);
    }

    private static async Task DiskPressureHasHysteresis(TestContext t)
    {
        var probe = new Probe();
        var changes = new List<ResourcePressure>();

        await using var monitor = new ResourceMonitor(probe.Read, NullLogger.Instance, limits: new ResourceLimits
        {
            MinFreeBytes = 1_000,
            ResumeFreeBytes = 2_000,
        });
        monitor.Changed += status => changes.Add(status.Pressure);

        var status = monitor.Check();
        t.Ok(!status.DiskLow, "free space above the entry line is healthy");
        t.Equal(ResourcePressure.None, status.Pressure, "no pressure is reported");

        probe.Free = 999;
        status = monitor.Check();
        t.Ok(status.DiskLow, "free space below the entry line enters pressure");
        t.Equal(ResourcePressure.Disk, status.Pressure, "the pressure kind is disk");
        t.Equal(1, changes.Count, "the transition fires once");

        probe.Free = 1_999;
        status = monitor.Check();
        t.Ok(status.DiskLow, "space between the entry and resume lines keeps the degraded state");
        t.Equal(1, changes.Count, "holding the state does not fire another transition");

        probe.Free = 2_000;
        status = monitor.Check();
        t.Ok(!status.DiskLow, "reaching the resume line clears the pressure");
        t.Equal(2, changes.Count, "recovery fires exactly once");

        probe.Free = 999;
        status = monitor.Check();
        t.Ok(status.DiskLow, "dropping below the entry line after recovery enters pressure again");
        t.Equal(3, changes.Count, "the second entry fires once");
    }

    private static async Task MemoryPressureHasHysteresis(TestContext t)
    {
        var probe = new Probe();

        await using var monitor = new ResourceMonitor(probe.Read, NullLogger.Instance, limits: new ResourceLimits
        {
            MaxMemoryBytes = 1_000,
        });

        var status = monitor.Check();
        t.Ok(!status.MemoryHigh, "exactly at the limit is not over it");

        probe.WorkingSet = 1_001;
        status = monitor.Check();
        t.Ok(status.MemoryHigh, "exceeding the limit by one byte enters pressure");
        t.Equal(ResourcePressure.Memory, status.Pressure, "the pressure kind is memory");

        // 恢复线是上限的 90%（留 10% 回差）。
        probe.WorkingSet = 950;
        status = monitor.Check();
        t.Ok(status.MemoryHigh, "a small drop does not clear the pressure");

        probe.WorkingSet = 900;
        status = monitor.Check();
        t.Ok(!status.MemoryHigh, "dropping to the resume line clears the pressure");
    }

    private static async Task UnreadableProbeIsNotPressure(TestContext t)
    {
        await using var monitor = new ResourceMonitor(
            () => throw new InvalidOperationException("no disk information on this platform"),
            NullLogger.Instance,
            limits: new ResourceLimits { MinFreeBytes = 1, MaxMemoryBytes = 1 });

        var status = monitor.Check();

        t.Ok(!status.Degraded, "an unreadable probe must not be reported as pressure");
        t.Equal(null, status.Reading.FreeBytes, "the missing reading stays null instead of zero");
        t.Equal(null, status.Reading.WorkingSetBytes, "the missing memory reading stays null too");
    }

    private static Task FileLogPausesDuringPressure(TestContext t)
    {
        var dir = Path.Combine(t.TempDir, "logs");

        using var provider = new JsonLineFileLogProvider();
        provider.Configure(new FileLogOptions
        {
            Enabled = true,
            Directory = dir,
            FileName = "uic.log",
            MaxBytes = 1 << 20,
            Retain = 2,
        }, t.TempDir);

        var path = provider.ActivePath!;
        var logger = provider.CreateLogger("uic.runtime");

        logger.LogInformation("before pressure");
        provider.Suspend();

        t.Equal(null, provider.ActivePath, "a suspended provider holds no file");
        t.Ok(provider.IsSuspended, "the suspended state is visible");

        logger.LogInformation("during pressure");
        provider.Resume();
        logger.LogInformation("after recovery");

        provider.Dispose();

        var lines = File.ReadAllLines(path);
        t.Equal(2, lines.Length, "the paused line never reaches disk");
        t.Contains(lines[0], "before pressure", "the line before the pause is kept");
        t.Contains(lines[1], "after recovery", "writing resumes on the same file");

        return Task.CompletedTask;
    }

    private static Task InvalidWatermarksAreRejected(TestContext t)
    {
        var baseDoc = new ConfigDocument
        {
            Site = "resource-test",
            Channels =
            {
                new Channel { Id = "ch", Protocol = "virtual", Transport = "none" },
            },
            Devices =
            {
                new Device
                {
                    Id = "dev",
                    ChannelId = "ch",
                    Interval = TimeSpan.FromSeconds(1),
                    Points =
                    {
                        new Point
                        {
                            Id = "p",
                            Address = Address.Register("holding", 0),
                            DataType = DataType.UInt16,
                        },
                    },
                },
            },
        };

        static HashSet<string> Paths(ConfigDocument doc)
            => ModelValidator.Validate(doc).Select(p => p.Path).ToHashSet(StringComparer.Ordinal);

        var negative = new ConfigDocument
        {
            Site = baseDoc.Site,
            Channels = baseDoc.Channels,
            Devices = baseDoc.Devices,
            Storage = new StorageOptions
            {
                MinFreeBytes = -1,
                ResumeFreeBytes = -1,
                MaxMemoryBytes = -1,
                ResourceCheckInterval = TimeSpan.Zero,
            },
        };

        var paths = Paths(negative);
        t.Ok(paths.Contains("storage.minFreeBytes"), "a negative disk watermark is rejected");
        t.Ok(paths.Contains("storage.resumeFreeBytes"), "a negative resume line is rejected");
        t.Ok(paths.Contains("storage.maxMemoryBytes"), "a negative memory watermark is rejected");
        t.Ok(paths.Contains("storage.resourceCheckInterval"), "a zero check interval is rejected");

        var mismatched = new ConfigDocument
        {
            Site = baseDoc.Site,
            Channels = baseDoc.Channels,
            Devices = baseDoc.Devices,
            Storage = new StorageOptions
            {
                MinFreeBytes = 4_000,
                ResumeFreeBytes = 4_000,
            },
        };

        t.Ok(Paths(mismatched).Contains("storage.resumeFreeBytes"),
            "a resume line that is not above the entry line is rejected");

        var orphanResume = new ConfigDocument
        {
            Site = baseDoc.Site,
            Channels = baseDoc.Channels,
            Devices = baseDoc.Devices,
            Storage = new StorageOptions { ResumeFreeBytes = 4_000 },
        };

        t.Ok(Paths(orphanResume).Contains("storage.resumeFreeBytes"),
            "a resume line without an entry line is rejected");

        var valid = new ConfigDocument
        {
            Site = baseDoc.Site,
            Channels = baseDoc.Channels,
            Devices = baseDoc.Devices,
            Storage = new StorageOptions
            {
                MinFreeBytes = 4_000,
                ResumeFreeBytes = 8_000,
                MaxMemoryBytes = 64L * 1024 * 1024,
                ResourceCheckInterval = TimeSpan.FromSeconds(5),
            },
        };

        t.Equal(0, Paths(valid).Count, "a coherent watermark set passes validation");

        return Task.CompletedTask;
    }
}
