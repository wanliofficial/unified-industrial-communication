// Package instanceid 提供运行实例的稳定身份标识。
//
// 为什么需要它：同站点跑两个 UIC 实例是常见部署（一台主机多容器、双机热备、
// 灰度期间新旧并存）。MQTT 的 clientID 与 Sparkplug 的 nodeId 都由「站点名」
// 派生默认值——一旦两个实例共用站点名又没显式配 clientID/nodeId，它们会连到
// 同一个 Broker 会话上互相顶掉：Broker 只允许一个 clientID 在线，后到的把前一个
// 踢下线，表现为「数据莫名其妙断流、日志里却没有任何错误」。这是典型的静默失效。
//
// 解决办法是给每个实例一个稳定且唯一的身份：首次启动时生成、持久化到数据目录，
// 之后重启复用。它有两个用途：
//  1. 派生 MQTT clientID / Sparkplug nodeId 的默认值时带上它，使同站点多实例
//     默认就不冲突；
//  2. 暴露到 /version 与诊断包，让运维一眼区分「这是哪台实例」。
//
// 为什么不写进配置文件：实例身份是「部署事实」而不是「业务配置」。它若进
// config.Server 就会进入配置的规范化内容哈希（Canonical 序列化整个 File），
// 于是每次换机器/换数据目录都产生一个新配置版本，污染版本列表与回滚历史。
package instanceid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileName 是实例身份在数据目录里的持久化文件名。
const fileName = "instance.id"

// Load 返回数据目录对应的实例身份。
//
// 数据目录里已有身份文件就复用（重启稳定）；否则生成一个新的并落盘。落盘失败
// 不致命：返回错误让调用方决定是否继续——实例身份缺失只影响「多实例默认不冲突」
// 这一层兜底，不应让整个进程起不来。
func Load(dataDir string) (string, error) {
	path := filepath.Join(dataDir, fileName)
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
		// 文件存在但为空：当作没有，重新生成。空身份比新身份更糟——它会派生出
		// 空 clientID 后缀，等于没做消歧。
	}
	id := New()
	if err := write(path, id); err != nil {
		return "", err
	}
	return id, nil
}

// New 生成一个随机会例身份。用 crypto/rand 而非 time/pid：同一秒内重启两次、
// 或容器重启后 pid 复用，都会让「身份」撞车，而身份撞车正是我们要消除的问题。
func New() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 不可用是灾难级环境故障；退到一个仍然唯一（纳秒级时间）
		// 的值，好过 panic 让整个进程起不来。唯一性下降但服务可用。
		return fmt.Sprintf("inst-%x", time.Now().UnixNano())
	}
	return "inst-" + hex.EncodeToString(b[:])
}

// write 原子写入身份文件：先写临时文件再 rename，避免「写一半崩溃留下半截身份」。
func write(path, id string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id), 0o600); err != nil {
		return fmt.Errorf("instanceid: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("instanceid: commit %s: %w", path, err)
	}
	return nil
}
