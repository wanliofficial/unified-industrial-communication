package modbus

import (
	"fmt"
	"sync"
	"time"

	"uic/internal/transport"
)

// 串口注册表。
//
// 存在的理由：一条 RS-485 总线上通常挂多台从站，每台从站在配置里是独立
// 设备，各自走一遍 Open。如果驱动简单地「每设备 open 一次」，同一个
// /dev/ttyUSB0 会被打开多次，Linux 上两个 fd 同时读一个 tty 会互相偷
// 字节——表现是「单台跑得通，多台就随机超时」，而且完全不可复现。
//
// 因此物理串口按路径去重，引用计数复用，事务串行化交给 sharedSerial.mu。
var (
	portMu    sync.Mutex
	portTable = map[string]*sharedSerial{}
)

// sharedSerial 是一个已经打开的物理串口，被同一路径上的所有设备共享。
type sharedSerial struct {
	cfg transport.SerialConfig
	st  transport.Stream
	id  string
	// proto 记录这根线上跑的是哪种分帧（rtu / ascii）。同一根物理线
	// 不可能同时跑两种分帧：静默复用会让两种帧交错，症状是随机的
	// CRC/LRC 错，几乎无法从日志判断。
	proto string
	refs  int

	// mu 是总线事务锁：从「清空接收缓冲」到「读完整帧」必须独占。
	// 半双工链路上一旦交错，收到的字节会来自另一台设备的响应。
	mu sync.Mutex
}

// portRef 是一条设备连接持有的一个引用。
//
// 它是 per-conn 的视图而不是共享对象：Close 必须只减一次引用计数，
// 否则两台设备先后 Close 会把仍在使用的端口关掉。
type portRef struct {
	port *sharedSerial
	once sync.Once
	err  error
}

func (r *portRef) Write(p []byte) error { return r.port.st.Write(p) }

func (r *portRef) ReadFull(n int) ([]byte, error) { return r.port.st.ReadFull(n) }

// SetDeadline 把设备超时传到底层串口。
func (r *portRef) SetDeadline(t time.Time) error {
	if s, ok := r.port.st.(transport.DeadlineSetter); ok {
		return s.SetDeadline(t)
	}
	return nil
}

// Drain 丢弃接收缓冲中的残留字节。
func (r *portRef) Drain() error {
	if f, ok := r.port.st.(transport.Flusher); ok {
		return f.Drain()
	}
	return nil
}

// Close 释放一次引用；最后一个使用者负责真正关闭 fd。幂等。
func (r *portRef) Close() error {
	r.once.Do(func() {
		portMu.Lock()
		p := r.port
		p.refs--
		last := p.refs <= 0
		if last {
			delete(portTable, p.id)
		}
		portMu.Unlock()
		if last {
			r.err = p.st.Close()
		}
	})
	return r.err
}

// acquirePort 取得（必要时打开）cfg.Port 对应的共享串口。
//
// 同一路径已用不同参数打开时直接报错，而不是静默再开一个 fd：现场把
// 两个通道的波特率配错是常见事故，静默复用只会让故障挪到更难查的地方。
func acquirePort(proto string, cfg transport.SerialConfig, opener SerialOpener) (*portRef, error) {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opener == nil {
		opener = transport.OpenSerial
	}

	portMu.Lock()
	defer portMu.Unlock()

	if p, ok := portTable[cfg.Port]; ok {
		if p.cfg != cfg {
			return nil, fmt.Errorf("serial %s already open as %s, refusing to reopen as %s",
				cfg.Port, p.cfg, cfg)
		}
		if p.proto != proto {
			return nil, fmt.Errorf("serial %s already open for %s framing, refusing to open for %s",
				cfg.Port, p.proto, proto)
		}
		p.refs++
		return &portRef{port: p}, nil
	}

	st, err := opener(cfg)
	if err != nil {
		return nil, err
	}
	p := &sharedSerial{cfg: cfg, st: st, id: cfg.Port, proto: proto, refs: 1}
	portTable[cfg.Port] = p
	return &portRef{port: p}, nil
}

// openPortCount 返回当前打开的物理串口数，供测试与诊断使用。
func openPortCount() int {
	portMu.Lock()
	defer portMu.Unlock()
	return len(portTable)
}
