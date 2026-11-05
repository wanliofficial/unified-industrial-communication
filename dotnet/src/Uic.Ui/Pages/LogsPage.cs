using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;

namespace Uic.Ui.Pages;

/// <summary>
/// 日志。网关的环形缓冲在内存里，这里按 2 秒节奏拉一次增量：
/// 用 seq 做游标，只显示比上次最大 seq 更新的行，避免整表重建导致的滚动跳动。
/// </summary>
public sealed class LogsPage : IPage, IDisposable
{
    private readonly GatewayApi _api;
    private readonly StackPanel _rows = new() { Spacing = 0 };
    private readonly TextBlock _status;
    private readonly ComboBox _level;
    private readonly TextBox _device;
    private readonly CheckBox _follow;

    private DispatcherTimer? _timer;
    private long _lastSeq = -1;
    private bool _disposed;
    private bool _busy;

    public LogsPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("日志", "内存环形缓冲；按 seq 增量追加");

        _status = Ui.Caption("加载中…");

        _level = new ComboBox { Width = 120, PlaceholderText = "全部级别" };
        _level.Items.Add("debug");
        _level.Items.Add("info");
        _level.Items.Add("warning");
        _level.Items.Add("error");
        _level.SelectedIndex = -1;
        _level.SelectionChanged += (_, _) => _ = ReloadAsync();

        _device = Ui.Input(placeholder: "设备 ID（可空）");
        _device.Width = 170;
        _device.TextChanged += (_, _) => _ = ReloadAsync();

        _follow = new CheckBox { Content = "自动跟随", IsChecked = true, VerticalAlignment = VerticalAlignment.Center };
        _follow.Checked += (_, _) => Start();
        _follow.Unchecked += (_, _) => Stop();

        actions.Children.Add(_level);
        actions.Children.Add(_device);
        actions.Children.Add(_follow);
        actions.Children.Add(Ui.Button("清空", (_, _) => ClearRows()));

        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(0.6), "时刻"),
            (Ui.Star(0.5), "级别"),
            (Ui.Star(0.8), "设备"),
            (Ui.Star(1.1), "类别"),
            (Ui.Star(3.2), "消息")));
        table.Children.Add(_rows);

        var card = Ui.Panel(Ui.Scroll(table), new Thickness(0));

        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.Children.Add(card);

        Grid.SetRow(_status, 1);
        _status.Margin = new Thickness(2, 8, 0, 0);
        body.Children.Add(_status);

        View = root;

        Start();
    }

    public FrameworkElement View { get; }

    public Task RefreshAsync()
    {
        return ReloadAsync();
    }

    private void Start()
    {
        if (_disposed) return;

        if (_timer is not null) return;
        if (_follow.IsChecked != true) return;

        _timer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(2) };
        _timer.Tick += async (_, _) => await PollAsync();
        _timer.Start();
    }

    private void Stop()
    {
        _timer?.Stop();
        _timer = null;
    }

    private async Task ReloadAsync()
    {
        if (_disposed) return;

        // 过滤条件变了，游标要归零，否则新条件下的历史行永远补不回来。
        _lastSeq = -1;
        ClearRows();

        await PollAsync(full: true);
    }

    private async Task PollAsync(bool full = false)
    {
        if (_disposed || _busy) return;

        _busy = true;

        try
        {
            var level = _level.SelectedItem as string;
            var device = string.IsNullOrWhiteSpace(_device.Text) ? null : _device.Text.Trim();

            var page = await _api.LogsAsync(limit: full ? 300 : 200, level: level, device: device);
            if (_disposed) return;

            var fresh = page.Items
                .Where(e => e.Seq > _lastSeq)
                .OrderBy(e => e.Seq)
                .ToArray();

            if (fresh.Length == 0)
            {
                _status.Text = $"{_rows.Children.Count} 条日志 · 无新增 · {UiFormat.Time(DateTimeOffset.Now)}";
                return;
            }

            foreach (var entry in fresh) _rows.Children.Add(BuildRow(entry));

            _lastSeq = fresh[^1].Seq;

            _status.Text = $"{_rows.Children.Count} 条日志 · 更新于 {UiFormat.Time(DateTimeOffset.Now)}";

            // 剪掉过老的行：浏览器里堆上万个 TextBlock 只会拖慢滚动。
            while (_rows.Children.Count > 600) _rows.Children.RemoveAt(0);
        }
        catch (Exception ex)
        {
            if (_disposed) return;
            _status.Text = "加载失败：" + ex.Message;
        }
        finally
        {
            _busy = false;
        }
    }

    private static UIElement BuildRow(LogEntryDto entry)
    {
        var levelColor = UiFormat.Color(UiFormat.LevelColor(entry.Level));

        var message = Ui.Text(entry.Message, 12, Ui.Ink, wrap: true);

        var cells = new (GridLength, UIElement?)[]
        {
            (Ui.Star(0.6), Ui.Text(UiFormat.Time(entry.At), 11.5, Ui.Faint, mono: true)),
            (Ui.Star(0.5), Ui.Text(entry.Level, 11, levelColor, bold: true)),
            (Ui.Star(0.8), Ui.Text(entry.Device ?? "—", 11.5, Ui.Muted, mono: true)),
            (Ui.Star(1.1), Ui.Text(entry.Category, 11.5, Ui.Faint, mono: true)),
            (Ui.Star(3.2), message),
        };

        return Ui.Row(cells);
    }

    private void ClearRows() => _rows.Children.Clear();

    public void Dispose()
    {
        _disposed = true;
        Stop();
    }
}
