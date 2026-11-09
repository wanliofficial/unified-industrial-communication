using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;
using Uic.Ui.Pages;

namespace Uic.Ui;

public sealed partial class MainPage : Page
{
    private readonly Microsoft.UI.Dispatching.DispatcherQueue _dispatcher;

    private GatewayApi? _api;
    private DispatcherTimer? _healthTimer;
    private IPage? _current;

    /// <summary>刚在登录框里输入的口令。强制改密时预填「当前口令」，省得再抄一遍随机串。</summary>
    private string? _pendingPassword;

    public MainPage()
    {
        InitializeComponent();

        // 会话失效的 401 在 HTTP 线程上触发，必须回到 UI 线程再改界面。
        // 在构造函数里抓一次就够了：页面本身就活在 UI 线程。
        _dispatcher = Microsoft.UI.Dispatching.DispatcherQueue.GetForCurrentThread();

        // 登录前不预选任何页：选中会立刻拉一次 /devices，在未认证时只会得到 401。
        NavList.SelectedIndex = -1;

        Loaded += OnLoaded;
        Unloaded += OnUnloaded;
    }

    private async void OnLoaded(object sender, RoutedEventArgs e)
    {
        GatewayApi.SessionExpired -= OnSessionExpired;
        GatewayApi.SessionExpired += OnSessionExpired;

        _api = new GatewayApi();

        // 只有「存在令牌」才值得探一次；没有令牌直接开登录页，省一个必然失败的请求。
        if (TokenStore.Load() is null)
        {
            ShowLogin(null);
            return;
        }

        await RestoreSessionAsync();
    }

    private void OnUnloaded(object sender, RoutedEventArgs e)
    {
        GatewayApi.SessionExpired -= OnSessionExpired;
        _healthTimer?.Stop();
        _healthTimer = null;
    }

    /// <summary>刷新页面后用 localStorage 里的会话直接回到控制台；过期就回登录页。</summary>
    private async Task RestoreSessionAsync()
    {
        if (_api is null) return;

        try
        {
            var me = await _api.MeAsync();
            Session.Set(me);

            if (me.MustChangePassword)
            {
                ShowPasswordChange("当前口令是一次性口令，请先设置新口令。");
                return;
            }

            EnterShell();
        }
        catch (GatewayException ex) when (ex.Unauthorized)
        {
            SignOutLocally("登录已过期，请重新登录。");
        }
        catch
        {
            SignOutLocally("无法连接网关，请确认服务已启动。");
        }
    }

    // ------------------------------------------------------------------
    // 登录
    // ------------------------------------------------------------------

    private void ShowLogin(string? message)
    {
        LoginOverlay.Visibility = Visibility.Visible;
        PasswordOverlay.Visibility = Visibility.Collapsed;

        LoginError.Visibility = message is null ? Visibility.Collapsed : Visibility.Visible;
        LoginError.Text = message ?? string.Empty;

        UsernameBox.Focus(FocusState.Programmatic);
    }

    private async void OnConnectClick(object sender, RoutedEventArgs e)
    {
        if (_api is null) return;

        var username = UsernameBox.Text?.Trim();
        if (string.IsNullOrEmpty(username))
        {
            ShowLoginError("请输入用户名。");
            return;
        }

        var password = PasswordBox.Password;
        if (string.IsNullOrEmpty(password))
        {
            ShowLoginError("请输入口令。");
            return;
        }

        ConnectButton.IsEnabled = false;
        LoginStatus.Text = "正在登录…";
        LoginError.Visibility = Visibility.Collapsed;

        try
        {
            // 登录请求本身不能带旧令牌：否则旧会话过期时的 401 会被当成「会话失效」
            // 触发一次登出，用户看到的提示会和实际原因对不上。
            _api.SetToken(null);
            TokenStore.Save(null);

            var response = await _api.LoginAsync(username, password);

            _api.SetToken(response.Token);
            TokenStore.Save(response.Token);
            Session.Set(ToMe(response.User));
            _pendingPassword = password;

            LoginStatus.Text = string.Empty;
            PasswordBox.Password = string.Empty;

            if (response.User.MustChangePassword)
            {
                ShowPasswordChange("当前口令是一次性口令，请先设置新口令。");
                return;
            }

            EnterShell();
        }
        catch (GatewayException ex)
        {
            ShowLoginError(Describe(ex));
        }
        catch (Exception ex)
        {
            ShowLoginError("登录失败：" + ex.Message);
        }
        finally
        {
            ConnectButton.IsEnabled = true;
        }
    }

    private void ShowLoginError(string message)
    {
        LoginError.Text = message;
        LoginError.Visibility = Visibility.Visible;
        LoginStatus.Text = string.Empty;
    }

