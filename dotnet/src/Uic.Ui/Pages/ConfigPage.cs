using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;

namespace Uic.Ui.Pages;

/// <summary>
/// 配置页：查看当前生效的配置、编辑 JSON、先校验再应用，并可从历史版本回滚。
///
/// 「校验」和「应用」分成两个按钮而不是应用时顺带校验：现场值改动常常要反复试几次，
/// 校验是低风险的，应用会重启设备并可能影响在产线运行的采集。
/// </summary>
public sealed class ConfigPage : IPage, IDisposable
{
    private readonly GatewayApi _api;
    private readonly TextBox _editor;
    private readonly TextBox _comment;
    private readonly TextBlock _status;
    private readonly TextBlock _problems;
    private readonly StackPanel _versions = new() { Spacing = 0 };
    private readonly Button _applyButton;

    /// <summary>只读角色也能看配置；应用与回滚按钮按权限隐藏，服务端仍然会再校验一次。</summary>
    private readonly bool _canWrite;

    private bool _disposed;
    private string _loadedJson = string.Empty;
    private string _loadedVersion = string.Empty;

    public ConfigPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("配置", "编辑后先校验再应用；历史上可回滚");

        _status = Ui.Caption("加载中…");
        _problems = Ui.Text(string.Empty, 12, Ui.Bad, wrap: true);

        _editor = new TextBox
        {
            AcceptsReturn = true,
            TextWrapping = TextWrapping.NoWrap,
            FontFamily = new FontFamily("Consolas, Menlo, monospace"),
            FontSize = 12.5,
            IsSpellCheckEnabled = false,
            HorizontalAlignment = HorizontalAlignment.Stretch,
            VerticalAlignment = VerticalAlignment.Stretch,
        };

        _editor.TextChanged += (_, _) => OnEditorChanged();

        _comment = Ui.Input(placeholder: "变更说明（可选）");
        _comment.Width = 240;

        _canWrite = Session.Can(UiPermissions.ConfigWrite);

        _applyButton = Ui.Button("应用", OnApplyClick, accent: true);
        _applyButton.IsEnabled = false;
        _applyButton.Visibility = _canWrite ? Visibility.Visible : Visibility.Collapsed;

        _editor.IsReadOnly = !_canWrite;
        _comment.Visibility = _canWrite ? Visibility.Visible : Visibility.Collapsed;

        actions.Children.Add(_comment);
        actions.Children.Add(Ui.Button("校验", OnValidateClick));
        actions.Children.Add(Ui.Button("重新加载", (_, _) => _ = RefreshAsync()));
        actions.Children.Add(_applyButton);

        var editorCard = Ui.Panel(Ui.Scroll(_editor), new Thickness(0));

        var right = new StackPanel { Spacing = 12 };
        right.Children.Add(Ui.Text("历史版本", 14, Ui.Ink, bold: true));
        right.Children.Add(Ui.Panel(Ui.Scroll(_versions), new Thickness(0)));

        var rightCard = new StackPanel { Spacing = 10, Margin = new Thickness(14, 0, 0, 0), Width = 260 };
        rightCard.Children.Add(right);

