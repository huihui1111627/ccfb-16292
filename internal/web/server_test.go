package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"pawnrouter/internal/pawn"
)

func TestStateEndpointConcurrentWithSimulation(t *testing.T) {
	cfg := pawn.SmallConfig()
	cfg.MessageCount = 4000
	sim, err := pawn.NewSimulation(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer()
	srv.SetSimulation(sim)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				res, err := http.Get(ts.URL + "/api/state")
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
		}()
	}

	sim.Run()
	wg.Wait()

	res, err := http.Post(ts.URL+"/api/verify?limit=1000", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("verify status=%d body=%s", res.StatusCode, string(body))
	}
}
