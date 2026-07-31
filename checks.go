package main

// Checks privados: monitores de banco executados pelo agente dentro da rede
// do cliente (bancos sem IP público). O agente puxa os checks do servidor
// (GET /api/agent/checks, autenticado pela credencial, somente TLS), executa
// localmente e devolve apenas o resultado (ok/latência/erro) — as credenciais
// do banco nunca saem da memória do processo.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

const checkTimeout = 10 * time.Second

type agentCheck struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	TargetHost      string `json:"target_host"`
	TargetPort      int    `json:"target_port"`
	DatabaseName    string `json:"database_name"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	IntervalSeconds int    `json:"interval_seconds"`
}

type checkResult struct {
	CheckID   string `json:"check_id"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// fetchChecks puxa os checks habilitados deste host.
func fetchChecks(cfg config, client *http.Client, hostID string) ([]agentCheck, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		cfg.server+"/api/agent/checks?host_id="+url.QueryEscape(hostID), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("servidor respondeu %d", resp.StatusCode)
	}
	var out struct {
		Checks []agentCheck `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Checks, nil
}

// sendCheckResults devolve os resultados ao servidor.
func sendCheckResults(cfg config, client *http.Client, hostID string, results []checkResult) error {
	body, err := json.Marshal(map[string]any{"host_id": hostID, "results": results})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.server+"/api/agent/checks/results", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("servidor respondeu %d", resp.StatusCode)
	}
	return nil
}

// runCheck executa um check e mede a latência.
func runCheck(c agentCheck) checkResult {
	start := time.Now()
	err := probeTarget(c)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return checkResult{CheckID: c.ID, OK: false, LatencyMs: lat, Error: trimErr(err)}
	}
	return checkResult{CheckID: c.ID, OK: true, LatencyMs: lat}
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func probeTarget(c agentCheck) error {
	switch c.Type {
	case "postgres":
		db := c.DatabaseName
		if db == "" {
			db = "postgres"
		}
		dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=prefer&connect_timeout=10",
			url.QueryEscape(c.Username), url.QueryEscape(c.Password),
			net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), url.PathEscape(db))
		return sqlPing("pgx", dsn)
	case "mysql":
		dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s&readTimeout=10s&tls=preferred",
			c.Username, c.Password,
			net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), c.DatabaseName)
		return sqlPing("mysql", dsn)
	case "sqlserver":
		q := url.Values{}
		if c.DatabaseName != "" {
			q.Set("database", c.DatabaseName)
		}
		q.Set("dial timeout", "10")
		u := &url.URL{
			Scheme:   "sqlserver",
			User:     url.UserPassword(c.Username, c.Password),
			Host:     net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)),
			RawQuery: q.Encode(),
		}
		return sqlPing("sqlserver", u.String())
	case "mongodb":
		return mongoPing(c)
	case "redis":
		return redisPing(c)
	case "tcp":
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), checkTimeout)
		if err != nil {
			return err
		}
		return conn.Close()
	default:
		return fmt.Errorf("tipo de check desconhecido: %s", c.Type)
	}
}

func sqlPing(driver, dsn string) error {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetConnMaxLifetime(checkTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	return db.PingContext(ctx)
}

func mongoPing(c agentCheck) error {
	u := &url.URL{Scheme: "mongodb", Host: net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort))}
	if c.Username != "" {
		u.User = url.UserPassword(c.Username, c.Password)
		q := url.Values{}
		auth := c.DatabaseName
		if auth == "" {
			auth = "admin"
		}
		q.Set("authSource", auth)
		u.RawQuery = q.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().
		ApplyURI(u.String()).
		SetConnectTimeout(checkTimeout).
		SetServerSelectionTimeout(checkTimeout))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	return client.Ping(ctx, readpref.Primary())
}

// redisPing fala RESP puro (sem dependência): AUTH opcional + PING.
func redisPing(c agentCheck) error {
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), checkTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(checkTimeout))
	r := bufio.NewReader(conn)

	cmd := func(parts ...string) (string, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "*%d\r\n", len(parts))
		for _, p := range parts {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
		}
		if _, err := conn.Write([]byte(b.String())); err != nil {
			return "", err
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	if c.Password != "" {
		var reply string
		if c.Username != "" {
			reply, err = cmd("AUTH", c.Username, c.Password)
		} else {
			reply, err = cmd("AUTH", c.Password)
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(reply, "-") {
			return fmt.Errorf("redis: %s", strings.TrimPrefix(reply, "-"))
		}
	}
	reply, err := cmd("PING")
	if err != nil {
		return err
	}
	if reply != "+PONG" {
		return fmt.Errorf("redis: resposta inesperada %q", reply)
	}
	return nil
}

// checkRunner mantém o agendamento por check (respeita interval_seconds).
type checkRunner struct {
	lastRun map[string]time.Time
}

func newCheckRunner() *checkRunner {
	return &checkRunner{lastRun: map[string]time.Time{}}
}

// cycle puxa os checks e executa os que estão no vencimento.
func (cr *checkRunner) cycle(cfg config, client *http.Client, hostID string) {
	if hostID == "" {
		return
	}
	checks, err := fetchChecks(cfg, client, hostID)
	if err != nil {
		log.Printf("checks: erro ao buscar: %v", err)
		return
	}
	if len(checks) == 0 {
		return
	}
	var results []checkResult
	now := time.Now()
	for _, c := range checks {
		iv := time.Duration(c.IntervalSeconds) * time.Second
		if iv < 15*time.Second {
			iv = 60 * time.Second
		}
		if last, ok := cr.lastRun[c.ID]; ok && now.Sub(last) < iv {
			continue
		}
		cr.lastRun[c.ID] = now
		res := runCheck(c)
		results = append(results, res)
		if res.OK {
			log.Printf("check %q (%s %s:%d): ok em %dms", c.Name, c.Type, c.TargetHost, c.TargetPort, res.LatencyMs)
		} else {
			log.Printf("check %q (%s %s:%d): FALHOU: %s", c.Name, c.Type, c.TargetHost, c.TargetPort, res.Error)
		}
	}
	if len(results) > 0 {
		if err := sendCheckResults(cfg, client, hostID, results); err != nil {
			log.Printf("checks: erro ao enviar resultados: %v", err)
		}
	}
}
