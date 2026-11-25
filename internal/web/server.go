package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"sync"

	"pawnrouter/internal/pawn"
)

var errNotReady = errors.New("仿真尚未初始化完成")

//go:embed index.html
var indexHTML []byte

//go:embed app.js styles.css
var assets embed.FS

type Server struct {
	mux *http.ServeMux

	simMu sync.RWMutex
	sim   *pawn.Simulation

	verifyMu    sync.RWMutex
	verifyRunMu sync.Mutex
	report      *pawn.VerifyReport
}

func NewServer() *Server {
	s := &Server{mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) SetSimulation(sim *pawn.Simulation) {
	s.simMu.Lock()
	s.sim = sim
	s.simMu.Unlock()
}

func (s *Server) getSim() *pawn.Simulation {
	s.simMu.RLock()
	defer s.simMu.RUnlock()
	return s.sim
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	sub, _ := fs.Sub(assets, ".")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/node", s.handleNode)
	s.mux.HandleFunc("/api/store-trace", s.handleStoreTrace)
	s.mux.HandleFunc("/api/tolerance", s.handleTolerance)
	s.mux.HandleFunc("/api/proxy/add", s.handleProxyAdd)
	s.mux.HandleFunc("/api/proxy/remove", s.handleProxyRemove)
	s.mux.HandleFunc("/api/proxy/move", s.handleProxyMove)
	s.mux.HandleFunc("/api/replay", s.handleReplay)
	s.mux.HandleFunc("/api/verify", s.handleVerify)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
}

type stateResponse struct {
	Ready     bool                  `json:"ready"`
	Config    pawn.Config           `json:"config"`
	Progress  int64                 `json:"progress"`
	Running   bool                  `json:"running"`
	Done      bool                  `json:"done"`
	Network   *pawn.NetworkSnapshot `json:"network,omitempty"`
	Hist      pawn.HistogramView    `json:"hist"`
	Benchmark pawn.Benchmark        `json:"benchmark"`
	Recent    []pawn.Trace          `json:"recent"`
	Report    *pawn.VerifyReport    `json:"report,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeJSON(w, stateResponse{Ready: false})
		return
	}
	snap := sim.Net.Snapshot(5)
	bm := sim.BenchmarkView()
	s.verifyMu.RLock()
	report := s.report
	s.verifyMu.RUnlock()

	writeJSON(w, stateResponse{
		Ready: true, Config: sim.Cfg, Progress: sim.Progress(),
		Running: sim.IsRunning(), Done: sim.IsDone(),
		Network: &snap, Hist: sim.Hist.View(), Benchmark: bm,
		Recent: sim.RecentTraces(), Report: report,
	})
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	id := r.URL.Query().Get("id")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	d, err := sim.Net.NodeDetail(id, offset, limit)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, d)
}

func (s *Server) handleStoreTrace(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	store := r.URL.Query().Get("store")
	if store == "" {
		store = "S01"
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("raw_limit"))
	tr, err := sim.Net.StoreTrace(store, limit)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, tr)
}

type toleranceRequest struct {
	Tolerance *float64 `json:"tolerance"`
	Percent   *float64 `json:"percent"`
}

func (s *Server) handleTolerance(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	var req toleranceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := 0.1
	switch {
	case req.Tolerance != nil:
		t = *req.Tolerance
	case req.Percent != nil:
		t = *req.Percent / 100.0
	}
	if t < 0 {
		t = 0
	}
	sim.Net.SetTolerance(t)
	writeJSON(w, map[string]any{"ok": true, "tolerance": t})
}

type proxyRequest struct {
	ID          string `json:"id"`
	ParentID    string `json:"parent_id"`
	NewParentID string `json:"new_parent_id"`
}

func (s *Server) handleProxyAdd(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	var req proxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ParentID == "" {
		req.ParentID = "HQ"
	}
	if err := sim.Net.AddProxy(req.ID, req.ParentID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleProxyRemove(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	var req proxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := sim.Net.RemoveProxy(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleProxyMove(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	var req proxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.NewParentID == "" {
		req.NewParentID = req.ParentID
	}
	if err := sim.Net.MoveProxy(req.ID, req.NewParentID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	_, tr := sim.ReplayRandom()
	writeJSON(w, tr)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.verifyMu.RLock()
		rep := s.report
		s.verifyMu.RUnlock()
		writeJSON(w, rep)
		return
	}

	sim := s.getSim()
	if sim == nil {
		writeError(w, http.StatusServiceUnavailable, errNotReady)
		return
	}
	if !sim.IsDone() {
		writeError(w, http.StatusConflict, errors.New("固定种子消息仍在回放；请等待进度完成后再核对"))
		return
	}
	if !s.verifyRunMu.TryLock() {
		writeError(w, http.StatusConflict, errors.New("已有核对任务正在运行"))
		return
	}
	defer s.verifyRunMu.Unlock()

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// 核对可能较久；这个演示接口默认核对前 20000 条，全量用 cmd/verify。
	if limit == 0 {
		limit = 20_000
	}
	s.verifyMu.Lock()
	s.report = nil
	s.verifyMu.Unlock()

	rep := pawn.VerifyDirect(sim, limit)

	s.verifyMu.Lock()
	s.report = &rep
	s.verifyMu.Unlock()
	writeJSON(w, rep)
}
