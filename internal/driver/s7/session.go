package s7

import (
	"context"
	"fmt"
	"time"

	"uic/internal/codec/s7"
	"uic/internal/driver"
)

// SessionOptions 描述一次「块服务」会话。
//
// 块服务不进入采集调度：它发生在组态、备份、排障时，一次可能传几 MB，
// 与周期采集的延迟目标完全冲突。因此这里单独开一条连接、单独一个会话，
// 用完即关。与驱动共用 conn 的握手与超时处理，避免「两套 S7 实现」。
type SessionOptions struct {
	Address string        // host:port
	Rack    int           // 机架 0..7
	Slot    int           // 槽位 0..31（S7-1200/1500 常为 1，S7-300/400 常为 2）
	Timeout time.Duration // 单次事务超时；默认 5s
	Dial    Dialer        // 测试注入；nil 表示真实 TCP
}

// Session 是一条只做块服务的 S7 连接。
//
// 并发契约与 driver.Conn 相同：方法内部串行，调用方不应并发调用。
type Session struct {
	c *conn
}

// 上限是防御性的，不是协议限制：真实块远小于这些值，但没有上限时一条
// 坏响应（长度字段被写成 99999）会让 CLI 无限追加内存。
const (
	maxUploadBytes  = 16 << 20
	maxUploadChunks = 1 << 16
)

