package pawn

import (
	"fmt"
	"sort"
)

type FilterView struct {
	ID                string   `json:"id"`
	OwnerID           string   `json:"owner_id,omitempty"`
	MissingAsMatch    bool     `json:"missing_as_match"`
	ConditionCount    int      `json:"condition_count"`
	Conditions        []string `json:"conditions"`
	SourceIDs         []string `json:"source_ids,omitempty"`
	MergedCount       int      `json:"merged_count"`
	EstimatedOverhead float64  `json:"estimated_overhead"`
}

func NewFilterView(f Filter) FilterView {
	conds := make([]string, 0, len(f.Clauses))
	for i := range f.Clauses {
		conds = append(conds, f.Clauses[i].Explain())
	}
	return FilterView{
		ID: f.ID, OwnerID: f.OwnerID, MissingAsMatch: f.MissingAsMatch,
		ConditionCount: len(f.Clauses), Conditions: conds,
		SourceIDs:   append([]string{}, f.SourceIDs...),
		MergedCount: f.MergedCount, EstimatedOverhead: f.EstimatedOverhead,
	}
}

type NodeStatsView struct {
	MatchCalls     int64   `json:"match_calls"`
	Candidates     int64   `json:"candidates"`
	MatchedEntries int64   `json:"matched_entries"`
	TotalMatchNS   int64   `json:"total_match_ns"`
	MaxMatchNS     int64   `json:"max_match_ns"`
	AvgMatchMicros float64 `json:"avg_match_micros"`
}

type NodeView struct {
	ID              string        `json:"id"`
	Level           Level         `json:"level"`
	ParentID        string        `json:"parent_id"`
	Children        []string      `json:"children"`
	RawSubCount     int           `json:"raw_sub_count"`
	ReceivedCount   int           `json:"received_count"`
	ReportedCount   int           `json:"reported_count"`
	Stats           NodeStatsView `json:"stats"`
	ReceivedPreview []FilterView  `json:"received_preview"`
	ReportedPreview []FilterView  `json:"reported_preview"`
}

type LinkView struct {
	Key           string  `json:"key"`
	ParentID      string  `json:"parent_id"`
	ChildID       string  `json:"child_id"`
	Up            int64   `json:"up"`
	Down          int64   `json:"down"`
	Necessary     int64   `json:"necessary"`
	Extra         int64   `json:"extra"`
	OverheadRatio float64 `json:"overhead_ratio"`
	Retired       bool    `json:"retired,omitempty"`
}

type NetworkSnapshot struct {
	RootID       string        `json:"root_id"`
	Tolerance    float64       `json:"tolerance"`
	Nodes        []NodeView    `json:"nodes"`
	Links        []LinkView    `json:"links"`
	RetiredLinks []LinkView    `json:"retired_links"`
	Events       []EventRecord `json:"events"`
}

func statsView(n *Node) NodeStatsView {
	calls := n.Stats.MatchCalls.Load()
	total := n.Stats.MatchNSTotal.Load()
	v := NodeStatsView{
		MatchCalls: calls, Candidates: n.Stats.Candidates.Load(),
		MatchedEntries: n.Stats.MatchedEntries.Load(),
		TotalMatchNS:   total, MaxMatchNS: n.Stats.MatchNSMax.Load(),
	}
	if calls > 0 {
		v.AvgMatchMicros = float64(total) / float64(calls) / 1000.0
	}
	return v
}

func previewFilters(fs []Filter, limit int) []FilterView {
	if limit < 0 || limit > len(fs) {
		limit = len(fs)
	}
	out := make([]FilterView, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, NewFilterView(fs[i]))
	}
	return out
}

func (n *Network) Snapshot(previewLimit int) NetworkSnapshot {
	n.mu.RLock()
	defer n.mu.RUnlock()

	ids := make([]string, 0, len(n.Nodes))
	for id := range n.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	nodes := make([]NodeView, 0, len(ids))
	for _, id := range ids {
		node := n.Nodes[id]
		raw := 0
		if node.Subs != nil {
			raw = len(node.Subs)
		}
		children := append([]string{}, node.ChildIDs...)
		sort.Strings(children)
		nodes = append(nodes, NodeView{
			ID: id, Level: node.Level, ParentID: node.ParentID, Children: children,
			RawSubCount: raw, ReceivedCount: len(node.Received), ReportedCount: len(node.Reported),
			Stats:           statsView(node),
			ReceivedPreview: previewFilters(node.Received, previewLimit),
			ReportedPreview: previewFilters(node.Reported, previewLimit),
		})
	}

	childIDs := make([]string, 0, len(n.Links))
	for id := range n.Links {
		childIDs = append(childIDs, id)
	}
	sort.Strings(childIDs)
	links := make([]LinkView, 0, len(childIDs))
	for _, cid := range childIDs {
		links = append(links, linkView(n.Links[cid], false))
	}
	retired := append([]LinkView{}, n.RetiredLinks...)

	events := append([]EventRecord{}, n.EventLog...)
	sort.Slice(events, func(i, j int) bool { return events[i].Seq > events[j].Seq })
	if len(events) > 40 {
		events = events[len(events)-40:]
	}
	// 上面的排序对 Seq=-1 的运行时事件不够理想；直接保留追加顺序的最后 40 条并反转。
	events = append([]EventRecord{}, n.EventLog...)
	if len(events) > 40 {
		events = events[len(events)-40:]
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}

	return NetworkSnapshot{
		RootID: n.RootID, Tolerance: n.Tolerance, Nodes: nodes,
		Links: links, RetiredLinks: retired, Events: events,
	}
}

