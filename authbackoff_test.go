package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captureLog redireciona o log padrão durante o teste.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

func TestAuthRejection(t *testing.T) {
	tests := []struct {
		err      error
		wantCode int
		wantOK   bool
	}{
		{&statusError{who: "servidor", code: 401}, 401, true},
		{&statusError{who: "servidor", code: 403}, 403, true},
		{fmt.Errorf("envolto: %w", &statusError{code: 401}), 401, true},
		{&statusError{code: 503}, 0, false},
		{&statusError{code: 404}, 0, false},
		{errors.New("connection refused"), 0, false},
		{nil, 0, false},
	}
	for _, tt := range tests {
		code, ok := authRejection(tt.err)
		if code != tt.wantCode || ok != tt.wantOK {
			t.Errorf("authRejection(%v) = %d,%v want %d,%v", tt.err, code, ok, tt.wantCode, tt.wantOK)
		}
	}
}

func TestAuthBackoffNextGrowsAndCaps(t *testing.T) {
	b := authBackoff{jitter: func() float64 { return 0 }}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, time.Hour, time.Hour, time.Hour}
	for i, w := range want {
		if got := b.next(time.Minute); got != w {
			t.Fatalf("recusa %d: espera %s, want %s", i+1, got, w)
		}
	}
	if b.failures != len(want) {
		t.Fatalf("failures = %d", b.failures)
	}
	// Muitas recusas: continua no teto, sem estourar o Duration.
	b.failures = 500
	if got := b.next(time.Minute); got != time.Hour {
		t.Fatalf("depois de 500 recusas: %s", got)
	}
	if n := b.reset(); n != 501 || b.failures != 0 {
		t.Fatalf("reset devolveu %d, failures=%d", n, b.failures)
	}
	if n := b.reset(); n != 0 {
		t.Fatalf("reset sem recusas devolveu %d", n)
	}
}

func TestAuthBackoffBaseAndJitter(t *testing.T) {
	// Intervalo base maior (5 min) e base inválida (cai no padrão de 60 s).
	b := authBackoff{jitter: func() float64 { return 0 }}
	if got := b.next(5 * time.Minute); got != 10*time.Minute {
		t.Fatalf("base 5m: %s", got)
	}
	b = authBackoff{jitter: func() float64 { return 0 }}
	if got := b.next(0); got != 2*time.Minute {
		t.Fatalf("base 0: %s", got)
	}
	// Base acima do teto já começa no teto.
	b = authBackoff{jitter: func() float64 { return 0 }}
	if got := b.next(2 * time.Hour); got != time.Hour {
		t.Fatalf("base 2h: %s", got)
	}
	// Jitter máximo tira 20%.
	b = authBackoff{jitter: func() float64 { return 0.9999999 }}
	got := b.next(time.Minute)
	if got > 2*time.Minute || got < 96*time.Second {
		t.Fatalf("jitter: %s fora de [96s, 2m]", got)
	}
	// Jitter real (crypto/rand) fica sempre na faixa.
	for i := 0; i < 200; i++ {
		r := cryptoJitter()
		if r < 0 || r >= 1 {
			t.Fatalf("cryptoJitter = %v", r)
		}
	}
	b = authBackoff{} // sem jitter injetado usa o crypto/rand
	b.failures = 10
	if d := b.next(time.Minute); d > time.Hour || d < 48*time.Minute {
		t.Fatalf("espera com jitter real: %s", d)
	}
}

func TestAuthRejectedMessage(t *testing.T) {
	msg := authRejectedMessage(401, "agt_2a63aaaabbbbccccddddeeee", 3, 8*time.Minute+400*time.Millisecond, "linux")
	for _, part := range []string{
		"credencial recusada pelo UpGuard", "HTTP 401", "agt_2a63…",
		"confira CLIENT_ID/SECRET em /etc/upguard-agent/agent.env", "Recusa 3 seguida", "próxima tentativa em 8m0s",
	} {
		if !strings.Contains(msg, part) {
			t.Errorf("mensagem sem %q: %s", part, msg)
		}
	}
	if strings.Contains(msg, "bbbbccccddddeeee") {
		t.Errorf("mensagem com o client_id inteiro: %s", msg)
	}
	msg403 := authRejectedMessage(403, "agt_x", 1, time.Minute, "windows")
	for _, part := range []string{"HTTP 403", "limite de itens do plano", "client_id agt_x;", "--client-id/--client-secret"} {
		if !strings.Contains(msg403, part) {
			t.Errorf("mensagem 403 sem %q: %s", part, msg403)
		}
	}
}

