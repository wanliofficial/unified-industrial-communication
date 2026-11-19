// Package finscore 是「FINS 命令语义 + 内存区读写」的共享实现。
//
// 它只认 FINS 帧，不认信封：FINS/TCP 用二进制定长封套与节点号协商，Host
// Link 用可打印 ASCII 封套与异或校验，两者对上层暴露的行为完全一致——都是
// 「发一帧、收一帧、拿到已解析的应答」。把这一层抽出来，两个驱动就不必各自
// 维护一份近五百行的内存区读写逻辑（区名解析、字/位混排、跨字拆块、字节序、
// 错误码到点位质量的映射）。两份代码分头演化的结局通常是：一个驱动修好的
// 边界，另一个驱动仍然错着，而且错法一模一样难查。
//
// 三条分层规矩：
//
//  1. **信封实现（Transactor）负责一切与传输有关的判断**：帧怎么封、超时怎么
//     设、SID 怎么分配、应答怎么校验。finscore 不问「你用的是哪种传输」。
//  2. **分块上限由信封实现声明**：FINS/TCP 一次能读 999 字，Host Link 只能
//     读 269 字（整帧一千多字符的硬限）。写死一个「都取 240」的保守值虽然
//     能跑，却会让以太网侧的白白多花一倍事务；写死 999 则会让串口侧稳定收
//     帧长错。
//  3. **错误分类沿用驱动契约**：信封层抛出的错误必须已经带 driver.Kind，
//     finscore 只补「点位质量」这一层映射。唯一的例外是信封层的噪声/串话
//     类错误，它们实现 CommError，直接记为通信错误。
package finscore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	finscodec "uic/internal/codec/fins"
	"uic/internal/codec/value"
	"uic/internal/driver"
	"uic/internal/model"
)

// Transactor 是一个信封实现：把 FINS 帧送出去，把应答拿回来。
//
// 契约：
//   - Header 返回本次事务使用的 FINS 请求帧头（节点地址由信封决定：
//     FINS/TCP 用协商出来的节点号，Host Link 用 0）；
//   - RoundTrip 负责分配并校验 SID、设置超时、丢弃噪声帧；它返回的
//     FINS 帧的 EndCode 已解析但**不判定**（非 0 结束码的语义由调用方分类）；
//   - RoundTrip 返回的错误必须带 driver.Kind；
//   - 同一个 Transactor 上的调用是串行的（finscore 已按契约串行使用）。
type Transactor interface {
	Header() finscodec.Header
	RoundTrip(ctx context.Context, req []byte) (finscodec.Frame, error)
	Close() error
}

// Limits 声明信封对单次事务的元素上限。
//
// 四个值都要给：读和写的约束来自不同的地方。Host Link 的读受应答帧长约束
// （数据 1076 字符），写受命令帧长约束（正文 1080 字符再减去 12 字符参数），
// 于是读 269 字、写 267 字，差的这两字不是笔误。
type Limits struct {
	WordsPerRead  int
	WordsPerWrite int
	BitsPerRead   int
	BitsPerWrite  int
}

// DefaultLimits 是 FINS/TCP 的上限：单帧 2012 字节负载，字读 480、位读
// 1920 都是留了余量的保守值（协议上限分别是 999 与 1998）。
func DefaultLimits() Limits {
	return Limits{WordsPerRead: 480, WordsPerWrite: 480, BitsPerRead: 1920, BitsPerWrite: 1920}
}

// normalize 把未设置（<=0）或超出协议上限的值夹到合法区间。
//
// 夹紧而不是报错：上限是性能参数，配置写错不该让设备起不来，但也不能让
// 它越界到对端拒收。
func (l Limits) normalize() Limits {
	d := DefaultLimits()
	if l.WordsPerRead <= 0 || l.WordsPerRead > finscodec.MaxWords {
		l.WordsPerRead = d.WordsPerRead
	}
	if l.WordsPerWrite <= 0 || l.WordsPerWrite > finscodec.MaxWords {
		l.WordsPerWrite = d.WordsPerWrite
	}
	if l.BitsPerRead <= 0 || l.BitsPerRead > finscodec.MaxBits {
		l.BitsPerRead = d.BitsPerRead
	}
	if l.BitsPerWrite <= 0 || l.BitsPerWrite > finscodec.MaxBits {
		l.BitsPerWrite = d.BitsPerWrite
	}
	return l
}

