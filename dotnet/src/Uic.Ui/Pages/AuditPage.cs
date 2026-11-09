using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;

namespace Uic.Ui.Pages;

/// <summary>
/// 审计。与「日志」刻意分成两个页面：日志回答「发生了什么」，会轮转、会被日志量冲掉；
/// 审计回答「谁让它发生的」，按天落盘并按份数保留，是追责依据。
///
/// 这一页只读，且只有 audit.read 能进——审计员看得到操作记录，改不了配置。
/// </summary>
public sealed class AuditPage : IPage, IDisposable
{
    private readonly GatewayApi _api;
    private readonly StackPanel _rows = new() { Spacing = 0 };
    private readonly TextBlock _status;
    private readonly TextBox _action;
    private readonly TextBox _actor;
    private readonly ComboBox _result;

    private bool _disposed;
    private bool _busy;

    public AuditPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("审计", "谁在什么时候对什么做了什么；默认最近 24 小时");

        _status = Ui.Caption("加载中…");

        _action = Ui.Input(placeholder: "动作，如 config.apply");
        _action.Width = 190;
        _action.TextChanged += (_, _) => _ = RefreshAsync();

        _actor = Ui.Input(placeholder: "账号");
        _actor.Width = 140;
        _actor.TextChanged += (_, _) => _ = RefreshAsync();

        _result = new ComboBox { Width = 120, PlaceholderText = "全部结果" };
        _result.Items.Add("ok");
        _result.Items.Add("denied");
        _result.SelectedIndex = -1;
        _result.SelectionChanged += (_, _) => _ = RefreshAsync();

        actions.Children.Add(_action);
        actions.Children.Add(_actor);
        actions.Children.Add(_result);
        actions.Children.Add(Ui.Button("刷新", (_, _) => _ = RefreshAsync(), accent: true));

        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(1.1), "时间"),
            (Ui.Star(1.3), "动作"),
            (Ui.Star(0.6), "结果"),
            (Ui.Star(0.9), "账号"),
            (Ui.Star(0.7), "来源"),
            (Ui.Star(1.2), "目标"),
            (Ui.Star(1.0), "远端"),
            (Ui.Star(2.6), "细节")));

        table.Children.Add(_rows);

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
        if (_disposed || _busy) return;

        _busy = true;

        try
        {
            var action = string.IsNullOrWhiteSpace(_action.Text) ? null : _action.Text.Trim();
            var actor = string.IsNullOrWhiteSpace(_actor.Text) ? null : _actor.Text.Trim();
            var result = _result.SelectedItem as string;

            var page = await _api.AuditAsync(action, actor, result, limit: 200);
            if (_disposed) return;

            _rows.Children.Clear();

            foreach (var entry in page.Items.OrderByDescending(e => e.Seq))
                _rows.Children.Add(BuildRow(entry));

            if (page.Items.Length == 0)
            {
                _rows.Children.Add(new Border
                {
                    Padding = new Thickness(16, 28, 16, 28),
                    Child = Ui.Text("所选条件下没有审计记录", 13, Ui.Faint),
                });
            }

            _status.Text = $"{page.Total} 条记录 · 更新于 {UiFormat.Time(DateTimeOffset.Now)}";
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _rows.Children.Clear();
            _status.Text = "加载失败：" + ex.Message;
        }
        finally
        {
            _busy = false;
        }
    }

    private static UIElement BuildRow(AuditEntryDto entry)
    {
        var color = entry.Result switch
        {
            "ok" => Ui.Good,
            "denied" => Ui.Bad,
            _ => Ui.Muted,
        };

        // 细节是各动作自定义的 JSON。这里只做「一行摘要」，完整内容留给导出文件，
        // 免得把表格撑成不可读的嵌套 JSON。
        var detail = entry.Detail is { } node ? node.GetRawText() : string.Empty;
        if (detail.Length > 160) detail = detail[..160] + "…";

        var cells = new (GridLength, UIElement?)[]
        {
            (Ui.Star(1.1), Ui.Text(UiFormat.Time(entry.At), 11.5, Ui.Faint, mono: true)),
            (Ui.Star(1.3), Ui.Text(entry.Action, 12, Ui.Ink, mono: true)),
            (Ui.Star(0.6), Ui.Text(entry.Result, 11.5, color, bold: true)),
            (Ui.Star(0.9), Ui.Text(entry.Actor, 12, Ui.Muted)),
            (Ui.Star(0.7), Ui.Text(entry.Source, 11.5, Ui.Faint, mono: true)),
            (Ui.Star(1.2), Ui.Text(entry.Target ?? "—", 11.5, Ui.Muted, mono: true)),
            (Ui.Star(1.0), Ui.Text(entry.Remote ?? "—", 11.5, Ui.Faint, mono: true)),
            (Ui.Star(2.6), Ui.Text(detail, 11, Ui.Faint, mono: true)),
        };

        return Ui.Row(cells);
    }

    public void Dispose() => _disposed = true;
}
