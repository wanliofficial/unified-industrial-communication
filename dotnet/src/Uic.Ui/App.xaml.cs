using Microsoft.UI.Xaml;

namespace Uic.Ui;

public partial class App : Application
{
    private Window? _window;

    public App()
    {
        InitializeComponent();
    }

    protected override void OnLaunched(LaunchActivatedEventArgs args)
    {
        _window = new Window
        {
            Title = "UIC 工业连接底座",
        };

        _window.Content = new MainPage();
        _window.Activate();
    }
}
