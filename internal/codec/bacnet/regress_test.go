package bacnet

import "testing"

// 探针缺陷：ParseConfirmedRequest 的长度守卫 off-by-one。
//
// ConfirmedRequest 头固定 4 字节：pdu-type/flags、max-segs|seg|win、
// invokeID、service-choice；params（apdu[4:]）可以为空。守卫写 < 3 时，
// 一条恰好 3 字节的确认请求会读到 apdu[3] 与 apdu[4:]，直接 slice 越界
// panic。codec 对畸形报文必须回错误而非 panic：从站 serve 循环没有 recover，
// 一个截断的确认请求会崩掉整个进程。
func TestParseConfirmedRequestTruncatedHeaderNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseConfirmedRequest panicked on 3-byte input: %v", r)
		}
	}()

	// 3 字节（缺 service-choice）：越界，必须回错误。
	if _, err := ParseConfirmedRequest([]byte{APDUConfirmedRequest, 0x05, 0x07}); err == nil {
		t.Fatal("3-byte ConfirmedRequest must error, not panic")
	}

	// 0 字节：越界，必须回错误。
	if _, err := ParseConfirmedRequest(nil); err == nil {
		t.Fatal("empty ConfirmedRequest must error")
	}

	// 4 字节（含空 params）是合法的：service=ReadProperty，无参数。
	req, err := ParseConfirmedRequest([]byte{APDUConfirmedRequest, 0x05, 0x07, ServiceReadProperty})
	if err != nil {
		t.Fatalf("4-byte ConfirmedRequest must parse: %v", err)
	}
	if req.Service != ServiceReadProperty || req.InvokeID != 0x07 {
		t.Fatalf("parsed = %+v", req)
	}

	// 分段标志应被识别（首字节 bit3）。
	seg, err := ParseConfirmedRequest([]byte{APDUConfirmedRequest | 0x08, 0x05, 0x07, ServiceReadProperty})
	if err != nil {
		t.Fatalf("segmented ConfirmedRequest must parse: %v", err)
	}
	if !seg.Segmented {
		t.Fatal("segmented flag must be set")
	}
}
