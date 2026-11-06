using Microsoft.UI.Xaml;

namespace Uic.Ui;

/// <summary>
/// 六个页面共用的最小约定：一个根 <see cref="FrameworkElement"/> 加一个刷新入口。
/// 页面自己管订阅的生命周期，离开时 <see cref="IDisposable.Dispose"/> 必须停止轮询。
/// </summary>
public interface IPage
{
    FrameworkElement View { get; }

    Task RefreshAsync();
}

/// <summary>与 <see cref="IPage"/> 配套的通用小工具，避免每个页面各写一遍。</summary>
public static class UiFormat
{
    public static string QualityColor(string quality) => quality switch
    {
        "good" => "#1E8E3E",
        "uncertain" or "stale" => "#B26A00",
        "disabled" => "#8A94A3",
        _ => "#C0392B",
    };

    public static string StateColor(string state) => state switch
    {
        "collecting" or "running" or "enabled" => "#1E8E3E",
        "degraded" => "#B26A00",
        "stopped" or "disabled" or "paused" => "#8A94A3",
        _ => "#C0392B",
    };

    /// <summary>
    /// 能力四态的固定配色。颜色只表示「确定程度」，不表示好坏：
    /// unsupported 是灰色而不是红色——很多协议本来就不该有订阅能力。
    /// </summary>
    public static string CapColor(string state) => state switch
    {
        "supported" => "#1E8E3E",
        "runtimeDependent" => "#B26A00",
        "experimental" => "#1D5FA8",
        _ => "#8A94A3",
    };

    public static string LevelColor(string level) => level.ToLowerInvariant() switch
    {
        "trace" or "debug" => "#8A94A3",
        "information" or "info" => "#1B2029",
        "warning" or "warn" => "#B26A00",
        "error" or "critical" => "#C0392B",
        _ => "#5B6472",
    };

    public static Windows.UI.Color Color(string hex)
    {
        var value = Convert.ToUInt32(hex.TrimStart('#'), 16);
        return Windows.UI.Color.FromArgb(
            255,
            (byte)((value >> 16) & 0xFF),
            (byte)((value >> 8) & 0xFF),
            (byte)(value & 0xFF));
    }

    public static Microsoft.UI.Xaml.Media.SolidColorBrush Brush(string hex)
        => new(Color(hex));

    public static string Time(DateTimeOffset value)
        => value.ToLocalTime().ToString("HH:mm:ss");

    public static string DateTime(DateTimeOffset value)
        => value.ToLocalTime().ToString("MM-dd HH:mm:ss");

    public static string Age(DateTimeOffset value)
    {
        var delta = DateTimeOffset.UtcNow - value.ToUniversalTime();
        if (delta < TimeSpan.Zero) delta = TimeSpan.Zero;

        return delta.TotalSeconds switch
        {
            < 1 => "刚刚",
            < 60 => $"{delta.TotalSeconds:0} 秒前",
            < 3600 => $"{delta.TotalMinutes:0} 分钟前",
            < 86400 => $"{delta.TotalHours:0} 小时前",
            _ => $"{delta.TotalDays:0} 天前",
        };
    }

    public static string Duration(TimeSpan span)
        => span.TotalMilliseconds < 1000
            ? $"{span.TotalMilliseconds:0} ms"
            : $"{span.TotalSeconds:0.##} s";

    public static string Percent(double ratio)
        => double.IsNaN(ratio) ? "—" : $"{ratio * 100:0.#}%";
}
