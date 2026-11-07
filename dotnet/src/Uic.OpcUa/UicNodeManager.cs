using Microsoft.Extensions.Logging;
using Opc.Ua;
using Opc.Ua.Server;
using Uic.Domain;
using Uic.Engine;
using DomainType = Uic.Domain.DataType;
using DomainValue = Uic.Domain.DataValue;

namespace Uic.OpcUa;

/// <summary>
/// 地址空间：把配置里的设备点位映射成可浏览、可订阅、可写的变量节点。
///
/// 结构（与北向契约 §7.1 一致）：
/// <code>
/// Objects
///  └── UIC                        Folder，本系统入口
///       └── {site}                Folder，站点
///            └── {device}         Object（DeviceType）
///                 ├── _state      String，设备运行态
///                 ├── _quality    String，设备整体质量
///                 └── {point}     Variable，类型按点位映射
/// </code>
/// NodeId 统一用字符串：ns=2;s={site}.{device}.{point}。用字符串而不是数字，
/// 是因为它要在配置迭代、服务重启后保持稳定——数字标识一重建就漂移，
/// SCADA 侧的绑定会集体失效。
///
/// 值来自 <see cref="PointCache"/>，不直接连设备：这样 OPC UA 与 REST/MQTT 看到的
/// 是同一份最后已知值，客户端订阅也不会触发额外采集。
///
/// 写入走 <see cref="Scheduler.WriteAsync"/>，与 REST 写入是同一条路径，
/// 因此串行化、超时、审计语义完全一致。
/// </summary>
public sealed class UicNodeManager : CustomNodeManager2
{
    private sealed class PointBinding
    {
        public required string DeviceId { get; init; }
        public required string PointId { get; init; }
        public required DomainType Type { get; init; }
        public required Uic.Driver.PointRef Ref { get; init; }
        public required BaseDataVariableState Node { get; init; }
    }

    private sealed class DeviceBinding
    {
        public required string DeviceId { get; init; }
        public required string Site { get; init; }
        public required BaseObjectState Node { get; init; }
        public required BaseDataVariableState StateNode { get; init; }
        public required BaseDataVariableState QualityNode { get; init; }
    }

    private const string DeviceTypeName = "DeviceType";

    private readonly PointCache _cache;
    private readonly Scheduler _engine;
    private readonly ILogger _log;
    private readonly bool _allowWrite;
    private readonly string _defaultSite;
    private readonly Dictionary<(string Device, string Point), PointBinding> _points = new();
    private readonly Dictionary<NodeId, PointBinding> _byNode = new();
    private readonly Dictionary<string, DeviceBinding> _devices = new(StringComparer.Ordinal);
    private readonly Dictionary<string, string> _signatures = new(StringComparer.Ordinal);

    private FolderState _root = null!;
    private BaseObjectTypeState _deviceType = null!;
    private FolderState? _siteNode;
    private string _siteName = string.Empty;
    private ConfigDocument _config;
    private IDisposable? _subscription;
    private long _writes;
    private long _writeFailures;

    public UicNodeManager(
        IServerInternal server,
        ApplicationConfiguration configuration,
        ILogger log,
        string namespaceUri,
        string site,
        PointCache cache,
        Scheduler engine,
        ConfigDocument config,
        bool allowWrite)
        : base(server, configuration, log, namespaceUri)
    {
        _log = log;
        _defaultSite = site;
        _cache = cache;
        _engine = engine;
        _config = config;
        _allowWrite = allowWrite;
    }

    public long Writes => Interlocked.Read(ref _writes);
    public long WriteFailures => Interlocked.Read(ref _writeFailures);

    /// <summary>开始跟踪缓存变化。地址空间建好之后再调用，避免节点还不存在时更新。</summary>
    public void Start() => _subscription ??= _cache.Subscribe(OnSample);