// Transactor 是一个信封实现：把 FINS 帧送出去，把应答拿回来。
func New(cfg driver.OpenConfig, tr Transactor, lim Limits) *Conn {
	return &Conn{cfg: cfg, tr: tr, lim: lim.normalize(), devID: cfg.Device.ID}
}

// CommError 由信封层的错误实现，表示「这次失败源于线路而不是点位」：
// 校验和不符、单元号串话、迟到的应答。它们的正确质量是通信错误——把
// 噪声记成「坏值」，运维会去查点表而不是查终端电阻。
type CommError interface {
	Comm() bool
}

// Conn 实现 driver.Conn 的读/写/探活/关闭。
type Conn struct {
	cfg   driver.OpenConfig
	tr    Transactor
	lim   Limits
	devID string
	// wmu 保护「读字 → 改位 → 写字」这类复合写入。按契约同一 Conn 不会被
	// 并发调用；加锁是为了在错误使用下宁可慢一点，也不要静默丢位。
	wmu sync.Mutex
}

// Probe 读一个字验证链路与地址语义。
//
// 读 DM0 而不是读状态区：DM 是最常见、也几乎一定存在的区，且不依赖 PLC
// 的程序里有任何特定逻辑。
func (c *Conn) Probe(ctx context.Context) error {
	_, err := c.readWords(ctx, "dm", 0, 1)
	return err
}

// Close 关闭底层链路。
func (c *Conn) Close() error { return c.tr.Close() }

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// Read 读取一组 span。单个 span 失败不影响其它 span：现场最需要的正是
// 「哪几个点坏了」而不是「整批都没了」。
//
// 部分失败语义见 docs §3.3.1：**有任意一个点位成功就不向上返回错误**，
// 全失败才把第一个错误交出去；链路级失败（超时/断连）是这条闸门的例外，
// 必须立即收尾并上抛，即便本轮已经有好样本。
func (c *Conn) Read(ctx context.Context, spans []driver.ReadSpan) ([]driver.RawSample, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	var out []driver.RawSample
	var firstErr error
	good := 0
	for si, span := range spans {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "read", c.devID, "context done", true, err)
			// 剩余 span 的点位必须显式标坏，不能让它们从结果里消失——消失
			// 的点位在采集层表现为「没有新样本」，会被当成设备安静（甚至
			// 被兜底成 protocol 错），把真正的超时原因盖掉。
			for _, rest := range spans[si:] {
				out = append(out, c.failed(rest, e)...)
			}
			return out, e
		}
		samples, err := c.readSpan(ctx, span)
		if err != nil {
			// 链路级失败：立即返回，不由下面的闸门吞掉——一轮采集被周期
			// 超时打断本身就是「设备太慢」的信号，需要让调度器看到。
			if driver.IsLinkFailure(err) {
				// 前面几块已经读回来的好点位必须保留（§3.3.1；与 SNMP/ENIP
				// 的既有处理一致）。整 span 回填成同一条错误会把「设备正常、
				// 只是某一块坏了」放大成整段数据丢失。
				out = append(out, c.withPendingFailed(span, samples, err)...)
				for _, rest := range spans[si+1:] {
					out = append(out, c.failed(rest, err)...)
				}
				return out, err
			}
			if firstErr == nil {
				firstErr = err
			}
			// 同理：只把没拿到值的点位标坏，已解出的好样本保留。
			out = append(out, c.withPendingFailed(span, samples, err)...)
			continue
		}
		// span 成功不等于「有成功的点位」：解码阶段的错误（EM 当前区的 bool
		// 缺位号、类型与宽度不符）是逐点记在样本上的，readSpan 仍然返回
		// nil error。按 span 计数会把这批「全员失败」当成成功，`Read` 于是
		// 返回 nil error——正是 §3.3.1 点名禁止的那种静默。
		if n := goodSamples(samples); n > 0 {
			good += n
		} else if e := firstSampleErr(samples); e != nil && firstErr == nil {
			firstErr = e
		}
		out = append(out, samples...)
	}
	// 部分失败语义（docs §3.3.1）：有成功点就不向上返回错误。
	return out, driver.PartialReadError(good, firstErr)
}

// goodSamples 数一段结果里真正带值的点位。失败样本一律带 Err（见 errSample /
// failed），因此「Err == nil」就是「这个点位读到了值」。
func goodSamples(samples []driver.RawSample) int {
	n := 0
	for _, s := range samples {
		if s.Err == nil {
			n++
		}
	}
	return n
}

