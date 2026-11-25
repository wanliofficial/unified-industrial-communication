// Package heartbeat 周期性地把进程存活快照写入数据目录，供外部 supervisor 读取。
//
// 为什么需要它：/healthz 只能回答「HTTP 层还在不在」。但要区分**「进程死了」**
// 与**「进程活着但采集中枢卡死」**，一个带时间戳的文件往往更通用——裸机 cron、
// 老式监控脚本、断网现场、无法访问 HTTP 端口的容器编排，都能靠「文件的 ts 还在
// 不前进 / scheduler.verdict 是不是 stalled」来判断。它是一个零依赖、可被任意
// 脚本解析的信号。
//
// 与 HTTP 的关系：两者并列而非替代。能访问 HTTP 的用 /healthz 与 /api/v1/health；
// 不能的用这个文件。二者读的是同一份运行时事实。
package heartbeat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// fileName 是心跳文件在数据目录里的固定名字。
const fileName = "heartbeat.json"

// Path 返回给定数据目录下的心跳文件路径。
func Path(dataDir string) string { return filepath.Join(dataDir, fileName) }

// Scheduler 是采集中枢的活性快照。
type Scheduler struct {
	// Verdict 取值 healthy | stalled | unknown（见 internal/watchdog）。
	Verdict      string `json:"verdict"`
	AgeMs        int64  `json:"ageMs"`
	StallAfterMs int64  `json:"stallAfterMs,omitempty"`
}

// Devices 是设备状态汇总（与 /api/v1/health 的计数同源）。
type Devices struct {
	Total      int `json:"total"`
	Collecting int `json:"collecting"`
	Degraded   int `json:"degraded"`
	Faulted    int `json:"faulted"`
	Disabled   int `json:"disabled"`
}

// Snapshot 是一次心跳的完整内容。
type Snapshot struct {
	// TS 是本次写入的时刻（Unix 秒）。外部脚本用它判断文件是否「新鲜」。
	TS int64 `json:"ts"`
	// TSMs 是本次写入的时刻（Unix 毫秒）。秒级精度下，任何快于 1s 的心跳
	// 间隔都会让相邻两次写入的 TS 相同——读方据此会误判「文件不再更新」。
	// 因此额外给出毫秒时间戳，让亚秒级心跳也能被如实观测。
	TSMs int64 `json:"tsMs"`
	// TSAt 是 TSMs 的 RFC3339 表示，方便人读。
	TSAt          string    `json:"tsAt"`
	PID           int       `json:"pid"`
	InstanceID    string    `json:"instanceId,omitempty"`
	Version       string    `json:"version,omitempty"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	Scheduler     Scheduler `json:"scheduler"`
	Devices       Devices   `json:"devices"`
}

// Write 原子写入心跳文件：先写临时文件再 rename，避免读方看到写一半的 JSON。
//
// 写入失败不是致命错误——心跳是观测手段，进程本身的寿命不该取决于能否落盘；
// 调用方记录告警即可。
func Write(dataDir string, s Snapshot) error {
	path := Path(dataDir)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("heartbeat: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("heartbeat: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("heartbeat: commit %s: %w", path, err)
	}
	return nil
}