    public override void CreateAddressSpace(IDictionary<NodeId, IList<IReference>> externalReferences)
    {
        try
        {
            CreateAddressSpaceCore(externalReferences);
        }
        catch (Exception ex)
        {
            _log.LogError(ex, "building the OPC UA address space failed");
            throw;
        }
    }

    private void CreateAddressSpaceCore(IDictionary<NodeId, IList<IReference>> externalReferences)
    {
        lock (Lock)
        {
            if (!externalReferences.TryGetValue(ObjectIds.ObjectsFolder, out var objects))
            {
                externalReferences[ObjectIds.ObjectsFolder] = objects = new List<IReference>();
            }

            if (!externalReferences.TryGetValue(ObjectIds.ObjectTypesFolder, out var types))
            {
                externalReferences[ObjectIds.ObjectTypesFolder] = types = new List<IReference>();
            }

            _deviceType = new BaseObjectTypeState
            {
                SymbolicName = DeviceTypeName,
                NodeId = new NodeId(DeviceTypeName, NamespaceIndex),
                BrowseName = new QualifiedName(DeviceTypeName, NamespaceIndex),
                DisplayName = new LocalizedText(DeviceTypeName),
                Description = new LocalizedText("设备对象：包含运行态、质量与点位集合"),
                SuperTypeId = ObjectTypeIds.FolderType,
                IsAbstract = false,
                WriteMask = AttributeWriteMask.None,
                UserWriteMask = AttributeWriteMask.None,
            };

            types.Add(new NodeStateReference(ReferenceTypeIds.HasSubtype, false, _deviceType.NodeId));
            AddPredefinedNode(SystemContext, _deviceType);

            _root = new FolderState(null)
            {
                SymbolicName = "UIC",
                ReferenceTypeId = ReferenceTypeIds.Organizes,
                TypeDefinitionId = ObjectTypeIds.FolderType,
                NodeId = new NodeId("UIC", NamespaceIndex),
                BrowseName = new QualifiedName("UIC", NamespaceIndex),
                DisplayName = new LocalizedText("UIC"),
                Description = new LocalizedText("网关点位地址空间"),
                WriteMask = AttributeWriteMask.None,
                UserWriteMask = AttributeWriteMask.None,
                EventNotifier = EventNotifiers.None,
            };

            objects.Add(new NodeStateReference(ReferenceTypeIds.Organizes, false, _root.NodeId));
            AddPredefinedNode(SystemContext, _root);

            Apply(_config, force: true);
        }
    }

    /// <summary>
    /// 热更新：设备/点位集合变化时重建对应子树；只有单位、死区等元数据变化时不重建，
    /// 以免客户端已建立的 MonitoredItem 失效。
    /// </summary>
    public void UpdateConfig(ConfigDocument config)
    {
        lock (Lock)
        {
            _config = config;
            Apply(config, force: false);
        }
    }

    private void Apply(ConfigDocument config, bool force)
    {
        var site = string.IsNullOrWhiteSpace(config.Site) ? _defaultSite : config.Site;

        // 站点变了等于整套地址空间换名，只能整体重建。
        if (_siteNode is not null && !string.Equals(site, _siteName, StringComparison.Ordinal))
        {
            foreach (var deviceId in _devices.Keys.ToArray()) RemoveDevice(deviceId);
            foreach (var key in _signatures.Keys.ToArray()) _signatures.Remove(key);
            force = true;
        }

        var siteNode = EnsureSite(site);

        foreach (var device in config.Devices.Where(d => d.Enabled))
        {
            var signature = Signature(device);
            if (!force && _signatures.TryGetValue(device.Id, out var old) && old == signature)
            {
                continue;
            }

            if (_devices.ContainsKey(device.Id)) RemoveDevice(device.Id);
            AddDevice(siteNode, site, device);
            _signatures[device.Id] = signature;
        }

        foreach (var removed in _signatures.Keys
                     .Except(config.Devices.Where(d => d.Enabled).Select(d => d.Id), StringComparer.Ordinal)
                     .ToArray())
        {
            if (_devices.ContainsKey(removed)) RemoveDevice(removed);
            _signatures.Remove(removed);
        }
    }