    /// <summary>把契约错误码翻成现场能看懂的话；未知错误退回原始 message。</summary>
    private static string Describe(GatewayException ex) => ex.Code switch
    {
        "auth.invalid_credentials" => "用户名或口令不正确。",
        "auth.user_disabled" => "该账号已被停用，请联系管理员。",
        "request.rate_limited" => "登录失败次数过多，请稍后再试。",
        "auth.weak_password" => "新口令不符合要求（至少 8 位，且不能与当前口令相同）。",
        "auth.invalid_password" => "当前口令不正确。",
        "user.last_admin" => "不能停用或降级最后一个管理员。",
        "user.exists" => "该用户名已存在。",
        _ => ex.Message,
    };

    private static MeDto ToMe(UserDto user)
        => new(user.Id, user.DisplayName, user.Role, user.Permissions,
            user.Enabled, user.MustChangePassword, "session");

    // ------------------------------------------------------------------
    // 改密（首次强制 + 主动修改）
    // ------------------------------------------------------------------

    private void ShowPasswordChange(string? message)
    {
        LoginOverlay.Visibility = Visibility.Collapsed;
        PasswordOverlay.Visibility = Visibility.Visible;

        // 预填刚输入的口令；主动改密时留空，由用户自己填。
        CurrentPasswordBox.Password = _pendingPassword ?? string.Empty;
        NewPasswordBox.Password = string.Empty;
        ConfirmPasswordBox.Password = string.Empty;

        PasswordError.Visibility = message is null ? Visibility.Collapsed : Visibility.Visible;
        PasswordError.Text = message ?? string.Empty;

        NewPasswordBox.Focus(FocusState.Programmatic);
    }

    private void OnChangePasswordClick(object sender, RoutedEventArgs e)
        => ShowPasswordChange(null);

    private async void OnPasswordSaveClick(object sender, RoutedEventArgs e)
    {
        if (_api is null) return;

        var current = CurrentPasswordBox.Password;
        var next = NewPasswordBox.Password;
        var confirm = ConfirmPasswordBox.Password;

        if (string.IsNullOrEmpty(current))
        {
            ShowPasswordError("请输入当前口令。");
            return;
        }

        if (next.Length < 8)
        {
            ShowPasswordError("新口令至少 8 位。");
            return;
        }

        if (!string.Equals(next, confirm, StringComparison.Ordinal))
        {
            ShowPasswordError("两次输入的新口令不一致。");
            return;
        }

        ChangePasswordButton.IsEnabled = false;
        PasswordStatus.Text = "正在保存…";
        PasswordError.Visibility = Visibility.Collapsed;

        try
        {
            var response = await _api.ChangePasswordAsync(current, next);

            // 改密会撤销全部旧会话并换发新令牌：浏览器必须立刻切过去，
            // 否则下一次请求就用着刚被作废的令牌，表现得像「改完就被踢了」。
            _api.SetToken(response.Token);
            TokenStore.Save(response.Token);
            Session.Set(ToMe(response.User));

            _pendingPassword = null;
            CurrentPasswordBox.Password = string.Empty;
            NewPasswordBox.Password = string.Empty;
            ConfirmPasswordBox.Password = string.Empty;
            PasswordStatus.Text = string.Empty;

            EnterShell();
        }
        catch (GatewayException ex)
        {
            ShowPasswordError(Describe(ex));
        }
        catch (Exception ex)
        {
            ShowPasswordError("修改失败：" + ex.Message);
        }
        finally
        {
            ChangePasswordButton.IsEnabled = true;
        }
    }

    private void ShowPasswordError(string message)
    {
        PasswordError.Text = message;
        PasswordError.Visibility = Visibility.Visible;
        PasswordStatus.Text = string.Empty;
    }

    // ------------------------------------------------------------------
    // 进入 / 退出
    // ------------------------------------------------------------------

    private void EnterShell()
    {
        LoginOverlay.Visibility = Visibility.Collapsed;
        PasswordOverlay.Visibility = Visibility.Collapsed;

        ApplyPermissions();
        StartHealthPolling();
    }

    /// <summary>
    /// 按权限显示导航与顶栏账号。隐藏项仍留在 Items 里而不是删掉：
    /// 权限变化（重新登录）后只需再调一次，不必重建整个列表。
    /// </summary>
    private void ApplyPermissions()
    {
        UserText.Text = Session.IsAuthenticated
            ? $"{Session.DisplayName} · {Session.Role}"
            : string.Empty;

        PasswordButton.Visibility =
            Session.Current?.Kind == "session" ? Visibility.Visible : Visibility.Collapsed;

        foreach (var item in NavList.Items)
        {
            if (item is not ListViewItem row) continue;

            var tag = row.Tag as string ?? string.Empty;
            row.Visibility = Session.Can(PermissionFor(tag)) ? Visibility.Visible : Visibility.Collapsed;
        }

        var first = NavList.Items.OfType<ListViewItem>()
            .FirstOrDefault(i => i.Visibility == Visibility.Visible);

        if (first is not null) NavList.SelectedItem = first;
    }

    private static string PermissionFor(string tag) => tag switch
    {
        "config" => UiPermissions.ConfigRead,
        "audit" => UiPermissions.AuditRead,
        "users" => UiPermissions.UserManage,
        _ => UiPermissions.SystemRead,
    };

