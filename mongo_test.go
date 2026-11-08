package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMongoURI(t *testing.T) {
	tests := []struct {
		c    agentCheck
		want string
	}{
		{agentCheck{TargetHost: "m.local", TargetPort: 27017}, "mongodb://m.local:27017"},
		{agentCheck{TargetHost: "m.local", TargetPort: 27017, Username: "mon", Password: "p@ss"},
			"mongodb://mon:p%40ss@m.local:27017?authSource=admin"},
		{agentCheck{TargetHost: "::1", TargetPort: 27018, Username: "mon", Password: "x", DatabaseName: "app"},
			"mongodb://mon:x@[::1]:27018?authSource=app"},
	}
	for _, tt := range tests {
		if got := mongoURI(tt.c); got != tt.want {
			t.Errorf("mongoURI(%+v) = %q, want %q", tt.c, got, tt.want)
		}
	}
}

type fakeMongo struct {
	pingErr      error
	version      string
	dbs          []dbEntry
	dbsErr       error
	disconnected bool
}

func (f *fakeMongo) Ping(context.Context) error          { return f.pingErr }
func (f *fakeMongo) BuildVersion(context.Context) string { return f.version }
func (f *fakeMongo) Databases(context.Context) ([]dbEntry, error) {
	return f.dbs, f.dbsErr
}
func (f *fakeMongo) Disconnect(context.Context) error { f.disconnected = true; return nil }

func connectTo(f *fakeMongo, wantURI string, t *testing.T) mongoConnectFunc {
	return func(ctx context.Context, uri string) (mongoSession, error) {
		if uri != wantURI {
			t.Fatalf("uri = %q, want %q", uri, wantURI)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("conexão deveria ter timeout")
		}
		return f, nil
	}
}

func TestMongoProbe(t *testing.T) {
	f := &fakeMongo{version: "7.0.5", dbs: []dbEntry{{"admin", 10}, {"app", 900}, {"local", 90}}}
	d, err := mongoProbe(connectTo(f, "mongodb://x:1", t), "mongodb://x:1")
	if err != nil {
		t.Fatal(err)
	}
	want := &checkDetails{Version: "7.0.5", DatabaseCount: 3, TotalSizeBytes: 1000,
		Databases: []dbEntry{{"app", 900}, {"local", 90}, {"admin", 10}}}
	if !reflect.DeepEqual(d, want) || !f.disconnected {
		t.Fatalf("got %+v (desconectou=%v)", d, f.disconnected)
	}
}

func TestMongoProbeBestEffortAndErrors(t *testing.T) {
	f := &fakeMongo{dbsErr: errors.New("not authorized")}
	d, err := mongoProbe(connectTo(f, "u", t), "u")
	if err != nil || d.Version != "" || d.DatabaseCount != 0 {
		t.Fatalf("sem permissão de listar: %+v %v", d, err)
	}

	f = &fakeMongo{pingErr: errors.New("server selection timeout")}
	if _, err := mongoProbe(connectTo(f, "u", t), "u"); err == nil || !f.disconnected {
		t.Fatalf("ping falhou: err=%v desconectou=%v", err, f.disconnected)
	}

	failConnect := func(context.Context, string) (mongoSession, error) { return nil, errors.New("uri inválida") }
	if _, err := mongoProbe(failConnect, "u"); err == nil {
		t.Fatal("falha de conexão deveria propagar")
	}
}

func TestDefaultProberMongoUsesDriver(t *testing.T) {
	if _, err := defaultProber().mongo("mongodb://%zz"); err == nil {
		t.Fatal("URI inválida deveria falhar já no driver")
	}
}

// TestConnectMongoReal usa o driver oficial contra uma porta sem servidor: a
// conexão é preguiçosa, então cada operação falha pelo prazo do contexto.
func TestConnectMongoReal(t *testing.T) {
	if _, err := connectMongo(context.Background(), "mongodb://%zz"); err == nil {
		t.Fatal("URI inválida deveria falhar")
	}
	s, err := connectMongo(context.Background(), mongoURI(agentCheck{TargetHost: "127.0.0.1", TargetPort: closedPort(t)}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.Ping(ctx); err == nil {
		t.Fatal("ping sem servidor deveria falhar")
	}
	if v := s.BuildVersion(ctx); v != "" {
		t.Fatalf("versão sem servidor = %q", v)
	}
	if _, err := s.Databases(ctx); err == nil {
		t.Fatal("listar bancos sem servidor deveria falhar")
	}
	if err := s.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
}
