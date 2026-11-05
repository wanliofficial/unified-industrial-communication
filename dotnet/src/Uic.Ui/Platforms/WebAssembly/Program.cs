using Microsoft.UI.Xaml;
using Uno.UI.Hosting;

namespace Uic.Ui;

/// <summary>
/// WebAssembly 宿主入口。Uno 6.x 用 <see cref="UnoPlatformHostBuilder"/> 选平台宿主。
///
/// 必须 await <c>RunAsync</c>：WASM 的宿主循环是「启动后永不返回」，
/// 同步阻塞等它完成会直接死锁（浏览器里没有可让出的线程），
/// 宿主会因此抛 InvalidOperationException。
/// </summary>
public sealed class Program
{
    public static async Task Main(string[] args)
    {
        var host = UnoPlatformHostBuilder.Create()
            .App(() => new App())
            .UseWebAssembly()
            .Build();

        await host.RunAsync();
    }
}
