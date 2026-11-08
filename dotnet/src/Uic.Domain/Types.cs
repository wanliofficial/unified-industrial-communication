namespace Uic.Domain;

/// <summary>地址的寻址范式。四类寻址统一到一个结构里，避免每加一个协议就改上层。</summary>
public enum AddressKind
{
    Register,
    Symbol,
    Object,
    File,
}

/// <summary>平台统一数据类型。协议私有类型在驱动内映射到这个集合。</summary>
public enum DataType
{
    Bool,
    Int16,
    UInt16,
    Int32,
    UInt32,
    Int64,
    UInt64,
    Float32,
    Float64,
    String,
    Bcd16,
    Bcd32,
    Bytes,
    DateTime,
}

/// <summary>32/64 位值在 16 位寄存器之间的排列。四态是现场最常见的差异来源。</summary>
public enum ByteOrder
{
    ABCD,
    CDAB,
    BADC,
    DCBA,
}

/// <summary>质量码。区分「值为 0」与「根本没读到」是排障的第一道分水岭。</summary>
public enum Quality
{
    Good,
    Uncertain,
    Stale,
    Bad,
    Timeout,
    CommError,
    AddressError,
    NotConnected,
    Disabled,
}

/// <summary>设备的配置态。与运行态分离，是「配置了但没采」能被看见的前提。</summary>
public enum ConfiguredState
{
    Enabled,
    Disabled,
}

/// <summary>设备的运行态。</summary>
public enum RuntimeState
{
    Configured,
    Connecting,
    Collecting,
    Degraded,
    Faulted,
    Disabled,
}

public static class DataTypeExtensions
{
    private static readonly Dictionary<DataType, int> Widths = new()
    {
        [DataType.Int16] = 2,
        [DataType.UInt16] = 2,
        [DataType.Int32] = 4,
        [DataType.UInt32] = 4,
        [DataType.Int64] = 8,
        [DataType.UInt64] = 8,
        [DataType.Float32] = 4,
        [DataType.Float64] = 8,
        [DataType.Bcd16] = 2,
        [DataType.Bcd32] = 4,
        [DataType.DateTime] = 8,
    };

    /// <summary>字节宽度；bool 为 0（按位寻址），string/bytes 为 -1（需显式长度）。</summary>
    public static int ByteWidth(this DataType t) => Widths.TryGetValue(t, out var n) ? n : -1;

    /// <summary>占用的 16 位寄存器个数。</summary>
    public static int Registers(this DataType t)
    {
        var n = t.ByteWidth();
        return n <= 0 ? 1 : (n + 1) / 2;
    }

    public static bool IsNumeric(this DataType t) => t switch
    {
        DataType.Int16 or DataType.UInt16 or DataType.Int32 or DataType.UInt32 or
        DataType.Int64 or DataType.UInt64 or DataType.Float32 or DataType.Float64 or
        DataType.Bcd16 or DataType.Bcd32 => true,
        _ => false,
    };

    /// <summary>
    /// 能否组成数组。只放行定宽数值类型：它们每个元素占同样的寄存器数，
    /// 解码时按固定步长切分即可。string/bytes 的长度靠点位字段自行声明，
    /// datetime 的线上值不是 double，硬塞进 <c>array</c> 会丢语义。
    /// </summary>
    public static bool SupportsArray(this DataType t) => t.IsNumeric();
}