// firstSampleErr 返回一段结果里的第一个点位级错误，供「整段全失败但 span
// 本身没报错」时喂给部分失败闸门。
func firstSampleErr(samples []driver.RawSample) error {
	for _, s := range samples {
		if s.Err != nil {
			return s.Err
		}
	}
	return nil
}

// readSpan 处理一个 span。与 MC 不同，FINS 的同一个内存区可以同时有字点位
// 和位点位（例如 DM100 字 + DM100.3 位），所以这里按访问方式拆成两次事务，
// 再按原始顺序合并结果。
func (c *Conn) readSpan(ctx context.Context, span driver.ReadSpan) ([]driver.RawSample, error) {
	if len(span.Points) == 0 {
		return nil, nil
	}
	area, ok := finscodec.Lookup(span.Points[0].Address.Area)
	if !ok {
		return nil, c.areaError("read", span.Points[0].Address.Area)
	}
	for _, p := range span.Points {
		a, ok := finscodec.Lookup(p.Address.Area)
		if !ok || a.Key != area.Key {
			return nil, driver.E(driver.ErrInternal, "read", c.devID,
				"span mixes FINS memory areas", false, nil)
		}
	}

	out := make([]driver.RawSample, len(span.Points))
	wordIdx := make([]int, 0, len(span.Points))
	bitIdx := make([]int, 0, len(span.Points))
	bitAccess, bitErr := finscodec.BitAccess(area)

	for i, p := range span.Points {
		if p.DataType == model.TypeBool && bitErr == nil {
			if p.Address.Bit < 0 || p.Address.Bit > 15 {
				out[i] = c.errSample(p.ID, driver.E(driver.ErrAddress, "read", c.devID,
					fmt.Sprintf("bool point on %s needs address.bit 0..15", area.Key), false, nil))
				continue
			}
			bitIdx = append(bitIdx, i)
			continue
		}
		wordIdx = append(wordIdx, i)
	}

	if len(wordIdx) > 0 {
		if err := c.readWordPoints(ctx, span, area, wordIdx, out); err != nil {
			return out, err
		}
	}
	if len(bitIdx) > 0 {
		if err := c.readBitPoints(ctx, span, area, bitAccess, bitIdx, out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (c *Conn) readWordPoints(ctx context.Context, span driver.ReadSpan, area finscodec.Area, idx []int, out []driver.RawSample) error {
	min, max := wordBounds(span, idx)
	for start := min; start < max; {
		end := start + c.lim.WordsPerRead
		if end > max {
			end = max
		}
		// 一次请求必须覆盖「起点落在本次范围内」的每个点位的完整宽度，否则
		// 解码时那段字节还没读回来。宽度超过单次上限的点只能整块读：拆开
		// 读就无法把它们拼回一个值（RawSample 是一个点位一个值）。
		end = widenToPoints(span, idx, start, end)
		data, err := c.readWords(ctx, area.Key, start, end-start)
		if err != nil {
			return err
		}
		for _, i := range idx {
			p := span.Points[i]
			off := p.Address.Offset
			if off < start || off >= end {
				continue
			}
			out[i] = c.decodeWordPoint(p, area, start, data)
		}
		start = end
	}
	return nil
}

// widenToPoints 把一次读的结束地址扩展到「所有起点在 [start,end) 内的点位
// 的完整宽度」为止。
//
// 举一个具体的例子：limit 是 267 字，点表里有一个 300 字的字符串点位。按
// 上限切第一块是 0..267，该点位起点在内、宽度 300，于是块尾推到 300，一次
// 读 300 字——它超过了 Host Link 的帧长预算，会得到一条明确的「帧太长」错误，
// 而不是「响应比请求短」这种看起来像对端 bug 的报错。
func widenToPoints(span driver.ReadSpan, idx []int, start, end int) int {
	for changed := true; changed; {
		changed = false
		for _, i := range idx {
			p := span.Points[i]
			off := p.Address.Offset
			if off < start || off >= end {
				continue
			}
			if w := off + pointWidth(p); w > end {
				end = w
				changed = true
			}
		}
	}
	return end
}

// pointWidth 返回一个点位占的字数。
func pointWidth(p driver.PointRef) int {
	if p.DataType == model.TypeBool {
		return 1
	}
	if p.Length > 0 {
		return p.Length
	}
	return registerWidth(p.DataType)
}

func (c *Conn) readBitPoints(ctx context.Context, span driver.ReadSpan, area finscodec.Area, acc finscodec.Access, idx []int, out []driver.RawSample) error {
	min, max := bitBounds(span, idx)
	// 一次读整字范围：请求的位从 min 字的 bit0 开始，数量是字数*16。这样
	// 不需要为每个点单独算起始位，也保证同一字上的多个点位共享一次事务。
	wordsPerRead := c.lim.BitsPerRead / 16
	if wordsPerRead < 1 {
		wordsPerRead = 1
	}
	for start := min; start < max; {
		end := start + wordsPerRead
		if end > max {
			end = max
		}
		count := (end - start) * 16
		data, err := c.readBits(ctx, acc, start, 0, count)
		if err != nil {
			return err
		}
		for _, i := range idx {
			p := span.Points[i]
			off := p.Address.Offset
			if off < start || off >= end {
				continue
			}
			pos := (off-start)*16 + p.Address.Bit
			if pos < 0 || pos >= len(data) {
				out[i] = c.errSample(p.ID, driver.E(driver.ErrProtocol, "decode", c.devID,
					"response shorter than requested bits", false, nil))
				continue
			}
			out[i] = driver.RawSample{
				PointID: p.ID,
				Value:   model.BoolValue(data[pos] != 0),
				Quality: model.QualityGood,
			}
		}
		start = end
	}
	return nil
}

func wordBounds(span driver.ReadSpan, idx []int) (int, int) {
	min, max := span.Points[idx[0]].Address.Offset, span.Points[idx[0]].Address.Offset
	for _, i := range idx {
		p := span.Points[i]
		w := p.Length
		if w <= 0 {
			w = registerWidth(p.DataType)
		}
		if p.DataType == model.TypeBool {
			w = 1
		}
		if p.Address.Offset < min {
			min = p.Address.Offset
		}
		if e := p.Address.Offset + w; e > max {
			max = e
		}
	}
	return min, max
}

func bitBounds(span driver.ReadSpan, idx []int) (int, int) {
	min, max := span.Points[idx[0]].Address.Offset, span.Points[idx[0]].Address.Offset
	for _, i := range idx {
		off := span.Points[i].Address.Offset
		if off < min {
			min = off
		}
		if off+1 > max {
			max = off + 1
		}
	}
	return min, max
}

func (c *Conn) decodeWordPoint(p driver.PointRef, area finscodec.Area, start int, data []byte) driver.RawSample {
	idx := p.Address.Offset - start
	width := p.Length
	if p.DataType == model.TypeBool {
		width = 1
	}
	if width <= 0 {
		width = registerWidth(p.DataType)
	}
	byteOff := idx * 2
	if byteOff+width*2 > len(data) {
		return c.errSample(p.ID, driver.E(driver.ErrProtocol, "decode", "",
			"response shorter than requested words", false, nil))
	}
	raw := data[byteOff : byteOff+width*2]

	if p.DataType == model.TypeBool {
		// 走到这里说明该区没有直接的位访问码（当前 EM 区），只能从整字里
		// 取位。
		if p.Address.Bit < 0 || p.Address.Bit > 15 {
			return c.errSample(p.ID, driver.E(driver.ErrAddress, "decode", "",
				fmt.Sprintf("bool point on %s needs address.bit 0..15", area.Key), false, nil))
		}
		word := binary.BigEndian.Uint16(raw)
		return driver.RawSample{
			PointID: p.ID,
			Value:   model.BoolValue(word&(1<<uint(p.Address.Bit)) != 0),
			Quality: model.QualityGood,
		}
	}

	v, err := value.Decode(raw, p.DataType, OrderFor(width*2, p.ByteOrder))
	if err != nil {
		return c.errSample(p.ID, driver.E(driver.ErrProtocol, "decode", p.ID, err.Error(), false, err))
	}
	return driver.RawSample{PointID: p.ID, Value: v, Quality: model.QualityGood}
}

// OrderFor 返回多字数值的字节序。FINS 的原生布局是「每个字大端、低字在前」：
// 32 位值对应 CDAB，64 位值对应 DCBA（整体倒序）。用户显式配置时以配置为准。
//
// 这条默认值对 Host Link 同样成立——它换的只是信封，内存区的字节序是 PLC
// 一侧的事实，与怎么封帧无关。
func OrderFor(n int, order model.ByteOrder) model.ByteOrder {
	if order != "" {
		return order
	}
	if n >= 8 {
		return model.OrderDCBA
	}
	return model.OrderCDAB
}

func registerWidth(dt model.DataType) int {
	if n := dt.Registers(); n > 0 {
		return n
	}
	return 1
}

func (c *Conn) readWords(ctx context.Context, areaKey string, word, count int) ([]byte, error) {
	area, ok := finscodec.Lookup(areaKey)
	if !ok {
		return nil, c.areaError("read", areaKey)
	}
	frame, err := finscodec.BuildMemoryRead(c.tr.Header(), finscodec.WordAccess(area),
		uint16(word), 0, uint16(count))
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
	}
	resp, err := c.tr.RoundTrip(ctx, frame)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) < count*2 {
		return nil, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("short read response: %d bytes for %d words", len(resp.Data), count), false, nil)
	}
	return resp.Data[:count*2], nil
}