        var split = new Grid();
        split.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });
        split.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        split.Children.Add(editorCard);

        Grid.SetColumn(rightCard, 1);
        split.Children.Add(rightCard);

        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.Children.Add(split);

        var footer = new StackPanel { Spacing = 4, Margin = new Thickness(2, 8, 0, 0) };
        footer.Children.Add(_status);
        footer.Children.Add(_problems);

        Grid.SetRow(footer, 1);
        body.Children.Add(footer);

        View = root;
    }

    public FrameworkElement View { get; }

    public async Task RefreshAsync()
    {
        if (_disposed) return;

        try
        {
            var response = await _api.ConfigAsync();
            if (_disposed) return;

            // 直接写 JsonElement：不走任何序列化元数据，裁剪后同样可用。
            // 用 UnsafeRelaxedJsonEscaping 是为了让中文按原样显示在编辑器里，
            // 默认编码器会把每个汉字转成 \uXXXX，现场没法读也没法改。
            var json = ToIndented(response.Config);

            // 序列化回来的是脱敏版本（口令字段被替换），直接应用会把口令洗成占位符，
            // 所以明确提示：本地改完再应用，不要原样保存。
            _loadedJson = json;
            _loadedVersion = response.Version;
            _editor.Text = json;

            _status.Text = $"配置文件 {response.Version} · 已加载（口令类字段已脱敏，改动后请自行确认）";
            _status.Foreground = new SolidColorBrush(Ui.Muted);
            _problems.Text = string.Empty;

            await LoadVersionsAsync();
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _status.Text = "加载配置失败：" + ex.Message;
            _status.Foreground = new SolidColorBrush(Ui.Bad);
        }
    }

    private async Task LoadVersionsAsync()
    {
        try
        {
            var versions = await _api.VersionsAsync();
            if (_disposed) return;

            _versions.Children.Clear();

            foreach (var version in versions.Items.OrderByDescending(v => v.CreatedAt).Take(30))
            {
                _versions.Children.Add(BuildVersionRow(version));
            }

            if (versions.Items.Length == 0)
            {
                _versions.Children.Add(new Border
                {
                    Padding = new Thickness(10, 12, 10, 12),
                    Child = Ui.Text("尚无历史版本", 12, Ui.Faint),
                });
            }
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _versions.Children.Clear();
            _versions.Children.Add(new Border
            {
                Padding = new Thickness(10, 12, 10, 12),
                Child = Ui.Text("加载版本列表失败：" + ex.Message, 11.5, Ui.Bad, wrap: true),
            });
        }
    }

    private UIElement BuildVersionRow(ConfigVersionDto version)
    {
        var stack = new StackPanel { Spacing = 2 };
        stack.Children.Add(Ui.Text(version.Id, 11.5, Ui.Ink, mono: true));
        stack.Children.Add(Ui.Text(
            $"{UiFormat.DateTime(version.CreatedAt)} · {version.Size / 1024.0:0.#} KB",
            10.5, Ui.Faint));
        if (!string.IsNullOrWhiteSpace(version.Comment))
        {
            stack.Children.Add(Ui.Text(version.Comment, 10.5, Ui.Muted, wrap: true));
        }

        var content = new Grid();
        content.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });
        content.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        content.Children.Add(stack);

        var isCurrent = version.Current;
        var actions = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 4 };

        if (isCurrent)
        {
            actions.Children.Add(Ui.Chip("当前", Ui.Good));
        }
        else
        {
            // 回滚会把整份配置切回旧版本，运营员角色默认没有这项能力。
            if (Session.Can(UiPermissions.ConfigRollback))
            {
                var rollback = Ui.Button("回滚", async (_, _) => await RollbackAsync(version.Id));
                rollback.Padding = new Thickness(8, 2, 8, 2);
                rollback.FontSize = 11.5;
                actions.Children.Add(rollback);
            }
        }

        Grid.SetColumn(actions, 1);
        content.Children.Add(actions);

        return new Border
        {
            Child = content,
            Padding = new Thickness(10, 7, 10, 7),
            BorderBrush = new SolidColorBrush(Ui.Line),
            BorderThickness = new Thickness(0, 0, 0, 1),
        };
    }

    private void OnEditorChanged()
    {
        var dirty = _editor.Text != _loadedJson;
        _applyButton.IsEnabled = _canWrite && dirty && !_disposed;

        if (dirty)
        {
            _status.Text = $"基于 {_loadedVersion} 的未保存改动";
            _status.Foreground = new SolidColorBrush(UiFormat.Color("#B26A00"));
        }
    }

    private async void OnValidateClick(object sender, RoutedEventArgs e)
    {
        await ValidateAsync(silentWhenValid: false);
    }

    private async Task<bool> ValidateAsync(bool silentWhenValid)
    {
        _problems.Text = string.Empty;

        try
        {
            var response = await _api.ValidateAsync(_editor.Text);
            if (_disposed) return false;

            if (response.Valid)
            {
                if (!silentWhenValid)
                {
                    _problems.Text = "校验通过。";
                    _problems.Foreground = new SolidColorBrush(Ui.Good);
                }

                return true;
            }

            _problems.Foreground = new SolidColorBrush(Ui.Bad);
            _problems.Text = "校验未通过：" + Environment.NewLine + string.Join(Environment.NewLine,
                response.Problems.Select(p => "· " + p));
            return false;
        }
        catch (Exception ex)
        {
            if (_disposed) return false;

            _problems.Foreground = new SolidColorBrush(Ui.Bad);
            _problems.Text = "校验请求失败：" + ex.Message;
            return false;
        }
    }

    private async void OnApplyClick(object sender, RoutedEventArgs e)
    {
        // 应用前强制过一次校验：手写 JSON 里常见的逗号/括号错误，
        // 直接打到 /apply 会在网关侧报 422，不如在这里先挡住。
        if (!await ValidateAsync(silentWhenValid: true)) return;

        _applyButton.IsEnabled = false;
        _status.Foreground = new SolidColorBrush(Ui.Faint);
        _status.Text = "正在应用…";

        try
        {
            var comment = string.IsNullOrWhiteSpace(_comment.Text) ? null : _comment.Text.Trim();
            var response = await _api.ApplyAsync(_editor.Text, comment);
            if (_disposed) return;

            if (!response.Applied)
            {
                _status.Foreground = new SolidColorBrush(Ui.Bad);
                _status.Text = $"未应用（版本 {response.Version}）";
                return;
            }

            var changed = response.Changed;
            _status.Foreground = new SolidColorBrush(Ui.Good);
            _status.Text = $"已应用 {response.Version} · {response.DurationMs} ms · " +
                           $"设备 +{changed.DevicesAdded} ~{changed.DevicesModified} -{changed.DevicesRemoved} · " +
                           $"点位 +{changed.PointsAdded} ~{changed.PointsModified} -{changed.PointsRemoved}" +
                           (response.RestartedDevices.Length == 0
                               ? string.Empty
                               : $" · 重启 {string.Join(',', response.RestartedDevices)}");

            _comment.Text = string.Empty;
            await ReloadAsync();
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _status.Foreground = new SolidColorBrush(Ui.Bad);
            _status.Text = "应用失败：" + ex.Message;
            _applyButton.IsEnabled = true;
        }
    }

    private async Task ReloadAsync()
    {
        await RefreshAsync();

        if (_disposed) return;

        _status.Foreground = new SolidColorBrush(Ui.Good);
        _status.Text = $"已重新加载配置 {_loadedVersion}";
    }

    private async Task RollbackAsync(string versionId)
    {
        _status.Foreground = new SolidColorBrush(Ui.Faint);
        _status.Text = $"正在回滚到 {versionId}…";

        try
        {
            var response = await _api.CommandAsync("config.rollback",
                new Dictionary<string, string> { ["version"] = versionId });
            if (_disposed) return;

            if (response.Ok)
            {
                _status.Foreground = new SolidColorBrush(Ui.Good);
                _status.Text = $"已回滚到 {versionId}" + (response.Detail is null ? string.Empty : " · " + response.Detail);
                await RefreshAsync();
            }
            else
            {
                _status.Foreground = new SolidColorBrush(Ui.Bad);
                _status.Text = "回滚被拒绝：" + (response.Detail ?? "网关未返回原因");
            }
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            _status.Foreground = new SolidColorBrush(Ui.Bad);
            _status.Text = "回滚失败：" + ex.Message;
        }
    }

    /// <summary>
    /// 把配置树按缩进格式写回文本。手写 Utf8JsonWriter 而不是 JsonSerializer：
    /// 后者要查类型元数据，裁剪后的 WASM 里可能拿不到，而 JsonElement 自带 WriteTo。
    /// </summary>
    private static string ToIndented(JsonElement element)
    {
        using var buffer = new MemoryStream();

        using (var writer = new Utf8JsonWriter(buffer, new JsonWriterOptions
        {
            Indented = true,
            Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping,
        }))
        {
            element.WriteTo(writer);
        }

        return Encoding.UTF8.GetString(buffer.ToArray());
    }

    public void Dispose() => _disposed = true;
}
