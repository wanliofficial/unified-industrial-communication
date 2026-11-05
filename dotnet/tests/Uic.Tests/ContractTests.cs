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
        tests.Add(new TestCase("contract/unknown_device_returns_error_envelope", UnknownDeviceReturnsEnvelope));
        tests.Add(new TestCase("contract/config_is_redacted", ConfigIsRedacted));
        tests.Add(new TestCase("contract/config_validate_reports_problems", ConfigValidateReportsProblems));
        tests.Add(new TestCase("contract/write_returns_per_point_results", WriteReturnsPerPointResults));
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

    private static Task RunAsync(Func<Task> body) => body();
}
