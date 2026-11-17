package cjt188

import "testing"

// TestParseConsumedIncludesLeadingNoise 钉住成功路径 consumed 的契约：
// consumed 必须从缓冲区开头起算（start + total），这样调用方写
// `buf = buf[consumed:]` 才能恰好停在帧尾。只返回 total 会把前导噪声留在
// 头部、同时砍掉帧尾等长的真实字节——比解析失败更难发现，因为它不报错。
func TestParseConsumedIncludesLeadingNoise(t *testing.T) {
	addr := mustAddr(t, "00000805000001")
	fr, err := ReadRequest(0x10, addr, DIReadData, 0x07)
	if err != nil {
		t.Fatal(err)
	}
	for _, pre := range []int{0, 2, 4} {
		buf := make([]byte, 0, pre+len(fr))
		for i := 0; i < pre; i++ {
			buf = append(buf, Preamble)
		}
		buf = append(buf, fr...)
		// 帧后再塞几个字节，确认 consumed 不会把后续数据吞进来。
		buf = append(buf, 0xAA, 0xBB, 0xCC)
		_, consumed, err := Parse(buf)
		if err != nil {
			t.Fatalf("pre=%d: %v", pre, err)
		}
		if want := pre + len(fr); consumed != want {
			t.Fatalf("pre=%d: consumed=%d, want %d (start+total)", pre, consumed, want)
		}
	}
}
