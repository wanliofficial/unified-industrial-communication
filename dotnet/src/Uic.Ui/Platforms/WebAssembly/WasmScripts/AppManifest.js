// 应用清单：Uno 的 bootstrapper 与 Fluent 主题启动时读取这个全局对象。
// 缺了它，Uno.UI.js 会在 require 阶段直接报 scripterror，整个 WASM 应用起不来。
var UnoAppManifest = {
    splashScreenImage: "",
    splashScreenColor: "#f2f5f9",
    displayName: "UIC 工业连接底座",
    // 与 App.xaml 中 Application.Resources 的强调色保持一致。
    accentColor: "#1d5fa8",
    lightThemeAccentColor: "#1d5fa8",
    darkThemeAccentColor: "#3d8bd4",
    lightThemeBackgroundColor: "#f2f5f9",
    darkThemeBackgroundColor: "#151a21",
};