// Ponta a ponta com httptest: recusas seguidas aumentam a espera, o log
// explica o que conferir (sem o secret) e, quando a credencial volta a valer,
// o agente retoma o intervalo do portal e os checks.
func TestAgentTickAuthRejectedBacksOffAndRecovers(t *testing.T) {
	logs := captureLog(t)
	srv := newAPIServer(t)
	var status atomic.Int32
	status.Store(http.StatusUnauthorized)
	srv.handle("POST /api/agent/metrics", func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			jsonReply(code, `{"message":"Invalid agent credentials"}`)(w, r)
			return
		}
		metricsReply("host-9", 60, false)(w, r)
	})
	srv.handle("GET /api/agent/checks", jsonReply(200, `{"checks":[]}`))

	a, _, _ := testAgent(t, srv)
	a.auth.jitter = func() float64 { return 0 }

	var got []time.Duration
	for i := 0; i < 7; i++ {
		got = append(got, a.tick())
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, time.Hour, time.Hour}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("esperas = %v, want %v", got, want)
		}
	}
	if countPath(srv.requests(), http.MethodGet, "/api/agent/checks") != 0 {
		t.Fatal("com a credencial recusada não deveria buscar os checks")
	}
	out := logs.String()
	if !strings.Contains(out, "credencial recusada pelo UpGuard: HTTP 401") ||
		!strings.Contains(out, "confira CLIENT_ID/SECRET") {
		t.Fatalf("log não explica a recusa:\n%s", out)
	}
	if strings.Contains(out, a.cfg.clientSecret) {
		t.Fatalf("log imprimiu o secret:\n%s", out)
	}
	if strings.Contains(out, "erro ao enviar métricas") {
		t.Fatalf("recusa não deveria cair no log genérico:\n%s", out)
	}

	// 403 também recua (continua a sequência).
	status.Store(http.StatusForbidden)
	if d := a.tick(); d != time.Hour {
		t.Fatalf("403: espera %s", d)
	}

	// A credencial volta a valer: intervalo normal, checks e log de retomada.
	status.Store(http.StatusOK)
	if d := a.tick(); d != 60*time.Second {
		t.Fatalf("depois de aceita: espera %s, want 1m", d)
	}
	if a.auth.failures != 0 {
		t.Fatalf("failures = %d depois do sucesso", a.auth.failures)
	}
	if countPath(srv.requests(), http.MethodGet, "/api/agent/checks") != 1 {
		t.Fatal("depois de aceita deveria rodar os checks")
	}
	if !strings.Contains(logs.String(), "credencial aceita pelo UpGuard depois de 8 recusa(s)") {
		t.Fatalf("sem log de retomada:\n%s", logs.String())
	}

	// Nova recusa depois do sucesso recomeça do começo (2× o intervalo).
	status.Store(http.StatusUnauthorized)
	if d := a.tick(); d != 2*time.Minute {
		t.Fatalf("recusa nova: espera %s, want 2m", d)
	}
}

// Erro que não é de credencial (503, rede) mantém o intervalo e não mexe no contador.
func TestAgentTickOtherErrorsKeepInterval(t *testing.T) {
	captureLog(t)
	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", jsonReply(503, ``))
	a, _, _ := testAgent(t, srv)
	a.auth.failures = 2
	if d := a.tick(); d != 60*time.Second {
		t.Fatalf("503: espera %s", d)
	}
	if a.auth.failures != 2 {
		t.Fatalf("503 alterou o contador de recusas: %d", a.auth.failures)
	}
}
