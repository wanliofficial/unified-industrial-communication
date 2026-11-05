using System.Diagnostics;
using System.Text.Json;
using Microsoft.Extensions.Logging;
using Uic.Config;
using Uic.Domain;
using Uic.Driver;
using Uic.Engine;
using Uic.Northbound;
using Uic.Obs;
using Uic.OpcUa;
using Uic.Outbox;
using OutboxQueue = Uic.Outbox.Outbox;

namespace Uic.Runtime;

public sealed record RuntimeOptions
{
    public required string DataDir { get; init; }

    /// <summary>配置文件的路径；为空或不存在时使用兜底配置。</summary>
    public string? ConfigPath { get; init; }

    public string GatewayVersion { get; init; } = "0.1.0";
    public LogLevelSwitch Level { get; init; } = new();
    public SchedulerOptions? SchedulerOptions { get; init; }

    /// <summary>首次启动的兜底配置。CI 与自测会替换成最小配置。</summary>
    public Func<ConfigDocument>? Fallback { get; init; }
}

/// <summary>
/// 运行时装配：注册表 + 配置存储 + 调度器 + 上报队列 + 北向 sink。
///
/// 所有「谁持有谁」的决定都在这里，别处不再做装配。这样北向、引擎、驱动
/// 都可以保持单向依赖，不会出现「HTTP 层悄悄启动了一个采集线程」这类问题。
/// </summary>
public sealed class GatewayRuntime : IGatewayControl, IAsyncDisposable
{
    private readonly ILoggerFactory _logs;
    private readonly ILogger _log;
    private readonly ConfigStore _store;
    private readonly Scheduler _engine;
    private readonly OutboxQueue _outbox;
    private readonly MetricsRegistry _metrics;
    private readonly RingBufferLogProvider _ring;
    private readonly SampleFanout _fanout;
    private readonly DeviceStateWatcher _watcher;
    private readonly string _dataDir;
    private readonly string _gatewayVersion;
    private readonly DateTimeOffset _startedAt = DateTimeOffset.UtcNow;

    private readonly SemaphoreSlim _applyGate = new(1, 1);

    private MqttSink? _mqtt;
    private JournalSink? _journal;
    private UicOpcUaServer? _opcUa;
    private ConfigDocument _config;
    private int _disposed;

    private GatewayRuntime(
        string dataDir,
        string gatewayVersion,
        ILoggerFactory logs,
        RingBufferLogProvider ring,
        ConfigStore store,
        DriverRegistry drivers,
        ConfigDocument config,
        SchedulerOptions? schedulerOptions)
    {
        _dataDir = dataDir;
        _gatewayVersion = gatewayVersion;
        _logs = logs;
        _ring = ring;
        _store = store;
        _config = config;
        _log = logs.CreateLogger("uic.runtime");
        _metrics = new MetricsRegistry();

        _engine = new Scheduler(drivers, new PointCache(), logs, schedulerOptions);
        _engine.Start(config);

        var storage = config.Storage;
        _outbox = new OutboxQueue(dataDir, new OutboxOptions
        {
            MaxRecords = storage.OutboxMaxRecords,
            MaxBytes = storage.OutboxMaxBytes,
            Ttl = storage.OutboxTtl,
            BatchSize = storage.OutboxBatchSize,
            FlushInterval = storage.OutboxFlushInterval,
        });

        ConfigureSinks(config, previous: null);

        _fanout = new SampleFanout(_engine.Cache, _outbox, _metrics, Targets(config));
        _fanout.Start();
        _outbox.Start();

        _watcher = new DeviceStateWatcher(
            _engine,
            () => _mqtt,
            logs.CreateLogger("uic.watchdog"),
            _metrics);
        _watcher.Start();

        RegisterGauges();
    }

    public Scheduler Engine => _engine;
    public ConfigStore Store => _store;
    public OutboxQueue Outbox => _outbox;
    public MetricsRegistry Metrics => _metrics;
    public RingBufferLogProvider Logs => _ring;
    public DateTimeOffset StartedAt => _startedAt;
    public LogLevelSwitch Level { get; private init; } = new();

    public ConfigDocument CurrentConfig => _config;
    public string ConfigVersion => _store.CurrentId;

    /// <summary>启动顺序：先读配置（失败就别启动），再起采集，最后起北向 sink。</summary>
    public static async Task<GatewayRuntime> CreateAsync(
        RuntimeOptions options,
        DriverRegistry drivers,
        ILoggerFactory logs,
        RingBufferLogProvider ring,
        CancellationToken ct = default)
    {
        Directory.CreateDirectory(options.DataDir);

        var store = new ConfigStore(options.DataDir);
        var fallback = options.Fallback ?? (() => DemoConfig.Build());
        var config = await store.BootstrapAsync(options.ConfigPath ?? string.Empty, fallback, ct)
            .ConfigureAwait(false);

        var runtime = new GatewayRuntime(
            options.DataDir,
            options.GatewayVersion,
            logs,
            ring,
            store,
            drivers,
            config,
            options.SchedulerOptions)
        {
            Level = options.Level,
        };

        // OPC UA 的启动是异步的，而且失败必须让进程起不来（配置说 enabled 却没监听，
        // 比直接退出更难排查）。其它北向 sink 都在构造函数里同步就绪。
        await runtime.StartOpcUaAsync(config, ct).ConfigureAwait(false);

        return runtime;
    }

