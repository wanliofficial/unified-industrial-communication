using System.Globalization;
using System.Text;
using Uic.Domain;

namespace Uic.Config;

/// <summary>CSV 导入的解析结果。问题按行收集，避免「改完第十行才知道第一行有错」。</summary>
public sealed record PointCsvResult(
    IReadOnlyList<Point> Points,
    IReadOnlyList<string> Added,
    IReadOnlyList<string> Updated,
    IReadOnlyList<ValidationProblem> Problems)
{
    public bool Ok => Problems.Count == 0;
}

/// <summary>
/// 点表 CSV 导入。
///
/// 现场点表通常来自 Excel：可能带 BOM、逗号分隔、字段里有引号、行尾是 CRLF，
/// 也可能把地址写成 <c>40001</c> 这种 Modbus 参考号。这里刻意支持这几种写法，
/// 而**不支持**公式或脚本——导入器只做数据搬运，不做计算。
///
/// 列名大小写不敏感，接受常见别名：
/// <c>id, name, address, area, offset, bit, length, dataType, byteOrder, unit,
/// scale, bias, deadband, deadbandMode, rangeMin, rangeMax, writable, enabled, byteLength</c>。
/// </summary>
public static class PointCsv
{
    private static readonly string[] Required = { "id" };

    public static PointCsvResult Import(
        string csv,
        IReadOnlyList<Point> existing,
        string deviceId)
    {
        var problems = new List<ValidationProblem>();
        var rows = ParseRows(csv ?? string.Empty, problems);

        if (rows.Count == 0)
        {
            if (problems.Count == 0)
                problems.Add(new ValidationProblem("csv", "no data rows"));
            return new PointCsvResult(Array.Empty<Point>(), Array.Empty<string>(), Array.Empty<string>(), problems);
        }

        var header = rows[0].Fields
            .Select((name, index) => (Name: Normalize(name), Index: index))
            .GroupBy(x => x.Name)
            .ToDictionary(g => g.Key, g => g.First().Index, StringComparer.Ordinal);

        foreach (var required in Required)
        {
            if (!header.ContainsKey(required))
                problems.Add(new ValidationProblem("csv.header", $"missing required column '{required}'"));
        }

        if (!header.ContainsKey("address") &&
            !(header.ContainsKey("area") && header.ContainsKey("offset")))
        {
            problems.Add(new ValidationProblem("csv.header",
                "either 'address' or both 'area' and 'offset' are required"));
        }

        if (problems.Count > 0)
            return new PointCsvResult(Array.Empty<Point>(), Array.Empty<string>(), Array.Empty<string>(), problems);

        var existingById = existing.ToDictionary(p => p.Id, StringComparer.Ordinal);
        var seen = new HashSet<string>(StringComparer.Ordinal);
        var points = new List<Point>();
        var added = new List<string>();
        var updated = new List<string>();

        for (var i = 1; i < rows.Count; i++)
        {
            var row = rows[i];
            var line = row.Line;

            // 表头键已归一化成小写并去掉下划线/空格，这里也必须归一化再查，
            // 否则 camelCase 列名（如 deadbandMode）永远匹配不上。
            string? Field(string name)
                => header.TryGetValue(Normalize(name), out var index) && index < row.Fields.Count
                    ? row.Fields[index].Trim()
                    : null;

            var id = Field("id");
            if (string.IsNullOrWhiteSpace(id))
            {
                problems.Add(new ValidationProblem($"line {line}.id", "required"));
                continue;
            }

            if (!ModelValidator.IsValidItemId(id))
            {
                problems.Add(new ValidationProblem($"line {line}.id",
                    $"'{id}' must match ^[a-z0-9][a-z0-9._-]{{0,63}}$"));
                continue;
            }

            if (!seen.Add(id))
            {
                problems.Add(new ValidationProblem($"line {line}.id",
                    $"duplicate point id '{id}' in this file"));
                continue;
            }

            var addressProblems = new List<ValidationProblem>();
            var address = ParseAddress(Field("address"), Field("area"), Field("offset"),
                Field("bit"), out var addressError);

            if (address is null)
            {
                problems.Add(new ValidationProblem($"line {line}.address", addressError ?? "invalid address"));
                continue;
            }

            address.Validate(addressProblems, $"line {line}.address");
            if (addressProblems.Count > 0)
            {
                problems.AddRange(addressProblems);
                continue;
            }

            var dataTypeText = Field("dataType") ?? Field("datatype");
            var dataType = DataType.UInt16;

            if (!string.IsNullOrWhiteSpace(dataTypeText) &&
                !Enum.TryParse(dataTypeText, ignoreCase: true, out dataType))
            {
                problems.Add(new ValidationProblem($"line {line}.dataType",
                    $"unknown data type '{dataTypeText}'"));
                continue;
            }

            ByteOrder? byteOrder = null;
            var byteOrderText = Field("byteOrder") ?? Field("byteorder");
            if (!string.IsNullOrWhiteSpace(byteOrderText))
            {
                if (!Enum.TryParse<ByteOrder>(byteOrderText, ignoreCase: true, out var parsedOrder))
                {
                    problems.Add(new ValidationProblem($"line {line}.byteOrder",
                        $"unknown byte order '{byteOrderText}' (ABCD/CDAB/BADC/DCBA)"));
                    continue;
                }

                byteOrder = parsedOrder;
            }

            var scale = ParseDouble(Field("scale"), 1, line, "scale", problems);
            var bias = ParseDouble(Field("bias"), 0, line, "bias", problems);
            var deadband = ParseNullableDouble(Field("deadband"), line, "deadband", problems);
            var registerLength = ParseInt(Field("length") ?? Field("registerLength"), 0, line, "length", problems);
            var byteLength = ParseInt(Field("byteLength") ?? Field("bytes"), 0, line, "byteLength", problems);
            var writable = ParseBool(Field("writable"), true, line, "writable", problems);
            var enabled = ParseBool(Field("enabled"), true, line, "enabled", problems);

            if (scale == 0)
            {
                problems.Add(new ValidationProblem($"line {line}.scale", "must be non-zero"));
                continue;
            }

            var deadbandMode = DeadbandMode.Absolute;
            var modeText = Field("deadbandMode");
            if (!string.IsNullOrWhiteSpace(modeText))
            {
                if (!JsonEnum.TryParseDeadbandMode(modeText, out deadbandMode))
                {
                    problems.Add(new ValidationProblem($"line {line}.deadbandMode",
                        $"unknown deadband mode '{modeText}' (absolute/percent/rate)"));
                    continue;
                }
            }

            var rangeMin = ParseNullableDouble(Field("rangeMin"), line, "rangeMin", problems);
            var rangeMax = ParseNullableDouble(Field("rangeMax"), line, "rangeMax", problems);

            var point = new Point
            {
                Id = id,
                Name = Field("name"),
                Address = address with { Length = registerLength },
                DataType = dataType,
                ByteOrder = byteOrder,
                Scale = scale,
                Offset = bias,
                Deadband = deadband,
                DeadbandMode = deadbandMode,
                RangeMin = rangeMin,
                RangeMax = rangeMax,
                Unit = Field("unit"),
                Writable = writable,
                Enabled = enabled,
                Length = byteLength,
            };

            // 与启动时的校验共用一套规则：能导入的配置必须能启动。
            var pointProblems = new List<ValidationProblem>();
            ModelValidator.ValidatePoint(point, $"line {line}", pointProblems);
            if (pointProblems.Count > 0)
            {
                problems.AddRange(pointProblems);
                continue;
            }

            points.Add(point);

            if (existingById.ContainsKey(id)) updated.Add(id);
            else added.Add(id);
        }

        if (problems.Count > 0)
            return new PointCsvResult(Array.Empty<Point>(), Array.Empty<string>(), Array.Empty<string>(), problems);

        return new PointCsvResult(points, added, updated, problems);
    }

