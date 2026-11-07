package pointtable

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"uic/internal/model"
)

// 点表 CSV 是集成商真正会拿到的东西：客户给一份 Excel，列名各异，地址写成
// 各种习惯形式。本包负责把它变成 []model.Point，并在导入前把能静态发现的
// 问题全部报出来（带行号与列名），而不是等启动后每个周期刷一条 address_error。
//
// 设计取舍：
//   - 列名大小写不敏感、支持中英文别名，但**未知列直接报错**。少一个拼错的
//     列被打招呼式忽略，会让人以为「描述没导进来」是产品 bug。
//   - 一行出错不影响其它行的解析：1000 行点表一次改完，比修一行跑一次快。
//   - 冲突（同一设备内重复点位 ID）是错误；重复地址只是警告——两个点读同一
//     地址有时是有意的（不同缩放/单位），但更常见是复制粘贴事故。

// RowError 是一条带位置的导入错误。
type RowError struct {
	Line    int
	Column  string
	Value   string
	Message string
}

func (e *RowError) Error() string {
	var b strings.Builder
	if e.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", e.Line)
	}
	if e.Column != "" {
		fmt.Fprintf(&b, "%s: ", e.Column)
	}
	b.WriteString(e.Message)
	if e.Value != "" {
		fmt.Fprintf(&b, " (got %q)", e.Value)
	}
	return b.String()
}

// Errors 是一次导入发现的全部问题。错误信息只展开前几条：1000 行全错时
// 打印 1000 条既刷屏又没用，先修前几条通常能覆盖同一类错误。
type Errors []*RowError

func (es Errors) Error() string {
	const show = 8
	var b strings.Builder
	for i, e := range es {
		if i == show {
			fmt.Fprintf(&b, "\n... and %d more error(s)", len(es)-show)
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.Error())
	}
	return b.String()
}

// Warning 是不阻断导入的提示。
type Warning struct {
	Line    int
	PointID string
	Message string
}

func (w Warning) String() string {
	if w.Line > 0 {
		return fmt.Sprintf("line %d: point %q: %s", w.Line, w.PointID, w.Message)
	}
	return fmt.Sprintf("point %q: %s", w.PointID, w.Message)
}

// Result 是解析结果。
type Result struct {
	Points   []model.Point
	Warnings []Warning
}

var pointIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// 列名别名。normalizeHeader 会先做小写与去空白，因此这里只写规范形式。
var headerAliases = map[string]string{
	"point": "point", "id": "point", "pointid": "point", "点位": "point", "点号": "point",
	"address": "address", "addr": "address", "地址": "address",
	"type": "type", "datatype": "type", "数据类型": "type", "类型": "type",
	"name": "name", "名称": "name",
	"unit": "unit", "单位": "unit",
	"scale": "scale", "系数": "scale", "缩放": "scale",
	"bias": "bias", "偏置": "bias",
	"deadband": "deadband", "deadband%": "deadband", "死区": "deadband",
	"writable": "writable", "可写": "writable",
	"enabled": "enabled", "启用": "enabled",
	"byteorder": "byteOrder", "字节序": "byteOrder",
	"interval": "interval", "周期": "interval",
	"length": "length", "长度": "length",
	"description": "description", "desc": "description", "描述": "description",
}

var requiredColumns = []string{"point", "address", "type"}

// ParseCSV 解析点表。protocol 用于地址策略与长度推导（见 PolicyFor）。
func ParseCSV(r io.Reader, protocol string) (*Result, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = false

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, Errors{{Line: 1, Message: "empty CSV (header required)"}}
		}
		return nil, fmt.Errorf("read header: %w", err)
	}
	// Excel 导出的 UTF-8 CSV 常带 BOM；不剥掉会让第一列永远匹配不上。
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\uFEFF")
	}

	cols := make(map[string]int, len(header))
	var problems Errors
	for i, h := range header {
		key := normalizeHeader(h)
		if key == "" {
			problems = append(problems, &RowError{Line: 1, Column: fmt.Sprintf("column %d", i+1), Message: "empty column name"})
			continue
		}
		canon, ok := headerAliases[key]
		if !ok {
			problems = append(problems, &RowError{Line: 1, Column: key, Message: "unknown column"})
			continue
		}
		if prev, dup := cols[canon]; dup {
			problems = append(problems, &RowError{
				Line: 1, Column: key,
				Message: fmt.Sprintf("duplicate column (column %d and %d)", prev+1, i+1),
			})
			continue
		}
		cols[canon] = i
	}
	for _, req := range requiredColumns {
		if _, ok := cols[req]; !ok {
			problems = append(problems, &RowError{Line: 1, Column: req, Message: "required column is missing"})
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}

	res := &Result{}
	seenID := make(map[string]int, 64)
	seenAddr := make(map[string]string, 64)

	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				problems = append(problems, &RowError{Line: pe.Line, Column: fmt.Sprintf("column %d", pe.Column), Message: pe.Err.Error()})
				continue
			}
			return nil, fmt.Errorf("read line %d: %w", line, err)
		}
		if len(rec) == 0 {
			continue
		}
		// 整行空白：Excel 常在末尾留空行，不应当成错误。
		blank := true
		for _, v := range rec {
			if strings.TrimSpace(v) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		point, errs := parseRow(rec, cols, line, protocol)
		problems = append(problems, errs...)
		if point == nil {
			continue
		}
		if prev, dup := seenID[point.ID]; dup {
			problems = append(problems, &RowError{
				Line: line, Column: "point", Value: point.ID,
				Message: fmt.Sprintf("duplicate point id (first seen on line %d)", prev),
			})
			continue
		}
		seenID[point.ID] = line

		canon := point.Address.Canonical()
		if prev, dup := seenAddr[canon]; dup {
			res.Warnings = append(res.Warnings, Warning{
				Line: line, PointID: point.ID,
				Message: fmt.Sprintf("same address %s as point %q", canon, prev),
			})
		} else {
			seenAddr[canon] = point.ID
		}
		if pol := PolicyFor(protocol); pol.Width != WidthExplicit && point.DataType.IsNumeric() {
			if want, err := deriveLength("holding", point.DataType, pol.Width); err == nil && want != point.Address.Length {
				// 只对寄存器地址提示：符号/对象地址的 Length 没有协议含义
				// （模型里它恒为 0），拿类型宽度去比只会制造噪音。
				if point.Address.Kind == model.AddressRegister && point.Address.Bit < 0 {
					res.Warnings = append(res.Warnings, Warning{
						Line: line, PointID: point.ID,
						Message: fmt.Sprintf("length %d differs from %s width (%d)", point.Address.Length, point.DataType, want),
					})
				}
			}
		}
		res.Points = append(res.Points, *point)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return res, nil
}