func (c *Conn) readBits(ctx context.Context, acc finscodec.Access, word int, bit byte, count int) ([]byte, error) {
	frame, err := finscodec.BuildMemoryRead(c.tr.Header(), acc, uint16(word), bit, uint16(count))
	if err != nil {
		return nil, driver.E(driver.ErrAddress, "read", c.devID, err.Error(), false, err)
	}
	resp, err := c.tr.RoundTrip(ctx, frame)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) < count {
		return nil, driver.E(driver.ErrProtocol, "read", c.devID,
			fmt.Sprintf("short bit response: %d bytes for %d bits", len(resp.Data), count), false, nil)
	}
	return resp.Data[:count], nil
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// Write 逐条写入。单条失败不影响后续：批量点位里一条地址越界，不该让其余
// 几十条一起失败。
//
// 返回的 `results` 必须**逐项**回答（docs §3.4）：成功的条目不能因为同一批里
// 另一条失败而被回填上错误——调用方据此决定「哪几条要重发/要告警」，把成功
// 项也标上 Err 会让运维对着一个实际已写下去的点位反复排查。
func (c *Conn) Write(ctx context.Context, reqs []driver.WriteRequest) ([]driver.WriteResult, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	results := make([]driver.WriteResult, 0, len(reqs))
	var firstErr error
	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			e := driver.E(driver.ErrTimeout, "write", c.devID, "context done", true, err)
			// 剩余条目必须显式标坏，不能让它们从结果里消失。
			for _, rest := range reqs[i:] {
				results = append(results, c.writeFail(rest, e))
			}
			return results, e
		}
		res, err := c.writeOne(ctx, req)
		results = append(results, res)
		if err != nil {
			// 链路级失败：后续每条都只会等满一次超时，必须短路并把剩余条目
			// 显式标坏（对比 ENIP/其它驱动「标失败并 break」）。地址/权限/不支持
			// 是点级问题，继续写下一条。
			if driver.IsLinkFailure(err) {
				for _, rest := range reqs[i+1:] {
					results = append(results, c.writeFail(rest, err))
				}
				return results, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return results, firstErr
}

func (c *Conn) writeOne(ctx context.Context, req driver.WriteRequest) (driver.WriteResult, error) {
	area, ok := finscodec.Lookup(req.Address.Area)
	if !ok {
		err := c.areaError("write", req.Address.Area)
		return c.writeFail(req, err), err
	}
	if req.DataType == model.TypeBool {
		if req.Address.Bit < 0 || req.Address.Bit > 15 {
			err := driver.E(driver.ErrAddress, "write", c.devID,
				fmt.Sprintf("bool point on %s needs address.bit 0..15", area.Key), false, nil)
			return c.writeFail(req, err), err
		}
		if acc, err := finscodec.BitAccess(area); err == nil {
			return c.writeBit(ctx, req, acc)
		}
		return c.writeWordBit(ctx, req, area)
	}
	return c.writeWords(ctx, req, area)
}

func (c *Conn) writeBit(ctx context.Context, req driver.WriteRequest, acc finscodec.Access) (driver.WriteResult, error) {
	data := []byte{0}
	if req.Value.Bool {
		data[0] = 1
	}
	return c.write(ctx, req, acc, req.Address.Offset, byte(req.Address.Bit), 1, data)
}

func (c *Conn) writeWordBit(ctx context.Context, req driver.WriteRequest, area finscodec.Area) (driver.WriteResult, error) {
	// 读整字 → 只改目标位 → 写整字：邻位属于别的点位，不能被清零。
	data, err := c.readWords(ctx, area.Key, req.Address.Offset, 1)
	if err != nil {
		return c.writeFail(req, err), err
	}
	if len(data) < 2 {
		err := driver.E(driver.ErrProtocol, "write", c.devID, "short word read", false, nil)
		return c.writeFail(req, err), err
	}
	word := binary.BigEndian.Uint16(data)
	if req.Value.Bool {
		word |= 1 << uint(req.Address.Bit)
	} else {
		word &^= 1 << uint(req.Address.Bit)
	}
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, word)
	return c.write(ctx, req, finscodec.WordAccess(area), req.Address.Offset, 0, 1, payload)
}

