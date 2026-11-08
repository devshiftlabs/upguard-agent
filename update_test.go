package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionLess(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.2.3", "1.2.4", true},
		{"1.2.3", "1.3.0", true},
		{"1.9.9", "2.0.0", true},
		{"1.10.0", "1.9.0", false},
		{"1.2.3", "1.2.3", false},
		{"2.0.0", "1.9.9", false},
		{"dev", "1.0.0", true}, // build local sempre se considera mais antigo
		{"1.2", "1.2.1", true},
		{"1.2.3-rc1", "1.2.3", false},
	}
	for _, tt := range tests {
		if got := versionLess(tt.a, tt.b); got != tt.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
	if parseVer("4.5.6.7") != [3]int{4, 5, 6} {
		t.Errorf("parseVer com 4 partes = %v", parseVer("4.5.6.7"))
	}
}

// releaseServer simula a API do GitHub (latest) e o download do binário.
type releaseServer struct {
	*httptest.Server
	tag       string
	latestErr int
	binCode   int
	binBody   string
	downloads []string
	uas       []string
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{tag: "v1.4.0", binCode: 200, binBody: "#!binario-novo"}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.uas = append(rs.uas, r.Header.Get("User-Agent"))
		switch {
		case r.URL.Path == "/latest":
			if r.Header.Get("Accept") != "application/vnd.github+json" {
				http.Error(w, "accept", 400)
				return
			}
			if rs.latestErr != 0 {
				w.WriteHeader(rs.latestErr)
				return
			}
			_, _ = io.WriteString(w, `{"tag_name":"`+rs.tag+`"}`)
		case strings.HasPrefix(r.URL.Path, "/download/"):
			rs.downloads = append(rs.downloads, strings.TrimPrefix(r.URL.Path, "/download/"))
			w.WriteHeader(rs.binCode)
			_, _ = io.WriteString(w, rs.binBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rs.Close)
	return rs
}

type execCall struct {
	argv0 string
	argv  []string
}

// testUpdater aponta o updater para o servidor fake e para um executável
// temporário; reexec/exit são registrados em vez de trocar o processo.
func testUpdater(t *testing.T, rs *releaseServer, goos, current string) (*updater, string, *[]execCall, *[]int) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "upguard-agent")
	if err := os.WriteFile(exe, []byte("binario-antigo"), 0o755); err != nil {
		t.Fatal(err)
	}
	var execs []execCall
	var exits []int
	u := newUpdater(rs.Client())
	u.latestURL = rs.URL + "/latest"
	u.binBase = rs.URL + "/download"
	u.current = current
	u.goos, u.goarch = goos, "arm64"
	u.executable = func() (string, error) { return exe, nil }
	u.reexec = func(argv0 string, argv []string, _ []string) error {
		execs = append(execs, execCall{argv0, argv})
		return nil
	}
	u.exit = func(code int) { exits = append(exits, code) }
	return u, exe, &execs, &exits
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewUpdaterDefaults(t *testing.T) {
	u := newUpdater(http.DefaultClient)
	if u.latestURL != ghLatestAPI || u.binBase != releaseBinBase || u.current != version ||
		u.executable == nil || u.reexec == nil || u.exit == nil {
		t.Fatalf("defaults inesperados: %+v", u)
	}
}

func TestLatestVersion(t *testing.T) {
	rs := newReleaseServer(t)
	u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
	if v, err := u.latestVersion(); err != nil || v != "1.4.0" {
		t.Fatalf("latestVersion = %q, %v", v, err)
	}
	if rs.uas[0] != userAgent {
		t.Fatalf("User-Agent = %q", rs.uas[0])
	}
	rs.latestErr = 403
	if _, err := u.latestVersion(); err == nil || err.Error() != "github respondeu 403" {
		t.Fatalf("esperava erro 403, got %v", err)
	}
	rs.latestErr = 0
	rs.tag = `\x` // escape inválido no JSON
	if _, err := u.latestVersion(); err == nil {
		t.Fatal("JSON inválido deveria falhar")
	}
	u.latestURL = "://bad"
	if _, err := u.latestVersion(); err == nil {
		t.Fatal("URL inválida deveria falhar")
	}
}

func TestMaybeUpdateNewVersionReexecs(t *testing.T) {
	rs := newReleaseServer(t)
	u, exe, execs, exits := testUpdater(t, rs, "linux", "1.3.9")
	u.maybeUpdate(false)
	if len(rs.downloads) != 1 || rs.downloads[0] != "upguard-agent-linux-arm64" {
		t.Fatalf("downloads = %v", rs.downloads)
	}
	if got := readFile(t, exe); got != "#!binario-novo" {
		t.Fatalf("binário não substituído: %q", got)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("o arquivo temporário deveria ter sido renomeado")
	}
	if len(*execs) != 1 || (*execs)[0].argv0 != exe || len(*exits) != 0 {
		t.Fatalf("deveria reexecutar o binário: execs=%v exits=%v", *execs, *exits)
	}
}

