using Microsoft.UI.Text;
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Media;
using Windows.UI;

namespace Uic.Ui;

/// <summary>
/// 用代码搭界面。这批页面结构简单（表格 + 表单），XAML 的 DataTemplate/绑定反而绕：
/// 数据一行行手工放，改了字段编译期就报错，不用等运行时看空单元格。
/// </summary>
public static class Ui
{
    public static readonly Color Ink = UiFormat.Color("#1B2029");
    public static readonly Color Muted = UiFormat.Color("#5B6472");
    public static readonly Color Faint = UiFormat.Color("#8A94A3");
    public static readonly Color Line = UiFormat.Color("#E1E5EB");
    public static readonly Color Accent = UiFormat.Color("#1D5FA8");
    public static readonly Color Good = UiFormat.Color("#1E8E3E");
    public static readonly Color Bad = UiFormat.Color("#C0392B");

    public static TextBlock Text(
        string text,
        double size = 13,
        Color? color = null,
        bool bold = false,
        bool mono = false,
        bool wrap = false)
    {
        var block = new TextBlock
        {
            Text = text,
            FontSize = size,
            Foreground = new SolidColorBrush(color ?? Ink),
            FontWeight = bold ? FontWeights.SemiBold : FontWeights.Normal,
            TextWrapping = wrap ? TextWrapping.Wrap : TextWrapping.NoWrap,
            TextTrimming = wrap ? TextTrimming.None : TextTrimming.CharacterEllipsis,
            VerticalAlignment = VerticalAlignment.Center,
        };

        if (mono) block.FontFamily = new FontFamily("Consolas, Menlo, monospace");

        return block;
    }

    public static TextBlock Caption(string text, bool wrap = false)
        => Text(text, 11.5, Faint, wrap: wrap);

    public static Border Panel(UIElement child, Thickness? padding = null, Color? background = null, double radius = 8)
        => new()
        {
            Child = child,
            Padding = padding ?? new Thickness(16),
            Background = new SolidColorBrush(background ?? UiFormat.Color("#FFFFFF")),
            BorderBrush = new SolidColorBrush(Line),
            BorderThickness = new Thickness(1),
            CornerRadius = new CornerRadius(radius),
        };

    public static Border Band(string title, UIElement body)
    {
        var stack = new StackPanel { Spacing = 10 };
        stack.Children.Add(Text(title, 14, Ink, bold: true));
        stack.Children.Add(body);

        return Panel(stack, new Thickness(16), Color.FromArgb(255, 0xF5, 0xF7, 0xFA));
    }

    public static Grid Columns(params (GridLength Width, UIElement? Cell)[] cells)
    {
        var grid = new Grid();
        for (var i = 0; i < cells.Length; i++)
        {
            grid.ColumnDefinitions.Add(new ColumnDefinition { Width = cells[i].Width });

            if (cells[i].Cell is { } cell)
            {
                Grid.SetColumn(cell, i);
                grid.Children.Add(cell);
            }
        }

        return grid;
    }

    public static GridLength Star(double value = 1) => new(value, GridUnitType.Star);
    public static GridLength Px(double value) => new(value, GridUnitType.Pixel);
    public static GridLength Auto() => GridLength.Auto;

    /// <summary>表头。比数据行淡一档，滚动时不抢注意力。</summary>
    public static Border Header(params (GridLength Width, string Text)[] columns)
    {
        var cells = new (GridLength, UIElement?)[columns.Length];
        for (var i = 0; i < columns.Length; i++)
        {
            cells[i] = (columns[i].Width, Text(columns[i].Text, 11.5, Faint, bold: true));
        }

        return new Border
        {
            Child = Columns(cells),
            Padding = new Thickness(12, 8, 12, 8),
            Background = new SolidColorBrush(UiFormat.Color("#F7F9FB")),
            BorderBrush = new SolidColorBrush(Line),
            BorderThickness = new Thickness(0, 0, 0, 1),
        };
    }

    public static Border Row(params (GridLength Width, UIElement? Cell)[] cells)
        => new()
        {
            Child = Columns(cells),
            Padding = new Thickness(12, 8, 12, 8),
            BorderBrush = new SolidColorBrush(Line),
            BorderThickness = new Thickness(0, 0, 0, 1),
        };

    public static Border Dot(Color color, double size = 8)
        => new()
        {
            Width = size,
            Height = size,
            CornerRadius = new CornerRadius(size / 2),
            Background = new SolidColorBrush(color),
            VerticalAlignment = VerticalAlignment.Center,
        };

    public static Border Chip(string text, Color foreground)
        => new()
        {
            Child = Text(text, 11, foreground, bold: true),
            Background = new SolidColorBrush(Color.FromArgb(
                24, foreground.R, foreground.G, foreground.B)),
            CornerRadius = new CornerRadius(9),
            Padding = new Thickness(8, 2, 8, 2),
            VerticalAlignment = VerticalAlignment.Center,
            HorizontalAlignment = HorizontalAlignment.Left,
        };

    public static Button Button(string text, RoutedEventHandler onClick, bool accent = false)
    {
        var button = new Button
        {
            Content = text,
            Padding = new Thickness(14, 6, 14, 6),
        };

        if (accent)
        {
            button.Style = (Style)Application.Current.Resources["AccentButtonStyle"];
        }

        button.Click += onClick;
        return button;
    }

    public static TextBox Input(string? header = null, string? placeholder = null, bool mono = false)
    {
        var box = new TextBox
        {
            PlaceholderText = placeholder ?? string.Empty,
        };

        if (header is not null) box.Header = header;
        if (mono) box.FontFamily = new FontFamily("Consolas, Menlo, monospace");

        return box;
    }

    /// <summary>
    /// 表格容器。横向滚动必须关掉：ScrollViewer 一旦允许横向滚动，
    /// 就会用无限宽度测量内容，星号列随之塌成内容宽度，整行叠在一起。
    /// 需要横向滚动的调用方（例如长 JSON）由控件自己处理。
    /// </summary>
    public static ScrollViewer Scroll(UIElement content)
        => new()
        {
            Content = content,
            VerticalScrollBarVisibility = ScrollBarVisibility.Auto,
            HorizontalScrollBarVisibility = ScrollBarVisibility.Disabled,
        };

    /// <summary>页面骨架：标题 + 右侧操作区 + 内容区。所有页面用同一套留白。</summary>
    public static (Grid Root, StackPanel Actions, Grid Body) Scaffold(string title, string subtitle)
    {
        var actions = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            Spacing = 8,
            HorizontalAlignment = HorizontalAlignment.Right,
            VerticalAlignment = VerticalAlignment.Center,
        };

        var head = new Grid { Margin = new Thickness(0, 0, 0, 14) };
        head.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });
        head.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });

        var titles = new StackPanel { Spacing = 2 };
        titles.Children.Add(Text(title, 20, Ink, bold: true));
        titles.Children.Add(Caption(subtitle));

        head.Children.Add(titles);

        Grid.SetColumn(actions, 1);
        head.Children.Add(actions);

        var body = new Grid();

        var root = new Grid { Padding = new Thickness(20) };
        root.RowDefinitions.Add(new RowDefinition { Height = GridLength.Auto });
        root.RowDefinitions.Add(new RowDefinition { Height = new GridLength(1, GridUnitType.Star) });

        root.Children.Add(head);

        Grid.SetRow(body, 1);
        root.Children.Add(body);

        return (root, actions, body);
    }
}
