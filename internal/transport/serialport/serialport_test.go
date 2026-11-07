package serialport

import (
	"errors"
	"sync"
	"testing"
	"time"

	"uic/internal/transport"
)

// fakeStream 是内存串口。
type fakeStream struct {
	mu        sync.Mutex
	writes    [][]byte
	closed    int
	deadline  time.Time
	drains    int
	readQueue []byte
}

func (f *fakeStream) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed > 0 {
		return errors.New("closed")
	}
	f.writes = append(f.writes, append([]byte(nil), p...))
	return nil
}

func (f *fakeStream) ReadFull(n int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed > 0 {
		return nil, errors.New("closed")
	}
	if len(f.readQueue) < n {
		return nil, errors.New("no data")
	}
	out := append([]byte(nil), f.readQueue[:n]...)
	f.readQueue = f.readQueue[n:]
	return out, nil
}

func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeStream) SetDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline = t
	return nil
}

func (f *fakeStream) Drain() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
	f.readQueue = nil
	return nil
}

func newOpener(st transport.Stream, opens *int) Opener {
	return func(transport.SerialConfig) (transport.Stream, error) {
		if opens != nil {
			*opens++
		}
		return st, nil
	}
}

func TestAcquireReusesSamePath(t *testing.T) {
	st := &fakeStream{}
	opens := 0
	cfg := transport.SerialConfig{Port: "/dev/fake-serialport-1", Baud: 9600}

	r1, err := Acquire("rtu", cfg, newOpener(st, &opens))
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	r2, err := Acquire("rtu", cfg, newOpener(st, &opens))
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if opens != 1 {
		t.Fatalf("opened %d times, want 1", opens)
	}
	if got := Count(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
	if err := r1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if st.closed != 0 {
		t.Fatal("port closed while a second reference is alive")
	}
	if err := r2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if st.closed != 1 {
		t.Fatalf("underlying close = %d, want 1", st.closed)
	}
	if got := Count(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}

func TestAcquireRejectsParamConflict(t *testing.T) {
	st := &fakeStream{}
	opener := newOpener(st, nil)
	if _, err := Acquire("rtu", transport.SerialConfig{Port: "/dev/fake-serialport-2", Baud: 9600}, opener); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = Count() }()
	if _, err := Acquire("rtu", transport.SerialConfig{Port: "/dev/fake-serialport-2", Baud: 19200}, opener); err == nil {
		t.Fatal("conflicting baud must be rejected")
	}
}

func TestAcquireRejectsFramingConflict(t *testing.T) {
	st := &fakeStream{}
	opener := newOpener(st, nil)
	cfg := transport.SerialConfig{Port: "/dev/fake-serialport-3", Baud: 2400}
	r, err := Acquire("dlt645", cfg, opener)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = r.Close() }()

	// 同一根线不能既跑 645 又跑 Modbus RTU：两种分帧交错会让校验码
	// 随机失败，现场几乎无法定位。
	if _, err := Acquire("rtu", cfg, opener); err == nil {
		t.Fatal("mixing framings on one port must be rejected")
	}
}

func TestAcquireValidatesConfig(t *testing.T) {
	if _, err := Acquire("rtu", transport.SerialConfig{}, nil); err == nil {
		t.Fatal("empty port must be rejected")
	}
}

func TestRefForwardsIO(t *testing.T) {
	st := &fakeStream{readQueue: []byte{1, 2, 3, 4}}
	r, err := Acquire("rtu", transport.SerialConfig{Port: "/dev/fake-serialport-4", Baud: 9600}, newOpener(st, nil))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = r.Close() }()

	if err := r.Write([]byte{0xAA}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(st.writes) != 1 || st.writes[0][0] != 0xAA {
		t.Fatalf("writes = %v", st.writes)
	}
	got, err := r.ReadFull(4)
	if err != nil || len(got) != 4 || got[3] != 4 {
		t.Fatalf("read = %v, %v", got, err)
	}
	if err := r.SetDeadline(time.Now()); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if err := r.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if st.drains != 1 {
		t.Fatalf("drains = %d", st.drains)
	}
}

func TestRefCloseIsIdempotent(t *testing.T) {
	st := &fakeStream{}
	r, err := Acquire("rtu", transport.SerialConfig{Port: "/dev/fake-serialport-5", Baud: 9600}, newOpener(st, nil))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if st.closed != 1 {
		t.Fatalf("underlying close = %d, want 1", st.closed)
	}
}

func TestBusLockSerializesTransactions(t *testing.T) {
	st := &fakeStream{}
	cfg := transport.SerialConfig{Port: "/dev/fake-serialport-6", Baud: 9600}
	r1, err := Acquire("dlt645", cfg, newOpener(st, nil))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = r1.Close() }()
	r2, err := Acquire("dlt645", cfg, newOpener(st, nil))
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	defer func() { _ = r2.Close() }()

	// 同一个 Port 的两个引用必须互相排斥；否则半双工链路上两条事务的
	// 字节会交错。
	r1.Lock()
	done := make(chan struct{})
	go func() {
		r2.Lock()
		r2.Unlock()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second reference acquired the bus lock while it was held")
	case <-time.After(50 * time.Millisecond):
	}
	r1.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bus lock was not released")
	}
}
