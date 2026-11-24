package instanceid

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// idRe 校验生成格式：inst-<12 hex>。
var idRe = regexp.MustCompile(`^inst-[0-9a-f]{12}$`)

// TestLoadFirstRunGeneratesAndPersists：数据目录无身份文件时，Load 生成一个新
// 身份并落盘，且落盘内容与返回值一致。
func TestLoadFirstRunGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	id, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !idRe.MatchString(id) {
		t.Fatalf("首次生成的身份 %q 不匹配 %s", id, idRe)
	}
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("读回身份文件失败: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != id {
		t.Fatalf("落盘内容 %q 与返回值 %q 不一致", got, id)
	}
}

// TestLoadStableAcrossCalls：同一数据目录二次 Load 复用同一身份（重启稳定）。
// 这正是「实例身份持久」的核心断言——否则每次重启换身份，多实例消歧形同虚设。
func TestLoadStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("首次 Load: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := Load(dir)
		if err != nil {
			t.Fatalf("再次 Load(%d): %v", i, err)
		}
		if again != first {
			t.Fatalf("第 %d 次 Load 身份漂移: 首次 %q 现在 %q", i, first, again)
		}
	}
}

// TestLoadDistinctDirsDistinctIDs：不同数据目录得到不同身份——同站点多实例
// 部署（各自独立数据目录）默认就不冲突的根基。
func TestLoadDistinctDirsDistinctIDs(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	idA, err := Load(a)
	if err != nil {
		t.Fatalf("Load a: %v", err)
	}
	idB, err := Load(b)
	if err != nil {
		t.Fatalf("Load b: %v", err)
	}
	if idA == idB {
		t.Fatalf("两个不同数据目录得到相同身份 %q", idA)
	}
}

// TestLoadEmptyFileRegenerates：身份文件存在但为空时当作没有、重新生成。
// 空身份比新身份更糟——它会派生出空 clientID 后缀，等于没做消歧。
func TestLoadEmptyFileRegenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写空身份文件: %v", err)
	}
	id, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !idRe.MatchString(id) {
		t.Fatalf("空文件重生后身份 %q 不匹配 %s", id, idRe)
	}
}

// TestLoadDoesNotReturnEmpty：无论文件内容如何，Load 永不返回空串。
func TestLoadDoesNotReturnEmpty(t *testing.T) {
	dir := t.TempDir()
	id, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if id == "" {
		t.Fatal("Load 返回了空身份")
	}
}
