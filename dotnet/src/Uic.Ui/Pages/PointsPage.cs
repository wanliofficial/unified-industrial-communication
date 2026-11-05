using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;

namespace Uic.Ui.Pages;

/// <summary>
/// 点位表 + 写入面板。
///
/// 写入刻意做成「选中一行 → 右侧填值 → 确认」两步：工业现场一次误写可能让阀门动作，
/// 单行上的内联编辑框太容易被误触。
/// </summary>
public sealed class PointsPage : IPage, IDisposable
{
    private readonly GatewayApi _api;
    private readonly StackPanel _rows = new() { Spacing = 0 };
    private readonly ComboBox _devicePicker = new() { Width = 190, PlaceholderText = "全部设备" };
    private readonly TextBlock _status;

    private readonly TextBlock _editTarget = Ui.Text("未选择点位", 13, Ui.Faint, mono: true);
    private readonly TextBox _editValue = Ui.Input(placeholder: "输入新值", mono: true);
    private readonly TextBlock _editHint = Ui.Caption(string.Empty, wrap: true);
    private readonly TextBlock _editResult = Ui.Text(string.Empty, 12, Ui.Faint, wrap: true);
    private readonly Button _writeButton;

    private PointDto? _selected;
    private bool _disposed;
    private bool _loadingDevices;

    public PointsPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("点位", "全部点位的当前值；选一行后在右侧写入");
        _status = Ui.Caption(string.Empty);

        actions.Children.Add(_devicePicker);
        actions.Children.Add(Ui.Button("刷新", (_, _) => _ = RefreshAsync(), accent: true));

        _devicePicker.SelectionChanged += (_, _) =>
        {
            if (!_loadingDevices) _ = RefreshAsync();
        };

