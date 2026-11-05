using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;
using Windows.UI;

namespace Uic.Ui.Pages;

/// <summary>
/// 实时监视：靠 SSE 推送增量，不是轮询。
///
/// 表格按点位 ID 复用控件（第一次出现时建行，之后只改文字），
/// 1000 点场景下每秒重建上千个 TextBlock 会直接把浏览器拖死。
/// </summary>
public sealed class MonitorPage : IPage, IDisposable
{
    private const int MaxRows = 300;

    private readonly GatewayApi _api;
    private readonly StackPanel _rows = new() { Spacing = 0 };
    private readonly TextBlock _status;
    private readonly TextBlock _rateText;
    private readonly TextBox _filter;
    private readonly Dictionary<string, LiveRow> _live = new(StringComparer.Ordinal);

    private CancellationTokenSource? _streamCts;
    private bool _disposed;
    private bool _running;
    private long _events;
    private DateTimeOffset _lastRateAt = DateTimeOffset.UtcNow;

    public MonitorPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("监视", "SSE 实时推送；行按首次出现的顺序固定，值就地更新");
        _status = Ui.Caption("未连接");
        _rateText = Ui.Caption(string.Empty);

        _filter = Ui.Input(placeholder: "按点位过滤…");
        _filter.Width = 200;
        _filter.TextChanged += (_, _) => ApplyFilter();

