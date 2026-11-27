package consensus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/raft"
	bbolt "go.etcd.io/bbolt"
)

// bboltStore 用 go.etcd.io/bbolt 实现 raft.LogStore + raft.StableStore。
//
// 为什么自写而不是用 raft-boltdb/v2：后者会同时拖入遗留的 boltdb/bolt v1.3.1
// 兼容层（与 bbolt 双份），让依赖面与 SBOM 多出无必要模块。ADR-0005 的取舍是
// 「有限、可枚举、可审计」的依赖——所以这里只依赖一个 bbolt，把两个小接口
// （LogStore/StableStore 合计 ~10 个方法）自己实现。快照用 raft 内置的
// FileSnapshotStore（文件落盘），不再吃第三个存储依赖。
//
// 正确性要求（raft 对存储的契约）：
//   - StoreLogs 必须原子（整批要么全成要么全败），否则崩溃可能留下「日志
//     出现空洞」导致副本无法一致复制；
//   - DeleteRange 闭区间删除；
//   - 数据需持久化（Sync），否则崩溃丢失已 commit 日志。
type bboltStore struct {
	db *bbolt.DB
}

const (
	logBucketName    = "raft-log"
	stableBucketName = "raft-stable"
)

// newBboltStore 打开（或创建）一个 bbolt 存储文件。
func newBboltStore(path string) (*bboltStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("consensus: open bbolt %s: %w", path, err)
	}
	// 立即建 bucket，避免首次 Update 才创建带来的分支。
	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(logBucketName)); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists([]byte(stableBucketName))
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("consensus: init bbolt buckets: %w", err)
	}
	return &bboltStore{db: db}, nil
}

// Close 关闭底层 bbolt。
func (s *bboltStore) Close() error { return s.db.Close() }

// ---- raft.LogStore ----

// FirstIndex 返回最早日志索引；无日志返回 0。
func (s *bboltStore) FirstIndex() (uint64, error) {
	var first uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(logBucketName))
		k, _ := b.Cursor().First()
		if k != nil {
			first = binary.BigEndian.Uint64(k)
		}
		return nil
	})
	return first, err
}

// LastIndex 返回最新日志索引；无日志返回 0。
func (s *bboltStore) LastIndex() (uint64, error) {
	var last uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(logBucketName))
		k, _ := b.Cursor().Last()
		if k != nil {
			last = binary.BigEndian.Uint64(k)
		}
		return nil
	})
	return last, err
}

// GetLog 读取指定索引的日志到 log。索引不存在返回 raft.ErrLogNotFound。
func (s *bboltStore) GetLog(index uint64, log *raft.Log) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(logBucketName))
		v := b.Get(indexKey(index))
		if v == nil {
			return raft.ErrLogNotFound
		}
		return decodeLog(v, log)
	})
}

// StoreLog 写入单条日志。
func (s *bboltStore) StoreLog(log *raft.Log) error { return s.StoreLogs([]*raft.Log{log}) }

// StoreLogs 原子写入一批日志（单事务，保证要么全成要么全败）。
func (s *bboltStore) StoreLogs(logs []*raft.Log) error {
	encoded := make([][]byte, len(logs))
	for i, l := range logs {
		encoded[i] = encodeLog(l)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(logBucketName))
		for i, l := range logs {
			if err := b.Put(indexKey(l.Index), encoded[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteRange 删除 [min,max] 闭区间的日志。
func (s *bboltStore) DeleteRange(min, max uint64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(logBucketName))
		// 收集待删键（不能在 Cursor 同一事务里边遍历边删的某些实现不安全，
		// 这里先收集再删；bbolt 允许 Delete 后继续 Next）。
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(indexKey(min)); k != nil; k, _ = c.Next() {
			if binary.BigEndian.Uint64(k) > max {
				break
			}
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- raft.StableStore ----

// Set 写入一个稳定键值（term、votedFor、当前配置等）。
func (s *bboltStore) Set(key []byte, val []byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(stableBucketName)).Put(key, val)
	})
}

// Get 读取稳定键；不存在返回 nil（raft 契约要求空切片而非错误）。
func (s *bboltStore) Get(key []byte) ([]byte, error) {
	var out []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket([]byte(stableBucketName)).Get(key)
		if v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	if out == nil {
		out = []byte{}
	}
	return out, err
}

// SetUint64 写入一个稳定 uint64。
func (s *bboltStore) SetUint64(key []byte, val uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], val)
	return s.Set(key, buf[:])
}

