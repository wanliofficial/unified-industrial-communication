// Package cache 是点位当前值的唯一事实来源，并向 SSE / MQTT / OPC UA
// 提供变更事件。所有读取路径都必须经过这里，不允许各模块各自维护副本。
package cache

import (
	"sort"
	"sync"
	"sync/atomic"

	"uic/internal/model"
)

// EventType 是缓存事件类型。
type EventType string

const (
	EventSample  EventType = "sample"
	EventQuality EventType = "quality"
)

// Event 是发布给订阅者的事件。
type Event struct {
	Type   EventType     `json:"type"`
	Sample model.Sample  `json:"sample"`
	From   model.Quality `json:"from,omitempty"`
	To     model.Quality `json:"to,omitempty"`
}

// Cache 是并发安全的点位快照。
type Cache struct {
	mu      sync.RWMutex
	samples map[string]model.Sample
	subs    map[int]*subscriber
	nextID  int
}

type subscriber struct {
	filter func(model.Sample) bool
	ch     chan Event
	// closed 用原子标志而不是 close(ch) 表示退订：Set 在锁外推送，
	// 直接关闭会与推送竞争并 panic（SSE 断开恰好发生在采集写入时）。
	closed atomic.Bool
}

// New 创建空缓存。
func New() *Cache {
	return &Cache{samples: map[string]model.Sample{}, subs: map[int]*subscriber{}}
}

// Set 写入采样，并在值或质量变化时发布事件。
func (c *Cache) Set(s model.Sample) {
	key := s.Key()
	c.mu.Lock()
	prev, existed := c.samples[key]
	c.samples[key] = s
	subs := make([]*subscriber, 0, len(c.subs))
	for _, sub := range c.subs {
		subs = append(subs, sub)
	}
	c.mu.Unlock()

	if existed && prev.Value.Equal(s.Value) && prev.Quality == s.Quality {
		return
	}
	for _, sub := range subs {
		if sub.closed.Load() {
			continue
		}
		if sub.filter != nil && !sub.filter(s) {
			continue
		}
		if existed && prev.Quality != s.Quality {
			push(sub.ch, Event{Type: EventQuality, Sample: s, From: prev.Quality, To: s.Quality})
		}
		if !existed || !prev.Value.Equal(s.Value) {
			push(sub.ch, Event{Type: EventSample, Sample: s})
		}
	}
}

func push(ch chan Event, ev Event) {
	select {
	case ch <- ev:
	default:
		// 慢订阅者：丢弃最旧事件，保证不阻塞采集。
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- ev:
		default:
		}
	}
}

// Get 读取单点快照。
func (c *Cache) Get(device, point string) (model.Sample, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.samples[device+"/"+point]
	return s, ok
}

// List 返回指定设备的全部点位（device 为空表示全部），按 device/point 排序。
func (c *Cache) List(device string) []model.Sample {
	c.mu.RLock()
	out := make([]model.Sample, 0, len(c.samples))
	for _, s := range c.samples {
		if device == "" || s.DeviceID == device {
			out = append(out, s)
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].PointID < out[j].PointID
	})
	return out
}

// Count 返回点位总数与各质量码计数。
func (c *Cache) Count() (int, map[model.Quality]int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	byQ := map[model.Quality]int{}
	for _, s := range c.samples {
		byQ[s.Quality]++
	}
	return len(c.samples), byQ
}

// Subscribe 注册订阅者。filter 为 nil 表示全部。返回的 cancel 必须调用。
func (c *Cache) Subscribe(buffer int, filter func(model.Sample) bool) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	sub := &subscriber{filter: filter, ch: make(chan Event, buffer)}
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.subs[id] = sub
	c.mu.Unlock()
	return sub.ch, func() {
		c.mu.Lock()
		if s, ok := c.subs[id]; ok {
			delete(c.subs, id)
			s.closed.Store(true)
		}
		c.mu.Unlock()
	}
}

// SubscriberCount 返回当前订阅者数量。
func (c *Cache) SubscriberCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.subs)
}
