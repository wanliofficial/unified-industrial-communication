// Package link 把「真实串口」与「串口服务器/DTU（TCP 透传）」统一成一条
// 带总线锁的链路。
//
// 存在理由：按数据标识/表号寻址的表计协议（DL/T 645、CJ/T 188）在现场有两
// 种同样常见的接法——一根 RS-485 直接进盒子，或者一个串口服务器把同一条
// 总线透传到以太网。两种接法的分帧、校验、命令序列完全一样，差别只有
// 「字节从哪来」。把这层差异放进每个驱动，就会出现「串口能读、串口服务器
// 读不到」这种只在某一种接法下复现的 bug。
//
// 锁的语义：
//   - 串口：锁是**整条物理总线**的（serialport.Ref 的 Port.mu），因为一条
//     RS-485 上挂着多台从站，事务之间必须互斥；
//   - TCP：没有物理总线，锁退化成每连接一把互斥量（一个连接对应一台设备
//     或一个透传口）。
//
// 两者都保证「排空 → 发送 → 收完帧」这整段不被另一个事务插入。
package link

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"uic/internal/driver"
	"uic/internal/model"
	"uic/internal/transport"
	"uic/internal/transport/serialport"
)

// Link 是驱动需要的最小链路能力：字节流 + 截止时间 + 排空 + 总线锁。
type Link interface {
	transport.Stream
	SetDeadline(t time.Time) error
	Drain() error
	Lock()
	Unlock()
}

// Options 是建链的协议侧参数。
type Options struct {
	// Proto 是串口共享层登记的分帧方式（"dlt645" / "cjt188" …）。
	//
	// 同一根物理线不可能同时跑两种分帧；共享层会用这个值拒绝静默复用。
	Proto string
	// DefaultPort 是 TCP 承载省略端口时补的端口（串口服务器常用 4001）。
	DefaultPort string
	// Opener 仅供测试注入内存串口；为 nil 时走真实串口。
	Opener serialport.Opener
}

// Open 按 transport.kind 建链。只接受 serial 与 tcp：
//
//   - serial：参数经 serialport 共享层按物理路径去重，同路径不同参数直接报错；
//   - tcp：连上即可。分帧与串口完全一致，因此内置 TCP 从站能直接当自检目标。
//
// 错误分类遵循驱动契约：参数不合法是 ErrUnsupported（配置问题，重试无用），
// 打开/连接失败是 ErrDisconnected（可重试）。
func Open(ctx context.Context, opt Options, t model.Transport, timeout time.Duration, devID string) (Link, error) {
	if opt.DefaultPort == "" {
		opt.DefaultPort = "4001"
	}
	switch t.Kind {
	case "serial":
		sc := transport.SerialFromTransport(t.Port, t.Baud, t.DataBits, t.Parity, t.StopBits)
		if err := sc.Validate(); err != nil {
			return nil, driver.E(driver.ErrUnsupported, "open", devID, err.Error(), false, err)
		}
		ref, err := serialport.Acquire(opt.Proto, sc, opt.Opener)
		if err != nil {
			// 打开串口失败通常是「设备被拔了/权限不对/参数被占用」，属可重试。
			return nil, driver.E(driver.ErrDisconnected, "open", devID,
				fmt.Sprintf("open serial %s failed", sc.Port), true, err)
		}
		return ref, nil
	case "tcp":
		addr := strings.TrimSpace(t.Address)
		if addr == "" {
			return nil, driver.E(driver.ErrUnsupported, "open", devID,
				"transport.address required (host[:port])", false, nil)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil && !strings.Contains(addr, ":") {
			addr = net.JoinHostPort(addr, opt.DefaultPort)
		}
		st, err := transport.DialTCP(ctx, addr, timeout)
		if err != nil {
			return nil, driver.E(driver.ErrDisconnected, "dial", devID,
				fmt.Sprintf("connect %s failed", addr), true, err)
		}
		return &tcpLink{st: st}, nil
	default:
		return nil, driver.E(driver.ErrUnsupported, "open", devID,
			fmt.Sprintf("requires serial or tcp transport, got %q", t.Kind), false, nil)
	}
}

// tcpLink 给串口服务器套上总线锁与 Drain 语义。
//
// Drain 在 TCP 上做不了「清空内核缓冲」；能做的只是把已到达的残留字节读掉。
// 这个区别值得写清楚：串口服务器上偶发的「读到上一轮应答」只能靠帧同步
// （codec.Reader 的扫描）兜底，而不是靠 tcflush。
type tcpLink struct {
	st transport.Stream
	mu sync.Mutex
}

func (t *tcpLink) Write(p []byte) error           { return t.st.Write(p) }
func (t *tcpLink) ReadFull(n int) ([]byte, error) { return t.st.ReadFull(n) }
func (t *tcpLink) Close() error                   { return t.st.Close() }
func (t *tcpLink) Lock()                          { t.mu.Lock() }
func (t *tcpLink) Unlock()                        { t.mu.Unlock() }

func (t *tcpLink) SetDeadline(dl time.Time) error {
	if s, ok := t.st.(transport.DeadlineSetter); ok {
		return s.SetDeadline(dl)
	}
	return nil
}

// Drain 是一次「尽力而为」的排空：把 1 毫秒内能读到的字节丢掉。
//
// 注意它会临时把读截止时间压到「现在 + 1ms」。调用方必须在 Drain **之后**
// 再设本次事务的截止时间；反过来会让截止时间被覆盖成过去时刻，症状是
// 「连上了，但每次写都 i/o timeout」。
func (t *tcpLink) Drain() error {
	if s, ok := t.st.(transport.DeadlineSetter); ok {
		_ = s.SetDeadline(time.Now().Add(time.Millisecond))
	}
	for i := 0; i < 64; i++ {
		if _, err := t.st.ReadFull(1); err != nil {
			break
		}
	}
	return nil
}
