// Package finsmem 是 FINS 内存区的从站侧实现：一块按内存区索引的字表，加上
// 「收到一帧 FINS 请求 → 返回一帧 FINS 应答」的处理逻辑。
//
// 抽出来的理由与主站侧的 finscore 相同：FINS/TCP 与 Host Link 的差别只在
// 信封，内存区语义必须一模一样。如果两个内置从站各写一份，很快就会出现
// 「同一个点表在 fins 模拟器上能写、在 hostlink 模拟器上越界」这种只在本机
// 复现的问题，而现场用的正是其中一种。
//
// 内存模型刻意简单：每个内存区一个 map，键是字地址；未配置地址读返回 0
// （真实 PLC 多数区也如此），写则落表。位读写落在对应的字上，因此「写
// CIO0.0 后读 CIO0 字」能看到最低位变化，与真实 PLC 的行为一致。
//
// 地址越界返回 0x1103（address out of range），区码不认识返回 0x1101
// （area type mismatch），命令太短返回 0x1002。这些是 PLC 真会回的码，
// 而不是「随便给个错」——主站的错误分类正是按这几个码做的，模拟器如果
// 总是静默成功，那些分类就永远没被测过。
package finsmem

import (
	"encoding/binary"
	"sync"

	finscodec "uic/internal/codec/fins"
)

// Mem 是一块可从多 goroutine 访问的 FINS 内存表。
type Mem struct {
	mu  sync.Mutex
	mem map[byte]map[uint16]uint16 // 按字访问码索引
}

// New 返回空表。
func New() *Mem { return &Mem{mem: map[byte]map[uint16]uint16{}} }