        // 表格
        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(1.5), "设备 / 点位"),
            (Ui.Star(0.9), "值"),
            (Ui.Star(0.8), "类型"),
            (Ui.Star(0.7), "质量"),
            (Ui.Star(0.6), "单位"),
            (Ui.Star(1.0), "采集时间"),
            (Ui.Star(0.6), "可写")));
        table.Children.Add(_rows);

        var tableCard = Ui.Panel(Ui.Scroll(table), new Thickness(0));

        // 写入面板
        _writeButton = Ui.Button("写入", OnWriteClick, accent: true);
        _writeButton.IsEnabled = false;

        var editor = new StackPanel { Spacing = 10 };
        editor.Children.Add(Ui.Text("写入点位", 14, Ui.Ink, bold: true));
        editor.Children.Add(_editTarget);
        editor.Children.Add(_editValue);
        editor.Children.Add(_editHint);
        editor.Children.Add(_writeButton);
        editor.Children.Add(_editResult);

        var editorCard = Ui.Panel(editor, new Thickness(16));
        editorCard.Width = 260;
        editorCard.VerticalAlignment = VerticalAlignment.Top;
        editorCard.Margin = new Thickness(14, 0, 0, 0);

        var split = new Grid();
        split.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });
        split.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        split.Children.Add(tableCard);

        Grid.SetColumn(editorCard, 1);
        split.Children.Add(editorCard);

        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.Children.Add(split);

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
            await LoadDevicePickerAsync();

            var selected = _devicePicker.SelectedItem as DeviceChoice;
            var page = await _api.PointsAsync(selected?.Id);
            if (_disposed) return;

            _rows.Children.Clear();

            foreach (var point in page.Items.OrderBy(p => p.Device, StringComparer.Ordinal)
                         .ThenBy(p => p.Point, StringComparer.Ordinal))
            {
                _rows.Children.Add(BuildRow(point));
            }

            if (page.Items.Length == 0)
            {
                _rows.Children.Add(new Border
                {
                    Padding = new Thickness(16, 28, 16, 28),
                    Child = Ui.Text("没有点位", 13, Ui.Faint),
                });
            }

            _status.Text = $"{page.Total} 个点位 · 更新于 {UiFormat.Time(DateTimeOffset.Now)}";

            // 选中的点位可能在刷新后消失了（设备被热更新删除），这时要清掉编辑面板。
            if (_selected is not null && page.Items.All(p =>
                    !(p.Device == _selected.Device && p.Point == _selected.Point)))
            {
                Select(null);
            }
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _rows.Children.Clear();
            _status.Text = "加载失败：" + ex.Message;
        }
    }

    private async Task LoadDevicePickerAsync()
    {
        if (_devicePicker.Items.Count > 0) return;

        _loadingDevices = true;

        try
        {
            var page = await _api.DevicesAsync();
            if (_disposed) return;

            _devicePicker.Items.Clear();
            _devicePicker.Items.Add(new DeviceChoice(null, "全部设备"));

            foreach (var device in page.Items.OrderBy(d => d.Id, StringComparer.Ordinal))
            {
                _devicePicker.Items.Add(new DeviceChoice(device.Id, device.Name ?? device.Id));
            }

            _devicePicker.SelectedIndex = 0;
        }
        finally
        {
            _loadingDevices = false;
        }
    }

    private UIElement BuildRow(PointDto point)
    {
        var selected = _selected is not null &&
                       _selected.Device == point.Device &&
                       _selected.Point == point.Point;

        var label = new StackPanel { Spacing = 1 };
        label.Children.Add(Ui.Text(point.Point, 12.5, Ui.Ink, mono: true));
        label.Children.Add(Ui.Text($"{point.Device}{(point.Name is null ? string.Empty : " · " + point.Name)}",
            10.5, Ui.Faint));

        var row = Ui.Row(
            (Ui.Star(1.5), label),
            (Ui.Star(0.9), Ui.Text(point.Value.Display, 12.5, Ui.Ink, mono: true)),
            (Ui.Star(0.8), Ui.Text(point.Value.Type, 11.5, Ui.Faint, mono: true)),
            (Ui.Star(0.7), Ui.Chip(point.Quality, UiFormat.Color(UiFormat.QualityColor(point.Quality)))),
            (Ui.Star(0.6), Ui.Text(point.Unit ?? "—", 11.5, Ui.Faint)),
            (Ui.Star(1.0), Ui.Text(UiFormat.Time(point.CollectedAt), 11.5, Ui.Faint, mono: true)),
            (Ui.Star(0.6), Ui.Text(point.Writable ? "是" : "否", 11.5,
                point.Writable ? Ui.Accent : Ui.Faint)));

        if (selected)
        {
            row.Background = new SolidColorBrush(UiFormat.Color("#EAF2FC"));
        }

        // 同 DevicesPage：Button 会以无限宽度测量，星号列会塌掉。
        var container = new Border
        {
            Child = row,
            HorizontalAlignment = HorizontalAlignment.Stretch,
            Background = new SolidColorBrush(Microsoft.UI.Colors.Transparent),
        };

        container.Tapped += (_, _) => Select(point);
        return container;
    }

    private void Select(PointDto? point)
    {
        _selected = point;
        _editResult.Text = string.Empty;
        _editValue.Text = string.Empty;

        if (point is null)
        {
            _editTarget.Text = "未选择点位";
            _editTarget.Foreground = new SolidColorBrush(Ui.Faint);
            _editHint.Text = string.Empty;
            _writeButton.IsEnabled = false;
            _ = RefreshAsync();
            return;
        }

        _editTarget.Text = $"{point.Device} / {point.Point}";
        _editTarget.Foreground = new SolidColorBrush(Ui.Ink);
        _editHint.Text = point.Writable
            ? $"类型 {point.Value.Type}，当前值 {point.Value.Display}{(point.Unit is null ? string.Empty : " " + point.Unit)}。"
            : "该点位不可写（只读地址或未开启写入）。";

        _writeButton.IsEnabled = point.Writable;
        _ = RefreshAsync();
    }

    private async void OnWriteClick(object sender, RoutedEventArgs e)
    {
        if (_selected is null) return;

        var raw = _editValue.Text?.Trim();

        if (string.IsNullOrEmpty(raw))
        {
            _editResult.Text = "请输入要写入的值。";
            _editResult.Foreground = new SolidColorBrush(Ui.Bad);
            return;
        }

        if (!TryBuildValue(_selected.Value.Type, raw, out var value, out var error))
        {
            _editResult.Text = error;
            _editResult.Foreground = new SolidColorBrush(Ui.Bad);
            return;
        }

        _writeButton.IsEnabled = false;
        _editResult.Text = "正在写入…";
        _editResult.Foreground = new SolidColorBrush(Ui.Faint);

        try
        {
            var response = await _api.WriteAsync(_selected.Device, _selected.Point, value);
            if (_disposed) return;

            var result = response.Results.FirstOrDefault();

            if (result is { Ok: true })
            {
                _editResult.Text = $"已写入 · {UiFormat.Time(result.AppliedAt)}";
                _editResult.Foreground = new SolidColorBrush(Ui.Good);
            }
            else if (result?.Quality == "uncertain")
            {
                // 命令已经下发了，只是回读对不上：多半是设备自身在覆盖该值
                // （虚拟设备每轮扫描都会重算，从站侧也可能拒绝保持）。
                // 这和「被拒绝」是完全不同的处置：前者要查设备逻辑，后者要查配置。
                _editResult.Text = $"已下发，但回读校验未通过 · {UiFormat.Time(result.AppliedAt)}";
                _editResult.Foreground = new SolidColorBrush(UiFormat.Color("#B26A00"));
            }
            else
            {
                _editResult.Text = "写入被拒绝：" + (result?.Error ?? "网关未返回结果");
                _editResult.Foreground = new SolidColorBrush(Ui.Bad);
            }
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _editResult.Text = "写入失败：" + ex.Message;
            _editResult.Foreground = new SolidColorBrush(Ui.Bad);
        }
        finally
        {
            if (!_disposed) _writeButton.IsEnabled = _selected?.Writable == true;
        }
    }

    /// <summary>
    /// 按点位类型解析输入。不写「宽松解析后截断」：现场把 300 填进 uint8 点位，
    /// 静默变成 44 比直接报错危险得多。
    /// </summary>
    private static bool TryBuildValue(string type, string raw, out DataValueDto value, out string error)
    {
        value = new DataValueDto(type, null, null, null);
        error = string.Empty;

        var culture = System.Globalization.CultureInfo.InvariantCulture;

        switch (type)
        {
            case "bool":
                if (bool.TryParse(raw, out var b))
                {
                    value = new DataValueDto(type, null, b, null);
                    return true;
                }

                if (raw is "0" or "1")
                {
                    value = new DataValueDto(type, null, raw == "1", null);
                    return true;
                }

                error = "布尔点位请填 true / false（或 1 / 0）。";
                return false;

            case "string" or "bytes":
                value = new DataValueDto(type, null, null, raw);
                return true;

            case "bcd16" or "bcd32":
            case "int16" or "uint16" or "int32" or "uint32" or "int64" or "uint64":
                if (!long.TryParse(raw, System.Globalization.NumberStyles.Integer, culture, out var integer))
                {
                    error = $"{type} 点位请填整数。";
                    return false;
                }

                if (!InIntegerRange(type, integer, out var rangeError))
                {
                    error = rangeError;
                    return false;
                }

                value = new DataValueDto(type, integer, null, null);
                return true;

            case "float32" or "float64":
                if (!double.TryParse(raw, System.Globalization.NumberStyles.Float, culture, out var number))
                {
                    error = $"{type} 点位请填数字（小数用点，例如 12.5）。";
                    return false;
                }

                value = new DataValueDto(type, number, null, null);
                return true;

            default:
                error = $"未知类型 {type}，无法写入。";
                return false;
        }
    }

    private static bool InIntegerRange(string type, long value, out string error)
    {
        error = string.Empty;

        var (min, max) = type switch
        {
            "int16" => ((long)short.MinValue, (long)short.MaxValue),
            "uint16" or "bcd16" => (0L, (long)ushort.MaxValue),
            "int32" => ((long)int.MinValue, (long)int.MaxValue),
            "uint32" or "bcd32" => (0L, (long)uint.MaxValue),
            "int64" => (long.MinValue, long.MaxValue),
            "uint64" => (0L, long.MaxValue),
            _ => (long.MinValue, long.MaxValue),
        };

        if (value >= min && value <= max) return true;

        error = $"{type} 的取值范围是 {min} … {max}，{value} 超出范围。";
        return false;
    }

    public void Dispose() => _disposed = true;

    private sealed record DeviceChoice(string? Id, string Label)
    {
        public override string ToString() => Label;
    }
}
