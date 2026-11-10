namespace Uic.Codec.Mqtt;

/// <summary>
/// MQTT 主题过滤器校验与匹配。放在协议层：发布方与订阅方两侧必须用同一套规则，
/// 各写一份迟早出现「<c>+</c> 能匹配一级、<c>#</c> 能匹配零级」之类的分歧。
///
/// 与 MQTT 3.1.1 规范一致：
/// <list type="bullet">
/// <item><c>+</c> 只匹配一个层级（<c>plant/+/temp</c> 命中 <c>plant/line1/temp</c>，不命中 <c>plant/a/temp</c>）；</item>
/// <item><c>#</c> 匹配其后任意层级（含零级），必须是过滤器最后一级（<c>plant/#</c> 命中 <c>plant</c> 与 <c>plant/a/b</c>）；</item>
/// <item>主题与过滤器都不允许出现空层级（<c>a//b</c>）。</item>
/// </list>
///
/// 订阅端必须自己做匹配：Broker 虽然按过滤器投递，但一条消息可能同时命中多个点位
/// （例如 <c>plant/#</c> 与 <c>plant/line1/temp</c>），客户端得知道「这条属于哪个点位」。
/// </summary>
public static class MqttTopic
{
    /// <summary>
    /// 校验主题过滤器。合法返回 <c>true</c>；非法时把原因写进 <paramref name="error"/>。
    /// 纯函数，用例直接覆盖各种写法，不需要 Broker。
    /// </summary>
    public static bool TryValidate(string? filter, out string? error)
    {
        error = null;

        if (string.IsNullOrWhiteSpace(filter))
        {
            error = "topic filter is required: write the topic to subscribe, e.g. plant/line1/temp";
            return false;
        }

        var levels = filter.Split('/');
        for (var i = 0; i < levels.Length; i++)
        {
            var level = levels[i];
            if (level.Length == 0)
            {
                error = $"topic filter '{filter}' has an empty level (a double slash is not allowed)";
                return false;
            }

            if (level.Contains('#'))
            {
                // # 必须独占最后一级：sport/#/x 是非法的。
                if (level != "#" || i != levels.Length - 1)
                {
                    error = $"topic filter '{filter}' has an invalid '#'; it must be the whole last level";
                    return false;
                }
            }
            else if (level.Contains('+'))
            {
                if (level != "+")
                {
                    error = $"topic filter '{filter}' has an invalid '+'; it must be a whole level, e.g. plant/+/temp";
                    return false;
                }
            }
        }

        return true;
    }

    /// <summary>
    /// 具体主题是否命中过滤器。两端都不含空层级（发布主题由 Broker 保证，订阅过滤器由
    /// <see cref="TryValidate"/> 保证），这里只做层级比对。
    /// </summary>
    public static bool Matches(string filter, string topic)
    {
        if (string.IsNullOrEmpty(filter) || string.IsNullOrEmpty(topic)) return false;

        var filters = filter.Split('/');
        var topics = topic.Split('/');

        for (var i = 0; i < filters.Length; i++)
        {
            if (filters[i] == "#") return true; // 匹配其后的任意层级（含零级）
            if (i >= topics.Length) return false; // 过滤器比主题深，且没有 # 兜底
            if (filters[i] != "+" && !string.Equals(filters[i], topics[i], StringComparison.Ordinal))
                return false;
        }

        // 层级数必须严格相等：plant/+ 不该命中 plant（少一级），也不该命中 plant/a/b（多一级）。
        return filters.Length == topics.Length;
    }
}