    private FolderState EnsureSite(string site)
    {
        if (_siteNode is not null && string.Equals(site, _siteName, StringComparison.Ordinal))
        {
            return _siteNode;
        }

        var node = new FolderState(_root)
        {
            SymbolicName = site,
            ReferenceTypeId = ReferenceTypeIds.Organizes,
            TypeDefinitionId = ObjectTypeIds.FolderType,
            NodeId = new NodeId(site, NamespaceIndex),
            BrowseName = new QualifiedName(site, NamespaceIndex),
            DisplayName = new LocalizedText(site),
            WriteMask = AttributeWriteMask.None,
            UserWriteMask = AttributeWriteMask.None,
            EventNotifier = EventNotifiers.None,
        };

        _root.AddChild(node);
        AddPredefinedNode(SystemContext, node);

        // 旧站点节点此时已经没有子节点：先摘引用再从预定义节点里移除。
        if (_siteNode is not null)
        {
            _root.RemoveChild(_siteNode);
            RemovePredefinedNode(SystemContext, _siteNode, new List<LocalReference>());
        }

        _siteNode = node;
        _siteName = site;
        return node;
    }

    private static string Signature(Device device) => string.Join('|', device.Points
        .Where(p => p.Enabled)
        .Select(p => $"{p.Id}:{p.DataType}:{p.Writable}:{p.Name}:{p.Unit}"));

    private void AddDevice(FolderState parent, string site, Device device)
    {
        var node = new BaseObjectState(parent)
        {
            SymbolicName = device.Id,
            ReferenceTypeId = ReferenceTypeIds.Organizes,
            TypeDefinitionId = _deviceType.NodeId,
            NodeId = new NodeId($"{site}.{device.Id}", NamespaceIndex),
            BrowseName = new QualifiedName(device.Id, NamespaceIndex),
            DisplayName = new LocalizedText(device.Name ?? device.Id),
            Description = new LocalizedText($"{device.ChannelId} / unit {device.UnitId}"),
            WriteMask = AttributeWriteMask.None,
            UserWriteMask = AttributeWriteMask.None,
            EventNotifier = EventNotifiers.None,
        };

        var stateNode = CreateReadOnlyVariable(node, $"{site}.{device.Id}._state", "_state",
            new LocalizedText("运行态"), DataTypeIds.String,
            _engine.Find(device.Id) is { } actor ? JsonEnum.ToWire(actor.State) : "configured");

        var qualityNode = CreateReadOnlyVariable(node, $"{site}.{device.Id}._quality", "_quality",
            new LocalizedText("质量"), DataTypeIds.String, "bad");

        foreach (var point in device.Points.Where(p => p.Enabled))
        {
            var pointNode = new BaseDataVariableState(node)
            {
                SymbolicName = point.Id,
                ReferenceTypeId = ReferenceTypeIds.HasComponent,
                TypeDefinitionId = VariableTypeIds.BaseDataVariableType,
                NodeId = new NodeId($"{site}.{device.Id}.{point.Id}", NamespaceIndex),
                BrowseName = new QualifiedName(point.Id, NamespaceIndex),
                DisplayName = new LocalizedText(point.Name ?? point.Id),
                Description = point.Unit is null ? null : new LocalizedText($"unit: {point.Unit}"),
                DataType = MapDataType(point.DataType),
                ValueRank = ValueRanks.Scalar,
                AccessLevel = (byte)(point.Writable && _allowWrite
                    ? AccessLevels.CurrentRead | AccessLevels.CurrentWrite
                    : AccessLevels.CurrentRead),
                UserAccessLevel = (byte)(point.Writable && _allowWrite
                    ? AccessLevels.CurrentRead | AccessLevels.CurrentWrite
                    : AccessLevels.CurrentRead),
                Historizing = false,
                MinimumSamplingInterval = 100,
                StatusCode = StatusCodes.BadWaitingForInitialData,
                Timestamp = DateTime.UtcNow,
                Value = null,
            };

            // 缓存里已有的值先灌进去：客户端连上来不必等到下一个采集周期。
            var existing = _cache.Get(device.Id, point.Id);
            if (existing is not null && existing.Value is not null)
            {
                try
                {
                    pointNode.Value = ToClr(existing.Value, point.DataType);
                    pointNode.StatusCode = MapQuality(existing.Quality);
                    pointNode.Timestamp = (existing.DeviceTime ?? existing.CollectedAt).UtcDateTime;
                }
                catch (Exception ex)
                {
                    _log.LogDebug(ex, "point {Device}/{Point} has a value that cannot be mapped to OPC UA",
                        device.Id, point.Id);
                }
            }

            var binding = new PointBinding
            {
                DeviceId = device.Id,
                PointId = point.Id,
                Type = point.DataType,
                Ref = Uic.Driver.PointRef.Of(device, point),
                Node = pointNode,
            };

            node.AddChild(pointNode);
            _points[(device.Id, point.Id)] = binding;
            _byNode[pointNode.NodeId] = binding;
        }

        parent.AddChild(node);
        AddPredefinedNode(SystemContext, node);

        _devices[device.Id] = new DeviceBinding
        {
            DeviceId = device.Id,
            Site = site,
            Node = node,
            StateNode = stateNode,
            QualityNode = qualityNode,
        };

        _log.LogInformation("opc ua node for device {Device} added ({Points} point(s))",
            device.Id, device.Points.Count(p => p.Enabled));
    }

