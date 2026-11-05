using System.Collections.Concurrent;

namespace Uic.Driver;

/// <summary>
/// 驱动注册表。第一版是编译期静态注册；第二版走进程外 RPC。
/// 顺序不能反——先证明架构，再谈生态。
/// </summary>
public sealed class DriverRegistry
{
    private readonly ConcurrentDictionary<string, IDriver> _drivers = new(StringComparer.Ordinal);

    public void Register(IDriver driver)
    {
        var id = driver.Meta.Id;
        if (string.IsNullOrWhiteSpace(id))
            throw new ArgumentException("driver meta.id must not be empty", nameof(driver));

        if (driver.Meta.SdkVersion != DriverSdk.Version)
        {
            throw new InvalidOperationException(
                $"driver '{id}' targets SDK {driver.Meta.SdkVersion}, host requires {DriverSdk.Version}");
        }

        if (!_drivers.TryAdd(id, driver))
            throw new InvalidOperationException($"driver '{id}' is already registered");
    }

    public void Register(params IDriver[] drivers)
    {
        foreach (var d in drivers) Register(d);
    }

    public IDriver Get(string id) =>
        _drivers.TryGetValue(id, out var d)
            ? d
            : throw new KeyNotFoundException($"driver '{id}' is not registered");

    public bool TryGet(string id, out IDriver driver) => _drivers.TryGetValue(id, out driver!);

    public IReadOnlyList<DriverMeta> List() =>
        _drivers.Values.Select(d => d.Meta).OrderBy(m => m.Id, StringComparer.Ordinal).ToArray();
}