func TestMaybeUpdateUpToDateDoesNothing(t *testing.T) {
	rs := newReleaseServer(t)
	for _, cur := range []string{"1.4.0", "1.5.0"} {
		u, exe, execs, _ := testUpdater(t, rs, "linux", cur)
		u.maybeUpdate(false)
		if len(rs.downloads) != 0 || len(*execs) != 0 || readFile(t, exe) != "binario-antigo" {
			t.Fatalf("versão %s não deveria atualizar", cur)
		}
	}
	rs.latestErr = 500
	u, _, execs, _ := testUpdater(t, rs, "linux", "0.0.1")
	u.maybeUpdate(false)
	if len(rs.downloads) != 0 || len(*execs) != 0 {
		t.Fatal("falha ao consultar a release não deveria atualizar")
	}
}

func TestMaybeUpdateForcedSkipsVersionCheck(t *testing.T) {
	rs := newReleaseServer(t)
	rs.latestErr = 500 // nem consulta
	u, exe, execs, _ := testUpdater(t, rs, "darwin", "9.9.9")
	u.maybeUpdate(true)
	if len(rs.downloads) != 1 || rs.downloads[0] != "upguard-agent-darwin-arm64" || len(*execs) != 1 {
		t.Fatalf("forçado deveria baixar e reexecutar: downloads=%v execs=%v", rs.downloads, *execs)
	}
	if readFile(t, exe) != "#!binario-novo" {
		t.Fatal("binário não substituído")
	}
}

func TestSelfUpdateWindowsExits(t *testing.T) {
	rs := newReleaseServer(t)
	u, exe, execs, exits := testUpdater(t, rs, goosWindows, "1.0.0")
	if err := u.selfUpdate(); err != nil {
		t.Fatal(err)
	}
	if rs.downloads[0] != "upguard-agent-windows-arm64.exe" {
		t.Fatalf("Windows deveria baixar o .exe: %v", rs.downloads)
	}
	if len(*exits) != 1 || (*exits)[0] != 0 || len(*execs) != 0 {
		t.Fatalf("Windows deveria sair com 0 (serviço reinicia): exits=%v execs=%v", *exits, *execs)
	}
	if readFile(t, exe) != "#!binario-novo" {
		t.Fatal("binário não substituído")
	}
}

func TestSelfUpdateErrors(t *testing.T) {
	rs := newReleaseServer(t)

	t.Run("download 404 mantém o binário", func(t *testing.T) {
		rs.binCode = 404
		defer func() { rs.binCode = 200 }()
		u, exe, execs, _ := testUpdater(t, rs, "linux", "1.0.0")
		err := u.selfUpdate()
		if err == nil || err.Error() != "download respondeu 404" {
			t.Fatalf("esperava erro 404, got %v", err)
		}
		if readFile(t, exe) != "binario-antigo" || len(*execs) != 0 {
			t.Fatal("falha no download não pode trocar o binário")
		}
		u.maybeUpdate(true) // só loga a falha
	})

	t.Run("executável desconhecido", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.executable = func() (string, error) { return "", errors.New("sem executável") }
		if err := u.selfUpdate(); err == nil {
			t.Fatal("deveria falhar")
		}
	})

	t.Run("diretório sem permissão de escrita", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		missing := filepath.Join(t.TempDir(), "nao-existe", "upguard-agent")
		u.executable = func() (string, error) { return missing, nil }
		if err := u.selfUpdate(); err == nil {
			t.Fatal("deveria falhar ao criar o temporário")
		}
	})

	t.Run("rename falha", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		dir := filepath.Join(t.TempDir(), "agente-e-diretorio")
		if err := os.MkdirAll(filepath.Join(dir, "conteudo"), 0o755); err != nil {
			t.Fatal(err)
		}
		u.executable = func() (string, error) { return dir, nil } // destino é diretório não vazio
		if err := u.selfUpdate(); err == nil {
			t.Fatal("rename sobre diretório não vazio deveria falhar")
		}
	})

	t.Run("reexec falha", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.reexec = func(string, []string, []string) error { return errors.New("exec negado") }
		if err := u.selfUpdate(); err == nil || err.Error() != "exec negado" {
			t.Fatalf("erro do exec deveria propagar, got %v", err)
		}
	})

	t.Run("conexão cai no meio do download", func(t *testing.T) {
		cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "parcial")
		}))
		defer cut.Close()
		u, exe, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.client = cut.Client()
		u.binBase = cut.URL
		if err := u.selfUpdate(); err == nil {
			t.Fatal("download truncado deveria falhar")
		}
		if readFile(t, exe) != "binario-antigo" {
			t.Fatal("download truncado não pode trocar o binário")
		}
	})

	t.Run("URL inválida", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.binBase = "://bad"
		if err := u.selfUpdate(); err == nil {
			t.Fatal("deveria falhar")
		}
	})

	t.Run("servidor fora do ar", func(t *testing.T) {
		down := httptest.NewServer(http.NotFoundHandler())
		down.Close()
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.binBase = down.URL
		if err := u.selfUpdate(); err == nil {
			t.Fatal("deveria falhar")
		}
	})
}
