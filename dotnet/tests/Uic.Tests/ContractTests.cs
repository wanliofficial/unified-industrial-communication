using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Hosting.Server.Features;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;
using Uic.Config;
using Uic.Domain;
using Uic.Driver;
using Uic.Driver.Modbus;
using Uic.Driver.Mqtt;
using Uic.Driver.Virtual;
using Uic.Engine;
using Uic.Northbound;
using Uic.Obs;
using Uic.Runtime;

namespace Uic.Tests;

/// <summary>
/// REST 契约冒烟：在进程内起一个真实的 Kestrel，走真实的路由、鉴权与错误信封。
///
/// 不 mock HTTP 层：契约的价值就在于「跨进程看过去是什么样」，
/// 用 TestServer 或直接调 handler 都会漏掉中间件顺序这类问题。
/// </summary>
internal static class ContractTests
{
    private const string Token = "contract-token";

    /// <summary>
    /// 配置里的凭据只能是引用。用例把一个真实值放进环境变量，
    /// 这样走的是与生产完全相同的解析路径（而不是在内存里塞明文绕过校验）。
    /// </summary>
    public const string TokenVariable = "UIC_CONTRACT_TOKEN";

    /// <summary>配置里保存的凭据引用；明文只通过环境变量注入。</summary>
    private static readonly string TokenRef = "${env:" + TokenVariable + "}";

    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("contract/health_and_version_are_public", HealthAndVersionArePublic));
        tests.Add(new TestCase("contract/health_reports_drops_in_the_last_hour", HealthReportsDropsInTheLastHour));
        tests.Add(new TestCase("contract/resource_pressure_degrades_health", ResourcePressureDegradesHealth));
        tests.Add(new TestCase("contract/apply_keeps_redacted_secrets", ApplyKeepsRedactedSecrets));
        tests.Add(new TestCase("contract/history_query_reads_journal", HistoryQueryReadsJournal));
        tests.Add(new TestCase("contract/pagination_cursor_is_validated", PaginationCursorIsValidated));
        tests.Add(new TestCase("contract/api_requires_bearer_token", ApiRequiresBearerToken));
        tests.Add(new TestCase("contract/devices_and_points_are_listed", DevicesAndPointsAreListed));
        tests.Add(new TestCase("contract/disabled_device_shows_both_states", DisabledDeviceShowsBothStates));
        tests.Add(new TestCase("contract/drivers_expose_capability_states", DriversExposeCapabilityStates));
        tests.Add(new TestCase("contract/unknown_device_returns_error_envelope", UnknownDeviceReturnsEnvelope));
        tests.Add(new TestCase("contract/unknown_point_returns_error_envelope", UnknownPointReturnsErrorEnvelope));
        tests.Add(new TestCase("contract/config_is_redacted", ConfigIsRedacted));
        tests.Add(new TestCase("contract/config_validate_reports_problems", ConfigValidateReportsProblems));
        tests.Add(new TestCase("contract/malformed_legacy_field_is_not_a_server_fault", MalformedLegacyFieldIsNotAServerFault));
        tests.Add(new TestCase("contract/plaintext_credentials_are_rejected", PlaintextCredentialsAreRejected));
        tests.Add(new TestCase("contract/every_credential_field_is_redacted", EveryCredentialFieldIsRedacted));
        tests.Add(new TestCase("contract/apply_refuses_unregistered_driver", ApplyRefusesUnregisteredDriver));
        tests.Add(new TestCase("contract/write_returns_per_point_results", WriteReturnsPerPointResults));
        tests.Add(new TestCase("contract/write_values_are_engineering_units", WriteValuesAreEngineeringUnits));
        tests.Add(new TestCase("contract/single_point_write_is_idempotent", SinglePointWriteIsIdempotent));
        tests.Add(new TestCase("contract/malformed_write_body_is_a_client_error", MalformedWriteBodyIsAClientError));
        tests.Add(new TestCase("contract/idempotency_key_rejects_a_different_body", IdempotencyKeyRejectsADifferentBody));
        tests.Add(new TestCase("contract/idempotency_window_ignores_a_clock_jump", IdempotencyWindowIgnoresAClockJump));
        tests.Add(new TestCase("contract/idempotency_ledger_is_per_runtime", IdempotencyLedgerIsPerRuntime));
        tests.Add(new TestCase("contract/template_instantiates_and_applies_devices", TemplateInstantiatesAndApplies));
        tests.Add(new TestCase("contract/points_csv_import_previews_and_applies", PointsCsvImportPreviewsAndApplies));
        tests.Add(new TestCase("contract/config_export_import_round_trip", ConfigExportImportRoundTrip));
        tests.Add(new TestCase("contract/config_import_migrates_legacy_shape", ConfigImportMigratesLegacy));
        tests.Add(new TestCase("contract/points_expose_effective_interval", PointsExposeEffectiveInterval));
        tests.Add(new TestCase("contract/points_expose_array_count_and_values", PointsExposeArrayCount));
        tests.Add(new TestCase("contract/console_device_creation_flow", ConsoleDeviceCreationFlow));
        tests.Add(new TestCase("contract/audit_records_write_and_config", AuditRecordsWriteAndConfig));
        tests.Add(new TestCase("contract/audit_export_is_ndjson", AuditExportIsNdjson));
        tests.Add(new TestCase("contract/audit_denies_are_recorded", AuditDeniesAreRecorded));
        tests.Add(new TestCase("contract/auth_login_forces_password_change", AuthLoginForcesPasswordChange));
        tests.Add(new TestCase("contract/auth_roles_are_enforced", AuthRolesAreEnforced));
        tests.Add(new TestCase("contract/auth_users_require_manage", AuthUsersRequireManage));
        tests.Add(new TestCase("contract/auth_login_is_rate_limited", AuthLoginIsRateLimited));
        tests.Add(new TestCase("contract/auth_logout_revokes_session", AuthLogoutRevokesSession));
        tests.Add(new TestCase("contract/rate_limit_returns_429_with_the_shared_code", RateLimitReturns429));
        tests.Add(new TestCase("contract/metrics_report_live_sse_connections", MetricsReportLiveSseConnections));
        tests.Add(new TestCase("contract/sse_refuses_the_connection_past_the_cap", SseRefusesTheConnectionPastTheCap));
        tests.Add(new TestCase("contract/metrics_export_runtime_series", MetricsExportRuntimeSeries));
        tests.Add(new TestCase("contract/stream_filters_by_point_pattern", StreamFiltersByPointPattern));
        tests.Add(new TestCase("contract/diagnostics_bundle_carries_the_four_parts", DiagnosticsBundleCarriesTheFourParts));
        tests.Add(new TestCase("contract/opcua_restart_failure_is_counted",
            OpcUaRestartFailureIsCounted));
        tests.Add(new TestCase("contract/command_rejects_missing_parameter",
            CommandRejectsMissingParameter));
        tests.Add(new TestCase("contract/command_requires_confirm_for_destructive_actions",
            CommandRequiresConfirmForDestructiveActions));
    }

    // ------------------------------------------------------------------
    // 夹具
    // ------------------------------------------------------------------

    internal sealed class Host : IAsyncDisposable
    {
        private readonly WebApplication _app;
        private readonly GatewayRuntime _runtime;
        private readonly ILoggerFactory _logs;

        private Host(WebApplication app, GatewayRuntime runtime, ILoggerFactory logs, HttpClient client)
        {
            _app = app;
            _runtime = runtime;
            _logs = logs;
            Client = client;
        }

        public HttpClient Client { get; }

        public GatewayRuntime Runtime => _runtime;

        public static async Task<Host> StartAsync(
            TestContext t,
            Func<ConfigDocument>? config = null,
            bool tls = false,
            TimeProvider? clock = null,
            string? dataDir = null)
        {
            // 配置里只有 ${env:UIC_CONTRACT_TOKEN}，真实值放进程环境：
            // 与现场「环境注入」走同一条解析路径，也让用例无法绕过明文校验。
            Environment.SetEnvironmentVariable(TokenVariable, Token);

            var ring = new RingBufferLogProvider(128);
            var logs = LoggerFactory.Create(builder =>
            {
                builder.ClearProviders();
                builder.AddProvider(ring);
                builder.SetMinimumLevel(LogLevel.Warning);
            });

            // TLS 用例需要配置里先有证书路径，而证书又要写进临时目录：
            // 先在宿主里生成，再把它注入调用方给出的配置。
            X509Certificate2? certificate = null;
            if (tls)
            {
                certificate = TestCertificates.Create();
                var certPath = TestCertificates.WritePfx(t.TempDir, certificate);
                var inner = config ?? BuildConfig;

                config = () =>
                {
                    var doc = inner();

                    // ConfigDocument 是可变类而不是 record：逐字段复制，
                    // 只替换 Northbound.Rest.Tls，其余部分原样保留。
                    return new ConfigDocument
                    {
                        SchemaVersion = doc.SchemaVersion,
                        Site = doc.Site,
                        Description = doc.Description,
                        Channels = doc.Channels,
                        Devices = doc.Devices,
                        Northbound = new NorthboundOptions
                        {
                            Rest = doc.Northbound.Rest with
                            {
                                Tls = new RestTlsOptions
                                {
                                    Enabled = true,
                                    CertificateFile = certPath,
                                },
                            },
                            Mqtt = doc.Northbound.Mqtt,
                            OpcUa = doc.Northbound.OpcUa,
                        },
                        Storage = doc.Storage,
                        Logging = doc.Logging,
                    };
                };
            }

            var options = new RuntimeOptions
            {
                DataDir = dataDir ?? t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = config ?? BuildConfig,
                SchedulerOptions = clock is null ? null : new SchedulerOptions { Time = clock },
                Clock = clock,
            };

            var runtime = await GatewayRuntime.CreateAsync(options, Drivers(), logs, ring, t.Ct);

            var builder = WebApplication.CreateBuilder(new WebApplicationOptions
            {
                EnvironmentName = "Development",
            });

            builder.Logging.ClearProviders();

            if (tls)
            {
                var rest = runtime.CurrentConfig.Northbound.Rest;
                var serverCertificate = Tls.LoadServerCertificate(rest, t.TempDir, runtime.SecretValues);

                builder.WebHost.ConfigureKestrel(kestrel =>
                    kestrel.ConfigureHttpsDefaults(https => https.ServerCertificate = serverCertificate));
                builder.WebHost.UseUrls("https://127.0.0.1:0");
            }
            else
            {
                builder.WebHost.UseUrls("http://127.0.0.1:0");
            }

            var app = builder.Build();
            app.UseNorthbound(runtime.CreateNorthboundContext());

            await app.StartAsync(t.Ct);

            var address = app.Services.GetRequiredService<IServer>()
                .Features.Get<IServerAddressesFeature>()!
                .Addresses.First();

            var http = tls
                ? new HttpClient(new HttpClientHandler
                {
                    // 钉住这张自签证书：既验证 HTTPS 真的起来，又不依赖系统信任库。
                    ServerCertificateCustomValidationCallback = (_, presented, _, _) =>
                        presented is not null &&
                        presented.GetCertHashString() == certificate!.GetCertHashString(),
                })
                { BaseAddress = new Uri(address), Timeout = TimeSpan.FromSeconds(10) }
                : new HttpClient { BaseAddress = new Uri(address), Timeout = TimeSpan.FromSeconds(10) };

            return new Host(app, runtime, logs, http);
        }

        public async ValueTask DisposeAsync()
        {
            Client.Dispose();
            await _app.StopAsync();
            await _app.DisposeAsync();
            await _runtime.DisposeAsync();
            _logs.Dispose();
        }
    }

    internal static DriverRegistry Drivers()
    {
        var registry = new DriverRegistry();
        registry.Register(ModbusMasterDriver.Tcp(), ModbusMasterDriver.Rtu(), ModbusMasterDriver.Ascii());
        registry.Register(new VirtualDriver());
        registry.Register(new MqttClientDriver());
        return registry;
    }

    /// <summary>最小配置：一台虚拟设备 + 一条启用了令牌的 REST 通道。</summary>
    private static ConfigDocument BuildConfig() => WithMode("const");

    /// <summary>
    /// 与 <see cref="BuildConfig"/> 相同，只是把虚拟设备改成持续变化的正弦波形。
    /// 死区判定只在值变化时产生样本：需要「订阅后不断有新样本」的用例必须用这个。
    /// </summary>
    private static ConfigDocument WaveConfig() => WithMode("sine");

    private static ConfigDocument WithMode(string mode) => new()
    {
        SchemaVersion = 1,
        Site = "contract-site",
        Description = "contract tests",
        Channels = new List<Channel>
        {
            new() { Id = "virtual-bus", Name = "虚拟总线", Protocol = "virtual", Transport = "none" },
        },
        Devices = new List<Device>
        {
            new()
            {
                Id = "v1",
                Name = "虚拟设备",
                ChannelId = "virtual-bus",
                Interval = TimeSpan.FromMilliseconds(200),
                Labels = new Dictionary<string, string>(StringComparer.Ordinal)
                {
                    ["mode"] = mode,
                    ["amplitude"] = "10",
                    ["baseline"] = "42",
                },
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "temp",
                        Name = "温度",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.Float64,
                        Unit = "°C",
                    },
                    new()
                    {
                        Id = "flag",
                        Name = "标志位",
                        Address = Address.Register("coil", 0),
                        DataType = DataType.Bool,
                    },
                },
            },
        },
        Northbound = new NorthboundOptions
        {
            Rest = new RestOptions
            {
                Enabled = true,

                // 真实绑定由 Kestrel 的 UseUrls("...:0") 决定；这个字段只是配置里的声明，
                // 校验器不接受 0 号端口，所以填一个合法值。
                Listen = "127.0.0.1:18080",
                Token = TokenRef,
            },
        },
        Storage = new StorageOptions
        {
            DataDir = "./data",
            OutboxFlushInterval = TimeSpan.FromMilliseconds(50),
        },
    };

    private static HttpRequestMessage Get(string path, bool auth = true)
    {
        var request = new HttpRequestMessage(HttpMethod.Get, path);

        if (auth)
        {
            // 令牌允许省略 "Bearer " 前缀的强制格式？不允许——这里按契约走标准写法。
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
        }

        return request;
    }

    private static HttpRequestMessage Post(string path, string json, bool auth = true)
    {
        var request = new HttpRequestMessage(HttpMethod.Post, path)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        };

        if (auth) request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);

        return request;
    }

    private static HttpRequestMessage PostWithKey(string path, string json, string key)
    {
        var request = Post(path, json);
        request.Headers.Add("X-Idempotency-Key", key);
        return request;
    }

    private static async Task<JsonDocument> ReadJsonAsync(TestContext t, HttpResponseMessage response)
    {
        var text = await response.Content.ReadAsStringAsync(t.Ct);
        t.Ok(!string.IsNullOrWhiteSpace(text), "response has a body");
        return JsonDocument.Parse(text);
    }

    // ------------------------------------------------------------------
    // 用例
    // ------------------------------------------------------------------

    private static Task HealthAndVersionArePublic(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            // 存活/就绪/版本/指标不要求令牌：编排系统探活时不会被鉴权拦住。
            var health = await host.Client.GetAsync("/healthz", t.Ct);
            t.Equal(HttpStatusCode.OK, health.StatusCode, "/healthz status");
            t.Equal("ok", (await health.Content.ReadAsStringAsync(t.Ct)).Trim(), "/healthz body");

            var ready = await host.Client.GetAsync("/readyz", t.Ct);
            t.Ok(ready.StatusCode is HttpStatusCode.OK or HttpStatusCode.ServiceUnavailable,
                "/readyz answers with a readiness verdict");

            using var version = await ReadJsonAsync(t, await host.Client.GetAsync("/api/v1/version", t.Ct));

            var root = version.RootElement;
            t.Equal("uic", root.GetProperty("name").GetString(), "product name is protocol-neutral");
            t.Equal("0.0.0-test", root.GetProperty("version").GetString(), "gateway version");
            t.Equal("1", root.GetProperty("apiVersion").GetString(), "api version");
            t.Equal("1", root.GetProperty("schemaVersion").GetString(), "schema version");
            t.Equal("contract-site", root.GetProperty("site").GetString(), "site");
            t.Ok(root.GetProperty("startedAt").GetString()!.EndsWith('Z'), "startedAt is UTC with Z");
            t.Ok(root.GetProperty("configVersion").GetString()!.StartsWith('v'), "config version is content-addressed");

            var metrics = await host.Client.GetAsync("/api/v1/metrics", t.Ct);
            t.Equal(HttpStatusCode.OK, metrics.StatusCode, "/api/v1/metrics status");
            t.Contains(await metrics.Content.ReadAsStringAsync(t.Ct), "# TYPE", "metrics are in Prometheus text format");
        });
    }

    /// <summary>
    /// `/api/v1/health` 的 `droppedLastHour` 必须是「最近一小时」，不是累计丢弃数。
    ///
    /// 累计值只会变大：一次磁盘打满之后接口会永久报同一个数，运维无法回答
    /// 「现在还在丢吗」。这里用真实的 Outbox（小上限触发真实丢弃）、真实的 HTTP
    /// 端点与可拨的单调时钟，而不是断言一个传进去的常量。
    /// </summary>
    private static Task HealthReportsDropsInTheLastHour(TestContext t)
    {
        return RunAsync(async () =>
        {
            var clock = new FakeClock();
            var config = WithStorage(BuildConfig(), new StorageOptions
            {
                OutboxMaxRecords = 3,
                OutboxFlushInterval = TimeSpan.FromMilliseconds(50),
            });

            await using var host = await Host.StartAsync(t, () => config, clock: clock);

            // 投给一个没有 sink 的分区名：后台 flush 不会把它推走，计数是确定的。
            for (var i = 0; i < 5; i++) host.Runtime.Outbox.Enqueue("unrouted", "x" + i);

            using var fresh = await ReadJsonAsync(t, await host.Client.GetAsync("/api/v1/health", t.Ct));
            var outbox = fresh.RootElement.GetProperty("outbox");

            t.Equal(3L, outbox.GetProperty("pending").GetInt64(), "the queue is capped at three");
            t.Equal(2L, outbox.GetProperty("droppedLastHour").GetInt64(),
                "the two drops that just happened are reported");

            // 单调时钟走一小时：窗口滑空，但累计值不变——两者的区别正是字段名的承诺。
            clock.Advance(TimeSpan.Zero, TimeSpan.FromHours(1));

            using var later = await ReadJsonAsync(t, await host.Client.GetAsync("/api/v1/health", t.Ct));
            var laterOutbox = later.RootElement.GetProperty("outbox");

            t.Equal(3L, laterOutbox.GetProperty("pending").GetInt64(), "pending is unchanged");
            t.Equal(0L, laterOutbox.GetProperty("droppedLastHour").GetInt64(),
                "drops older than an hour leave the window");
            t.Equal(2L, host.Runtime.Outbox.Stats().DroppedTotal,
                "the cumulative total still remembers the drops");
        });
    }

    private static Task ResourcePressureDegradesHealth(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 用一个必然超过真实剩余空间的阈值制造磁盘告急：比 mock 探针更接近
            // 「装配是否正确」这个问题——资源看门狗、health、journal 停止必须联动。
            var config = WithStorage(BuildConfig(), new StorageOptions
            {
                Journal = true,
                MinFreeBytes = long.MaxValue / 2,
                ResumeFreeBytes = long.MaxValue / 2 + 1,
            });

            await using var host = await Host.StartAsync(t, () => config);

            t.Ok(!host.Runtime.Outbox.Sinks.Contains("journal"),
                "journal is not registered while the disk watermark is breached");

            using var health = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/health"), t.Ct));

            var root = health.RootElement;
            t.Equal("degraded", root.GetProperty("status").GetString(),
                "health reports degraded for resource pressure");

            var resources = root.GetProperty("resources");
            t.Equal("degraded", resources.GetProperty("status").GetString(), "the resource block agrees");
            t.Equal(true, resources.GetProperty("diskLow").GetBoolean(), "diskLow is set");
            t.Equal(false, resources.GetProperty("memoryHigh").GetBoolean(), "memory is not involved");
            t.Ok(resources.GetProperty("freeBytes").ValueKind == JsonValueKind.Number,
                "the real free-space reading is exposed");

            var problems = root.GetProperty("problems");
            t.Ok(problems.EnumerateArray().Any(p =>
                    p.GetProperty("device").GetString() == "$resources"),
                "the problem list names the resource watermark");

            var metrics = await host.Client.GetStringAsync("/api/v1/metrics", t.Ct);
            t.Contains(metrics, "uic_resources_degraded 1", "the gauge reflects the degraded state");

            // 累加计数器与瞬时 gauge 是两件事：gauge 说「现在是否降级」，计数器说
            // 「一共降级过几次」。看板上的「资源告急次数」只看得到后者，写漏了
            // 就没有任何历史可查——恢复之后 gauge 归零，事故痕迹也一起消失。
            t.Contains(metrics, "uic_resource_pressure_total{kind=\"disk\"} 1",
                "the pressure counter records the disk degradation (and names the kind)");

            // 阈值撤回后必须自动恢复：看门狗只在跳变时触发，恢复路径没写对就会
            // 「降级一次、永久停写」，这正是资源降级最容易踩的坑。
            var recovered = WithStorage(config, new StorageOptions { Journal = true });
            var outcome = await host.Runtime.ApplyConfigAsync(recovered, "watermark removed", t.Ct);

            t.Ok(outcome.Applied, "the config with cleared watermarks is applied");
            t.Ok(host.Runtime.Outbox.Sinks.Contains("journal"),
                "journal is registered again after recovery");

            using var healthy = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/health"), t.Ct));

            t.Equal("ok", healthy.RootElement.GetProperty("status").GetString(),
                "health returns to ok after recovery");
            t.Equal("ok", healthy.RootElement.GetProperty("resources").GetProperty("status").GetString(),
                "the resource block returns to ok");
        });
    }

    /// <summary>
    /// 控制台「读配置 → 改一处 → 应用」的闭环：GET 会把密钥脱敏成 <c>"***"</c>，
    /// 原样提交回来时服务端必须还原，否则一次改名就会把 API 令牌写成占位符。
    /// 这里走真实 HTTP，因为错误可能只发生在解析/应用这段装配里。
    /// </summary>
    private static Task ApplyKeepsRedactedSecrets(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var envelope = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/config"), t.Ct));

            var config = envelope.RootElement.GetProperty("config").Clone();

            t.Equal(RedactedSecrets.Placeholder,
                config.GetProperty("northbound").GetProperty("rest").GetProperty("token").GetString(),
                "the read-back token is a placeholder");

            // 模拟控制台里改一台设备的名字，其余字段（含占位符）原样提交。
            var devices = config.GetProperty("devices").EnumerateArray().ToArray();
            var edited = JsonNode.Parse(config.GetRawText())!.AsObject();
            edited["devices"]![0]!["name"] = "改名后的设备";

            using var applied = await ReadJsonAsync(t, await host.Client.SendAsync(
                Post("/api/v1/config/apply", edited.ToJsonString()), t.Ct));

            var root = applied.RootElement;
            t.Equal(true, root.GetProperty("applied").GetBoolean(), "the config is applied");

            var current = host.Runtime.CurrentConfig;
            t.Equal(TokenRef, current.Northbound.Rest.Token,
                "the reference survives the redacted round-trip (the file keeps no plaintext)");
            t.Equal(Token, host.Runtime.CreateNorthboundContext().Rest.Token,
                "the reference still resolves to the real token");
            t.Equal("改名后的设备", current.Devices[0].Name, "the intended edit is applied");
            t.Equal(devices.Length, current.Devices.Count, "no device was added or dropped");
        });
    }

    /// <summary>
    /// 历史查询走真实 journal 文件：采集 → Outbox → journal sink → REST。
    /// 这条链最容易在「文件路径/刷新间隔」上错位，所以断言的是实际落盘后的结果。
    /// </summary>
    private static Task HistoryQueryReadsJournal(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 固定值不会产生新样本（缓存只在变化时发布），历史用例需要一个会变的波形。
            var source = BuildConfig();
            source.Devices[0].Labels["mode"] = "sine";

            var config = WithStorage(source, new StorageOptions
            {
                Journal = true,
                OutboxFlushInterval = TimeSpan.FromMilliseconds(50),
            });

            await using var host = await Host.StartAsync(t, () => config);

            // 先等到至少 3 条样本，才能验证 limit 与 cursor 真的分页。
            JsonElement firstPage = default;
            var deadline = DateTimeOffset.UtcNow.AddSeconds(15);

            while (DateTimeOffset.UtcNow < deadline)
            {
                using var poll = await ReadJsonAsync(t, await host.Client.SendAsync(
                    Get("/api/v1/history?device=v1&point=temp&limit=2"), t.Ct));

                if (poll.RootElement.GetProperty("total").GetInt32() >= 3)
                {
                    firstPage = poll.RootElement.Clone();
                    break;
                }

                await Task.Delay(100, t.Ct);
            }

            if (firstPage.ValueKind != JsonValueKind.Object)
                t.Fail("history stayed empty: journal writing or the query path is broken");

            var items = firstPage.GetProperty("items").EnumerateArray().ToArray();
            t.Equal(2, items.Length, "limit caps the page size");
            t.Equal("v1", items[0].GetProperty("device").GetString(), "device filter is applied");
            t.Equal("temp", items[0].GetProperty("point").GetString(), "point filter is applied");
            t.Equal("good", items[0].GetProperty("quality").GetString(), "quality survives the journal round-trip");
            t.Equal("float64", items[0].GetProperty("value").GetProperty("type").GetString(),
                "the value wire shape is unchanged");

            var cursor = firstPage.GetProperty("nextCursor").GetString();
            t.Ok(!string.IsNullOrWhiteSpace(cursor), "more samples produce a next cursor");

            using var second = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get($"/api/v1/history?device=v1&point=temp&limit=2&cursor={Uri.EscapeDataString(cursor!)}"),
                t.Ct));

            var secondItems = second.RootElement.GetProperty("items").EnumerateArray().ToArray();
            t.Ok(secondItems.Length > 0, "the cursor returns the next page");
            t.Ok(secondItems[0].GetProperty("seq").GetUInt64() !=
                 items[^1].GetProperty("seq").GetUInt64(),
                "the second page does not repeat the first page");

            // 反序时间范围是调用方错误，必须明确报错，而不是悄悄返回空。
            var bad = await host.Client.SendAsync(Get(
                "/api/v1/history?from=2026-11-07T10:00:00Z&to=2026-11-07T09:00:00Z"), t.Ct);

            t.Equal(HttpStatusCode.BadRequest, bad.StatusCode, "a reversed range is rejected");

            using var error = await ReadJsonAsync(t, bad);
            t.Equal("request.invalid_range",
                error.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error code names the range problem");

            var badTime = await host.Client.SendAsync(Get("/api/v1/history?from=not-a-time"), t.Ct);
            t.Equal(HttpStatusCode.BadRequest, badTime.StatusCode, "an unparsable timestamp is rejected");

            using var badTimeError = await ReadJsonAsync(t, badTime);
            t.Equal("request.invalid_parameter",
                badTimeError.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error code names the malformed parameter");
        });
    }

    /// <summary>
    /// 分页游标必须被严格校验：合法 base64 但内容不是非负整数、或被改写过的形式，
    /// 一律 400，不能「当成 offset=0」从第一页重来。
    ///
    /// 静默回到第一页是最糟的失败方式：HTTP 200，客户端只会看到整页重复，
    /// 或者按自己的去重逻辑跳过一页；两种都表现为「数据少了/多了」，而日志里一片正常。
    /// 游标本身是不透明串，服务端只认自己发出的那个形式。
    /// </summary>
    private static Task PaginationCursorIsValidated(TestContext t)
    {
        static string B64(string text) => Convert.ToBase64String(Encoding.UTF8.GetBytes(text));

        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            // 正常翻页：游标原样传回即可，第二页不能重复第一页。
            using var first = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/points?device=v1&limit=1"), t.Ct));

            var firstPage = first.RootElement.GetProperty("items").EnumerateArray().ToArray();
            t.Equal(1, firstPage.Length, "limit caps the page size");

            var cursor = first.RootElement.GetProperty("nextCursor").GetString();
            t.Ok(!string.IsNullOrWhiteSpace(cursor), "two points produce a next cursor");

            using var second = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get($"/api/v1/points?device=v1&limit=1&cursor={Uri.EscapeDataString(cursor!)}"), t.Ct));

            var secondPage = second.RootElement.GetProperty("items").EnumerateArray().ToArray();
            t.Equal(1, secondPage.Length, "the cursor returns the second page");
            t.Ok(secondPage[0].GetProperty("point").GetString() !=
                 firstPage[0].GetProperty("point").GetString(),
                "the second page does not repeat the first page");

            // 合法 base64、内容不是非负整数：不能当成 offset=0 从头再来。
            foreach (var text in new[] { "not-a-number", "-1", "1.5" })
            {
                var bad = await host.Client.SendAsync(Get(
                    $"/api/v1/points?device=v1&limit=1&cursor={Uri.EscapeDataString(B64(text))}"), t.Ct);

                t.Equal(HttpStatusCode.BadRequest, bad.StatusCode, $"cursor '{text}' is rejected");

                using var error = await ReadJsonAsync(t, bad);
                t.Equal("request.invalid_cursor",
                    error.RootElement.GetProperty("error").GetProperty("code").GetString(),
                    "the error code names the cursor problem");
            }

            // 游标对调用方是不透明串。能解开、但不是服务端发出的形式（这里是补零后的
            // "010"）同样拒绝：对方在自行解析/重建游标，替它猜一个偏移猜错了也是 200。
            var rewritten = await host.Client.SendAsync(Get(
                $"/api/v1/points?device=v1&limit=1&cursor={Uri.EscapeDataString("MDEw")}"), t.Ct);
            t.Equal(HttpStatusCode.BadRequest, rewritten.StatusCode,
                "a rewritten cursor that decodes to a number is rejected");
        });
    }

    private static ConfigDocument WithStorage(ConfigDocument source, StorageOptions storage) => new()
    {
        SchemaVersion = source.SchemaVersion,
        Site = source.Site,
        Description = source.Description,
        Channels = source.Channels,
        Devices = source.Devices,
        Northbound = source.Northbound,
        Logging = source.Logging,
        Storage = storage,
    };

    private static Task ApiRequiresBearerToken(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            var anonymous = await host.Client.SendAsync(Get("/api/v1/devices", auth: false), t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, anonymous.StatusCode, "no token is rejected");
            t.Ok(anonymous.Headers.WwwAuthenticate.Any(h => h.Scheme == "Bearer"),
                "WWW-Authenticate names the scheme");

            using (var envelope = await ReadJsonAsync(t, anonymous))
            {
                var error = envelope.RootElement.GetProperty("error");
                t.Equal("auth.unauthorized", error.GetProperty("code").GetString(), "error code");
                t.Ok(!string.IsNullOrWhiteSpace(error.GetProperty("requestId").GetString()), "request id is echoed");
            }

            var wrong = new HttpRequestMessage(HttpMethod.Get, "/api/v1/devices");
            wrong.Headers.Authorization = new AuthenticationHeaderValue("Bearer", "not-the-token");
            var rejected = await host.Client.SendAsync(wrong, t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, rejected.StatusCode, "a wrong token is rejected");

            var authorized = await host.Client.SendAsync(Get("/api/v1/devices"), t.Ct);
            t.Equal(HttpStatusCode.OK, authorized.StatusCode, "the right token is accepted");

            // 路由匹配不区分大小写，鉴权判定必须跟着不区分：
            // 用大小写变体绕过鉴权是扫描器的第一条试探（/API/v1/devices）。
            var upper = await host.Client.SendAsync(Get("/API/v1/devices", auth: false), t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, upper.StatusCode,
                "a case-variant path must not skip authentication");

            // 带令牌但权限不够时，大小写变体同样不能跳过权限检查。
            // 只堵住「未认证」不够：权限判定也基于同一个路径前缀。
            var upperWithToken = await host.Client.SendAsync(Get("/API/v1/devices"), t.Ct);
            t.Equal(HttpStatusCode.OK, upperWithToken.StatusCode,
                "a case-variant path with a valid token still works (routing is case-insensitive)");
        });
    }

    private static Task DevicesAndPointsAreListed(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var devices = await ReadJsonAsync(t, await host.Client.SendAsync(Get("/api/v1/devices"), t.Ct));
            var items = devices.RootElement.GetProperty("items");

            t.Equal(1, items.GetArrayLength(), "one device is listed");
            t.Equal("v1", items[0].GetProperty("id").GetString(), "device id");
            t.Equal("virtual", items[0].GetProperty("protocol").GetString(), "protocol");
            t.Equal(2, items[0].GetProperty("points").GetProperty("total").GetInt32(), "point total");
            t.Equal(1, devices.RootElement.GetProperty("total").GetInt32(), "paged total");

            // 等到虚拟设备产生第一个样本：刚启动时缓存是空的，这是真实的启动时序。
            var good = await WaitForPointAsync(host, "v1", "temp", t);
            t.Equal("good", good.GetProperty("quality").GetString(), "the point becomes good");
            t.Close(42, good.GetProperty("value").GetProperty("num").GetDouble(), 1e-9, "const waveform uses the baseline");
            t.Equal("°C", good.GetProperty("unit").GetString(), "unit comes from the config");
            t.Ok(good.GetProperty("seq").GetUInt64() > 0, "seq is assigned");

            // 质量码写错不能返回空列表：200 + `items: []` 与「设备没有点位」完全一样，
            // 调用方只能去翻配置才知道自己拼错了。
            foreach (var path in new[] { "/api/v1/points?quality=gud", "/api/v1/devices/v1/points?quality=gud" })
            {
                using var invalid = await host.Client.SendAsync(Get(path), t.Ct);
                t.Equal(HttpStatusCode.BadRequest, invalid.StatusCode,
                    $"an unknown quality code is rejected on {path}");

                using var error = await ReadJsonAsync(t, invalid);
                t.Equal("request.invalid_parameter",
                    error.RootElement.GetProperty("error").GetProperty("code").GetString(),
                    "the error names the parameter");
            }

            // 合法过滤仍然照旧：good 有值、bad 为空。
            using var filtered = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/points?quality=good"), t.Ct));
            t.Ok(filtered.RootElement.GetProperty("items").GetArrayLength() >= 1,
                "a known quality code still filters normally");
        });
    }

    /// <summary>
    /// 配置态与运行态是两件事，必须分别可见。
    ///
    /// 「设备配置里 enabled:false」不等于「设备出故障了」：前者是运维主动停采，
    /// 后者要报警、要查链路。只报一个 <c>state</c> 字段的话，这两种情况在界面上
    /// 长得一样——运维会把「我关了它」当成「它坏了」，或者反过来把真故障当成人为停用
    /// 而不去处理。所以 API 同时给出 configuredState 与 runtimeState：
    /// 停用的设备仍然出现在列表里（配置还在、点位还在，只是不采），
    /// 两个 state 都是 disabled，且不会产生样本。
    /// </summary>
    private static Task DisabledDeviceShowsBothStates(TestContext t)
    {
        return RunAsync(async () =>
        {
            var source = BuildConfig();

            // 两台设备：一台停用、一台启用。只有这样才能证明 configuredState 真的
            // 来自配置——单看一台停用设备，两个字段都是 disabled，把 configuredState
            // 写成 runtimeState 也照样"通过"。
            var template = source.Devices[0];

            // Device 是可变类而不是 record：逐字段复制，只改要改的字段。
            Device Copy(string id, bool enabled, string name) => new()
            {
                Id = id,
                Name = name,
                ChannelId = template.ChannelId,
                Enabled = enabled,
                Interval = template.Interval,
                Timeout = template.Timeout,
                Retries = template.Retries,
                UnitId = template.UnitId,
                ByteOrder = template.ByteOrder,
                FailureThreshold = template.FailureThreshold,
                RecoverInterval = template.RecoverInterval,
                Labels = template.Labels,
                Points = template.Points.Select(p => new Point
                {
                    Id = p.Id,
                    Name = p.Name,
                    Address = p.Address,
                    DataType = p.DataType,
                    Unit = p.Unit,
                }).ToList(),
            };

            var config = new ConfigDocument
            {
                SchemaVersion = source.SchemaVersion,
                Site = source.Site,
                Description = source.Description,
                Channels = source.Channels,
                Devices = new List<Device>
                {
                    Copy("v1", enabled: false, "停用设备"),
                    Copy("v2", enabled: true, "启用设备"),
                },
                Northbound = source.Northbound,
                Logging = source.Logging,
                Storage = source.Storage,
            };

            await using var host = await Host.StartAsync(t, () => config);

            using var devices = await ReadJsonAsync(t, await host.Client.SendAsync(Get("/api/v1/devices"), t.Ct));
            var items = devices.RootElement.GetProperty("items");

            t.Equal(2, items.GetArrayLength(), "a disabled device is still listed: it exists in the config");

            var byId = new Dictionary<string, JsonElement>(StringComparer.Ordinal);
            foreach (var item in items.EnumerateArray())
                byId[item.GetProperty("id").GetString()!] = item.Clone();

            t.Equal("disabled", byId["v1"].GetProperty("configuredState").GetString(),
                "configuredState reflects the config");
            t.Equal("disabled", byId["v1"].GetProperty("runtimeState").GetString(),
                "a disabled device is not collecting");

            // 启用设备的配置态是 enabled，而运行态是 Connecting/Collecting 之类：
            // 这两个字段不是同一件事，任何一边抄另一边都会在这里露出来。
            t.Equal("enabled", byId["v2"].GetProperty("configuredState").GetString(),
                "configuredState stays enabled regardless of the runtime state");
            t.Ok(byId["v2"].GetProperty("runtimeState").GetString() != "enabled",
                "runtimeState is a separate vocabulary (configured/connecting/collecting/…)");

            // 点位仍在契约里：上位机按点表建订阅，设备停用不该让点表消失
            // （那会让「先把设备停掉再改点表」的操作直接失败）。
            using var points = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/devices/v1/points"), t.Ct));
            t.Equal(2, points.RootElement.GetProperty("items").GetArrayLength(),
                "a disabled device keeps its point table");

            // 停用的设备不采集：等一会儿也拿不到 good 样本。
            await Task.Delay(600, t.Ct);

            using var point = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/points/v1/temp"), t.Ct));
            t.Ok(point.RootElement.GetProperty("quality").GetString() != "good",
                "a disabled device must not produce samples");
        });
    }

    /// <summary>
    /// 点位周期要出现在线上契约里：上位机需要知道「这个点位多久没更新算过期」。
    /// 继承设备周期时 `interval` 省略，`effectiveInterval` 始终给出解析后的值。
    /// </summary>
    private static Task PointsExposeEffectiveInterval(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 配置模型的属性是 init-only，改配置只能走「序列化 → 改 JSON → 解析」，
            // 顺带验证 `interval` 的线上写法能被读回来。
            var node = JsonNode.Parse(JsonDefaults.ToJson(BuildConfig()))!.AsObject();
            node["devices"]![0]!["points"]![0]!["interval"] = "500ms";
            var source = ConfigStore.Parse(node.ToJsonString());

            await using var host = await Host.StartAsync(t, () => source);

            using var document = await ReadJsonAsync(
                t, await host.Client.SendAsync(Get("/api/v1/points/v1/temp"), t.Ct));

            var point = document.RootElement;
            t.Equal("500ms", point.GetProperty("interval").GetString(),
                "an overridden point interval is reported");
            t.Equal("500ms", point.GetProperty("effectiveInterval").GetString(),
                "the effective interval equals the override");

            using var inherited = await ReadJsonAsync(
                t, await host.Client.SendAsync(Get("/api/v1/points/v1/flag"), t.Ct));

            var flag = inherited.RootElement;
            t.Ok(!flag.TryGetProperty("interval", out _),
                "an inherited interval is omitted rather than sent as null");
            t.Equal("200ms", flag.GetProperty("effectiveInterval").GetString(),
                "the inherited cadence is the device interval");
        });
    }

    /// <summary>
    /// 数组点位在北向契约里要同时暴露两件事：<c>count</c>（期望维度）与
    /// <c>value.array</c>（实际值）。只给其中一个都不够用：
    /// 上位机要靠 count 预分配列，靠 array 渲染；空值点位（未采集到）只有 count 可依。
    /// </summary>
    private static Task PointsExposeArrayCount(TestContext t)
    {
        return RunAsync(async () =>
        {
            var node = JsonNode.Parse(JsonDefaults.ToJson(BuildConfig()))!.AsObject();
            node["devices"]![0]!["points"]![0]!.AsObject()["count"] = 3;
            var source = ConfigStore.Parse(node.ToJsonString());

            await using var host = await Host.StartAsync(t, () => source);

            var point = await WaitForPointAsync(host, "v1", "temp", t);

            t.Equal(3, point.GetProperty("count").GetInt32(), "count is exposed on the wire");

            var array = point.GetProperty("value").GetProperty("array");
            t.Equal(3, array.GetArrayLength(), "the value carries one number per element");

            // 断言的波形各元素相同，所以这里应全部等于 baseline=42。
            for (var i = 0; i < array.GetArrayLength(); i++)
            {
                t.Close(42, array[i].GetDouble(), 1e-9, $"element {i}");
            }

            t.Ok(!point.GetProperty("value").TryGetProperty("num", out _),
                "an array value does not also carry num");

            // 标量点位的线上形态不受影响：不出现 count/array 的额外字段。
            using var scalar = await ReadJsonAsync(
                t, await host.Client.SendAsync(Get("/api/v1/points/v1/flag"), t.Ct));

            t.Equal(0, scalar.RootElement.GetProperty("count").GetInt32(),
                "a scalar point reports count 0");
            t.Ok(!scalar.RootElement.GetProperty("value").TryGetProperty("array", out _),
                "a scalar value carries no array field");
        });
    }

    /// <summary>
    /// 驱动能力是四态而不是布尔：把 unsupported 写成 false、把 runtimeDependent 写成 true
    /// 都会让控制台给出错误结论。这条用例锁住线上名字，防止序列化悄悄变回数字。
    /// </summary>
    private static Task DriversExposeCapabilityStates(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var document = await ReadJsonAsync(
                t, await host.Client.SendAsync(Get("/api/v1/drivers"), t.Ct));

            var items = document.RootElement.GetProperty("items");
            t.Equal(5, items.GetArrayLength(), "all registered drivers are listed");

            var byId = items.EnumerateArray().ToDictionary(
                d => d.GetProperty("id").GetString()!,
                d => d.Clone(),
                StringComparer.Ordinal);

            t.Ok(byId.ContainsKey("modbus-tcp") && byId.ContainsKey("modbus-rtu") &&
                 byId.ContainsKey("modbus-ascii") && byId.ContainsKey("virtual") &&
                 byId.ContainsKey("mqtt"),
                "the built-in drivers are present");

            // modbus-rtu / modbus-ascii 与 modbus-tcp 同源（能力/寻址一致），但走串口：传输是 serial、
            // 名字各异、配置字段是「串口 + 波特率」而非「主机 + 端口」。这三点此前只断言了「在场」，
            // 可控制台「驱动」页正是按它们生成表单——传输或字段名漏一项，用户就填不对、点表里选了
            // modbus-rtu 却拿到 TCP 的 host/port 表单。这里把差异点钉住。
            foreach (var (id, name) in new[] { ("modbus-rtu", "Modbus RTU"), ("modbus-ascii", "Modbus ASCII") })
            {
                var variant = byId[id];
                t.Equal(name, variant.GetProperty("name").GetString(), $"{id} name is exposed");
                t.Equal("serial", variant.GetProperty("transports")[0].GetString(),
                    $"{id} runs over a serial line, not tcp");
                t.Equal("master", variant.GetProperty("roles")[0].GetString(), $"{id} is a master");
                t.Equal("register", variant.GetProperty("addressKinds")[0].GetString(),
                    $"{id} uses register addressing");

                // 串口变体与 tcp 同源，能力声明也必须逐项一致——上面那段注释声称的「能力/寻址一致」，
                // 此前只验证了寻址与配置字段，能力四态没有。控制台「驱动」页照这份声明渲染能力，
                // 若 rtu/ascii 的位访问或写能力悄悄漂移（误标成 Unsupported），没有用例会发现。
                t.Equal("supported", Cap(variant, "read"), $"{id} can read");
                t.Equal("supported", Cap(variant, "write"), $"{id} can write");
                t.Equal("supported", Cap(variant, "multiRead"), $"{id} batches reads");
                t.Equal("supported", Cap(variant, "bitAccess"), $"{id} supports bit access");
                t.Equal("unsupported", Cap(variant, "subscribe"), $"{id} master has no subscriptions");
                t.Equal("unsupported", Cap(variant, "browse"), $"{id} has no address space to browse");
                t.Equal("unsupported", Cap(variant, "discovery"), $"{id} has no discovery");
                t.Equal("unsupported", Cap(variant, "structTypes"), $"{id} has no structured types");

                var fields = variant.GetProperty("configFields").EnumerateArray()
                    .ToDictionary(f => f.GetProperty("key").GetString()!, f => f.Clone(), StringComparer.Ordinal);
                t.Equal(2, fields.Count, $"{id} declares a serial port and a baud rate");
                t.Equal("串口", fields["transport.port"].GetProperty("label").GetString(),
                    $"{id} names the port field after a serial port");
                t.Equal("string", fields["transport.port"].GetProperty("type").GetString(),
                    $"{id} serial port is a device path, not a number");
                t.Equal("int", fields["transport.baud"].GetProperty("type").GetString(),
                    $"{id} declares the baud rate");
                t.Ok(fields.ContainsKey("transport.host") is false,
                    $"{id} has no tcp host field");
            }

            var modbus = byId["modbus-tcp"];
            t.Equal("1.0", modbus.GetProperty("sdkVersion").GetString(), "sdk version is exposed");
            t.Equal("supported", Cap(modbus, "read"), "modbus can read");
            t.Equal("supported", Cap(modbus, "write"), "modbus can write");
            t.Equal("supported", Cap(modbus, "multiRead"), "modbus batches reads");
            t.Equal("supported", Cap(modbus, "bitAccess"), "modbus supports bit access");
            t.Equal("unsupported", Cap(modbus, "subscribe"), "modbus master has no subscriptions");
            t.Equal("unsupported", Cap(modbus, "browse"), "modbus has no address space to browse");

            t.Equal("master", modbus.GetProperty("roles")[0].GetString(), "modbus is a master");
            t.Equal("register", modbus.GetProperty("addressKinds")[0].GetString(), "modbus uses register addressing");

            var virtualDriver = byId["virtual"];
            t.Equal("supported", Cap(virtualDriver, "subscribe"), "the virtual device can push changes");
            t.Equal("supported", Cap(virtualDriver, "browse"), "the virtual device exposes symbols");
            t.Equal("unsupported", Cap(virtualDriver, "discovery"), "discovery stays unsupported");
            // virtual 声明的读/写/批量/位访问此前也没有任何用例钉住——它和 tcp 一样是 Supported，
            // 但 behavioral 用例（写后回读等）不读这份声明，所以声明若漂移到 Unsupported 无人发现。
            t.Equal("supported", Cap(virtualDriver, "read"), "the virtual device can read");
            t.Equal("supported", Cap(virtualDriver, "write"), "the virtual device accepts writes");
            t.Equal("supported", Cap(virtualDriver, "multiRead"), "the virtual device batches reads");
            t.Equal("supported", Cap(virtualDriver, "bitAccess"), "the virtual device supports bit access");

            var mqtt = byId["mqtt"];
            t.Equal("supported", Cap(mqtt, "read"), "mqtt can read from its subscription cache");
            t.Equal("unsupported", Cap(mqtt, "write"), "the mqtt southbound driver is read-only");
            t.Equal("supported", Cap(mqtt, "subscribe"), "mqtt is subscription based");
            t.Equal("symbol", mqtt.GetProperty("addressKinds")[0].GetString(),
                "mqtt addresses points by topic (symbol addressing)");

            // 表单元数据必须原样透出：控制台与接入方都靠它知道某个协议要填什么，
            // 各自再抄一份协议手册迟早与驱动实现分叉。
            var tcpFields = modbus.GetProperty("configFields").EnumerateArray()
                .ToDictionary(f => f.GetProperty("key").GetString()!, f => f.Clone(), StringComparer.Ordinal);

            t.Equal(2, tcpFields.Count, "modbus-tcp declares host and port");
            t.Equal("主机", tcpFields["transport.host"].GetProperty("label").GetString(), "label is exposed");
            t.Equal("string", tcpFields["transport.host"].GetProperty("type").GetString(), "type is exposed");
            t.Ok(tcpFields["transport.host"].GetProperty("required").GetBoolean(), "host is required");
            t.Equal("channel", tcpFields["transport.host"].GetProperty("scope").GetString(), "scope is exposed");
            t.Equal(502, tcpFields["transport.port"].GetProperty("default").GetInt32(), "default is exposed");
            t.Ok(!modbus.TryGetProperty("pointFields", out _),
                "a driver that declares no point fields omits the list instead of sending an empty one");

            var virtualFields = virtualDriver.GetProperty("configFields").EnumerateArray()
                .ToDictionary(f => f.GetProperty("key").GetString()!, f => f.Clone(), StringComparer.Ordinal);
            var fault = virtualFields["labels.fault"];
            t.Equal("none", fault.GetProperty("default").GetString(), "virtual defaults to no fault");
            t.Ok(fault.GetProperty("enum").EnumerateArray().Select(e => e.GetString()).Contains("timeout"),
                "enum values are exposed, not flattened into help text");

            var mqttPointFields = mqtt.GetProperty("pointFields").EnumerateArray()
                .ToDictionary(f => f.GetProperty("key").GetString()!, f => f.Clone(), StringComparer.Ordinal);
            t.Ok(mqttPointFields["address.path"].GetProperty("required").GetBoolean(),
                "the topic filter is a required point field");
            // address.property（取值字段）此前只有行为用例证明它生效（DriverUsesPointFieldOverride），
            // 但「声明本身存在且标成可选」从未被钉住——它和 address.path 一样由前端按这份元数据生成输入项，
            // 一旦这条声明被删或误标成 required，控制台表单就少填/多填一个框，没有用例会发现。
            // 它必须保持 optional：留空时回退到设备级 payloadField，强制必填会把合法的「用默认字段」写法挡掉。
            t.Ok(mqttPointFields.ContainsKey("address.property"),
                "the value-field point is declared so the form renders an input for it");
            t.Ok(!mqttPointFields["address.property"].TryGetProperty("required", out var propRequired)
                     || !propRequired.GetBoolean(),
                "the value field stays optional: blank falls back to the device payloadField");
            t.Equal("point", mqttPointFields["address.property"].GetProperty("scope").GetString(),
                "the value field is scoped to the point, not the device");

            static string? Cap(JsonElement driver, string name)
                => driver.GetProperty("capabilities").GetProperty(name).GetString();
        });
    }

    private static async Task<JsonElement> WaitForPointAsync(Host host, string device, string point, TestContext t)
    {
        var deadline = Stopwatch.StartNew();

        JsonElement last = default;

        while (deadline.Elapsed < TimeSpan.FromSeconds(10))
        {
            using var document = await ReadJsonAsync(
                t, await host.Client.SendAsync(Get($"/api/v1/points/{device}/{point}"), t.Ct));

            last = document.RootElement.Clone();

            if (last.GetProperty("quality").GetString() == "good") return last;

            await Task.Delay(100, t.Ct);
        }

        t.Fail($"{device}/{point} never reached good quality (last quality: " +
               $"{(last.ValueKind == JsonValueKind.Undefined ? "none" : last.GetProperty("quality").GetString())})");

        return last;
    }

    private static Task UnknownDeviceReturnsEnvelope(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var response = await host.Client.SendAsync(Get("/api/v1/devices/nope/points"), t.Ct);
            t.Equal(HttpStatusCode.NotFound, response.StatusCode, "unknown device is a 404");

            using var envelope = await ReadJsonAsync(t, response);
            var error = envelope.RootElement.GetProperty("error");

            // 错误信封是跨语言的契约：code 稳定、message 可读、requestId 可追踪。
            t.Equal("device.not_found", error.GetProperty("code").GetString(), "stable error code");
            t.Contains(error.GetProperty("message").GetString() ?? string.Empty, "nope", "message names the device");
        });
    }

    private static Task UnknownPointReturnsErrorEnvelope(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            // 设备存在、点位不存在：和「设备本身不存在」是两件事，运维要从 code 一眼分清
            // 是「点ID 写错了」还是「整台设备没注册」。这条 404（point.not_found）在错误表里有据，
            // 但此前只有 device.not_found 被测；FindPoint 抛的 point 分支无人断言，删掉它或挪走
            // 都会让未知点位冒成 500。默认 Host 配置的设备 v1 只有 temp/flag 两个点。
            var response = await host.Client.SendAsync(Get("/api/v1/points/v1/nope"), t.Ct);
            t.Equal(HttpStatusCode.NotFound, response.StatusCode, "unknown point is a 404");

            using var envelope = await ReadJsonAsync(t, response);
            var error = envelope.RootElement.GetProperty("error");

            t.Equal("point.not_found", error.GetProperty("code").GetString(), "stable error code");
            t.Contains(error.GetProperty("message").GetString() ?? string.Empty, "nope", "message names the point");
        });
    }

    private static Task ConfigIsRedacted(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var document = await ReadJsonAsync(t, await host.Client.SendAsync(Get("/api/v1/config"), t.Ct));
            var root = document.RootElement;

            t.Ok(root.GetProperty("version").GetString()!.StartsWith('v'), "version is content-addressed");

            var config = root.GetProperty("config");
            var token = config.GetProperty("northbound").GetProperty("rest").GetProperty("token").GetString();

            // 配置本身要能看，口令类字段必须看不到：现场投屏时这是硬要求。
            t.Equal("***", token, "the bearer token is redacted");
            t.Equal("contract-site", config.GetProperty("site").GetString(), "the rest of the config is readable");

            // 差异接口会展示字段值；轮换令牌后，前后值都不能是明文。
            var from = root.GetProperty("version").GetString()!;
            var edited = JsonNode.Parse(config.GetRawText())!.AsObject();
            edited["northbound"]!["rest"]!["token"] = "${env:UIC_CONTRACT_ROTATED_TOKEN}";

            Environment.SetEnvironmentVariable("UIC_CONTRACT_ROTATED_TOKEN", "rotated-token");

            using var rotated = await ReadJsonAsync(t, await host.Client.SendAsync(
                Post("/api/v1/config/apply", edited.ToJsonString()), t.Ct));
            t.Equal(true, rotated.RootElement.GetProperty("applied").GetBoolean(), "the rotated reference applies");
            t.Ok(!rotated.RootElement.GetRawText().Contains("rotated-token", StringComparison.Ordinal),
                "the response never contains the plaintext token");

            using var diff = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get($"/api/v1/config/diff?from={from}"), t.Ct));

            var tokenEntry = diff.RootElement.GetProperty("entries").EnumerateArray()
                .Single(e => e.GetProperty("path").GetString() == "northbound.rest.token");

            t.Equal("***", tokenEntry.GetProperty("before").GetString(), "the old token is not leaked");
            t.Equal("***", tokenEntry.GetProperty("after").GetString(), "the new token is not leaked");
        });
    }

    private static Task ConfigValidateReportsProblems(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            // 合法配置。
            var good = "{\"config\": " + JsonDefaults.ToJson(BuildConfig()) + "}";
            using (var ok = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Post("/api/v1/config/validate", good), t.Ct)))
            {
                t.Equal(true, ok.RootElement.GetProperty("valid").GetBoolean(), "the current config validates");
                t.Equal(0, ok.RootElement.GetProperty("problems").GetArrayLength(), "no problems");
            }

            // 明显非法的配置：空站点 + 指向不存在通道的设备。
            const string bad = """
                               {
                                 "config": {
                                   "schemaVersion": 1,
                                   "site": "",
                                   "channels": [],
                                   "devices": [
                                     { "id": "d1", "channelId": "missing", "points": [] }
                                   ]
                                 }
                               }
                               """;

            using (var invalid = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Post("/api/v1/config/validate", bad), t.Ct)))
            {
                t.Equal(false, invalid.RootElement.GetProperty("valid").GetBoolean(), "an invalid config is rejected");
                t.Ok(invalid.RootElement.GetProperty("problems").GetArrayLength() > 0, "problems are enumerated");
            }

            // 语法错误走的是另一条路径（JSON 解析），必须是 422 而不是 500。
            var broken = await host.Client.SendAsync(Post("/api/v1/config/validate", "{ nope"), t.Ct);
            t.Equal((HttpStatusCode)422, broken.StatusCode, "malformed JSON is an unprocessable entity");
        });
    }

    /// <summary>
    /// v0 旧字段（server.listen 等）写成非字符串 JSON（数字 / 对象）时，配置接口
    /// 绝不能冒成 500。
    ///
    /// 迁移链里这些字段原本用 <c>GetValue&lt;string&gt;()</c> 直接读，遇到非字符串抛
    /// 裸 <see cref="InvalidOperationException"/>；而 REST 只按 <see cref="ConfigException"/>
    /// 分类「配置非法」，裸异常会一路冒到最外层兜底 catch 变成 500。500 专指网关自身
    /// 故障——把一份客户端配置错误渲染成网关崩溃，会误触发告警、污染错误看板，和把
    /// 写请求体语法错误冒成 500 是同一类问题。
    ///
    /// 两个端点的正确表现不同：<c>config/validate</c> 的产出就是「问题清单」，语义非法
    /// 的配置返回 200 + <c>valid:false</c>（只要不是崩溃）；<c>config/apply</c> 遇配置
    /// 非法抛 <see cref="ConfigException"/>，由中间件渲染成 422。两者都必须避开 500。
    /// 单元层（config/migration_rejects_non_string_legacy_fields）另钉异常类型是
    /// ConfigException 而非 InvalidOperationException。
    /// </summary>
    private static Task MalformedLegacyFieldIsNotAServerFault(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            // server.listen 写成数字：迁移时读字符串会炸。
            var numericListen = """
                                {
                                  "config": {
                                    "schemaVersion": 1,
                                    "site": "plant-a",
                                    "server": { "listen": 9090 }
                                  }
                                }
                                """;

            // validate：配置语义非法 → 200 + valid:false（列出问题），绝不 500。
            var validate = await host.Client.SendAsync(
                Post("/api/v1/config/validate", numericListen), t.Ct);
            t.Ok(validate.StatusCode != HttpStatusCode.InternalServerError,
                "validate must never surface a client config error as a server fault");
            var body = await ReadJsonAsync(t, validate);
            t.Equal(false, body.RootElement.GetProperty("valid").GetBoolean(),
                "a non-string legacy field is reported as invalid, not accepted");
            t.Ok(body.RootElement.GetProperty("problems").GetArrayLength() > 0,
                "the problems list names what is wrong");

            // apply：配置非法 → 422 config.invalid，绝不 500。
            var apply = await host.Client.SendAsync(
                Post("/api/v1/config/apply", numericListen), t.Ct);
            t.Equal((HttpStatusCode)422, apply.StatusCode,
                "apply rejects the malformed field with a 422, not a 500");
            var applyBody = await ReadJsonAsync(t, apply);
            t.Equal("config.invalid",
                applyBody.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error code names the invalid config");
        });
    }

    /// <summary>
    /// 读接口必须把**每一个**凭据字段都替换成占位符，不能只认字面叫 `password` 的那个。
    ///
    /// `Secrets` 认定的凭据有三个：`rest.token`、`mqtt.password`、
    /// `rest.tls.certificatePassword`。脱敏原本按 key 名精确匹配
    /// `password`/`token`/`secret`，而 PFX 口令的 key 是 `certificatePassword`，
    /// 于是它原样出现在 `GET /api/v1/config` 的响应里——运维投屏、诊断包、
    /// 浏览器缓存都会带着它，而文档写的是「读接口统一显示 `***`」。
    ///
    /// 更要命的是它同时骗过了 `RedactedSecrets.Restore`：客户端读到明文再原样提交，
    /// 就不需要「还原」，明文被直接写进配置版本。所以这一条得从读侧堵死。
    /// 断言用「三个字段一起看」而不是逐个单测：只补 `certificatePassword` 一个特例，
    /// 下次新增凭据字段还会漏，而这条用例会跟着 `Secrets` 的清单一起提醒。
    /// </summary>
    private static Task EveryCredentialFieldIsRedacted(TestContext t)
    {
        return RunAsync(async () =>
        {
            const string pfxVariable = "UIC_CONTRACT_PFX_PASSWORD";
            const string mqttVariable = "UIC_CONTRACT_MQTT_PASSWORD";
            const string pfxSecret = "pfx-super-secret";
            const string mqttSecret = "mqtt-super-secret";

            Environment.SetEnvironmentVariable(pfxVariable, pfxSecret);
            Environment.SetEnvironmentVariable(mqttVariable, mqttSecret);

            ConfigDocument WithCredentials()
            {
                var doc = BuildConfig();

                return new ConfigDocument
                {
                    SchemaVersion = doc.SchemaVersion,
                    Site = doc.Site,
                    Description = doc.Description,
                    Channels = doc.Channels,
                    Devices = doc.Devices,
                    Northbound = new NorthboundOptions
                    {
                        Rest = doc.Northbound.Rest with
                        {
                            Tls = new RestTlsOptions
                            {
                                // 只填引用、不真的开 TLS：这条用例验的是「读出来的形状」，
                                // 与能不能握手无关，开 TLS 会额外引入证书依赖。
                                Enabled = false,
                                CertificateFile = "server.pfx",
                                CertificatePassword = "${env:" + pfxVariable + "}",
                            },
                        },
                        Mqtt = doc.Northbound.Mqtt with
                        {
                            Password = "${env:" + mqttVariable + "}",
                            Username = "gateway",
                        },
                        OpcUa = doc.Northbound.OpcUa,
                    },
                    Storage = doc.Storage,
                    Logging = doc.Logging,
                };
            }

            await using var host = await Host.StartAsync(t, WithCredentials);

            var response = await host.Client.SendAsync(Get("/api/v1/config"), t.Ct);
            var raw = await response.Content.ReadAsStringAsync(t.Ct);

            t.Equal(HttpStatusCode.OK, response.StatusCode, "the config is readable");

            // 明文无论如何都不能出现在响应体里，这是最直接的那条断言。
            t.Ok(!raw.Contains(pfxSecret, StringComparison.Ordinal),
                "the PFX password must never appear in the config response");
            t.Ok(!raw.Contains(mqttSecret, StringComparison.Ordinal),
                "the MQTT password must never appear in the config response");

            using var document = JsonDocument.Parse(raw);
            var tls = document.RootElement.GetProperty("config")
                .GetProperty("northbound").GetProperty("rest").GetProperty("tls");

            t.Equal("***", tls.GetProperty("certificatePassword").GetString(),
                "certificatePassword is a credential, so it is redacted like the others");

            // 证书路径不是凭据，不能被顺手抹掉：抹掉了客户端就没法把配置改一处再提交。
            t.Equal("server.pfx", tls.GetProperty("certificateFile").GetString(),
                "the certificate path stays readable");

            // diff 会展示字段值，凭据字段同样只能看到占位符。
            var version = document.RootElement.GetProperty("version").GetString()!;
            var edited = JsonNode.Parse(document.RootElement.GetProperty("config").GetRawText())!.AsObject();
            edited["northbound"]!["rest"]!["tls"]!["certificatePassword"] = "${env:UIC_CONTRACT_PFX_ROTATED}";
            Environment.SetEnvironmentVariable("UIC_CONTRACT_PFX_ROTATED", "pfx-rotated-secret");

            using (var applied = await ReadJsonAsync(t, await host.Client.SendAsync(
                       Post("/api/v1/config/apply", edited.ToJsonString()), t.Ct)))
            {
                t.Equal(true, applied.RootElement.GetProperty("applied").GetBoolean(),
                    "the rotated reference applies");
                t.Ok(!applied.RootElement.GetRawText().Contains("pfx-rotated-secret", StringComparison.Ordinal),
                    "the apply response never carries the plaintext");
            }

            using var diff = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get($"/api/v1/config/diff?from={version}"), t.Ct));

            var entry = diff.RootElement.GetProperty("entries").EnumerateArray()
                .Single(e => e.GetProperty("path").GetString()
                             == "northbound.rest.tls.certificatePassword");

            t.Equal("***", entry.GetProperty("before").GetString(),
                "the previous PFX password is not leaked through diff");
            t.Equal("***", entry.GetProperty("after").GetString(),
                "the new PFX password is not leaked through diff");
        });
    }

    /// <summary>
    /// 明文凭据在 apply 与 validate 两处都要被拒（FR-OPS-09 的红线在 API 上同样成立）。
    /// 422 + 带字段路径的问题清单，调用方才知道该改哪个字段。
    /// </summary>
    private static Task PlaintextCredentialsAreRejected(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var plaintext = JsonDocument.Parse(JsonDefaults.ToJson(BuildConfig()));
            var edited = JsonNode.Parse(plaintext.RootElement.GetRawText())!.AsObject();
            edited["northbound"]!["rest"]!["token"] = "hunter2";

            var body = "{\"config\": " + edited.ToJsonString() + "}";

            using (var validated = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Post("/api/v1/config/validate", body), t.Ct)))
            {
                t.Equal(false, validated.RootElement.GetProperty("valid").GetBoolean(),
                    "validate refuses a plaintext token");
                t.Ok(validated.RootElement.GetProperty("problems").EnumerateArray()
                        .Any(p => p.GetString()!.Contains("northbound.rest.token", StringComparison.Ordinal)),
                    "the problem names the credential field");
            }

            var applied = await host.Client.SendAsync(Post("/api/v1/config/apply", body), t.Ct);
            t.Equal((HttpStatusCode)422, applied.StatusCode, "apply refuses it before touching the runtime");

            using var error = JsonDocument.Parse(await applied.Content.ReadAsStringAsync(t.Ct));
            t.Equal("config.invalid", error.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error uses the config envelope");
            t.Ok(error.RootElement.GetProperty("error").GetProperty("details").EnumerateArray()
                    .Any(d => d.GetProperty("path").GetString() == "northbound.rest.token"),
                "the error points at the token field");

            // 失败的 apply 不能留下任何副作用：设备与令牌都还是原来的。
            t.Equal("${env:" + TokenVariable + "}",
                host.Runtime.CurrentConfig.Northbound.Rest.Token,
                "the stored configuration is unchanged");
        });
    }

    /// <summary>
    /// 引用了未注册驱动的配置必须在 apply 时整份被拒：422 + 字段路径，而不是 500。
    ///
    /// 配置校验只要求 <c>protocol</c> 非空（它不依赖驱动注册表），而调度器是
    /// 「先卸旧设备、再建新设备」，驱动是否已注册要到建新设备时才检查。驱动名写错时
    /// 前一台设备已经被拆掉、新的建不出来，接口只回一个 500，配置版本还没变——
    /// 运行态与配置从此不一致，运维只能重启进程。
    /// </summary>
    private static Task ApplyRefusesUnregisteredDriver(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            var versionBefore = host.Runtime.ConfigVersion;

            using var envelope = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/config"), t.Ct));

            var edited = JsonNode.Parse(
                envelope.RootElement.GetProperty("config").GetRawText())!.AsObject();
            edited["channels"]![0]!["protocol"] = "virtualx";

            var refused = await host.Client.SendAsync(
                Post("/api/v1/config/apply", edited.ToJsonString()), t.Ct);

            t.Equal((HttpStatusCode)422, refused.StatusCode,
                "an unregistered driver is a config error, not an internal error");

            using var error = JsonDocument.Parse(await refused.Content.ReadAsStringAsync(t.Ct));
            t.Equal("config.invalid",
                error.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error uses the config envelope");
            t.Ok(error.RootElement.GetProperty("error").GetProperty("details").EnumerateArray()
                    .Any(d => d.GetProperty("path").GetString() == "channels[virtual-bus].protocol"),
                "the error points at the channel protocol");

            // 被拒的 apply 不能有副作用：配置、版本与被拆的设备都还是原来的。
            t.Equal("virtual", host.Runtime.CurrentConfig.Channels[0].Protocol,
                "the stored configuration is unchanged");
            t.Equal(versionBefore, host.Runtime.ConfigVersion, "no config version was committed");

            using var devices = await ReadJsonAsync(t, await host.Client.SendAsync(
                Get("/api/v1/devices"), t.Ct));
            t.Equal(1, devices.RootElement.GetProperty("items").GetArrayLength(),
                "the running device was not torn down");

            // 采集没被停在半路：被拒之后老配置仍然出数，seq 继续增长。
            // 只看 quality=good 不够——那可能是 apply 之前缓存的最后一帧。
            var before = await WaitForPointAsync(host, "v1", "temp", t);
            var beforeSeq = before.GetProperty("seq").GetUInt64();

            var afterSeq = beforeSeq;
            var spin = Stopwatch.StartNew();
            while (afterSeq <= beforeSeq && spin.Elapsed < TimeSpan.FromSeconds(10))
            {
                using var point = await ReadJsonAsync(t, await host.Client.SendAsync(
                    Get("/api/v1/points/v1/temp"), t.Ct));
                afterSeq = point.RootElement.GetProperty("seq").GetUInt64();
                if (afterSeq > beforeSeq) break;
                await Task.Delay(100, t.Ct);
            }

            t.Ok(afterSeq > beforeSeq, "the device keeps collecting after the refused apply");
        });
    }

    private static Task WriteReturnsPerPointResults(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            await WaitForPointAsync(host, "v1", "temp", t);

            var body = """
                       {
                         "value": { "type": "float64", "num": 77.5 },
                         "confirm": true
                       }
                       """;

            var response = await host.Client.SendAsync(Post("/api/v1/points/v1/temp/write", body), t.Ct);
            t.Equal(HttpStatusCode.OK, response.StatusCode, "write is accepted");

            using var document = await ReadJsonAsync(t, response);
            var results = document.RootElement.GetProperty("results");

            // 逐项返回：一个点位失败不能让整批 500，前端要能逐条显示裁决。
            t.Equal(1, results.GetArrayLength(), "one result");

            var result = results[0];
            t.Equal("temp", result.GetProperty("point").GetString(), "result is attributed to the point");
            t.Ok(result.TryGetProperty("ok", out _), "result carries an ok flag");
            t.Ok(result.TryGetProperty("quality", out _), "result carries a quality");
            t.Ok(result.TryGetProperty("appliedAt", out var appliedAt) &&
                 appliedAt.GetString()!.EndsWith('Z'), "appliedAt is UTC with Z");

            // 只读点位必须在网关层被拒，而不是发到设备上再失败。
            var readOnly = await host.Client.SendAsync(Post("/api/v1/points/nope/temp/write", body), t.Ct);
            t.Equal(HttpStatusCode.NotFound, readOnly.StatusCode, "unknown device is a 404");
        });
    }

    /// <summary>
    /// 写入值必须是工程量，与读方向共用同一组 scale/offset。
    ///
    /// 读方向做了「原始值 × scale + offset」，写方向不做反算的话，同一个点位
    /// 「写 10、读回 1」——现场只会以为设备不接受设定值；更糟的是 confirm 回读
    /// 比的是原始值，会给出 verified=true，把错误的写入伪装成成功。
    /// </summary>
    private static Task WriteValuesAreEngineeringUnits(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 配置模型是 init-only，改配置走「序列化 → 改 JSON → 解析」，
            // 顺带验证 scale/offset 的线上写法能被读回来。
            var node = JsonNode.Parse(JsonDefaults.ToJson(BuildConfig()))!.AsObject();
            var points = node["devices"]![0]!["points"]!.AsArray();
            points[0]!["scale"] = 0.1;
            points.Add(JsonNode.Parse("""
                {
                  "id": "level", "name": "液位",
                  "address": { "area": "holding", "offset": 20 },
                  "dataType": "uint16", "scale": 0.5, "offset": 1
                }
                """)!);

            await using var host = await Host.StartAsync(t, () => ConfigStore.Parse(node.ToJsonString()));

            var initial = await WaitForPointAsync(host, "v1", "temp", t);
            t.Close(4.2, initial.GetProperty("value").GetProperty("num").GetDouble(), 1e-9,
                "read side scales the raw baseline");

            const string body = """
                                {
                                  "writes": [
                                    { "point": "temp",  "value": { "type": "float64", "num": 10 }, "confirm": true },
                                    { "point": "level", "value": { "type": "uint16",  "num": 11 }, "confirm": true }
                                  ]
                                }
                                """;

            var response = await host.Client.SendAsync(Post("/api/v1/points/v1/write", body), t.Ct);
            t.Equal(HttpStatusCode.OK, response.StatusCode, "batch write is accepted");

            using (var document = await ReadJsonAsync(t, response))
            {
                foreach (var result in document.RootElement.GetProperty("results").EnumerateArray())
                {
                    t.Equal(true, result.GetProperty("ok").GetBoolean(),
                        $"{result.GetProperty("point").GetString()} writes in engineering units");
                    t.Equal(true, result.GetProperty("verified").GetBoolean(),
                        $"{result.GetProperty("point").GetString()} readback matches the requested value");
                }
            }

            // 下一轮采集就能看到设备里的原始值被读方向重新换算回工程量。
            t.Close(10, await WaitForNumberAsync(host, "v1", "temp", 10, t), 1e-9,
                "a float point round-trips its engineering value");
            t.Close(11, await WaitForNumberAsync(host, "v1", "level", 11, t), 1e-9,
                "an integer point applies scale and offset the other way round");
        });
    }

    private static async Task<double> WaitForNumberAsync(
        Host host, string device, string point, double expected, TestContext t)
    {
        var deadline = Stopwatch.StartNew();
        var last = double.NaN;

        while (deadline.Elapsed < TimeSpan.FromSeconds(10))
        {
            using var document = await ReadJsonAsync(
                t, await host.Client.SendAsync(Get($"/api/v1/points/{device}/{point}"), t.Ct));

            var value = document.RootElement.GetProperty("value");
            last = value.GetProperty("num").GetDouble();

            if (Math.Abs(last - expected) < 1e-9) return last;

            await Task.Delay(100, t.Ct);
        }

        t.Fail($"{device}/{point} never settled on {expected} (last value: {last})");
        return last;
    }

    /// <summary>
    /// 单点写入也必须支持幂等键：现场重试最多的是单点，而不是 100 点的批量。
    /// 第二次请求不能再去碰设备，响应要明确告诉调用方这是重放。
    /// </summary>
    private static Task SinglePointWriteIsIdempotent(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            await WaitForPointAsync(host, "v1", "temp", t);

            const string body = """
                                { "value": { "type": "float64", "num": 88.5 } }
                                """;

            var first = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, "single-1"), t.Ct);

            t.Equal(HttpStatusCode.OK, first.StatusCode, "the first write succeeds");
            t.Equal("false", first.Headers.GetValues("X-Idempotent-Replay").Single(),
                "the first response is not a replay");

            var firstBody = await first.Content.ReadAsStringAsync(t.Ct);

            var second = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, "single-1"), t.Ct);

            t.Equal(HttpStatusCode.OK, second.StatusCode, "the repeat is accepted");
            t.Equal("true", second.Headers.GetValues("X-Idempotent-Replay").Single(),
                "the repeat is marked as a replay");
            t.Equal(firstBody, await second.Content.ReadAsStringAsync(t.Ct),
                "the cached response is returned verbatim");
        });
    }

    /// <summary>
    /// 写接口的请求体由客户端拼装，格式写坏是常态：多一个逗号、少一个引号、截断的报文。
    /// 这类「请求本身不成立」必须返回 400，绝不能冒成 500。
    ///
    /// 500 在运维那里专指「网关自己出了故障」，会触发告警、进错误看板、被当成发布事故；
    /// 而这里网关一切正常，只是收到了一段非法 JSON。配置导入那条路径早就把语法错误
    /// 归成 422/客户端错误并写了断言，写接口却把 JsonException 漏到了最外层的兜底
    /// catch，被当成未处理异常返回 500——同一套网关对两种请求体的处理标准不一致。
    /// </summary>
    private static Task MalformedWriteBodyIsAClientError(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            await WaitForPointAsync(host, "v1", "temp", t);

            // 单点接口：截断的 JSON。
            var single = await host.Client.SendAsync(
                Post("/api/v1/points/v1/temp/write", "{ \"value\": { "), t.Ct);
            t.Equal(HttpStatusCode.BadRequest, single.StatusCode,
                "a syntactically broken body is a 400, not a 500");
            var singleBody = await ReadJsonAsync(t, single);
            t.Equal("request.invalid_json",
                singleBody.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error code names the malformed body");

            // 批量接口同样不能 500。
            var batch = await host.Client.SendAsync(
                Post("/api/v1/points/v1/write", "{ not json"), t.Ct);
            t.Equal(HttpStatusCode.BadRequest, batch.StatusCode,
                "the batch endpoint also rejects a broken body with a 400");

            // 关键断言：绝不能被兜底 catch 变成 500。500 会把一次客户端格式错误
            // 误报成网关故障，污染告警与错误看板。
            t.Ok(single.StatusCode != HttpStatusCode.InternalServerError,
                "a malformed body must never surface as a server fault");
        });
    }

    /// <summary>
    /// 写入幂等窗口必须用单调时钟算，绝不能用墙钟。
    ///
    /// 它回答的是「这次重投离第一次写入过了多久」。用墙钟算，一次 NTP 校时前跳一小时
    /// 就会让客户端重投的同一个 key 被当成新请求，对设备把同一个物理动作再执行一遍
    /// ——现场最怕的正是这种重复动作；后跳则让缓存永不过期，一次真正的新写入会被
    /// 当成重放而根本不执行。
    /// </summary>
    private static Task IdempotencyWindowIgnoresAClockJump(TestContext t)
    {
        return RunAsync(async () =>
        {
            var clock = new FakeClock();
            await using var host = await Host.StartAsync(t, clock: clock);
            await WaitForPointAsync(host, "v1", "temp", t);

            const string body = """
                                { "value": { "type": "float64", "num": 12.5 } }
                                """;
            const string key = "clock-jump-write-1";

            var first = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, key), t.Ct);
            t.Equal("false", first.Headers.GetValues("X-Idempotent-Replay").Single(),
                "the first write is executed");

            // 只拨墙钟一小时、单调时钟不动：真实上只过了几毫秒，重投必须仍被判成重放。
            // 否则客户端那次超时重试会真的再写一次设备。
            clock.Advance(TimeSpan.FromHours(1), TimeSpan.Zero);

            var afterJump = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, key), t.Ct);
            t.Equal(HttpStatusCode.OK, afterJump.StatusCode, "the repeat after a clock jump is accepted");
            t.Equal("true", afterJump.Headers.GetValues("X-Idempotent-Replay").Single(),
                "a wall-clock jump must not make a retry look like a new write");

            // 单调时间真的越过窗口，这时同一 key 才应该被当作新请求重新执行。
            clock.Advance(TimeSpan.Zero, TimeSpan.FromMinutes(6));

            var afterWindow = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, key), t.Ct);
            t.Equal("false", afterWindow.Headers.GetValues("X-Idempotent-Replay").Single(),
                "past the real window the same key is executed again");
        });
    }

    /// <summary>
    /// 幂等台账必须随运行时实例走，不能是进程级静态字典。
    ///
    /// 台账里记的是「首次写入时的单调时刻」，只能与**同一个**单调时钟相减。
    /// 静态字典会让同一进程里新起的运行时读到上一个运行时的条目：两个时钟读数
    /// 相减是负数，`elapsed &lt; window` 永远成立，于是重投被判成重放、返回的是
    /// 另一台网关、另一批设备的首次响应——而设备一次都没被写过。
    /// 现场症状是「换完配置点写按钮没反应」，而日志、审计、接口返回值全都正常。
    ///
    /// 这里刻意让两个运行时的单调时钟相隔一天（模拟前一实例被替换后新实例刚启动，
    /// 或用例/嵌入式场景在同一个宿主进程里先后起两套运行时），并让第二套使用
    /// 同名的设备与点位——这正是静态字典最容易串味的情形。
    /// </summary>
    private static Task IdempotencyLedgerIsPerRuntime(TestContext t)
    {
        return RunAsync(async () =>
        {
            const string body = """
                                { "value": { "type": "float64", "num": 71.5 } }
                                """;
            const string key = "shared-ledger-key";

            // 第一套运行时：单调时钟已经跑了一小时（进程里先起的那个实例）。
            var early = new FakeClock();
            early.Advance(TimeSpan.Zero, TimeSpan.FromHours(1));

            await using (var first = await Host.StartAsync(t, clock: early))
            {
                await WaitForPointAsync(first, "v1", "temp", t);

                var accepted = await first.Client.SendAsync(
                    PostWithKey("/api/v1/points/v1/temp/write", body, key), t.Ct);

                t.Equal(HttpStatusCode.OK, accepted.StatusCode, "the first runtime accepts the write");
                t.Equal("false", accepted.Headers.GetValues("X-Idempotent-Replay").Single(),
                    "the first runtime executes the write");
            }

            // 第二套运行时的单调时钟刚从 0 起算（真实场景里两套运行时的单调基准
            // 本来就毫无关系，新的那个通常更小）。进程级字典会拿它们相减。
            var later = new FakeClock();

            await using var second = await Host.StartAsync(t, clock: later, dataDir: t.TempDir + "-second");
            await WaitForPointAsync(second, "v1", "temp", t);

            var fresh = await second.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, key), t.Ct);

            t.Equal(HttpStatusCode.OK, fresh.StatusCode, "the second runtime accepts the write");
            t.Equal("false", fresh.Headers.GetValues("X-Idempotent-Replay").Single(),
                "a key used against another runtime must not be treated as a replay");

            // 第二套自己的重投仍然要被判成重放：修的是串台，不是把幂等关掉。
            var repeat = await second.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", body, key), t.Ct);

            t.Equal("true", repeat.Headers.GetValues("X-Idempotent-Replay").Single(),
                "the second runtime still replays its own repeats");
        });
    }

    /// <summary>
    /// 同一个幂等键配另一份请求体必须报冲突（409），不能把首次结果重放出来。
    ///
    /// 重放的话，调用方拿到的是 <c>ok: true</c> / <c>verified: true</c>，
    /// 会以为新值已经写进设备——而那次写入从未发生。这类「静默丢写 + 假成功」
    /// 在现场极难追查：日志、审计、接口返回值三者都说写成功了。
    /// 键被复用到另一个请求上通常是调用方拼接 id 的 bug，必须让它立刻暴露。
    /// </summary>
    private static Task IdempotencyKeyRejectsADifferentBody(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            await WaitForPointAsync(host, "v1", "temp", t);

            const string key = "conflict-1";
            const string first = """
                                 { "value": { "type": "float64", "num": 33.5 }, "confirm": true }
                                 """;
            const string second = """
                                  { "value": { "type": "float64", "num": 44.5 }, "confirm": true }
                                  """;

            var accepted = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", first, key), t.Ct);

            t.Equal(HttpStatusCode.OK, accepted.StatusCode, "the first write succeeds");
            t.Equal("false", accepted.Headers.GetValues("X-Idempotent-Replay").Single(),
                "the first response is not a replay");

            // 原样重投仍然允许（这是幂等键存在的意义）。
            var repeat = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", first, key), t.Ct);

            t.Equal(HttpStatusCode.OK, repeat.StatusCode, "an identical retry is still accepted");
            t.Equal("true", repeat.Headers.GetValues("X-Idempotent-Replay").Single(),
                "an identical retry is a replay");

            // 换了值却复用同一个键：必须拒绝，且不能把 33.5 的结果当成 44.5 的答复。
            var conflict = await host.Client.SendAsync(
                PostWithKey("/api/v1/points/v1/temp/write", second, key), t.Ct);

            t.Equal(HttpStatusCode.Conflict, conflict.StatusCode,
                "the same key with a different body is a conflict");

            using var error = await ReadJsonAsync(t, conflict);
            t.Equal("request.idempotency_conflict",
                error.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error code says why the write was refused");

            t.Ok(conflict.Headers.Contains("X-Idempotent-Replay") == false,
                "a conflict is not reported as a replay");

            // 设备上的值必须还是第一次写入的那个：冲突的请求没有下发任何写入。
            t.Close(33.5, await WaitForNumberAsync(host, "v1", "temp", 33.5, t), 1e-9,
                "the refused write never reached the device");
        });
    }

    /// <summary>
    /// 模板实例化：先预览，再应用；批量 index 自动注入；重复 ID 拒绝。
    /// </summary>
    private static Task TemplateInstantiatesAndApplies(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            const string template = """
                                    {
                                      "schemaVersion": 1,
                                      "id": "virtual-meter",
                                      "name": "虚拟表计",
                                      "protocol": "virtual",
                                      "parameters": [
                                        { "name": "index", "type": "int", "required": true },
                                        { "name": "base", "type": "int", "default": "0" },
                                        { "name": "amplitude", "type": "string", "default": "10" }
                                      ],
                                      "device": {
                                        "id": "meter-{index}",
                                        "channelId": "virtual-bus",
                                        "interval": "1s",
                                        "labels": { "mode": "const", "amplitude": "{amplitude}", "baseline": "7" },
                                        "points": [
                                          {
                                            "id": "voltage",
                                            "address": { "kind": "register", "area": "holding", "offset": "{base}" },
                                            "dataType": "float64",
                                            "unit": "V"
                                          }
                                        ]
                                      }
                                    }
                                    """;

            var created = await host.Client.SendAsync(Post("/api/v1/templates", template), t.Ct);
            t.Equal(HttpStatusCode.Created, created.StatusCode, "the template is stored");

            const string previewBody = """
                                       {
                                         "instances": [
                                           { "base": "10" },
                                           { "base": "20" }
                                         ],
                                         "apply": false
                                       }
                                       """;

            var preview = await host.Client.SendAsync(
                Post("/api/v1/templates/virtual-meter/instantiate", previewBody), t.Ct);

            t.Equal(HttpStatusCode.OK, preview.StatusCode, "the preview succeeds");

            using (var document = await ReadJsonAsync(t, preview))
            {
                var devices = document.RootElement.GetProperty("devices");
                t.Equal(2, devices.GetArrayLength(), "two devices are previewed");
                t.Equal("meter-1", devices[0].GetProperty("id").GetString(), "index is injected");
                t.Equal("meter-2", devices[1].GetProperty("id").GetString(), "the second index is injected");
                t.Equal(10, devices[0].GetProperty("points")[0]
                    .GetProperty("address").GetProperty("offset").GetInt32(),
                    "a numeric placeholder becomes a JSON number");
                t.Equal("virtual-meter", devices[0].GetProperty("templateId").GetString(),
                    "the instance records its template");
            }

            var applied = await host.Client.SendAsync(
                Post("/api/v1/templates/virtual-meter/instantiate",
                    previewBody.Replace("\"apply\": false", "\"apply\": true")), t.Ct);

            t.Equal(HttpStatusCode.OK, applied.StatusCode, "the apply succeeds");

            using (var document = await ReadJsonAsync(t, applied))
            {
                t.Equal(true, document.RootElement.GetProperty("applied").GetBoolean(), "config was applied");
                t.Ok(document.RootElement.GetProperty("version").GetString()!.StartsWith('v'),
                    "a new version is produced");
            }

            var devicesNow = await host.Client.SendAsync(Get("/api/v1/devices?limit=100"), t.Ct);
            using (var document = await ReadJsonAsync(t, devicesNow))
            {
                t.Equal(3, document.RootElement.GetProperty("total").GetInt32(),
                    "the two instances join the existing device");
            }

            // 再应用一次必须被拒：ID 冲突是现场最常见的模板误用。
            var duplicate = await host.Client.SendAsync(
                Post("/api/v1/templates/virtual-meter/instantiate",
                    previewBody.Replace("\"apply\": false", "\"apply\": true")), t.Ct);

            t.Equal(HttpStatusCode.UnprocessableEntity, duplicate.StatusCode,
                "duplicate device ids are rejected");

            // 模板实例化会改设备表，必须在审计里点名到具体设备与模板；
            // 预览（apply=false）不改配置，不该留痕。重复 apply 在进入审计包装之前
            // 就被校验拒绝（422），因此也不产生记录——这里的 1 条正好同时证明这两件事。
            using (var audit = await ReadJsonAsync(t,
                       await host.Client.SendAsync(
                           Get("/api/v1/audit?action=config.template_instantiate"), t.Ct)))
            {
                t.Equal(1, audit.RootElement.GetProperty("total").GetInt32(),
                    "only the applied run is recorded; the preview and the rejected duplicate are not");

                var entry = audit.RootElement.GetProperty("items")[0];
                t.Equal("ok", entry.GetProperty("result").GetString(), "the applied run is recorded as ok");

                var detail = entry.GetProperty("detail");
                t.Equal("virtual-meter", detail.GetProperty("template").GetString(),
                    "the record names the template");
                t.Ok(detail.GetProperty("devices").EnumerateArray()
                        .Select(d => d.GetString()).OrderBy(d => d, StringComparer.Ordinal)
                        .SequenceEqual(new[] { "meter-1", "meter-2" }),
                    "the record names every device it created");
                t.Ok(entry.GetProperty("target").GetString()!.StartsWith('v'),
                    "the record points at the version it produced");
            }
        });
    }

    /// <summary>CSV 导入默认只预览；确认后才应用，并且按 id 合并而不是整表替换。</summary>
    private static Task PointsCsvImportPreviewsAndApplies(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            const string csv = """
                               id,name,address,dataType,unit,writable
                               extra,扩展点,40003,uint16,V,true
                               temp2,备用温度,holding:10,float32,degC,true
                               """;

            var preview = await host.Client.SendAsync(
                Post("/api/v1/devices/v1/points/import", JsonSerializer.Serialize(new { csv })), t.Ct);

            t.Equal(HttpStatusCode.OK, preview.StatusCode, "the preview succeeds");

            using (var document = await ReadJsonAsync(t, preview))
            {
                t.Equal(true, document.RootElement.GetProperty("valid").GetBoolean(), "the csv is valid");
                t.Equal(false, document.RootElement.GetProperty("applied").GetBoolean(),
                    "dry-run does not change the config");
                t.Equal(2, document.RootElement.GetProperty("added").GetArrayLength(), "both rows are new");
                t.Equal(2, document.RootElement.GetProperty("points").GetArrayLength(), "two points are previewed");
            }

            var applied = await host.Client.SendAsync(
                Post("/api/v1/devices/v1/points/import?dryRun=false",
                    JsonSerializer.Serialize(new { csv })), t.Ct);

            t.Equal(HttpStatusCode.OK, applied.StatusCode, "the import applies");

            using (var document = await ReadJsonAsync(t, applied))
            {
                t.Equal(true, document.RootElement.GetProperty("applied").GetBoolean(), "config was applied");
                t.Ok(document.RootElement.GetProperty("version").GetString()!.StartsWith('v'),
                    "a new version is produced");
            }

            var points = await host.Client.SendAsync(Get("/api/v1/points?device=v1&limit=100"), t.Ct);
            using (var document = await ReadJsonAsync(t, points))
            {
                t.Equal(4, document.RootElement.GetProperty("total").GetInt32(),
                    "the two imported points join the two existing points");
            }

            // 坏地址必须在预览阶段就报出来，并且带上行号。
            var bad = await host.Client.SendAsync(
                Post("/api/v1/devices/v1/points/import",
                    JsonSerializer.Serialize(new { csv = "id,address\nbroken,nonsense" })), t.Ct);

            using (var document = await ReadJsonAsync(t, bad))
            {
                t.Equal(false, document.RootElement.GetProperty("valid").GetBoolean(), "the bad row is rejected");
                t.Equal(1, document.RootElement.GetProperty("problems").GetArrayLength(), "one problem is reported");
                t.Contains(document.RootElement.GetProperty("problems")[0].GetProperty("path").GetString()!,
                    "line 2", "the problem points at the offending line");
            }

            // 点表导入是批量改动点位，审计要能回答「给哪台设备加了几个点」。
            // 预览不改配置、坏行被拒也不改配置，两者都不该留痕。
            using (var audit = await ReadJsonAsync(t,
                       await host.Client.SendAsync(
                           Get("/api/v1/audit?action=config.points_import"), t.Ct)))
            {
                t.Equal(1, audit.RootElement.GetProperty("total").GetInt32(),
                    "only the applied import is recorded; preview and rejected rows are not");

                var entry = audit.RootElement.GetProperty("items")[0];
                t.Equal("v1", entry.GetProperty("target").GetString(),
                    "the record names the device whose point table changed");
                t.Equal(4, entry.GetProperty("detail").GetProperty("total").GetInt32(),
                    "the record counts the point table after the import");
            }
        });
    }

    /// <summary>导出 → 导入必须能回到同一个配置版本内容。</summary>
    private static Task ConfigExportImportRoundTrip(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var export = await host.Client.SendAsync(Get("/api/v1/config/export?redact=false"), t.Ct);
            t.Equal(HttpStatusCode.OK, export.StatusCode, "the export succeeds");

            using var document = await ReadJsonAsync(t, export);
            var exported = document.RootElement.GetProperty("config");
            t.Equal(false, document.RootElement.GetProperty("redacted").GetBoolean(),
                "secrets are included when explicitly requested");

            var importBody = JsonSerializer.Serialize(new
            {
                config = JsonSerializer.Deserialize<JsonElement>(exported.GetRawText()),
                comment = "round trip",
            });

            var imported = await host.Client.SendAsync(
                Post("/api/v1/config/import?apply=true", importBody), t.Ct);

            t.Equal(HttpStatusCode.OK, imported.StatusCode, "the import succeeds");

            using (var result = await ReadJsonAsync(t, imported))
            {
                t.Equal(true, result.RootElement.GetProperty("valid").GetBoolean(), "the imported config is valid");
                t.Equal(true, result.RootElement.GetProperty("applied").GetBoolean(), "the import is applied");
                t.Ok(result.RootElement.GetProperty("version").GetString()!.StartsWith('v'),
                    "the import produces a new version");
            }

            var redactedExport = await host.Client.SendAsync(Get("/api/v1/config/export"), t.Ct);
            using (var result = await ReadJsonAsync(t, redactedExport))
            {
                t.Equal(true, result.RootElement.GetProperty("redacted").GetBoolean(),
                    "exports are redacted by default");
                t.Equal("***",
                    result.RootElement.GetProperty("config").GetProperty("northbound")
                        .GetProperty("rest").GetProperty("token").GetString(),
                    "the bearer token never leaves the gateway by default");
            }

            // 未脱敏导出会把凭据引用带出机器，必须留痕；默认的脱敏导出不写这条。
            // 没有这条断言，「谁把凭据带走了」就只能靠人记得去读代码。
            using (var audit = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Get("/api/v1/audit?action=config.export"), t.Ct)))
            {
                t.Equal(1, audit.RootElement.GetProperty("total").GetInt32(),
                    "only the unredacted export is recorded; the default one is not");
                t.Equal("config",
                    audit.RootElement.GetProperty("items")[0].GetProperty("target").GetString(),
                    "the export record points at the whole configuration");
            }

            // 导入会整体替换配置，是最需要留痕的一条：谁、从哪儿、换成哪个版本。
            using (var audit = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Get("/api/v1/audit?action=config.import"), t.Ct)))
            {
                t.Equal(1, audit.RootElement.GetProperty("total").GetInt32(),
                    "the applied import is recorded exactly once");
            }
        });
    }

    /// <summary>旧版配置在导入时就地升级，并且把升级项列出来——静默改写比拒绝更危险。</summary>
    private static Task ConfigImportMigratesLegacy(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            const string legacy = """
                                  {
                                    "schemaVersion": 1,
                                    "site": "contract-site",
                                    "channels": [
                                      { "id": "virtual-bus", "protocol": "virtual", "transport": "none" }
                                    ],
                                    "devices": [
                                      {
                                        "id": "legacy-1",
                                        "channelId": "virtual-bus",
                                        "points": [
                                          {
                                            "id": "value",
                                            "address": { "kind": "register", "area": "holding", "offset": 0 },
                                            "dataType": "float64"
                                          }
                                        ]
                                      }
                                    ],
                                    "server": { "listen": "0.0.0.0:9090" },
                                    "northbound": {
                                      "mqtt": {
                                        "enabled": true,
                                        "broker": "tcp://broker.local:1883",
                                        "topic": "uic/{site}/{device}/{point}/value"
                                      }
                                    }
                                  }
                                  """;

            var imported = await host.Client.SendAsync(
                Post("/api/v1/config/import", JsonSerializer.Serialize(new
                {
                    config = JsonSerializer.Deserialize<JsonElement>(legacy),
                })), t.Ct);

            t.Equal(HttpStatusCode.OK, imported.StatusCode, "the legacy config is accepted");

            using var document = await ReadJsonAsync(t, imported);
            var root = document.RootElement;

            t.Equal(false, root.GetProperty("applied").GetBoolean(), "import without ?apply=true only previews");
            t.Equal(true, root.GetProperty("valid").GetBoolean(), "the migrated config is valid");

            var migrations = root.GetProperty("migrations");
            t.Ok(migrations.GetArrayLength() >= 2, "the two legacy shapes are reported");

            var config = root.GetProperty("config");
            t.Equal("0.0.0.0:9090",
                config.GetProperty("northbound").GetProperty("rest").GetProperty("listen").GetString(),
                "server.listen is moved under northbound.rest");
            t.Equal("broker.local",
                config.GetProperty("northbound").GetProperty("mqtt").GetProperty("host").GetString(),
                "the broker URL is split into host");
            t.Equal(1883,
                config.GetProperty("northbound").GetProperty("mqtt").GetProperty("port").GetInt32(),
                "the broker URL is split into port");
            t.Ok(!config.TryGetProperty("server", out _), "the legacy section is removed");
        });
    }

    /// <summary>
    /// 控制台「新建设备」的完整链路：读脱敏配置 → 追加通道与设备 → 校验 → 应用。
    ///
    /// 这里刻意按页面真实的调用顺序走 HTTP，而不是直接调用 <c>ApplyConfigAsync</c>：
    /// 页面拼的是 JSON 文本，任何一处字段名或形状的偏差都只会在这一步暴露。
    /// </summary>
    private static Task ConsoleDeviceCreationFlow(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var current = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/config"), t.Ct));

            // 页面拿到的就是这份脱敏配置：口令是 "***"。
            var root = JsonNode.Parse(current.RootElement.GetProperty("config").GetRawText())!.AsObject();
            t.Equal("***", root["northbound"]?["rest"]?["token"]?.ToString(), "the token is redacted");

            t.Ok(ConfigDraft.AddChannel(root,
                ConfigDraft.NewChannel("console-bus", null, "virtual")) is null,
                "the console appends a channel");

            t.Ok(ConfigDraft.AddDevice(root,
                ConfigDraft.NewDevice("console-1", "控制台建的设备", "console-bus",
                    TimeSpan.FromSeconds(1), 1,
                    ConfigDraft.NewRegisterPoint("value", "值", "float64", "holding", 0))) is null,
                "the console appends a device");

            var text = root.ToJsonString(JsonDefaults.Indented);

            using (var validation = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Post("/api/v1/config/validate", text), t.Ct)))
            {
                t.Equal(true, validation.RootElement.GetProperty("valid").GetBoolean(),
                    "the drafted configuration validates: " +
                    string.Join("; ", validation.RootElement.GetProperty("problems")
                        .EnumerateArray().Select(p => p.GetString())));
            }

            var applied = await host.Client.SendAsync(Post("/api/v1/config/apply", text), t.Ct);
            t.Equal(HttpStatusCode.OK, applied.StatusCode, "the drafted configuration applies");

            using (var outcome = await ReadJsonAsync(t, applied))
            {
                t.Equal(true, outcome.RootElement.GetProperty("applied").GetBoolean(), "the config is applied");
                t.Equal(1, outcome.RootElement.GetProperty("changed").GetProperty("devicesAdded").GetInt32(),
                    "the device is reported as added");
                t.Ok(outcome.RootElement.GetProperty("version").GetString()!.StartsWith('v'),
                    "a new version is committed");
            }

            // 设备真的起来了：列表里能看到它，点位也能读到值。
            using (var devices = await ReadJsonAsync(t,
                       await host.Client.SendAsync(Get("/api/v1/devices"), t.Ct)))
            {
                var created = devices.RootElement.GetProperty("items").EnumerateArray()
                    .SingleOrDefault(d => d.GetProperty("id").GetString() == "console-1");

                t.Ok(created.ValueKind == JsonValueKind.Object, "the new device is listed");
                t.Equal("console-bus", created.GetProperty("channel").GetString(), "it sits on the new channel");
            }

            await WaitForPointAsync(host, "console-1", "value", t);

            // 脱敏占位符必须被换回原来的引用：否则下一次重启就把 "***" 当凭据了。
            // GET /config 永远会再脱敏一次，所以直接看生效的配置对象。
            t.Equal(TokenRef, host.Runtime.CurrentConfig.Northbound.Rest.Token,
                "the redacted placeholder is restored to the reference before the config is stored");
            t.Equal(Token, host.Runtime.CreateNorthboundContext().Rest.Token,
                "the effective token is still the resolved value");
            t.Ok(host.Runtime.CurrentConfig.Devices.Any(d => d.Id == "console-1"),
                "the new device is part of the effective configuration");
        });
    }

    /// <summary>
    /// 审计的主链路：一次点位写入 + 一次配置生效，都要能在 /audit 里按动作查回来，
    /// 并且带上能追到人的字段（actor / source / remote / requestId）。
    ///
    /// 断言走真实 HTTP：审计是在中间件与处理器里写的，只测 <see cref="Uic.Obs.AuditLog"/>
    /// 会漏掉「端点忘了记」这类最常见的问题。
    /// </summary>
    private static Task AuditRecordsWriteAndConfig(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            await WaitForPointAsync(host, "v1", "temp", t);

            const string body = """
                                { "value": { "type": "float64", "num": 77.5 }, "confirm": true }
                                """;

            var write = await host.Client.SendAsync(Post("/api/v1/points/v1/temp/write", body), t.Ct);
            t.Equal(HttpStatusCode.OK, write.StatusCode, "the write is accepted");

            var writeRequestId = write.Headers.GetValues("X-Request-Id").Single();

            // 改一处设备名再提交，验证 config.apply 同样留痕。
            using var read = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/config"), t.Ct));

            var edited = JsonNode.Parse(read.RootElement.GetProperty("config").GetRawText())!.AsObject();
            edited["devices"]![0]!["name"] = "审计用例改名";

            var apply = await host.Client.SendAsync(
                Post("/api/v1/config/apply", edited.ToJsonString()), t.Ct);
            t.Equal(HttpStatusCode.OK, apply.StatusCode, "the config applies");

            // 请求级失败的写入（设备不存在）同样要留痕：它往往是一次配置错误的现场。
            var rejected = await host.Client.SendAsync(
                Post("/api/v1/points/nope/temp/write", body), t.Ct);
            t.Equal(HttpStatusCode.NotFound, rejected.StatusCode, "writing to an unknown device is rejected");

            string version;
            using (var applied = await ReadJsonAsync(t, apply))
                version = applied.RootElement.GetProperty("version").GetString()!;

            using var audit = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/audit?limit=100"), t.Ct));

            var root = audit.RootElement;
            t.Ok(root.GetProperty("total").GetInt32() >= 2, "both actions are recorded");

            var entries = root.GetProperty("items").EnumerateArray()
                .Select(e => e.Clone()).ToArray();

            var pointWrite = entries.SingleOrDefault(e =>
                e.GetProperty("action").GetString() == "point.write" &&
                e.GetProperty("target").GetString() == "v1/temp");

            t.Ok(pointWrite.ValueKind == JsonValueKind.Object, "the point write is audited");
            t.Equal("ok", pointWrite.GetProperty("result").GetString(), "a successful write is recorded as ok");
            t.Equal("api-token", pointWrite.GetProperty("actor").GetString(),
                "the actor is the static API token, not a made-up user");
            t.Equal("rest", pointWrite.GetProperty("source").GetString(), "the source says where the action came from");
            t.Equal("v1/temp", pointWrite.GetProperty("target").GetString(), "the target names the point");
            t.Equal(writeRequestId, pointWrite.GetProperty("requestId").GetString(),
                "the audit row is linked to the request id in the response");

            var remote = pointWrite.GetProperty("remote").GetString();
            t.Ok(remote is not null && remote.Contains("127.0.0.1"), $"the remote address is recorded, got {remote}");

            // 写入的审计要能回放「从什么改成了什么」——这是它区别于日志的核心。
            var detail = pointWrite.GetProperty("detail");
            t.Equal("float64", detail.GetProperty("value").GetProperty("type").GetString(),
                "the written value is recorded with its type");
            t.Close(77.5, detail.GetProperty("value").GetProperty("num").GetDouble(), 1e-9,
                "the written value is recorded");
            t.Ok(detail.TryGetProperty("oldValue", out var oldValue) &&
                 oldValue.ValueKind == JsonValueKind.Object,
                "the previous value is captured before the write");
            t.Equal(true, detail.GetProperty("confirm").GetBoolean(), "the confirm flag is recorded");

            // 走向不存在设备的写入没有逐条结果，但请求级失败也要留痕。
            var requestFailure = entries.Single(e =>
                e.GetProperty("action").GetString() == "point.write" &&
                e.GetProperty("target").GetString() == "nope");

            t.Equal("failed", requestFailure.GetProperty("result").GetString(),
                "a write that failed before reaching any point is recorded as failed");
            t.Equal("temp", requestFailure.GetProperty("detail").GetProperty("points")[0].GetString(),
                "the audit names the points the caller tried to write");

            var configApply = entries.SingleOrDefault(e =>
                e.GetProperty("action").GetString() == "config.apply");

            t.Ok(configApply.ValueKind == JsonValueKind.Object, "the config apply is audited");
            t.Equal("ok", configApply.GetProperty("result").GetString(), "the apply succeeded");
            t.Equal(version, configApply.GetProperty("target").GetString(),
                "the audit names the version that was committed");
            t.Ok(configApply.GetProperty("detail").TryGetProperty("changes", out _),
                "the applied diff is recorded");
        });
    }

    /// <summary>
    /// 导出的契约：NDJSON 附件（一行一条），且导出动作本身也要留下一条审计。
    ///
    /// 只实现查询而不记录导出，等于允许把审计悄悄搬走——那正是审计要防的事。
    /// </summary>
    private static Task AuditExportIsNdjson(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            await WaitForPointAsync(host, "v1", "temp", t);

            const string body = """
                                { "value": { "type": "float64", "num": 66.5 } }
                                """;

            var write = await host.Client.SendAsync(Post("/api/v1/points/v1/temp/write", body), t.Ct);
            t.Equal(HttpStatusCode.OK, write.StatusCode, "the write is accepted");

            var export = await host.Client.SendAsync(
                Get("/api/v1/audit/export?action=point.write"), t.Ct);
            t.Equal(HttpStatusCode.OK, export.StatusCode, "the export is served");

            t.Equal("application/x-ndjson", export.Content.Headers.ContentType?.MediaType,
                "the export is served as NDJSON");

            var disposition = export.Content.Headers.ContentDisposition;
            t.Ok(disposition is not null && disposition.DispositionType == "attachment",
                "the export is an attachment");
            t.Ok(disposition?.FileName?.Trim('"').EndsWith(".ndjson") == true,
                $"the file name ends with .ndjson, got {disposition?.FileName}");

            var text = await export.Content.ReadAsStringAsync(t.Ct);
            var lines = text.Split('\n', StringSplitOptions.RemoveEmptyEntries);

            t.Ok(lines.Length >= 1, "the export contains at least the write we just did");

            foreach (var line in lines)
            {
                using var parsed = JsonDocument.Parse(line);
                t.Equal("point.write", parsed.RootElement.GetProperty("action").GetString(),
                    "every exported line matches the filter");
            }

            // 导出本身要留痕：审计文件离开机器这件事和内容同等重要。
            using var exported = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/audit?action=audit.export"), t.Ct));

            t.Equal(1, exported.RootElement.GetProperty("total").GetInt32(),
                "the export itself is recorded");
        });
    }

    /// <summary>
    /// 被拒绝的请求要留痕，但同源 IP 的连续 401 不能在 30 秒内刷满磁盘。
    ///
    /// 审计在这里回答的是「谁在被拒绝」，不是「对方试了多少次」——次数由指标去数。
    /// </summary>
    private static Task AuditDeniesAreRecorded(TestContext t)
    {
        // 去重窗口是进程级静态状态；前面的用例可能已经打过 401。
        RestApi.ResetAuthFailureThrottleForTests();

        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var denied = await host.Client.SendAsync(Get("/api/v1/devices", auth: false), t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, denied.StatusCode, "the anonymous request is rejected");

            // 紧接着再来一次：同一 IP 在窗口内，不应产生第二条。
            var again = await host.Client.SendAsync(Get("/api/v1/devices", auth: false), t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, again.StatusCode, "the repeat is rejected too");

            using var audit = await ReadJsonAsync(t,
                await host.Client.SendAsync(Get("/api/v1/audit?action=auth.failed"), t.Ct));

            var root = audit.RootElement;
            t.Equal(1, root.GetProperty("total").GetInt32(),
                "repeated denials from one address are deduplicated within the window");

            var entry = root.GetProperty("items")[0];
            t.Equal("denied", entry.GetProperty("result").GetString(), "a denial is recorded as denied");
            t.Equal("anonymous", entry.GetProperty("actor").GetString(),
                "there is no trustworthy actor before authentication");
            t.Equal("/api/v1/devices", entry.GetProperty("target").GetString(),
                "the denied path is recorded");

            var detail = entry.GetProperty("detail");
            t.Equal("GET", detail.GetProperty("method").GetString(), "the method is recorded");
            t.Equal(false, detail.GetProperty("hasToken").GetBoolean(),
                "the record says the caller did not present a token");

            // 不能把令牌本身写进审计。
            t.Ok(!entry.GetRawText().Contains(Token, StringComparison.Ordinal),
                "the audit record never contains the credential");
        });
    }


    // ------------------------------------------------------------------
    // 认证夹具
    // ------------------------------------------------------------------

    private static HttpRequestMessage GetWith(string path, string? token)
    {
        var request = new HttpRequestMessage(HttpMethod.Get, path);
        if (!string.IsNullOrEmpty(token))
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);

        return request;
    }

    private static HttpRequestMessage PostWith(string path, string json, string? token)
    {
        var request = new HttpRequestMessage(HttpMethod.Post, path)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        };

        if (!string.IsNullOrEmpty(token))
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);

        return request;
    }

    private static HttpRequestMessage PutWith(string path, string json, string? token)
    {
        var request = new HttpRequestMessage(HttpMethod.Put, path)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        };

        if (!string.IsNullOrEmpty(token))
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);

        return request;
    }

    private static HttpRequestMessage DeleteWith(string path, string? token)
    {
        var request = new HttpRequestMessage(HttpMethod.Delete, path);
        if (!string.IsNullOrEmpty(token))
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);

        return request;
    }

    private static string AdminInitialPassword(Host host)
    {
        var path = host.Runtime.Auth.Users.InitialPasswordPath
                   ?? throw new CheckFailedException("the runtime did not write an initial password file");

        var line = File.ReadAllLines(path)
            .FirstOrDefault(l => l.StartsWith("password:", StringComparison.Ordinal));

        return line is null
            ? throw new CheckFailedException("the initial password file has no password line")
            : line["password:".Length..].Trim();
    }

    private static async Task<(string Token, JsonElement User)> LoginAsync(
        TestContext t, Host host, string username, string password)
    {
        var body = JsonSerializer.Serialize(new { username, password });
        var response = await host.Client.SendAsync(PostWith("/api/v1/auth/login", body, null), t.Ct);
        t.Equal(HttpStatusCode.OK, response.StatusCode, $"login as '{username}' succeeds");

        using var document = await ReadJsonAsync(t, response);
        var root = document.RootElement.Clone();

        return (root.GetProperty("token").GetString()!, root.GetProperty("user").Clone());
    }

    private static async Task<string> ChangePasswordAsync(
        TestContext t, Host host, string token, string current, string next)
    {
        var body = JsonSerializer.Serialize(new { currentPassword = current, newPassword = next });
        var response = await host.Client.SendAsync(PostWith("/api/v1/auth/password", body, token), t.Ct);
        t.Equal(HttpStatusCode.OK, response.StatusCode, "the password change succeeds");

        using var document = await ReadJsonAsync(t, response);
        return document.RootElement.GetProperty("token").GetString()!;
    }

    /// <summary>建一个已知口令的账号并完成首次改密，返回可直接使用的会话 Token。</summary>
    private static async Task<string> SignInAsAsync(
        TestContext t, Host host, string id, string role, string password)
    {
        var create = JsonSerializer.Serialize(new { id, role, password });
        var created = await host.Client.SendAsync(PostWith("/api/v1/users", create, Token), t.Ct);
        t.Equal(HttpStatusCode.OK, created.StatusCode, $"user '{id}' is created");

        var (token, user) = await LoginAsync(t, host, id, password);
        t.Equal(true, user.GetProperty("mustChangePassword").GetBoolean(),
            "a newly created account must change its password");

        return await ChangePasswordAsync(t, host, token, password, password + "-changed");
    }

    /// <summary>
    /// 登录 → 强制改密 → 用新口令正常工作。这是现场第一次开机时的完整路径。
    /// </summary>
    private static Task AuthLoginForcesPasswordChange(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var initial = AdminInitialPassword(host);
            var (token, user) = await LoginAsync(t, host, "admin", initial);

            t.Equal("admin", user.GetProperty("role").GetString(), "the bootstrap user is an admin");
            t.Equal(true, user.GetProperty("mustChangePassword").GetBoolean(),
                "the bootstrap password is flagged as temporary");
            t.Ok(!initial.Contains("admin", StringComparison.OrdinalIgnoreCase),
                "the generated password is not a default like the user name");

            var blocked = await host.Client.SendAsync(GetWith("/api/v1/devices", token), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, blocked.StatusCode,
                "before changing the password, reading devices is forbidden");

            using (var error = await ReadJsonAsync(t, blocked))
            {
                t.Equal("auth.password_change_required", error.RootElement.GetProperty("error")
                    .GetProperty("code").GetString(), "the error code says why");
            }

            var next = "admin-password-1";
            var newToken = await ChangePasswordAsync(t, host, token, initial, next);

            var old = await host.Client.SendAsync(GetWith("/api/v1/devices", token), t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, old.StatusCode,
                "the old session is revoked when the password changes");

            var allowed = await host.Client.SendAsync(GetWith("/api/v1/devices", newToken), t.Ct);
            t.Equal(HttpStatusCode.OK, allowed.StatusCode, "the new session works");

            var (_, again) = await LoginAsync(t, host, "admin", next);
            t.Equal(false, again.GetProperty("mustChangePassword").GetBoolean(),
                "the forced-change flag is cleared");

            using var me = await ReadJsonAsync(t,
                await host.Client.SendAsync(GetWith("/api/v1/auth/me", newToken), t.Ct));
            t.Equal("session", me.RootElement.GetProperty("kind").GetString(),
                "a logged-in user is a session, not the static token");
        });
    }

    private static Task AuthRolesAreEnforced(TestContext t)
    {
        RestApi.ResetDenialThrottleForTests();

        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            await WaitForPointAsync(host, "v1", "temp", t);

            var viewer = await SignInAsAsync(t, host, "viewer1", "viewer", "viewer-password-1");
            var auditor = await SignInAsAsync(t, host, "auditor1", "auditor", "auditor-password-1");

            var read = await host.Client.SendAsync(GetWith("/api/v1/devices", viewer), t.Ct);
            t.Equal(HttpStatusCode.OK, read.StatusCode, "a viewer can read devices");

            var write = await host.Client.SendAsync(
                PostWith("/api/v1/points/v1/temp/write",
                    """{"value":{"type":"float64","num":10},"confirm":true}""", viewer), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, write.StatusCode, "a viewer cannot write points");

            var apply = await host.Client.SendAsync(
                PostWith("/api/v1/config/apply", """{"json":"{}"}""", viewer), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, apply.StatusCode, "a viewer cannot apply configuration");

            var command = await host.Client.SendAsync(
                PostWith("/api/v1/commands",
                    """{"action":"driver.restart","params":{"device":"v1"}}""", viewer), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, command.StatusCode,
                "a viewer cannot restart a driver even though the route is readable");

            var audit = await host.Client.SendAsync(GetWith("/api/v1/audit", viewer), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, audit.StatusCode, "a viewer cannot read the audit log");

            var auditorAudit = await host.Client.SendAsync(GetWith("/api/v1/audit", auditor), t.Ct);
            t.Equal(HttpStatusCode.OK, auditorAudit.StatusCode, "an auditor can read the audit log");

            var auditorWrite = await host.Client.SendAsync(
                PostWith("/api/v1/points/v1/temp/write",
                    """{"value":{"type":"float64","num":10},"confirm":true}""", auditor), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, auditorWrite.StatusCode, "an auditor cannot write points");

            // 权限矩阵在前端只是一份导航；真正的边界要在审计里能查到。
            using var entries = await ReadJsonAsync(t,
                await host.Client.SendAsync(GetWith("/api/v1/audit?action=auth.forbidden", Token), t.Ct));

            var denied = entries.RootElement.GetProperty("items").EnumerateArray()
                .Select(e => e.Clone())
                .ToArray();

            t.Ok(denied.Any(e => e.GetProperty("actor").GetString() == "viewer1"),
                "the denial is attributed to the real account, not the token");
            t.Ok(denied.All(e => e.GetProperty("result").GetString() == "denied"),
                "denials are recorded as denied");

            using var me = await ReadJsonAsync(t,
                await host.Client.SendAsync(GetWith("/api/v1/auth/me", viewer), t.Ct));
            t.Equal("viewer", me.RootElement.GetProperty("role").GetString(), "me reports the role");
            t.Ok(me.RootElement.GetProperty("permissions").EnumerateArray()
                    .All(p => p.GetString() != "point.write"),
                "me reports the effective permissions");
        });
    }

    private static Task AuthUsersRequireManage(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var viewer = await SignInAsAsync(t, host, "viewer2", "viewer", "viewer-password-2");

            var denied = await host.Client.SendAsync(
                PostWith("/api/v1/users", """{"id":"mallory","role":"admin"}""", viewer), t.Ct);
            t.Equal(HttpStatusCode.Forbidden, denied.StatusCode, "only user.manage can create accounts");

            // 不传口令时服务端生成一个一次性口令，管理员抄给用户，用户首次登录必须改。
            var created = await host.Client.SendAsync(
                PostWith("/api/v1/users", """{"id":"operator1","role":"operator"}""", Token), t.Ct);
            t.Equal(HttpStatusCode.OK, created.StatusCode, "an admin can create an account");

            using var document = await ReadJsonAsync(t, created);
            var temporary = document.RootElement.GetProperty("temporaryPassword").GetString();
            t.Ok(!string.IsNullOrWhiteSpace(temporary), "a temporary password is returned once");
            t.Equal(true, document.RootElement.GetProperty("user").GetProperty("mustChangePassword")
                .GetBoolean(), "the new account must change it");

            var (_, user) = await LoginAsync(t, host, "operator1", temporary!);
            t.Equal(true, user.GetProperty("mustChangePassword").GetBoolean(),
                "the temporary password is marked as temporary");

            using var list = await ReadJsonAsync(t,
                await host.Client.SendAsync(GetWith("/api/v1/users", Token), t.Ct));
            var text = list.RootElement.GetRawText();

            t.Contains(text, "operator1", "the list contains the new account");
            t.Ok(!text.Contains("pbkdf2", StringComparison.OrdinalIgnoreCase),
                "the user list never exposes password hashes");

            var update = await host.Client.SendAsync(
                PutWith("/api/v1/users/operator1",
                    """{"enabled":false}""", Token), t.Ct);
            t.Equal(HttpStatusCode.OK, update.StatusCode, "an admin can disable an account");

            var disabledLogin = await host.Client.SendAsync(
                PostWith("/api/v1/auth/login",
                    JsonSerializer.Serialize(new { username = "operator1", password = temporary }), null),
                t.Ct);
            t.Equal(HttpStatusCode.Forbidden, disabledLogin.StatusCode,
                "a disabled account cannot log in");

            var remove = await host.Client.SendAsync(
                DeleteWith("/api/v1/users/operator1", Token), t.Ct);
            t.Equal(HttpStatusCode.NoContent, remove.StatusCode, "an admin can delete an account");
        });
    }

    private static Task AuthLoginIsRateLimited(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);
            var initial = AdminInitialPassword(host);

            for (var i = 0; i < 5; i++)
            {
                var bad = await host.Client.SendAsync(
                    PostWith("/api/v1/auth/login",
                        JsonSerializer.Serialize(new { username = "admin", password = "wrong-" + i }), null),
                    t.Ct);

                t.Equal(HttpStatusCode.Unauthorized, bad.StatusCode, "a wrong password is a 401");
            }

            var locked = await host.Client.SendAsync(
                PostWith("/api/v1/auth/login",
                    JsonSerializer.Serialize(new { username = "admin", password = initial }), null),
                t.Ct);

            t.Equal((HttpStatusCode)429, locked.StatusCode,
                "the sixth attempt is rate limited even with the correct password");

            using var body = await ReadJsonAsync(t, locked);
            t.Equal("request.rate_limited", body.RootElement.GetProperty("error")
                .GetProperty("code").GetString(), "the error code is the contract's rate-limit code");
        });
    }

    /// <summary>
    /// 限流的端到端形状：走真实 Kestrel 中间件，确认「超配额 → 429 + 同一个 code」，
    /// 并且读/写配额各算各的、非 <c>/api/</c> 路径不计数。
    ///
    /// 用可替换的限流器（容量 3）而不是打满 100 次/秒：后者在本地回环上一秒就跑完，
    /// 结果完全取决于机器有多快。
    /// </summary>
    private static Task RateLimitReturns429(TestContext t)
    {
        return RunAsync(async () =>
        {
            RestApi.ResetRateLimitersForTests();

            using var limiters = RestApi.UseTestRateLimiters(readCapacity: 3, writeCapacity: 3);
            await using var host = await Host.StartAsync(t);

            // 静态资源与探活路径不计配额：WASM 首屏会并行拉几百个程序集，
            // 把它们算进读配额会让控制台在加载阶段就被自己打回 429。
            for (var i = 0; i < 20; i++)
            {
                var probe = await host.Client.GetAsync("/healthz", t.Ct);
                t.Equal(HttpStatusCode.OK, probe.StatusCode, "non-/api paths are not rate limited");
            }

            for (var i = 0; i < 3; i++)
            {
                var ok = await host.Client.GetAsync("/api/v1/version", t.Ct);
                t.Equal(HttpStatusCode.OK, ok.StatusCode, $"read {i} is within the burst");
            }

            var limited = await host.Client.GetAsync("/api/v1/version", t.Ct);
            t.Equal((HttpStatusCode)429, limited.StatusCode, "the read over the burst is a 429");

            // 大小写变体也必须计数：路由不区分大小写，限流若按 Ordinal 判前缀，
            // 改个大小写就等于免费拿到无限配额。
            var limitedUpper = await host.Client.GetAsync("/API/v1/version", t.Ct);
            t.Equal((HttpStatusCode)429, limitedUpper.StatusCode,
                "a case-variant path is not exempt from the rate limiter");

            using var body = await ReadJsonAsync(t, limited);
            t.Equal("request.rate_limited",
                body.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the limiter uses the same wire code as the login lockout");

            // 写配额是另一个桶：读被打回 429 不代表写也被打回。
            var write = await host.Client.SendAsync(
                PostWith("/api/v1/auth/login",
                    JsonSerializer.Serialize(new { username = "nobody", password = "nope" }), null), t.Ct);

            t.Ok(write.StatusCode != (HttpStatusCode)429,
                $"the write bucket is separate from the read bucket, got {(int)write.StatusCode}");
        });
    }

    private static Task AuthLogoutRevokesSession(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var initial = AdminInitialPassword(host);
            var (token, _) = await LoginAsync(t, host, "admin", initial);
            var session = await ChangePasswordAsync(t, host, token, initial, "admin-password-2");

            var bye = await host.Client.SendAsync(
                PostWith("/api/v1/auth/logout", "{}", session), t.Ct);

            t.Equal(HttpStatusCode.NoContent, bye.StatusCode, "logout succeeds");

            var me = await host.Client.SendAsync(GetWith("/api/v1/auth/me", session), t.Ct);
            t.Equal(HttpStatusCode.Unauthorized, me.StatusCode, "the session no longer works");

            using var audit = await ReadJsonAsync(t,
                await host.Client.SendAsync(GetWith("/api/v1/audit?action=auth.logout", Token), t.Ct));

            var entry = audit.RootElement.GetProperty("items")[0];
            t.Equal("admin", entry.GetProperty("actor").GetString(),
                "logout is audited against the account that logged out");
        });
    }

    private static Task MetricsReportLiveSseConnections(TestContext t)
    {
        return RunAsync(async () =>
        {
            RestApi.ResetRateLimitersForTests();
            await using var host = await Host.StartAsync(t);

            // 连一条真实的 SSE 流：读响应头就返回，服务端此时已经建立订阅。
            using var request = new HttpRequestMessage(HttpMethod.Get, "/api/v1/stream?heartbeat=30");
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
            request.Headers.Accept.ParseAdd("text/event-stream");

            using var response = await host.Client.SendAsync(
                request, HttpCompletionOption.ResponseHeadersRead, t.Ct);

            t.Equal(HttpStatusCode.OK, response.StatusCode, "the stream is established");

            // 先读一条 hello：确认服务端确实走到了订阅之后的那一步，而不是只回了响应头。
            await using var stream = await response.Content.ReadAsStreamAsync(t.Ct);
            using var reader = new StreamReader(stream);
            var hello = await reader.ReadLineAsync(t.Ct);
            t.Contains(hello ?? "", "event: hello", "the stream starts with hello");

            // 指标必须反映真实连接数。写死成常量 0 时，看板会一直显示「没有订阅者」，
            // 而实际上连接已经接近 50 上限 —— 这条路径正是运维判断「该扩容还是该排障」的依据。
            var metrics = await host.Client.GetStringAsync("/api/v1/metrics", t.Ct);
            var line = metrics.Split('\n').FirstOrDefault(l => l.StartsWith("uic_sse_connections ", StringComparison.Ordinal));

            t.Ok(line is not null, "the gauge is exported");
            var value = double.Parse(line!.Split(' ')[1], CultureInfo.InvariantCulture);
            t.Equal(1d, value, "the gauge counts the live stream");
        });
    }

    /// <summary>
    /// SSE 并发上限（`FR-NOR-03` 声称的「50 路并发上限」）：第 51 条必须被拒（429），
    /// 而不是悄悄建一条永远收不到样本的流。
    ///
    /// 为什么单独立一条：`metrics_report_live_sse_connections` 的注释里写着「连接已经
    /// 接近 50 上限」，但它只开了一条流。把 `MapStream` 里那句
    /// `if (hub.ConnectionCount >= MaxSseConnections)` 整个删掉，此前没有任何用例会失败。
    ///
    /// 50 条流**并发**建立而不是串行：串行时每条都要等一次响应头，在负载重的机器上
    /// 50 次累加会把这条用例拖到分钟级（实测出现过 35 s，甚至在重负载下触到 180 s 硬超时）。
    /// 并发之后总耗时约等于最慢的那一条，用例才稳定。每条 `heartbeat=300` 建好后挂着不读，
    /// 连接确实被占住。`ResponseHeadersRead` 在服务端 flush 首个 hello 后才返回，而
    /// `hub.Subscribe` 就发生在写 hello 之前——所以「50 条都拿到响应头」等价于「50 条都已订阅」，
    /// 再用 `uic_sse_connections` 复核连接数。
    /// </summary>
    private static Task SseRefusesTheConnectionPastTheCap(TestContext t)
    {
        return RunAsync(async () =>
        {
            RestApi.ResetRateLimitersForTests();
            await using var host = await Host.StartAsync(t);

            const int cap = 50;
            var held = new List<HttpResponseMessage>(cap);
            var requests = new List<HttpRequestMessage>(cap);

            // 这条用例把 50 条流持住若干秒，而 Host 的 HttpClient 带 10 s 的 Timeout：
            // 超时对 ResponseHeadersRead 之后的流式读取仍然生效，持流期间一旦被掐断，
            // 连接数会掉下去，第 51 条就可能反过来被接受。所以单独用一个不设超时的客户端
            // （取消仍由 t.Ct 控制，不会真的挂死）。
            using var streamClient = new HttpClient
            {
                BaseAddress = host.Client.BaseAddress,
                Timeout = Timeout.InfiniteTimeSpan,
            };

            try
            {
                // 并发建流：总耗时由最慢的一条决定，而不是 50 条相加。
                var pending = new List<Task<HttpResponseMessage>>(cap);
                for (var i = 0; i < cap; i++)
                {
                    var request = new HttpRequestMessage(HttpMethod.Get, "/api/v1/stream?heartbeat=300");
                    request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
                    request.Headers.Accept.ParseAdd("text/event-stream");
                    pending.Add(streamClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, t.Ct));
                    requests.Add(request);
                }

                var responses = await Task.WhenAll(pending);
                held.AddRange(responses);
                foreach (var response in responses)
                    t.Equal(HttpStatusCode.OK, response.StatusCode, "every stream up to the cap is accepted");

                // 都拿到响应头说明都已订阅；用指标复核连接数确实到 50。
                await WaitForSseGaugeAsync(t, host, cap, "all streams are counted as live");

                // 第 51 条：必须被拒。服务端在写响应头之前就抛 TooManyRequests，
                // 所以拿不到 200。给它 20 s 上限：上限被删掉时它会一直挂着等 hello，
                // 用例在这里判负而不是卡死。
                using var overflow = new HttpRequestMessage(HttpMethod.Get, "/api/v1/stream?heartbeat=300");
                overflow.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
                overflow.Headers.Accept.ParseAdd("text/event-stream");

                var sendTask = streamClient.SendAsync(overflow, HttpCompletionOption.ResponseHeadersRead, t.Ct);
                if (await Task.WhenAny(sendTask, Task.Delay(TimeSpan.FromSeconds(20), t.Ct)) != sendTask)
                    t.Fail("the 51st connection should be refused immediately, not hang waiting for hello");

                using var rejected = await sendTask;
                t.Equal((HttpStatusCode)429, rejected.StatusCode,
                    "a connection past the cap is refused, not silently accepted");

                using var envelope = await ReadJsonAsync(t, rejected);
                t.Equal("request.rate_limited",
                    envelope.RootElement.GetProperty("error").GetProperty("code").GetString(),
                    "the refusal uses the shared throttling code");
            }
            finally
            {
                foreach (var response in held) response.Dispose();
                foreach (var request in requests) request.Dispose();
            }

            // 名额必须能回收：全部释放后 gauge 归零，再连一条应当成功。
            // 这条挡住「上限只增不减」或「释放时不减」的计数实现错误。
            await WaitForSseGaugeAsync(t, host, 0, "the hub drained its subscribers after they disconnected");

            using var again = new HttpRequestMessage(HttpMethod.Get, "/api/v1/stream?heartbeat=300");
            again.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
            again.Headers.Accept.ParseAdd("text/event-stream");
            using var reconnected = await streamClient.SendAsync(
                again, HttpCompletionOption.ResponseHeadersRead, t.Ct);
            t.Equal(HttpStatusCode.OK, reconnected.StatusCode,
                "a slot freed by a disconnect is usable again");
        });
    }

    /// <summary>
    /// 轮询 `/api/v1/metrics` 的 `uic_sse_connections`，等它走到期望值。
    /// 用指标而不是 `SseHub` 实例：hub 在 `UseNorthbound` 内部创建，`Host` 拿不到引用。
    /// </summary>
    private static async Task WaitForSseGaugeAsync(TestContext t, Host host, double expected, string what)
    {
        var deadline = Stopwatch.StartNew();
        string? line = null;

        while (true)
        {
            var text = await host.Client.GetStringAsync("/api/v1/metrics", t.Ct);
            line = text.Split('\n')
                .FirstOrDefault(l => l.StartsWith("uic_sse_connections ", StringComparison.Ordinal));

            if (line is not null &&
                double.Parse(line.Split(' ')[1], CultureInfo.InvariantCulture) == expected)
                return;

            if (deadline.Elapsed > TimeSpan.FromSeconds(30))
                t.Fail($"{what}: expected uic_sse_connections == {expected}, last saw {(line ?? "no series")}");

            await Task.Delay(50, t.Ct);
        }
    }

    /// <summary>
    /// `/api/v1/metrics` 必须是一个真正可用的 Prometheus 抓取端点：文本格式（HELP/TYPE）、
    /// 运行期的 gauge 真的反映当前状态，而不是一串写死的 0。
    ///
    /// 为什么单独立一条：`FR-OPS-05` 在追溯矩阵里写的是「已实现、指标都有序列」，
    /// 但此前唯一沾边的用例只断言了 `uic_sse_connections` 一个 gauge。看板依赖的
    /// 设备数、采集中的设备数、未配 sink 时的 Outbox 这类序列一条都没被断言过——
    /// 序列名或标签一漂，端点仍然返回 200，运维只会看到空面板。
    /// </summary>
    private static Task MetricsExportRuntimeSeries(TestContext t)
    {
        return RunAsync(async () =>
        {
            RestApi.ResetRateLimitersForTests();
            await using var host = await Host.StartAsync(t);

            // 先等设备真正进入 collecting：`uic_devices_collecting` 是运行态快照，
            // 刚启动时还是 0。不等就断言，用例会随机器负载随机红。
            await WaitForPointAsync(host, "v1", "temp", t);

            using var request = new HttpRequestMessage(HttpMethod.Get, "/api/v1/metrics");
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
            using var response = await host.Client.SendAsync(request, t.Ct);

            t.Equal(HttpStatusCode.OK, response.StatusCode, "the scrape endpoint answers");

            // 内容类型必须带 Prometheus 的文本版本：抓取端靠它判定格式，
            // 返回 application/json 会被直接丢弃。
            var mediaType = response.Content.Headers.ContentType?.MediaType ?? string.Empty;
            t.Equal("text/plain", mediaType, "the endpoint serves the Prometheus text format");
            var version = response.Content.Headers.ContentType?.Parameters
                .FirstOrDefault(p => p.Name.Equals("version", StringComparison.OrdinalIgnoreCase))?.Value;
            t.Equal("0.0.4", version, "the exposition format is declared");

            var body = await response.Content.ReadAsStringAsync(t.Ct);

            // HELP 与 TYPE 是必需的元数据：缺了它们 Grafana 不会报错，只是面板一片空白。
            t.Contains(body, "# HELP uic_devices_total", "the device gauge carries HELP");
            t.Contains(body, "# TYPE uic_devices_total gauge", "the device gauge is typed");

            // 最小配置里只有一台虚拟设备：序列必须反映真实数量，写死 0 或 1 都可能碰巧过关，
            // 所以下面还有一条「采集中」的用例把它钉到运行态上。
            t.Equal(1d, MetricValue(body, "uic_devices_total"), "uic_devices_total counts configured devices");
            t.Equal(1d, MetricValue(body, "uic_devices_collecting"),
                "uic_devices_collecting only counts devices that reached collecting");

            // 每一个数值都必须是合法的 Prometheus 浮点（或 NaN/Inf），不能出现本地化的小数逗号：
            // 用解析器逐值过一遍，比只 grep 序列名更能挡住格式漂移。
            foreach (var line in body.Split('\n'))
            {
                if (line.Length == 0 || line[0] == '#') continue;

                var space = line.LastIndexOf(' ');
                t.Ok(space > 0, $"metric line has a value: {line}");

                var number = line[(space + 1)..];
                t.Ok(double.TryParse(number, NumberStyles.Float, CultureInfo.InvariantCulture, out _),
                    $"the value of '{line[..Math.Max(0, space)]}' is a Prometheus float, not '{number}'");
            }

            // 未配置任何北向 sink 时队列必须稳定在 0，而不是缺失：缺失会被当成「没上报」。
            t.Equal(0d, MetricValue(body, "uic_outbox_pending"), "the outbox backlog is exported even at zero");
        });
    }

    /// <summary>
    /// 取一条无标签 gauge 的值。带标签的序列不是这里的目标，遇到就直接报错——
    /// 静默取其第一个标签会掩盖「序列形状变了」。
    /// </summary>
    private static double MetricValue(string body, string name)
    {
        foreach (var line in body.Split('\n'))
        {
            if (!line.StartsWith(name + " ", StringComparison.Ordinal)) continue;

            var text = line[(name.Length + 1)..];
            if (!double.TryParse(text, NumberStyles.Float, CultureInfo.InvariantCulture, out var value))
                throw new CheckFailedException($"metric '{name}' is not a bare number: '{text}'");

            return value;
        }

        throw new CheckFailedException($"metric '{name}' is not exported");
    }

    /// <summary>
    /// 诊断包（FR-OPS-07）：一次导出要能替现场回答「你那份配置长什么样、指标如何、
    /// 日志说了什么、版本是哪个」。
    ///
    /// 为什么单独立一条：此前只有 <c>mqtt/command_diag_bundle_is_whitelisted</c> 证明
    /// 「这个命令允许调」，包里的内容一个字都没被断言过——而排障时真正被打开看的正是内容。
    /// 尤其两件必须钉死的事：
    /// 1. 配置是**脱敏**的（投屏、贴进工单的场景里不能带明文口令）；
    /// 2. 指标/日志不是空壳（空文件也能过「响应是 zip」这种浅断言）。
    /// </summary>
    private static Task DiagnosticsBundleCarriesTheFourParts(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            using var response = await host.Client.SendAsync(Get("/api/v1/diagnostics/bundle"), t.Ct);
            t.Equal(HttpStatusCode.OK, response.StatusCode, "the bundle endpoint answers");
            t.Equal("application/zip", response.Content.Headers.ContentType?.MediaType,
                "the bundle is served as a zip");

            var bytes = await response.Content.ReadAsByteArrayAsync(t.Ct);
            using var zip = new System.IO.Compression.ZipArchive(new MemoryStream(bytes));

            string Read(string name)
            {
                var entry = zip.GetEntry(name);
                t.Ok(entry is not null, $"the bundle contains {name}");
                using var stream = entry!.Open();
                using var reader = new StreamReader(stream, Encoding.UTF8);
                return reader.ReadToEnd();
            }

            // 版本：排障第一问是「跑的是哪个版本」。
            using var manifest = JsonDocument.Parse(Read("manifest.json"));
            var manifestRoot = manifest.RootElement;
            t.Equal("0.0.0-test", manifestRoot.GetProperty("gateway").GetString(),
                "the manifest names the running gateway version");
            t.Equal(RestApi.ApiVersion, manifestRoot.GetProperty("apiVersion").GetString(),
                "the manifest names the API version");
            t.Equal("contract-site", manifestRoot.GetProperty("site").GetString(),
                "the manifest names the site");

            // 配置：必须是脱敏的。明文只在环境变量里，包里出现即失败。
            var config = Read("config.redacted.json");
            t.Contains(config, "\"***\"", "credential fields are redacted");
            t.Ok(!config.Contains(Token, StringComparison.Ordinal),
                "the plaintext token never reaches the bundle");

            // 指标：真实序列而不是空壳。
            var metrics = Read("metrics.txt");
            t.Contains(metrics, "# TYPE uic_devices_total", "metrics carry Prometheus metadata");
            t.Contains(metrics, "uic_devices_total ", "metrics carry at least one device series");

            // 日志：这个宿主默认不产生告警级事件（启动顺利、无资源压力），
            // 所以日志段可能为空——那本身不是缺陷。真正要钉死的是脱敏：
            // 排障包会被原样转给别人，日志里绝不能带明文令牌。
            var logs = Read("logs/uic.log");
            t.Ok(!logs.Contains(Token, StringComparison.Ordinal),
                "the log section never carries the plaintext token");

            // 健康与设备快照：排障时用来对齐「那一刻系统怎么想」。
            using var health = JsonDocument.Parse(Read("health.json"));
            t.Ok(health.RootElement.TryGetProperty("status", out _), "health.json carries the aggregated status");
            using var devices = JsonDocument.Parse(Read("devices.json"));
            t.Ok(devices.RootElement.ValueKind == JsonValueKind.Array
                 && devices.RootElement.GetArrayLength() > 0,
                "devices.json lists the configured devices");
        });
    }

    private static Task StreamFiltersByPointPattern(TestContext t)
    {
        return RunAsync(async () =>
        {
            RestApi.ResetRateLimitersForTests();

            // 用正弦波形而不是默认的 const：值不变时死区判定不产生样本，
            // 订阅建立后一条都不会来，用例会误报成「过滤失败」。
            await using var host = await Host.StartAsync(t, WaveConfig);

            // 订阅表达式是 `device/point`（产品规格与 UI 都用 `/`）。设备 id 本身允许包含
            // `.`（`^[a-z0-9][a-z0-9._-]{0,63}$`），所以分隔符不能是点：`meter-1.v2.temp`
            // 这种 id 用点分隔时根本切不开，只能整串当设备名去比，永远匹配不上。
            using var subscription = await OpenStreamAsync(t, host, "/api/v1/stream?points=v1/temp&heartbeat=30");

            var samples = await ReadSamplesAsync(t, subscription, TimeSpan.FromSeconds(10), wanted: 1);

            t.Ok(samples.Count >= 1, "the filtered stream delivers at least one sample");
            foreach (var sample in samples)
                t.Equal("temp", sample.Point, "only the subscribed point is delivered");

            // 不匹配的模式也必须建立成功（200 + hello），但收不到该点位的样本。
            using var other = await OpenStreamAsync(t, host, "/api/v1/stream?points=v1/nosuchpoint&heartbeat=30");
            var none = await ReadSamplesAsync(t, other, TimeSpan.FromSeconds(2), wanted: 1);
            t.Equal(0, none.Count, "a pattern that matches nothing delivers no samples");

            // 质量码拼错不能建出一条永远不出样本的空流：那与「设备没数据」在客户端
            // 看来完全一样，而 hello 与心跳都正常，没有任何线索。
            //
            // 用 ResponseHeadersRead 发：拒绝时立刻拿到 400；若没有校验，这里会拿到
            // 200 + 一个不会自己结束的流——那正是要抓住的症状。
            using var invalidRequest = Get("/api/v1/stream?quality=gud");
            invalidRequest.Headers.Accept.ParseAdd("text/event-stream");
            using var invalid = await host.Client.SendAsync(
                invalidRequest, HttpCompletionOption.ResponseHeadersRead, t.Ct);

            t.Equal(HttpStatusCode.BadRequest, invalid.StatusCode,
                "an unknown quality code is rejected instead of silently filtering everything out");

            using var body = await ReadJsonAsync(t, invalid);
            t.Equal("request.invalid_parameter",
                body.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the error names the parameter");
        });
    }

    private static async Task<StreamSubscription> OpenStreamAsync(TestContext t, Host host, string path)
    {
        var request = new HttpRequestMessage(HttpMethod.Get, path);
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Token);
        request.Headers.Accept.ParseAdd("text/event-stream");

        var response = await host.Client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, t.Ct);
        t.Equal(HttpStatusCode.OK, response.StatusCode, $"the stream {path} is established");

        var stream = await response.Content.ReadAsStreamAsync(t.Ct);
        return new StreamSubscription(response, stream, new StreamReader(stream));
    }

    /// <summary>读到够数量或超时为止。SSE 是长连接，不能等流自己结束。</summary>
    private static async Task<List<SampleDto>> ReadSamplesAsync(
        TestContext t, StreamSubscription subscription, TimeSpan timeout, int wanted)
    {
        var samples = new List<SampleDto>();
        using var limit = CancellationTokenSource.CreateLinkedTokenSource(t.Ct);
        limit.CancelAfter(timeout);

        string? type = null;
        StringBuilder? data = null;

        try
        {
            while (samples.Count < wanted && !limit.IsCancellationRequested)
            {
                var line = await subscription.Reader.ReadLineAsync(limit.Token);

                // 流被服务端关掉时返回 null：按「读到这里」处理，断言交给调用方。
                if (line is null) break;

                if (line.Length == 0)
                {
                    if (type == "sample" && data is not null)
                    {
                        samples.Add(JsonSerializer.Deserialize<SampleDto>(data.ToString(), JsonDefaults.Options)!);
                    }

                    type = null;
                    data = null;
                    continue;
                }

                if (line.StartsWith("event:", StringComparison.Ordinal))
                {
                    type = line["event:".Length..].Trim();
                    data = new StringBuilder();
                }
                else if (line.StartsWith("data:", StringComparison.Ordinal) && data is not null)
                {
                    if (data.Length > 0) data.Append('\n');
                    data.Append(line["data:".Length..].Trim());
                }
            }
        }
        catch (OperationCanceledException) when (limit.IsCancellationRequested)
        {
            // 超时：把已经收到的样本交回去，由调用方断言数量。
        }

        return samples;
    }

    private sealed class StreamSubscription : IDisposable
    {
        private readonly HttpResponseMessage _response;

        public StreamSubscription(HttpResponseMessage response, Stream stream, StreamReader reader)
        {
            _response = response;
            Stream = stream;
            Reader = reader;
        }

        public Stream Stream { get; }
        public StreamReader Reader { get; }

        public void Dispose()
        {
            Reader.Dispose();
            Stream.Dispose();
            _response.Dispose();
        }
    }

    /// <summary>
    /// 热更新时 OPC UA 服务端起不来：apply 仍算成功，但必须留下计数。
    ///
    /// 为什么单独钉这一条：<c>uic_opcua_start_failures_total</c> 此前只在实现里有自增点。
    /// 这里的语义很特别——设备与配置都已经生效，让整个 apply 失败会给出「什么都没变」的
    /// 错误信号，所以失败只记日志 + 计数。计数一旦写漏，现场表现就是「配置说 OPC UA 开着、
    /// 客户端连不上、监控上一条错都没有」，这是最难查的一类。
    ///
    /// 注入方式是真实的：把端点指向一个已被占用的端口，服务端绑定就会失败。
    /// </summary>
    private static Task OpcUaRestartFailureIsCounted(TestContext t)
    {
        return RunAsync(async () =>
        {
            // 先占住一个端口：真实占用比伪造一个异常更接近现场（端口被别的服务抢走）。
            var blocker = new System.Net.Sockets.TcpListener(
                System.Net.IPAddress.Loopback, 0);
            blocker.Start();
            var busyPort = ((System.Net.IPEndPoint)blocker.LocalEndpoint).Port;

            try
            {
                await using var host = await Host.StartAsync(t);

                var before = host.Runtime.Metrics.CounterValue("uic_opcua_start_failures_total");
                t.Equal(0d, before, "no startup failure before the endpoint is moved");

                // 打开 OPC UA 并指到被占用的端口：静态校验会过（scheme/端口格式都对），
                // 失败发生在绑定阶段——正是这条计数要覆盖的路径。
                var node = JsonNode.Parse(
                    JsonDefaults.ToJson(host.Runtime.CurrentConfig))!.AsObject();
                node["northbound"]!["opcUa"] = JsonNode.Parse($$"""
                    { "enabled": true, "endpoint": "opc.tcp://127.0.0.1:{{busyPort}}" }
                    """);

                var next = ConfigStore.Parse(node.ToJsonString());
                var outcome = await host.Runtime.ApplyConfigAsync(next, "opc ua on a busy port", t.Ct);

                // apply 本身必须成功：设备已经在新配置上跑起来了，报失败会让运维以为改动没生效。
                t.Ok(outcome.Applied, "the apply succeeds even though the OPC UA endpoint failed");

                var failures = host.Runtime.Metrics.CounterValue("uic_opcua_start_failures_total");
                t.Equal(1d, failures, "the failed restart is counted");

                // 服务端确实没起来，而且这个事实对外可见：设备照常采集，只是没有 UA 端点。
                t.Ok(host.Runtime.OpcUaServer is null, "no OPC UA server is running after the failure");
                var point = await WaitForPointAsync(host, "v1", "temp", t);
                t.Equal("good", point.GetProperty("quality").GetString(),
                    "collection is unaffected by the northbound failure");
            }
            finally
            {
                blocker.Stop();
            }
        });
    }

    /// <summary>
    /// 命令缺必填参数必须是 400 + request.missing_parameter，不能是「权限」问题，也不能静默成功。
    ///
    /// 为什么单独立一条：命令执行器在参数缺失时抛 BadRequest("request.missing_parameter")，
    /// 这是 northbound-api.md 错误表里明列的 400 码。它与「没权限」(403 auth.forbidden)、
    /// 「命令不存在」(400 request.unknown_action) 是三件不同的事，运维要从 code 一眼分清是
    /// 哪类问题：缺参去补全请求体，越权去申请权限，动作拼错去改配置。三者若被塌缩成同一个
    /// 笼统 400，现场会朝错误方向排查。这里用静态 API Token（Admin、全权限）发命令，正是要
    /// 把「权限」这条岔路排除掉，确认落到的是参数校验本身。
    /// </summary>
    private static Task CommandRejectsMissingParameter(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            // driver.restart 需要 device；params 留空，执行器应先在 Param("device") 上抛错。
            var missing = await host.Client.SendAsync(
                Post("/api/v1/commands",
                    """{"action":"driver.restart","params":{}}"""), t.Ct);

            t.Equal(HttpStatusCode.BadRequest, missing.StatusCode,
                "a command missing a required parameter is a 400, not a permission problem");
            using var body = await ReadJsonAsync(t, missing);
            var error = body.RootElement.GetProperty("error");
            t.Equal("request.missing_parameter", error.GetProperty("code").GetString(),
                "the code names the missing parameter, not a generic bad request");
            t.Ok(error.GetProperty("message").GetString()!.Contains("device"),
                "the message points at the missing parameter name");

            await host.DisposeAsync();
        });
    }

    /// <summary>
    /// 破坏性命令（config.rollback / outbox.drop）忘带 confirm:true 必须被 400 + request.confirm_required
    /// 拦下，而且拦在执行之前——一条手滑的回滚绝不能就此生效。
    ///
    /// 为什么单独立一条：这两条命令会切整份配置、丢待投递数据，属于「需要操作员二次确认」的动作。
    /// 执行器里 Require(scope) 通过之后，下一步就是 if (!request.Confirm) throw confirm_required，
    /// 顺序很关键：确认这道闸门在真正动配置/丢数据之前。REST 允许这两个动作（CommandScope.All），
    /// 所以这里不是 403 forbidden（那是 MQTT 通道才有的），而是「忘了带确认」的 400。若有人把那个
    /// if 删掉或挪到执行之后，回滚就会在没确认的情况下直接生效——把「手滑一次覆盖整张点表」从
    /// 失误放大成事故。用例钉住两条动作都返回同一码，且状态没被改动。
    /// </summary>
    private static Task CommandRequiresConfirmForDestructiveActions(TestContext t)
    {
        return RunAsync(async () =>
        {
            await using var host = await Host.StartAsync(t);

            var configBefore = host.Runtime.ConfigVersion;

            var rollback = await host.Client.SendAsync(
                Post("/api/v1/commands",
                    """{"action":"config.rollback","params":{"version":"v1"},"confirm":false}"""),
                t.Ct);
            t.Equal(HttpStatusCode.BadRequest, rollback.StatusCode,
                "a destructive command without confirm is rejected before it runs");
            using var rollbackBody = await ReadJsonAsync(t, rollback);
            t.Equal("request.confirm_required",
                rollbackBody.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "the code names the missing confirmation");

            var drop = await host.Client.SendAsync(
                Post("/api/v1/commands",
                    """{"action":"outbox.drop","params":{},"confirm":false}"""),
                t.Ct);
            t.Equal(HttpStatusCode.BadRequest, drop.StatusCode,
                "outbox.drop is gated by the same confirmation");
            using var dropBody = await ReadJsonAsync(t, drop);
            t.Equal("request.confirm_required",
                dropBody.RootElement.GetProperty("error").GetProperty("code").GetString(),
                "outbox.drop reports the same missing-confirmation code");

            // 两道闸门都在执行前拦下：配置版本没被改动，确认这道确认确实挡在回滚之前。
            t.Equal(configBefore, host.Runtime.ConfigVersion,
                "the rejected rollback left the live configuration untouched");

            await host.DisposeAsync();
        });
    }

    private static Task RunAsync(Func<Task> body) => body();
}
