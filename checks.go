package main

// Checks privados: monitores de banco executados pelo agente dentro da rede
// do cliente (bancos sem IP público). O agente puxa os checks do servidor
// (GET /api/agent/checks, autenticado pela credencial, somente TLS), executa
// localmente e devolve apenas o resultado (ok/latência/erro) — as credenciais
// do banco nunca saem da memória do processo.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
)

const (
	checkTimeout = 10 * time.Second
	// minCheckInterval: abaixo disso o check roda a cada defaultCheckInterval.
	minCheckInterval     = 15 * time.Second
	defaultCheckInterval = 60 * time.Second
	maxDetailDatabases   = 50
	maxErrLen            = 300
)

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

// addr é host:port do alvo (IPv6 entre colchetes).
func (c agentCheck) addr() string {
	return net.JoinHostPort(c.TargetHost, fmt.Sprint(c.TargetPort))
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
	if len(d.Databases) > maxDetailDatabases {
		d.Databases = d.Databases[:maxDetailDatabases]
	}
	return d
}

// sortDBs ordena por tamanho decrescente (insertion sort estável).
func sortDBs(dbs []dbEntry) {
	for i := 1; i < len(dbs); i++ {
		for j := i; j > 0 && dbs[j].SizeBytes > dbs[j-1].SizeBytes; j-- {
			dbs[j], dbs[j-1] = dbs[j-1], dbs[j]
		}
	}
}

// fetchChecks puxa os checks habilitados deste host.
func fetchChecks(cfg config, client *http.Client, hostID string) ([]agentCheck, error) {
	var out struct {
		Checks []agentCheck `json:"checks"`
	}
	if err := apiCall(cfg, client, http.MethodGet, "/api/agent/checks?host_id="+url.QueryEscape(hostID), nil, &out, true); err != nil {
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
	return apiCall(cfg, client, http.MethodPost, "/api/agent/checks/results", body, nil, false)
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > maxErrLen {
		s = s[:maxErrLen]
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

func postgresDSN(c agentCheck) string {
	db := c.DatabaseName
	if db == "" {
		db = "postgres"
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=prefer&connect_timeout=10",
		url.QueryEscape(c.Username), url.QueryEscape(c.Password), c.addr(), url.PathEscape(db))
}

func mysqlDSN(c agentCheck) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s&readTimeout=10s&tls=preferred",
		c.Username, c.Password, c.addr(), c.DatabaseName)
}

func sqlserverDSN(c agentCheck) string {
	q := url.Values{}
	if c.DatabaseName != "" {
		q.Set("database", c.DatabaseName)
	}
	q.Set("dial timeout", "10")
	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(c.Username, c.Password),
		Host:     c.addr(),
		RawQuery: q.Encode(),
	}
	return u.String()
}

type dialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

// prober executa um check conforme o tipo. Os campos são as conexões reais
// (database/sql, driver do Mongo, TCP) e são trocados por fakes nos testes.
type prober struct {
	sql   func(driver, dsn, versionQ, dbsQ string) (*checkDetails, error)
	mongo func(uri string) (*checkDetails, error)
	dial  dialFunc
}

func defaultProber() prober {
	return prober{
		sql:   sqlProbe,
		mongo: func(uri string) (*checkDetails, error) { return mongoProbe(connectMongo, uri) },
		dial:  net.DialTimeout,
	}
}

func (p prober) probe(c agentCheck) (*checkDetails, error) {
	switch c.Type {
	case "postgres":
		return p.sql("pgx", postgresDSN(c), pgVersionQ, pgDBsQ)
	case "mysql":
		return p.sql("mysql", mysqlDSN(c), myVersionQ, myDBsQ)
	case "sqlserver":
		return p.sql("sqlserver", sqlserverDSN(c), msVersionQ, msDBsQ)
	case "mongodb":
		return p.mongo(mongoURI(c))
	case "redis":
		return redisProbe(p.dial, c)
	case networkTCP:
		conn, err := p.dial(networkTCP, c.addr(), checkTimeout)
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
	d.Databases = queryDatabases(ctx, db, dbsQ)
	return finishDetails(d), nil
}

// queryDatabases lê (nome, tamanho) de cada banco; linhas inválidas são puladas
// e tamanho NULL vira 0.
func queryDatabases(ctx context.Context, db *sql.DB, dbsQ string) []dbEntry {
	rows, err := db.QueryContext(ctx, dbsQ)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []dbEntry
	for rows.Next() {
		var e dbEntry
		var size sql.NullInt64
		if rows.Scan(&e.Name, &size) == nil {
			if size.Valid {
				e.SizeBytes = size.Int64
			}
			out = append(out, e)
		}
	}
	return out
}

// checkRunner mantém o agendamento por check (respeita interval_seconds).
type checkRunner struct {
	lastRun map[string]time.Time
	now     func() time.Time
	probe   func(agentCheck) (*checkDetails, error)
}

func newCheckRunner() *checkRunner {
	return &checkRunner{lastRun: map[string]time.Time{}, now: time.Now, probe: defaultProber().probe}
}

// runCheck executa um check e mede a latência.
func (cr *checkRunner) runCheck(c agentCheck) checkResult {
	start := time.Now()
	details, err := cr.probe(c)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return checkResult{CheckID: c.ID, OK: false, LatencyMs: lat, Error: trimErr(err)}
	}
	return checkResult{CheckID: c.ID, OK: true, LatencyMs: lat, Details: details}
}

// checkInterval é o intervalo efetivo do check (mínimo 15s; abaixo, 60s).
func checkInterval(c agentCheck) time.Duration {
	iv := time.Duration(c.IntervalSeconds) * time.Second
	if iv < minCheckInterval {
		return defaultCheckInterval
	}
	return iv
}

// runDue executa os checks cujo intervalo já venceu.
func (cr *checkRunner) runDue(checks []agentCheck) []checkResult {
	var results []checkResult
	now := cr.now()
	for _, c := range checks {
		if last, ok := cr.lastRun[c.ID]; ok && now.Sub(last) < checkInterval(c) {
			continue
		}
		cr.lastRun[c.ID] = now
		res := cr.runCheck(c)
		results = append(results, res)
		if res.OK {
			log.Printf("check %q (%s %s:%d): ok em %dms", c.Name, c.Type, c.TargetHost, c.TargetPort, res.LatencyMs)
		} else {
			log.Printf("check %q (%s %s:%d): FALHOU: %s", c.Name, c.Type, c.TargetHost, c.TargetPort, res.Error)
		}
	}
	return results
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
	results := cr.runDue(checks)
	if len(results) == 0 {
		return
	}
	if err := sendCheckResults(cfg, client, hostID, results); err != nil {
		log.Printf("checks: erro ao enviar resultados: %v", err)
	}
}
