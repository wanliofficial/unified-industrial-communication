using Opc.Ua;
using Uic.Domain;

namespace Uic.Driver.OpcUa;

/// <summary>
/// OPC UA 的寻址解析：把配置里的 <c>address.path</c> 文本变成 <see cref="NodeId"/>。
///
/// 为什么地址用文本而不是"命名空间号 + 标识符"两个字段：NodeId 的文本写法
/// （<c>ns=2;s=Demo.Temp</c>、<c>ns=2;i=85</c>）是 OPC UA 规范的一部分，
/// 运维在 UaExpert 里看到的、PLC 工程师发过来的就是这一串。把它拆成两个字段，
/// 现场就得把 "ns=2;s=Foo" 手工拆两半，拆错的那一半还照样能连上、只是读到空值。
///
/// 支持的形式：
/// <list type="bullet">
/// <item><c>ns=2;s=Demo.Temp</c> / <c>ns=2;i=85</c> / <c>ns=2;g=...</c> / <c>ns=2;b=...</c></item>
/// <item><c>i=85</c>、<c>s=Demo.Temp</c>：用 <see cref="Address.Index"/> 当命名空间号</item>
/// <item>裸文本 <c>Demo.Temp</c>：按字符串标识符处理，命名空间取 <see cref="Address.Index"/></item>
/// </list>
///
/// 明确不支持 <c>nsu=</c> / <c>svr=</c>：它们依赖服务端的命名空间表，
/// 而"表里的第几项"在不同服务端之间会变；这里要求写死数字索引，
/// 让配置里看到的就是抓包里看到的。
/// </summary>
public static class OpcUaAddress
{
    public const string Protocol = "opcua";

    /// <summary>解析点位地址；格式不对时抛 <see cref="DriverException"/> 的 address 类错误。</summary>
    public static NodeId Parse(Address address)
    {
        if (address.Kind != AddressKind.Symbol)
        {
            throw DriverException.Address(Protocol, "encode",
                $"OPC UA points use symbol addressing; got kind '{address.Kind}' " +
                "(write address.path as a NodeId, e.g. ns=2;s=Demo.Temp)");
        }

        if (!TryParse(address.Path, address.Index, out var nodeId, out var error))
            throw DriverException.Address(Protocol, "encode", error!);

        return nodeId!;
    }

    /// <summary>纯解析函数：用例直接覆盖各种写法，不需要服务端。</summary>
    public static bool TryParse(string? text, int defaultNamespace, out NodeId? nodeId, out string? error)
    {
        nodeId = null;
        error = null;

        var trimmed = text?.Trim();
        if (string.IsNullOrEmpty(trimmed))
        {
            error = "address.path is required: write the NodeId text, e.g. ns=2;s=Demo.Temp";
            return false;
        }

        if (defaultNamespace is < 0 or > ushort.MaxValue)
        {
            error = $"address.index must be 0..65535, got {defaultNamespace}";
            return false;
        }

        var rest = trimmed;

        // NodeId 的命名空间索引在规范里是 UInt16；用 ushort 承载，越界在解析期就暴露。
        var ns = (ushort)defaultNamespace;

        if (trimmed.StartsWith("nsu=", StringComparison.OrdinalIgnoreCase) ||
            trimmed.StartsWith("svr=", StringComparison.OrdinalIgnoreCase))
        {
            error = $"address.path '{trimmed}' uses a server-scoped namespace; " +
                    "write the numeric namespace index instead (ns=<n>;s=... / ns=<n>;i=...)";
            return false;
        }

        if (trimmed.StartsWith("ns=", StringComparison.OrdinalIgnoreCase))
        {
            var semi = trimmed.IndexOf(';');
            if (semi < 0)
            {
                error = $"address.path '{trimmed}' is missing the identifier after 'ns=<n>;'";
                return false;
            }

            var indexText = trimmed[3..semi];
            if (!ushort.TryParse(indexText, out ns))
            {
                error = $"address.path '{trimmed}' has an invalid namespace index '{indexText}' (0..65535)";
                return false;
            }

            rest = trimmed[(semi + 1)..];
        }

        if (rest.Length == 0)
        {
            error = $"address.path '{trimmed}' has no identifier";
            return false;
        }

        // "s=Foo" 这类标准写法按标识符类型解析；其它文本（含空格、点号的符号名）
        // 当成字符串标识符——现场最常见的写法是 "<site>.<device>.<point>"。
        if (rest.Length >= 2 && rest[1] == '=')
        {
            var value = rest[2..];

            switch (char.ToLowerInvariant(rest[0]))
            {
                case 'i':
                    if (!uint.TryParse(value, out var numeric))
                    {
                        error = $"address.path '{trimmed}' has an invalid numeric identifier '{value}'";
                        return false;
                    }

                    nodeId = new NodeId(numeric, ns);
                    return true;

                case 's':
                    if (value.Length == 0)
                    {
                        error = $"address.path '{trimmed}' has an empty string identifier";
                        return false;
                    }

                    nodeId = new NodeId(value, ns);
                    return true;

                case 'g':
                    if (!Guid.TryParse(value, out var guid))
                    {
                        error = $"address.path '{trimmed}' has an invalid GUID identifier '{value}'";
                        return false;
                    }

                    nodeId = new NodeId(guid, ns);
                    return true;

                case 'b':
                    try
                    {
                        nodeId = new NodeId(Convert.FromBase64String(value), ns);
                        return true;
                    }
                    catch (FormatException)
                    {
                        error = $"address.path '{trimmed}' has an invalid base64 ByteString identifier";
                        return false;
                    }
            }
        }

        nodeId = new NodeId(rest, ns);
        return true;
    }

    /// <summary>日志与错误信息里的短形式；等价于规范的 NodeId 文本。</summary>
    public static string Describe(NodeId nodeId) => nodeId.ToString();
}