    private async Task StartOpcUaAsync(ConfigDocument config, CancellationToken ct)
    {
        if (!config.Northbound.OpcUa.Enabled) return;

        var options = OpcUaServerOptions.FromConfig(config.Northbound.OpcUa, _dataDir, config.Site);
        _opcUa = await UicOpcUaServer.StartAsync(
            options, _engine.Cache, _engine, config, _logs, ct).ConfigureAwait(false);
    }

    /// <summary>
    /// 按新配置重启 OPC UA。失败只记录不抛出：此时设备与配置已经生效，
    /// 让整个 apply 失败会给出「什么都没变」的错误信号。失败通过日志与
    /// uic_opcua_start_failures_total 暴露。
    /// </summary>
    private async Task RestartOpcUaAsync(ConfigDocument config, CancellationToken ct)
    {
        var old = Interlocked.Exchange(ref _opcUa, null);
        if (old is not null)
        {
            await old.DisposeAsync().ConfigureAwait(false);
        }

        if (!config.Northbound.OpcUa.Enabled) return;

        try
        {
            var options = OpcUaServerOptions.FromConfig(config.Northbound.OpcUa, _dataDir, config.Site);
            _opcUa = await UicOpcUaServer.StartAsync(
                options, _engine.Cache, _engine, config, _logs, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _metrics.Counter("uic_opcua_start_failures_total", 1);
            _log.LogError(ex, "config applied, but the opc ua northbound failed to start");
        }
    }

    /// <summary>北向需要的上下文。Site 与 listen 在启动时定稿，变更需要重启进程。</summary>
    public NorthboundContext CreateNorthboundContext(RestOptions? restOverride = null)
        => new(
            _engine,
            _store,
            _outbox,
            _metrics,
            _ring,
            this,
            restOverride ?? _config.Northbound.Rest,
            _config.Site,
            _gatewayVersion,
            _startedAt);

    // ------------------------------------------------------------------
    // IGatewayControl
    // ------------------------------------------------------------------

    public async Task<ApplyOutcome> ApplyConfigAsync(ConfigDocument doc, string? comment, CancellationToken ct)
    {
        ModelValidator.ThrowIfInvalid(doc);

        await _applyGate.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            var before = _config;
            var started = Stopwatch.GetTimestamp();

            // 先让调度器收敛，再落版本：反过来会出现「版本已切换但设备没重启」的假成功。
            var result = await _engine.ApplyAsync(doc).ConfigureAwait(false);
            var version = _store.Commit(doc, comment);
            _config = doc;

            if (before.Site != doc.Site)
            {
                _log.LogWarning(
                    "site changed from '{Old}' to '{New}'; northbound topics and API metadata keep the old value until restart",
                    before.Site, doc.Site);
            }

            ConfigureSinks(doc, before);
            _fanout.SetTargets(Targets(doc));

            if (before is null || !SameJson(before.Northbound.OpcUa, doc.Northbound.OpcUa) || before.Site != doc.Site)
            {
                await RestartOpcUaAsync(doc, ct).ConfigureAwait(false);
            }
            else
            {
                // 点位/单位变化不需要重启服务端，重建受影响的子树即可。
                _opcUa?.UpdateConfig(doc);
            }

            var elapsed = (long)Stopwatch.GetElapsedTime(started).TotalMilliseconds;

            _log.LogInformation(
                "config {Version} applied in {Ms}ms (+{Added} ~{Restarted} -{Removed})",
                version, elapsed, result.Added.Count, result.Restarted.Count, result.Removed.Count);

            return new ApplyOutcome(
                version,
                true,
                result.Restarted,
                result.Added,
                result.Removed,
                elapsed,
                comment);
        }
        finally
        {
            _applyGate.Release();
        }
    }

    public async Task<ApplyOutcome> RollbackAsync(string versionId, CancellationToken ct)
    {
        var doc = _store.LoadVersion(versionId)
                  ?? throw new KeyNotFoundException($"config version '{versionId}' not found or is unreadable");

        return await ApplyConfigAsync(doc, $"rollback to {versionId}", ct).ConfigureAwait(false);
    }

    public async Task RestartDeviceAsync(string deviceId, CancellationToken ct)
    {
        var actor = _engine.Find(deviceId)
                    ?? throw new KeyNotFoundException($"device '{deviceId}' not found");

        _log.LogInformation("restarting device {Device} on request", deviceId);
        await actor.RestartAsync().ConfigureAwait(false);
    }

    public Task<bool> SelfTestAsync(string deviceId, CancellationToken ct)
        => _engine.SelfTestAsync(deviceId, ct);