// SetWord 预置一个字的初值。
func (m *Mem) SetWord(area string, index int, v uint16) {
	a, ok := finscodec.Lookup(area)
	if !ok || index < 0 || index > 0xFFFF {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setWord(a.Word, uint16(index), v)
}

// GetWord 读取一个字。
func (m *Mem) GetWord(area string, index int) uint16 {
	a, ok := finscodec.Lookup(area)
	if !ok || index < 0 || index > 0xFFFF {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.word(a.Word, uint16(index))
}

// SetBit 预置一个位的初值（word 是字地址，bit 是 0..15）。
func (m *Mem) SetBit(area string, word, bit int, v bool) {
	a, ok := finscodec.Lookup(area)
	if !ok || word < 0 || word > 0xFFFF || bit < 0 || bit > 15 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.word(a.Word, uint16(word))
	if v {
		w |= 1 << uint(bit)
	} else {
		w &^= 1 << uint(bit)
	}
	m.setWord(a.Word, uint16(word), w)
}

// GetBit 读取一个位。
func (m *Mem) GetBit(area string, word, bit int) bool {
	a, ok := finscodec.Lookup(area)
	if !ok || word < 0 || word > 0xFFFF || bit < 0 || bit > 15 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.word(a.Word, uint16(word))&(1<<uint(bit)) != 0
}

func (m *Mem) word(code byte, addr uint16) uint16 {
	if mm := m.mem[code]; mm != nil {
		return mm[addr]
	}
	return 0
}

func (m *Mem) setWord(code byte, addr uint16, v uint16) {
	mm := m.mem[code]
	if mm == nil {
		mm = map[uint16]uint16{}
		m.mem[code] = mm
	}
	mm[addr] = v
}

// Respond 处理一帧 FINS 请求，返回一帧 FINS 应答。
//
// 入参与出参都是标准 FINS 帧（10 字节头 + 命令 + 数据），因此 FINS/TCP 与
// Host Link 两个从站都能直接复用，只需要各自做信封的拆装。
func (m *Mem) Respond(req finscodec.Frame) []byte {
	// 读和写的 MRC 都是 0x01，真正区分动作的是 SRC（0x01 读 / 0x02 写）。
	if req.MRC != finscodec.MRCRead || (req.SRC != finscodec.SRCRead && req.SRC != finscodec.SRCWrite) {
		return finscodec.BuildResponse(req, 0x0401, nil) // 未定义命令
	}
	if len(req.Data) < 6 {
		return finscodec.BuildResponse(req, 0x1002, nil) // 命令太短
	}
	code := req.Data[0]
	word := binary.BigEndian.Uint16(req.Data[1:3])
	bit := req.Data[3]
	count := binary.BigEndian.Uint16(req.Data[4:6])

	wordArea, isWord := finscodec.ByWordCode(code)
	_, isBit := finscodec.ByBitCode(code)
	if !isWord && !isBit {
		return finscodec.BuildResponse(req, 0x1101, nil)
	}
	if count == 0 {
		return finscodec.BuildResponse(req, 0x1102, nil)
	}

	// 位访问与字访问共用内存表：位码先映射到字码。
	memCode := code
	bitAccess := isBit
	if bitAccess {
		memCode, _ = finscodec.BitToWordCode(code)
	} else {
		memCode = wordArea.Word
	}
	if bitAccess && bit > 15 {
		return finscodec.BuildResponse(req, 0x1103, nil)
	}

	if req.SRC == finscodec.SRCRead {
		return m.handleRead(req, memCode, bitAccess, word, bit, count)
	}
	return m.handleWrite(req, memCode, bitAccess, word, bit, count)
}

func (m *Mem) handleRead(req finscodec.Frame, code byte, bitAccess bool, word uint16, bit byte, count uint16) []byte {
	if int(count) > finscodec.MaxBits || (!bitAccess && int(count) > finscodec.MaxWords) {
		return finscodec.BuildResponse(req, 0x1102, nil)
	}
	if bitAccess {
		if int(word)+(int(bit)+int(count)-1)/16 > 0xFFFF {
			return finscodec.BuildResponse(req, 0x1103, nil)
		}
	} else if int(word)+int(count)-1 > 0xFFFF {
		return finscodec.BuildResponse(req, 0x1103, nil)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if bitAccess {
		out := make([]byte, count)
		for i := 0; i < int(count); i++ {
			pos := int(bit) + i
			w := m.word(code, word+uint16(pos/16))
			if w&(1<<uint(pos%16)) != 0 {
				out[i] = 1
			}
		}
		return finscodec.BuildResponse(req, 0, out)
	}
	out := make([]byte, int(count)*2)
	for i := 0; i < int(count); i++ {
		binary.BigEndian.PutUint16(out[i*2:], m.word(code, word+uint16(i)))
	}
	return finscodec.BuildResponse(req, 0, out)
}

func (m *Mem) handleWrite(req finscodec.Frame, code byte, bitAccess bool, word uint16, bit byte, count uint16) []byte {
	if len(req.Data) < 6 {
		return finscodec.BuildResponse(req, 0x1002, nil)
	}
	data := req.Data[6:]
	if bitAccess {
		if len(data) != int(count) {
			return finscodec.BuildResponse(req, 0x1002, nil)
		}
	} else if len(data) != int(count)*2 {
		return finscodec.BuildResponse(req, 0x1002, nil)
	}
	if int(count) > finscodec.MaxBits || (!bitAccess && int(count) > finscodec.MaxWords) {
		return finscodec.BuildResponse(req, 0x1102, nil)
	}
	if bitAccess {
		if int(word)+(int(bit)+int(count)-1)/16 > 0xFFFF {
			return finscodec.BuildResponse(req, 0x1103, nil)
		}
	} else if int(word)+int(count)-1 > 0xFFFF {
		return finscodec.BuildResponse(req, 0x1103, nil)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if bitAccess {
		for i := 0; i < int(count); i++ {
			pos := int(bit) + i
			addr := word + uint16(pos/16)
			w := m.word(code, addr)
			if data[i] != 0 {
				w |= 1 << uint(pos%16)
			} else {
				w &^= 1 << uint(pos%16)
			}
			m.setWord(code, addr, w)
		}
		return finscodec.BuildResponse(req, 0, nil)
	}
	for i := 0; i < int(count); i++ {
		m.setWord(code, word+uint16(i), binary.BigEndian.Uint16(data[i*2:]))
	}
	return finscodec.BuildResponse(req, 0, nil)
}
