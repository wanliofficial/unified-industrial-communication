package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	slavemb "uic/internal/slave/modbus"
)

// 从站服务把采集结果反向暴露出去。三条硬约束：
//
//  1. **读走 cache**：从站不查配置、不触发采集，只回答「现在是什么值」。
//  2. **写走 Runtime.Write**：与 REST 写入共用设备 actor、可写校验与
//     确认语义，绝不直接碰驱动连接。
//  3. **配置变更重建监听**：映射改了却继续用旧表，会让外部主站读到
//     上一个版本的数据，这是最难发现的一类「配置已生效但行为没变」。

// slaveSource 把缓存适配成从站的取值接口。
type slaveSource struct{ rt *Runtime }

func (s slaveSource) Sample(device, point string) (model.Sample, bool) {
	return s.rt.cache.Get(device, point)
}

// slaveWriter 把从站写入适配成北向写入。
type slaveWriter struct{ rt *Runtime }

func (w slaveWriter) Write(ctx context.Context, reqs []slavemb.WriteRequest) []slavemb.WriteResult {
	out := make([]slavemb.WriteResult, len(reqs))
	// 按设备分组：一次 Modbus 多寄存器写可能跨越多个设备，必须拆成
	// 多次设备写入——每个设备有自己的 actor 与连接。
	byDevice := map[string][]int{}
	var order []string
	for i, q := range reqs {
		if _, ok := byDevice[q.Device]; !ok {
			order = append(order, q.Device)
		}
		byDevice[q.Device] = append(byDevice[q.Device], i)
	}
	for _, dev := range order {
		idx := byDevice[dev]
		items := make([]httpapi.WriteItem, len(idx))
		for j, i := range idx {
			items[j] = httpapi.WriteItem{Point: reqs[i].Point, Value: reqs[i].Value}
		}
		res, err := w.rt.Write(withSource(ctx, "modbus-slave"), dev, items)
		if err != nil {
			for _, i := range idx {
				out[i] = slavemb.WriteResult{Err: err}
			}
			continue
		}
		for j, i := range idx {
			if j >= len(res) {
				out[i] = slavemb.WriteResult{Err: fmt.Errorf("missing write result for %s/%s", dev, reqs[i].Point)}
				continue
			}
			if res[j].OK {
				out[i] = slavemb.WriteResult{OK: true}
				continue
			}
			msg := res[j].Message
			if msg == "" {
				msg = "write rejected"
			}
			out[i] = slavemb.WriteResult{Err: fmt.Errorf("%s: %s", res[j].Code, msg)}
		}
	}
	return out
}

// pointsByKey 建立 "device/point" → 点位 的索引。
func pointsByKey(devs []model.Device) map[string]model.Point {
	out := make(map[string]model.Point)
	for _, d := range devs {
		for _, p := range d.Points {
			out[d.ID+"/"+p.ID] = p
		}
	}
	return out
}

// slaveFingerprint 决定热更新时是否需要重建某个从站。
//
// 除了映射本身，还要把「被映射点位的类型与宽度」算进来：把温度从
// uint16 改成 float32 时映射表一个字都没变，但线上字节完全不同。
// 只看映射的指纹会漏掉这种最危险的变化。
func slaveFingerprint(s model.Slave, points map[string]model.Point) string {
	h := sha256.New()
	b, _ := json.Marshal(s)
	h.Write(b)
	keys := make([]string, 0, len(s.Units))
	for _, u := range s.Units {
		for _, m := range u.Mappings {
			keys = append(keys, m.Point)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, ok := points[k]
		if !ok {
			continue
		}
		fmt.Fprintf(h, "|%s=%s/%d/%d/%t", k, p.DataType, p.RegisterWidth(), p.Address.Length, p.Writable)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// openSlaves 按配置绑定全部从站。任一失败即整体失败：只起来一半从站
// 比完全没起来更难排查。
func (r *Runtime) openSlaves(f *config.File) error {
	if len(f.Slaves) == 0 {
		return nil
	}
	points := pointsByKey(f.Devices)
	for _, sc := range f.Slaves {
		srv, err := slavemb.New(slavemb.Options{
			Slave:   sc,
			Points:  points,
			Source:  slaveSource{rt: r},
			Writer:  slaveWriter{rt: r},
			Log:     r.log.With("slave", sc.ID),
			Metrics: r.metrics,
			// 版本号来自构建信息，而不是配置文件：上位机屏幕上显示的版本
			// 必须与进程里跑的二进制一致，否则现场排障第一句话就问不出来。
			Revision: r.version.Version,
		})
		if err != nil {
			r.closeSlaves()
			return err
		}
		if r.slaves == nil {
			r.slaves = map[string]*slavemb.Server{}
			r.slavePrints = map[string]string{}
		}
		r.slaves[sc.ID] = srv
		r.slavePrints[sc.ID] = slaveFingerprint(sc, points)
		r.log.Info("slave_listening", "slave", sc.ID, "addr", srv.Addr(), "units", len(sc.Units))
	}
	return nil
}

// closeSlaves 关闭全部从站。
func (r *Runtime) closeSlaves() {
	for id, s := range r.slaves {
		if err := s.Close(); err != nil {
			r.log.Warn("slave_close_failed", "slave", id, "error", err.Error())
		}
		delete(r.slaves, id)
		delete(r.slavePrints, id)
	}
}

// syncSlaves 让运行中的从站与配置一致：新增的建、删除的关、变化的重建。
// 返回发生变化的从站 id，供 ApplyResult 报告。
func (r *Runtime) syncSlaves(f *config.File) []string {
	desired := make(map[string]model.Slave, len(f.Slaves))
	for _, s := range f.Slaves {
		desired[s.ID] = s
	}
	var changed []string
	for id := range r.slaves {
		if _, keep := desired[id]; !keep {
			_ = r.slaves[id].Close()
			delete(r.slaves, id)
			delete(r.slavePrints, id)
			changed = append(changed, id)
		}
	}
	points := pointsByKey(f.Devices)
	for id, sc := range desired {
		if _, ok := r.slaves[id]; ok && r.slavePrints[id] == slaveFingerprint(sc, points) {
			continue
		}
		if old, ok := r.slaves[id]; ok {
			_ = old.Close()
			delete(r.slaves, id)
			delete(r.slavePrints, id)
		}
		srv, err := slavemb.New(slavemb.Options{
			Slave:    sc,
			Points:   points,
			Source:   slaveSource{rt: r},
			Writer:   slaveWriter{rt: r},
			Log:      r.log.With("slave", id),
			Metrics:  r.metrics,
			Revision: r.version.Version,
		})
		if err != nil {
			r.log.Error("slave_rebuild_failed", "slave", id, "error", err.Error())
			continue
		}
		r.slaves[id] = srv
		r.slavePrints[id] = slaveFingerprint(sc, points)
		changed = append(changed, id)
	}
	sort.Strings(changed)
	return changed
}

// Slaves 返回从站运行态（供健康检查与测试）。
func (r *Runtime) Slaves() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.slaves))
	for id, s := range r.slaves {
		out[id] = s.Addr()
	}
	return out
}
