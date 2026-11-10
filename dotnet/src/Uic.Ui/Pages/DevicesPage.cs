using System.Globalization;
using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;
using Uic.Domain;
using Windows.UI;

namespace Uic.Ui.Pages;

/// <summary>
/// 设备列表。首屏只拉汇总（/devices），展开某一行时才拉该设备的点位（/devices/{id}），
/// 否则 20 台设备 × 数百点位一次性灌进浏览器，滚动会明显卡顿。
///
/// 顶部的「新建设备」表单走完整配置提交（GET /config → 追加通道/设备 → validate → apply），
/// 而不是另开一个「只建设备」的旁路：现场首开流程本来就是「先建通道，再挂设备」，
/// 走同一条配置链路才能保证版本、校验与回滚和配置页完全一致。
/// </summary>
public sealed class DevicesPage : IPage, IDisposable
{
    private static readonly string[] Protocols =
        { "modbus-tcp", "modbus-rtu", "modbus-ascii", "s7", "opcua", "mqtt", "virtual" };

    private static readonly string[] PointTypes =
    {
        "uint16", "int16", "uint32", "int32", "uint64", "int64",
        "float32", "float64", "bool", "bcd16", "bcd32",
    };

    private static readonly string[] ModbusAreas = { "holding", "input", "coil", "discrete" };

    /// <summary>
    /// S7 的区名。db1/db2 是最常用的，先列出来；其余区在地址栏里照样能手填。
    /// 不在这里铺满 db1..db65535 的下拉——那种下拉没人能滚到第 40 项。
    /// </summary>
    private static readonly string[] S7Areas = { "db1", "db2", "db3", "db10", "i", "q", "m" };

    private readonly GatewayApi _api;
    private readonly StackPanel _list = new() { Spacing = 0 };
    private readonly TextBlock _status;
    private readonly TextBox _search;
    /// <summary>展开过的设备点位。列表本身不刷新时复用，避免每次展开都重拉一遍。</summary>
    private readonly Dictionary<string, PointDto[]> _expanded = new(StringComparer.Ordinal);

    /// <summary>驱动能力缓存：能力徽标按 protocol（= 驱动 id）查表。</summary>
    private Dictionary<string, DriverDto> _drivers = new(StringComparer.Ordinal);

    // ---- 新建设备表单 ----
    private readonly Border _formCard;
    private readonly StackPanel _form = new() { Spacing = 8 };
    private readonly ComboBox _channelPicker = new();
    private readonly StackPanel _newChannelFields = new() { Orientation = Orientation.Horizontal, Spacing = 10 };
    private readonly TextBox _channelId = Ui.Input(mono: true);
    private readonly ComboBox _protocol = new();
    private readonly StackPanel _tcpFields = new() { Orientation = Orientation.Horizontal, Spacing = 10 };
    private readonly TextBox _host = Ui.Input(mono: true);
    private readonly TextBox _port = Ui.Input(mono: true);
    private readonly StackPanel _serialFields = new() { Orientation = Orientation.Horizontal, Spacing = 10 };
    private readonly TextBox _serialPort = Ui.Input(mono: true);
    private readonly TextBox _baud = Ui.Input(mono: true);
    private readonly TextBox _deviceId = Ui.Input(mono: true);
    private readonly TextBox _deviceName = Ui.Input();
    private readonly TextBox _interval = Ui.Input(mono: true);
    private readonly TextBox _unitId = Ui.Input(mono: true);
    private readonly TextBox _pointId = Ui.Input(mono: true);
    private readonly TextBox _pointName = Ui.Input();
    private readonly ComboBox _pointType = new();
    private readonly ComboBox _area = new();
    private readonly TextBox _offset = Ui.Input(mono: true);

    /// <summary>OPC UA 的 NodeId 文本。寄存器协议的「区域 + 偏移」在 UA 上没有对应物。</summary>
    private readonly TextBox _nodeId = Ui.Input(mono: true);

    /// <summary>两种寻址各自的字段容器：切换协议时整块显示/隐藏，而不是逐个控件改。</summary>
    private readonly StackPanel _nodeIdField = new() { Orientation = Orientation.Horizontal, Spacing = 10 };
    private readonly StackPanel _registerFields = new() { Orientation = Orientation.Horizontal, Spacing = 10 };
    private readonly TextBlock _formResult = Ui.Text(string.Empty, 12, Ui.Faint, wrap: true);
    private readonly Button _createButton;

