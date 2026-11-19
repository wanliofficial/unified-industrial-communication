package s7

import (
	"encoding/hex"
	"testing"

	"uic/internal/driver"
)

// 本文件是 S7 codec 的回归测试。钉住的核心契约：响应解析必须**严格**，
// 任何「长度对不上」的响应都要判坏，不能靠猜把缺失项当成功。
//
// 背景：S7 写响应每个项目只有一个返回码字节，而响应本身不带「实际返回了几个
// 项」。一个被截断的响应（链路抖动、对端实现不全）若被当成「剩下的都成功」，
// 一半的写请求会显示成功、实际没落到 PLC——计数类点位重复累加，脉冲类点位
// 少动一次，而且现场完全看不出问题。

func parseFrame(t *testing.T, s string) PDU {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	pdu, err := Parse(b)
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	return pdu
}

// 写响应数据区被截断：3 项请求只回 1 个返回码。必须判坏，不能把第 2、3 项
// 当成成功。
func TestWriteAckTruncatedDataIsBad(t *testing.T) {
	// TPKT(4) + COTP(3) + ACK_DATA 头(12) + 参数[0x05,3](2) + 数据 1 字节。
	// 头：32 03 0000 0001 0002 0001 0000（errclass/errcode）
	pdu := parseFrame(t, "03000016"+"02f080"+"32030000000100020001"+"0000"+"0503"+"ff")
	errs, err := ParseWriteAck(pdu, 3)
	if err == nil {
		t.Fatalf("truncated write ack accepted: errs=%v", errs)
	}
	if len(errs) != 0 {
		t.Fatalf("truncated ack must not return per-item results, got %v", errs)
	}
}

// 参数区项数与请求数不一致：判坏。
func TestWriteAckCountMismatchIsBad(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncWrite, 2}, Data: []byte{CodeOK, CodeOK}}
	if _, err := ParseWriteAck(pdu, 3); err == nil {
		t.Fatal("count mismatch must be rejected")
	}
}

// 参数区不是写功能码：判坏。
func TestWriteAckWrongFuncIsBad(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncRead, 1}, Data: []byte{CodeOK}}
	if _, err := ParseWriteAck(pdu, 1); err == nil {
		t.Fatal("read param in a write ack must be rejected")
	}
}

// 错误帧（JobAck 不是 AckData）：判坏。
func TestWriteAckWrongRosctrIsBad(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRJob, Param: []byte{FuncWrite, 1}, Data: []byte{CodeOK}}
	if _, err := ParseWriteAck(pdu, 1); err == nil {
		t.Fatal("non-ack rosctr must be rejected")
	}
}

// 完整响应（每项一个返回码）必须照常成功：严格解析不能误杀正常路径。
func TestWriteAckFullOKSucceeds(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncWrite, 3},
		Data: []byte{CodeOK, CodeOK, CodeOK}}
	errs, err := ParseWriteAck(pdu, 3)
	if err != nil {
		t.Fatalf("full ack rejected: %v", err)
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("item %d: %v", i, e)
		}
	}
}

// 单项错误必须带着自己的分类，不能被整体吞掉。
func TestWriteAckPerItemErrorKeepsKind(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncWrite, 3},
		Data: []byte{CodeOK, CodeObjectNotFound, CodeAccessDenied}}
	errs, err := ParseWriteAck(pdu, 3)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if errs[0] != nil {
		t.Fatalf("item 0 should succeed: %v", errs[0])
	}
	if k := errs[1].(ItemError).Kind(); k != driver.ErrAddress {
		t.Fatalf("item 1 kind = %v, want address (0x0a 对象不存在)", k)
	}
	if k := errs[2].(ItemError).Kind(); k != driver.ErrPermission {
		t.Fatalf("item 2 kind = %v, want permission", k)
	}
}

// 读响应的数据区被截断：判坏（parse 侧已经严格）。
func TestReadAckTruncatedPayloadIsBad(t *testing.T) {
	// 1 项 REAL(4 字节)：参数 0401 + 数据 ff 07 00 04 3f800000。
	// 完整帧：4+3+12+2+8 = 29 = 0x1d。
	full := parseFrame(t, "0300001d"+"02f080"+"32030000000200020008"+"0000"+"0401"+"ff0700043f800000")
	out, errs, err := ParseReadAck(full, []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 4}})
	if err != nil {
		t.Fatalf("full ack rejected: %v", err)
	}
	if errs[0] != nil || len(out[0]) != 4 {
		t.Fatalf("full ack mis-parsed: %v % x", errs[0], out[0])
	}

	// 数据区砍到 5 字节（缺 3 字节有效载荷）：判坏。
	short := parseFrame(t, "0300001a"+"02f080"+"32030000000200020005"+"0000"+"0401"+"ff0700043f")
	if _, _, err := ParseReadAck(short, []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 4}}); err == nil {
		t.Fatal("truncated read ack payload must be rejected")
	}
}

// 参数区项数与请求数不一致（读）：判坏。
func TestReadAckCountMismatchIsBad(t *testing.T) {
	pdu := PDU{ROSCTR: ROSCTRAckData, Param: []byte{FuncRead, 2}, Data: []byte{CodeOK, RSByteWord, 0, 8, 0, 0}}
	if _, _, err := ParseReadAck(pdu, []Item{{Area: AreaDB, DB: 1, ByteAddr: 0, Bit: -1, Bytes: 1}}); err == nil {
		t.Fatal("count mismatch must be rejected")
	}
}
