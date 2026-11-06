package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"uic/internal/config"
	"uic/internal/model"
	"uic/internal/northbound/httpapi"
	nbopcua "uic/internal/northbound/opcua"
)

// opcuaNorthbound 是「正在跑的北向 OPC UA 服务端 + 它的配置指纹」。
//
// 指纹是热更新的判据：地址、站点、点位集合、可写性、类型，任何一项变了
// 都要重建地址空间（NodeId 与 Node 的 DataType 都在启动时固化在服务端
// 内核里，没有原地改的接口）。只改点位名之类的展示信息也会重建——代价是
// SCADA 的一次短暂重连，换来的是「配置说什么，服务端就是什么」。
type opcuaNorthbound struct {
	srv *nbopcua.Server
	fp  string
}

// OPCUASpecs 实现了 nbopcua.Source：把当前配置里的点位映射成地址空间。
//
// 只暴露 enabled 的设备与点位。一个被禁用的点位如果还留在地址空间里，
// SCADA 会读到一个永远 BadNotConnected 的变量，运维没有任何办法从客户端
// 侧看出「它只是被关了」。
func (r *Runtime) OPCUASpecs() []nbopcua.Spec {
	f := r.cfg.Current()
	var out []nbopcua.Spec
	for _, d := range f.Devices {
		if !d.Enabled {
			continue
		}
		for _, p := range d.Points {
			if !p.Enabled {
				continue
			}
			out = append(out, nbopcua.Spec{
				Device:     d.ID,
				DeviceName: d.Name,
				Point:      p.ID,
				Name:       p.Name,
				DataType:   p.DataType,
				Writable:   p.Writable,
			})
		}
	}
	return out
}

// OPCUASeeds 实现了 nbopcua.Source：给地址空间一份启动初值。
//
// 没有走缓存直接取，而是用 Point()：它对「还没采到」的点位会合成一个
// not_connected 样本，正好是北向该显示的状态。用零值 + Good 才是错的——
// 客户端连上就会看到 0.0 且质量良好。
func (r *Runtime) OPCUASeeds() []nbopcua.Seed {
	specs := r.OPCUASpecs()
	out := make([]nbopcua.Seed, 0, len(specs))
	for _, sp := range specs {
		s, ok := r.Point(sp.Device, sp.Point)
		if !ok {
			continue
		}
		out = append(out, nbopcua.Seed{Device: sp.Device, Point: sp.Point, Sample: s})
	}
	return out
}

// OPCUAWrite 实现了 nbopcua.Source：把客户端的写入转给南向设备。
//
// 复用 HTTP 写入路径（含幂等、确认、设备忙时的排队），所以「SCADA 写」
// 与「REST 写」在设备侧是同一条链路。自己再写一条直连驱动的快捷路径，
// 迟早会出现「REST 能写、SCADA 不能写」这种只在一个方向复现的问题。
func (r *Runtime) OPCUAWrite(ctx context.Context, device, point string, v model.Value) nbopcua.Outcome {
	res, err := r.Write(withSource(ctx, "opcua-server"), device, []httpapi.WriteItem{{Point: point, Value: v}})
	if err != nil {
		code, msg := apiErrorOf(err)
		return nbopcua.Outcome{OK: false, Code: code, Message: msg}
	}
	if len(res) != 1 {
		return nbopcua.Outcome{OK: false, Code: "write.unexpected", Message: "write returned no result"}
	}
	return nbopcua.Outcome{OK: res[0].OK, Code: res[0].Code, Message: res[0].Message}
}

// apiErrorOf 从错误里取出 API 错误码；非 API 错误统一归为 service.not_ready。
func apiErrorOf(err error) (string, string) {
	var ae *httpapi.APIError
	if errors.As(err, &ae) {
		return ae.Code, ae.Message
	}
	return "service.not_ready", err.Error()
}

// openOPCUA 在启动时按配置拉起北向服务端。
//
// 端口被占用必须让启动失败：一个「网关起来了但 SCADA 连不上」的部署，
// 排查成本远高于启动时报一行 address already in use。
func (r *Runtime) openOPCUA(f *config.File) error {
	if !f.Northbound.OPCUA.Enabled {
		return nil
	}
	srv, err := nbopcua.Start(nbopcua.Config{
		Addr:    f.Northbound.OPCUA.Listen,
		Site:    f.Site,
		Source:  r,
		Metrics: r.metrics,
		Log:     r.log,
	})
	if err != nil {
		return fmt.Errorf("runtime: opcua northbound: %w", err)
	}
	r.nbMu.Lock()
	r.nbOPCUA = &opcuaNorthbound{srv: srv, fp: opcuaFingerprint(f)}
	r.nbMu.Unlock()
	return nil
}

