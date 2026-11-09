package main

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// recordedReq guarda o que o servidor fake recebeu.
type recordedReq struct {
	method, path, query, user, pass, ua, ctype string
	authOK                                     bool
	body                                       []byte
}

// apiServer é um servidor fake da API do UpGuard: registra as requisições e
// responde com o handler de cada rota.
type apiServer struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []recordedReq
	routes map[string]func(w http.ResponseWriter, r *http.Request)
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{routes: map[string]func(http.ResponseWriter, *http.Request){}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, ok := r.BasicAuth()
		s.mu.Lock()
		s.reqs = append(s.reqs, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, user, pass,
			r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), ok, body})
		h := s.routes[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *apiServer) handle(route string, h func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[route] = h
}

func (s *apiServer) requests() []recordedReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedReq(nil), s.reqs...)
}

func (s *apiServer) cfg() config {
	return config{clientID: "agt_test", clientSecret: "sk_test", server: s.URL}
}

func jsonReply(code int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func TestSendPostsPayloadWithBasicAuth(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", jsonReply(200,
		`{"host_id":"h-1","config":{"interval_seconds":30,"paused":true,"update_requested":true}}`))

	p := payload{Host: hostInfo{Hostname: "srv-a", OS: "linux"}, Metrics: &metrics{CPUPercent: 12.5, DiskTotal: 99}}
	sc, err := send(srv.cfg(), srv.Client(), p)
	if err != nil {
		t.Fatal(err)
	}
	if sc != (serverConfig{hostID: "h-1", interval: 30, paused: true, updateRequested: true}) {
		t.Fatalf("config do servidor = %+v", sc)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("esperava 1 requisição, got %d", len(reqs))
	}
	r := reqs[0]
	if !r.authOK || r.user != "agt_test" || r.pass != "sk_test" {
		t.Fatalf("basic auth incorreto: %+v", r)
	}
	if r.ctype != "application/json" || r.ua != userAgent {
		t.Fatalf("headers incorretos: ctype=%q ua=%q", r.ctype, r.ua)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	if got["host"]["hostname"] != "srv-a" || got["metrics"]["cpu_percent"] != 12.5 || got["metrics"]["disk_total_bytes"] != 99.0 {
		t.Fatalf("payload inesperado: %s", r.body)
	}
}

func TestSendPausedOmitsMetricsField(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", jsonReply(200, `{}`))
	if _, err := send(srv.cfg(), srv.Client(), payload{Host: hostInfo{Hostname: "x"}}); err != nil {
		t.Fatal(err)
	}
	if body := string(srv.requests()[0].body); strings.Contains(body, `"metrics"`) {
		t.Fatalf("heartbeat não deveria ter metrics: %s", body)
	}
}

func TestSendErrors(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("POST /api/agent/metrics", jsonReply(401, `{"error":"invalid"}`))
	_, err := send(srv.cfg(), srv.Client(), payload{})
	var se *statusError
	if !errors.As(err, &se) || se.code != 401 || err.Error() != "servidor respondeu 401" {
		t.Fatalf("esperava statusError 401, got %v", err)
	}

	// 2xx com corpo inválido: sucesso com config vazia (comportamento tolerante).
	srv.handle("POST /api/agent/metrics", jsonReply(200, `não é json`))
	sc, err := send(srv.cfg(), srv.Client(), payload{})
	if err != nil || sc != (serverConfig{}) {
		t.Fatalf("corpo inválido: sc=%+v err=%v", sc, err)
	}

	// Payload não serializável (NaN) falha antes de qualquer requisição.
	n := len(srv.requests())
	if _, err := send(srv.cfg(), srv.Client(), payload{Metrics: &metrics{CPUPercent: math.NaN()}}); err == nil {
		t.Fatal("NaN deveria falhar no json.Marshal")
	}
	if len(srv.requests()) != n {
		t.Fatal("não deveria ter enviado requisição")
	}

	// URL inválida e servidor fora do ar.
	if _, err := send(config{server: "://bad"}, srv.Client(), payload{}); err == nil {
		t.Fatal("URL inválida deveria falhar")
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	if _, err := send(config{server: down.URL}, http.DefaultClient, payload{}); err == nil {
		t.Fatal("servidor fora do ar deveria falhar")
	}
}

func TestFetchProbes(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("GET /api/agent/probes", jsonReply(200,
		`{"probes":[{"name":"Nginx","bin":"nginx","args":["-v"],"use_stderr":true}]}`))
	probes, ok := fetchProbes(srv.cfg(), srv.Client())
	want := []probe{{Name: "Nginx", Bin: "nginx", Args: []string{"-v"}, UseStderr: true}}
	if !ok || !reflect.DeepEqual(probes, want) {
		t.Fatalf("probes = %+v ok=%v", probes, ok)
	}
	r := srv.requests()[0]
	if !r.authOK || r.user != "agt_test" || r.ctype != "" {
		t.Fatalf("requisição de probes inesperada: %+v", r)
	}

	srv.handle("GET /api/agent/probes", jsonReply(500, `{}`))
	if _, ok := fetchProbes(srv.cfg(), srv.Client()); ok {
		t.Fatal("500 deveria falhar")
	}
	srv.handle("GET /api/agent/probes", jsonReply(200, `{bad`))
	if _, ok := fetchProbes(srv.cfg(), srv.Client()); ok {
		t.Fatal("JSON inválido deveria falhar")
	}
}