    private BaseDataVariableState CreateReadOnlyVariable(
        NodeState parent,
        string identifier,
        string name,
        LocalizedText displayName,
        NodeId dataType,
        object? initial)
    {
        var node = new BaseDataVariableState(parent)
        {
            SymbolicName = name,
            ReferenceTypeId = ReferenceTypeIds.HasComponent,
            TypeDefinitionId = VariableTypeIds.BaseDataVariableType,
            NodeId = new NodeId(identifier, NamespaceIndex),
            BrowseName = new QualifiedName(name, NamespaceIndex),
            DisplayName = displayName,
            DataType = dataType,
            ValueRank = ValueRanks.Scalar,
            AccessLevel = AccessLevels.CurrentRead,
            UserAccessLevel = AccessLevels.CurrentRead,
            Historizing = false,
            MinimumSamplingInterval = 1000,
            StatusCode = StatusCodes.Good,
            Timestamp = DateTime.UtcNow,
            Value = initial,
        };

        parent.AddChild(node);
        return node;
    }

    private void RemoveDevice(string deviceId)
    {
        var points = _points.Where(kv => kv.Key.Device == deviceId).Select(kv => kv.Value).ToArray();
        foreach (var point in points)
        {
            _points.Remove((point.DeviceId, point.PointId));
            _byNode.Remove(point.Node.NodeId);
            point.Node.Parent?.RemoveChild(point.Node);
        }

        if (!_devices.Remove(deviceId, out var device)) return;

        _byNode.Remove(device.StateNode.NodeId);
        _byNode.Remove(device.QualityNode.NodeId);
        device.Node.Parent?.RemoveChild(device.Node);

        RemovePredefinedNode(SystemContext, device.Node, new List<LocalReference>());
        _log.LogInformation("opc ua node for device {Device} removed", deviceId);
    }