    public Task FlushOutboxAsync(CancellationToken ct) => _outbox.FlushAsync(ct);

    public void SetLogLevel(string level)
    {
        if (!Level.TrySet(level))
        {
            throw ApiException.BadRequest("request.invalid_parameter",
                $"unknown log level '{level}'");
        }

        _log.LogInformation("log level set to {Level}", Level.Level);
    }

    // ------------------------------------------------------------------
    // 北向 sink 装配
    // ------------------------------------------------------------------

    private static IReadOnlyList<string> Targets(ConfigDocument config)
    {
        var targets = new List<string>(2);
        if (config.Northbound.Mqtt.Enabled) targets.Add("mqtt");
        if (config.Storage.Journal) targets.Add("journal");
        return targets;
    }

    private void ConfigureSinks(ConfigDocument config, ConfigDocument? previous)
    {
        var mqtt = config.Northbound.Mqtt;
        var mqttChanged = previous is null ||
                          !SameJson(previous.Northbound.Mqtt, mqtt) ||
                          previous.Site != config.Site;

        if (mqttChanged)
        {
            var old = _mqtt;
            _mqtt = null;
            _outbox.RemoveSink("mqtt");

            if (old is not null)
            {
                // 旧连接上的在飞消息已经没意义；队列里的记录会在新 sink 上重发。
                _ = old.DisposeAsync().AsTask();
            }

            if (mqtt.Enabled)
            {
                var sink = new MqttSink(mqtt, config.Site, _logs.CreateLogger("uic.mqtt"));
                _outbox.RegisterSink(sink);
                sink.Start();
                _mqtt = sink;
                _log.LogInformation("mqtt sink started: {Host}:{Port}", mqtt.Host, mqtt.Port);
            }
        }

        var journalWanted = config.Storage.Journal;
        if (_journal is null && journalWanted)
        {
            _journal = new JournalSink(Path.Combine(_dataDir, "journal"));
            _outbox.RegisterSink(_journal);
            _log.LogInformation("journal sink started at {Path}", _journal.FilePath);
        }
        else if (_journal is not null && !journalWanted)
        {
            _outbox.RemoveSink("journal");
            var old = _journal;
            _journal = null;
            _ = old.DisposeAsync().AsTask();
        }

        if (previous is not null && !SameJson(previous.Storage, config.Storage))
        {
            _log.LogWarning(
                "storage limits changed; outbox keeps its startup configuration until restart (batches/retention)");
        }
    }

    private static bool SameJson<T>(T a, T b)
        => JsonSerializer.Serialize(a, JsonDefaults.Options) == JsonSerializer.Serialize(b, JsonDefaults.Options);

    private void RegisterGauges()
    {
        _metrics.Gauge("uic_outbox_pending", () => _outbox.Stats().Pending,
            "records waiting for a northbound sink");
        _metrics.Gauge("uic_devices_total", () => _engine.Actors.Count,
            "configured devices");
        _metrics.Gauge("uic_devices_collecting",
            () => _engine.Actors.Count(a => a.State == RuntimeState.Collecting),
            "devices currently delivering good data");
        _metrics.Gauge("uic_sse_connections", () => 0,
            "active SSE streams (reported by the HTTP layer)");
        _metrics.Gauge("uic_opcua_writes_total", () => _opcUa?.NodeManager.Writes ?? 0,
            "OPC UA write requests accepted");
        _metrics.Gauge("uic_opcua_write_failures_total", () => _opcUa?.NodeManager.WriteFailures ?? 0,
            "OPC UA write requests that failed");
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        await _watcher.DisposeAsync().ConfigureAwait(false);
        _fanout.Dispose();

        if (_opcUa is not null)
        {
            // 先停 OPC UA：客户端断开后不会再有写入进入调度器，随后的 flush 才是终态。
            await _opcUa.DisposeAsync().ConfigureAwait(false);
            _opcUa = null;
        }

        // 停机时先尽力把队列冲干净，再发 $death；顺序反了会让最后几条数据永远留在盘上。
        try
        {
            using var flush = new CancellationTokenSource(TimeSpan.FromSeconds(3));
            await _outbox.FlushAsync(flush.Token).ConfigureAwait(false);
        }
        catch
        {
            // 停机上仍有未投递的数据是允许的，下次启动会继续。
        }

        if (_mqtt is not null)
        {
            try
            {
                using var bye = new CancellationTokenSource(TimeSpan.FromSeconds(2));
                await _mqtt.PublishDeathAsync(bye.Token).ConfigureAwait(false);
            }
            catch
            {
                // $death 发不出去只能留痕，退出码不受影响。
            }

            await _mqtt.DisposeAsync().ConfigureAwait(false);
        }

        if (_journal is not null) await _journal.DisposeAsync().ConfigureAwait(false);

        await _outbox.DisposeAsync().ConfigureAwait(false);
        await _engine.DisposeAsync().ConfigureAwait(false);
        _applyGate.Dispose();
    }
}
