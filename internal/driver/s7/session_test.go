package s7

import (
	"bytes"
	"context"
	"testing"
	"time"

	s7codec "uic/internal/codec/s7"
	"uic/internal/driver"
	s7sim "uic/internal/sim/s7"
)

func openSession(t *testing.T, srv *s7sim.Server) *Session {
	t.Helper()
	s, err := OpenSession(context.Background(), SessionOptions{
		Address: srv.Addr(), Rack: 0, Slot: 2, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 块目录与块属性是两条独立的 USER_DATA 查询：数量表与块号列表不能混。
func TestSessionListAndInfo(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetBytes(s7codec.AreaDB, 1, 0, []byte{0xDE, 0xAD, 0xBE, 0xEF})

	s := openSession(t, srv)
	ctx := context.Background()

	counts, err := s.ListBlocks(ctx)
	if err != nil {
		t.Fatalf("list blocks: %v", err)
	}
	got := map[byte]int{}
	for _, c := range counts {
		got[c.Type] = c.Count
	}
	if got[s7codec.BlockOB] != 1 {
		t.Fatalf("OB count = %d, want 1", got[s7codec.BlockOB])
	}
	if got[s7codec.BlockDB] != 1 {
		t.Fatalf("DB count = %d, want 1", got[s7codec.BlockDB])
	}
	if got[s7codec.BlockFB] != 0 {
		t.Fatalf("FB count = %d, want 0", got[s7codec.BlockFB])
	}

	dbs, err := s.ListBlocksOfType(ctx, s7codec.BlockDB)
	if err != nil {
		t.Fatalf("list db: %v", err)
	}
	if len(dbs) != 1 || dbs[0] != 1 {
		t.Fatalf("db numbers = %v, want [1]", dbs)
	}

	info, err := s.BlockInfo(ctx, s7codec.BlockDB, 1)
	if err != nil {
		t.Fatalf("block info: %v", err)
	}
	if info.Type != s7codec.BlockDB || info.Number != 1 {
		t.Fatalf("info = %+v", info)
	}
	if info.LoadSize != 1024 || info.MC7Size != 1024 {
		t.Fatalf("sizes = load %d mc7 %d, want 1024", info.LoadSize, info.MC7Size)
	}
	if info.Author != "UIC-SIM" || info.Family != "SIM" {
		t.Fatalf("strings = %+v", info)
	}

	// OB1 默认存在但内容为空：上传必须成功且长度为 0，而不是报错。
	ob, err := s.UploadBlock(ctx, s7codec.BlockOB, 1)
	if err != nil {
		t.Fatalf("upload ob: %v", err)
	}
	if len(ob) != 0 {
		t.Fatalf("ob len = %d, want 0", len(ob))
	}
}

func TestSessionUploadDB(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	want := make([]byte, 1024)
	for i := range want {
		want[i] = byte(i * 7)
	}
	srv.SetBytes(s7codec.AreaDB, 1, 0, want)

	s := openSession(t, srv)
	got, err := s.UploadBlock(context.Background(), s7codec.BlockDB, 1)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("uploaded %d bytes, mismatch at %d", len(got), firstDiff(got, want))
	}
}

// 分片必须真的分：240 的 PDU 每片最多 222 字节负载，5KB 要 23 片。
// 只测小块会让「永远只取第一片」这种 bug 全绿。
func TestSessionUploadFragmentsLargeBlock(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 240)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	want := make([]byte, 5000)
	for i := range want {
		want[i] = byte(i*31 + 5)
	}
	srv.SetBlock(s7codec.BlockDB, 7, want)

	s := openSession(t, srv)
	if s.PDULen() != 240 {
		t.Fatalf("pdu = %d, want 240", s.PDULen())
	}
	got, err := s.UploadBlock(context.Background(), s7codec.BlockDB, 7)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("uploaded %d bytes, first diff at %d", len(got), firstDiff(got, want))
	}
}

func TestSessionRejectsBadBlockNumber(t *testing.T) {
	srv, err := s7sim.NewServer("127.0.0.1:0", 480)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	s := openSession(t, srv)

	_, err = s.UploadBlock(context.Background(), s7codec.BlockDB, 100000)
	if err == nil {
		t.Fatal("block number 100000 must be rejected")
	}
	if driver.KindOf(err) != driver.ErrAddress {
		t.Fatalf("kind = %v, want ErrAddress", driver.KindOf(err))
	}
	if _, err := s.BlockInfo(context.Background(), 0x99, 1); err == nil {
		t.Fatal("unknown block type must be rejected")
	}
}

func TestOpenSessionRejectsBadAddress(t *testing.T) {
	_, err := OpenSession(context.Background(), SessionOptions{})
	if err == nil {
		t.Fatal("empty address must be rejected")
	}
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("kind = %v", driver.KindOf(err))
	}
	_, err = OpenSession(context.Background(), SessionOptions{Address: "127.0.0.1:1", Rack: 9})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("rack kind = %v", driver.KindOf(err))
	}
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
