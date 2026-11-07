// Package serialport 提供「按物理路径共享的串口」。
//
// 存在的理由：一条 RS-485 总线上通常挂多台从站，每台从站在配置里是独立
// 设备，各自走一遍 Open。如果驱动简单地「每设备 open 一次」，同一个
// /dev/ttyUSB0 会被打开多次，Linux 上两个 fd 同时读一个 tty 会互相偷
// 字节——表现是「单台跑得通，多台就随机超时」，而且完全不可复现。
//
// 因此物理串口按路径去重，引用计数复用，事务串行化交给 Port 的总线锁。
// 这一层只做「同一个 fd 被多家共享」这一件事，不含任何协议分帧：RTU、
// ASCII、DL/T 645、CJ/T 188、IEC 101 的分帧差异极大，放进来的话每个协议
// 都要在共享结构里加一个分支。
//
// 为什么不是每个驱动各写一份：这段逻辑的三个不变量（引用计数不能提前
// 关闭、参数冲突必须报错、分帧方式冲突必须报错）与协议无关，但每一条
// 写错都只在现场多设备场景下暴露。复制三份等于给自己留下三个只在凌晨
// 三点报警的 bug。
package serialport

import (
	"fmt"
	"sync"
	"time"

	"uic/internal/transport"
)

// Opener 打开串口，默认走 transport.OpenSerial；测试可注入内存实现。
type Opener func(cfg transport.SerialConfig) (transport.Stream, error)

// Port 是一个已经打开的物理串口，被同一路径上的所有设备共享。
type Port struct {
	cfg  transport.SerialConfig
	st   transport.Stream
	path string

	// proto 记录这根线上跑的是哪种分帧（rtu / ascii / dlt645 …）。同一根
	// 物理线不可能同时跑两种分帧：静默复用会让两种帧交错，症状是随机的
	// 校验错，几乎无法从日志判断。
	proto string
	refs  int

	// mu 是总线事务锁：从「清空接收缓冲」到「读完整帧」必须独占。半双工
	// 链路上一旦交错，收到的字节会来自另一台设备的响应。
	mu sync.Mutex
}

// Path 返回物理路径。
func (p *Port) Path() string { return p.path }

// Proto 返回这根线登记的协议标识。
func (p *Port) Proto() string { return p.proto }

// Refs 返回当前引用数，供诊断使用。
func (p *Port) Refs() int {
	mu.Lock()
	defer mu.Unlock()
	return p.refs
}

var (
	mu    sync.Mutex
	table = map[string]*Port{}
)

// Ref 是一条设备连接持有的一个引用。
//
// 它是 per-conn 的视图而不是共享对象：Close 必须只减一次引用计数，否则
// 两台设备先后 Close 会把仍在使用的端口关掉。
//
// Ref 实现了 transport.Stream（含 SetDeadline 与 Drain），因此可以像普通
// 串口一样被分帧层直接使用；额外的 Lock/Unlock 只用于「一次总线事务」
// 这种跨多次读写的临界区。
type Ref struct {
	port *Port
	once sync.Once
	err  error
}

// Acquire 取得（必要时打开）cfg.Port 对应的共享串口。
//
// proto 是分帧方式的标识（"rtu" / "ascii" / "dlt645" …）。同一路径已用
// 不同参数、或不同分帧方式打开时直接报错，而不是静默再开一个 fd：现场
// 把两个通道的波特率配错是常见事故，静默复用只会让故障挪到更难查的地方。
func Acquire(proto string, cfg transport.SerialConfig, opener Opener) (*Ref, error) {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opener == nil {
		opener = transport.OpenSerial
	}

	mu.Lock()
	defer mu.Unlock()

	if p, ok := table[cfg.Port]; ok {
		if p.cfg != cfg {
			return nil, fmt.Errorf("serial %s already open as %s, refusing to reopen as %s",
				cfg.Port, p.cfg, cfg)
		}
		if p.proto != proto {
			return nil, fmt.Errorf("serial %s already open for %s framing, refusing to open for %s",
				cfg.Port, p.proto, proto)
		}
		p.refs++
		return &Ref{port: p}, nil
	}

	st, err := opener(cfg)
	if err != nil {
		return nil, err
	}
	p := &Port{cfg: cfg, st: st, path: cfg.Port, proto: proto, refs: 1}
	table[cfg.Port] = p
	return &Ref{port: p}, nil
}

// Write 写整包字节。
func (r *Ref) Write(p []byte) error { return r.port.st.Write(p) }

// ReadFull 读满 n 字节或超时。
func (r *Ref) ReadFull(n int) ([]byte, error) { return r.port.st.ReadFull(n) }

// SetDeadline 把设备超时传到底层串口。
func (r *Ref) SetDeadline(t time.Time) error {
	if s, ok := r.port.st.(transport.DeadlineSetter); ok {
		return s.SetDeadline(t)
	}
	return nil
}

// Drain 丢弃接收缓冲中的残留字节。
func (r *Ref) Drain() error {
	if f, ok := r.port.st.(transport.Flusher); ok {
		return f.Drain()
	}
	return nil
}

// Lock 取得总线事务锁。调用方必须保证配对 Unlock，且临界区内不做与本次
// 事务无关的事情（例如等外部信号）——总线被占住时同一条线上的所有设备
// 都在排队。
func (r *Ref) Lock() { r.port.mu.Lock() }

// Unlock 释放总线事务锁。
func (r *Ref) Unlock() { r.port.mu.Unlock() }

// Close 释放一次引用；最后一个使用者负责真正关闭 fd。幂等。
func (r *Ref) Close() error {
	r.once.Do(func() {
		mu.Lock()
		p := r.port
		p.refs--
		last := p.refs <= 0
		if last {
			delete(table, p.path)
		}
		mu.Unlock()
		if last {
			r.err = p.st.Close()
		}
	})
	return r.err
}

// Count 返回当前打开的物理串口数，供测试与诊断使用。
func Count() int {
	mu.Lock()
	defer mu.Unlock()
	return len(table)
}