// closeOPCUA 关闭北向服务端并等待其推送循环退出。
func (r *Runtime) closeOPCUA() {
	r.nbMu.Lock()
	nb := r.nbOPCUA
	r.nbOPCUA = nil
	r.nbMu.Unlock()
	if nb == nil {
		return
	}
	if err := nb.srv.Close(); err != nil {
		r.log.Warn("opcua_server_close_failed", "error", errText(err))
	}
}

// syncOPCUA 在配置热更新后重建北向服务端。
//
// 重建失败时用上一份配置回退：配置已经写进 manager 了，不能因为北向端口
// 抢不到就把整次 Apply 变成失败；但也绝不能让北向就这么消失——回退能保住
// 「旧的地址空间还在服务」，同时在日志里明确写出「新配置没生效」。
func (r *Runtime) syncOPCUA(f, before *config.File) {
	r.nbMu.Lock()
	defer r.nbMu.Unlock()

	if !f.Northbound.OPCUA.Enabled {
		if r.nbOPCUA != nil {
			nb := r.nbOPCUA
			r.nbOPCUA = nil
			if err := nb.srv.Close(); err != nil {
				r.log.Warn("opcua_server_close_failed", "error", errText(err))
			}
		}
		return
	}

	fp := opcuaFingerprint(f)
	if r.nbOPCUA != nil && r.nbOPCUA.fp == fp {
		return
	}
	start := func(file *config.File) (*opcuaNorthbound, error) {
		srv, err := nbopcua.Start(nbopcua.Config{
			Addr:    file.Northbound.OPCUA.Listen,
			Site:    file.Site,
			Source:  r,
			Metrics: r.metrics,
			Log:     r.log,
		})
		if err != nil {
			return nil, err
		}
		return &opcuaNorthbound{srv: srv, fp: opcuaFingerprint(file)}, nil
	}

	old := r.nbOPCUA
	r.nbOPCUA = nil
	if old != nil {
		if err := old.srv.Close(); err != nil {
			r.log.Warn("opcua_server_close_failed", "error", errText(err))
		}
	}
	nb, err := start(f)
	if err != nil {
		r.log.Error("opcua_server_restart_failed", "error", errText(err))
		if before != nil && before.Northbound.OPCUA.Enabled {
			if nb2, err2 := start(before); err2 == nil {
				r.nbOPCUA = nb2
				r.log.Warn("opcua_server_restored",
					"detail", "新配置的北向 OPC UA 未能启动，已回退到上一份配置")
			} else {
				r.log.Error("opcua_server_restore_failed", "error", errText(err2))
			}
		}
		return
	}
	r.nbOPCUA = nb
}

// opcuaFingerprint 计算北向配置指纹。点位先排序，避免把「配置里换了顺序」
// 当成「地址空间变了」而做一次无谓的重建。
func opcuaFingerprint(f *config.File) string {
	specs := make([]nbopcua.Spec, 0, len(f.Devices))
	for _, d := range f.Devices {
		if !d.Enabled {
			continue
		}
		for _, p := range d.Points {
			if !p.Enabled {
				continue
			}
			specs = append(specs, nbopcua.Spec{
				Device: d.ID, Point: p.ID, DataType: p.DataType, Writable: p.Writable,
			})
		}
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Device != specs[j].Device {
			return specs[i].Device < specs[j].Device
		}
		return specs[i].Point < specs[j].Point
	})
	var b strings.Builder
	b.WriteString(f.Northbound.OPCUA.Listen)
	b.WriteByte('\x00')
	b.WriteString(f.Site)
	for _, s := range specs {
		b.WriteByte('\x00')
		b.WriteString(s.Device)
		b.WriteByte('/')
		b.WriteString(s.Point)
		b.WriteByte('/')
		b.WriteString(string(s.DataType))
		if s.Writable {
			b.WriteString("/w")
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// 编译期断言：Runtime 就是北向服务端的 Source。接口一旦变动，这里先炸，
// 而不是等到某个只在启动时才走到的分支。
var _ nbopcua.Source = (*Runtime)(nil)
