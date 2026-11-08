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

    /// <summary>JSON 行文件日志。配置热更新时由运行时重新 Configure，不必重启进程。</summary>
    public JsonLineFileLogProvider? FileLog { get; init; }

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
    private readonly JsonLineFileLogProvider? _fileLog;
    private readonly SampleFanout _fanout;
    private readonly DeviceStateWatcher _watcher;
    private readonly ResourceMonitor _resources;
    private readonly string _dataDir;
    private readonly string _gatewayVersion;
    private readonly DateTimeOffset _startedAt = DateTimeOffset.UtcNow;

    private readonly SemaphoreSlim _applyGate = new(1, 1);

    /// <summary>
    /// 保护 journal 挂载与目标集合。配置热更新在 apply 线程跑，资源看门狗在自己的
    /// 定时线程跑，两边的「停历史/开历史」必须串行，否则会出现「刚停掉又被配置
    /// 更新挂回去」的状态，而看门狗只在状态跳变时触发，不会再纠正它。
    /// </summary>
    private readonly object _sinkGate = new();

    private MqttSink? _mqtt;
    private MqttCommandSink? _mqttCommands;
    private JournalSink? _journal;
    private UicOpcUaServer? _opcUa;
    private ConfigDocument _config;

    /// <summary>
    /// 配置里的凭据只在内存中以明文存在。配置文档本身（落盘、diff、导出）始终带
    /// 引用（<c>${env:...}</c> / <c>enc:v1:...</c>），明文只出现在这里。
    /// </summary>
    private ResolvedSecrets _secrets;

    private bool _historySuspended;
    private int _disposed;

    private GatewayRuntime(
        string dataDir,
        string gatewayVersion,
        ILoggerFactory logs,
        RingBufferLogProvider ring,
        JsonLineFileLogProvider? fileLog,
        ConfigStore store,
        DriverRegistry drivers,
        ConfigDocument config,
        SchedulerOptions? schedulerOptions)
    {
        _dataDir = dataDir;
        _gatewayVersion = gatewayVersion;
        _logs = logs;
        _ring = ring;
        _fileLog = fileLog;
        _store = store;
        _config = config;

        // 凭据解析放在任何线程启动之前：环境变量没注入、密钥不对，就直接起不来，
        // 而不是带着一个空令牌开始对外服务（空令牌等于关闭鉴权）。
        _secrets = Secrets.Resolve(config);

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

        // 资源看门狗最后启动：它的降级动作要停 journal、停文件日志，
        // 这些对象必须先存在。启动时的第一次检查会立即给出当前水位。
        _resources = new ResourceMonitor(
            () => ResourceMonitor.Sample(_dataDir),
            logs.CreateLogger("uic.resources"),
            _metrics,
            ResourceLimits.FromStorage(storage));
        _resources.Changed += OnResourceChanged;
        _resources.Start();

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
            options.FileLog,
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
            // 覆盖只用于命令行的监听地址；令牌永远来自解析后的配置，
            // 避免「--listen 覆盖」顺手把凭据也覆盖成未解析的引用。
            (restOverride ?? _config.Northbound.Rest) with { Token = _secrets.RestToken },
            _config.Site,
            _gatewayVersion,
            _startedAt,
            _resources,
            new JournalHistory(_dataDir));

    // ------------------------------------------------------------------
    // IGatewayControl
    // ------------------------------------------------------------------

    public async Task<ApplyOutcome> ApplyConfigAsync(ConfigDocument doc, string? comment, CancellationToken ct)
    {
        ModelValidator.ThrowIfInvalid(doc);

        // 凭据在任何副作用发生之前解析：环境变量没注入、密钥不对，本次 apply 直接失败，
        // 设备不会先重启一轮再告诉我们配置存不下。
        var secrets = Secrets.Resolve(doc);

        await _applyGate.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            var before = _config;
            var started = Stopwatch.GetTimestamp();

            // 先让调度器收敛，再落版本：反过来会出现「版本已切换但设备没重启」的假成功。
            var result = await _engine.ApplyAsync(doc).ConfigureAwait(false);
            var version = _store.Commit(doc, comment);
            _config = doc;
            _secrets = secrets;

            if (before.Site != doc.Site)
            {
                _log.LogWarning(
                    "site changed from '{Old}' to '{New}'; northbound topics and API metadata keep the old value until restart",
                    before.Site, doc.Site);
            }

            // 日志落盘设置可以在线切换；其它北向监听地址仍然要求重启。
            if (!SameJson(before.Logging, doc.Logging))
            {
                _fileLog?.Configure(doc.Logging.File, _dataDir);
                _log.LogInformation("file logging {State}",
                    doc.Logging.File.Enabled ? $"-> {_fileLog?.ActivePath}" : "disabled");
            }

            ConfigureSinks(doc, before);
            _fanout.SetTargets(Targets(doc));

            // 水位阈值可以热更新；下一次检查立即生效，不必等进程重启。
            _resources.Configure(ResourceLimits.FromStorage(doc.Storage));

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

    public Task FlushOutboxAsync(string? sink, CancellationToken ct) => _outbox.FlushAsync(sink, ct);

    public async Task<string> GenerateDiagnosticsAsync(CancellationToken ct)
    {
        // 诊断包写进 dataDir 而不是临时目录：现场排障时容器可能随时被替换，
        // 操作员需要能在下一次重启前把文件拷走。
        var dir = Path.Combine(_dataDir, "diagnostics");
        Directory.CreateDirectory(dir);

        var name = DiagnosticsBundle.FileName(DateTimeOffset.UtcNow);
        var path = Path.Combine(dir, name);
        var bytes = DiagnosticsBundle.Build(CreateNorthboundContext());

        await File.WriteAllBytesAsync(path, bytes, ct).ConfigureAwait(false);
        return path;
    }

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

    private IReadOnlyList<string> Targets(ConfigDocument config)
    {
        lock (_sinkGate)
        {
            var targets = new List<string>(2);
            if (config.Northbound.Mqtt.Enabled) targets.Add("mqtt");

            // 磁盘降级期间不再写 journal：区域空间要留给 Outbox 落盘。
            // 已排队的记录不删除，恢复后继续从原位点投递。
            if (config.Storage.Journal && !_historySuspended) targets.Add("journal");
            return targets;
        }
    }

    /// <summary>
    /// 磁盘水位降级时停历史（journal）。停的是「新增」而不是「已有」：
    /// 已排队的记录保留，恢复后继续投递，避免为了省空间把没送出去的数据也删了。
    /// </summary>
    private void SetHistorySuspended(bool suspended)
    {
        lock (_sinkGate)
        {
            if (_historySuspended == suspended) return;
            _historySuspended = suspended;

            if (suspended)
            {
                if (_journal is null) return;

                _outbox.RemoveSink("journal");
                var old = _journal;
                _journal = null;
                _ = old.DisposeAsync().AsTask();
                _log.LogWarning("journal sink paused: disk free space is below the watermark");
                return;
            }

            if (_journal is not null || !_config.Storage.Journal) return;

            _journal = new JournalSink(Path.Combine(_dataDir, "journal"));
            _outbox.RegisterSink(_journal);
            _log.LogInformation("journal sink resumed at {Path}", _journal.FilePath);
        }
    }

    private void OnResourceChanged(ResourceStatus status)
    {
        if (status.DiskLow)
        {
            // 先停文件日志再停 journal：先砍掉最不重要的写入。
            _fileLog?.Suspend();
        }
        else
        {
            _fileLog?.Resume();
        }

        SetHistorySuspended(status.DiskLow);
        _fanout.SetTargets(Targets(_config));
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
            var oldCommands = _mqttCommands;
            _mqtt = null;
            _mqttCommands = null;
            _outbox.RemoveSink("mqtt");

            if (oldCommands is not null)
            {
                // 先摘掉命令回调，再关连接：避免旧回调在旧连接的最后几条消息上执行命令。
                _ = oldCommands.DisposeAsync().AsTask();
            }

            if (old is not null)
            {
                // 旧连接上的在飞消息已经没意义；队列里的记录会在新 sink 上重发。
                _ = old.DisposeAsync().AsTask();
            }

            if (mqtt.Enabled)
            {
                // 只把解析后的口令交给客户端：MqttOptions 是 record，克隆一份即可，
                // 配置文档本身保持不变（作业日志与导出里仍然是引用）。
                var resolved = mqtt with { Password = _secrets.MqttPassword };

                var sink = new MqttSink(resolved, config.Site, _logs.CreateLogger("uic.mqtt"));

                MqttCommandSink? commands = null;
                if (mqtt.Commands)
                {
                    // 构造时登记订阅，因此必须在 sink.Start() 之前创建。
                    commands = new MqttCommandSink(
                        sink.Client, resolved, config.Site, this,
                        _logs.CreateLogger("uic.mqtt.commands"));
                }

                _outbox.RegisterSink(sink);
                sink.Start();
                _mqtt = sink;
                _mqttCommands = commands;

                _log.LogInformation("mqtt sink started: {Host}:{Port}", mqtt.Host, mqtt.Port);

                if (commands is not null)
                {
                    _log.LogInformation("mqtt command channel on {Topic} -> {Result} (restart/selftest/flush/log-level)",
                        commands.Topic, commands.ResultTopic);
                }
            }
        }

        lock (_sinkGate)
        {
            var journalWanted = config.Storage.Journal;
            if (_journal is null && journalWanted && !_historySuspended)
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
        }

        if (previous is not null && !SameJson(previous.Storage, config.Storage))
        {
            _log.LogWarning(
                "storage limits changed; outbox keeps its startup configuration until restart (batches/retention), " +
                "resource watermarks apply immediately");
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
        _metrics.Gauge("uic_disk_free_bytes",
            () => _resources.Status.Reading.FreeBytes ?? -1,
            "bytes available in the data directory partition (-1 when unknown)");
        _metrics.Gauge("uic_resources_degraded",
            () => _resources.Status.Degraded ? 1 : 0,
            "1 when disk or memory is below the configured watermark");
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0) return;

        // 先停资源看门狗：否则它会在关闭过程中把 journal 又挂回去。
        await _resources.DisposeAsync().ConfigureAwait(false);
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

            if (_mqttCommands is not null)
            {
                await _mqttCommands.DisposeAsync().ConfigureAwait(false);
                _mqttCommands = null;
            }

            await _mqtt.DisposeAsync().ConfigureAwait(false);
        }

        if (_journal is not null) await _journal.DisposeAsync().ConfigureAwait(false);

        await _outbox.DisposeAsync().ConfigureAwait(false);
        await _engine.DisposeAsync().ConfigureAwait(false);
        _applyGate.Dispose();
    }
}
