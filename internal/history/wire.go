package history

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"uic/internal/model"
)

// 磁盘上的记录格式刻意用短键 + 扁平值，而不是直接 json.Marshal(model.Sample)：
//
//	{"t":1762345678901,"d":"press","p":"power","ty":"float64","v":73.4,"q":"good"}
//
// 一条记录约 70 字节，比带 device/point/collectedAt/seq 的 Sample（约 220 字节）
// 小三倍。历史库是唯一会长期增长的数据，单条记录大小直接决定「同样的磁盘能
// 存多久」；而 Sample 里的 seq/collectedAt 在历史场景没有意义（顺序由时间戳
// 决定，采集时刻就是记录时刻）。
//
// `ty` 必须显式存：`num:0` 与「没有值」在 JSON 里不可区分，而 int32 0 与
// float64 0 在回读时需要还原成同一个点位声明的类型。
type wireRecord struct {
	T  int64          `json:"t"`
	D  string         `json:"d"`
	P  string         `json:"p"`
	TY model.DataType `json:"ty,omitempty"`
	V  *float64       `json:"v,omitempty"`
	S  string         `json:"s,omitempty"`
	B  *bool          `json:"b,omitempty"`
	// X 是 bytes 的 base64。不能塞进 s：JSON 字符串必须是合法 UTF-8，
	// 编码器会把非法字节替换成 U+FFFD，回读出来的内容就变了。
	X *string       `json:"x,omitempty"`
	Q model.Quality `json:"q,omitempty"`
}

// encodeRecord 把一条记录编码成一行 JSON（含行尾换行）。
func encodeRecord(r Record) ([]byte, error) {
	if r.Device == "" || r.Point == "" {
		return nil, errors.New("history: device and point are required")
	}
	w := wireRecord{
		T:  r.Time.UnixMilli(),
		D:  r.Device,
		P:  r.Point,
		TY: r.Value.Type,
		Q:  r.Quality,
	}
	switch {
	case r.Value.Type == model.TypeBool:
		b := r.Value.Bool
		w.B = &b
	case r.Value.Type == model.TypeBytes:
		x := base64.StdEncoding.EncodeToString([]byte(r.Value.Text))
		w.X = &x
	case r.Value.Type == model.TypeString:
		w.S = r.Value.Text
	default:
		v := r.Value.Num
		w.V = &v
	}
	// 不能再用 json.Marshal 的 Encoder（会多一个换行/需要缓冲），这里手动
	// 追加换行后一次写入，保证一条记录要么整行落盘、要么完全不可见。
	b, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("history: encode record: %w", err)
	}
	return append(b, '\n'), nil
}

// decodeRecord 解析一行。未知字段被忽略（向前兼容：以后加字段不会让旧
// 版本读出垃圾，只会丢新字段——历史数据本来就不需要完美回放新语义）。
func decodeRecord(line []byte) (Record, error) {
	var w wireRecord
	if err := json.Unmarshal(line, &w); err != nil {
		return Record{}, err
	}
	if w.D == "" || w.P == "" || w.T == 0 {
		return Record{}, errors.New("history: incomplete record")
	}
	r := Record{
		Time:    time.UnixMilli(w.T).UTC(),
		Device:  w.D,
		Point:   w.P,
		Quality: w.Q,
	}
	if r.Quality == "" {
		// 早期记录可能没写质量码；默认 good 比默认 bad 更接近事实，
		// 而且 bad 会让整段趋势在 UI 上被判为不可信。
		r.Quality = model.QualityGood
	}
	switch {
	case w.B != nil:
		r.Value = model.BoolValue(*w.B)
		if w.TY != "" {
			r.Value.Type = w.TY
		}
	case w.X != nil:
		data, err := base64.StdEncoding.DecodeString(*w.X)
		if err != nil {
			return Record{}, errors.New("history: invalid base64 payload")
		}
		r.Value = model.TextValue(model.TypeBytes, string(data))
	case isTextType(w.TY):
		r.Value = model.TextValue(w.TY, w.S)
	case w.V != nil:
		r.Value = model.NumValue(w.TY, *w.V)
	default:
		// 数字 0 会被编码为显式的 "v":0；既没有 v/s/b 说明这行被截断或
		// 被外部工具改写，按坏行处理，不猜一个 0 出来。
		return Record{}, errors.New("history: record has no value")
	}
	return r, nil
}

func isTextType(t model.DataType) bool {
	return t == model.TypeString || t == model.TypeBytes
}
