using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;

namespace Uic.Ui.Pages;

/// <summary>
/// 账号管理。只有 <c>user.manage</c> 能进——这是「操作级授权」在界面上的落点：
/// 导航可以藏，但真正的边界是服务端对每个请求的权限校验。
///
/// 口令只在这里出现一次（新建或重置生成的临时口令），之后再也取不回来：
/// 前端不缓存、接口也不返回。管理员必须当场抄给用户。
/// </summary>
public sealed class UsersPage : IPage, IDisposable
{
    private static readonly string[] Roles = { "admin", "operator", "viewer", "auditor" };

    private readonly GatewayApi _api;
    private readonly StackPanel _rows = new() { Spacing = 0 };
    private readonly TextBlock _status;
    private readonly TextBlock _notice;

    private readonly TextBox _id = Ui.Input(placeholder: "登录名，例如 zhangsan", mono: true);
    private readonly TextBox _displayName = Ui.Input(placeholder: "显示名（可选）");
    private readonly ComboBox _role = new() { Width = 140 };
    private readonly TextBox _password = Ui.Input(placeholder: "留空则由服务端生成一次性口令", mono: true);
    private readonly Border _formCard;

    private string? _pendingDelete;
    private bool _disposed;
    private bool _busy;

    public UsersPage(GatewayApi api)
    {
        _api = api;

        var (root, actions, body) = Ui.Scaffold("账号", "本地账号与角色；临时口令只显示一次");

        _status = Ui.Caption("加载中…");
        _notice = Ui.Text(string.Empty, 12.5, Ui.Accent, mono: true, wrap: true);

        foreach (var role in Roles) _role.Items.Add(role);
        _role.SelectedIndex = 2;

        actions.Children.Add(Ui.Button("新建账号", (_, _) => ToggleForm()));
        actions.Children.Add(Ui.Button("刷新", (_, _) => _ = RefreshAsync(), accent: true));

        // 新建表单
        var form = new StackPanel { Spacing = 10 };
        var formRow = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 10 };

        _id.Width = 190;
        _displayName.Width = 170;
        _password.Width = 260;

        formRow.Children.Add(_id);
        formRow.Children.Add(_displayName);
        formRow.Children.Add(_role);
        formRow.Children.Add(_password);
        formRow.Children.Add(Ui.Button("创建", (_, _) => _ = CreateAsync(), accent: true));
        formRow.Children.Add(Ui.Button("取消", (_, _) => ToggleForm()));

        form.Children.Add(formRow);
        form.Children.Add(Ui.Caption("新建账号会带「首次登录必须改密」标记；口令留空时由服务端生成 16 位随机口令。", wrap: true));

        _formCard = Ui.Band("新建账号", form);
        _formCard.Margin = new Thickness(0, 0, 0, 12);
        _formCard.Visibility = Visibility.Collapsed;

        var table = new StackPanel();
        table.Children.Add(Ui.Header(
            (Ui.Star(1.1), "用户名"),
            (Ui.Star(1.2), "显示名"),
            (Ui.Star(0.8), "角色"),
            (Ui.Star(0.7), "状态"),
            (Ui.Star(0.9), "强制改密"),
            (Ui.Star(1.2), "最近登录"),
            (Ui.Star(2.0), "操作")));

        table.Children.Add(_rows);