    private void OnSample(Sample sample)
    {
        lock (Lock)
        {
            if (!_points.TryGetValue((sample.DeviceId, sample.PointId), out var binding)) return;

            var node = binding.Node;
            var status = MapQuality(sample.Quality);
            var timestamp = (sample.DeviceTime ?? sample.CollectedAt).UtcDateTime;

            object? value = node.Value;
            if (sample.Value is not null)
            {
                try
                {
                    value = ToClr(sample.Value, binding.Type);
                }
                catch (Exception ex)
                {
                    _log.LogDebug(ex, "cannot map sample for {Device}/{Point} to OPC UA",
                        sample.DeviceId, sample.PointId);
                    value = null;
                    status = StatusCodes.BadTypeMismatch;
                }
            }

            var changed = !Equals(node.Value, value) ||
                          node.StatusCode != status ||
                          Math.Abs((node.Timestamp - timestamp).TotalMilliseconds) >= 1;

            if (changed)
            {
                node.Value = value;
                node.StatusCode = status;
                node.Timestamp = timestamp;
                node.ClearChangeMasks(SystemContext, false);
            }

            UpdateDeviceQuality(sample.DeviceId);
        }
    }

    private void UpdateDeviceQuality(string deviceId)
    {
        if (!_devices.TryGetValue(deviceId, out var device)) return;

        var samples = _cache.ForDevice(deviceId);
        if (samples.Count == 0) return;

        var actor = _engine.Find(deviceId);
        SetString(device.StateNode, actor is null ? "unknown" : JsonEnum.ToWire(actor.State));

        var worst = samples.OrderBy(s => QualityRank(s.Quality)).First();
        SetString(device.QualityNode, JsonEnum.ToWire(worst.Quality));
    }

    /// <summary>排序用：数字越大越差。设备整体质量取最差的那一个点位。</summary>
    private static int QualityRank(Quality quality) => quality switch
    {
        Quality.Good => 0,
        Quality.Uncertain => 1,
        Quality.Stale => 2,
        Quality.Disabled => 3,
        Quality.Bad => 4,
        Quality.Timeout => 5,
        Quality.CommError => 6,
        Quality.AddressError => 7,
        Quality.NotConnected => 8,
        _ => 9,
    };

    private void SetString(BaseDataVariableState node, string value)
    {
        if (string.Equals(node.Value as string, value, StringComparison.Ordinal)) return;

        node.Value = value;
        node.StatusCode = StatusCodes.Good;
        node.Timestamp = DateTime.UtcNow;
        node.ClearChangeMasks(SystemContext, false);
    }

    // ------------------------------------------------------------------
    // 写
    // ------------------------------------------------------------------

    /// <summary>
    /// 写入拦截。OPC UA 的 Write 是同步服务，而采集写入是异步且可能等待连接，
    /// 因此这里用有界等待（8 秒）把结果翻译成 StatusCode。宁可让客户端收到
    /// BadTimeout，也不把服务端请求线程永久占住。
    /// </summary>
    public override void Write(OperationContext context, IList<WriteValue> nodesToWrite, IList<ServiceResult> errors)
    {
        for (var i = 0; i < nodesToWrite.Count; i++)
        {
            var write = nodesToWrite[i];
            if (write.Processed) continue;

            PointBinding? binding;
            lock (Lock)
            {
                _byNode.TryGetValue(write.NodeId, out binding);
            }

            if (binding is null)
            {
                // 本命名空间内的非点位节点（设备对象、_state/_quality）不可写；
                // 别的命名空间不归本管理器，保持 Processed=false 交给其它 node manager。
                if (write.NodeId.NamespaceIndex != NamespaceIndex) continue;

                write.Processed = true;
                _log.LogDebug("opc ua write rejected for non-point node {NodeId}", write.NodeId);
                errors[i] = new ServiceResult(StatusCodes.BadNotWritable);
                continue;
            }

            // Processed 必须由处理者置位：MasterNodeManager 会把未置位的项统一改写成
            // BadNodeIdUnknown，即使写入本身成功（这个坑很容易误判成「节点不存在」）。
            write.Processed = true;
            errors[i] = WritePoint(write, binding);
        }
    }

