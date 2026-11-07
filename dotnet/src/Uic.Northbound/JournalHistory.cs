using System.Globalization;
using System.Text.Json;
using Uic.Domain;

namespace Uic.Northbound;

/// <summary>一次历史查询的过滤条件。时间已是 UTC。</summary>
public sealed record HistoryQuery(
    string? Device,
    string? Point,
    DateTimeOffset From,
    DateTimeOffset To,
    TimeSpan? Interval);

/// <summary>扫描结果。分页在端点层做，读取层只负责过滤、去重与降采样。</summary>
public sealed record HistoryResult(IReadOnlyList<SampleDto> Items, int Total);

/// <summary>
/// 基于 <c>journal</c> sink 的本地历史查询。
///
/// 首版不引入时序库：journal 已经把样本按时间顺序落成 NDJSON，直接顺序扫描即可。
/// 代价是每次查询都要读文件，因此这里做四件事来守住可用性：
/// 1. 原始字节按行扫描，逐字段解析，避免每条样本都走反射式反序列化；
/// 2. 设备/点位不匹配时在解析到对应字段后立即放弃，常见过滤只读两个属性；
/// 3. 按 <c>(device, point, seq)</c> 去重——Outbox 是至少一次投递，重放会写出重复行；
/// 4. 可选按时间桶降采样，每个点位每桶只保留最后一条。
///
/// 保留策略仍是 journal 文件的策略：没有自动滚动清理，数据量上来后应换时序库，
/// 但 <c>GET /api/v1/history</c> 的接口形状不需要变。
/// </summary>
public sealed class JournalHistory
{
    private readonly string _path;

    public JournalHistory(string dataDir)
        => _path = Path.Combine(dataDir, "journal", "journal.ndjson");

    public string FilePath => _path;

    public async Task<HistoryResult> QueryAsync(HistoryQuery query, CancellationToken ct)
    {
        // journal 是可选的：没开过 journal 的现场应该看到空历史，而不是 404。
        if (!File.Exists(_path)) return new HistoryResult(Array.Empty<SampleDto>(), 0);

        var seen = new HashSet<(string Device, string Point, ulong Seq)>();

        // 不开降采样时直接累积原始样本；开了之后每个 (设备, 点位, 时间桶) 只留最后一条。
        List<SampleDto>? raw = query.Interval is null ? new List<SampleDto>() : null;
        Dictionary<(string Device, string Point, long Bucket), SampleDto>? buckets =
            query.Interval is null
                ? null
                : new Dictionary<(string, string, long), SampleDto>();

        try
        {
            await using var stream = new FileStream(
                _path,
                FileMode.Open,
                FileAccess.Read,
                // 允许采集进程继续追加；读到的只是查询执行时已经落盘的部分。
                FileShare.ReadWrite | FileShare.Delete,
                bufferSize: 64 * 1024,
                FileOptions.Asynchronous | FileOptions.SequentialScan);

            var lines = new ByteLineReader(stream);

            while (await lines.NextAsync(ct).ConfigureAwait(false))
            {
                ct.ThrowIfCancellationRequested();

                SampleDto? sample;
                try
                {
                    sample = ParseSample(lines.Current.Span, query);
                }
                catch (JsonException)
                {
                    // 掉电只可能损坏最后半行；一条坏行不能毁掉整次查询。
                    continue;
                }
                catch (InvalidOperationException)
                {
                    continue;
                }
                catch (FormatException)
                {
                    continue;
                }

                if (sample is null) continue;
                if (!seen.Add((sample.Device, sample.Point, sample.Seq))) continue;

                if (buckets is null)
                {
                    raw!.Add(sample);
                    continue;
                }

                var bucket = sample.CollectedAt.UtcTicks / query.Interval!.Value.Ticks;
                var key = (sample.Device, sample.Point, bucket);

                if (!buckets.TryGetValue(key, out var existing) || IsNewer(sample, existing))
                    buckets[key] = sample;
            }
        }
        catch (FileNotFoundException)
        {
            // 查询期间 journal 被运维清理掉：按「没有历史」返回，不把 500 抛给前端。
            return new HistoryResult(Array.Empty<SampleDto>(), 0);
        }
        catch (DirectoryNotFoundException)
        {
            return new HistoryResult(Array.Empty<SampleDto>(), 0);
        }

        var items = buckets is null ? raw! : buckets.Values.ToList();
        items.Sort(CompareSamples);

        return new HistoryResult(items, items.Count);
    }