        var card = Ui.Panel(Ui.Scroll(table), new Thickness(0));

        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });

        body.Children.Add(_formCard);
        body.Children.Add(_notice);
        Grid.SetRow(card, 2);
        body.Children.Add(card);

        Grid.SetRow(_status, 3);
        _status.Margin = new Thickness(2, 8, 0, 0);
        body.Children.Add(_status);

        View = root;
    }

    public FrameworkElement View { get; }

    private void ToggleForm()
        => _formCard.Visibility = _formCard.Visibility == Visibility.Visible
            ? Visibility.Collapsed
            : Visibility.Visible;

    public async Task RefreshAsync()
    {
        if (_disposed || _busy) return;

        _busy = true;

        try
        {
            var page = await _api.UsersAsync();
            if (_disposed) return;

            _rows.Children.Clear();

            foreach (var user in page.Items.OrderBy(u => u.Id, StringComparer.Ordinal))
                _rows.Children.Add(BuildRow(user));

            _status.Text = $"{page.Total} 个账号 · 更新于 {UiFormat.Time(DateTimeOffset.Now)}";
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

    private UIElement BuildRow(UserDto user)
    {
        var roleColor = user.Role switch
        {
            "admin" => Ui.Accent,
            "operator" => Ui.Ink,
            _ => Ui.Muted,
        };

        var state = user.Enabled ? Ui.Chip("启用", Ui.Good) : Ui.Chip("停用", Ui.Bad);
        var mustChange = user.MustChangePassword ? "是" : "否";

        var buttons = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 6 };

        var reset = Ui.Button("重置口令", (_, _) => _ = ResetAsync(user.Id), accent: false);
        reset.Padding = new Thickness(8, 2, 8, 2);
        reset.FontSize = 11.5;
        buttons.Children.Add(reset);

        var toggle = Ui.Button(user.Enabled ? "停用" : "启用",
            (_, _) => _ = SetEnabledAsync(user.Id, !user.Enabled));
        toggle.Padding = new Thickness(8, 2, 8, 2);
        toggle.FontSize = 11.5;
        buttons.Children.Add(toggle);

        // 删除需要点两次：第一次把按钮改成「确认删除」，避免误触把账号删掉。
        var pending = string.Equals(_pendingDelete, user.Id, StringComparison.Ordinal);
        var remove = Ui.Button(pending ? "确认删除" : "删除",
            (_, _) => _ = DeleteAsync(user.Id));
        remove.Padding = new Thickness(8, 2, 8, 2);
        remove.FontSize = 11.5;
        if (pending) remove.Foreground = new Microsoft.UI.Xaml.Media.SolidColorBrush(Ui.Bad);
        buttons.Children.Add(remove);

        var cells = new (GridLength, UIElement?)[]
        {
            (Ui.Star(1.1), Ui.Text(user.Id, 12.5, Ui.Ink, mono: true)),
            (Ui.Star(1.2), Ui.Text(user.DisplayName ?? "—", 12, Ui.Muted)),
            (Ui.Star(0.8), Ui.Text(user.Role, 12, roleColor, bold: true, mono: true)),
            (Ui.Star(0.7), state),
            (Ui.Star(0.9), Ui.Text(mustChange, 12, user.MustChangePassword ? Ui.Bad : Ui.Faint)),
            (Ui.Star(1.2), Ui.Text(
                user.LastLoginAt is { } at ? UiFormat.Time(at) : "—", 11.5, Ui.Faint, mono: true)),
            (Ui.Star(2.0), buttons),
        };

        return Ui.Row(cells);
    }

    private async Task CreateAsync()
    {
        if (_busy) return;

        var id = _id.Text?.Trim();
        if (string.IsNullOrEmpty(id))
        {
            _status.Text = "请填写登录名。";
            return;
        }

        _busy = true;

        try
        {
            var result = await _api.CreateUserAsync(
                id,
                string.IsNullOrWhiteSpace(_displayName.Text) ? null : _displayName.Text.Trim(),
                _role.SelectedItem as string,
                string.IsNullOrWhiteSpace(_password.Text) ? null : _password.Text);

            ShowTemporaryPassword(result.TemporaryPassword, $"账号 '{result.User.Id}' 已创建");

            _id.Text = string.Empty;
            _displayName.Text = string.Empty;
            _password.Text = string.Empty;
            _formCard.Visibility = Visibility.Collapsed;

            await RefreshAsync();
        }
        catch (Exception ex)
        {
            _status.Text = "创建失败：" + ex.Message;
        }
        finally
        {
            _busy = false;
        }
    }

    private async Task ResetAsync(string id)
    {
        if (_busy) return;

        _busy = true;

        try
        {
            var result = await _api.ResetPasswordAsync(id);
            ShowTemporaryPassword(result.TemporaryPassword, $"账号 '{id}' 的口令已重置");
            await RefreshAsync();
        }
        catch (Exception ex)
        {
            _status.Text = "重置失败：" + ex.Message;
        }
        finally
        {
            _busy = false;
        }
    }

    private async Task SetEnabledAsync(string id, bool enabled)
    {
        if (_busy) return;

        _busy = true;

        try
        {
            await _api.UpdateUserAsync(id, null, null, enabled);
            _status.Text = enabled ? $"账号 '{id}' 已启用。" : $"账号 '{id}' 已停用，现有会话已撤销。";
            await RefreshAsync();
        }
        catch (Exception ex)
        {
            _status.Text = "操作失败：" + ex.Message;
        }
        finally
        {
            _busy = false;
        }
    }

    private async Task DeleteAsync(string id)
    {
        if (_busy) return;

        if (!string.Equals(_pendingDelete, id, StringComparison.Ordinal))
        {
            // 第一次点击只是进入确认态；再点同一个按钮才会真的删除。
            _pendingDelete = id;
            _status.Text = $"再次点击「确认删除」以删除账号 '{id}'。";
            await RefreshAsync();
            return;
        }

        _pendingDelete = null;
        _busy = true;

        try
        {
            await _api.DeleteUserAsync(id);
            _status.Text = $"账号 '{id}' 已删除。";
            await RefreshAsync();
        }
        catch (Exception ex)
        {
            _status.Text = "删除失败：" + ex.Message;
        }
        finally
        {
            _busy = false;
        }
    }

    private void ShowTemporaryPassword(string? password, string title)
    {
        if (string.IsNullOrEmpty(password))
        {
            _notice.Text = $"{title}。";
            return;
        }

        _notice.Text = $"{title}。一次性临时口令：{password}（只显示这一次，请立即抄录；首次登录必须修改）";
    }

    public void Dispose() => _disposed = true;
}
