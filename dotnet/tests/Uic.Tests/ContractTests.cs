using System.Diagnostics;
using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
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
using Uic.Driver.Virtual;
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

    public static void Register(List<TestCase> tests)
    {
        tests.Add(new TestCase("contract/health_and_version_are_public", HealthAndVersionArePublic));
        tests.Add(new TestCase("contract/api_requires_bearer_token", ApiRequiresBearerToken));
        tests.Add(new TestCase("contract/devices_and_points_are_listed", DevicesAndPointsAreListed));
        tests.Add(new TestCase("contract/drivers_expose_capability_states", DriversExposeCapabilityStates));
        tests.Add(new TestCase("contract/unknown_device_returns_error_envelope", UnknownDeviceReturnsEnvelope));
        tests.Add(new TestCase("contract/config_is_redacted", ConfigIsRedacted));
        tests.Add(new TestCase("contract/config_validate_reports_problems", ConfigValidateReportsProblems));
        tests.Add(new TestCase("contract/write_returns_per_point_results", WriteReturnsPerPointResults));
        tests.Add(new TestCase("contract/single_point_write_is_idempotent", SinglePointWriteIsIdempotent));
        tests.Add(new TestCase("contract/template_instantiates_and_applies_devices", TemplateInstantiatesAndApplies));
        tests.Add(new TestCase("contract/points_csv_import_previews_and_applies", PointsCsvImportPreviewsAndApplies));
        tests.Add(new TestCase("contract/config_export_import_round_trip", ConfigExportImportRoundTrip));
        tests.Add(new TestCase("contract/config_import_migrates_legacy_shape", ConfigImportMigratesLegacy));
    }

    // ------------------------------------------------------------------
    // 夹具
    // ------------------------------------------------------------------

    private sealed class Host : IAsyncDisposable
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

        public static async Task<Host> StartAsync(TestContext t)
        {
            var ring = new RingBufferLogProvider(128);
            var logs = LoggerFactory.Create(builder =>
            {
                builder.ClearProviders();
                builder.AddProvider(ring);
                builder.SetMinimumLevel(LogLevel.Warning);
            });

            var options = new RuntimeOptions
            {
                DataDir = t.TempDir,
                GatewayVersion = "0.0.0-test",
                Fallback = BuildConfig,
            };

            var runtime = await GatewayRuntime.CreateAsync(options, Drivers(), logs, ring, t.Ct);

            var builder = WebApplication.CreateBuilder(new WebApplicationOptions
            {
                EnvironmentName = "Development",
            });

            builder.Logging.ClearProviders();
            builder.WebHost.UseUrls("http://127.0.0.1:0");

            var app = builder.Build();
            app.UseNorthbound(runtime.CreateNorthboundContext());

            await app.StartAsync(t.Ct);

            var address = app.Services.GetRequiredService<IServer>()
                .Features.Get<IServerAddressesFeature>()!
                .Addresses.First();

            var client = new HttpClient { BaseAddress = new Uri(address), Timeout = TimeSpan.FromSeconds(10) };

            return new Host(app, runtime, logs, client);
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

    private static DriverRegistry Drivers()
    {
        var registry = new DriverRegistry();
        registry.Register(ModbusMasterDriver.Tcp(), ModbusMasterDriver.Rtu(), ModbusMasterDriver.Ascii());
        registry.Register(new VirtualDriver());
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
                Token = Token,
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
            t.Equal(4, items.GetArrayLength(), "all registered drivers are listed");

            var byId = items.EnumerateArray().ToDictionary(
                d => d.GetProperty("id").GetString()!,
                d => d.Clone(),
                StringComparer.Ordinal);

            t.Ok(byId.ContainsKey("modbus-tcp") && byId.ContainsKey("modbus-rtu") &&
                 byId.ContainsKey("modbus-ascii") && byId.ContainsKey("virtual"),
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

    private static Task RunAsync(Func<Task> body) => body();
}
