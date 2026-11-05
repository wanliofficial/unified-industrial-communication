using System.Diagnostics.CodeAnalysis;

namespace Uic.OpcUa;

/// <summary>OPC UA 配置错误。启动期抛出，宿主据此以退出码 2 终止。</summary>
public sealed class OpcUaConfigurationException : Exception
{
    public OpcUaConfigurationException(string message) : base(message) { }
    public OpcUaConfigurationException(string message, Exception inner) : base(message, inner) { }
}

/// <summary>启动或停止 OPC UA 服务失败。运行时错误走日志，进程不受影响。</summary>
public sealed class OpcUaException : Exception
{
    public OpcUaException(string message) : base(message) { }
    public OpcUaException(string message, Exception inner) : base(message, inner) { }
}

public static class Guard
{
    public static T Required<T>([NotNullWhen(true)] T? value, string name) where T : class
        => value ?? throw new OpcUaConfigurationException($"'{name}' is required");
}
