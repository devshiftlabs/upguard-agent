package main

// Driver database/sql fake (em memória) para exercitar sqlProbe de verdade:
// abre conexão, faz ping, roda a query de versão e a de bancos e lê as linhas.
// O comportamento é escolhido pelo DSN.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

const fakeDriverName = "upguard-fake-sql"

func init() { sql.Register(fakeDriverName, fakeDriver{}) }

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	if strings.Contains(dsn, "openfail") {
		return nil, errors.New("conexão recusada")
	}
	return &fakeSQLConn{dsn: dsn}, nil
}

type fakeSQLConn struct{ dsn string }

func (c *fakeSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare não suportado")
}
func (c *fakeSQLConn) Close() error              { return nil }
func (c *fakeSQLConn) Begin() (driver.Tx, error) { return nil, errors.New("sem transações") }

func (c *fakeSQLConn) Ping(context.Context) error {
	if strings.Contains(c.dsn, "pingfail") {
		return errors.New("autenticação falhou")
	}
	return nil
}

func (c *fakeSQLConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch query {
	case "VERSION_Q":
		if strings.Contains(c.dsn, "noversion") {
			return nil, errors.New("permissão negada")
		}
		return &fakeRows{cols: []string{"v"}, data: [][]driver.Value{{"16.2"}}}, nil
	case "DBS_Q":
		if strings.Contains(c.dsn, "nodbs") {
			return nil, errors.New("permissão negada")
		}
		return &fakeRows{cols: []string{"name", "size"}, data: [][]driver.Value{
			{"app", int64(100)},
			{"vazio", nil},  // tamanho NULL vira 0
			{nil, int64(1)}, // nome NULL: linha inválida, pulada
			{"grande", int64(500)},
		}}, nil
	}
	return nil, errors.New("query inesperada: " + query)
}

type fakeRows struct {
	cols []string
	data [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.i])
	r.i++
	return nil
}

func TestSQLProbe(t *testing.T) {
	d, err := sqlProbe(fakeDriverName, "ok", "VERSION_Q", "DBS_Q")
	if err != nil {
		t.Fatal(err)
	}
	want := &checkDetails{Version: "16.2", DatabaseCount: 3, TotalSizeBytes: 600,
		Databases: []dbEntry{{"grande", 500}, {"app", 100}, {"vazio", 0}}}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("got %+v, want %+v", d, want)
	}
}

func TestSQLProbeBestEffortMetadata(t *testing.T) {
	// Sem permissão para metadados o check continua up, só sem detalhes.
	d, err := sqlProbe(fakeDriverName, "noversion nodbs", "VERSION_Q", "DBS_Q")
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "" || d.DatabaseCount != 0 || d.Databases != nil {
		t.Fatalf("sem permissão: %+v", d)
	}
}

func TestSQLProbeErrors(t *testing.T) {
	for _, dsn := range []string{"pingfail", "openfail"} {
		if _, err := sqlProbe(fakeDriverName, dsn, "VERSION_Q", "DBS_Q"); err == nil {
			t.Errorf("%s deveria falhar", dsn)
		}
	}
	if _, err := sqlProbe("driver-que-nao-existe", "x", "", ""); err == nil {
		t.Error("driver desconhecido deveria falhar")
	}
}

func TestRunCheckWithSQLProbe(t *testing.T) {
	cr := newCheckRunner()
	cr.probe = prober{sql: func(_, dsn, _, _ string) (*checkDetails, error) {
		// Encaminha para o driver fake mantendo o caminho real de sqlProbe.
		if strings.Contains(dsn, "senha-errada") {
			return sqlProbe(fakeDriverName, "pingfail", "VERSION_Q", "DBS_Q")
		}
		return sqlProbe(fakeDriverName, "ok", "VERSION_Q", "DBS_Q")
	}}.probe
	ok := cr.runCheck(agentCheck{ID: "pg1", Type: "postgres", TargetHost: "db", TargetPort: 5432, Password: "certa"})
	if !ok.OK || ok.CheckID != "pg1" || ok.Details == nil || ok.Details.Version != "16.2" || ok.Error != "" {
		t.Fatalf("ok = %+v", ok)
	}
	bad := cr.runCheck(agentCheck{ID: "pg2", Type: "postgres", TargetHost: "db", TargetPort: 5432, Password: "senha-errada"})
	if bad.OK || bad.Error != "autenticação falhou" || bad.Details != nil {
		t.Fatalf("bad = %+v", bad)
	}
}
