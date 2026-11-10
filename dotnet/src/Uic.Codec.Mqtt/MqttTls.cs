using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Uic.Domain;

namespace Uic.Codec.Mqtt;

/// <summary>
/// MQTT 自定义 CA 装载。
///
/// 放在协议层而不是某一侧调用方：北向发布与南向订阅都要校验同一个自签 Broker，
/// 两边的失败语义必须一致——「CA 读不出来」是配置问题，不是运行期偶发。
/// 错误里带上调用方给的字段路径，这样同一个实现能在两份配置树里都指出该改哪一行。
/// </summary>
public static class MqttTls
{
    /// <summary>空文件返回空集合，调用方退回系统信任库。</summary>
    public static X509Certificate2Collection LoadRoots(
        string? caFile,
        string? dataDir,
        string fieldPath)
    {
        var roots = new X509Certificate2Collection();
        if (string.IsNullOrWhiteSpace(caFile)) return roots;

        var path = Resolve(caFile, dataDir);

        try
        {
            roots.ImportFromPemFile(path);
            return roots;
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException
                                       or CryptographicException or ArgumentException)
        {
            throw new ConfigException("MQTT CA certificate is not usable", new[]
            {
                new ValidationProblem(fieldPath, $"'{path}': {ex.Message}"),
            });
        }
    }

    /// <summary>
    /// 相对路径按数据目录解析。与 <c>Uic.Config.Secrets.ResolvePath</c> 同义，
    /// 但协议层不引用配置层——否则驱动就得为了一个路径拼接依赖整个配置模块。
    /// </summary>
    private static string Resolve(string path, string? dataDir)
        => Path.IsPathRooted(path)
            ? path
            : Path.Combine(dataDir ?? Environment.CurrentDirectory, path);
}
