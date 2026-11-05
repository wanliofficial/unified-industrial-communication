using Uic.Tests;

// 自包含测试入口。
//
// 不依赖任何第三方测试框架：
//   dotnet run --project tests/Uic.Tests
//   dotnet run --project tests/Uic.Tests -- --filter planner
//   dotnet run --project tests/Uic.Tests -- --opcua
//
// 需要外部资源的用例（目前只有 OPC UA，要起端点并生成证书）默认跳过，
// 用 --opcua 显式开启：CI 上跑得慢，本地跑得通才是它存在的意义。

var tests = new List<TestCase>();

try
{
    GoldenTests.Register(tests);
}
catch (Exception ex) when (ex is DirectoryNotFoundException or InvalidDataException)
{
    Console.Error.WriteLine("cannot load the golden corpus: " + ex.Message);
    Console.Error.WriteLine("run from the repository (the fixtures live in dotnet/testdata/golden).");
    return 2;
}

CodecTests.Register(tests);
PipelineTests.Register(tests);
EngineTests.Register(tests);
PlannerTests.Register(tests);
OutboxTests.Register(tests);
ConfigStoreTests.Register(tests);
JsonTests.Register(tests);
LoopbackTests.Register(tests);
SerialTests.Register(tests);
MqttTests.Register(tests);
MqttCommandTests.Register(tests);
ContractTests.Register(tests);
OpcUaTests.Register(tests);

return await TestRunner.RunAsync(tests, args);