    /// <summary>通道目录。选择「既有通道」时才需要它，因此只在打开表单时拉一次。</summary>
    private List<(string Id, string Protocol)> _channels = new();

    /// <summary>用户是否手动改过端口。改过就不再用协议默认值覆盖他填的值。</summary>
    private bool _portTouched;

    /// <summary>程序正在回填默认端口：此时触发的 TextChanged 不算用户输入。</summary>
    private bool _settingPort;

    private bool _disposed;
    private bool _busy;
    private bool _formOpen;
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

        // 新建设备最终走 config.apply：没有 config.write 的角色不该看到这个入口。
        if (Session.Can(UiPermissions.ConfigWrite))
            actions.Children.Add(Ui.Button("新建设备", (_, _) => _ = ToggleFormAsync()));

        actions.Children.Add(Ui.Button("刷新", (_, _) => _ = RefreshAsync(), accent: true));

        // 按钮必须在 BuildForm 之前建好：表单构造时就要把它挂进行里。
        _createButton = Ui.Button("创建", (_, _) => _ = CreateAsync(), accent: true);

        BuildForm();

        _formCard = Ui.Band("新建设备", _form);
        _formCard.Margin = new Thickness(0, 0, 0, 12);
        _formCard.Visibility = Visibility.Collapsed;

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

        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        body.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });
        body.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });

        body.Children.Add(_formCard);
        body.Children.Add(card);
        Grid.SetRow(card, 1);

        Grid.SetRow(_status, 2);
        _status.Margin = new Thickness(2, 8, 0, 0);
        body.Children.Add(_status);

        View = root;
    }

    public FrameworkElement View { get; }

    // ------------------------------------------------------------------
    // 新建设备表单
    // ------------------------------------------------------------------

    /// <summary>
    /// 表单是内联展开的，不弹对话框：现场常用的动作是「建一台、看一眼列表、再建一台」，
    /// 弹窗每次都要重新定位，还会挡住刚刷新的结果。
    /// </summary>
    private void BuildForm()
    {
        _channelPicker.Items.Add("＋ 新建通道");
        _channelPicker.SelectedIndex = 0;
        _channelPicker.SelectionChanged += (_, _) => ApplyFormVisibility();

        _channelId.PlaceholderText = "ch-modbus-2";

        foreach (var protocol in Protocols) _protocol.Items.Add(protocol);
        _protocol.SelectedIndex = 0;
        _protocol.SelectionChanged += (_, _) => ApplyFormVisibility();

        _host.PlaceholderText = "192.168.1.10";
        _port.PlaceholderText = "502";
        _port.Text = "502";
        _port.TextChanged += (_, _) =>
        {
            // 程序自己写默认端口不算「用户改过」；否则切一次协议后
            // 就再也不跟着默认值走了。
            if (!_settingPort) _portTouched = true;
        };
        _serialPort.PlaceholderText = "/dev/ttyUSB0";
        _baud.PlaceholderText = "9600";
        _baud.Text = "9600";

        _deviceId.PlaceholderText = "meter-2";
        _deviceName.PlaceholderText = "电表 2（可选）";
        _interval.PlaceholderText = "1s";
        _interval.Text = "1s";
        _unitId.PlaceholderText = "1";
        _unitId.Text = "1";

        _pointId.PlaceholderText = "voltage";
        _pointName.PlaceholderText = "电压（可选）";
        _offset.PlaceholderText = "0 或 0.3";
        _nodeId.PlaceholderText = "ns=2;s=Demo.Temp";

        foreach (var type in PointTypes) _pointType.Items.Add(type);
        _pointType.SelectedIndex = 0;

        foreach (var area in ModbusAreas) _area.Items.Add(area);
        _area.SelectedIndex = 0;

        // 通道：既有通道下拉 + 新建通道时展开的传输参数。
        var channelRow = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 10 };
        channelRow.Children.Add(Field("通道", _channelPicker, 220));

        _newChannelFields.Children.Add(Field("通道 ID", _channelId, 170));
        _newChannelFields.Children.Add(Field("协议", _protocol, 150));
        channelRow.Children.Add(_newChannelFields);

        _tcpFields.Children.Add(Field("主机", _host, 170));
        _tcpFields.Children.Add(Field("端口", _port, 90));
        _serialFields.Children.Add(Field("串口设备", _serialPort, 170));
        _serialFields.Children.Add(Field("波特率", _baud, 90));

        var transportRow = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 10 };
        transportRow.Children.Add(_tcpFields);
        transportRow.Children.Add(_serialFields);

        var deviceRow = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 10 };
        deviceRow.Children.Add(Field("设备 ID", _deviceId, 170));
        deviceRow.Children.Add(Field("名称", _deviceName, 190));
        deviceRow.Children.Add(Field("采集周期", _interval, 100));
        deviceRow.Children.Add(Field("从站地址", _unitId, 90));

        _nodeIdField.Children.Add(Field("节点 / 主题", _nodeId, 260));
        _registerFields.Children.Add(Field("区域", _area, 130));
        _registerFields.Children.Add(Field("偏移", _offset, 90));

        var pointRow = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 10 };
        pointRow.Children.Add(Field("首个点位 ID", _pointId, 150));
        pointRow.Children.Add(Field("点位名称", _pointName, 170));
        pointRow.Children.Add(Field("数据类型", _pointType, 130));
        pointRow.Children.Add(_registerFields);
        pointRow.Children.Add(_nodeIdField);

        var buttons = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 8 };
        buttons.Children.Add(_createButton);
        buttons.Children.Add(Ui.Button("取消", (_, _) => _ = ToggleFormAsync()));

        _form.Children.Add(channelRow);
        _form.Children.Add(transportRow);
        _form.Children.Add(deviceRow);
        _form.Children.Add(pointRow);
        _form.Children.Add(buttons);
        _form.Children.Add(_formResult);
        _form.Children.Add(Ui.Caption(
            "地址填「区域 + 偏移」，例如 holding 区偏移 0；偏移写成 0.3 表示第 3 位。" +
            "创建会提交整份配置并重启受影响的设备，校验不通过时不会落盘。", wrap: true));

        ApplyFormVisibility();
    }

    private static StackPanel Field(string label, FrameworkElement control, double width)
    {
        control.Width = width;

        var stack = new StackPanel { Spacing = 2 };
        stack.Children.Add(Ui.Text(label, 11, Ui.Faint));
        stack.Children.Add(control);
        return stack;
    }

    private string SelectedProtocol => _protocol.SelectedItem as string ?? Protocols[0];

    private string SelectedPointType => _pointType.SelectedItem as string ?? PointTypes[0];

    /// <summary>
    /// 当前区域下拉里选中的值。下拉内容随协议变化（S7 没有 holding/coil），
    /// 所以不能像以前那样写死回退到 <c>holding</c>——那会把 S7 的点位
    /// 悄悄写成 Modbus 的形状。
    /// </summary>
    private string SelectedArea =>
        _area.SelectedItem as string
        ?? (_area.Items.Count > 0 ? _area.Items[0] as string : null)
        ?? "holding";

    private bool SelectedProtocolIsS7 => SelectedProtocol == "s7";

    /// <summary>OPC UA 用 NodeId 文本寻址：没有区名，也没有寄存器偏移。</summary>
    private bool SelectedProtocolIsOpcUa => SelectedProtocol == "opcua";

    /// <summary>MQTT 用主题寻址，和 UA 一样没有区名/寄存器偏移；字段框共用。</summary>
    private bool SelectedProtocolIsMqtt => SelectedProtocol == "mqtt";

    /// <summary>符号寻址：地址是一段文本（NodeId 或主题）。</summary>
    private bool SelectedProtocolUsesSymbol => SelectedProtocolIsOpcUa || SelectedProtocolIsMqtt;

    /// <summary>协议决定可用的区名。S7 的区是 db&lt;N&gt;/i/q/m，与 Modbus 的 holding/coil 不通用。</summary>
    private void RebuildAreaPicker()
    {
        var wanted = SelectedProtocolIsS7 ? S7Areas : ModbusAreas;
        var previous = _area.SelectedItem as string;

        _area.Items.Clear();
        foreach (var area in wanted) _area.Items.Add(area);

        var index = previous is null ? -1 : Array.IndexOf(wanted, previous);
        _area.SelectedIndex = index >= 0 ? index : 0;

        _offset.PlaceholderText = SelectedProtocolIsS7 ? "字节偏移，例如 0 或 0.3" : "0 或 0.3";
    }

    /// <summary>
    /// 只显示当前选择下真正有用的字段。虚拟设备没有传输参数，
    /// 串口设备不该看到 host/port —— 少一个能填错的框，就少一次 422。
    /// 区域下拉也要跟着协议换：S7 没有 holding/coil，留着只会让人选错。
    /// </summary>
    private void ApplyFormVisibility()
    {
        var creatingChannel = _channelPicker.SelectedIndex <= 0;
        var transport = ConfigDraft.TransportFor(SelectedProtocol);

        _newChannelFields.Visibility = creatingChannel ? Visibility.Visible : Visibility.Collapsed;
        _tcpFields.Visibility = creatingChannel && transport == "tcp" ? Visibility.Visible : Visibility.Collapsed;
        _serialFields.Visibility = creatingChannel && transport == "serial" ? Visibility.Visible : Visibility.Collapsed;

        RebuildAreaPicker();

        // 寻址方式随协议切换：UA/MQTT 没有寄存器窗口，寄存器协议也没有 NodeId/主题。
        _registerFields.Visibility = SelectedProtocolUsesSymbol ? Visibility.Collapsed : Visibility.Visible;
        _nodeIdField.Visibility = SelectedProtocolUsesSymbol ? Visibility.Visible : Visibility.Collapsed;
        _nodeId.PlaceholderText = SelectedProtocolIsMqtt
            ? "plant/line1/temp"
            : "ns=2;s=Demo.Temp";

        // 端口默认值只在「刚切协议」时有意义：填过 102 再切回 Modbus 不该被改回 502，
        // 所以只在用户没动过的时候替换。
        if (creatingChannel && transport == "tcp" && !_portTouched)
        {
            _settingPort = true;
            _port.Text = ConfigDraft.DefaultPort(SelectedProtocol).ToString(CultureInfo.InvariantCulture);
            _settingPort = false;
        }
    }

    private async Task ToggleFormAsync()
    {
        _formOpen = !_formOpen;
        _formCard.Visibility = _formOpen ? Visibility.Visible : Visibility.Collapsed;

        if (!_formOpen || _disposed) return;

        SetFormResult(string.Empty, Ui.Faint);
        await LoadChannelsAsync();
    }

    private async Task LoadChannelsAsync()
    {
        try
        {
            var response = await _api.ConfigAsync();
            if (_disposed) return;

            var channels = new List<(string, string)>();

            if (JsonNode.Parse(response.Config.GetRawText())?["channels"] is JsonArray items)
            {
                foreach (var item in items)
                {
                    var id = item?["id"]?.ToString();
                    if (string.IsNullOrWhiteSpace(id)) continue;

                    channels.Add((id, item?["protocol"]?.ToString() ?? string.Empty));
                }
            }

            _channels = channels;
            RebuildChannelPicker();
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            // 通道目录只是方便选择：拉不到时仍然可以手填一个新的通道 ID。
            SetFormResult("读取通道列表失败，可直接新建通道：" + ex.Message, Ui.Bad);
        }
    }

    private void RebuildChannelPicker()
    {
        var previous = _channelPicker.SelectedIndex;

        _channelPicker.Items.Clear();
        _channelPicker.Items.Add("＋ 新建通道");

        foreach (var (id, protocol) in _channels)
        {
            _channelPicker.Items.Add(string.IsNullOrEmpty(protocol) ? id : $"{id} · {protocol}");
        }

        _channelPicker.SelectedIndex =
            previous > 0 && previous < _channelPicker.Items.Count ? previous : 0;

        ApplyFormVisibility();
    }

    private async Task CreateAsync()
    {
        if (_busy || _disposed) return;

        var problems = new List<string>();

        var deviceId = (_deviceId.Text ?? string.Empty).Trim();
        if (deviceId.Length == 0) problems.Add("设备 ID 不能为空（^[a-z0-9][a-z0-9._-]{0,63}$）");

        var pointId = (_pointId.Text ?? string.Empty).Trim();
        if (pointId.Length == 0) problems.Add("首个点位 ID 不能为空（设备至少要有一个点位）");

        var interval = TimeSpan.FromSeconds(1);
        if (!string.IsNullOrWhiteSpace(_interval.Text))
        {
            try
            {
                interval = DurationConverter.Parse(_interval.Text!);
            }
            catch (FormatException)
            {
                problems.Add("采集周期无法解析，例如 1s / 500ms / 2m");
            }
        }

        if (!TryInt(_unitId.Text, out var unitId)) problems.Add("从站地址必须是 1..255 的整数");

        var offset = 0;
        var bit = -1;

        if (SelectedProtocolIsOpcUa)
        {
            // UA 的点位没有寄存器偏移；缺 NodeId 时不要退回 "0"，那会生成一个
            // 语法合法但指向 NamespaceIndex 0 的节点，读回来永远是 BadNodeIdUnknown。
            if (string.IsNullOrWhiteSpace(_nodeId.Text))
                problems.Add("OPC UA 点位必须填写 NodeId，例如 ns=2;s=Demo.Temp");
        }
        else if (SelectedProtocolIsMqtt)
        {
            if (string.IsNullOrWhiteSpace(_nodeId.Text))
                problems.Add("MQTT 点位必须填写主题，例如 plant/line1/temp");
        }
        else if (!TryParseOffset(_offset.Text, SelectedProtocolIsS7 ? 7 : 15,
                     out offset, out bit, out var offsetError))
        {
            problems.Add(offsetError!);
        }

        // S7 的 bool 必须落在某一位上：整字节非零不等于「ON」，猜出来的值比读不出更危险。
        if (SelectedProtocolIsS7 && SelectedPointType == "bool" && bit < 0)
            problems.Add("S7 的 bool 点位必须带位号，例如 0.0 或 12.5");

        var creatingChannel = _channelPicker.SelectedIndex <= 0;

        var channelId = creatingChannel
            ? (_channelId.Text ?? string.Empty).Trim()
            : _channels[_channelPicker.SelectedIndex - 1].Id;

        if (creatingChannel && channelId.Length == 0) problems.Add("新建通道时通道 ID 不能为空");

        var transport = ConfigDraft.TransportFor(SelectedProtocol);

        var port = ConfigDraft.DefaultPort(SelectedProtocol);
        if (creatingChannel && transport == "tcp" && !TryInt(_port.Text, out port))
            problems.Add("端口必须是 1..65535 的整数");

        var baud = 9600;
        if (creatingChannel && transport == "serial" && !TryInt(_baud.Text, out baud))
            problems.Add("波特率必须是 300..921600 的整数");

        if (problems.Count > 0)
        {
            ShowProblems(problems);
            return;
        }

        _busy = true;
        _createButton.IsEnabled = false;
        SetFormResult("正在读取当前配置…", Ui.Faint);

        try
        {
            var current = await _api.ConfigAsync();
            if (_disposed) return;

            // 直接改 GET /config 返回的树：脱敏字段（"***"）原样保留，服务端
            // RedactedSecrets.Restore 会在应用前换回当前密钥，页面不必操心口令。
            if (JsonNode.Parse(current.Config.GetRawText()) is not JsonObject root)
            {
                SetFormResult("当前配置不是 JSON 对象，无法在此页新建设备", Ui.Bad);
                return;
            }

            if (creatingChannel)
            {
                var channel = ConfigDraft.NewChannel(
                    channelId, null, SelectedProtocol,
                    host: _host.Text, port: port, serialPort: _serialPort.Text, baud: baud);

                if (ConfigDraft.AddChannel(root, channel) is { } channelError)
                {
                    ShowProblems(new[] { channelError });
                    return;
                }
            }

            var point = SelectedProtocolUsesSymbol
                ? ConfigDraft.NewSymbolPoint(
                    pointId, _pointName.Text, SelectedPointType, _nodeId.Text ?? string.Empty)
                : ConfigDraft.NewRegisterPoint(
                    pointId, _pointName.Text, SelectedPointType, SelectedArea, offset, bit);

            var device = ConfigDraft.NewDevice(
                deviceId, _deviceName.Text, channelId, interval, unitId, point);

            if (ConfigDraft.AddDevice(root, device) is { } deviceError)
            {
                ShowProblems(new[] { deviceError });
                return;
            }

            var text = ToJson(root);

            // 校验走服务端同一套 ModelValidator：浏览器里再写一份规则，
            // 迟早出现「页面说没问题、网关说不行」。
            var validation = await _api.ValidateAsync(text);
            if (_disposed) return;

            if (!validation.Valid)
            {
                ShowProblems(validation.Problems);
                return;
            }

            SetFormResult("校验通过，正在应用…", Ui.Faint);

            var applied = await _api.ApplyAsync(text, $"控制台新建设备 {deviceId}");
            if (_disposed) return;

            if (!applied.Applied)
            {
                SetFormResult($"未应用（版本 {applied.Version}）", Ui.Bad);
                return;
            }

            SetFormResult(
                $"已创建设备 {deviceId} · 配置版本 {applied.Version} · {applied.DurationMs} ms" +
                (applied.RestartedDevices.Length == 0
                    ? string.Empty
                    : $" · 重启 {string.Join(',', applied.RestartedDevices)}"),
                Ui.Good);

            _deviceId.Text = string.Empty;
            _deviceName.Text = string.Empty;
            _pointId.Text = string.Empty;
            _pointName.Text = string.Empty;
            _nodeId.Text = string.Empty;

            await LoadChannelsAsync();
            await RefreshAsync();
        }
        catch (Exception ex)
        {
            if (_disposed) return;

            SetFormResult("创建设备失败：" + ex.Message, Ui.Bad);
        }
        finally
        {
            if (!_disposed)
            {
                _busy = false;
                _createButton.IsEnabled = true;
            }
        }
    }

    /// <summary>
    /// 偏移解析只处理「非负整数[.位号]」。参考号（40001）的解析规则留在
    /// <c>Uic.Config</c> 的点表导入里：同一套规则抄两遍，两个入口迟早给出不同结果。
    ///
    /// <paramref name="maxBit"/> 由协议决定：Modbus 的线圈/寄存器位可到 15，
    /// S7 的位地址只有 0..7，写超过 7 的位会在线路上被 PLC 拒掉。
    /// </summary>
    private static bool TryParseOffset(
        string? text, int maxBit, out int offset, out int bit, out string? error)
    {
        offset = 0;
        bit = -1;
        error = null;

        var value = (text ?? string.Empty).Trim();
        if (value.Length == 0)
        {
            error = "偏移不能为空";
            return false;
        }

        var dot = value.IndexOf('.');
        if (dot > 0)
        {
            if (!int.TryParse(value[(dot + 1)..], NumberStyles.Integer, CultureInfo.InvariantCulture,
                    out bit) || bit < 0 || bit > maxBit)
            {
                error = $"位号必须是 0..{maxBit}";
                return false;
            }

            value = value[..dot];
        }

        if (!int.TryParse(value, NumberStyles.Integer, CultureInfo.InvariantCulture, out offset) ||
            offset < 0)
        {
            error = "偏移必须是非负整数";
            return false;
        }

        return true;
    }

    private static bool TryInt(string? text, out int value)
        => int.TryParse((text ?? string.Empty).Trim(), NumberStyles.Integer,
            CultureInfo.InvariantCulture, out value);

    private void ShowProblems(IEnumerable<string> problems)
    {
        var lines = problems.Where(p => !string.IsNullOrWhiteSpace(p)).ToArray();

        SetFormResult(
            "未提交：" + Environment.NewLine +
            string.Join(Environment.NewLine, lines.Select(p => "· " + p)),
            Ui.Bad);
    }

    private void SetFormResult(string text, Color color)
    {
        _formResult.Text = text;
        _formResult.Foreground = new SolidColorBrush(color);
    }

    /// <summary>
    /// 把配置树写回文本。手写 <see cref="Utf8JsonWriter"/> 而不是 JsonSerializer：
    /// 后者要查类型元数据，裁剪后的 WASM 里可能拿不到，而 JsonNode 自带 WriteTo。
    /// </summary>
    private static string ToJson(JsonNode node)
    {
        using var buffer = new MemoryStream();

        using (var writer = new Utf8JsonWriter(buffer, new JsonWriterOptions
        {
            Indented = true,
            Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping,
        }))
        {
            node.WriteTo(writer);
        }

        return Encoding.UTF8.GetString(buffer.ToArray());
    }

    // ------------------------------------------------------------------
    // 设备列表
    // ------------------------------------------------------------------

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
