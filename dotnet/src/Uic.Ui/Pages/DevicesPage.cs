using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;

namespace Uic.Ui.Pages;

/// <summary>
/// 设备列表。首屏只拉汇总（/devices），展开某一行时才拉该设备的点位（/devices/{id}），
/// 否则 20 台设备 × 数百点位一次性灌进浏览器，滚动会明显卡顿。
/// </summary>
public sealed class DevicesPage : IPage, IDisposable
{
    private readonly GatewayApi _api;
    private readonly StackPanel _list = new() { Spacing = 0 };
    private readonly TextBlock _status;
    private readonly TextBox _search;
    /// <summary>展开过的设备点位。列表本身不刷新时复用，避免每次展开都重拉一遍。</summary>
    private readonly Dictionary<string, PointDto[]> _expanded = new(StringComparer.Ordinal);

    /// <summary>驱动能力缓存：能力徽标按 protocol（= 驱动 id）查表。</summary>
    private Dictionary<string, DriverDto> _drivers = new(StringComparer.Ordinal);

    private bool _disposed;
    private string? _expandedDevice;

    public DevicesPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("设备", "按通道分组的采集设备；点击一行查看点位");

        _search = Ui.Input(placeholder: "按名称或 ID 过滤…");
        _search.Width = 220;
        _search.TextChanged += (_, _) => _ = RefreshAsync();

        _status = Ui.Caption(string.Empty);

