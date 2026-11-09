package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeUpdater struct{ calls []bool }

func (f *fakeUpdater) maybeUpdate(force bool) { f.calls = append(f.calls, force) }

func TestLoopStateApply(t *testing.T) {
	tests := []struct {
		name       string
		start      loopState
		sc         serverConfig
		want       loopState
		wantUpdate []bool
	}{
		{"sem mudanças", loopState{60 * time.Second, false}, serverConfig{}, loopState{60 * time.Second, false}, nil},
		{"ajusta intervalo", loopState{60 * time.Second, false}, serverConfig{interval: 300}, loopState{300 * time.Second, false}, nil},
		{"ignora intervalo abaixo de 10s", loopState{60 * time.Second, false}, serverConfig{interval: 5}, loopState{60 * time.Second, false}, nil},
		{"pausa", loopState{60 * time.Second, false}, serverConfig{paused: true, interval: 60}, loopState{60 * time.Second, true}, nil},
		{"retoma", loopState{60 * time.Second, true}, serverConfig{paused: false}, loopState{60 * time.Second, false}, nil},
		{"update forçado", loopState{60 * time.Second, false}, serverConfig{updateRequested: true}, loopState{60 * time.Second, false}, []bool{true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := tt.start
			upd := &fakeUpdater{}
			st.apply(upd, tt.sc)
			if st != tt.want {
				t.Fatalf("estado = %+v, want %+v", st, tt.want)
			}
			if len(upd.calls) != len(tt.wantUpdate) || (len(upd.calls) == 1 && !upd.calls[0]) {
				t.Fatalf("updates = %v, want %v", upd.calls, tt.wantUpdate)
			}
		})
	}
}

func TestLoopStateNextDelay(t *testing.T) {
	tests := []struct {
		st   loopState
		want time.Duration
	}{
		{loopState{60 * time.Second, false}, 60 * time.Second},
		{loopState{60 * time.Second, true}, 30 * time.Second},
		{loopState{15 * time.Second, true}, 15 * time.Second},
		{loopState{10 * time.Minute, false}, 10 * time.Minute},
	}
	for _, tt := range tests {
		if got := tt.st.nextDelay(); got != tt.want {
			t.Errorf("nextDelay(%+v) = %s, want %s", tt.st, got, tt.want)
		}
	}
}

func TestLogSent(t *testing.T) {
	logSent(payload{Metrics: &metrics{CPUPercent: 1}})
	logSent(payload{})
}

// testAgent monta um agente com coleta fake, sem probes e apontando para srv.
func testAgent(t *testing.T, srv *apiServer) (*agent, *fakeUpdater, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	upd := &fakeUpdater{}
	a := newAgent(srv.cfg())
	a.cfg.interval = 60 * time.Second
	a.state.current = a.cfg.interval
	a.client = srv.Client()
	a.sys = fakeSys()
	a.inv = &swInventory{fetch: func() ([]probe, bool) { return nil, true }, run: func([]probe) []sw { return nil }, now: time.Now}
	a.upd = upd
	a.now = func() time.Time { return now }
	a.checks.now = a.now
	return a, upd, &now
}

func metricsReply(hostID string, interval int, paused bool) func(http.ResponseWriter, *http.Request) {
	body, _ := json.Marshal(map[string]any{"host_id": hostID,
		"config": map[string]any{"interval_seconds": interval, "paused": paused}})
	return jsonReply(200, string(body))
}

func countPath(reqs []recordedReq, method, path string) int {
	n := 0
	for _, r := range reqs {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func TestAgentTickFullCycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go acceptAndClose(ln)
	port := ln.Addr().(*net.TCPAddr).Port

	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", metricsReply("host-42", 120, false))
	checksBody, _ := json.Marshal(map[string]any{"checks": []agentCheck{
		{ID: "c1", Name: "porta", Type: "tcp", TargetHost: "127.0.0.1", TargetPort: port, IntervalSeconds: 60},
	}})
	srv.handle("GET /api/agent/checks", jsonReply(200, string(checksBody)))
	srv.handle("POST /api/agent/checks/results", jsonReply(200, `{}`))

	a, upd, now := testAgent(t, srv)
	if d := a.tick(); d != 120*time.Second {
		t.Fatalf("1º tick: próximo em %s, want 2m", d)
	}
	if len(upd.calls) != 1 || upd.calls[0] {
		t.Fatalf("1º ciclo deveria checar update (não forçado): %v", upd.calls)
	}
	if a.hostID != "host-42" {
		t.Fatalf("hostID = %q", a.hostID)
	}
	reqs := srv.requests()
	if reqs[1].path != "/api/agent/checks" || reqs[1].query != "host_id=host-42" {
		t.Fatalf("deveria buscar os checks do host: %+v", reqs[1])
	}
	var res struct {
		HostID  string        `json:"host_id"`
		Results []checkResult `json:"results"`
	}
	if err := json.Unmarshal(reqs[2].body, &res); err != nil {
		t.Fatal(err)
	}
	if res.HostID != "host-42" || len(res.Results) != 1 || !res.Results[0].OK || res.Results[0].CheckID != "c1" {
		t.Fatalf("resultado do check = %+v", res)
	}

	// 2º ciclo 1h depois: não checa update de novo (6h) e o hostID vazio na
	// resposta mantém o anterior.
	*now = now.Add(time.Hour)
	srv.handle("POST /api/agent/metrics", metricsReply("", 0, false))
	a.tick()
	if len(upd.calls) != 1 || a.hostID != "host-42" {
		t.Fatalf("2º ciclo: updates=%v hostID=%q", upd.calls, a.hostID)
	}

	// 7h depois do 1º: checa update de novo.
	*now = now.Add(6 * time.Hour)
	a.tick()
	if len(upd.calls) != 2 {
		t.Fatalf("após 6h deveria checar update de novo: %v", upd.calls)
	}
}

func TestAgentTickPausedSkipsChecks(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", metricsReply("host-1", 300, true))
	a, _, _ := testAgent(t, srv)
	if d := a.tick(); d != 30*time.Second {
		t.Fatalf("pausado deveria consultar a cada 30s, got %s", d)
	}
	reqs := srv.requests()
	if countPath(reqs, http.MethodGet, "/api/agent/checks") != 0 {
		t.Fatal("pausado não roda checks")
	}
	// O próximo envio já sai como heartbeat (sem métricas).
	a.tick()
	reqs = srv.requests()
	if strings.Contains(string(reqs[len(reqs)-1].body), `"metrics"`) {
		t.Fatal("pausado deveria enviar só heartbeat")
	}
}

func TestAgentTickSendError(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", jsonReply(503, ``))
	a, _, _ := testAgent(t, srv)
	if d := a.tick(); d != 60*time.Second {
		t.Fatalf("erro de envio mantém o intervalo atual, got %s", d)
	}
	if countPath(srv.requests(), http.MethodGet, "/api/agent/checks") != 0 {
		t.Fatal("sem resposta do servidor não roda checks")
	}
}

func TestAgentRunStopsOnCancel(t *testing.T) {
	srv := newAPIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv.handle("POST /api/agent/metrics", func(w http.ResponseWriter, r *http.Request) {
		cancel() // encerra o laço depois do 1º envio
		metricsReply("", 0, false)(w, r)
	})
	a, _, _ := testAgent(t, srv)
	done := make(chan struct{})
	go func() { a.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run não encerrou após o cancelamento")
	}
	if countPath(srv.requests(), http.MethodPost, "/api/agent/metrics") != 1 {
		t.Fatal("deveria ter feito exatamente 1 ciclo imediato")
	}
}