    /// <summary>
    /// 手写逐字段解析。属性顺序来自 <see cref="SampleDto"/> 的固定序列化结果，
    /// 但实现不依赖顺序：每个属性都按键名匹配，未知字段直接跳过。
    /// </summary>
    private static SampleDto? ParseSample(ReadOnlySpan<byte> utf8, HistoryQuery query)
    {
        var reader = new Utf8JsonReader(utf8, isFinalBlock: true, state: default);
        if (!reader.Read() || reader.TokenType != JsonTokenType.StartObject) return null;

        string? device = null;
        string? point = null;
        string? quality = null;
        string? unit = null;
        string? error = null;
        DateTimeOffset? deviceTime = null;
        var collectedAt = default(DateTimeOffset);
        ulong seq = 0;
        var valueStart = -1;
        var valueEnd = -1;

        while (reader.Read())
        {
            if (reader.TokenType == JsonTokenType.EndObject) break;

            var name = reader.ValueSpan;
            if (!reader.Read()) return null;

            if (name.SequenceEqual("device"u8))
            {
                device = reader.GetString();

                // 设备过滤是调用方最常见的缩小手段；不匹配就没必要继续解析。
                if (query.Device is { } wanted &&
                    !string.Equals(device, wanted, StringComparison.Ordinal)) return null;
            }
            else if (name.SequenceEqual("point"u8))
            {
                point = reader.GetString();
                if (query.Point is { } wanted &&
                    !string.Equals(point, wanted, StringComparison.Ordinal)) return null;
            }
            else if (name.SequenceEqual("value"u8))
            {
                // 先记住位置、跳过内容：只有整行都命中过滤条件时才真正解析值。
                valueStart = (int)reader.TokenStartIndex;
                reader.Skip();
                valueEnd = (int)reader.BytesConsumed;
            }
            else if (name.SequenceEqual("quality"u8))
            {
                quality = reader.GetString();
            }
            else if (name.SequenceEqual("unit"u8))
            {
                unit = reader.TokenType == JsonTokenType.Null ? null : reader.GetString();
            }
            else if (name.SequenceEqual("deviceTime"u8))
            {
                deviceTime = reader.TokenType == JsonTokenType.Null ? null : ReadInstant(ref reader);
            }
            else if (name.SequenceEqual("collectedAt"u8))
            {
                collectedAt = ReadInstant(ref reader);
                if (collectedAt < query.From || collectedAt > query.To) return null;
            }
            else if (name.SequenceEqual("seq"u8))
            {
                seq = reader.GetUInt64();
            }
            else if (name.SequenceEqual("error"u8))
            {
                error = reader.TokenType == JsonTokenType.Null ? null : reader.GetString();
            }
            else
            {
                reader.Skip();
            }
        }

        if (device is null || point is null || valueStart < 0) return null;

        var value = ParseValue(utf8[valueStart..valueEnd]);

        return new SampleDto(
            device,
            point,
            value,
            quality ?? "good",
            unit,
            deviceTime,
            collectedAt,
            seq,
            error);
    }

