using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;

namespace Uic.Ui.Pages;

/// <summary>
/// 驱动能力页。四态徽标不是装饰：现场最常踩的坑就是把「型号相关」当成「支持」，
/// 配置完才发现换一台仪表就写不进去。这里把「不确定」明确画成琥珀色。
/// </summary>
public sealed class DriversPage : IPage, IDisposable
{
    /// <summary>与 <c>CapabilitiesDto</c> 的属性一一对应；顺序即界面顺序。</summary>
    private static readonly (string Key, string Label)[] Capabilities =
    {
        ("read", "读"),
        ("write", "写"),
        ("subscribe", "订阅"),
        ("browse", "浏览"),
        ("discovery", "发现"),
        ("multiRead", "多读"),
        ("bitAccess", "位访问"),
        ("structTypes", "结构"),
    };

    private readonly GatewayApi _api;
    private readonly StackPanel _list = new() { Spacing = 0 };
    private readonly TextBlock _status;

    private bool _disposed;

    public DriversPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("驱动", "能力四态：支持 / 不支持 / 视运行时 / 实验");

        _status = Ui.Caption(string.Empty);
        actions.Children.Add(Ui.Button("刷新", (_, _) => _ = RefreshAsync(), accent: true));

        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(2.2), "驱动"),
            (Ui.Star(0.8), "角色"),
            (Ui.Star(0.9), "传输"),
            (Ui.Star(1.1), "寻址"),
            (Ui.Star(3.4), "能力四态"),
            (Ui.Star(2.4), "说明")));
        table.Children.Add(_list);

        var content = new StackPanel { Spacing = 10 };
        content.Children.Add(Ui.Panel(Ui.Scroll(table), new Thickness(0)));
        content.Children.Add(BuildLegend());

        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.Children.Add(content);

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
            var page = await _api.DriversAsync();
            if (_disposed) return;

            _list.Children.Clear();

            foreach (var driver in page.Items)
            {
                _list.Children.Add(BuildDriverRow(driver));
            }

            _status.Text = $"{page.Items.Length} 个驱动 · 更新于 {UiFormat.Time(DateTimeOffset.Now)}";
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _list.Children.Clear();
            _status.Text = "加载失败：" + ex.Message;
        }
    }

    public void Dispose() => _disposed = true;

    private static UIElement BuildDriverRow(DriverDto driver)
    {
        var name = new StackPanel { Spacing = 1 };
        name.Children.Add(Ui.Text(driver.Name, 13, Ui.Ink, bold: true));
        name.Children.Add(Ui.Text($"{driver.Id} · v{driver.Version}", 10.5, Ui.Faint, mono: true));

        var caps = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 4 };
        foreach (var (key, label) in Capabilities)
        {
            var state = CapValue(driver.Capabilities, key);
            var chip = Ui.Chip(label, UiFormat.Color(UiFormat.CapColor(state)));

            // 徽标上只放一个汉字，完整状态放进悬停提示，避免一行塞八个长词。
            ToolTipService.SetToolTip(chip, $"{label}：{StateLabel(state)}");
            caps.Children.Add(chip);
        }

        return Ui.Row(
            (Ui.Star(2.2), name),
            (Ui.Star(0.8), Ui.Text(Roles(driver.Roles), 12, Ui.Muted)),
            (Ui.Star(0.9), Ui.Text(string.Join(" / ", driver.Transports), 11.5, Ui.Muted, mono: true)),
            (Ui.Star(1.1), Ui.Text(string.Join(" / ", driver.AddressKinds), 11.5, Ui.Muted, mono: true)),
            (Ui.Star(3.4), caps),
            (Ui.Star(2.4), Ui.Text(driver.Description ?? "—", 11.5, Ui.Faint, wrap: true)));
    }

    private static UIElement BuildLegend()
    {
        var panel = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 8 };
        panel.Children.Add(Ui.Caption("图例："));

        foreach (var state in new[] { "supported", "unsupported", "runtimeDependent", "experimental" })
        {
            panel.Children.Add(Ui.Chip(StateLabel(state), UiFormat.Color(UiFormat.CapColor(state))));
        }

        panel.Children.Add(Ui.Caption("「视运行时」= 同一协议在不同型号上可能不同，不能当成保证。"));
        return panel;
    }

    private static string CapValue(CapabilitiesDto capabilities, string key) => key switch
    {
        "read" => capabilities.Read,
        "write" => capabilities.Write,
        "subscribe" => capabilities.Subscribe,
        "browse" => capabilities.Browse,
        "discovery" => capabilities.Discovery,
        "multiRead" => capabilities.MultiRead,
        "bitAccess" => capabilities.BitAccess,
        "structTypes" => capabilities.StructTypes,
        _ => "unsupported",
    };

    private static string StateLabel(string state) => state switch
    {
        "supported" => "支持",
        "runtimeDependent" => "视运行时",
        "experimental" => "实验",
        _ => "不支持",
    };

    private static string Roles(string[] roles)
    {
        var labels = roles.Select(r => r switch
        {
            "master" => "主站",
            "slave" => "从站",
            "both" => "双角色",
            _ => r,
        });

        return string.Join(" / ", labels);
    }
}
