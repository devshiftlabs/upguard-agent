package main

// Auto-update: consulta a última release no GitHub e, se houver versão nova
// (ou o portal forçar), baixa o binário da plataforma e re-executa o processo.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	ghLatestAPI     = "https://api.github.com/repos/devshiftlabs/upguard-agent/releases/latest"
	releaseBinBase  = "https://github.com/devshiftlabs/upguard-agent/releases/latest/download"
	downloadTimeout = 120 * time.Second
)

type updater struct {
	client     *http.Client
	latestURL  string
	binBase    string
	current    string
	goos       string
	goarch     string
	executable func() (string, error)
	// reexec substitui o processo pelo binário novo (syscall.Exec).
	reexec func(argv0 string, argv []string, envv []string) error
	// exit encerra o processo (Windows: o gerenciador de serviço reinicia).
	exit func(code int)
}

func newUpdater(client *http.Client) *updater {
	return &updater{
		client:     client,
		latestURL:  ghLatestAPI,
		binBase:    releaseBinBase,
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
func (u *updater) get(ctx context.Context, url, accept, who string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
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

// latestVersion consulta a API do GitHub pela última release (tag sem "v").
func (u *updater) latestVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resp, err := u.get(ctx, u.latestURL, "application/vnd.github+json", "github")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimPrefix(out.TagName, "v"), nil
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

// selfUpdate baixa o binário mais novo da última release e re-executa o processo.
// O rename atômico sobre o binário em execução é permitido no Linux/macOS (o
// processo atual mantém o inode antigo); no Windows o serviço reinicia sozinho.
func (u *updater) selfUpdate() error {
	exe, err := u.executable()
	if err != nil {
		return err
	}
	ext := ""
	if u.goos == goosWindows {
		ext = ".exe"
	}
	url := u.binBase + "/upguard-agent-" + u.goos + "-" + u.goarch + ext
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	resp, err := u.get(ctx, url, "", "download")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		return err
	}
	log.Printf("binário atualizado — reexecutando %s", exe)
	if u.goos == goosWindows {
		u.exit(0) // o gerenciador de serviço reinicia com o novo binário
		return nil
	}
	return u.reexec(exe, os.Args, os.Environ())
}

// maybeUpdate atualiza o agente se o portal forçou (force) ou se há versão nova.
func (u *updater) maybeUpdate(force bool) {
	if force {
		log.Printf("atualização forçada pelo portal — atualizando")
	} else {
		latest, err := u.latestVersion()
		if err != nil || !versionLess(u.current, latest) {
			return
		}
		log.Printf("nova versão disponível: %s (atual %s) — atualizando", latest, u.current)
	}
	if err := u.selfUpdate(); err != nil {
		log.Printf("falha ao auto-atualizar: %v", err)
	}
}
