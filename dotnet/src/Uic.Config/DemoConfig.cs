using Uic.Domain;

namespace Uic.Config;

/// <summary>
/// 开箱即用的示例配置。
///
/// 它的用途不是「演示玩具」，而是提供一套可在无硬件环境完整跑通
/// 采集 → 缓存 → 北向 → 落盘的闭环，便于 CI 与新同事上手。
/// </summary>
public static class DemoConfig
{
    /// <summary>
    /// <paramref name="enableLocalModbus"/> 为 true 时启用指向本机 15020 的 Modbus 设备，
    /// 供 <c>uic sim</c> 使用：模拟从站与采集主站跑在同一个进程里，开箱即可看到完整闭环。
    /// </summary>
    public static ConfigDocument Build(bool enableLocalModbus = false, int modbusPort = 15020) => new()
    {
        SchemaVersion = 1,
        Site = "demo-site",
        Description = "开箱即用的示例配置：一台虚拟设备 + 一台本机 Modbus TCP 设备",
        Channels = new List<Channel>
        {
            new()
            {
                Id = "virtual-bus",
                Name = "虚拟总线",
                Protocol = "virtual",
                Transport = "none",
            },
            new()
            {
                Id = "modbus-tcp-1",
                Name = "本机 Modbus TCP",
                Protocol = "modbus-tcp",
                Transport = "tcp",
                Host = "127.0.0.1",
                Port = modbusPort,
                Timeout = TimeSpan.FromSeconds(2),
                ConnectTimeout = TimeSpan.FromSeconds(2),
                MaxConcurrency = 2,
            },
        },
        Devices = new List<Device>
        {
            new()
            {
                Id = "virtual-1",
                Name = "虚拟设备 1",
                ChannelId = "virtual-bus",
                Enabled = true,
                Interval = TimeSpan.FromSeconds(1),
                UnitId = 1,
                Labels = new Dictionary<string, string>(StringComparer.Ordinal)
                {
                    ["mode"] = "sine",
                    ["periodSeconds"] = "30",
                    ["amplitude"] = "100",
                    ["baseline"] = "50",
                },
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "temperature",
                        Name = "温度",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.Float64,
                        Unit = "°C",
                        Scale = 1,
                        Deadband = 0.05,
                    },
                    new()
                    {
                        Id = "pressure",
                        Name = "压力",
                        Address = Address.Register("holding", 4),
                        DataType = DataType.Float64,
                        Unit = "kPa",
                        Scale = 1,
                        Deadband = 0.1,
                    },
                    new()
                    {
                        Id = "running",
                        Name = "运行状态",
                        Address = Address.Register("coil", 0),
                        DataType = DataType.Bool,
                    },
                    new()
                    {
                        Id = "counter",
                        Name = "累计计数",
                        Address = Address.Register("holding", 8),
                        DataType = DataType.UInt32,
                        Unit = "pcs",
                    },
                },
            },
            new()
            {
                Id = "modbus-1",
                Name = "本机 Modbus 从站",
                ChannelId = "modbus-tcp-1",
                Enabled = enableLocalModbus,
                Interval = TimeSpan.FromMilliseconds(500),
                UnitId = 1,
                ByteOrder = ByteOrder.ABCD,
                Timeout = TimeSpan.FromSeconds(2),
                FailureThreshold = 3,
                RecoverInterval = TimeSpan.FromSeconds(10),
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "holding-0",
                        Name = "保持寄存器 0",
                        Address = Address.Register("holding", 0),
                        DataType = DataType.UInt16,
                    },
                    new()
                    {
                        Id = "flow",
                        Name = "流量",
                        Address = Address.Register("holding", 10, 2),
                        DataType = DataType.Float32,
                        Unit = "m³/h",
                        Scale = 0.1,
                        Deadband = 0.01,
                    },
                    new()
                    {
                        Id = "alarm",
                        Name = "报警位",
                        Address = Address.Register("coil", 3),
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
                Listen = "0.0.0.0:8080",
            },
            Mqtt = new MqttOptions
            {
                Enabled = false,
                Host = "127.0.0.1",
                Port = 1883,
                TopicPrefix = "uic",
                Qos = 1,
            },
            OpcUa = new OpcUaOptions
            {
                Enabled = false,
                Endpoint = "opc.tcp://0.0.0.0:4840",
            },
        },
        Storage = new StorageOptions
        {
            DataDir = "./data",
            Journal = false,
            OutboxBatchSize = 64,
            OutboxFlushInterval = TimeSpan.FromSeconds(1),
        },
    };
}