using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;
using Uic.Ui.Pages;

namespace Uic.Ui;

public sealed partial class MainPage : Page
{
    private GatewayApi? _api;
    private DispatcherTimer? _healthTimer;
    private IPage? _current;

    public MainPage()
    {
        InitializeComponent();
        Loaded += OnLoaded;
        Unloaded += OnUnloaded;
    }

    private async void OnLoaded(object sender, RoutedEventArgs e)
    {
        // 先不带条件探测一次：网关未配置令牌时应当直接进控制台，
        // 而不是让现场操作员对着一个空令牌输入框发愣。
        // 令牌可能早已被网关换掉，猜错了也只是 401 —— ProbeAsync 会如实返回 false。
        var token = TokenStore.Load();

        _api = new GatewayApi();

        if (await ProbeAsync(_api))
        {
            EnterShell();
            return;
        }

        _api = null;
        TokenStore.Save(null);

        ShowLogin(string.IsNullOrEmpty(token) ? null : "上次使用的令牌已失效，请重新输入。");
    }

    private void OnUnloaded(object sender, RoutedEventArgs e)
    {
        _healthTimer?.Stop();
        _healthTimer = null;
    }

    // ------------------------------------------------------------------
    // 登录
    // ------------------------------------------------------------------

    private void ShowLogin(string? message)
    {
        LoginOverlay.Visibility = Visibility.Visible;
        LoginError.Visibility = message is null ? Visibility.Collapsed : Visibility.Visible;
        LoginError.Text = message ?? string.Empty;
        TokenBox.Focus(FocusState.Programmatic);
    }

    private async void OnConnectClick(object sender, RoutedEventArgs e)
    {
        var token = TokenBox.Text?.Trim();
        ConnectButton.IsEnabled = false;
        LoginStatus.Text = "正在连接…";
        LoginError.Visibility = Visibility.Collapsed;

        try
        {
            TokenStore.Save(string.IsNullOrEmpty(token) ? null : token);
            var api = new GatewayApi();

            if (!await ProbeAsync(api))
            {
                TokenStore.Save(null);
                LoginError.Text = "无法连接网关，或令牌不正确。请确认网关地址与令牌。";
                LoginError.Visibility = Visibility.Visible;
                LoginStatus.Text = string.Empty;
                return;
            }

            _api = api;
            LoginStatus.Text = string.Empty;
            EnterShell();
        }
        catch (Exception ex)
        {
            LoginError.Text = ex.Message;
            LoginError.Visibility = Visibility.Visible;
            LoginStatus.Text = string.Empty;
        }
        finally
        {
            ConnectButton.IsEnabled = true;
        }
    }

    /// <summary>拿一个公开端点探活：/api/v1/version 不需要令牌，能连上就说明地址对了。</summary>
    private static async Task<bool> ProbeAsync(GatewayApi api)
    {
        try
        {
            var version = await api.VersionAsync();
            return !string.IsNullOrEmpty(version.Name);
        }
        catch (GatewayException ex) when (ex.Unauthorized)
        {
            return false;
        }
        catch
        {
            return false;
        }
    }

    private void EnterShell()
    {
        LoginOverlay.Visibility = Visibility.Collapsed;
        NavList.SelectedIndex = 0;
        StartHealthPolling();
    }

    private void OnSignOutClick(object sender, RoutedEventArgs e)
    {
        _healthTimer?.Stop();
        _healthTimer = null;

        if (_current is IDisposable disposable) disposable.Dispose();
        _current = null;

        _api = null;
        TokenStore.Save(null);
        ContentHost.Children.Clear();
        HealthDot.Fill = new SolidColorBrush(ColorOf("#7A8697"));
        HealthText.Text = "已退出";
        SiteText.Text = "—";
        VersionText.Text = string.Empty;
        ShowLogin(null);
    }

    // ------------------------------------------------------------------
    // 导航
    // ------------------------------------------------------------------

    private void OnNavSelectionChanged(object sender, SelectionChangedEventArgs e)
    {
        if (_api is null) return;
        if (NavList.SelectedItem is not ListViewItem item) return;

        var tag = item.Tag as string ?? "devices";

        if (_current is IDisposable disposable) disposable.Dispose();

        IPage page = tag switch
        {
            "points" => new PointsPage(_api),
            "drivers" => new DriversPage(_api),
            "monitor" => new MonitorPage(_api),
            "logs" => new LogsPage(_api),
            "config" => new ConfigPage(_api),
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