// GetUint64 读取稳定 uint64；不存在返回 0（raft 契约）。
func (s *bboltStore) GetUint64(key []byte) (uint64, error) {
	v, err := s.Get(key)
	if err != nil || len(v) == 0 {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("consensus: corrupt uint64 stable value")
	}
	return binary.BigEndian.Uint64(v), nil
}

// ---- 编码 ----

// indexKey 把索引编码为 8 字节大端键（保持字节序与数值序一致，便于 Cursor 区间）。
func indexKey(index uint64) []byte {
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], index)
	return k[:]
}

// encodeLog 把 raft.Log 编码为可落盘的字节。
//
// 手工编码而非 msgpack/gob：日志是长期落盘数据，需要跨二进制升级可解码——
// 自定格式演进最可控；同时避免为存储再吃一个序列化依赖。格式与 codec.go
// 一致的「定长头 + 变长尾」思路。
//
// 格式（大端）：
//
//	[0..8)    Index uint64
//	[8..16)   Term uint64
//	[16]      Type uint8
//	[17]      appendedAtPresent uint8 (0/1)
//	[18..26)  AppendedAt UnixNano int64 (present 时)
//	[+4]      DataLen uint32，然后 Data
//	[+4]      ExtLen uint32，然后 Extensions
func encodeLog(l *raft.Log) []byte {
	appendedAtPresent := byte(0)
	var appendedAtNano int64
	if !l.AppendedAt.IsZero() {
		appendedAtPresent = 1
		appendedAtNano = l.AppendedAt.UnixNano()
	}
	headerLen := 8 + 8 + 1 + 1 + 8 + 4 + 4
	buf := make([]byte, 0, headerLen+len(l.Data)+len(l.Extensions))
	buf = binary.BigEndian.AppendUint64(buf, l.Index)
	buf = binary.BigEndian.AppendUint64(buf, l.Term)
	buf = append(buf, uint8(l.Type))
	buf = append(buf, appendedAtPresent)
	buf = binary.BigEndian.AppendUint64(buf, uint64(appendedAtNano))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(l.Data)))
	buf = append(buf, l.Data...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(l.Extensions)))
	buf = append(buf, l.Extensions...)
	return buf
}

// decodeLog 把落盘字节还原为 raft.Log，严格边界校验。
func decodeLog(b []byte, out *raft.Log) error {
	const minLen = 8 + 8 + 1 + 1 + 8 + 4 + 4
	if len(b) < minLen {
		return errors.New("consensus: truncated log entry")
	}
	out.Index = binary.BigEndian.Uint64(b[0:8])
	out.Term = binary.BigEndian.Uint64(b[8:16])
	out.Type = raft.LogType(b[16])
	present := b[17]
	nano := int64(binary.BigEndian.Uint64(b[18:26]))
	if present == 1 {
		out.AppendedAt = time.Unix(0, nano)
	} else {
		out.AppendedAt = time.Time{}
	}
	pos := 26
	dataLen := binary.BigEndian.Uint32(b[pos : pos+4])
	pos += 4
	if uint64(dataLen) > uint64(len(b)-pos) {
		return errors.New("consensus: inconsistent log data length")
	}
	out.Data = append([]byte(nil), b[pos:pos+int(dataLen)]...)
	pos += int(dataLen)
	if pos+4 > len(b) {
		return errors.New("consensus: truncated log extensions length")
	}
	extLen := binary.BigEndian.Uint32(b[pos : pos+4])
	pos += 4
	if uint64(extLen) > uint64(len(b)-pos) {
		return errors.New("consensus: inconsistent log extensions length")
	}
	out.Extensions = append([]byte(nil), b[pos:pos+int(extLen)]...)
	pos += int(extLen)
	if pos != len(b) {
		return errors.New("consensus: trailing bytes in log entry")
	}
	return nil
}