        actions.Children.Add(_search);
        actions.Children.Add(Ui.Button("刷新", (_, _) => _ = RefreshAsync(), accent: true));

        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(0.8), "状态"),
            (Ui.Star(1.5), "设备"),
            (Ui.Star(1.5), "通道 / 协议"),
            (Ui.Star(1.2), "能力"),
            (Ui.Star(0.9), "点位"),
            (Ui.Star(0.7), "成功率"),
            (Ui.Star(0.6), "P95"),
            (Ui.Star(1.4), "最近错误")));

        table.Children.Add(_list);

        var card = Ui.Panel(Ui.Scroll(table), new Thickness(0));
        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.Children.Add(card);

        Grid.SetRow(_status, 1);
        _status.Margin = new Thickness(2, 8, 0, 0);
        body.Children.Add(_status);

        View = root;
    }

    public FrameworkElement View { get; }

    public async Task RefreshAsync()
    {
        if (_disposed) return;

        try
        {
            var page = await _api.DevicesAsync();
            if (_disposed) return;

            _drivers = await LoadDriversAsync();

            var filter = _search.Text?.Trim();

            var devices = string.IsNullOrEmpty(filter)
                ? page.Items
                : page.Items
                    .Where(d => d.Id.Contains(filter, StringComparison.OrdinalIgnoreCase) ||
                                (d.Name?.Contains(filter, StringComparison.OrdinalIgnoreCase) ?? false))
                    .ToArray();

            _list.Children.Clear();

            foreach (var device in devices)
            {
                _list.Children.Add(BuildDeviceRow(device));
            }

            if (devices.Length == 0)
            {
                _list.Children.Add(new Border
                {
                    Padding = new Thickness(16, 28, 16, 28),
                    Child = Ui.Text("没有匹配的设备", 13, Ui.Faint),
                });
            }

            _status.Text = $"{devices.Length} / {page.Total} 台设备 · 更新于 {UiFormat.Time(DateTimeOffset.Now)}";
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _list.Children.Clear();
            _status.Text = "加载失败：" + ex.Message;
        }
    }

    private async Task<Dictionary<string, DriverDto>> LoadDriversAsync()
    {
        try
        {
            var drivers = await _api.DriversAsync();
            return drivers.Items.ToDictionary(d => d.Id, StringComparer.Ordinal);
        }
        catch
        {
            // 能力徽标只是增强信息：驱动端点不可用时，设备列表照常显示。
            return new Dictionary<string, DriverDto>(StringComparer.Ordinal);
        }
    }

    private UIElement BuildCapabilities(DeviceSummaryDto device)
    {
        if (!_drivers.TryGetValue(device.Protocol, out var driver))
            return Ui.Text("—", 11.5, Ui.Faint);

        var caps = driver.Capabilities;
        var panel = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 6 };

        // 只放四个最常用的能力；完整八项在「驱动」页。单字 + 悬停提示，避免挤爆列宽。
        panel.Children.Add(CapDot("读", caps.Read));
        panel.Children.Add(CapDot("写", caps.Write));
        panel.Children.Add(CapDot("订", caps.Subscribe));
        panel.Children.Add(CapDot("浏", caps.Browse));

        return panel;
    }

    private static UIElement CapDot(string label, string state)
    {
        var color = UiFormat.Color(UiFormat.CapColor(state));
        var panel = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 3 };
        panel.Children.Add(Ui.Dot(color, 6));
        panel.Children.Add(Ui.Text(label, 11, color));

        var box = new Border { Child = panel };
        ToolTipService.SetToolTip(box, $"{label}：{StateLabel(state)}");
        return box;
    }

    private static string StateLabel(string state) => state switch
    {
        "supported" => "支持",
        "runtimeDependent" => "视运行时",
        "experimental" => "实验",
        _ => "不支持",
    };

    private UIElement BuildDeviceRow(DeviceSummaryDto device)
    {
        var stateColor = UiFormat.Color(UiFormat.StateColor(device.RuntimeState));

        var state = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 6 };
        state.Children.Add(Ui.Dot(stateColor));
        state.Children.Add(Ui.Text(device.RuntimeState, 12, stateColor));

        var name = new StackPanel { Spacing = 1 };
        name.Children.Add(Ui.Text(device.Name ?? device.Id, 13, Ui.Ink, bold: true));
        if (device.Name is not null) name.Children.Add(Ui.Text(device.Id, 11, Ui.Faint, mono: true));

        var points = new StackPanel { Spacing = 1 };
        points.Children.Add(Ui.Text(device.Points.Total.ToString(), 13));
        points.Children.Add(Ui.Text($"好 {device.Points.Good} · 坏 {device.Points.Bad}", 10.5, Ui.Faint));

        var successColor = device.Stats.SuccessRate switch
        {
            >= 0.999 => Ui.Good,
            >= 0.95 => UiFormat.Color("#B26A00"),
            _ => Ui.Bad,
        };

        var row = new StackPanel { Spacing = 0 };
        row.Children.Add(Ui.Row(
            (Ui.Star(0.8), state),
            (Ui.Star(1.5), name),
            (Ui.Star(1.5), Ui.Text($"{device.Channel} · {device.Protocol}", 12, Ui.Muted, mono: true)),
            (Ui.Star(1.2), BuildCapabilities(device)),
            (Ui.Star(0.9), points),
            (Ui.Star(0.7), Ui.Text(UiFormat.Percent(device.Stats.SuccessRate), 12, successColor)),
            (Ui.Star(0.6), Ui.Text($"{device.Stats.P95DurationMs:0} ms", 12, Ui.Muted, mono: true)),
            (Ui.Star(1.4), Ui.Text(device.Stats.LastError ?? "—", 11.5,
                device.Stats.LastError is null ? Ui.Faint : Ui.Bad))));

        if (_expandedDevice == device.Id && _expanded.TryGetValue(device.Id, out var points2))
        {
            row.Children.Add(BuildExpandedPoints(points2));
        }

        // 不要用 Button 包整行：Button 的内容宿主以无限宽度测量，
        // 星号列会全部塌成内容宽度，整行挤成一团。Border 会正常拉伸。
        var container = new Border
        {
            Child = row,
            HorizontalAlignment = HorizontalAlignment.Stretch,
            Background = new SolidColorBrush(Microsoft.UI.Colors.Transparent),
        };

        container.PointerEntered += (_, _) =>
            container.Background = new SolidColorBrush(UiFormat.Color("#F7F9FB"));

        container.PointerExited += (_, _) =>
            container.Background = new SolidColorBrush(Microsoft.UI.Colors.Transparent);

        container.Tapped += async (_, _) => await ToggleAsync(device.Id);
        return container;
    }

    private async Task ToggleAsync(string deviceId)
    {
        if (_expandedDevice == deviceId)
        {
            _expandedDevice = null;
            await RefreshAsync();
            return;
        }

        try
        {
            var detail = await _api.DevicePointsAsync(deviceId);
            if (_disposed) return;

            _expanded[deviceId] = detail.Items;
            _expandedDevice = deviceId;
            await RefreshAsync();
        }
        catch (Exception ex)
        {
            _status.Text = "加载点位失败：" + ex.Message;
        }
    }

    private static UIElement BuildExpandedPoints(PointDto[] points)
    {
        var grid = new StackPanel
        {
            Spacing = 0,
            Margin = new Thickness(24, 0, 0, 10),
            Padding = new Thickness(0),
        };

        var inner = new StackPanel();
        inner.Children.Add(Ui.Header(
            (Ui.Star(1.4), "点位"),
            (Ui.Star(1.0), "值"),
            (Ui.Star(0.7), "质量"),
            (Ui.Star(0.6), "周期"),
            (Ui.Star(0.7), "单位"),
            (Ui.Star(1.2), "采集时间"),
            (Ui.Star(1.6), "名称")));

        foreach (var point in points.OrderBy(p => p.Point, StringComparer.Ordinal))
        {
            inner.Children.Add(Ui.Row(
                (Ui.Star(1.4), Ui.Text(point.Point, 12, Ui.Ink, mono: true)),
                (Ui.Star(1.0), Ui.Text(point.Value.Display, 12, Ui.Ink, mono: true)),
                (Ui.Star(0.7), Ui.Chip(point.Quality, UiFormat.Color(UiFormat.QualityColor(point.Quality)))),
                (Ui.Star(0.6), Ui.Text(UiFormat.Duration(point.EffectiveInterval), 11.5,
                    point.Interval is null ? Ui.Faint : Ui.Ink, mono: true)),
                (Ui.Star(0.7), Ui.Text(point.Unit ?? "—", 11.5, Ui.Faint)),
                (Ui.Star(1.2), Ui.Text(UiFormat.Time(point.CollectedAt), 11.5, Ui.Faint, mono: true)),
                (Ui.Star(1.6), Ui.Text(point.Name ?? "—", 11.5, Ui.Faint))));
        }

        if (points.Length == 0)
        {
            inner.Children.Add(new Border
            {
                Padding = new Thickness(16, 14, 16, 14),
                Child = Ui.Text("该设备没有点位", 12, Ui.Faint),
            });
        }

        grid.Children.Add(Ui.Panel(inner, new Thickness(0), UiFormat.Color("#FAFBFC")));
        return grid;
    }

    public void Dispose()
    {
        _disposed = true;
        _expanded.Clear();
    }
}
