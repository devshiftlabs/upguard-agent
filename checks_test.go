package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// closedPort devolve uma porta local sem ninguém escutando.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestFinishDetails(t *testing.T) {
	d := finishDetails(&checkDetails{Databases: []dbEntry{{"a", 10}, {"b", 30}, {"c", 20}, {"d", 30}}})
	want := []dbEntry{{"b", 30}, {"d", 30}, {"c", 20}, {"a", 10}} // decrescente e estável
	if !reflect.DeepEqual(d.Databases, want) || d.DatabaseCount != 4 || d.TotalSizeBytes != 90 {
		t.Fatalf("details = %+v", d)
	}
	// Total já informado (redis) é preservado; a lista é limitada a 50.
	var many []dbEntry
	for i := 0; i < 60; i++ {
		many = append(many, dbEntry{Name: fmt.Sprint(i), SizeBytes: int64(i)})
	}
	d = finishDetails(&checkDetails{TotalSizeBytes: 7, Databases: many})
	if d.DatabaseCount != 60 || len(d.Databases) != 50 || d.TotalSizeBytes != 7 || d.Databases[0].SizeBytes != 59 {
		t.Fatalf("limite/total: count=%d len=%d total=%d", d.DatabaseCount, len(d.Databases), d.TotalSizeBytes)
	}
	if d := finishDetails(&checkDetails{}); d.DatabaseCount != 0 || d.TotalSizeBytes != 0 {
		t.Fatalf("vazio: %+v", d)
	}
}

func TestTrimErr(t *testing.T) {
	if got := trimErr(errors.New("curto")); got != "curto" {
		t.Fatal(got)
	}
	if got := trimErr(errors.New(strings.Repeat("x", 500))); len(got) != 300 {
		t.Fatalf("len = %d", len(got))
	}
}

func TestDSNs(t *testing.T) {
	c := agentCheck{TargetHost: "db.local", TargetPort: 5432, Username: "mon@itor", Password: "p@ss:/w?rd"}
	if got := postgresDSN(c); got != "postgres://mon%40itor:p%40ss%3A%2Fw%3Frd@db.local:5432/postgres?sslmode=prefer&connect_timeout=10" {
		t.Errorf("postgres default db: %s", got)
	}
	c.DatabaseName = "app db"
	if got := postgresDSN(c); !strings.Contains(got, "@db.local:5432/app%20db?") {
		t.Errorf("postgres db: %s", got)
	}

	m := agentCheck{TargetHost: "10.0.0.1", TargetPort: 3306, Username: "u", Password: "p", DatabaseName: "shop"}
	if got := mysqlDSN(m); got != "u:p@tcp(10.0.0.1:3306)/shop?timeout=10s&readTimeout=10s&tls=preferred" {
		t.Errorf("mysql: %s", got)
	}

	s := agentCheck{TargetHost: "::1", TargetPort: 1433, Username: "sa", Password: "a b"}
	if got := sqlserverDSN(s); got != "sqlserver://sa:a%20b@[::1]:1433?dial+timeout=10" {
		t.Errorf("sqlserver sem db: %s", got)
	}
	s.DatabaseName = "master"
	if got := sqlserverDSN(s); got != "sqlserver://sa:a%20b@[::1]:1433?database=master&dial+timeout=10" {
		t.Errorf("sqlserver com db: %s", got)
	}
}

