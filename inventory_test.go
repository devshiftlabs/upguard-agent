package main

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	tests := []struct{ in, want string }{
		{"nginx version: nginx/1.25.3\n", "1.25.3"},
		{"\n\n  PostgreSQL 16.2 (Ubuntu)\nmais linhas 9.9", "16.2"},
		{"Python 3.12.1.4", "3.12.1.4"},
		{"OpenSSH_9.6p1", "9.6"},
		{"versão desconhecida\n", "versão desconhecida"},
		{"", "instalado"},
		{"   \n \n", "instalado"},
	}
	for _, tt := range tests {
		if got := parseVersion(tt.in); got != tt.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRunProbes(t *testing.T) {
	probes := []probe{
		{Name: "Nginx", Bin: "nginx", Args: []string{"-v"}, UseStderr: true},
		{Name: "Shell injection", Bin: "sh -c 'rm -rf /'"}, // nome inválido: nunca executa
		{Name: "Caminho", Bin: "/usr/bin/id"},              // caminho absoluto não é permitido
		{Name: "Ausente", Bin: "nao-instalado"},
		{Name: "Redis", Bin: "redis-server", Args: []string{"--version"}},
	}
	lookPath := func(bin string) (string, error) {
		if bin == "nao-instalado" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + bin, nil
	}
	var ran []string
	run := func(path string, args []string, useStderr bool) string {
		ran = append(ran, path+" "+strings.Join(args, " ")+map[bool]string{true: " (stderr)", false: ""}[useStderr])
		if strings.HasSuffix(path, "nginx") {
			return "nginx version: nginx/1.25.3"
		}
		return "Redis server v=7.2.4 sha=00000000:0"
	}
	got := runProbes(probes, lookPath, run)
	want := []sw{{Name: "Nginx", Version: "1.25.3"}, {Name: "Redis", Version: "7.2.4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	wantRan := []string{"/usr/bin/nginx -v (stderr)", "/usr/bin/redis-server --version"}
	if !reflect.DeepEqual(ran, wantRan) {
		t.Fatalf("executou %v, want %v", ran, wantRan)
	}
}

// TestRunProbeCmdReal executa um binário de verdade (o próprio go do teste).
func TestRunProbeCmdReal(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go fora do PATH")
	}
	if out := runProbeCmd(goBin, []string{"version"}, false); !strings.HasPrefix(out, "go version go") {
		t.Fatalf("stdout = %q", out)
	}
	if v := parseVersion(runProbeCmd(goBin, []string{"version"}, false)); !strings.Contains(v, ".") {
		t.Fatalf("versão do go não extraída: %q", v)
	}
	// Subcomando inválido escreve no stderr; com useStderr=false não captura nada.
	if out := runProbeCmd(goBin, []string{"subcomando-invalido"}, true); !strings.Contains(out, "subcomando-invalido") {
		t.Fatalf("stderr = %q", out)
	}
	if out := runProbeCmd(goBin, []string{"subcomando-invalido"}, false); out != "" {
		t.Fatalf("stdout deveria estar vazio, got %q", out)
	}
}

func TestSWInventoryCache(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	fetchOK := true
	fetches, runs := 0, 0
	inv := &swInventory{
		fetch: func() ([]probe, bool) {
			fetches++
			if !fetchOK {
				return nil, false
			}
			return []probe{{Name: "x", Bin: "x"}}, true
		},
		run: func(p []probe) []sw {
			runs++
			return []sw{{Name: p[0].Name, Version: time.Duration(runs).String()}}
		},
		now: func() time.Time { return now },
	}
	first := inv.get()
	if fetches != 1 || runs != 1 || len(first) != 1 {
		t.Fatalf("1ª chamada: fetches=%d runs=%d inv=%v", fetches, runs, first)
	}
	now = now.Add(59 * time.Minute)
	if got := inv.get(); fetches != 1 || !reflect.DeepEqual(got, first) {
		t.Fatal("dentro de 1h deveria usar o cache")
	}
	now = now.Add(2 * time.Minute)
	fetchOK = false
	if got := inv.get(); fetches != 2 || runs != 1 || !reflect.DeepEqual(got, first) {
		t.Fatal("busca falhou: deveria manter o inventário anterior")
	}
	fetchOK = true
	if got := inv.get(); fetches != 3 || runs != 2 || reflect.DeepEqual(got, first) {
		t.Fatal("cache vencido deveria reexecutar os probes")
	}
}

func TestSWInventoryNoCacheFetchFails(t *testing.T) {
	inv := &swInventory{
		fetch: func() ([]probe, bool) { return nil, false },
		run: func(p []probe) []sw {
			if p != nil {
				t.Fatal("sem probes não deveria haver o que rodar")
			}
			return nil
		},
		now: time.Now,
	}
	if inv.get() != nil {
		t.Fatal("sem probes o inventário fica vazio")
	}
	if again := inv.get(); again != nil {
		t.Fatal("sem cache, a próxima chamada tenta de novo e segue vazia")
	}
}

func TestNewSWInventoryUsesServerProbes(t *testing.T) {
	srv := newAPIServer(t)
	srv.handle("GET /api/agent/probes", jsonReply(200, `{"probes":[{"name":"Go","bin":"go","args":["version"]},{"name":"X","bin":"bin-que-nao-existe-123"}]}`))
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go fora do PATH")
	}
	got := newSWInventory(srv.cfg(), srv.Client()).get()
	if len(got) != 1 || got[0].Name != "Go" || !strings.Contains(got[0].Version, ".") {
		t.Fatalf("inventário = %+v", got)
	}
}
