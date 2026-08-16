// Package lb 实现内存级负载均衡调度器，支持轮询、平滑加权轮询、
// 最少连接三种选取策略。所有方法并发安全。
package lb

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// Strategy 是选取策略。
type Strategy string

const (
	// StrategyRR 为轮询：在可选节点按注册顺序构成的列表上取模循环。
	StrategyRR Strategy = "rr"
	// StrategyWRR 为平滑加权轮询（smooth weighted round-robin）。
	StrategyWRR Strategy = "wrr"
	// StrategyLC 为最少连接：选取活跃连接数最小的节点。
	StrategyLC Strategy = "lc"
)

// ParseStrategy 将字符串解析为策略，非法时返回错误。
func ParseStrategy(s string) (Strategy, error) {
	switch Strategy(s) {
	case StrategyRR, StrategyWRR, StrategyLC:
		return Strategy(s), nil
	}
	return "", fmt.Errorf("未知策略: %q", s)
}

// idRe 校验节点 ID：非空、仅含字母/数字/下划线/连字符。
var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// 校验与状态相关错误。调用方可通过 errors.Is 判别。
var (
	ErrEmptyID          = errors.New("节点 ID 不能为空")
	ErrBadID            = errors.New("节点 ID 格式非法")
	ErrEmptyAddr        = errors.New("节点地址不能为空")
	ErrNegativeWeight   = errors.New("权重不能为负数")
	ErrUnknownNode      = errors.New("未知节点")
	ErrNoAvailable      = errors.New("没有可用的后端节点")
	ErrReleaseUnderflow = errors.New("活跃连接数已为 0，无法释放")
)

// Node 是一份后端节点定义及其运行时统计。
type Node struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Weight  int    `json:"weight"`
	Healthy bool   `json:"healthy"`

	// 运行时状态，不直接对外序列化（由 Stats 暴露）。
	currentWeight int   // 平滑加权轮询的当前权重，跨调用保留
	active        int   // 活跃连接数
	selections    int64 // 累计被选中次数
	seq           int   // 注册顺序序号
}

// Stats 是节点的对外统计快照。
type Stats struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	Weight     int    `json:"weight"`
	Healthy    bool   `json:"healthy"`
	Active     int    `json:"active"`
	Selections int64  `json:"selections"`
}

// Store 持有全部节点，并发安全。order 记录注册顺序（仅保留现存节点）。
type Store struct {
	mu         sync.Mutex
	nodes      map[string]*Node
	order      []string // 现存节点 ID，按注册顺序
	rrCounter  uint64   // 轮询计数器
	seqCounter int      // 注册序号发生器
}

// New 创建一个空的调度器存储。
func New() *Store {
	return &Store{nodes: make(map[string]*Node)}
}

// UpsertNode 注册或更新节点。
// 节点 ID 非空且合法、地址非空、权重非负；否则返回错误且不写入。
// 若节点已存在，仅替换权重与地址，活跃连接数、累计选中次数、
// 健康状态与平滑轮询当前权重保持不变。
func (s *Store) UpsertNode(id, address string, weight int) error {
	if id == "" {
		return ErrEmptyID
	}
	if !idRe.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrBadID, id)
	}
	if address == "" {
		return ErrEmptyAddr
	}
	if weight < 0 {
		return ErrNegativeWeight
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.nodes[id]; ok {
		existing.Address = address
		existing.Weight = weight
		// active/selections/Healthy/currentWeight 保留不变。
		return nil
	}
	n := &Node{
		ID:      id,
		Address: address,
		Weight:  weight,
		Healthy: true,
		seq:     s.seqCounter,
	}
	s.seqCounter++
	s.nodes[id] = n
	s.order = append(s.order, id)
	return nil
}

// RemoveNode 按 ID 移除节点；不存在返回错误。移除后该节点不再参与调度。
func (s *Store) RemoveNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[id]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, id)
	}
	delete(s.nodes, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

// SetHealth 将节点标记为健康（up）或不健康（down）。不存在返回错误。
func (s *Store) SetHealth(id string, healthy bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, id)
	}
	n.Healthy = healthy
	return nil
}

// eligible 返回可选节点（健康且权重大于 0），按注册顺序排列。调用方须持锁。
func (s *Store) eligible() []*Node {
	out := make([]*Node, 0, len(s.order))
	for _, id := range s.order {
		n := s.nodes[id]
		if n != nil && n.Healthy && n.Weight > 0 {
			out = append(out, n)
		}
	}
	return out
}

// Pick 按给定策略选取一个节点，并为其占用一条连接（活跃连接数 +1，累计选中 +1）。
// 没有可选节点时返回 ErrNoAvailable。
func (s *Store) Pick(strategy Strategy) (id, address string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	elig := s.eligible()
	if len(elig) == 0 {
		return "", "", ErrNoAvailable
	}
	var picked *Node
	switch strategy {
	case StrategyRR:
		idx := int(s.rrCounter % uint64(len(elig)))
		picked = elig[idx]
		s.rrCounter++
	case StrategyWRR:
		picked = s.pickWRR(elig)
	case StrategyLC:
		picked = s.pickLC(elig)
	default:
		return "", "", fmt.Errorf("未知策略: %q", strategy)
	}
	picked.active++
	picked.selections++
	return picked.ID, picked.Address, nil
}

// pickWRR 实现 nginx 风格的平滑加权轮询：每轮各节点 currentWeight 累加自身权重，
// 取 currentWeight 最大者，并将其减去全部可选权重之和。相等时取注册顺序靠前者。
func (s *Store) pickWRR(elig []*Node) *Node {
	total := 0
	for _, n := range elig {
		n.currentWeight += n.Weight
		total += n.Weight
	}
	var best *Node
	for _, n := range elig {
		if best == nil || n.currentWeight > best.currentWeight {
			best = n
		}
	}
	best.currentWeight -= total
	return best
}

// pickLC 选取活跃连接数最小的节点；并列时按权重降序、再按注册顺序先后选取。
func (s *Store) pickLC(elig []*Node) *Node {
	var best *Node
	for _, n := range elig {
		switch {
		case best == nil:
			best = n
		case n.active < best.active:
			best = n
		case n.active == best.active && n.Weight > best.Weight:
			best = n
		}
	}
	return best
}

// Release 释放节点上的一条连接（活跃连接数 -1）。
// 节点不存在返回 ErrUnknownNode；活跃连接数已为 0 返回 ErrReleaseUnderflow，不改变状态。
func (s *Store) Release(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, id)
	}
	if n.active <= 0 {
		return ErrReleaseUnderflow
	}
	n.active--
	return nil
}

// Stats 返回全部现存节点的统计快照，按注册顺序排列。
func (s *Store) Stats() []Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Stats, 0, len(s.order))
	for _, id := range s.order {
		n := s.nodes[id]
		out = append(out, Stats{
			ID:         n.ID,
			Address:    n.Address,
			Weight:     n.Weight,
			Healthy:    n.Healthy,
			Active:     n.active,
			Selections: n.selections,
		})
	}
	return out
}

// HasNode 报告节点是否存在（仅用于自检与诊断）。
func (s *Store) HasNode(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.nodes[id]
	return ok
}
