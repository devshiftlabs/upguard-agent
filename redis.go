package main

// Check de Redis falando RESP puro (sem dependência): AUTH opcional + PING +
// INFO (versão, memória usada e keyspaces — best-effort).

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

type respConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// do envia um comando RESP e devolve a primeira linha da resposta; para
// respostas bulk ($N) lê também o corpo e o devolve.
func (rc *respConn) do(parts ...string) (line, body string, err error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	if _, err := rc.conn.Write([]byte(b.String())); err != nil {
		return "", "", err
	}
	line, err = rc.r.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "$") || line == "$-1" {
		return line, "", nil
	}
	var n int
	if _, err := fmt.Sscanf(line, "$%d", &n); err != nil || n < 0 {
		return line, "", nil
	}
	buf := make([]byte, n+2) // payload + \r\n
	if _, err := io.ReadFull(rc.r, buf); err != nil {
		return line, "", err
	}
	return line, string(buf[:n]), nil
}

// auth autentica se houver senha (ACL com usuário, ou senha única).
func (rc *respConn) auth(username, password string) error {
	if password == "" {
		return nil
	}
	args := []string{"AUTH", password}
	if username != "" {
		args = []string{"AUTH", username, password}
	}
	reply, _, err := rc.do(args...)
	if err != nil {
		return err
	}
	if strings.HasPrefix(reply, "-") {
		return fmt.Errorf("redis: %s", strings.TrimPrefix(reply, "-"))
	}
	return nil
}

func redisProbe(dial dialFunc, c agentCheck) (*checkDetails, error) {
	conn, err := dial(networkTCP, c.addr(), checkTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(checkTimeout))
	rc := &respConn{conn: conn, r: bufio.NewReader(conn)}

	if err := rc.auth(c.Username, c.Password); err != nil {
		return nil, err
	}
	reply, _, err := rc.do("PING")
	if err != nil {
		return nil, err
	}
	if reply != "+PONG" {
		return nil, fmt.Errorf("redis: resposta inesperada %q", reply)
	}

	d := &checkDetails{}
	if _, info, err := rc.do("INFO"); err == nil && info != "" {
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
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, "used_memory:"), "%d", &d.TotalSizeBytes)
		case strings.HasPrefix(line, "db") && strings.Contains(line, ":keys="):
			name, rest, _ := strings.Cut(line, ":")
			var keys int64
			_, _ = fmt.Sscanf(rest, "keys=%d", &keys)
			d.Databases = append(d.Databases, dbEntry{
				Name: fmt.Sprintf("%s (%d chaves)", name, keys),
			})
		}
	}
}