    /// <summary>值对象只有 type/num/bool/text 四个有效字段，手写解析比 JsonDocument 少一次分配。</summary>
    private static DataValue ParseValue(ReadOnlySpan<byte> utf8)
    {
        var reader = new Utf8JsonReader(utf8, isFinalBlock: true, state: default);
        if (!reader.Read() || reader.TokenType != JsonTokenType.StartObject)
            return new DataValue { Type = DataType.UInt16 };

        var type = DataType.UInt16;
        double? num = null;
        bool? boolean = null;
        string? text = null;

        while (reader.Read())
        {
            if (reader.TokenType == JsonTokenType.EndObject) break;

            var name = reader.ValueSpan;
            if (!reader.Read()) break;

            if (name.SequenceEqual("type"u8))
                type = JsonEnum.ParseDataType(reader.GetString()!);
            else if (name.SequenceEqual("num"u8))
                num = reader.GetDouble();
            else if (name.SequenceEqual("bool"u8))
                boolean = reader.GetBoolean();
            else if (name.SequenceEqual("text"u8))
                text = reader.GetString();
            else
                reader.Skip();
        }

        return new DataValue { Type = type, Num = num, Bool = boolean, Text = text };
    }

    /// <summary>
    /// 时间字段绝大多数是本仓库写出的固定格式（UTC 毫秒），走无分配的快路径；
    /// 遇到旧数据或外部写入的其它 ISO-8601 写法再退回通用解析。
    /// </summary>
    private static DateTimeOffset ReadInstant(ref Utf8JsonReader reader)
    {
        if (reader.TokenType == JsonTokenType.String &&
            TryParseFastInstant(reader.ValueSpan, out var fast))
        {
            return fast;
        }

        var text = reader.GetString();
        if (!string.IsNullOrEmpty(text) &&
            DateTimeOffset.TryParse(
                text,
                CultureInfo.InvariantCulture,
                DateTimeStyles.AssumeUniversal | DateTimeStyles.AdjustToUniversal,
                out var parsed))
        {
            return parsed;
        }

        throw new FormatException("invalid timestamp in journal");
    }

    private static bool TryParseFastInstant(ReadOnlySpan<byte> text, out DateTimeOffset value)
    {
        value = default;

        // yyyy-MM-ddTHH:mm:ss.fffZ，共 24 个 ASCII 字符。
        if (text.Length != 24 ||
            text[4] != (byte)'-' || text[7] != (byte)'-' || text[10] != (byte)'T' ||
            text[13] != (byte)':' || text[16] != (byte)':' || text[19] != (byte)'.' ||
            text[23] != (byte)'Z')
        {
            return false;
        }

        if (!Digits(text, 0, 4, out var year) ||
            !Digits(text, 5, 2, out var month) ||
            !Digits(text, 8, 2, out var day) ||
            !Digits(text, 11, 2, out var hour) ||
            !Digits(text, 14, 2, out var minute) ||
            !Digits(text, 17, 2, out var second) ||
            !Digits(text, 20, 3, out var millisecond))
        {
            return false;
        }

        try
        {
            value = new DateTimeOffset(year, month, day, hour, minute, second, millisecond, TimeSpan.Zero);
            return true;
        }
        catch (ArgumentOutOfRangeException)
        {
            return false;
        }
    }

    private static bool Digits(ReadOnlySpan<byte> text, int start, int count, out int value)
    {
        value = 0;

        for (var i = 0; i < count; i++)
        {
            var digit = text[start + i] - (byte)'0';
            if (digit is < 0 or > 9) return false;
            value = value * 10 + digit;
        }

        return true;
    }

    /// <summary>同一时间桶内取「最后一条」：先比采集时间，再比点位序号。</summary>
    private static bool IsNewer(SampleDto candidate, SampleDto existing)
    {
        var byTime = candidate.CollectedAt.CompareTo(existing.CollectedAt);
        return byTime > 0 || (byTime == 0 && candidate.Seq > existing.Seq);
    }