    /// <summary>把导入结果合并进现有设备：同 id 覆盖，其余保留（合并而不是整表替换）。</summary>
    public static List<Point> Merge(IReadOnlyList<Point> existing, IReadOnlyList<Point> imported)
    {
        var byId = imported.ToDictionary(p => p.Id, StringComparer.Ordinal);
        var merged = new List<Point>(existing.Count + imported.Count);

        foreach (var point in existing)
            merged.Add(byId.TryGetValue(point.Id, out var replacement) ? replacement : point);

        var existingIds = new HashSet<string>(existing.Select(p => p.Id), StringComparer.Ordinal);
        foreach (var point in imported)
        {
            if (!existingIds.Contains(point.Id)) merged.Add(point);
        }

        return merged;
    }

    // ------------------------------------------------------------------
    // 地址表达式
    // ------------------------------------------------------------------

    /// <summary>
    /// 支持三种写法：
    /// 1. <c>area:offset[.bit]</c>（holding:100、coil:5.2）；
    /// 2. Modbus 参考号（40001、30001.3、00001、10001）；
    /// 3. 分列填写 area + offset + bit。
    /// </summary>
    public static Address? ParseAddress(
        string? expression,
        string? area,
        string? offset,
        string? bit,
        out string? error)
    {
        error = null;

        if (!string.IsNullOrWhiteSpace(expression))
        {
            var text = expression.Trim();

            if (TryParseReference(text, out var reference))
                return reference;

            var parts = text.Split(':', 2);
            if (parts.Length == 2)
            {
                area = parts[0];
                offset = parts[1];
            }
            else
            {
                error = $"cannot parse '{expression}' (expected area:offset or a Modbus reference like 40001)";
                return null;
            }
        }

        if (string.IsNullOrWhiteSpace(area) || string.IsNullOrWhiteSpace(offset))
        {
            error = "address requires an area and an offset";
            return null;
        }

        var normalizedArea = NormalizeArea(area);
        if (normalizedArea is null)
        {
            error = $"unknown area '{area}' (holding|input|coil|discrete)";
            return null;
        }

        var offsetText = offset.Trim();
        var bitIndex = -1;

        // area:100.3 —— 小数点后是位号。
        var dot = offsetText.IndexOf('.');
        if (dot > 0)
        {
            var bitText = offsetText[(dot + 1)..];
            offsetText = offsetText[..dot];

            if (!int.TryParse(bitText, NumberStyles.Integer, CultureInfo.InvariantCulture, out bitIndex) ||
                bitIndex is < 0 or > 15)
            {
                error = $"bit '{bitText}' must be 0..15";
                return null;
            }
        }

        if (!int.TryParse(offsetText, NumberStyles.Integer, CultureInfo.InvariantCulture, out var offsetValue) ||
            offsetValue < 0)
        {
            error = $"offset '{offsetText}' must be a non-negative integer";
            return null;
        }

        if (!string.IsNullOrWhiteSpace(bit))
        {
            if (!int.TryParse(bit, NumberStyles.Integer, CultureInfo.InvariantCulture, out bitIndex) ||
                bitIndex is < 0 or > 15)
            {
                error = $"bit '{bit}' must be 0..15";
                return null;
            }
        }

        return Address.Register(normalizedArea, offsetValue, length: 0, bit: bitIndex);
    }