// OpenSession 建立连接并完成 COTP + Setup 握手。
func OpenSession(ctx context.Context, opt SessionOptions) (*Session, error) {
	if opt.Address == "" {
		return nil, driver.E(driver.ErrUnsupported, "connect", "", "address required (host:port)", false, nil)
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if opt.Rack < 0 || opt.Rack > 7 {
		return nil, driver.E(driver.ErrUnsupported, "connect", opt.Address,
			fmt.Sprintf("rack %d out of range 0..7", opt.Rack), false, nil)
	}
	if opt.Slot < 0 || opt.Slot > 31 {
		return nil, driver.E(driver.ErrUnsupported, "connect", opt.Address,
			fmt.Sprintf("slot %d out of range 0..31", opt.Slot), false, nil)
	}
	dial := opt.Dial
	if dial == nil {
		dial = New().dial
	}
	stream, err := dial(ctx, opt.Address, opt.Timeout)
	if err != nil {
		return nil, driver.E(driver.ErrDisconnected, "dial", opt.Address,
			fmt.Sprintf("connect %s failed", opt.Address), true, err)
	}
	c := &conn{stream: stream, deviceID: opt.Address, timeout: opt.Timeout, pduLen: 480}
	if err := c.handshake(ctx, opt.Rack, opt.Slot); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return &Session{c: c}, nil
}

// Close 关闭会话。
func (s *Session) Close() error { return s.c.Close() }

// PDULen 返回协商后的 PDU 长度（块上传的分片大小由它推导）。
func (s *Session) PDULen() int { return s.c.pduLen }

// ListBlocks 返回七类块的数量。
func (s *Session) ListBlocks(ctx context.Context) ([]s7.BlockCount, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.roundTrip(ctx, s7.BuildListBlocks(c.nextRef()))
	if err != nil {
		return nil, err
	}
	ud, err := s7.ParseUserData(resp)
	if err != nil {
		return nil, s.protocol("list-blocks", err)
	}
	if ud.Group != s7.UserDataGroupBlocks || ud.Subfunc != s7.UserDataListAll {
		return nil, s.protocol("list-blocks", fmt.Errorf("unexpected response group=%d subfunc=%d",
			ud.Group, ud.Subfunc))
	}
	out, err := s7.ParseListBlocks(ud.Payload)
	if err != nil {
		return nil, s.protocol("list-blocks", err)
	}
	return out, nil
}

// ListBlocksOfType 返回某一类型的所有块号。
func (s *Session) ListBlocksOfType(ctx context.Context, blockType byte) ([]int, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.roundTrip(ctx, s7.BuildListBlocksOfType(c.nextRef(), blockType))
	if err != nil {
		return nil, err
	}
	ud, err := s7.ParseUserData(resp)
	if err != nil {
		return nil, s.protocol("list-blocks-of-type", err)
	}
	if ud.Group != s7.UserDataGroupBlocks || ud.Subfunc != s7.UserDataListBlocksOfType {
		return nil, s.protocol("list-blocks-of-type", fmt.Errorf("unexpected response group=%d subfunc=%d",
			ud.Group, ud.Subfunc))
	}
	out, err := s7.ParseListBlocksOfType(ud.Payload)
	if err != nil {
		return nil, s.protocol("list-blocks-of-type", err)
	}
	return out, nil
}

// BlockInfo 返回单个块的属性。
func (s *Session) BlockInfo(ctx context.Context, blockType byte, num int) (s7.BlockInfo, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()

	req, err := s7.BuildBlockInfo(c.nextRef(), blockType, num)
	if err != nil {
		return s7.BlockInfo{}, driver.E(driver.ErrAddress, "block-info", c.deviceID, err.Error(), false, err)
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return s7.BlockInfo{}, err
	}
	ud, err := s7.ParseUserData(resp)
	if err != nil {
		return s7.BlockInfo{}, s.protocol("block-info", err)
	}
	if ud.Group != s7.UserDataGroupBlocks || ud.Subfunc != s7.UserDataBlockInfo {
		return s7.BlockInfo{}, s.protocol("block-info", fmt.Errorf("unexpected response group=%d subfunc=%d",
			ud.Group, ud.Subfunc))
	}
	info, err := s7.ParseBlockInfo(ud.Payload)
	if err != nil {
		return s7.BlockInfo{}, s.protocol("block-info", err)
	}
	return info, nil
}

// UploadBlock 上传一个块的完整内容。
//
// 返回值是 PLC 给出的原始字节流，**包含块头**（真实 CPU 的第一片前面
// 有 36 字节紧凑块头）。不替调用方剥掉，是因为剥掉之后这份数据就不能
// 再用于下载；需要裸内容时由调用方按自己的格式处理。
//
// 三段式的收尾是强制的：Start 之后 PLC 会保留一个上传会话，不 End 就会
// 一直占着。因此即使中途失败也尽力补一条 End——失败路径不清理，反复
// 重试几次就可能把 PLC 的上传会话耗尽，而错误信息只会说「start failed」。
func (s *Session) UploadBlock(ctx context.Context, blockType byte, num int) (data []byte, err error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()

	req, err := s7.BuildStartUpload(c.nextRef(), blockType, num)
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "upload", c.deviceID, err.Error(), false, err)
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	start, err := s7.ParseStartUpload(resp)
	if err != nil {
		return nil, s.protocol("upload", err)
	}
	if start.Length > maxUploadBytes {
		return nil, s.protocol("upload", fmt.Errorf("block length %d exceeds limit %d", start.Length, maxUploadBytes))
	}

	defer func() {
		// 用独立的短超时收尾：调用方的 ctx 可能已经超时/取消，但 PLC 侧的
		// 会话仍然需要释放。
		ectx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		endResp, e := c.roundTrip(ectx, s7.BuildEndUpload(c.nextRef(), start.ID))
		if e != nil {
			if err == nil {
				err = fmt.Errorf("end upload: %w", e)
			}
			return
		}
		if e := s7.ParseEndUpload(endResp); e != nil && err == nil {
			err = s.protocol("end-upload", e)
		}
	}()

	buf := make([]byte, 0, start.Length)
	for i := 0; i < maxUploadChunks; i++ {
		resp, err := c.roundTrip(ctx, s7.BuildUpload(c.nextRef(), start.ID))
		if err != nil {
			return nil, err
		}
		frag, err := s7.ParseUpload(resp)
		if err != nil {
			return nil, s.protocol("upload", err)
		}
		if len(buf)+len(frag.Data) > maxUploadBytes {
			return nil, s.protocol("upload", fmt.Errorf("uploaded data exceeds limit %d", maxUploadBytes))
		}
		buf = append(buf, frag.Data...)
		if frag.Last {
			if start.Length > 0 && len(buf) != start.Length {
				return nil, s.protocol("upload", fmt.Errorf("uploaded %d bytes, block length says %d",
					len(buf), start.Length))
			}
			return buf, nil
		}
	}
	return nil, s.protocol("upload", fmt.Errorf("no final fragment after %d fragments", maxUploadChunks))
}

func (s *Session) protocol(op string, err error) error {
	return driver.E(driver.ErrProtocol, op, s.c.deviceID, err.Error(), true, err)
}
