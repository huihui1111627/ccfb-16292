package pawn

import (
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Violation struct {
	Seq          int      `json:"seq"`
	StoreID      string   `json:"store_id"`
	Kind         string   `json:"kind"` // missing_delivery / extra_delivery
	MatchingSubs []string `json:"matching_subs,omitempty"`
}

type VerifyReport struct {
	OK                   bool        `json:"ok"`
	MessagesChecked      int         `json:"messages_checked"`
	StoresChecked        int         `json:"stores_checked"`
	SubscriptionPairs    int64       `json:"subscription_pairs"`
	ActivePairsEvaluated int64       `json:"active_pairs_evaluated"`
	Violations           []Violation `json:"violations"`
	ElapsedMS            int64       `json:"elapsed_ms"`
}

func ActiveAt(p PlannedSub, seq int) bool {
	if p.AddSeq > seq {
		return false
	}
	return seq < p.DeleteSeq
}

// VerifyDirect 用完全独立于 Matcher/网络路由的逐条 Predicate.Matches oracle，
// 逐门店、逐消息核对 Simulation 记录的投递位掩码。
//
// maxMessages<=0 表示核对全部消息。消息按固定种子重新生成，分块送入各门店 worker。
func VerifyDirect(s *Simulation, maxMessages int) VerifyReport {
	start := time.Now()
	n := s.Cfg.MessageCount
	if maxMessages > 0 && maxMessages < n {
		n = maxMessages
	}

	byStore := make([][]PlannedSub, s.Cfg.StoreCount)
	var totalPairs int64
	for _, p := range s.Planned {
		// 不能用 S%02d：两位宽度会把 S100 错读成 10。
		if len(p.Sub.StoreID) < 2 || p.Sub.StoreID[0] != 'S' {
			continue
		}
		idx, err := strconv.Atoi(p.Sub.StoreID[1:])
		if err != nil || idx <= 0 || idx > s.Cfg.StoreCount {
			continue
		}
		si := idx - 1
		byStore[si] = append(byStore[si], p)
		totalPairs++
	}

	// 重新按固定种子走一遍生成器，保证 oracle 看到的是同样的消息序列。
	g := NewGenerator(s.Cfg)
	_ = g.PlannedSubs()

	report := VerifyReport{StoresChecked: s.Cfg.StoreCount, Violations: []Violation{}}
	var mu sync.Mutex
	var evaluated int64
	var violations []Violation

	chunkSize := 2048
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	checked := 0

	for startSeq := 0; startSeq < n; startSeq += chunkSize {
		end := startSeq + chunkSize
		if end > n {
			end = n
		}
		msgs := make([]Message, end-startSeq)
		for j := range msgs {
			msgs[j] = g.Message(startSeq + j)
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		for si := 0; si < s.Cfg.StoreCount; si++ {
			storeIdx := si
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				var localEval int64
				localViolations := []Violation{}
				for j, msg := range msgs {
					seq := startSeq + j
					actual := s.Masks[seq]&(1<<uint(storeIdx)) != 0
					expected := false
					var matchIDs []string
					subs := byStore[storeIdx]
					for k := range subs {
						p := subs[k]
						if !ActiveAt(p, seq) {
							continue
						}
						localEval++
						if p.Sub.Matches(&msg) {
							expected = true
							if len(matchIDs) < 8 {
								matchIDs = append(matchIDs, p.Sub.ID)
							} else if actual == expected {
								// 已确认命中且只需要样例；后续无需继续扫描。
								break
							}
						}
					}

					if expected != actual && len(localViolations) < 20 {
						v := Violation{Seq: seq, StoreID: StoreID(storeIdx)}
						if expected && !actual {
							v.Kind = "missing_delivery"
							v.MatchingSubs = matchIDs
						} else {
							v.Kind = "extra_delivery"
							// 多投说明 oracle 没找到命中；matchIDs 应为空。
							v.MatchingSubs = matchIDs
						}
						localViolations = append(localViolations, v)
					}
				}

				atomic.AddInt64(&evaluated, localEval)
				if len(localViolations) > 0 {
					mu.Lock()
					violations = append(violations, localViolations...)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		checked = end

		if len(violations) >= 50 {
			break
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Seq != violations[j].Seq {
			return violations[i].Seq < violations[j].Seq
		}
		return violations[i].StoreID < violations[j].StoreID
	})
	if len(violations) > 50 {
		violations = violations[:50]
	}
	report.MessagesChecked = checked
	report.SubscriptionPairs = totalPairs
	report.ActivePairsEvaluated = evaluated
	report.Violations = violations
	report.OK = len(violations) == 0
	report.ElapsedMS = time.Since(start).Milliseconds()
	return report
}
