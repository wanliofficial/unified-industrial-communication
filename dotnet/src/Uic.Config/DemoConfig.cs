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
    /// <paramref name="enableLocalS7"/> 为 true 时启用指向本机 15102 的 S7 设备，
    /// <paramref name="enableLocalBacnet"/> 为 true 时启用指向本机 47810/UDP 的 BACnet/IP 设备，
    /// 供 <c>uic sim</c> 使用：模拟从站与采集主站跑在同一个进程里，开箱即可看到完整闭环。
    /// </summary>
    public static ConfigDocument Build(
        bool enableLocalModbus = false,
        int modbusPort = 15020,
        bool enableLocalS7 = false,
        int s7Port = 15102,
        bool enableLocalBacnet = false,
        int bacnetPort = 47810) => new()
    {
        SchemaVersion = 1,
        Site = "demo-site",
        Description = "开箱即用的示例配置：一台虚拟设备 + 本机 Modbus TCP / S7 设备",
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
            new()
            {
                Id = "s7-tcp-1",
                Name = "本机 S7 PLC",
                Protocol = "s7",
                Transport = "tcp",
                Host = "127.0.0.1",
                Port = s7Port,
                Timeout = TimeSpan.FromSeconds(2),
                ConnectTimeout = TimeSpan.FromSeconds(2),
                MaxConcurrency = 1,
            },
            new()
            {
                Id = "bacnet-udp-1",
                Name = "本机 BACnet/IP",
                Protocol = "bacnet",
                Transport = "udp",
                Host = "127.0.0.1",
                Port = bacnetPort,
                Timeout = TimeSpan.FromSeconds(2),
                // BACnet/IP 没有「连接」：每条报文一个数据报，所以连接超时只用于
                // 「首条报文发出前」的保底，取与响应超时相同的量级即可。
                ConnectTimeout = TimeSpan.FromSeconds(2),
                MaxConcurrency = 1,
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
                    },                    new()
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
            new()
            {
                Id = "s7-1",
                Name = "本机 S7 从站",
                ChannelId = "s7-tcp-1",
                Enabled = enableLocalS7,
                Interval = TimeSpan.FromMilliseconds(500),
                UnitId = 1,
                ByteOrder = ByteOrder.ABCD,
                Timeout = TimeSpan.FromSeconds(2),
                FailureThreshold = 3,
                RecoverInterval = TimeSpan.FromSeconds(10),
                // 机架/槽位是会话参数，不是点位参数：S7-1200/1500 用 0/1，
                // S7-300 常见 0/2。内置从站不校验，填默认值即可连通。
                Labels = new Dictionary<string, string>(StringComparer.Ordinal)
                {
                    ["rack"] = "0",
                    ["slot"] = "1",
                },
                Points = new List<Point>
                {
                    new()
                    {
                        Id = "temperature",
                        Name = "温度",
                        // S7 的 offset 是**字节**偏移，不是寄存器序号。
                        Address = Address.Register("db1", 0),
                        DataType = DataType.Float64,
                        Unit = "°C",
                        Deadband = 0.05,
                    },
                    new()
                    {
                        Id = "pressure",
                        Name = "压力",
                        Address = Address.Register("db1", 8),
                        DataType = DataType.Float64,
                        Unit = "kPa",
                        Deadband = 0.1,
                    },
                    new()
                    {
                        Id = "counter",
                        Name = "累计计数",
                        Address = Address.Register("db1", 16),
                        DataType = DataType.UInt32,
                        Unit = "pcs",
                    },
                    new()
                    {
                        Id = "running",
                        Name = "运行状态",
                        // 位地址：M0.0。bool 点位必须带 bit，否则驱动会拒绝。
                        Address = Address.Register("m", 0, bit: 0),
                        DataType = DataType.Bool,
                    },
                },
            },
            new()
            {
                Id = "bacnet-1",
                Name = "本机 BACnet/IP 从站",
                ChannelId = "bacnet-udp-1",
                Enabled = enableLocalBacnet,
                Interval = TimeSpan.FromMilliseconds(500),
                UnitId = 1,
                Timeout = TimeSpan.FromSeconds(2),
                FailureThreshold = 3,
                RecoverInterval = TimeSpan.FromSeconds(10),
                // deviceInstance 让探活核对 I-Am 里的实例号：同网段常有多台 BACnet 设备，
                // 不核对的话「收到任何一台的 I-Am」都会被当成探活成功。
                Labels = new Dictionary<string, string>(StringComparer.Ordinal)
                {
                    ["deviceInstance"] = "1001",
                },
                Points = new List<Point>
                {
                    // BACnet 的点位是「对象类型 + 实例 + 属性」：
                    // objectType 0 = analog-input，85 = present-value。
                    new()
                    {
                        Id = "temperature",
                        Name = "温度",
                        Address = Address.Object("0", 1, "85"),
                        DataType = DataType.Float32,
                        Unit = "°C",
                        Deadband = 0.05,
                    },
                    new()
                    {
                        Id = "pressure",
                        Name = "压力",
                        Address = Address.Object("0", 2, "85"),
                        DataType = DataType.Float32,
                        Unit = "kPa",
                        Deadband = 0.1,
                    },
                    new()
                    {
                        Id = "counter",
                        Name = "累计计数",
                        Address = Address.Object("0", 3, "85"),
                        DataType = DataType.UInt32,
                        Unit = "pcs",
                    },
                    new()
                    {
                        Id = "running",
                        Name = "运行状态",
                        // 3 = binary-input：布尔值在 BACnet 里是「值就在标签头里」的应用标签，
                        // 没有载荷字节，与寄存器线圈是完全不同的编码。
                        Address = Address.Object("3", 1, "85"),
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