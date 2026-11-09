package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeRedis é um servidor RESP mínimo: entende AUTH, PING e INFO.
type fakeRedis struct {
	ln       net.Listener
	user     string
	pass     string
	info     string
	infoRaw  string // se definido, enviado cru no lugar do bulk do INFO
	hangUp   bool   // fecha a conexão logo após responder o INFO cru
	closeAt  string // fecha a conexão ao receber este comando
	received []string
}

func startFakeRedis(t *testing.T, fr *fakeRedis) agentCheck {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fr.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fr.serve(conn)
	}()
	return agentCheck{Type: "redis", TargetHost: "127.0.0.1", TargetPort: ln.Addr().(*net.TCPAddr).Port}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if _, err := r.ReadString('\n'); err != nil { // $len
			return nil, err
		}
		arg, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		parts = append(parts, strings.TrimSuffix(arg, "\r\n"))
	}
	return parts, nil
}

func (fr *fakeRedis) serve(conn net.Conn) {
	r := bufio.NewReader(conn)
	authed := fr.pass == ""
	for {
		parts, err := readCommand(r)
		if err != nil {
			return
		}
		fr.received = append(fr.received, strings.Join(parts, " "))
		if parts[0] == fr.closeAt {
			return
		}
		var reply string
		hangUp := false
		switch parts[0] {
		case "AUTH":
			reply, authed = fr.authReply(parts)
		case "PING":
			reply = fr.pingReply(authed)
		case "INFO":
			reply, hangUp = fr.infoReply()
		}
		_, _ = io.WriteString(conn, reply)
		if hangUp {
			return
		}
	}
}

func (fr *fakeRedis) authReply(parts []string) (string, bool) {
	user, pass := "default", parts[len(parts)-1]
	if len(parts) == 3 {
		user = parts[1]
	}
	if pass == fr.pass && (fr.user == "" || user == fr.user) {
		return "+OK\r\n", true
	}
	return "-WRONGPASS invalid username-password pair\r\n", false
}

func (fr *fakeRedis) pingReply(authed bool) string {
	if !authed {
		return "-NOAUTH Authentication required.\r\n"
	}
	return "+PONG\r\n"
}

func (fr *fakeRedis) infoReply() (string, bool) {
	if fr.infoRaw != "" {
		return fr.infoRaw, fr.hangUp
	}
	return fmt.Sprintf("$%d\r\n%s\r\n", len(fr.info), fr.info), false
}

const sampleInfo = "# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\n# Memory\r\nused_memory:1048576\r\n" +
	"# Keyspace\r\ndb0:keys=12,expires=0,avg_ttl=0\r\ndb3:keys=4,expires=1,avg_ttl=10\r\n"

func TestRedisProbeOK(t *testing.T) {
	fr := &fakeRedis{info: sampleInfo}
	c := startFakeRedis(t, fr)
	d, err := redisProbe(net.DialTimeout, c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "7.2.4" || d.TotalSizeBytes != 1048576 || d.DatabaseCount != 2 ||
		d.Databases[0].Name != "db0 (12 chaves)" || d.Databases[1].Name != "db3 (4 chaves)" {
		t.Fatalf("details = %+v", d)
	}
	if strings.Join(fr.received, "|") != "PING|INFO" {
		t.Fatalf("sem senha não deveria autenticar: %v", fr.received)
	}
}

func TestRedisProbeAuth(t *testing.T) {
	t.Run("senha única", func(t *testing.T) {
		fr := &fakeRedis{pass: "s3cret", info: sampleInfo}
		c := startFakeRedis(t, fr)
		c.Password = "s3cret"
		if _, err := redisProbe(net.DialTimeout, c); err != nil {
			t.Fatal(err)
		}
		if fr.received[0] != "AUTH s3cret" {
			t.Fatalf("AUTH = %q", fr.received[0])
		}
	})
	t.Run("ACL usuário+senha", func(t *testing.T) {
		fr := &fakeRedis{user: "monitor", pass: "s3cret", info: sampleInfo}
		c := startFakeRedis(t, fr)
		c.Username, c.Password = "monitor", "s3cret"
		if _, err := redisProbe(net.DialTimeout, c); err != nil {
			t.Fatal(err)
		}
		if fr.received[0] != "AUTH monitor s3cret" {
			t.Fatalf("AUTH = %q", fr.received[0])
		}
	})
	t.Run("senha errada", func(t *testing.T) {
		fr := &fakeRedis{pass: "s3cret"}
		c := startFakeRedis(t, fr)
		c.Password = "errada"
		_, err := redisProbe(net.DialTimeout, c)
		if err == nil || err.Error() != "redis: WRONGPASS invalid username-password pair" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("conexão cai no AUTH", func(t *testing.T) {
		fr := &fakeRedis{pass: "x", closeAt: "AUTH"}
		c := startFakeRedis(t, fr)
		c.Password = "x"
		if _, err := redisProbe(net.DialTimeout, c); err == nil {
			t.Fatal("deveria falhar")
		}
	})
}

func TestRedisProbeFailures(t *testing.T) {
	t.Run("PING sem autenticação", func(t *testing.T) {
		c := startFakeRedis(t, &fakeRedis{pass: "precisa"})
		_, err := redisProbe(net.DialTimeout, c)
		if err == nil || !strings.Contains(err.Error(), "resposta inesperada") || !strings.Contains(err.Error(), "NOAUTH") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("conexão cai no PING", func(t *testing.T) {
		c := startFakeRedis(t, &fakeRedis{closeAt: "PING"})
		if _, err := redisProbe(net.DialTimeout, c); err == nil {
			t.Fatal("deveria falhar")
		}
	})
	t.Run("porta fechada", func(t *testing.T) {
		c := agentCheck{Type: "redis", TargetHost: "127.0.0.1", TargetPort: closedPort(t)}
		if _, err := redisProbe(net.DialTimeout, c); err == nil {
			t.Fatal("deveria falhar")
		}
	})
}

func TestRedisProbeInfoBestEffort(t *testing.T) {
	tests := []struct {
		name string
		fr   *fakeRedis
	}{
		{"INFO nulo", &fakeRedis{infoRaw: "$-1\r\n"}},
		{"INFO com erro", &fakeRedis{infoRaw: "-ERR unknown command\r\n"}},
		{"INFO com tamanho inválido", &fakeRedis{infoRaw: "$abc\r\n"}},
		{"INFO truncado", &fakeRedis{infoRaw: "$100\r\nredis_version:7", hangUp: true}},
		{"conexão cai no INFO", &fakeRedis{closeAt: "INFO"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := startFakeRedis(t, tt.fr)
			d, err := redisProbe(net.DialTimeout, c)
			if err != nil {
				t.Fatalf("PING ok = up, mesmo sem INFO: %v", err)
			}
			if d.Version != "" || d.DatabaseCount != 0 {
				t.Fatalf("sem INFO válido não há detalhes: %+v", d)
			}
		})
	}
}

func TestParseRedisInfo(t *testing.T) {
	d := &checkDetails{}
	parseRedisInfo("redis_version:6.0.0\nused_memory:abc\ndb1:keys=x\ndbsize:1\nlinha solta\n", d)
	if d.Version != "6.0.0" || d.TotalSizeBytes != 0 || len(d.Databases) != 1 || d.Databases[0].Name != "db1 (0 chaves)" {
		t.Fatalf("details = %+v", d)
	}
}