    private static bool TryParseReference(string text, out Address? address)
    {
        address = null;

        var bit = -1;
        var dot = text.IndexOf('.');
        var digits = text;

        if (dot > 0)
        {
            var bitText = text[(dot + 1)..];
            digits = text[..dot];

            if (!int.TryParse(bitText, NumberStyles.Integer, CultureInfo.InvariantCulture, out bit) ||
                bit is < 0 or > 15)
            {
                return false;
            }
        }

        if (digits.Length is < 5 or > 6) return false;
        if (!digits.All(char.IsAsciiDigit)) return false;
        if (!int.TryParse(digits, NumberStyles.Integer, CultureInfo.InvariantCulture, out var reference))
            return false;

        var prefix = digits[0];
        var zeroBased = reference % 10000 - 1;
        if (zeroBased < 0) return false;

        var area = prefix switch
        {
            '0' => "coil",
            '1' => "discrete",
            '3' => "input",
            '4' => "holding",
            _ => null,
        };

        if (area is null) return false;

        address = Address.Register(area, zeroBased, length: 0, bit: bit);
        return true;
    }

    private static string? NormalizeArea(string area) => area.Trim().ToLowerInvariant() switch
    {
        "holding" or "hr" or "holdingregister" or "4x" => "holding",
        "input" or "ir" or "inputregister" or "3x" => "input",
        "coil" or "co" or "0x" => "coil",
        "discrete" or "di" or "discreteinput" or "1x" => "discrete",
        _ => null,
    };