func linkView(l *Link, retired bool) LinkView {
	down := l.Down.Load()
	necessary := l.DownNecessary.Load()
	extra := down - necessary
	if extra < 0 {
		extra = 0
	}
	ratio := 0.0
	if necessary > 0 {
		ratio = float64(extra) / float64(necessary)
	}
	return LinkView{
		Key: l.Key, ParentID: l.ParentID, ChildID: l.ChildID,
		Up: l.Up.Load(), Down: down, Necessary: necessary,
		Extra: extra, OverheadRatio: ratio, Retired: retired,
	}
}

type NodeDetail struct {
	NodeView
	Received []FilterView `json:"received"`
	Reported []FilterView `json:"reported"`
}

func (n *Network) NodeDetail(id string, offset, limit int) (NodeDetail, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	node, ok := n.Nodes[id]
	if !ok {
		return NodeDetail{}, fmt.Errorf("node %s not found", id)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	slice := func(fs []Filter) []FilterView {
		if offset < 0 {
			offset = 0
		}
		if offset >= len(fs) {
			return []FilterView{}
		}
		end := offset + limit
		if end > len(fs) {
			end = len(fs)
		}
		return previewFilters(fs[offset:end], end-offset)
	}
	v := NodeView{
		ID: id, Level: node.Level, ParentID: node.ParentID,
		Children:    append([]string{}, node.ChildIDs...),
		RawSubCount: len(node.Subs), ReceivedCount: len(node.Received), ReportedCount: len(node.Reported),
		Stats: statsView(node),
	}
	return NodeDetail{NodeView: v, Received: slice(node.Received), Reported: slice(node.Reported)}, nil
}

type StoreLevelTrace struct {
	NodeID   string       `json:"node_id"`
	Level    Level        `json:"level"`
	Title    string       `json:"title"`
	Received []FilterView `json:"received"`
	Merged   []FilterView `json:"merged"`
}

type StoreTrace struct {
	StoreID  string            `json:"store_id"`
	RawCount int               `json:"raw_count"`
	Raw      []FilterView      `json:"raw_preview"`
	Levels   []StoreLevelTrace `json:"levels"`
}

func sourceIDsOrSelf(f Filter) []string {
	if len(f.SourceIDs) > 0 {
		return f.SourceIDs
	}
	return []string{f.ID}
}

func intersectsAny(set map[string]struct{}, ids []string) bool {
	for _, id := range ids {
		if _, ok := set[id]; ok {
			return true
		}
	}
	return false
}

// StoreTrace 追踪一家门店的原始订阅在门店代理、区域代理、HQ 每一级被合并成了什么。
func (n *Network) StoreTrace(storeID string, rawLimit int) (StoreTrace, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	node, ok := n.Nodes[storeID]
	if !ok || node.Level != LevelStore {
		return StoreTrace{}, fmt.Errorf("store %s not found", storeID)
	}
	if rawLimit <= 0 || rawLimit > 500 {
		rawLimit = 80
	}

	rawIDs := make([]string, 0, len(node.Subs))
	for id := range node.Subs {
		rawIDs = append(rawIDs, id)
	}
	sort.Strings(rawIDs)
	rawPreview := make([]FilterView, 0, min(rawLimit, len(rawIDs)))
	for i, id := range rawIDs {
		if i >= rawLimit {
			break
		}
		rawPreview = append(rawPreview, NewFilterView(subToFilter(node.Subs[id])))
	}

	tr := StoreTrace{StoreID: storeID, RawCount: len(node.Subs), Raw: rawPreview}

	// 门店代理：收到原始订阅，输出 Reported。
	storeMerged := make([]FilterView, 0, len(node.Reported))
	currentIDs := map[string]struct{}{}
	for _, f := range node.Reported {
		storeMerged = append(storeMerged, NewFilterView(f))
		currentIDs[f.ID] = struct{}{}
	}
	tr.Levels = append(tr.Levels, StoreLevelTrace{
		NodeID: node.ID, Level: node.Level,
		Title:    fmt.Sprintf("门店代理 %s：%d 条原始订阅合并为 %d 条上报", node.ID, len(node.Subs), len(node.Reported)),
		Received: rawPreview, Merged: storeMerged,
	})

	childID := storeID
	parentID := node.ParentID
	for parentID != "" {
		parent := n.Nodes[parentID]
		if parent == nil {
			break
		}

		receivedFromChild := []FilterView{}
		for _, f := range parent.Received {
			if f.OwnerID == childID {
				receivedFromChild = append(receivedFromChild, NewFilterView(f))
			}
		}

		mergedHere := []FilterView{}
		nextIDs := map[string]struct{}{}
		for _, f := range parent.Reported {
			ids := sourceIDsOrSelf(f)
			if intersectsAny(currentIDs, ids) || currentIDs[f.ID] == struct{}{} {
				mergedHere = append(mergedHere, NewFilterView(f))
				nextIDs[f.ID] = struct{}{}
			}
		}

		title := fmt.Sprintf("%s 代理：收到下级 %s 的 %d 条上报，相关条件被并入 %d 条本级上报",
			parentID, childID, len(receivedFromChild), len(mergedHere))
		if parent.Level == LevelHQ {
			title = fmt.Sprintf("总部 %s：收到 %s 的 %d 条上报；根节点不再向上转发，右侧为同算法合并预览/路由表",
				parentID, childID, len(receivedFromChild))
		}
		tr.Levels = append(tr.Levels, StoreLevelTrace{
			NodeID: parentID, Level: parent.Level, Title: title,
			Received: receivedFromChild, Merged: mergedHere,
		})

		childID = parentID
		currentIDs = nextIDs
		parentID = parent.ParentID
	}

	return tr, nil
}