        actions.Children.Add(_filter);
        actions.Children.Add(Ui.Button("清空", (_, _) => Clear(), accent: false));

        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(1.4), "点位"),
            (Ui.Star(1.0), "值"),
            (Ui.Star(0.7), "质量"),
            (Ui.Star(0.6), "单位"),
            (Ui.Star(0.9), "时刻"),
            (Ui.Star(0.6), "序号")));
        table.Children.Add(_rows);

        var card = Ui.Panel(Ui.Scroll(table), new Thickness(0));

        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.Children.Add(card);

        var footer = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            Spacing = 16,
            Margin = new Thickness(2, 8, 0, 0),
        };
        footer.Children.Add(_status);
        footer.Children.Add(_rateText);

        Grid.SetRow(footer, 1);
        body.Children.Add(footer);

        View = root;
    }

    public FrameworkElement View { get; }

    public Task RefreshAsync()
    {
        // 监视页不能「刷新」：断流重连会导致丢事件窗口，重启一条新流即可。
        if (!_running) Start();
        return Task.CompletedTask;
    }

    private void Start()
    {
        if (_disposed || _running) return;

        _running = true;
        _streamCts = new CancellationTokenSource();
        _ = PumpAsync(_streamCts.Token);
    }

    private async Task PumpAsync(CancellationToken ct)
    {
        var backoff = TimeSpan.FromSeconds(1);

        while (!ct.IsCancellationRequested && !_disposed)
        {
            SetStatus("连接中…", Ui.Faint);

            try
            {
                await foreach (var evt in _api.StreamAsync(ct: ct).WithCancellation(ct))
                {
                    if (_disposed || ct.IsCancellationRequested) return;

                    backoff = TimeSpan.FromSeconds(1);

                    if (evt.Type == "sample")
                    {
                        ApplySample(evt.Data);
                    }
                    else if (evt.Type == "hello")
                    {
                        SetStatus("已连接", Ui.Good);
                    }
                    else if (evt.Type == "quality")
                    {
                        _events++;
                    }
                }

                // 服务端正常关闭（网关重启）：退避后重连。
                SetStatus("连接已关闭，正在重连…", UiFormat.Color("#B26A00"));
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                return;
            }
            catch (Exception ex)
            {
                if (_disposed) return;

                SetStatus("断开：" + Shorten(ex.Message), Ui.Bad);
            }

            UpdateRate();

            try
            {
                await Task.Delay(backoff, ct);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            backoff = TimeSpan.FromSeconds(Math.Min(backoff.TotalSeconds * 2, 15));
        }
    }

    private void ApplySample(string data)
    {
        if (!TryParseSample(data, out var sample)) return;

        _events++;

        var key = sample.Device + "/" + sample.Point;

        if (!_live.TryGetValue(key, out var row))
        {
            if (_live.Count >= MaxRows)
            {
                SetStatus($"已达到 {MaxRows} 行上限，其余点位不再追加（用过滤框聚焦）", UiFormat.Color("#B26A00"));
                return;
            }

            row = CreateRow(key);
            _live[key] = row;
            _rows.Children.Add(row.Container);
        }

        row.Device.Text = sample.Device;
        row.Point.Text = sample.Point;
        row.Value.Text = sample.Display;
        row.Quality.Child = Ui.Text(sample.Quality, 11,
            UiFormat.Color(UiFormat.QualityColor(sample.Quality)), bold: true);
        row.Quality.Background = new SolidColorBrush(Alpha(24, UiFormat.Color(UiFormat.QualityColor(sample.Quality))));
        row.Unit.Text = sample.Unit ?? "—";
        row.Time.Text = UiFormat.Time(sample.CollectedAt);
        row.Seq.Text = sample.Seq.ToString();

        // 质量色条：一眼看出哪些点在掉。
        row.Accent.Background = new SolidColorBrush(UiFormat.Color(UiFormat.QualityColor(sample.Quality)));

        ApplyFilterToRow(row, _filter.Text);

        if (_events % 20 == 0)
        {
            SetStatus($"已连接 · {_live.Count} 个点位", Ui.Good);
            UpdateRate();
        }
    }

    private LiveRow CreateRow(string key)
    {
        var accent = new Border
        {
            Width = 3,
            Background = new SolidColorBrush(Ui.Faint),
            CornerRadius = new CornerRadius(2),
            Margin = new Thickness(0, 2, 8, 2),
        };

        var device = Ui.Text(string.Empty, 10.5, Ui.Faint);
        var point = Ui.Text(string.Empty, 12.5, Ui.Ink, mono: true);

        var label = new StackPanel { Spacing = 1 };
        label.Children.Add(point);
        label.Children.Add(device);

        var labelWithBar = new StackPanel { Orientation = Orientation.Horizontal };
        labelWithBar.Children.Add(accent);
        labelWithBar.Children.Add(label);

        var value = Ui.Text("—", 12.5, Ui.Ink, mono: true);
        var quality = Ui.Chip("—", Ui.Faint);
        var unit = Ui.Text("—", 11.5, Ui.Faint);
        var time = Ui.Text("—", 11.5, Ui.Faint, mono: true);
        var seq = Ui.Text("—", 11, Ui.Faint, mono: true);

        var container = Ui.Row(
            (Ui.Star(1.4), labelWithBar),
            (Ui.Star(1.0), value),
            (Ui.Star(0.7), quality),
            (Ui.Star(0.6), unit),
            (Ui.Star(0.9), time),
            (Ui.Star(0.6), seq));

        return new LiveRow(container, accent, device, point, value, quality, unit, time, seq);
    }

    private void ApplyFilter()
    {
        foreach (var kv in _live) ApplyFilterToRow(kv.Value, _filter.Text);
    }

    private static void ApplyFilterToRow(LiveRow row, string? filter)
    {
        var text = filter?.Trim();

        if (string.IsNullOrEmpty(text))
        {
            row.Container.Visibility = Visibility.Visible;
            return;
        }

        row.Container.Visibility =
            row.Point.Text.Contains(text, StringComparison.OrdinalIgnoreCase) ||
            row.Device.Text.Contains(text, StringComparison.OrdinalIgnoreCase)
                ? Visibility.Visible
                : Visibility.Collapsed;
    }

    private void Clear()
    {
        _live.Clear();
        _rows.Children.Clear();
        _events = 0;
        SetStatus(_running ? "已连接" : "未连接", Ui.Faint);
    }

    private void SetStatus(string text, Color color)
    {
        _status.Text = text;
        _status.Foreground = new SolidColorBrush(color);
    }

    private void UpdateRate()
    {
        var now = DateTimeOffset.UtcNow;
        var elapsed = (now - _lastRateAt).TotalSeconds;

        if (elapsed < 1) return;

        var rate = _events / elapsed;
        _rateText.Text = rate >= 1 ? $"{rate:0} 事件/秒" : string.Empty;

        _events = 0;
        _lastRateAt = now;
    }

    private static string Shorten(string text)
        => text.Length <= 70 ? text : text[..70] + "…";

    /// <summary>
    /// SSE 的 data 是 JSON，这里只取需要的字段。
    /// 用 System.Text.Json 的 DOM 而不是强类型反序列化：契约新增字段时不会抛异常。
    /// </summary>
    private static bool TryParseSample(string data, out LiveSample sample)
    {
        sample = default;

        try
        {
            using var doc = System.Text.Json.JsonDocument.Parse(data);
            var root = doc.RootElement;

            var device = root.TryGetProperty("device", out var d) ? d.GetString() ?? "?" : "?";
            var point = root.TryGetProperty("point", out var p) ? p.GetString() ?? "?" : "?";
            var quality = root.TryGetProperty("quality", out var q) ? q.GetString() ?? "bad" : "bad";
            var unit = root.TryGetProperty("unit", out var u) && u.ValueKind == System.Text.Json.JsonValueKind.String
                ? u.GetString()
                : null;
            var seq = root.TryGetProperty("seq", out var s) && s.TryGetUInt64(out var seqValue) ? seqValue : 0;

            var collectedAt = DateTimeOffset.UtcNow;
            if (root.TryGetProperty("collectedAt", out var c) && c.ValueKind == System.Text.Json.JsonValueKind.String &&
                DateTimeOffset.TryParse(c.GetString(), out var parsed))
            {
                collectedAt = parsed;
            }

            var display = "—";
            var type = "uint16";

            if (root.TryGetProperty("value", out var v))
            {
                if (v.TryGetProperty("type", out var t) && t.ValueKind == System.Text.Json.JsonValueKind.String)
                {
                    type = t.GetString() ?? type;
                }

                display = DisplayOf(v);
            }

            sample = new LiveSample(device, point, display, type, quality, unit, collectedAt, seq);
            return true;
        }
        catch
        {
            return false;
        }
    }

    private static string DisplayOf(System.Text.Json.JsonElement value)
    {
        if (value.TryGetProperty("bool", out var b) &&
            b.ValueKind is System.Text.Json.JsonValueKind.True or System.Text.Json.JsonValueKind.False)
        {
            return b.GetBoolean() ? "true" : "false";
        }

        if (value.TryGetProperty("text", out var t) && t.ValueKind == System.Text.Json.JsonValueKind.String)
        {
            return t.GetString() ?? "—";
        }

        if (value.TryGetProperty("num", out var n))
        {
            if (n.ValueKind == System.Text.Json.JsonValueKind.Number && n.TryGetDouble(out var number))
            {
                return FormatNumber(number);
            }

            if (n.ValueKind == System.Text.Json.JsonValueKind.String &&
                double.TryParse(n.GetString(), System.Globalization.NumberStyles.Float,
                    System.Globalization.CultureInfo.InvariantCulture, out var parsed))
            {
                return FormatNumber(parsed);
            }
        }

        return "—";
    }

    private static string FormatNumber(double value)
        => Math.Abs(value - Math.Round(value)) < 1e-9
            ? value.ToString("0", System.Globalization.CultureInfo.InvariantCulture)
            : value.ToString("0.###", System.Globalization.CultureInfo.InvariantCulture);

    private static Color Alpha(byte alpha, Color color) => Color.FromArgb(alpha, color.R, color.G, color.B);

    public void Dispose()
    {
        _disposed = true;
        _running = false;

        try
        {
            _streamCts?.Cancel();
        }
        catch
        {
            // 取消已释放的 CTS 不致命。
        }

        _streamCts?.Dispose();
        _streamCts = null;
    }

    private readonly record struct LiveSample(
        string Device,
        string Point,
        string Display,
        string Type,
        string Quality,
        string? Unit,
        DateTimeOffset CollectedAt,
        ulong Seq);

    private sealed record LiveRow(
        Border Container,
        Border Accent,
        TextBlock Device,
        TextBlock Point,
        TextBlock Value,
        Border Quality,
        TextBlock Unit,
        TextBlock Time,
        TextBlock Seq);
}