func (c *Conn) writeWords(ctx context.Context, req driver.WriteRequest, area finscodec.Area) (driver.WriteResult, error) {
	words := reqWords(req)
	if words <= 0 {
		err := driver.E(driver.ErrProtocol, "write", c.devID,
			fmt.Sprintf("point %q has no width", req.PointID), false, nil)
		return c.writeFail(req, err), err
	}
	var payload []byte
	if req.DataType == model.TypeString || req.DataType == model.TypeBytes {
		// 字符串/字节：按地址宽度补零或截断。FINS 每个字存两个字符，字内是
		// 大端（"AB" → 0x4142），不需要额外交换。
		//
		// 超长**拒绝**而不是静默截断：copy 会在超出 words*2 时悄悄丢字符，
		// 调用方以为整个值都写下去了（§3.4 规则 3「写入失败不得静默」）。与
		// 下面数值分支的越界检查一致。
		if len(req.Value.Text) > words*2 {
			err := driver.E(driver.ErrAddress, "write", c.devID,
				fmt.Sprintf("point %q: value is %d bytes but address.length is %d words (%d bytes)",
					req.PointID, len(req.Value.Text), words, words*2), false, nil)
			return c.writeFail(req, err), err
		}
		raw := make([]byte, words*2)
		copy(raw, []byte(req.Value.Text))
		payload = raw
	} else {
		if err := checkRange(req.DataType, req.Value.Num); err != nil {
			err = driver.E(driver.ErrAddress, "write", c.devID,
				fmt.Sprintf("point %q: %v", req.PointID, err), false, err)
			return c.writeFail(req, err), err
		}
		raw, err := value.Encode(req.Value, req.DataType, OrderFor(words*2, req.ByteOrder))
		if err != nil {
			err = driver.E(driver.ErrProtocol, "write", c.devID, err.Error(), false, err)
			return c.writeFail(req, err), err
		}
		if len(raw) > words*2 {
			err := driver.E(driver.ErrAddress, "write", c.devID,
				fmt.Sprintf("point %q needs %d bytes but address has %d", req.PointID, len(raw), words*2), false, nil)
			return c.writeFail(req, err), err
		}
		payload = append(raw, make([]byte, words*2-len(raw))...)
	}
	return c.writeChunked(ctx, req, finscodec.WordAccess(area), req.Address.Offset, words, payload)
}