    /// <summary>
    /// 固定排序键。历史页要能连续翻页，排序不能依赖文件里的偶然顺序；
    /// 同毫秒的样本再按设备/点位/序号排，保证跨请求稳定。
    /// </summary>
    private static int CompareSamples(SampleDto a, SampleDto b)
    {
        var byTime = a.CollectedAt.CompareTo(b.CollectedAt);
        if (byTime != 0) return byTime;

        var byDevice = string.CompareOrdinal(a.Device, b.Device);
        if (byDevice != 0) return byDevice;

        var byPoint = string.CompareOrdinal(a.Point, b.Point);
        return byPoint != 0 ? byPoint : a.Seq.CompareTo(b.Seq);
    }

    /// <summary>把查询串里的时间戳解析成 UTC；格式错误直接 400，避免静默当成默认窗口。</summary>
    public static DateTimeOffset ParseInstant(string text, string parameter)
    {
        if (!DateTimeOffset.TryParse(
                text,
                CultureInfo.InvariantCulture,
                DateTimeStyles.AssumeUniversal | DateTimeStyles.AdjustToUniversal,
                out var value))
        {
            throw ApiException.BadRequest(
                "request.invalid_parameter",
                $"'{parameter}' must be an ISO-8601 timestamp",
                new { parameter });
        }

        return value.ToUniversalTime();
    }

    /// <summary>
    /// 在字节流上按行切分，避免先把整段文件解码成字符串。
    /// 行内通常是短 JSON，跨缓冲区时复制到一条复用的行缓冲里。
    /// </summary>
    private sealed class ByteLineReader
    {
        private readonly Stream _stream;
        private readonly byte[] _buffer;
        private int _start;
        private int _end;
        private bool _eof;
        private byte[] _line = new byte[512];
        private int _lineLength;
        private bool _spansBuffers;

        public ByteLineReader(Stream stream, int bufferSize = 256 * 1024)
        {
            _stream = stream;
            _buffer = new byte[bufferSize];
        }

        /// <summary>当前行的原始字节；下一次调用 <see cref="NextAsync"/> 后失效。</summary>
        public ReadOnlyMemory<byte> Current { get; private set; }

        public async ValueTask<bool> NextAsync(CancellationToken ct)
        {
            _lineLength = 0;
            _spansBuffers = false;

            while (true)
            {
                if (_start < _end)
                {
                    var span = _buffer.AsSpan(_start, _end - _start);
                    var newline = span.IndexOf((byte)'\n');

                    if (newline >= 0)
                    {
                        var lineStart = _start;
                        var lineEnd = _start + newline;
                        _start = lineEnd + 1;

                        if (_spansBuffers)
                        {
                            Append(span[..newline]);
                            Current = _line.AsMemory(0, TrimCarriageReturn(_lineLength));
                            return true;
                        }

                        if (lineEnd > lineStart && _buffer[lineEnd - 1] == (byte)'\r') lineEnd--;
                        Current = _buffer.AsMemory(lineStart, lineEnd - lineStart);
                        return true;
                    }

                    Append(span);
                    _start = _end;
                }

                if (_eof)
                {
                    if (!_spansBuffers || _lineLength == 0) return false;

                    Current = _line.AsMemory(0, TrimCarriageReturn(_lineLength));
                    _lineLength = 0;
                    return true;
                }

                _start = 0;
                _end = await _stream.ReadAsync(_buffer.AsMemory(), ct).ConfigureAwait(false);
                if (_end == 0) _eof = true;
            }
        }

        private int TrimCarriageReturn(int length)
            => length > 0 && _line[length - 1] == (byte)'\r' ? length - 1 : length;

        private void Append(ReadOnlySpan<byte> segment)
        {
            if (!_spansBuffers)
            {
                _spansBuffers = true;
                _lineLength = 0;
            }

            var required = _lineLength + segment.Length;
            if (_line.Length < required)
            {
                var grown = new byte[Math.Max(required, _line.Length * 2)];
                _line.AsSpan(0, _lineLength).CopyTo(grown);
                _line = grown;
            }

            segment.CopyTo(_line.AsSpan(_lineLength));
            _lineLength = required;
        }
    }
}
