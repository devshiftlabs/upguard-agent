package main

// Auto-update: consulta o manifesto da última versão no endereço de download
// do UpGuard (padrão https://upguard.com.br/downloads/agent, sem depender do
// GitHub) e, se houver versão nova (ou o portal forçar), baixa o binário da
// plataforma, confere o SHA-256 do manifesto (e a assinatura ed25519 do
// manifesto, quando o agente tem chave embutida) e re-executa o processo.
// Qualquer falha só é registrada no log: o agente segue monitorando.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/devshiftlabs/upguard-agent/internal/release"
)

const (
	defaultUpdateURL = "https://upguard.com.br/downloads/agent"
	downloadTimeout  = 120 * time.Second
	// maxBinarySize protege o disco do host contra um download sem fim.
	maxBinarySize = 256 << 20
)

// errUpdateDisabled: UPGUARD_UPDATE_URL=off desliga o auto-update.
var errUpdateDisabled = errors.New("auto-update desligado (UPGUARD_UPDATE_URL=off)")

type updater struct {
	client *http.Client
	// baseURL é o endereço base da publicação (sem barra final); vazio = desligado.
	baseURL string
	// pubKey, se não nil, torna obrigatória a assinatura do manifesto.
	pubKey     ed25519.PublicKey
	current    string
	goos       string
	goarch     string
	executable func() (string, error)
	// reexec substitui o processo pelo binário novo (syscall.Exec).
	reexec func(argv0 string, argv []string, envv []string) error
	// exit encerra o processo (Windows: o gerenciador de serviço reinicia).
	exit func(code int)
}

// normalizeUpdateURL trata o valor configurado: "off"/"none"/"false" desligam.
func normalizeUpdateURL(raw string) string {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "off", "none", "false", "0", "disabled":
		return ""
	}
	return strings.TrimRight(raw, "/")
}

func newUpdater(client *http.Client, baseURL string) *updater {
	pub, err := release.ParsePublicKey(release.PublicKeyB64)
	if err != nil {
		// Chave embutida corrompida é bug de build: sem chave válida não há
		// como conferir a assinatura, então o auto-update fica desligado.
		log.Printf("chave de assinatura inválida no binário — auto-update desligado: %v", err)
		baseURL = ""
	}
	return &updater{
		client:     client,
		baseURL:    normalizeUpdateURL(baseURL),
		pubKey:     pub,
		current:    version,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		executable: os.Executable,
		reexec:     syscall.Exec,
		exit:       os.Exit,
	}
}

// get faz um GET com User-Agent e devolve a resposta 2xx (o chamador fecha o
// corpo); who identifica a origem na mensagem de erro de status.
func (u *updater) get(ctx context.Context, url, who string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerUserAgent, userAgent)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &statusError{who: who, code: resp.StatusCode}
	}
	return resp, nil
}

// getSmall baixa um arquivo pequeno (manifesto/assinatura) com limite de tamanho.
func (u *updater) getSmall(ctx context.Context, name string) ([]byte, error) {
	resp, err := u.get(ctx, u.baseURL+"/"+name, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, release.MaxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > release.MaxManifestSize {
		return nil, fmt.Errorf("%s maior que o limite", name)
	}
	return data, nil
}

// fetchManifest baixa latest.json e, se o agente tem chave, confere a assinatura.
func (u *updater) fetchManifest() (*release.Manifest, error) {
	if u.baseURL == "" {
		return nil, errUpdateDisabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	data, err := u.getSmall(ctx, release.ManifestName)
	if err != nil {
		return nil, err
	}
	if u.pubKey != nil {
		sig, err := u.getSmall(ctx, release.SignatureName)
		if err != nil {
			return nil, err
		}
		if err := release.Verify(u.pubKey, data, sig); err != nil {
			return nil, err
		}
	}
	return release.Parse(data)
}

func parseVer(s string) [3]int {
	var v [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		n := 0
		_, _ = fmt.Sscanf(part, "%d", &n)
		v[i] = n
	}
	return v
}

// versionLess informa se a < b para versões "x.y.z".
func versionLess(a, b string) bool {
	pa, pb := parseVer(a), parseVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// binaryURL resolve a URL (relativa ao endereço base) do binário.
func (u *updater) binaryURL(f release.File) (string, error) {
	base, err := url.Parse(u.baseURL + "/")
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(f.URL)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

// download grava o binário em tmp conferindo tamanho e SHA-256; em qualquer
// falha o temporário é apagado e o binário atual fica intacto.
func (u *updater) download(f release.File, tmp string) (err error) {
	src, err := u.binaryURL(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	resp, err := u.get(ctx, src, "download")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxBinarySize+1))
	if err != nil {
		return err
	}
	if n > maxBinarySize || (f.Size > 0 && n != f.Size) {
		return fmt.Errorf("tamanho do binário não confere (%d bytes, esperado %d)", n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("SHA-256 do binário não confere (recebido %s, esperado %s) — arquivo descartado", got, f.SHA256)
	}
	return nil
}

// install baixa e confere o binário da versão do manifesto e re-executa o
// processo. O rename atômico sobre o binário em execução é permitido no
// Linux/macOS (o processo atual mantém o inode antigo); no Windows o serviço
// reinicia sozinho.
func (u *updater) install(m *release.Manifest) error {
	f, err := m.File(u.goos, u.goarch)
	if err != nil {
		return err
	}
	exe, err := u.executable()
	if err != nil {
		return err
	}
	tmp := exe + ".new"
	if err := u.download(f, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	log.Printf("binário %s instalado (SHA-256 conferido) — reexecutando %s", m.Version, exe)
	if u.goos == goosWindows {
		u.exit(0) // o gerenciador de serviço reinicia com o novo binário
		return nil
	}
	return u.reexec(exe, os.Args, os.Environ())
}

// maybeUpdate atualiza o agente se o portal forçou (force) ou se há versão
// nova. Nunca derruba o agente: erro só vai para o log.
func (u *updater) maybeUpdate(force bool) {
	m, err := u.fetchManifest()
	if errors.Is(err, errUpdateDisabled) {
		if force {
			log.Printf("atualização pedida pelo portal ignorada: %v", err)
		}
		return
	}
	if err != nil {
		log.Printf("não foi possível consultar a última versão em %s: %v", u.baseURL, err)
		return
	}
	switch {
	case force:
		log.Printf("atualização forçada pelo portal — instalando %s (atual %s)", m.Version, u.current)
	case versionLess(u.current, m.Version):
		log.Printf("nova versão disponível: %s (atual %s) — atualizando", m.Version, u.current)
	default:
		return
	}
	if err := u.install(m); err != nil {
		log.Printf("falha ao auto-atualizar: %v", err)
	}
}