func parseRow(rec []string, cols map[string]int, line int, protocol string) (*model.Point, Errors) {
	get := func(name string) string {
		i, ok := cols[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var errs Errors
	fail := func(column, value, format string, args ...any) {
		errs = append(errs, &RowError{Line: line, Column: column, Value: value, Message: fmt.Sprintf(format, args...)})
	}

	id := get("point")
	if id == "" {
		fail("point", "", "point id is required")
	} else if !pointIDRe.MatchString(id) {
		fail("point", id, "must match %s", pointIDRe.String())
	}

	dt := model.DataType(strings.ToLower(get("type")))
	dtOK := true
	if dt == "" {
		fail("type", "", "dataType is required")
		dtOK = false
	} else if !dt.Valid() {
		fail("type", string(dt), "unknown dataType")
		dtOK = false
	}

	// length 列要先读出来，再交给地址解析：像 `holding:10` + `length=8`
	// 这样的字符串点位，地址里本来就不写长度，只有先知道长度才可能成功。
	// 反过来（先解析再覆盖）会让这类点位永远报「length is required」。
	lengthHint := 0
	if v := get("length"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail("length", v, "must be a positive integer")
		} else if n > 125 {
			fail("length", v, "must be <= 125")
		} else {
			lengthHint = n
		}
	}

	// 类型本身不合法时不再解析地址：否则一行会冒出两条错误，第二条
	// （「length is required for nope」）只是第一条的衍生噪音。
	addrText := get("address")
	var addr model.Address
	if dtOK {
		a, err := ParseAddressWithLength(addrText, dt, protocol, lengthHint)
		if err != nil {
			fail("address", addrText, "%v", err)
		} else {
			addr = a
		}
	}

	p := model.Point{
		ID:          id,
		Name:        get("name"),
		Address:     addr,
		DataType:    dt,
		ByteOrder:   model.ByteOrder(strings.ToUpper(get("byteOrder"))),
		Unit:        get("unit"),
		Description: get("description"),
		// CSV 里的点默认启用：一份点表的存在本身就意味着「这些点要采」。
		Enabled: true,
	}

	if !p.ByteOrder.Valid() {
		fail("byteOrder", string(p.ByteOrder), "must be ABCD | CDAB | BADC | DCBA")
	}
	for _, f := range []struct {
		col string
		dst *float64
	}{{"scale", &p.Scale}, {"bias", &p.Bias}, {"deadband", &p.Deadband}} {
		v := get(f.col)
		if v == "" {
			continue
		}
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			fail(f.col, v, "must be a number")
			continue
		}
		*f.dst = x
	}
	if _, ok := cols["scale"]; ok && get("scale") == "0" {
		fail("scale", "0", "must not be 0 (omit the column to mean 1)")
	}

	if v := get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 100*time.Millisecond {
			fail("interval", v, "must be a duration >= 100ms")
		} else {
			p.Interval = model.Duration(d)
		}
	}
	for _, f := range []struct {
		col string
		dst *bool
	}{{"writable", &p.Writable}, {"enabled", &p.Enabled}} {
		v := strings.ToLower(get(f.col))
		if v == "" {
			continue
		}
		b, err := parseBool(v)
		if err != nil {
			fail(f.col, v, "must be true/false/yes/no/1/0")
			continue
		}
		*f.dst = b
	}

	if len(errs) > 0 {
		return nil, errs
	}
	if err := p.Validate(); err != nil {
		return nil, Errors{{Line: line, Message: err.Error()}}
	}
	return &p, nil
}

func normalizeHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.ReplaceAll(h, " ", "")
	h = strings.ReplaceAll(h, "_", "")
	h = strings.ReplaceAll(h, "-", "")
	return h
}

func parseBool(v string) (bool, error) {
	switch v {
	case "true", "yes", "y", "1":
		return true, nil
	case "false", "no", "n", "0":
		return false, nil
	}
	return false, fmt.Errorf("not a bool")
}
