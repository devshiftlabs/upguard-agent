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
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	"go.mongodb.org/mongo-driver/bson"
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
	CheckID   string        `json:"check_id"`
	OK        bool          `json:"ok"`
	LatencyMs int64         `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	Details   *checkDetails `json:"details,omitempty"`
}

// checkDetails: metadados do servidor de banco coletados no check (best-effort:
// requer permissão de leitura — recomendado usuário somente-leitura).
type checkDetails struct {
	Version        string    `json:"version,omitempty"`
	DatabaseCount  int       `json:"database_count,omitempty"`
	TotalSizeBytes int64     `json:"total_size_bytes,omitempty"`
	Databases      []dbEntry `json:"databases,omitempty"`
}

type dbEntry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// finishDetails ordena por tamanho, calcula totais e limita a lista.
func finishDetails(d *checkDetails) *checkDetails {
	sortDBs(d.Databases)
	d.DatabaseCount = len(d.Databases)
	if d.TotalSizeBytes == 0 { // redis já traz used_memory
		var total int64
		for _, e := range d.Databases {
			total += e.SizeBytes
		}
		d.TotalSizeBytes = total
	}
	if len(d.Databases) > 50 {
		d.Databases = d.Databases[:50]
	}
	return d
}

func sortDBs(dbs []dbEntry) {
	for i := 1; i < len(dbs); i++ {
		for j := i; j > 0 && dbs[j].SizeBytes > dbs[j-1].SizeBytes; j-- {
			dbs[j], dbs[j-1] = dbs[j-1], dbs[j]
		}
	}
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
	details, err := probeTarget(c)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return checkResult{CheckID: c.ID, OK: false, LatencyMs: lat, Error: trimErr(err)}
	}
	return checkResult{CheckID: c.ID, OK: true, LatencyMs: lat, Details: details}
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Queries de metadados por tipo (best-effort; requer usuário com leitura).
const (
	pgVersionQ = `SHOW server_version`
	pgDBsQ     = `SELECT datname, pg_database_size(datname)::bigint
	              FROM pg_database WHERE datistemplate = false`
	myVersionQ = `SELECT VERSION()`
	myDBsQ     = `SELECT s.schema_name,
	                     COALESCE(CAST(SUM(t.data_length + t.index_length) AS UNSIGNED), 0)
	              FROM information_schema.schemata s
	              LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name
	              GROUP BY s.schema_name`
	msVersionQ = `SELECT CAST(SERVERPROPERTY('productversion') AS VARCHAR(64))`
	msDBsQ     = `SELECT d.name, COALESCE(SUM(CAST(mf.size AS BIGINT)) * 8192, 0)
	              FROM sys.databases d
	              LEFT JOIN sys.master_files mf ON mf.database_id = d.database_id
	              GROUP BY d.name`
)

func probeTarget(c agentCheck) (*checkDetails, error) {
	switch c.Type {
	case "postgres":
		db := c.DatabaseName
		if db == "" {
			db = "postgres"
		}
		dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=prefer&connect_timeout=10",
			url.QueryEscape(c.Username), url.QueryEscape(c.Password),
			net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), url.PathEscape(db))
		return sqlProbe("pgx", dsn, pgVersionQ, pgDBsQ)
	case "mysql":
		dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s&readTimeout=10s&tls=preferred",
			c.Username, c.Password,
			net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), c.DatabaseName)
		return sqlProbe("mysql", dsn, myVersionQ, myDBsQ)
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
		return sqlProbe("sqlserver", u.String(), msVersionQ, msDBsQ)
	case "mongodb":
		return mongoProbe(c)
	case "redis":
		return redisProbe(c)
	case "tcp":
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), checkTimeout)
		if err != nil {
			return nil, err
		}
		return nil, conn.Close()
	default:
		return nil, fmt.Errorf("tipo de check desconhecido: %s", c.Type)
	}
}

// sqlProbe conecta (ping = up/down) e coleta versão + bancos/tamanhos.
// As queries de metadados são best-effort: sem permissão, o check segue up.
func sqlProbe(driver, dsn, versionQ, dbsQ string) (*checkDetails, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetConnMaxLifetime(checkTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	d := &checkDetails{}
	_ = db.QueryRowContext(ctx, versionQ).Scan(&d.Version)
	if rows, err := db.QueryContext(ctx, dbsQ); err == nil {
		defer rows.Close()
		for rows.Next() {
			var e dbEntry
			var size sql.NullInt64
			if rows.Scan(&e.Name, &size) == nil {
				if size.Valid {
					e.SizeBytes = size.Int64
				}
				d.Databases = append(d.Databases, e)
			}
		}
	}
	return finishDetails(d), nil
}

func mongoProbe(c agentCheck) (*checkDetails, error) {
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
		return nil, err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return nil, err
	}
	d := &checkDetails{}
	var bi struct {
		Version string `bson:"version"`
	}
	_ = client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&bi)
	d.Version = bi.Version
	if res, err := client.ListDatabases(ctx, bson.D{}); err == nil {
		for _, dbi := range res.Databases {
			d.Databases = append(d.Databases, dbEntry{Name: dbi.Name, SizeBytes: dbi.SizeOnDisk})
		}
	}
	return finishDetails(d), nil
}

// redisProbe fala RESP puro (sem dependência): AUTH opcional + PING + INFO
// (versão, memória usada e keyspaces — best-effort).
func redisProbe(c agentCheck) (*checkDetails, error) {
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort)), checkTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(checkTimeout))
	r := bufio.NewReader(conn)

	// cmd envia um comando RESP e devolve a primeira linha da resposta; para
	// respostas bulk ($N) lê também o corpo e o devolve.
	cmd := func(parts ...string) (string, string, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "*%d\r\n", len(parts))
		for _, p := range parts {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
		}
		if _, err := conn.Write([]byte(b.String())); err != nil {
			return "", "", err
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "$") && line != "$-1" {
			var n int
			if _, err := fmt.Sscanf(line, "$%d", &n); err == nil && n >= 0 {
				body := make([]byte, n+2) // payload + \r\n
				if _, err := io.ReadFull(r, body); err != nil {
					return line, "", err
				}
				return line, string(body[:n]), nil
			}
		}
		return line, "", nil
	}

	if c.Password != "" {
		var reply string
		if c.Username != "" {
			reply, _, err = cmd("AUTH", c.Username, c.Password)
		} else {
			reply, _, err = cmd("AUTH", c.Password)
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(reply, "-") {
			return nil, fmt.Errorf("redis: %s", strings.TrimPrefix(reply, "-"))
		}
	}
	reply, _, err := cmd("PING")
	if err != nil {
		return nil, err
	}
	if reply != "+PONG" {
		return nil, fmt.Errorf("redis: resposta inesperada %q", reply)
	}

	d := &checkDetails{}
	if _, info, err := cmd("INFO"); err == nil && info != "" {
		parseRedisInfo(info, d)
	}
	return finishDetails(d), nil
}

// parseRedisInfo extrai versão, memória usada e keyspaces do INFO.
func parseRedisInfo(info string, d *checkDetails) {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "redis_version:"):
			d.Version = strings.TrimPrefix(line, "redis_version:")
		case strings.HasPrefix(line, "used_memory:"):
			fmt.Sscanf(strings.TrimPrefix(line, "used_memory:"), "%d", &d.TotalSizeBytes)
		case strings.HasPrefix(line, "db") && strings.Contains(line, ":keys="):
			name, rest, _ := strings.Cut(line, ":")
			var keys int64
			fmt.Sscanf(rest, "keys=%d", &keys)
			d.Databases = append(d.Databases, dbEntry{
				Name: fmt.Sprintf("%s (%d chaves)", name, keys),
			})
		}
	}
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