func TestProberDispatch(t *testing.T) {
	type sqlCall struct{ driver, dsn, vq, dq string }
	var sqlCalls []sqlCall
	var mongoURIs []string
	var dials []string
	p := prober{
		sql: func(driver, dsn, vq, dq string) (*checkDetails, error) {
			sqlCalls = append(sqlCalls, sqlCall{driver, dsn, vq, dq})
			return &checkDetails{Version: driver}, nil
		},
		mongo: func(uri string) (*checkDetails, error) {
			mongoURIs = append(mongoURIs, uri)
			return &checkDetails{Version: "mongo"}, nil
		},
		dial: func(network, addr string, timeout time.Duration) (net.Conn, error) {
			dials = append(dials, network+" "+addr+" "+timeout.String())
			return nil, errFake
		},
	}
	base := agentCheck{TargetHost: "h", TargetPort: 1}
	for typ, wantDriver := range map[string]string{"postgres": "pgx", "mysql": "mysql", "sqlserver": "sqlserver"} {
		c := base
		c.Type = typ
		d, err := p.probe(c)
		if err != nil || d.Version != wantDriver {
			t.Fatalf("%s: %+v %v", typ, d, err)
		}
	}
	wantQ := map[string][2]string{"pgx": {pgVersionQ, pgDBsQ}, "mysql": {myVersionQ, myDBsQ}, "sqlserver": {msVersionQ, msDBsQ}}
	for _, c := range sqlCalls {
		if q := wantQ[c.driver]; c.vq != q[0] || c.dq != q[1] {
			t.Fatalf("queries erradas para %s", c.driver)
		}
	}

	c := base
	c.Type = "mongodb"
	if d, err := p.probe(c); err != nil || d.Version != "mongo" || mongoURIs[0] != "mongodb://h:1" {
		t.Fatalf("mongo: %+v %v %v", d, err, mongoURIs)
	}
	c.Type = "redis"
	if _, err := p.probe(c); !errors.Is(err, errFake) {
		t.Fatalf("redis deveria usar o dial: %v", err)
	}
	c.Type = "tcp"
	if _, err := p.probe(c); !errors.Is(err, errFake) {
		t.Fatalf("tcp deveria usar o dial: %v", err)
	}
	if want := []string{"tcp h:1 10s", "tcp h:1 10s"}; !reflect.DeepEqual(dials, want) {
		t.Fatalf("dials = %v", dials)
	}
	c.Type = "oracle"
	if _, err := p.probe(c); err == nil || !strings.Contains(err.Error(), "desconhecido: oracle") {
		t.Fatalf("tipo desconhecido: %v", err)
	}
}

func TestDefaultProberTCPAndRealDrivers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go acceptAndClose(ln)
	port := ln.Addr().(*net.TCPAddr).Port
	p := defaultProber()

	if d, err := p.probe(agentCheck{Type: "tcp", TargetHost: "127.0.0.1", TargetPort: port}); err != nil || d != nil {
		t.Fatalf("tcp aberto: %+v %v", d, err)
	}
	dead := closedPort(t)
	if _, err := p.probe(agentCheck{Type: "tcp", TargetHost: "127.0.0.1", TargetPort: dead}); err == nil {
		t.Fatal("tcp fechado deveria falhar")
	}
	// Drivers reais contra uma porta fechada: falham rápido no ping.
	for _, typ := range []string{"postgres", "mysql", "sqlserver"} {
		if _, err := p.probe(agentCheck{Type: typ, TargetHost: "127.0.0.1", TargetPort: dead, Username: "u", Password: "p"}); err == nil {
			t.Fatalf("%s em porta fechada deveria falhar", typ)
		}
	}
}

