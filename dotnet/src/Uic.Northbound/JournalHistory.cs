using System.Globalization;
using System.Text;
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
/// 1. 原始字节按行扫描，先用固定形状的快路径只取「时间 + 身份 + 序号」，
///    只有最终会被返回的行才走完整解析——降采样时上百万行里通常只有几万行需要物化；
/// 2. 设备/点位不匹配时立刻放弃，常见过滤只读三个字段；
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

        // 设备/点位 id 在行情里重复上百万次，逐条转成字符串再算元组哈希，
        // 光这两步就能吃掉一半时间。先按字节比较去重成整数 id。
        var names = new NameTable();

        // 不开降采样时直接累积原始样本；开了之后每个 (设备, 点位, 时间桶) 只留最后一条。
        List<SampleDto>? raw = query.Interval is null ? new List<SampleDto>() : null;
        Dictionary<(int Device, int Point, long Bucket), Candidate>? buckets =
            query.Interval is null
                ? null
                : new Dictionary<(int, int, long), Candidate>();

        // 重放去重只在「原样返回每一行」时才需要单独一张表：
        // 降采样下重复行的 (设备, 点位, 时间, 序号) 完全相同，选胜出者时比不过已有值，
        // 天然不会重复输出。省掉这张 100 万条的表，扫描能快三分之一。
        HashSet<(int Device, int Point, ulong Seq)>? seen =
            buckets is null ? new HashSet<(int, int, ulong)>() : null;

        var deviceFilter = query.Device is null ? default : Encoding.UTF8.GetBytes(query.Device);
        var pointFilter = query.Point is null ? default : Encoding.UTF8.GetBytes(query.Point);
        var intervalTicks = query.Interval?.Ticks ?? 0;

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

                var line = lines.Current.Span;

                // 两段式：先用廉价的身份扫描做过滤、去重、选桶，只有最终要返回的
                // 样本才走完整解析。降采样时 100 万行里通常只有几万行需要物化。
                ScanOutcome outcome;
                FastRow row;
                try
                {
                    outcome = TryScanIdentity(line, deviceFilter, pointFilter, query, out row);
                }
                catch (JsonException)
                {
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

                // 遇到任何「不认识的形状」（转义、非固定格式时间、缺字段……）都退回
                // 完整解析：快路径只负责把常见的行读快，不负责解释罕见写法。
                if (outcome == ScanOutcome.Fallback)
                {
                    SampleDto? fallback;
                    try
                    {
                        fallback = ParseSample(line, query);
                    }
                    catch (JsonException)
                    {
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

                    if (fallback is null) continue;

                    var deviceId = names.Intern(fallback.Device);
                    var pointId = names.Intern(fallback.Point);
                    if (seen is not null && !seen.Add((deviceId, pointId, fallback.Seq))) continue;

                    if (buckets is null)
                    {
                        raw!.Add(fallback);
                        continue;
                    }

                    var fbTicks = fallback.CollectedAt.UtcTicks;
                    var fbBucket = fbTicks / intervalTicks;
                    var fbKey = (deviceId, pointId, fbBucket);

                    // 慢路径也统一存成「偏移 + 排序键」，与快路径共用同一套
                    // 选桶与输出逻辑，避免两处语义漂移。
                    if (!buckets.TryGetValue(fbKey, out var fbExisting) ||
                        IsNewer(fbTicks, fallback.Seq, fbExisting))
                    {
                        buckets[fbKey] = new Candidate(lines.CurrentOffset, lines.CurrentLength, fbTicks, fallback.Seq);
                    }

                    continue;
                }

                if (outcome == ScanOutcome.Excluded) continue;

                var devId = names.Intern(row.Device(line));
                var ptId = names.Intern(row.Point(line));

                // 至少一次投递：重放会写出完全重复的行。原样返回时按身份去重；
                // 降采样时重复行自然选不上（见上面 seen 的说明）。
                if (seen is not null && !seen.Add((devId, ptId, row.Seq))) continue;

                if (buckets is null)
                {
                    var sample = ParseSample(line, query);
                    if (sample is not null) raw!.Add(sample);
                    continue;
                }

                var bucket = row.Ticks / intervalTicks;
                var key = (devId, ptId, bucket);

                // 只记下胜出行的文件偏移，等扫描结束再解析。
                //
                // journal 是按时间顺序追加的，同一桶里几乎每来一行都会「夺冠」。
                // 若在循环里解析，1 百万行就要解析 1 百万次；若在循环里拷贝字节，
                // 也要拷贝 1 百万次。记偏移只是存一个整数，最后按偏移顺序
                // 一次性回读并解析「桶数」条（降采样后通常几万）。
                if (!buckets.TryGetValue(key, out var last) ||
                    IsNewer(row.Ticks, row.Seq, last))
                {
                    buckets[key] = new Candidate(lines.CurrentOffset, lines.CurrentLength, row.Ticks, row.Seq);
                }
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

        var items = buckets is null ? raw! : await Materialize(buckets.Values, query, ct).ConfigureAwait(false);

        items.Sort(CompareSamples);

        return new HistoryResult(items, items.Count);
    }

    /// <summary>
    /// 降采样时「桶里的胜出行」：只留文件偏移与排序键，扫描结束再回读并解析。
    ///
    /// 排序键用 UTC ticks 而不是 <see cref="DateTimeOffset"/>：扫描阶段每条都要比时间，
    /// 结构体越薄越快，且 ticks 本来就是分桶要用的量。
    /// </summary>
    private readonly record struct Candidate(long Offset, int Length, long Ticks, ulong Seq);

    /// <summary>
    /// 把胜出行的原始行读回来再完整解析。
    ///
    /// 按偏移升序回读：journal 里胜出行本来就散布在全文件，但排序后偏移单调，
    /// 文件流只需向前跳，不必来回 seek，磁盘顺序读的特性还能吃到。
    /// </summary>
    private async Task<List<SampleDto>> Materialize(
        IEnumerable<Candidate> candidates,
        HistoryQuery query,
        CancellationToken ct)
    {
        var winners = new List<Candidate>(candidates);
        winners.Sort(static (a, b) => a.Offset.CompareTo(b.Offset));

        var items = new List<SampleDto>(winners.Count);
        if (winners.Count == 0) return items;

        var longest = 0;
        foreach (var winner in winners)
        {
            if (winner.Length > longest) longest = winner.Length;
        }

        var buffer = new byte[longest];

        await using var stream = new FileStream(
            _path,
            FileMode.Open,
            FileAccess.Read,
            FileShare.ReadWrite | FileShare.Delete,
            bufferSize: 64 * 1024,
            FileOptions.Asynchronous | FileOptions.SequentialScan);

        foreach (var winner in winners)
        {
            ct.ThrowIfCancellationRequested();

            stream.Position = winner.Offset;

            var offset = 0;
            while (offset < winner.Length)
            {
                var read = await stream.ReadAsync(buffer.AsMemory(offset, winner.Length - offset), ct)
                    .ConfigureAwait(false);
                if (read == 0) break;
                offset += read;
            }

            // 扫描时这一行是好行，回读时却可能已被并发的 journal 清理截断；
            // 与主扫描一致：坏行跳过，不让一行毁掉整次查询。
            try
            {
                var sample = ParseSample(buffer.AsSpan(0, offset), query);
                if (sample is not null) items.Add(sample);
            }
            catch (JsonException)
            {
            }
            catch (InvalidOperationException)
            {
            }
            catch (FormatException)
            {
            }
        }

        return items;
    }

    private enum ScanOutcome
    {
        /// <summary>行不满足时间/身份过滤，或字段值不是快路径认识的字面量，交给完整解析判断。</summary>
        Fallback,

        /// <summary>行被明确排除（时间越界、身份不匹配）。</summary>
        Excluded,

        /// <summary>身份字段已取到，可以直接进入去重与选桶。</summary>
        Scanned,
    }

    private readonly struct FastRow
    {
        public FastRow(int deviceStart, int deviceLength, int pointStart, int pointLength, long ticks, ulong seq)
        {
            DeviceStart = deviceStart;
            DeviceLength = deviceLength;
            PointStart = pointStart;
            PointLength = pointLength;
            Ticks = ticks;
            Seq = seq;
        }

        public int DeviceStart { get; }
        public int DeviceLength { get; }
        public int PointStart { get; }
        public int PointLength { get; }
        public long Ticks { get; }
        public ulong Seq { get; }

        public ReadOnlySpan<byte> Device(ReadOnlySpan<byte> line) => line.Slice(DeviceStart, DeviceLength);
        public ReadOnlySpan<byte> Point(ReadOnlySpan<byte> line) => line.Slice(PointStart, PointLength);
    }

    /// <summary>
    /// 快路径：只认本仓库写出的固定字段形状，取 <c>device</c>、<c>point</c>、
    /// <c>collectedAt</c>、<c>seq</c> 四个字段。
    ///
    /// 为什么不直接用 <see cref="Utf8JsonReader"/>：读者每读一个 token 都要做状态机
    /// 校验，100 万行 × 十几 token 的代价实测是本方法的 4 倍以上。这里只在
    /// 「字段名 + 引号 + 无转义」的确定形状上做字节查找，一旦形状不熟就返回
    /// <see cref="ScanOutcome.Fallback"/>，正确性由完整解析兜底——快路径只许加速，
    /// 不许改变语义。
    /// </summary>
    private static ScanOutcome TryScanIdentity(
        ReadOnlySpan<byte> line,
        ReadOnlySpan<byte> deviceFilter,
        ReadOnlySpan<byte> pointFilter,
        HistoryQuery query,
        out FastRow row)
    {
        row = default;

        var fromTicks = query.From.UtcTicks;
        var toTicks = query.To.UtcTicks;

        // 时间先判：绝大多数查询只关心一小段窗口，越界的行越早放弃越省。
        // 完整解析要求 device/point/value 齐全；缺 value 的行会让候选空占一个桶。
        // 只多一次前缀查找，换「快慢路径对同一行结论一致」。
        if (line.IndexOf("\"value\":"u8) < 0) return ScanOutcome.Fallback;

        if (!TryField(line, "\"collectedAt\":\""u8, out var atStart, out var atLength)) return ScanOutcome.Fallback;
        if (!TryParseFastTicks(line.Slice(atStart, atLength), out var ticks)) return ScanOutcome.Fallback;
        if (ticks < fromTicks || ticks > toTicks) return ScanOutcome.Excluded;

        if (!TryField(line, "\"device\":\""u8, out var devStart, out var devLength)) return ScanOutcome.Fallback;
        var device = line.Slice(devStart, devLength);
        if (!deviceFilter.IsEmpty && !device.SequenceEqual(deviceFilter)) return ScanOutcome.Excluded;

        if (!TryField(line, "\"point\":\""u8, out var ptStart, out var ptLength)) return ScanOutcome.Fallback;
        var point = line.Slice(ptStart, ptLength);
        if (!pointFilter.IsEmpty && !point.SequenceEqual(pointFilter)) return ScanOutcome.Excluded;

        if (!TryScalar(line, "\"seq\":"u8, out var seqStart, out var seqLength)) return ScanOutcome.Fallback;
        if (!TryParseUInt64(line.Slice(seqStart, seqLength), out var seq)) return ScanOutcome.Fallback;

        row = new FastRow(devStart, devLength, ptStart, ptLength, ticks, seq);
        return ScanOutcome.Scanned;
    }

    /// <summary>取 <c>"name":"value"</c> 里 value 的偏移；值里出现转义就不认，交回完整解析。</summary>
    private static bool TryField(ReadOnlySpan<byte> line, ReadOnlySpan<byte> prefix, out int start, out int length)
    {
        start = 0;
        length = 0;

        var at = line.IndexOf(prefix);
        if (at < 0) return false;

        start = at + prefix.Length;
        var rest = line[start..];
        var end = rest.IndexOf((byte)'"');
        if (end < 0) return false;

        // 转义序列（\u00B0C 这类）与原文长度不一致，不能按字节直接和过滤器比较。
        if (rest[..end].IndexOf((byte)'\\') >= 0) return false;

        length = end;
        return true;
    }

    /// <summary>取 <c>"name":123</c> 里数字的偏移。</summary>
    private static bool TryScalar(ReadOnlySpan<byte> line, ReadOnlySpan<byte> prefix, out int start, out int length)
    {
        start = 0;
        length = 0;

        var at = line.IndexOf(prefix);
        if (at < 0) return false;

        start = at + prefix.Length;
        var end = start;
        while (end < line.Length && line[end] is >= (byte)'0' and <= (byte)'9') end++;
        if (end == start) return false;

        length = end - start;
        return true;
    }

    private static bool TryParseUInt64(ReadOnlySpan<byte> text, out ulong value)
    {
        value = 0;

        foreach (var b in text)
        {
            if (b is < (byte)'0' or > (byte)'9') return false;
            if (value > ulong.MaxValue / 10) return false;
            value = value * 10 + (ulong)(b - (byte)'0');
        }

        return true;
    }

    /// <summary>
    /// 设备/点位名 → 小整数 id。
    ///
    /// 行情里同一个名字会重复上百万次：逐条解码成 string 再算元组哈希，
    /// 分配与哈希成本比字节扫描本身还高。这里按字节比较、命中即复用，
    /// 名字只保存一份副本，且不做解码——输出用的字符串由完整解析提供。
    /// </summary>
    private sealed class NameTable
    {
        private readonly Dictionary<ulong, List<Entry>> _byHash = new();
        private int _next;

        public int Intern(ReadOnlySpan<byte> name)
        {
            var hash = Fnv1a(name);

            if (!_byHash.TryGetValue(hash, out var entries))
            {
                entries = new List<Entry>(1);
                _byHash[hash] = entries;
            }

            foreach (var entry in entries)
            {
                if (name.SequenceEqual(entry.Bytes)) return entry.Id;
            }

            var added = new Entry(name.ToArray(), _next++);
            entries.Add(added);
            return added.Id;
        }

        public int Intern(string name) => Intern(Encoding.UTF8.GetBytes(name));

        private readonly record struct Entry(byte[] Bytes, int Id);

        private static ulong Fnv1a(ReadOnlySpan<byte> data)
        {
            var hash = 14695981039346656037UL;
            foreach (var b in data)
            {
                hash ^= b;
                hash *= 1099511628211UL;
            }

            return hash;
        }
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

    /// <summary>值对象只有 type/num/bool/text/array 五个有效字段，手写解析比 JsonDocument 少一次分配。</summary>
    private static DataValue ParseValue(ReadOnlySpan<byte> utf8)
    {
        var reader = new Utf8JsonReader(utf8, isFinalBlock: true, state: default);
        if (!reader.Read() || reader.TokenType != JsonTokenType.StartObject)
            return new DataValue { Type = DataType.UInt16 };

        var type = DataType.UInt16;
        double? num = null;
        bool? boolean = null;
        string? text = null;
        List<double>? array = null;

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
            else if (name.SequenceEqual("array"u8) && reader.TokenType == JsonTokenType.StartArray)
            {
                // 历史里的数组值必须能读回来：否则「查得到但值是空的」，
                // 比查不到更难排查。
                array = new List<double>();
                while (reader.Read() && reader.TokenType != JsonTokenType.EndArray)
                {
                    if (reader.TokenType == JsonTokenType.Number) array.Add(reader.GetDouble());
                    else reader.Skip();
                }
            }
            else
                reader.Skip();
        }

        return new DataValue { Type = type, Num = num, Bool = boolean, Text = text, Array = array };
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

    /// <summary>
    /// 快路径的时间解析：只输出 UTC ticks，不在扫描阶段构造 <see cref="DateTimeOffset"/>。
    ///
    /// <c>DateTimeOffset</c> 的构造要做一整套日历合法性校验（闰年、月末、各月天数），
    /// 上百万次调用能吃掉扫描时间的一大半。这里用「自 1970-01-01 起的天数」的
    /// 标准算法直接算 ticks，只保留必要的范围与日历检查；任何不认识的写法都返回
    /// false，让调用方退回完整解析，那里才是权威解释者。
    /// </summary>
    private static bool TryParseFastTicks(ReadOnlySpan<byte> text, out long ticks)
    {
        ticks = 0;

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

        // 2 月 30 日、13 月之类必须在这里就判掉：否则快路径会算出一个
        // 「完整解析会拒绝」的时间，两条路径对同一行给出不同结论。
        if (year < 1 || !IsValidDate(year, month, day) ||
            hour > 23 || minute > 59 || second > 59)
        {
            return false;
        }

        var days = DaysSinceUnixEpoch(year, month, day);
        ticks = (days + DaysFromYear1ToUnixEpoch) * TimeSpan.TicksPerDay
                + hour * TimeSpan.TicksPerHour
                + minute * TimeSpan.TicksPerMinute
                + second * TimeSpan.TicksPerSecond
                + millisecond * TimeSpan.TicksPerMillisecond;

        return true;
    }

    private static bool IsValidDate(int year, int month, int day)
    {
        if (month is < 1 or > 12 || day < 1) return false;

        var max = month switch
        {
            2 => (year % 4 == 0 && (year % 100 != 0 || year % 400 == 0)) ? 29 : 28,
            4 or 6 or 9 or 11 => 30,
            _ => 31,
        };

        return day <= max;
    }

    /// <summary>0001-01-01 到 1970-01-01 的天数；加上它就能得到 <see cref="DateTime"/> 的 ticks 原点。</summary>
    private const long DaysFromYear1ToUnixEpoch = 719162;

    /// <summary>公历日期 → 自 1970-01-01 起的天数（Howard Hinnant 的 days_from_civil）。</summary>
    private static long DaysSinceUnixEpoch(int year, int month, int day)
    {
        var y = month <= 2 ? year - 1 : year;
        var era = (y >= 0 ? y : y - 399) / 400;
        var yoe = y - era * 400;                                  // [0, 399]
        var mp = (month + 9) % 12;                                 // 3 月起算
        var doy = (153 * mp + 2) / 5 + day - 1;                     // [0, 365]
        var doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;            // [0, 146096]

        return era * 146097L + doe - 719468L;
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
    private static bool IsNewer(long candidateTicks, ulong candidateSeq, Candidate existing)
    {
        var byTime = candidateTicks.CompareTo(existing.Ticks);
        return byTime > 0 || (byTime == 0 && candidateSeq > existing.Seq);
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

        // 绝对偏移只在「回读胜出行」时用得上，但必须在这里维护：
        // 只有切行的人知道每行从文件的哪个字节开始。
        private long _bufferBase;
        private long _lineOffset;

        public ByteLineReader(Stream stream, int bufferSize = 256 * 1024)
        {
            _stream = stream;
            _buffer = new byte[bufferSize];
        }

        /// <summary>当前行的原始字节；下一次调用 <see cref="NextAsync"/> 后失效。</summary>
        public ReadOnlyMemory<byte> Current { get; private set; }

        /// <summary>当前行在文件中的起始偏移。</summary>
        public long CurrentOffset { get; private set; }

        /// <summary>当前行的字节数（已去掉行尾的 CR）。</summary>
        public int CurrentLength => Current.Length;

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
                            CurrentOffset = _lineOffset;
                            return true;
                        }

                        if (lineEnd > lineStart && _buffer[lineEnd - 1] == (byte)'\r') lineEnd--;
                        Current = _buffer.AsMemory(lineStart, lineEnd - lineStart);
                        CurrentOffset = _bufferBase + lineStart;
                        return true;
                    }

                    Append(span);
                    _start = _end;
                }

                if (_eof)
                {
                    if (!_spansBuffers || _lineLength == 0) return false;

                    Current = _line.AsMemory(0, TrimCarriageReturn(_lineLength));
                    CurrentOffset = _lineOffset;
                    _lineLength = 0;
                    return true;
                }

                _start = 0;
                _bufferBase += _end;
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
                _lineOffset = _bufferBase + _start;
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
