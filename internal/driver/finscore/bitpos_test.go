package finscore

import (
	"context"
	"encoding/binary"
	"testing"

	finscodec "uic/internal/codec/fins"
	"uic/internal/driver"
	"uic/internal/model"
)

// bitTr 是一个位应答信封：第 i 个位字节按调用方给定的模式填充，使不同
// 位置解出不同的真值。fakeTr 的位读应答恒为全 1（见 conn_test.go），无法
// 验证位定位公式，本文件用 bitTr 补上这个锚点。
type bitTr struct {
	hdr  finscodec.Header
	reqs [][]byte
	// bitAt 返回第 i 个位（从请求起始字 bit0 起算的连续位序号）的值。
	bitAt func(i int) byte
}

func (b *bitTr) Header() finscodec.Header { return finscodec.Header{ICF: 0x00, GCT: 2} }
func (b *bitTr) Close() error             { return nil }
func (b *bitTr) RoundTrip(ctx context.Context, req []byte) (finscodec.Frame, error) {
	b.reqs = append(b.reqs, append([]byte(nil), req...))
	parsed, err := finscodec.Parse(req)
	if err != nil {
		return finscodec.Frame{}, err
	}
	count := int(binary.BigEndian.Uint16(parsed.Data[4:6]))
	data := make([]byte, count)
	for i := 0; i < count; i++ {
		data[i] = b.bitAt(i)
	}
	out, _ := finscodec.Parse(finscodec.BuildResponse(parsed, 0, data))
	return out, nil
}

func newBitConn(tr *bitTr) *Conn {
	cfg := driver.OpenConfig{
		Channel: model.Channel{ID: "ch", Timeout: model.Duration(0)},
		Device:  model.Device{ID: "dev", ChannelID: "ch"},
	}
	return New(cfg, tr, Limits{WordsPerRead: 8, WordsPerWrite: 8, BitsPerRead: 128, BitsPerWrite: 128})
}

// TestReadBitPositionDecodesAcrossWords 锁定跨字位读的定位公式
// pos = (off-start)*16 + bit。
//
// 三个点跨两个字：word0.bit5、word1.bit0、word1.bit15，对应应答位数组的
// pos 5、16、31。只让这三个位置为真，若实现把字步长（*16）算错，word1 上
// 的两个点会读到错误位置而解码失败。反向再来一例（只有 pos16 为假），
// 证明定位不是「全真」蒙混。
func TestReadBitPositionDecodesAcrossWords(t *testing.T) {
	tr := &bitTr{bitAt: func(i int) byte {
		switch i {
		case 5, 16, 31:
			return 1
		default:
			return 0
		}
	}}
	c := newBitConn(tr)
	samples, _ := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: 5}, DataType: model.TypeBool},
		{ID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: 0}, DataType: model.TypeBool},
		{ID: "cc", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: 15}, DataType: model.TypeBool},
	}}})
	got := map[string]bool{}
	for _, s := range samples {
		got[s.PointID] = s.Value.Bool
	}
	if !got["a"] || !got["b"] || !got["cc"] {
		t.Fatalf("expected a/b/cc all true; got %v (reqs=%d)", got, len(tr.reqs))
	}

	// 反向：只有 pos16 为假，则 word1.bit0 必须为假、其余为真。
	tr2 := &bitTr{bitAt: func(i int) byte {
		if i == 16 {
			return 0
		}
		return 1
	}}
	c2 := newBitConn(tr2)
	samples2, _ := c2.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{
		{ID: "a", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 0, Bit: 5}, DataType: model.TypeBool},
		{ID: "b", Address: model.Address{Kind: model.AddressRegister, Area: "dm", Offset: 1, Bit: 0}, DataType: model.TypeBool},
	}}})
	got2 := map[string]bool{}
	for _, s := range samples2 {
		got2[s.PointID] = s.Value.Bool
	}
	if !got2["a"] || got2["b"] {
		t.Fatalf("expected a=true b=false; got %v", got2)
	}
}
