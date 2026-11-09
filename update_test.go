package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/devshiftlabs/upguard-agent/internal/release"
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

func TestNormalizeUpdateURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://upguard.com.br/downloads/agent/": "https://upguard.com.br/downloads/agent",
		" https://x/y ": "https://x/y",
		"off":           "",
		"OFF":           "",
		"none":          "",
		"false":         "",
		"0":             "",
		"disabled":      "",
		"":              "",
	} {
		if got := normalizeUpdateURL(in); got != want {
			t.Errorf("normalizeUpdateURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// releaseServer simula o endereço de download (latest.json, .sig e binários).
type releaseServer struct {
	*httptest.Server
	mu        sync.Mutex
	version   string
	bins      map[string]string // nome do binário -> conteúdo publicado
	tamper    map[string]string // conteúdo servido no lugar do publicado (adulterado)
	priv      ed25519.PrivateKey
	sigBroken bool
	codes     map[string]int // caminho -> status forçado
	rawMan    string         // manifesto cru (sobrescreve o gerado)
	downloads []string
	uas       []string
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (rs *releaseServer) manifest() []byte {
	if rs.rawMan != "" {
		return []byte(rs.rawMan)
	}
	m := release.Manifest{Version: rs.version, Files: map[string]release.File{}}
	for name, body := range rs.bins {
		key := strings.TrimSuffix(strings.TrimPrefix(name, "upguard-agent-"), ".exe")
		m.Files[key] = release.File{Name: name, URL: "v" + rs.version + "/" + name, SHA256: sha(body), Size: int64(len(body))}
	}
	b, _ := json.Marshal(m)
	return b
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{
		version: "1.4.0",
		bins: map[string]string{
			"upguard-agent-linux-arm64":       "#!binario-novo",
			"upguard-agent-darwin-arm64":      "#!binario-novo",
			"upguard-agent-windows-arm64.exe": "#!binario-novo",
		},
		tamper: map[string]string{},
		codes:  map[string]int{},
	}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.uas = append(rs.uas, r.Header.Get("User-Agent"))
		if code := rs.codes[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			return
		}
		switch {
		case r.URL.Path == "/agent/latest.json":
			_, _ = w.Write(rs.manifest())
		case r.URL.Path == "/agent/latest.json.sig":
			if rs.priv == nil {
				http.NotFound(w, r)
				return
			}
			sig := release.Sign(rs.priv, rs.manifest())
			if rs.sigBroken {
				sig = release.Sign(rs.priv, []byte("outro conteudo"))
			}
			_, _ = w.Write(sig)
		case strings.HasPrefix(r.URL.Path, "/agent/v"+rs.version+"/"):
			name := strings.TrimPrefix(r.URL.Path, "/agent/v"+rs.version+"/")
			rs.downloads = append(rs.downloads, name)
			body, ok := rs.bins[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if tb, ok := rs.tamper[name]; ok {
				body = tb
			}
			_, _ = io.WriteString(w, body)
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
	u := newUpdater(rs.Client(), rs.URL+"/agent/")
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

func assertUntouched(t *testing.T, exe string, execs *[]execCall) {
	t.Helper()
	if readFile(t, exe) != "binario-antigo" || len(*execs) != 0 {
		t.Fatal("o binário atual não pode ser trocado nem reexecutado")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("o temporário deveria ter sido apagado")
	}
}

func TestNewUpdaterDefaults(t *testing.T) {
	u := newUpdater(http.DefaultClient, defaultUpdateURL)
	if u.baseURL != defaultUpdateURL || u.current != version || u.pubKey != nil ||
		u.executable == nil || u.reexec == nil || u.exit == nil {
		t.Fatalf("defaults inesperados: %+v", u)
	}
	if !strings.HasPrefix(defaultUpdateURL, "https://upguard.com.br/") {
		t.Fatalf("o padrão deve ser o endereço próprio do UpGuard: %s", defaultUpdateURL)
	}
}

func TestNewUpdaterEmbeddedKey(t *testing.T) {
	old := release.PublicKeyB64
	t.Cleanup(func() { release.PublicKeyB64 = old })

	pub, _, err := release.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	release.PublicKeyB64 = pub
	if u := newUpdater(http.DefaultClient, defaultUpdateURL); u.pubKey == nil || u.baseURL == "" {
		t.Fatal("chave embutida deveria ser carregada")
	}
	release.PublicKeyB64 = "nao-e-base64!"
	if u := newUpdater(http.DefaultClient, defaultUpdateURL); u.baseURL != "" {
		t.Fatal("chave embutida inválida deveria desligar o auto-update")
	}
}

func TestFetchManifest(t *testing.T) {
	rs := newReleaseServer(t)
	u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
	m, err := u.fetchManifest()
	if err != nil || m.Version != "1.4.0" {
		t.Fatalf("fetchManifest = %+v, %v", m, err)
	}
	if rs.uas[0] != userAgent {
		t.Fatalf("User-Agent = %q", rs.uas[0])
	}

	rs.codes["/agent/latest.json"] = 503
	if _, err := u.fetchManifest(); err == nil || err.Error() != "latest.json respondeu 503" {
		t.Fatalf("esperava erro 503, got %v", err)
	}
	delete(rs.codes, "/agent/latest.json")

	rs.rawMan = "<html>SPA</html>"
	if _, err := u.fetchManifest(); err == nil {
		t.Fatal("HTML no lugar do manifesto deveria falhar")
	}
	rs.rawMan = strings.Repeat("x", release.MaxManifestSize+1)
	if _, err := u.fetchManifest(); err == nil || !strings.Contains(err.Error(), "limite") {
		t.Fatalf("manifesto gigante deveria falhar, got %v", err)
	}
	rs.rawMan = ""

	u.baseURL = "://bad"
	if _, err := u.fetchManifest(); err == nil {
		t.Fatal("URL inválida deveria falhar")
	}
	u.baseURL = ""
	if _, err := u.fetchManifest(); !errors.Is(err, errUpdateDisabled) {
		t.Fatalf("sem endereço deveria estar desligado, got %v", err)
	}
}

func TestFetchManifestSignature(t *testing.T) {
	rs := newReleaseServer(t)
	pubB64, privB64, err := release.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	rs.priv, _ = release.ParsePrivateKey(privB64)
	u, exe, execs, _ := testUpdater(t, rs, "linux", "1.0.0")
	u.pubKey, _ = release.ParsePublicKey(pubB64)

	if _, err := u.fetchManifest(); err != nil {
		t.Fatalf("assinatura válida deveria passar: %v", err)
	}

	rs.sigBroken = true
	if _, err := u.fetchManifest(); err == nil || !strings.Contains(err.Error(), "não confere") {
		t.Fatalf("assinatura de outro conteúdo deveria falhar, got %v", err)
	}
	u.maybeUpdate(false) // só loga
	assertUntouched(t, exe, execs)
	rs.sigBroken = false

	rs.priv = nil // servidor sem .sig: com chave embutida é obrigatório
	if _, err := u.fetchManifest(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("sem assinatura deveria falhar, got %v", err)
	}
	u.maybeUpdate(true) // forçado também exige a assinatura
	assertUntouched(t, exe, execs)
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
}

func TestMaybeUpdateEndpointDownKeepsRunning(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	rs := newReleaseServer(t)
	for _, force := range []bool{false, true} {
		u, exe, execs, exits := testUpdater(t, rs, "linux", "0.0.1")
		u.baseURL = down.URL
		u.maybeUpdate(force) // não pode entrar em pânico nem sair
		assertUntouched(t, exe, execs)
		if len(*exits) != 0 {
			t.Fatal("falha no endereço novo não pode encerrar o agente")
		}
	}
}

func TestMaybeUpdateDisabled(t *testing.T) {
	rs := newReleaseServer(t)
	for _, force := range []bool{false, true} {
		u, exe, execs, _ := testUpdater(t, rs, "linux", "0.0.1")
		u.baseURL = normalizeUpdateURL("off")
		u.maybeUpdate(force)
		assertUntouched(t, exe, execs)
	}
	if len(rs.uas) != 0 {
		t.Fatal("desligado não deveria fazer nenhuma requisição")
	}
}

func TestMaybeUpdateForcedReinstallsSameVersion(t *testing.T) {
	rs := newReleaseServer(t)
	u, exe, execs, _ := testUpdater(t, rs, "darwin", "9.9.9")
	u.maybeUpdate(true)
	if len(rs.downloads) != 1 || rs.downloads[0] != "upguard-agent-darwin-arm64" || len(*execs) != 1 {
		t.Fatalf("forçado deveria baixar e reexecutar: downloads=%v execs=%v", rs.downloads, *execs)
	}
	if readFile(t, exe) != "#!binario-novo" {
		t.Fatal("binário não substituído")
	}
}

func TestInstallWindowsExits(t *testing.T) {
	rs := newReleaseServer(t)
	u, exe, execs, exits := testUpdater(t, rs, goosWindows, "1.0.0")
	u.maybeUpdate(false)
	if len(rs.downloads) != 1 || rs.downloads[0] != "upguard-agent-windows-arm64.exe" {
		t.Fatalf("Windows deveria baixar o .exe: %v", rs.downloads)
	}
	if len(*exits) != 1 || (*exits)[0] != 0 || len(*execs) != 0 {
		t.Fatalf("Windows deveria sair com 0 (serviço reinicia): exits=%v execs=%v", *exits, *execs)
	}
	if readFile(t, exe) != "#!binario-novo" {
		t.Fatal("binário não substituído")
	}
}

func TestInstallRejectsTamperedBinary(t *testing.T) {
	rs := newReleaseServer(t)
	m, err := release.Parse(rs.manifest())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("conteúdo adulterado do mesmo tamanho", func(t *testing.T) {
		rs.tamper["upguard-agent-linux-arm64"] = "#!binario-mau!"
		defer delete(rs.tamper, "upguard-agent-linux-arm64")
		u, exe, execs, _ := testUpdater(t, rs, "linux", "1.0.0")
		err := u.install(m)
		if err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("binário adulterado deveria ser recusado, got %v", err)
		}
		assertUntouched(t, exe, execs)
		u.maybeUpdate(true) // via laço: só loga
		assertUntouched(t, exe, execs)
	})

	t.Run("tamanho diferente", func(t *testing.T) {
		rs.tamper["upguard-agent-linux-arm64"] = "#!binario-novo-com-sobra"
		defer delete(rs.tamper, "upguard-agent-linux-arm64")
		u, exe, execs, _ := testUpdater(t, rs, "linux", "1.0.0")
		if err := u.install(m); err == nil || !strings.Contains(err.Error(), "tamanho") {
			t.Fatalf("tamanho diferente deveria ser recusado, got %v", err)
		}
		assertUntouched(t, exe, execs)
	})
}

func TestInstallErrors(t *testing.T) {
	rs := newReleaseServer(t)
	m, err := release.Parse(rs.manifest())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("plataforma ausente no manifesto", func(t *testing.T) {
		u, exe, execs, _ := testUpdater(t, rs, "freebsd", "1.0.0")
		if err := u.install(m); err == nil || !strings.Contains(err.Error(), "freebsd-arm64") {
			t.Fatalf("deveria falhar sem binário da plataforma, got %v", err)
		}
		assertUntouched(t, exe, execs)
	})

	t.Run("download 404 mantém o binário", func(t *testing.T) {
		rs.codes["/agent/v1.4.0/upguard-agent-linux-arm64"] = 404
		defer delete(rs.codes, "/agent/v1.4.0/upguard-agent-linux-arm64")
		u, exe, execs, _ := testUpdater(t, rs, "linux", "1.0.0")
		if err := u.install(m); err == nil || err.Error() != "download respondeu 404" {
			t.Fatalf("esperava erro 404, got %v", err)
		}
		assertUntouched(t, exe, execs)
	})

	t.Run("executável desconhecido", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.executable = func() (string, error) { return "", errors.New("sem executável") }
		if err := u.install(m); err == nil {
			t.Fatal("deveria falhar")
		}
	})

	t.Run("diretório sem permissão de escrita", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		missing := filepath.Join(t.TempDir(), "nao-existe", "upguard-agent")
		u.executable = func() (string, error) { return missing, nil }
		if err := u.install(m); err == nil {
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
		if err := u.install(m); err == nil {
			t.Fatal("rename sobre diretório não vazio deveria falhar")
		}
		if _, err := os.Stat(dir + ".new"); !os.IsNotExist(err) {
			t.Fatal("o temporário deveria ter sido apagado")
		}
	})

	t.Run("reexec falha", func(t *testing.T) {
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.reexec = func(string, []string, []string) error { return errors.New("exec negado") }
		if err := u.install(m); err == nil || err.Error() != "exec negado" {
			t.Fatalf("erro do exec deveria propagar, got %v", err)
		}
	})

	t.Run("conexão cai no meio do download", func(t *testing.T) {
		cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "parcial")
		}))
		defer cut.Close()
		u, exe, execs, _ := testUpdater(t, rs, "linux", "1.0.0")
		u.client = cut.Client()
		u.baseURL = cut.URL
		if err := u.install(m); err == nil {
			t.Fatal("download truncado deveria falhar")
		}
		assertUntouched(t, exe, execs)
	})

	t.Run("URL do binário inválida", func(t *testing.T) {
		bad := *m
		bad.Files = map[string]release.File{"linux-arm64": {Name: "x", URL: "%zz", SHA256: strings.Repeat("0", 64)}}
		u, _, _, _ := testUpdater(t, rs, "linux", "1.0.0")
		if err := u.install(&bad); err == nil {
			t.Fatal("deveria falhar")
		}
		u.baseURL = "://bad"
		if err := u.install(m); err == nil {
			t.Fatal("deveria falhar")
		}
		u.baseURL = "http://[::1"
		if _, err := u.binaryURL(m.Files["linux-arm64"]); err == nil {
			t.Fatal("base inválida deveria falhar")
		}
	})
}