    private async void OnSignOutClick(object sender, RoutedEventArgs e)
    {
        if (_api is not null)
        {
            try
            {
                await _api.LogoutAsync();
            }
            catch
            {
                // 网络不通也要能退出：本地清掉令牌比服务端撤销更重要。
            }
        }

        SignOutLocally(null);
    }

    /// <summary>服务端返回 401 时（会话过期、账号被停用）由 <see cref="GatewayApi"/> 触发。</summary>
    private void OnSessionExpired()
    {
        if (!_dispatcher.TryEnqueue(() => SignOutLocally("登录已失效，请重新登录。")))
            SignOutLocally("登录已失效，请重新登录。");
    }

    private void SignOutLocally(string? message)
    {
        _healthTimer?.Stop();
        _healthTimer = null;

        if (_current is IDisposable disposable) disposable.Dispose();
        _current = null;

        _api?.SetToken(null);
        _api = null;
        _pendingPassword = null;
        TokenStore.Save(null);
        Session.Clear();

        ContentHost.Children.Clear();
        HealthDot.Fill = new SolidColorBrush(ColorOf("#7A8697"));
        HealthText.Text = "已退出";
        SiteText.Text = "—";
        VersionText.Text = string.Empty;
        UserText.Text = string.Empty;
        PasswordButton.Visibility = Visibility.Collapsed;

        // 退出后需要重建客户端：旧实例的 DefaultRequestHeaders 里已经没有令牌，
        // 但保留引用会让「再登录」沿用同一份 HttpClient 状态，容易漏掉清理。
        _api = new GatewayApi();

        _pendingPassword = null;
        PasswordBox.Password = string.Empty;
        CurrentPasswordBox.Password = string.Empty;
        NewPasswordBox.Password = string.Empty;
        ConfirmPasswordBox.Password = string.Empty;

        ShowLogin(message);
    }

    // ------------------------------------------------------------------
    // 导航
    // ------------------------------------------------------------------

    private void OnNavSelectionChanged(object sender, SelectionChangedEventArgs e)
    {
        if (_api is null) return;
        if (NavList.SelectedItem is not ListViewItem item) return;
        if (item.Visibility != Visibility.Visible) return;

        var tag = item.Tag as string ?? "devices";

        if (_current is IDisposable disposable) disposable.Dispose();

        IPage page = tag switch
        {
            "points" => new PointsPage(_api),
            "drivers" => new DriversPage(_api),
            "monitor" => new MonitorPage(_api),
            "logs" => new LogsPage(_api),
            "audit" => new AuditPage(_api),
            "config" => new ConfigPage(_api),
            "users" => new UsersPage(_api),
            _ => new DevicesPage(_api),
        };

        _current = page;
        ContentHost.Children.Clear();
        ContentHost.Children.Add(page.View);
        _ = page.RefreshAsync();
    }

    // ------------------------------------------------------------------
    // 顶栏健康指示
    // ------------------------------------------------------------------

    private void StartHealthPolling()
    {
        _healthTimer?.Stop();

        _healthTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(2.5) };
        _healthTimer.Tick += async (_, _) => await RefreshHealthAsync();
        _healthTimer.Start();

        _ = RefreshHealthAsync();
    }

    private async Task RefreshHealthAsync()
    {
        if (_api is null) return;

        try
        {
            var health = await _api.HealthAsync();
            var color = health.Status switch
            {
                "ok" or "healthy" or "good" => "#1E8E3E",
                "degraded" or "warn" or "warning" => "#B26A00",
                _ => "#C0392B",
            };

            HealthDot.Fill = new SolidColorBrush(ColorOf(color));
            HealthText.Text = $"{health.Status} · 设备 {health.Devices.Collecting}/{health.Devices.Total} 采集 · " +
                              $"点位 {health.Points.Good}/{health.Points.Total} 良好";
        }
        catch (Exception ex)
        {
            HealthDot.Fill = new SolidColorBrush(ColorOf("#C0392B"));
            HealthText.Text = "网关不可达：" + Shorten(ex.Message);
        }

        if (VersionText.Text.Length == 0)
        {
            try
            {
                var version = await _api.VersionAsync();
                SiteText.Text = string.IsNullOrEmpty(version.Site) ? "未命名站点" : version.Site;
                VersionText.Text = $"v{version.Version} · 配置 {version.ConfigVersion}";
            }
            catch
            {
                // 顶栏信息缺失不影响主流程，静默即可。
            }
        }
    }

    private static string Shorten(string text)
        => text.Length <= 60 ? text : text[..60] + "…";

    private static Windows.UI.Color ColorOf(string hex)
    {
        var value = Convert.ToUInt32(hex.TrimStart('#'), 16);
        return Windows.UI.Color.FromArgb(
            255,
            (byte)((value >> 16) & 0xFF),
            (byte)((value >> 8) & 0xFF),
            (byte)(value & 0xFF));
    }
}
