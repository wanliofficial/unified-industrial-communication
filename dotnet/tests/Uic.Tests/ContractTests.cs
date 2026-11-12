using System.Diagnostics;
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
        tests.Add(new TestCase("contract/resource_pressure_degrades_health", ResourcePressureDegradesHealth));
        tests.Add(new TestCase("contract/apply_keeps_redacted_secrets", ApplyKeepsRedactedSecrets));
        tests.Add(new TestCase("contract/history_query_reads_journal", HistoryQueryReadsJournal));
        tests.Add(new TestCase("contract/pagination_cursor_is_validated", PaginationCursorIsValidated));
        tests.Add(new TestCase("contract/api_requires_bearer_token", ApiRequiresBearerToken));
        tests.Add(new TestCase("contract/devices_and_points_are_listed", DevicesAndPointsAreListed));
        tests.Add(new TestCase("contract/drivers_expose_capability_states", DriversExposeCapabilityStates));
        tests.Add(new TestCase("contract/unknown_device_returns_error_envelope", UnknownDeviceReturnsEnvelope));
        tests.Add(new TestCase("contract/config_is_redacted", ConfigIsRedacted));
        tests.Add(new TestCase("contract/config_validate_reports_problems", ConfigValidateReportsProblems));
        tests.Add(new TestCase("contract/plaintext_credentials_are_rejected", PlaintextCredentialsAreRejected));
        tests.Add(new TestCase("contract/write_returns_per_point_results", WriteReturnsPerPointResults));
        tests.Add(new TestCase("contract/write_values_are_engineering_units", WriteValuesAreEngineeringUnits));
        tests.Add(new TestCase("contract/single_point_write_is_idempotent", SinglePointWriteIsIdempotent));
        tests.Add(new TestCase("contract/idempotency_window_ignores_a_clock_jump", IdempotencyWindowIgnoresAClockJump));
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
            TimeProvider? clock = null)
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
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = config ?? BuildConfig,
                SchedulerOptions = clock is null ? null : new SchedulerOptions { Time = clock },
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
    private static ConfigDocument BuildConfig() => new()
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
                    ["mode"] = "const",
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
            t.Equal("contract-site", root.GetProperty("site").GetString(), "site");
            t.Ok(root.GetProperty("startedAt").GetString()!.EndsWith('Z'), "startedAt is UTC with Z");
            t.Ok(root.GetProperty("configVersion").GetString()!.StartsWith('v'), "config version is content-addressed");

            var metrics = await host.Client.GetAsync("/api/v1/metrics", t.Ct);
            t.Equal(HttpStatusCode.OK, metrics.StatusCode, "/api/v1/metrics status");
            t.Contains(await metrics.Content.ReadAsStringAsync(t.Ct), "# TYPE", "metrics are in Prometheus text format");
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

            var mqtt = byId["mqtt"];
            t.Equal("supported", Cap(mqtt, "read"), "mqtt can read from its subscription cache");
            t.Equal("unsupported", Cap(mqtt, "write"), "the mqtt southbound driver is read-only");
            t.Equal("supported", Cap(mqtt, "subscribe"), "mqtt is subscription based");
            t.Equal("symbol", mqtt.GetProperty("addressKinds")[0].GetString(),
                "mqtt addresses points by topic (symbol addressing)");

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

    private static Task RunAsync(Func<Task> body) => body();
}