// writeChunked 按信封上限把一次字写拆成多帧。
//
// 为什么要拆而不是直接报「太长」：字符串点位很容易超过 267 字（534 字符）。
// 在以太网上一次写 999 字本来也是合法的，只有窄信封才需要拆；把拆分放在
// 这里，两个驱动共用一份代码，也不会出现「以太网能写、串口不能写」的
// 行为差异。
func (c *Conn) writeChunked(ctx context.Context, req driver.WriteRequest, acc finscodec.Access, word, words int, data []byte) (driver.WriteResult, error) {
	limit := c.lim.WordsPerWrite
	for done := 0; done < words; {
		n := words - done
		if n > limit {
			n = limit
		}
		chunk := data[done*2 : (done+n)*2]
		if _, err := c.write(ctx, req, acc, word+done, 0, n, chunk); err != nil {
			return c.writeFail(req, err), err
		}
		done += n
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

func (c *Conn) write(ctx context.Context, req driver.WriteRequest, acc finscodec.Access, word int, bit byte, count int, data []byte) (driver.WriteResult, error) {
	frame, err := finscodec.BuildMemoryWrite(c.tr.Header(), acc, uint16(word), bit, uint16(count), data)
	if err != nil {
		err = driver.E(driver.ErrAddress, "write", c.devID, err.Error(), false, err)
		return c.writeFail(req, err), err
	}
	if _, err := c.tr.RoundTrip(ctx, frame); err != nil {
		return c.writeFail(req, err), err
	}
	return driver.WriteResult{PointID: req.PointID, OK: true, Quality: model.QualityGood}, nil
}

// reqWords 计算一次写入占的字数。写请求不带点位定义，长度只能从地址与数据
// 类型自身推断：显式给了 address.length 就用它，否则按类型宽度。
func reqWords(req driver.WriteRequest) int {
	if req.Address.Length > 0 {
		return req.Address.Length
	}
	if n := req.DataType.Registers(); n > 0 {
		return n
	}
	return 1
}

// checkRange 拒绝越界与带小数的整数写入，以及 float32 溢出成 ±Inf / 下溢成 0。
//
// codec/value 的 Encode 用强制转换落地：70000 写进 INT16 会静默变成 4464，
// 1e300 写进 REAL 会静默变成 +Inf，1e-50 会静默变成 0。写入「成功」了，但
// 设备上的值已经不是调用方给的值。这与 ADS/DNP3/ENIP 的既有处理一致，必须在
// 发报文之前拦下（§3.4 规则 3「写入失败不得静默」）。
func checkRange(dt model.DataType, v float64) error {
	switch dt {
	case model.TypeInt16:
		if v != math.Trunc(v) || v < math.MinInt16 || v > math.MaxInt16 {
			return fmt.Errorf("%v 超出 int16 范围或不是整数", v)
		}
	case model.TypeUint16:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint16 {
			return fmt.Errorf("%v 超出 uint16 范围或不是整数", v)
		}
	case model.TypeInt32:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return fmt.Errorf("%v 超出 int32 范围或不是整数", v)
		}
	case model.TypeUint32:
		if v != math.Trunc(v) || v < 0 || v > math.MaxUint32 {
			return fmt.Errorf("%v 超出 uint32 范围或不是整数", v)
		}
	case model.TypeInt64:
		if v != math.Trunc(v) || v < math.MinInt64 || v >= 9223372036854775808.0 {
			return fmt.Errorf("%v 超出 int64 范围或不是整数", v)
		}
	case model.TypeUint64:
		if v != math.Trunc(v) || v < 0 || v >= 18446744073709551616.0 {
			return fmt.Errorf("%v 超出 uint64 范围或不是整数", v)
		}
	case model.TypeFloat32:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%v 不是合法的 float32 值", v)
		}
		if v != 0 {
			f := float32(v)
			if math.IsInf(float64(f), 0) || f == 0 {
				return fmt.Errorf("%v 超出 float32 可表示范围", v)
			}
		}
	case model.TypeFloat64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%v 不是合法的 float64 值", v)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 错误与工具
// ---------------------------------------------------------------------------

// endCodeError 把一个 FINS/Host Link 结束码翻成带 driver 分类的错误。
//
// 这是**信封实现之外**的另一个调用点：finscore 自己在构造结束码错误（点表
// 里的地址不合法、读回的数据不够长）。同一个结束码，从 Host Link 信封层
// 抛出和从共享命令层抛出，分类必须一模一样——否则「同一个地址错在 TCP 上
// 显示 address_error、在串口上只是 bad」，而这两个驱动共用的正是本包。
//
// 三类的划分依据是「重发一次会不会好」：
//
//   - **地址类**（0x1101..0x1104 区/范围/越界/写保护）：配置或点表问题。重发
//     一万次结果相同，而且它会走调度器的重试路径，拖长一轮采集；它也不该
//     计入熔断（设备本身是好的，只是这个点写错了）。
//   - **链路类**（0x0201..0x0205、0x0301/0x0302 节点不在网/令牌超时/路由表
//     错）：对端没接住这一帧。重发通常正是它等的动作，所以标为可重试。
//   - **其余**（模式错、权限、命令未定义、帧长错……）：协议错，重发只会把同
//     一个错误重复 N 次。
func endCodeError(code uint16, msg string) error {
	ec := &finscodec.EndCodeError{Code: code}
	switch {
	case ec.Address():
		return driver.E(driver.ErrAddress, "fins", "", msg, false, ec)
	case ec.Link():
		return driver.E(driver.ErrProtocol, "fins", "", msg, true, ec)
	default:
		return driver.E(driver.ErrProtocol, "fins", "", msg, false, ec)
	}
}

func (c *Conn) areaError(op, name string) error {
	return endCodeError(0x1101, fmt.Sprintf("unknown FINS area %q (supported: %s)",
		name, strings.Join(finscodec.Areas(), ", ")))
}

func (c *Conn) failed(span driver.ReadSpan, err error) []driver.RawSample {
	q := QualityOf(err)
	out := make([]driver.RawSample, 0, len(span.Points))
	for _, p := range span.Points {
		out = append(out, driver.RawSample{PointID: p.ID, Quality: q, Err: err})
	}
	return out
}

// withPendingFailed 把「还没拿到值」的点位按 err 标坏，已解出的好样本原样保留。
//
// readSpan 是逐块读、逐点解的：前面的块已经解出好值，后面的块失败时返回
// (部分结果, err)。这里只补坏那些 Err 仍为空的槽位——把成功点也回填成错误
// 会让调用方把一条已读到的点当成失败去重试，且违反 §3.3.1「有成功点不代表
// 整批失败」的语义。
func (c *Conn) withPendingFailed(span driver.ReadSpan, samples []driver.RawSample, err error) []driver.RawSample {
	q := QualityOf(err)
	if len(samples) == 0 {
		return c.failed(span, err)
	}
	out := make([]driver.RawSample, 0, len(span.Points))
	for i, p := range span.Points {
		if i < len(samples) && samples[i].Err != nil {
			// 已经逐点标坏（解码期错误），保留它自己的错误，诊断更准。
			out = append(out, samples[i])
			continue
		}
		if i < len(samples) && samples[i].Quality == model.QualityGood {
			out = append(out, samples[i])
			continue
		}
		out = append(out, driver.RawSample{PointID: p.ID, Quality: q, Err: err})
	}
	return out
}

func (c *Conn) errSample(id string, err error) driver.RawSample {
	return driver.RawSample{PointID: id, Quality: QualityOf(err), Err: err}
}

func (c *Conn) writeFail(req driver.WriteRequest, err error) driver.WriteResult {
	return driver.WriteResult{PointID: req.PointID, Quality: QualityOf(err), Err: err}
}

// QualityOf 把一次失败映射成点位质量。顺序有讲究，因为同一条失败可能既
// 带着协议层自己的分类、又带着信封层的噪声标记。
//
//  1. **结束码**（第 1 步）：`EndCodeError` 按地址/链路/其余三类分。它比
//     driver.Kind 具体——地址类结束码（0x1101..0x1104）必须得
//     `address_error`，只靠 Kind 会落到 `bad`。
//  2. **信封层的噪声标记**（第 2 步）：Comm() 为真说明源于线路（校验和错、
//     单元号串话、FINS/TCP 封装层错误码），质量记 comm_error。
//  3. **driver.Kind 兜底**（第 3 步）：超时、地址、断链等。
//
// 不在这里解包 OpError.Err 再递归：内层若是 `EndCodeError`，`errors.As`
// 本来就能穿透 OpError 找到它；内层若是 `*TCPError`，它自己实现了 Comm()，
// 第 2 步直接命中。所以解包不会改变任何结论，反而让「超时错误内层是
// *net.OpError」这类路径多绕一圈（而且 net.OpError 归 bad，会误把 timeout
// 降级）。两层各自负责自己最具体的那个分类，足够且没有歧义。
func QualityOf(err error) model.Quality {
	if err == nil {
		return model.QualityGood
	}
	// 1. 结束码：地址类记 address_error、链路类记 comm_error、其余记 bad。
	var ec *finscodec.EndCodeError
	if errors.As(err, &ec) {
		if ec.Address() {
			return model.QualityAddressError
		}
		if ec.Link() {
			return model.QualityCommError
		}
		return model.QualityBad
	}
	// 2. 信封层的噪声/串话类错误：校验和错、单元号错、迟到应答、FINS/TCP
	// 封装层错误码（*TCPError 实现 Comm()）。
	var ce CommError
	if errors.As(err, &ce) && ce.Comm() {
		return model.QualityCommError
	}
	// 3. 按 driver.Kind 兜底。
	switch driver.KindOf(err) {
	case driver.ErrTimeout:
		return model.QualityTimeout
	case driver.ErrAddress, driver.ErrUnsupported:
		return model.QualityAddressError
	case driver.ErrDisconnected:
		return model.QualityCommError
	}
	return model.QualityBad
}

// DefaultTimeout 是信封实现没有拿到 ctx 截止时间时的兜底超时。
//
// 3 秒是个折中：串口 9600bps 下最长一帧（1115 字符 ≈ 1.2 秒）加上 PLC 的
// 响应等待时间（最长 150ms）仍在预算内，而现场「配错波特率」的典型症状
// （一帧都收不到）能在 3 秒内暴露，不必等到调度器超时。
func DefaultTimeout() time.Duration { return 3 * time.Second }
