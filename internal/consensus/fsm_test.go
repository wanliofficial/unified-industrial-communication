package consensus

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// ---------------------------------------------------------------------------
// codec
// ---------------------------------------------------------------------------

func TestCommandCodecRoundTrip(t *testing.T) {
	cases := []Command{
		{Kind: KindApplyConfig, InstanceID: "inst-1", Payload: []byte(`{"schemaVersion":1}`)},
		{Kind: KindWritePoints, InstanceID: "inst-2", Payload: []byte{0x00, 0xff, 0x10}},
		{Kind: KindAckAlarms, InstanceID: "", Payload: nil},
		{Kind: KindCommand, InstanceID: "中文实例", Payload: []byte("动作")},
	}
	for _, c := range cases {
		got, err := decodeCommand(encodeCommand(c))
		if err != nil {
			t.Fatalf("decode %v: %v", c, err)
		}
		if got.Kind != c.Kind || got.InstanceID != c.InstanceID || !bytes.Equal(got.Payload, c.Payload) {
			t.Errorf("roundtrip mismatch: want %+v got %+v", c, got)
		}
	}
}

func TestCommandDecodeRejects(t *testing.T) {
	valid := encodeCommand(Command{Kind: KindCommand, InstanceID: "x", Payload: []byte("p")})

	t.Run("empty", func(t *testing.T) {
		if _, err := decodeCommand(nil); err == nil {
			t.Fatal("expected error for empty input")
		}
	})
	t.Run("bad version", func(t *testing.T) {
		b := append([]byte(nil), valid...)
		b[0] = 99
		if _, err := decodeCommand(b); !errors.Is(err, errBadVersion) {
			t.Fatalf("want errBadVersion, got %v", err)
		}
	})
	t.Run("truncated header", func(t *testing.T) {
		if _, err := decodeCommand(valid[:3]); err == nil {
			t.Fatal("expected error for truncated header")
		}
	})
	t.Run("length overflow", func(t *testing.T) {
		// 把首个长度字段改成超大值，触发越界。
		b := append([]byte(nil), valid...)
		b[1] = 0xff
		b[2] = 0xff
		b[3] = 0xff
		b[4] = 0xff
		if _, err := decodeCommand(b); !errors.Is(err, errLengthField) {
			t.Fatalf("want errLengthField, got %v", err)
		}
	})
	t.Run("trailing bytes", func(t *testing.T) {
		b := append(append([]byte(nil), valid...), 0xde, 0xad)
		if _, err := decodeCommand(b); !errors.Is(err, errLengthField) {
			t.Fatalf("want errLengthField for trailing bytes, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// fsm
// ---------------------------------------------------------------------------

func TestStateMachineApplyDispatches(t *testing.T) {
	app := newRecordingApplier()
	sm := NewStateMachine(app)
	cmd := Command{Kind: KindWritePoints, InstanceID: "i", Payload: []byte("x")}
	res := sm.Apply(&raft.Log{Index: 7, Data: encodeCommand(cmd)})
	if err := asError(res); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if sm.LastApplied() != 7 {
		t.Errorf("LastApplied = %d, want 7", sm.LastApplied())
	}
	if !sm.IsHealthy() {
		t.Error("expected healthy after successful apply")
	}
	got := app.Applied()
	if len(got) != 1 || got[0].Kind != KindWritePoints {
		t.Fatalf("applier got %+v", got)
	}
}

func TestStateMachineBadKindMarksUnhealthy(t *testing.T) {
	sm := NewStateMachine(newRecordingApplier())
	res := sm.Apply(&raft.Log{Index: 1, Data: encodeCommand(Command{Kind: Kind("bogus")})})
	if !errors.Is(asError(res), ErrBadCommand) {
		t.Fatalf("want ErrBadCommand, got %v", asError(res))
	}
	if sm.IsHealthy() {
		t.Fatal("expected unhealthy after bad kind")
	}
}

func TestStateMachineBadDecodeMarksUnhealthy(t *testing.T) {
	sm := NewStateMachine(newRecordingApplier())
	res := sm.Apply(&raft.Log{Index: 1, Data: []byte{0xff}}) // bad version
	if asError(res) == nil {
		t.Fatal("expected decode error")
	}
	if sm.IsHealthy() {
		t.Fatal("expected unhealthy after bad decode")
	}
}

func TestStateMachineApplyErrorMarksUnhealthy(t *testing.T) {
	app := newRecordingApplier()
	app.setFail(errors.New("boom"))
	sm := NewStateMachine(app)
	res := sm.Apply(&raft.Log{Index: 1, Data: encodeCommand(Command{Kind: KindCommand})})
	if asError(res) == nil {
		t.Fatal("expected apply error")
	}
	if sm.IsHealthy() {
		t.Fatal("expected unhealthy after apply failure")
	}
	// 失败不应推进 lastApplied。
	if sm.LastApplied() != 0 {
		t.Errorf("LastApplied = %d, want 0 after failed apply", sm.LastApplied())
	}
}

func TestStateMachineRestoreClearsError(t *testing.T) {
	app := newRecordingApplier()
	sm := NewStateMachine(app)
	// 先制造不健康。
	sm.Apply(&raft.Log{Index: 1, Data: []byte{0xff}})
	if sm.IsHealthy() {
		t.Fatal("precondition: expected unhealthy")
	}
	// Restore 成功应清除分歧标记。
	if err := sm.Restore(io.NopCloser(bytes.NewReader(nil))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !sm.IsHealthy() {
		t.Fatal("expected healthy after successful restore")
	}
}

// ---------------------------------------------------------------------------
// bbolt store
// ---------------------------------------------------------------------------

func TestBboltStoreLogLifecycle(t *testing.T) {
	store, err := newBboltStore(t.TempDir() + "/raft.db")
	if err != nil {
		t.Fatalf("newBboltStore: %v", err)
	}
	defer store.Close()

	if first, _ := store.FirstIndex(); first != 0 {
		t.Errorf("FirstIndex empty = %d, want 0", first)
	}
	if last, _ := store.LastIndex(); last != 0 {
		t.Errorf("LastIndex empty = %d, want 0", last)
	}

	logs := []*raft.Log{
		{Index: 1, Term: 1, Type: raft.LogCommand, Data: []byte("a")},
		{Index: 2, Term: 1, Type: raft.LogCommand, Data: []byte("b"), AppendedAt: time.Unix(1000, 0)},
		{Index: 3, Term: 2, Type: raft.LogNoop},
	}
	if err := store.StoreLogs(logs); err != nil {
		t.Fatalf("StoreLogs: %v", err)
	}
	if first, _ := store.FirstIndex(); first != 1 {
		t.Errorf("FirstIndex = %d, want 1", first)
	}
	if last, _ := store.LastIndex(); last != 3 {
		t.Errorf("LastIndex = %d, want 3", last)
	}

	var got raft.Log
	if err := store.GetLog(2, &got); err != nil {
		t.Fatalf("GetLog(2): %v", err)
	}
	if got.Index != 2 || got.Term != 1 || !bytes.Equal(got.Data, []byte("b")) {
		t.Errorf("GetLog(2) = %+v", got)
	}
	if !got.AppendedAt.Equal(time.Unix(1000, 0)) {
		t.Errorf("AppendedAt = %v, want %v", got.AppendedAt, time.Unix(1000, 0))
	}

	if err := store.GetLog(99, &got); !errors.Is(err, raft.ErrLogNotFound) {
		t.Errorf("GetLog(99) err = %v, want ErrLogNotFound", err)
	}

	if err := store.DeleteRange(1, 2); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if first, _ := store.FirstIndex(); first != 3 {
		t.Errorf("FirstIndex after delete = %d, want 3", first)
	}
	var deleted raft.Log
	if err := store.GetLog(1, &deleted); !errors.Is(err, raft.ErrLogNotFound) {
		t.Errorf("GetLog(1) after delete err = %v, want ErrLogNotFound", err)
	}
}

func TestBboltStoreStable(t *testing.T) {
	store, err := newBboltStore(t.TempDir() + "/raft.db")
	if err != nil {
		t.Fatalf("newBboltStore: %v", err)
	}
	defer store.Close()

	// 不存在的 key 返回空切片、无错误。
	if v, err := store.Get([]byte("nope")); err != nil || len(v) != 0 {
		t.Errorf("Get missing = %v, %v; want empty,nil", v, err)
	}
	if u, err := store.GetUint64([]byte("nope")); err != nil || u != 0 {
		t.Errorf("GetUint64 missing = %d, %v; want 0,nil", u, err)
	}

	if err := store.Set([]byte("curTerm"), []byte("42")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := store.Get([]byte("curTerm")); string(v) != "42" {
		t.Errorf("Get = %q, want 42", v)
	}

	if err := store.SetUint64([]byte("votedFor"), 7); err != nil {
		t.Fatalf("SetUint64: %v", err)
	}
	if u, _ := store.GetUint64([]byte("votedFor")); u != 7 {
		t.Errorf("GetUint64 = %d, want 7", u)
	}

	// 损坏的 uint64 值应报错而非静默返回 0。
	if err := store.Set([]byte("bad"), []byte("xx")); err != nil {
		t.Fatalf("Set bad: %v", err)
	}
	if _, err := store.GetUint64([]byte("bad")); err == nil {
		t.Error("GetUint64 on corrupt value should error")
	}
}

func TestLogEncodeDecodeRoundTrip(t *testing.T) {
	cases := []raft.Log{
		{Index: 0, Term: 0, Type: raft.LogCommand},
		{Index: 1<<40 + 5, Term: 9, Type: raft.LogConfiguration, Data: []byte{1, 2, 3}, Extensions: []byte{9, 8}},
		{Index: 3, Term: 2, Type: raft.LogNoop, AppendedAt: time.Unix(1234567890, 42)},
	}
	for _, l := range cases {
		var got raft.Log
		if err := decodeLog(encodeLog(&l), &got); err != nil {
			t.Fatalf("decodeLog: %v", err)
		}
		if got.Index != l.Index || got.Term != l.Term || got.Type != l.Type ||
			!bytes.Equal(got.Data, l.Data) || !bytes.Equal(got.Extensions, l.Extensions) {
			t.Errorf("roundtrip mismatch: want %+v got %+v", l, got)
		}
		if !got.AppendedAt.Equal(l.AppendedAt) {
			t.Errorf("AppendedAt mismatch: want %v got %v", l.AppendedAt, got.AppendedAt)
		}
	}
}

func TestDecodeLogRejects(t *testing.T) {
	valid := encodeLog(&raft.Log{Index: 1, Term: 1, Type: raft.LogCommand, Data: []byte("abc")})
	t.Run("truncated", func(t *testing.T) {
		var got raft.Log
		if err := decodeLog(valid[:10], &got); err == nil {
			t.Fatal("expected error for truncated log")
		}
	})
	t.Run("bad data length", func(t *testing.T) {
		b := append([]byte(nil), valid...)
		// dataLen 在 offset 26..30；改成超大。
		b[26], b[27], b[28], b[29] = 0xff, 0xff, 0xff, 0xff
		var got raft.Log
		if err := decodeLog(b, &got); err == nil {
			t.Fatal("expected error for overflow data length")
		}
	})
	t.Run("trailing", func(t *testing.T) {
		b := append(append([]byte(nil), valid...), 0x00)
		var got raft.Log
		if err := decodeLog(b, &got); err == nil {
			t.Fatal("expected error for trailing bytes")
		}
	})
}

// asError 把 raft FSM.Apply 的返回值（interface{}）还原为 error（非 error 返回 nil）。
func asError(v interface{}) error {
	if err, ok := v.(error); ok {
		return err
	}
	return nil
}
