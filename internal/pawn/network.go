package pawn

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Level string

const (
	LevelHQ     Level = "hq"
	LevelRegion Level = "region"
	LevelStore  Level = "store"
)

type Node struct {
	ID       string
	Level    Level
	ParentID string
	ChildIDs []string

	StoreIndex int

	// Subs 仅门店代理使用。
	Subs map[string]Subscription

	// Received 是这台代理直接收到的订阅/下级上报。
	Received []Filter
	// Reported 是合并后真正往上游报告的过滤器（HQ 的一份仅作预览）。
	Reported []Filter

	receivedMatcher *Matcher

	Stats NodeStats
}

type NodeStats struct {
	MatchCalls     atomic.Int64
	Candidates     atomic.Int64
	MatchedEntries atomic.Int64
	MatchNSTotal   atomic.Int64
	MatchNSMax     atomic.Int64
}

type Link struct {
	Key      string
	ParentID string
	ChildID  string

	// Up 是子→父方向转发的收当原始消息数。
	Up atomic.Int64
	// Down 是父→子方向通过订阅过滤后转发的消息数。
	Down atomic.Int64
	// DownNecessary 是事后由子树真实送达结果确认“这条转发确有必要”的次数。
	DownNecessary atomic.Int64
}

type EventRecord struct {
	Seq     int    `json:"seq"`
	At      string `json:"at,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type Network struct {
	Cfg Config

	RootID string
	Nodes  map[string]*Node
	Links  map[string]*Link

	StoreIDs   []string
	StoreIndex map[string]int

	Sample    []Message
	Tolerance float64
	subBits   map[string][]uint64

	EventLog     []EventRecord
	RetiredLinks []LinkView

	mu sync.RWMutex
}

func NewNetwork(cfg Config, sample []Message, tolerance float64) *Network {
	root, regions, stores, parent := InitialTopology(cfg)
	n := &Network{
		Cfg:        cfg,
		RootID:     root,
		Nodes:      map[string]*Node{},
		Links:      map[string]*Link{},
		StoreIDs:   stores,
		StoreIndex: map[string]int{},
		Sample:     sample,
		Tolerance:  tolerance,
		subBits:    map[string][]uint64{},
	}

	n.Nodes[root] = &Node{ID: root, Level: LevelHQ, StoreIndex: -1}
	for _, rid := range regions {
		pid := parent[rid]
		n.Nodes[rid] = &Node{ID: rid, Level: LevelRegion, ParentID: pid, StoreIndex: -1}
		n.Nodes[pid].ChildIDs = append(n.Nodes[pid].ChildIDs, rid)
		n.Links[rid] = &Link{Key: pid + "->" + rid, ParentID: pid, ChildID: rid}
	}
	for i, sid := range stores {
		pid := parent[sid]
		n.StoreIndex[sid] = i
		n.Nodes[sid] = &Node{
			ID: sid, Level: LevelStore, ParentID: pid, StoreIndex: i,
			Subs: map[string]Subscription{},
		}
		// 初始拓扑为连续分片；若用 round-robin 也可排序，这里统一排序保证页面稳定。
		n.Nodes[pid].ChildIDs = append(n.Nodes[pid].ChildIDs, sid)
		n.Links[sid] = &Link{Key: pid + "->" + sid, ParentID: pid, ChildID: sid}
	}
	for _, node := range n.Nodes {
		sort.Strings(node.ChildIDs)
	}
	return n
}

// CacheSubBits 预计算原始订阅在合并样本上的命中位图。
func (n *Network) CacheSubBits(subs []Subscription) {
	for i := range subs {
		s := subs[i]
		n.subBits[s.ID] = predicateBits(s.Predicate, n.Sample)
	}
}

func (n *Network) LoadInitial(planned []PlannedSub) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, p := range planned {
		if p.AddSeq == 0 {
			node := n.Nodes[p.Sub.StoreID]
			if node != nil && node.Level == LevelStore {
				node.Subs[p.Sub.ID] = p.Sub
				if _, ok := n.subBits[p.Sub.ID]; !ok {
					n.subBits[p.Sub.ID] = predicateBits(p.Sub.Predicate, n.Sample)
				}
			}
		}
	}
	n.rebuildAllLocked()
	n.EventLog = append(n.EventLog, EventRecord{Seq: 0, Kind: "start", Message: fmt.Sprintf("拓扑启动：%d 家门店、%d 个区域代理", n.Cfg.StoreCount, n.Cfg.RegionCount)})
}

func (n *Network) rebuildAllLocked() {
	// 自底向上：门店 -> 区域 -> HQ。
	for _, sid := range n.StoreIDs {
		n.rebuildStoreLocked(sid)
	}
	// 多轮以兼容动态重父化后 HQ 直连门店；当前三级树两轮足够。
	for _, id := range n.sortedNodeIDsLocked() {
		node := n.Nodes[id]
		if node.Level != LevelStore {
			n.rebuildAggregatorLocked(id)
		}
	}
	n.rebuildAggregatorLocked(n.RootID)
}

func (n *Network) sortedNodeIDsLocked() []string {
	ids := make([]string, 0, len(n.Nodes))
	for id := range n.Nodes {
		if id != n.RootID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func subToFilter(s Subscription) Filter {
	return Filter{
		ID:          s.ID,
		OwnerID:     s.StoreID,
		Predicate:   s.Predicate,
		MergedCount: 1,
	}
}

func (n *Network) mergeOptions(prefix string) MergeOptions {
	return MergeOptions{
		Tolerance:   n.Tolerance,
		Sample:      n.Sample,
		InitialBits: n.subBits,
		IDPrefix:    prefix,
	}
}

func (n *Network) rebuildStoreLocked(sid string) {
	node := n.Nodes[sid]
	if node == nil || node.Level != LevelStore {
		return
	}
	ids := make([]string, 0, len(node.Subs))
	for id := range node.Subs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	received := make([]Filter, 0, len(ids))
	preds := make([]Predicate, 0, len(ids))
	for _, id := range ids {
		f := subToFilter(node.Subs[id])
		received = append(received, f)
		preds = append(preds, f.Predicate)
	}
	node.Received = received
	node.receivedMatcher = NewMatcher(preds)
	node.Reported = MergeFilters(append([]Filter(nil), received...), n.mergeOptions("MRG-"+sid))
	for i := range node.Reported {
		node.Reported[i].OwnerID = sid
	}
}

func (n *Network) rebuildAggregatorLocked(id string) {
	node := n.Nodes[id]
	if node == nil || node.Level == LevelStore {
		return
	}
	childIDs := append([]string{}, node.ChildIDs...)
	sort.Strings(childIDs)
	node.ChildIDs = childIDs

	received := []Filter{}
	preds := []Predicate{}
	for _, cid := range childIDs {
		child := n.Nodes[cid]
		if child == nil {
			continue
		}
		for _, f := range child.Reported {
			cp := f
			cp.OwnerID = cid
			received = append(received, cp)
			preds = append(preds, cp.Predicate)
		}
	}
	node.Received = received
	node.receivedMatcher = NewMatcher(preds)
	node.Reported = MergeFilters(append([]Filter(nil), received...), n.mergeOptions("MRG-"+id))
	for i := range node.Reported {
		node.Reported[i].OwnerID = id
	}
}

func (n *Network) ApplyChanges(changes []SubChange) {
	if len(changes) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	affected := map[string]struct{}{}
	for _, ch := range changes {
		sid := ch.Sub.StoreID
		node := n.Nodes[sid]
		if node == nil || node.Level != LevelStore {
			continue
		}
		if ch.Add {
			node.Subs[ch.Sub.ID] = ch.Sub
			if _, ok := n.subBits[ch.Sub.ID]; !ok {
				n.subBits[ch.Sub.ID] = predicateBits(ch.Sub.Predicate, n.Sample)
			}
		} else {
			delete(node.Subs, ch.Sub.ID)
			delete(n.subBits, ch.Sub.ID)
		}
		affected[sid] = struct{}{}
	}

	// 同一门店的一批增删只重建一次，随后重建其祖先链。
	ancestors := map[string]struct{}{}
	for sid := range affected {
		n.rebuildStoreLocked(sid)
		pid := n.Nodes[sid].ParentID
		for pid != "" {
			ancestors[pid] = struct{}{}
			p := n.Nodes[pid]
			if p == nil {
				break
			}
			pid = p.ParentID
		}
	}
	// 区域先于 HQ；简单按 ID 排序不能保证层级，因此重复两轮，当前树足够。
	for pid := range ancestors {
		if n.Nodes[pid] != nil && n.Nodes[pid].Level != LevelHQ {
			n.rebuildAggregatorLocked(pid)
		}
	}
	n.rebuildAggregatorLocked(n.RootID)
}

func (n *Network) SetTolerance(t float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Tolerance = t
	n.rebuildAllLocked()
	n.EventLog = append(n.EventLog, EventRecord{Seq: -1, Kind: "merge", Message: fmt.Sprintf("合并允许超投比例调整为 %.0f%%，所有订阅表已重算", t*100)})
}

func (n *Network) Rebuild() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rebuildAllLocked()
}

func (n *Network) HasNode(id string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	_, ok := n.Nodes[id]
	return ok
}

// retireLinkLocked 结束一条当前父子边并归档其生命周期计数，然后把计数器清零，
// 供重父化后的新边使用同一个 child-keyed Link 对象。
func (n *Network) retireLinkLocked(childID string) {
	l := n.Links[childID]
	if l == nil {
		return
	}
	n.RetiredLinks = append(n.RetiredLinks, linkView(l, true))
	l.Up.Store(0)
	l.Down.Store(0)
	l.DownNecessary.Store(0)
}

func (n *Network) AddProxy(id, parentID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.Nodes[id]; ok {
		return fmt.Errorf("代理 %s 已存在", id)
	}
	parent, ok := n.Nodes[parentID]
	if !ok {
		return fmt.Errorf("父代理 %s 不存在", parentID)
	}
	if parent.Level == LevelStore {
		return errors.New("门店代理是叶子节点，不能在其下再挂代理")
	}
	node := &Node{ID: id, Level: LevelRegion, ParentID: parentID, StoreIndex: -1}
	n.Nodes[id] = node
	parent.ChildIDs = append(parent.ChildIDs, id)
	sort.Strings(parent.ChildIDs)
	n.Links[id] = &Link{Key: parentID + "->" + id, ParentID: parentID, ChildID: id}
	n.rebuildAncestorChainLocked(parentID)
	n.EventLog = append(n.EventLog, EventRecord{Seq: -1, Kind: "proxy-join", Message: fmt.Sprintf("代理 %s 加入，挂到 %s 下，订阅表已合并", id, parentID)})
	return nil
}

// rebuildAncestorChainLocked 从 startID 开始自底向上重建到 root 的所有聚合表。
// 多级代理嵌套时不能只重建父节点和 HQ，否则中间层会保留旧 Reported。
func (n *Network) rebuildAncestorChainLocked(startID string) {
	id := startID
	for id != "" {
		node := n.Nodes[id]
		if node == nil {
			return
		}
		if node.Level != LevelStore {
			n.rebuildAggregatorLocked(id)
		}
		id = node.ParentID
	}
}

func (n *Network) createsCycleLocked(childID, newParentID string) bool {
	if childID == n.RootID || newParentID == childID {
		return true
	}
	cur := newParentID
	for cur != "" {
		if cur == childID {
			return true
		}
		node := n.Nodes[cur]
		if node == nil {
			return false
		}
		cur = node.ParentID
	}
	return false
}

// RemoveProxy 退出一个聚合代理，并把它的直接子代理重父化到它的父代理。
// 这样模拟运行中区域代理退出不会造成门店断流，订阅表会原子调整。
func (n *Network) RemoveProxy(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	node, ok := n.Nodes[id]
	if !ok {
		return fmt.Errorf("代理 %s 不存在", id)
	}
	if id == n.RootID || node.Level == LevelStore {
		return errors.New("根代理或门店代理不能通过该接口退出")
	}
	parentID := node.ParentID
	parent := n.Nodes[parentID]
	if parent == nil {
		return fmt.Errorf("父代理 %s 不存在", parentID)
	}

	children := append([]string{}, node.ChildIDs...)
	for _, cid := range children {
		child := n.Nodes[cid]
		if child == nil {
			continue
		}
		n.retireLinkLocked(cid)
		child.ParentID = parentID
		parent.ChildIDs = append(parent.ChildIDs, cid)
		if link := n.Links[cid]; link != nil {
			link.ParentID = parentID
			link.Key = parentID + "->" + cid
		} else {
			n.Links[cid] = &Link{Key: parentID + "->" + cid, ParentID: parentID, ChildID: cid}
		}
	}
	node.ChildIDs = nil

	// 从父节点移除退出代理及其旧链路。
	kept := parent.ChildIDs[:0]
	for _, cid := range parent.ChildIDs {
		if cid != id {
			kept = append(kept, cid)
		}
	}
	parent.ChildIDs = uniqStrings(kept)
	sort.Strings(parent.ChildIDs)
	n.retireLinkLocked(id)
	delete(n.Links, id)
	delete(n.Nodes, id)

	n.rebuildAncestorChainLocked(parentID)
	n.EventLog = append(n.EventLog, EventRecord{Seq: -1, Kind: "proxy-leave", Message: fmt.Sprintf("代理 %s 退出，%d 个下级已改挂到 %s，订阅表已重算", id, len(children), parentID)})
	return nil
}

func (n *Network) MoveProxy(childID, newParentID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	child, ok := n.Nodes[childID]
	if !ok {
		return fmt.Errorf("代理 %s 不存在", childID)
	}
	newParent, ok := n.Nodes[newParentID]
	if !ok {
		return fmt.Errorf("目标父代理 %s 不存在", newParentID)
	}
	if newParent.Level == LevelStore {
		return errors.New("门店代理是叶子节点，不能作为父节点")
	}
	if n.createsCycleLocked(childID, newParentID) {
		return fmt.Errorf("不能把根代理移动到下级，也不能把代理移动到自己的后代下面")
	}
	oldParentID := child.ParentID
	if oldParentID == newParentID {
		return nil
	}
	oldParent := n.Nodes[oldParentID]
	if oldParent != nil {
		kept := oldParent.ChildIDs[:0]
		for _, id := range oldParent.ChildIDs {
			if id != childID {
				kept = append(kept, id)
			}
		}
		oldParent.ChildIDs = kept
	}
	n.retireLinkLocked(childID)
	child.ParentID = newParentID
	newParent.ChildIDs = append(newParent.ChildIDs, childID)
	sort.Strings(newParent.ChildIDs)
	if link := n.Links[childID]; link != nil {
		link.ParentID = newParentID
		link.Key = newParentID + "->" + childID
	} else {
		n.Links[childID] = &Link{Key: newParentID + "->" + childID, ParentID: newParentID, ChildID: childID}
	}
	// 新父链必须自底向上重建；旧父链最后再重建一次，保证两边从直接子表到 HQ 都不陈旧。
	n.rebuildAncestorChainLocked(newParentID)
	if oldParentID != "" && oldParentID != newParentID {
		n.rebuildAncestorChainLocked(oldParentID)
	}
	n.EventLog = append(n.EventLog, EventRecord{Seq: -1, Kind: "proxy-move", Message: fmt.Sprintf("代理 %s 从 %s 调整到 %s，相关订阅表已合并", childID, oldParentID, newParentID)})
	return nil
}

type Trace struct {
	Seq             int        `json:"seq"`
	MessageID       string     `json:"message_id"`
	OriginID        string     `json:"origin_id"`
	MatchNS         int64      `json:"match_ns"`
	CandidateCount  int64      `json:"candidate_count"`
	MatchedEntries  int64      `json:"matched_entries"`
	DeliveredStores []string   `json:"delivered_stores"`
	Path            []string   `json:"path"`
	Display         []AttrView `json:"display"`
}

// DispatchMessage 完整走一次“上游收当 -> 沿树过滤分发”。
func (n *Network) DispatchMessage(msg Message) (uint64, Trace) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	tr := Trace{
		Seq: msg.Seq, MessageID: msg.ID, OriginID: msg.OriginID,
		Display: msg.Display, Path: []string{},
	}

	// 原始收当消息从发源门店逐级上报 HQ；这些链路计数不受订阅合并影响。
	child := msg.OriginID
	for child != n.RootID && child != "" {
		node := n.Nodes[child]
		if node == nil {
			break
		}
		pid := node.ParentID
		if link := n.Links[child]; link != nil {
			link.Up.Add(1)
		}
		tr.Path = append(tr.Path, fmt.Sprintf("%s ↑ 上报 %s", child, pid))
		child = pid
	}

	var cand []int32
	mask := n.dispatchLocked(n.RootID, &msg, &tr, &cand)

	for i := 0; i < n.Cfg.StoreCount && i < 64; i++ {
		if mask&(1<<uint(i)) != 0 {
			tr.DeliveredStores = append(tr.DeliveredStores, StoreID(i))
		}
	}
	return mask, tr
}

func (n *Network) dispatchLocked(nodeID string, msg *Message, tr *Trace, cand *[]int32) uint64 {
	node := n.Nodes[nodeID]
	if node == nil || node.receivedMatcher == nil {
		return 0
	}

	start := time.Now()
	*cand = node.receivedMatcher.AppendMatch(msg, (*cand)[:0])
	elapsed := time.Since(start).Nanoseconds()

	node.Stats.MatchCalls.Add(1)
	node.Stats.Candidates.Add(int64(len(*cand)))
	node.Stats.MatchedEntries.Add(int64(len(*cand)))
	node.Stats.MatchNSTotal.Add(elapsed)
	// 最大值用 CAS 更新，竞争很少（模拟主循环 + 偶尔页面重放）。
	for {
		old := node.Stats.MatchNSMax.Load()
		if elapsed <= old || node.Stats.MatchNSMax.CompareAndSwap(old, elapsed) {
			break
		}
	}
	tr.MatchNS += elapsed
	tr.CandidateCount += int64(len(*cand))
	tr.MatchedEntries += int64(len(*cand))
	tr.Path = append(tr.Path, fmt.Sprintf("%s 索引命中 %d 条", nodeID, len(*cand)))

	var mask uint64
	if node.Level == LevelStore {
		if len(*cand) > 0 && node.StoreIndex >= 0 && node.StoreIndex < 64 {
			mask |= 1 << uint(node.StoreIndex)
			tr.Path = append(tr.Path, fmt.Sprintf("%s ✓ 精确条件通过并投递", nodeID))
		}
		return mask
	}

	seenOwner := make(map[string]struct{}, len(*cand))
	owners := make([]string, 0, len(*cand))
	for _, h := range *cand {
		if int(h) >= len(node.Received) {
			continue
		}
		f := node.Received[h]
		owner := f.OwnerID
		if owner == "" {
			continue
		}
		if _, ok := seenOwner[owner]; ok {
			continue
		}
		seenOwner[owner] = struct{}{}
		if n.Nodes[owner] != nil {
			owners = append(owners, owner)
		}
	}

	// 先收集 owner 再递归：递归会复用同一个 cand 缓冲，不能在 range handles 中递归。
	for _, owner := range owners {
		if link := n.Links[owner]; link != nil {
			link.Down.Add(1)
		}
		tr.Path = append(tr.Path, fmt.Sprintf("%s ↓ 转发 %s", nodeID, owner))
		childMask := n.dispatchLocked(owner, msg, tr, cand)
		if childMask != 0 {
			if link := n.Links[owner]; link != nil {
				link.DownNecessary.Add(1)
			}
		}
		mask |= childMask
	}
	return mask
}