    // ------------------------------------------------------------------
    // CSV 词法
    // ------------------------------------------------------------------

    private sealed record Row(int Line, IReadOnlyList<string> Fields);

    private static List<Row> ParseRows(string csv, List<ValidationProblem> problems)
    {
        var rows = new List<Row>();

        if (csv.Length > 0 && csv[0] == '\uFEFF') csv = csv[1..];

        var fields = new List<string>();
        var field = new StringBuilder();
        var inQuotes = false;
        var line = 1;
        var rowStartLine = 1;
        var hasContent = false;

        void EndField()
        {
            fields.Add(field.ToString());
            field.Clear();
        }

        void EndRow()
        {
            EndField();

            // 整行都是空字段视为空行（Excel 常在末尾留下 ,,,）。
            if (hasContent || fields.Any(f => !string.IsNullOrWhiteSpace(f)))
                rows.Add(new Row(rowStartLine, fields.ToArray()));

            fields.Clear();
            hasContent = false;
            // 当前字符是行尾分隔符，下一行从它的后面开始。
            rowStartLine = line + 1;
        }

        for (var i = 0; i < csv.Length; i++)
        {
            var c = csv[i];

            if (inQuotes)
            {
                if (c == '"')
                {
                    if (i + 1 < csv.Length && csv[i + 1] == '"')
                    {
                        field.Append('"');
                        i++;
                    }
                    else
                    {
                        inQuotes = false;
                    }
                }
                else
                {
                    if (c == '\n') line++;
                    field.Append(c);
                    hasContent = true;
                }

                continue;
            }

            switch (c)
            {
                case '"':
                    inQuotes = true;
                    hasContent = true;
                    break;

                case ',':
                    EndField();
                    hasContent = true;
                    break;

                case '\r':
                    // 交给 \n 处理；单独出现的 \r（老 Mac）也当行尾。
                    if (i + 1 >= csv.Length || csv[i + 1] != '\n')
                    {
                        EndRow();
                        line++;
                    }
                    break;

                case '\n':
                    EndRow();
                    line++;
                    break;

                default:
                    field.Append(c);
                    if (!char.IsWhiteSpace(c)) hasContent = true;
                    break;
            }
        }

        if (inQuotes)
            problems.Add(new ValidationProblem($"line {line}", "unterminated quoted field"));

        if (field.Length > 0 || fields.Count > 0 || hasContent)
            EndRow();

        return rows;
    }

    private static string Normalize(string name)
        => name.Trim().ToLowerInvariant().Replace("_", string.Empty).Replace(" ", string.Empty);

    private static double ParseDouble(string? text, double fallback, int line, string column,
        List<ValidationProblem> problems)
    {
        if (string.IsNullOrWhiteSpace(text)) return fallback;

        if (double.TryParse(text, NumberStyles.Float, CultureInfo.InvariantCulture, out var value))
            return value;

        problems.Add(new ValidationProblem($"line {line}.{column}", $"'{text}' is not a number"));
        return fallback;
    }

    private static double? ParseNullableDouble(string? text, int line, string column,
        List<ValidationProblem> problems)
    {
        if (string.IsNullOrWhiteSpace(text)) return null;
        return ParseDouble(text, 0, line, column, problems);
    }

    private static int ParseInt(string? text, int fallback, int line, string column,
        List<ValidationProblem> problems)
    {
        if (string.IsNullOrWhiteSpace(text)) return fallback;

        if (int.TryParse(text, NumberStyles.Integer, CultureInfo.InvariantCulture, out var value))
            return value;

        problems.Add(new ValidationProblem($"line {line}.{column}", $"'{text}' is not an integer"));
        return fallback;
    }

    private static bool ParseBool(string? text, bool fallback, int line, string column,
        List<ValidationProblem> problems)
    {
        if (string.IsNullOrWhiteSpace(text)) return fallback;

        switch (text.Trim().ToLowerInvariant())
        {
            case "1" or "true" or "yes" or "y" or "on": return true;
            case "0" or "false" or "no" or "n" or "off": return false;
        }

        problems.Add(new ValidationProblem($"line {line}.{column}", $"'{text}' is not a boolean"));
        return fallback;
    }
}