    private ServiceResult WritePoint(WriteValue write, PointBinding binding)
    {
        if (!_allowWrite || (binding.Node.UserAccessLevel & AccessLevels.CurrentWrite) == 0)
        {
            return new ServiceResult(StatusCodes.BadNotWritable);
        }

        var raw = write.Value?.Value;
        if (raw is null)
        {
            _log.LogWarning("opc ua write to {Device}/{Point} carried no value", binding.DeviceId, binding.PointId);
            return new ServiceResult(StatusCodes.BadTypeMismatch);
        }

        DomainValue value;
        try
        {
            value = FromClr(raw, binding.Type);
        }
        catch (Exception ex)
        {
            _log.LogDebug(ex, "opc ua write for {Device}/{Point} has an incompatible value",
                binding.DeviceId, binding.PointId);
            return new ServiceResult(StatusCodes.BadTypeMismatch);
        }

        Interlocked.Increment(ref _writes);

        using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(8));
        var task = _engine.WriteAsync(binding.DeviceId,
            new[] { new Uic.Driver.WriteCommand(binding.Ref, value) },
            cts.Token);

        try
        {
            if (!task.Wait(TimeSpan.FromSeconds(8)))
            {
                Interlocked.Increment(ref _writeFailures);
                _log.LogWarning("opc ua write to {Device}/{Point} timed out", binding.DeviceId, binding.PointId);
                return new ServiceResult(StatusCodes.BadTimeout);
            }

            var result = task.Result.FirstOrDefault();
            if (result is null || !result.Ok)
            {
                Interlocked.Increment(ref _writeFailures);
                _log.LogWarning("opc ua write to {Device}/{Point} failed: {Error}",
                    binding.DeviceId, binding.PointId, result?.Error ?? "no result");
                return new ServiceResult(StatusCodes.BadInternalError);
            }

            return ServiceResult.Good;
        }
        catch (Exception ex)
        {
            Interlocked.Increment(ref _writeFailures);
            _log.LogWarning(ex, "opc ua write to {Device}/{Point} failed", binding.DeviceId, binding.PointId);
            return new ServiceResult(StatusCodes.BadInternalError);
        }
    }

    // ------------------------------------------------------------------
    // 类型与质量映射
    // ------------------------------------------------------------------

    private static NodeId MapDataType(DomainType type) => type switch
    {
        DomainType.Bool => DataTypeIds.Boolean,
        DomainType.Int16 => DataTypeIds.Int16,
        DomainType.UInt16 => DataTypeIds.UInt16,
        DomainType.Int32 => DataTypeIds.Int32,
        DomainType.UInt32 => DataTypeIds.UInt32,
        DomainType.Int64 => DataTypeIds.Int64,
        DomainType.UInt64 => DataTypeIds.UInt64,
        DomainType.Float32 => DataTypeIds.Float,
        DomainType.Float64 => DataTypeIds.Double,
        DomainType.String => DataTypeIds.String,
        DomainType.Bcd16 => DataTypeIds.UInt16,
        DomainType.Bcd32 => DataTypeIds.UInt32,
        DomainType.Bytes => DataTypeIds.ByteString,
        DomainType.DateTime => DataTypeIds.DateTime,
        _ => DataTypeIds.String,
    };

    /// <summary>质量映射严格按北向契约 §7.3，不用一个笼统的 Bad 覆盖所有故障。</summary>
    public static StatusCode MapQuality(Quality quality) => quality switch
    {
        Quality.Good => StatusCodes.Good,
        Quality.Uncertain => StatusCodes.Uncertain,
        Quality.Stale => StatusCodes.UncertainLastUsableValue,
        Quality.Timeout => StatusCodes.BadTimeout,
        Quality.CommError => StatusCodes.BadCommunicationError,
        Quality.AddressError => StatusCodes.BadWaitingForInitialData,
        Quality.NotConnected => StatusCodes.BadNotConnected,
        Quality.Disabled => StatusCodes.BadOutOfService,
        Quality.Bad => StatusCodes.BadCommunicationError,
        _ => StatusCodes.BadWaitingForInitialData,
    };

    private static object ToClr(DomainValue value, DomainType type) => type switch
    {
        DomainType.Bool => value.Bool ?? value.AsBool(),
        DomainType.Int16 => (short)value.AsNumber(),
        DomainType.UInt16 => (ushort)value.AsNumber(),
        DomainType.Int32 => (int)value.AsNumber(),
        DomainType.UInt32 => (uint)value.AsNumber(),
        DomainType.Int64 => (long)value.AsNumber(),
        DomainType.UInt64 => (ulong)value.AsNumber(),
        DomainType.Float32 => (float)value.AsNumber(),
        DomainType.Float64 => value.AsNumber(),
        DomainType.String => value.Text ?? string.Empty,
        DomainType.Bcd16 => (ushort)value.AsNumber(),
        DomainType.Bcd32 => (uint)value.AsNumber(),
        DomainType.Bytes => Convert.FromBase64String(value.Text ?? string.Empty),
        // OPC UA 的 DateTime 用 System.DateTime 表达，且规定为 UTC。
        DomainType.DateTime => value.TryGetDateTime(out var instant)
            ? instant.UtcDateTime
            : DateTime.MinValue,
        _ => value.Text is not null ? DomainValue.String(value.Text) : DomainValue.Number(type, value.AsNumber()),
    };

    private static DomainValue FromClr(object raw, DomainType type)
    {
        if (type == DomainType.Bool)
        {
            return raw is bool b
                ? DomainValue.Boolean(b)
                : throw new InvalidCastException($"expected Boolean, got {raw.GetType().Name}");
        }

        if (type == DomainType.String)
        {
            return raw is string s
                ? DomainValue.String(s)
                : throw new InvalidCastException($"expected String, got {raw.GetType().Name}");
        }

        if (type == DomainType.Bytes)
        {
            return raw is byte[] bytes
                ? DomainValue.Bytes(bytes)
                : throw new InvalidCastException($"expected ByteString, got {raw.GetType().Name}");
        }

        if (type == DomainType.DateTime)
        {
            // 客户端可能送 Local 或 Unspecified，统一按 UTC 解释：
            // 同一份点位配置在不同时区的服务器上必须得到同一个时刻。
            var instant = raw switch
            {
                System.DateTime dt => dt.Kind == DateTimeKind.Local
                    ? new DateTimeOffset(dt).ToUniversalTime()
                    : new DateTimeOffset(DateTime.SpecifyKind(dt, DateTimeKind.Utc)),
                System.DateTimeOffset dto => dto.ToUniversalTime(),
                _ => throw new InvalidCastException($"expected DateTime, got {raw.GetType().Name}"),
            };

            return DomainValue.DateTime(instant);
        }

        var number = Convert.ToDouble(raw, System.Globalization.CultureInfo.InvariantCulture);

        return type switch
        {
            DomainType.Int16 => DomainValue.Number(type, Checked(number, short.MinValue, short.MaxValue, "Int16")),
            DomainType.UInt16 or DomainType.Bcd16 => DomainValue.Number(type, Checked(number, ushort.MinValue, ushort.MaxValue, "UInt16")),
            DomainType.Int32 => DomainValue.Number(type, Checked(number, int.MinValue, int.MaxValue, "Int32")),
            DomainType.UInt32 or DomainType.Bcd32 => DomainValue.Number(type, Checked(number, uint.MinValue, uint.MaxValue, "UInt32")),
            DomainType.Int64 => DomainValue.Number(type, Checked(number, long.MinValue, long.MaxValue, "Int64")),
            DomainType.UInt64 => DomainValue.Number(type, Checked(number, ulong.MinValue, ulong.MaxValue, "UInt64")),
            DomainType.Float32 => DomainValue.Number(type, Checked(number, -float.MaxValue, float.MaxValue, "Float")),
            DomainType.Float64 => DomainValue.Number(type, number),
            _ => throw new InvalidCastException($"unsupported type {type}"),
        };
    }

    private static double Checked(double value, double min, double max, string type)
    {
        if (double.IsNaN(value) || value < min || value > max)
        {
            throw new OverflowException($"{value} is outside the {type} range");
        }

        return value;
    }
}