func TestCheckInterval(t *testing.T) {
	for in, want := range map[int]time.Duration{0: 60 * time.Second, 14: 60 * time.Second, 15: 15 * time.Second, 300: 300 * time.Second} {
		if got := checkInterval(agentCheck{IntervalSeconds: in}); got != want {
			t.Errorf("checkInterval(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestCheckRunnerRunDueSchedules(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	var probed []string
	cr := &checkRunner{
		lastRun: map[string]time.Time{},
		now:     func() time.Time { return now },
		probe: func(c agentCheck) (*checkDetails, error) {
			probed = append(probed, c.ID)
			if c.ID == "bad" {
				return nil, errors.New(strings.Repeat("e", 400))
			}
			return &checkDetails{Version: "16"}, nil
		},
	}
	checks := []agentCheck{{ID: "fast", IntervalSeconds: 30}, {ID: "slow", IntervalSeconds: 300}, {ID: "bad", IntervalSeconds: 5}}

	res := cr.runDue(checks)
	if len(res) != 3 || !res[0].OK || res[0].Details.Version != "16" || res[2].OK || len(res[2].Error) != 300 {
		t.Fatalf("1ª rodada: %+v", res)
	}

	now = now.Add(40 * time.Second) // só o de 30s venceu
	if res := cr.runDue(checks); len(res) != 1 || res[0].CheckID != "fast" {
		t.Fatalf("40s depois: %+v", res)
	}
	now = now.Add(30 * time.Second) // 70s: fast (30s depois do último) e bad (intervalo <15s vira 60s)
	if res := cr.runDue(checks); len(res) != 2 || res[0].CheckID != "fast" || res[1].CheckID != "bad" {
		t.Fatalf("70s depois: %+v", res)
	}
	if want := []string{"fast", "slow", "bad", "fast", "fast", "bad"}; !reflect.DeepEqual(probed, want) {
		t.Fatalf("executados = %v", probed)
	}
}

func TestCheckRunnerCycle(t *testing.T) {
	srv := newAPIServer(t)
	cr := newCheckRunner()
	cr.probe = func(c agentCheck) (*checkDetails, error) { return nil, nil }

	// Sem hostID não chama o servidor.
	cr.cycle(srv.cfg(), srv.Client(), "")
	if len(srv.requests()) != 0 {
		t.Fatal("sem hostID não deveria buscar checks")
	}

	// Erro ao buscar.
	srv.handle("GET /api/agent/checks", jsonReply(500, ``))
	cr.cycle(srv.cfg(), srv.Client(), "h1")
	if countPath(srv.requests(), http.MethodPost, "/api/agent/checks/results") != 0 {
		t.Fatal("erro na busca não envia resultados")
	}

	// Lista vazia: nada a enviar.
	srv.handle("GET /api/agent/checks", jsonReply(200, `{"checks":[]}`))
	cr.cycle(srv.cfg(), srv.Client(), "h1")
	if countPath(srv.requests(), http.MethodPost, "/api/agent/checks/results") != 0 {
		t.Fatal("sem checks não envia resultados")
	}

	// Com check: envia; erro no envio só é logado.
	srv.handle("GET /api/agent/checks", jsonReply(200, `{"checks":[{"id":"x","type":"tcp"}]}`))
	srv.handle("POST /api/agent/checks/results", jsonReply(502, ``))
	cr.cycle(srv.cfg(), srv.Client(), "h1")
	reqs := srv.requests()
	last := reqs[len(reqs)-1]
	if last.path != "/api/agent/checks/results" || last.ctype != "application/json" || !last.authOK {
		t.Fatalf("envio de resultados inesperado: %+v", last)
	}
	var body map[string]any
	if err := json.Unmarshal(last.body, &body); err != nil || body["host_id"] != "h1" {
		t.Fatalf("corpo: %s", last.body)
	}
}

func TestFetchChecksErrors(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("GET /api/agent/checks", jsonReply(200, `{quebrado`))
	if _, err := fetchChecks(srv.cfg(), srv.Client(), "h"); err == nil {
		t.Fatal("JSON inválido deveria falhar")
	}
	srv.handle("GET /api/agent/checks", jsonReply(200, `{"checks":[{"id":"1","name":"pg","type":"postgres","target_host":"db","target_port":5432,"database_name":"app","username":"u","password":"p","interval_seconds":60}]}`))
	checks, err := fetchChecks(srv.cfg(), srv.Client(), "h&x=1")
	want := agentCheck{"1", "pg", "postgres", "db", 5432, "app", "u", "p", 60}
	if err != nil || len(checks) != 1 || checks[0] != want {
		t.Fatalf("checks = %+v, %v", checks, err)
	}
	if q := srv.requests()[1].query; q != "host_id=h%26x%3D1" {
		t.Fatalf("host_id deveria ser escapado: %q", q)
	}
	if err := sendCheckResults(srv.cfg(), srv.Client(), "h", []checkResult{{CheckID: "1"}}); err == nil {
		t.Fatal("rota inexistente (404) deveria falhar")
	}
}
