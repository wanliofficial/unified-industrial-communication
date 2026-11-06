using System.Text.Json;
using System.Text.Json.Serialization;

namespace Uic.Ui;

/// <summary>
/// UI 侧的 JSON 元数据上下文。
///
/// WASM 发布启用了裁剪，<c>System.Text.Json</c> 的运行时反射序列化会被裁掉；
/// 因此所有出入参必须在编译期登记。好处是字段改名会直接编译报错，而不是等到
/// 浏览器里才发现某一列永远空白。
/// </summary>
[JsonSourceGenerationOptions(
    PropertyNamingPolicy = JsonKnownNamingPolicy.CamelCase,
    PropertyNameCaseInsensitive = true,
    DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
    ReadCommentHandling = JsonCommentHandling.Skip,
    AllowTrailingCommas = true)]
[JsonSerializable(typeof(HealthDto))]
[JsonSerializable(typeof(VersionDto))]
[JsonSerializable(typeof(Paged<DeviceSummaryDto>))]
[JsonSerializable(typeof(Paged<PointDto>))]
[JsonSerializable(typeof(DriversDto))]
[JsonSerializable(typeof(Paged<LogEntryDto>))]
[JsonSerializable(typeof(ConfigResponseDto))]
[JsonSerializable(typeof(Paged<ConfigVersionDto>))]
[JsonSerializable(typeof(ValidateResponseDto))]
[JsonSerializable(typeof(ApplyResponseDto))]
[JsonSerializable(typeof(WriteResponseDto))]
[JsonSerializable(typeof(CommandResponseDto))]
[JsonSerializable(typeof(WriteRequest))]
[JsonSerializable(typeof(ValidateRequest))]
[JsonSerializable(typeof(ApplyRequest))]
[JsonSerializable(typeof(CommandRequest))]
[JsonSerializable(typeof(JsonElement))]
internal sealed partial class UiJson : JsonSerializerContext;
