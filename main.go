// UpGuard local monitoring agent.
//
// Coleta métricas de sistema (CPU, memória, disco, load, rede, uptime) + infos
// (IP, kernel, processos, serviços) e as envia periodicamente para a conta
// UpGuard, autenticado por client-id / client-secret (Basic auth). O intervalo
// é definido pelo portal: o servidor devolve o intervalo desejado a cada envio
// e o agente se ajusta. Binário único, sem dependências, cross-platform.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

var version = "dev"

const defaultServer = "https://api.upguard.com.br"

type sw struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type procInfo struct {
	PID  int32   `json:"pid"`
	Name string  `json:"name"`
	CPU  float64 `json:"cpu_percent"`
	Mem  float64 `json:"mem_percent"`
}

type hostInfo struct {
	Hostname        string     `json:"hostname"`
	OS              string     `json:"os"`
	Platform        string     `json:"platform"`
	Kernel          string     `json:"kernel"`
	AgentVersion    string     `json:"agent_version"`
	CPUCores        int        `json:"cpu_cores"`
	MemTotal        uint64     `json:"mem_total_bytes"`
	IPAddress       string     `json:"ip_address"`
	Processes       uint64     `json:"processes"`
	ServicesRunning int        `json:"services_running"`
	ServicesList    []string   `json:"services_list"`
	TopProcesses    []procInfo `json:"top_processes"`
	Software        []sw       `json:"software"`
}

type metrics struct {
	CPUPercent  float64 `json:"cpu_percent"`
	MemPercent  float64 `json:"mem_percent"`
	MemUsed     uint64  `json:"mem_used_bytes"`
	DiskPercent float64 `json:"disk_percent"`
	DiskUsed    uint64  `json:"disk_used_bytes"`
	DiskTotal   uint64  `json:"disk_total_bytes"`
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	NetRx       uint64  `json:"net_rx_bytes"`
	NetTx       uint64  `json:"net_tx_bytes"`
	Uptime      uint64  `json:"uptime_seconds"`
}

type payload struct {
	Host    hostInfo `json:"host"`
	Metrics *metrics `json:"metrics,omitempty"` // omitido quando o agente está pausado
}

type ingestResponse struct {
	HostID string `json:"host_id"`
	Config struct {
		IntervalSeconds int  `json:"interval_seconds"`
		Paused          bool `json:"paused"`
		UpdateRequested bool `json:"update_requested"`
	} `json:"config"`
}

// userAgent é resolvido em runtime (version vem de -ldflags no build).
var userAgent = "upguard-agent/" + version

const (
	ghLatestAPI    = "https://api.github.com/repos/devshiftlabs/upguard-agent/releases/latest"
	releaseBinBase = "https://github.com/devshiftlabs/upguard-agent/releases/latest/download"
)

type config struct {
	clientID, clientSecret, server, hostname string
	interval                                 time.Duration
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	clientID := flag.String("client-id", env("UPGUARD_CLIENT_ID", ""), "client id do agente")
	secret := flag.String("client-secret", env("UPGUARD_CLIENT_SECRET", ""), "client secret do agente")
	server := flag.String("server", env("UPGUARD_SERVER_URL", defaultServer), "URL base da API do UpGuard")
	hostname := flag.String("hostname", env("UPGUARD_HOSTNAME", ""), "nome do host (default: hostname do sistema)")
	intervalS := flag.Int("interval", 0, "intervalo inicial em segundos (o portal pode sobrescrever)")
	showVer := flag.Bool("version", false, "imprime a versão e sai")
	flag.Parse()

	if *showVer {
		fmt.Printf("upguard-agent %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	interval := 60
	if *intervalS > 0 {
		interval = *intervalS
	} else if v := os.Getenv("UPGUARD_INTERVAL"); v != "" {
		fmt.Sscanf(v, "%d", &interval)
	}
	if interval < 10 {
		interval = 10
	}
	return config{*clientID, *secret, *server, *hostname, time.Duration(interval) * time.Second}
}

func rootPath() string {
	if runtime.GOOS == "windows" {
		return "C:\\"
	}
	return "/"
}

// primaryIP descobre o IP de saída (sem enviar pacotes) com fallback para a
// primeira interface IPv4 não-loopback.
func primaryIP() string {
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return a.IP.String()
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}

// runningServices lista os serviços do host. systemd no Linux, rc.d no
// FreeBSD/pfSense; em outros SOs, nil.
func runningServices() []string {
	switch runtime.GOOS {
	case "linux":
		return systemdServices()
	case "freebsd":
		return rcServices()
	default:
		return nil
	}
}

// systemdServices lista os serviços systemd ativos (Linux).
func systemdServices() []string {
	out, err := exec.Command("systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--no-pager", "--plain").Output()
	if err != nil {
		return nil
	}
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		svcs = append(svcs, strings.TrimSuffix(fields[0], ".service"))
	}
	sort.Strings(svcs)
	return svcs
}

// rcServices lista os serviços rc.d habilitados (FreeBSD/pfSense). `service -e`
// devolve o caminho de cada script habilitado; reduzimos ao nome-base.
func rcServices() []string {
	out, err := exec.Command("service", "-e").Output()
	if err != nil {
		return nil
	}
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		svcs = append(svcs, strings.TrimSuffix(filepath.Base(line), ".sh"))
	}
	sort.Strings(svcs)
	return svcs
}

// topProcesses lista os processos que mais consomem CPU (com memória), até limit.
func topProcesses(limit int) []procInfo {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	out := make([]procInfo, 0, len(procs))
	for _, pr := range procs {
		name, err := pr.Name()
		if err != nil || name == "" {
			continue
		}
		cpuPct, _ := pr.CPUPercent()
		memPct, _ := pr.MemoryPercent()
		out = append(out, procInfo{
			PID:  pr.Pid,
			Name: name,
			CPU:  round2(cpuPct),
			Mem:  round2(float64(memPct)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPU != out[j].CPU {
			return out[i].CPU > out[j].CPU
		}
		return out[i].Mem > out[j].Mem
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// ---- Software inventory (server-driven probes) ----

type probe struct {
	Name      string   `json:"name"`
	Bin       string   `json:"bin"`
	Args      []string `json:"args"`
	UseStderr bool     `json:"use_stderr"`
}

var (
	swCache      []sw
	swCachedAt   time.Time
	binNameRe    = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	versionLineRe = regexp.MustCompile(`\d+\.\d+(\.\d+)*`)
)

// fetchProbes baixa a lista de probes do servidor (editável no portal).
func fetchProbes(cfg config, client *http.Client) ([]probe, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.server+"/api/agent/probes", nil)
	if err != nil {
		return nil, false
	}
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, false
	}
	var body struct {
		Probes []probe `json:"probes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false
	}
	return body.Probes, true
}

// runProbes executa cada probe (sem shell) e coleta a versão. Só roda binários
// com nome simples presentes no PATH; ignora o resto (segurança).
func runProbes(probes []probe) []sw {
	var out []sw
	for _, pr := range probes {
		if !binNameRe.MatchString(pr.Bin) {
			continue // nome de binário inválido — ignora
		}
		path, err := exec.LookPath(pr.Bin)
		if err != nil {
			continue // não instalado / fora do PATH
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, path, pr.Args...)
		var buf bytes.Buffer
		if pr.UseStderr {
			cmd.Stderr = &buf
		} else {
			cmd.Stdout = &buf
		}
		_ = cmd.Run()
		cancel()
		out = append(out, sw{Name: pr.Name, Version: parseVersion(buf.String())})
	}
	return out
}

func parseVersion(s string) string {
	line := ""
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			line = strings.TrimSpace(l)
			break
		}
	}
	if m := versionLineRe.FindString(line); m != "" {
		return m
	}
	if line == "" {
		return "instalado"
	}
	return line
}

// softwareInventory devolve o inventário (cache de 1h); rebusca probes + reexecuta.
func softwareInventory(cfg config, client *http.Client) []sw {
	if swCache != nil && time.Since(swCachedAt) < time.Hour {
		return swCache
	}
	probes, ok := fetchProbes(cfg, client)
	if !ok && swCache != nil {
		return swCache // mantém o anterior se a busca falhou
	}
	swCache = runProbes(probes)
	swCachedAt = time.Now()
	return swCache
}

// collect coleta uma amostra. Erros por métrica são tolerados (campo fica zero).
func collect(cfg config, client *http.Client, paused bool) payload {
	hostnameOverride := cfg.hostname
	p := payload{}
	p.Host.AgentVersion = version
	p.Host.OS = runtime.GOOS
	p.Host.Software = softwareInventory(cfg, client)

	var uptime uint64
	if hi, err := host.Info(); err == nil {
		p.Host.Hostname = hi.Hostname
		p.Host.Platform = strings.TrimSpace(fmt.Sprintf("%s %s", hi.Platform, hi.PlatformVersion))
		p.Host.Kernel = hi.KernelVersion
		p.Host.Processes = hi.Procs
		uptime = hi.Uptime
	}
	if hostnameOverride != "" {
		p.Host.Hostname = hostnameOverride
	}
	if p.Host.Hostname == "" {
		p.Host.Hostname, _ = os.Hostname()
	}
	p.Host.IPAddress = primaryIP()
	svcs := runningServices()
	p.Host.ServicesList = svcs
	p.Host.ServicesRunning = len(svcs)
	p.Host.TopProcesses = topProcesses(50)

	if c, err := cpu.Counts(true); err == nil {
		p.Host.CPUCores = c
	}
	var memUsedPct float64
	var memUsed uint64
	haveMem := false
	if vm, err := mem.VirtualMemory(); err == nil {
		p.Host.MemTotal = vm.Total
		memUsedPct, memUsed, haveMem = vm.UsedPercent, vm.Used, true
	}

	// Pausado: envia apenas info do host (heartbeat), sem coletar métricas.
	// O campo metrics é omitido, então o servidor não grava amostra nem avalia alertas.
	if paused {
		return p
	}

	m := metrics{Uptime: uptime}
	if pct, err := cpu.Percent(time.Second, false); err == nil && len(pct) > 0 {
		m.CPUPercent = round2(pct[0])
	}
	if haveMem {
		m.MemPercent = round2(memUsedPct)
		m.MemUsed = memUsed
	}
	if du, err := disk.Usage(rootPath()); err == nil {
		m.DiskPercent = round2(du.UsedPercent)
		m.DiskUsed = du.Used
		m.DiskTotal = du.Total
	}
	if la, err := load.Avg(); err == nil {
		m.Load1 = round2(la.Load1)
		m.Load5 = round2(la.Load5)
		m.Load15 = round2(la.Load15)
	}
	if io, err := psnet.IOCounters(false); err == nil && len(io) > 0 {
		m.NetRx = io[0].BytesRecv
		m.NetTx = io[0].BytesSent
	}
	p.Metrics = &m
	return p
}

// send envia a amostra e retorna o intervalo desejado pelo servidor (0 se n/d).
type serverConfig struct {
	hostID          string
	interval        int
	paused          bool
	updateRequested bool
}

func send(cfg config, client *http.Client, p payload) (serverConfig, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return serverConfig{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.server+"/api/agent/metrics", bytes.NewReader(body))
	if err != nil {
		return serverConfig{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)

	resp, err := client.Do(req)
	if err != nil {
		return serverConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return serverConfig{}, fmt.Errorf("servidor respondeu %d", resp.StatusCode)
	}
	var out ingestResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return serverConfig{out.HostID, out.Config.IntervalSeconds, out.Config.Paused, out.Config.UpdateRequested}, nil
}

// latestVersion consulta a API do GitHub pela última release (tag sem "v").
func latestVersion(client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ghLatestAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("github respondeu %d", resp.StatusCode)
	}
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
		if i >= 3 {
			break
		}
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
func selfUpdate(client *http.Client) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	url := releaseBinBase + "/upguard-agent-" + runtime.GOOS + "-" + runtime.GOARCH + ext
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download respondeu %d", resp.StatusCode)
	}
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
	if runtime.GOOS == "windows" {
		os.Exit(0) // o gerenciador de serviço reinicia com o novo binário
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// maybeUpdate atualiza o agente se o portal forçou (force) ou se há versão nova.
func maybeUpdate(client *http.Client, force bool) {
	if !force {
		latest, err := latestVersion(client)
		if err != nil || !versionLess(version, latest) {
			return
		}
		log.Printf("nova versão disponível: %s (atual %s) — atualizando", latest, version)
	} else {
		log.Printf("atualização forçada pelo portal — atualizando")
	}
	if err := selfUpdate(client); err != nil {
		log.Printf("falha ao auto-atualizar: %v", err)
	}
}

type loopState struct {
	current time.Duration
	paused  bool
}

// apply processa a config server-driven: update forçado, pausa e intervalo.
func (st *loopState) apply(client *http.Client, sc serverConfig) {
	if sc.updateRequested {
		maybeUpdate(client, true) // força; re-executa e não retorna
	}
	if sc.paused != st.paused {
		log.Printf("estado alterado pelo portal: paused=%v", sc.paused)
		st.paused = sc.paused
	}
	if sc.interval >= 10 {
		want := time.Duration(sc.interval) * time.Second
		if want != st.current {
			log.Printf("intervalo ajustado pelo portal: %s -> %s", st.current, want)
			st.current = want
		}
	}
}

func logSent(p payload) {
	if p.Metrics != nil {
		log.Printf("métricas enviadas: cpu=%.1f%% mem=%.1f%% disk=%.1f%% svc=%d",
			p.Metrics.CPUPercent, p.Metrics.MemPercent, p.Metrics.DiskPercent, p.Host.ServicesRunning)
	} else {
		log.Printf("pausado — heartbeat enviado (sem métricas)")
	}
}

func main() {
	cfg := loadConfig()
	if cfg.clientID == "" || cfg.clientSecret == "" {
		log.Fatal("client-id e client-secret são obrigatórios (flags ou UPGUARD_CLIENT_ID/UPGUARD_CLIENT_SECRET)")
	}

	log.Printf("upguard-agent %s iniciando — servidor=%s intervalo(inicial)=%s", version, cfg.server, cfg.interval)
	client := &http.Client{Timeout: 20 * time.Second}
	st := &loopState{current: cfg.interval}
	var lastUpdateCheck time.Time
	var hostID string
	runner := newCheckRunner()

	timer := time.NewTimer(0) // dispara imediatamente no início
	defer timer.Stop()
	for range timer.C {
		// Auto-update periódico (a cada 6h; também no primeiro ciclo). Se houver
		// versão nova, selfUpdate re-executa o processo e não retorna.
		if time.Since(lastUpdateCheck) > 6*time.Hour {
			lastUpdateCheck = time.Now()
			maybeUpdate(client, false)
		}
		p := collect(cfg, client, st.paused)
		if sc, err := send(cfg, client, p); err != nil {
			log.Printf("erro ao enviar métricas: %v", err)
		} else {
			logSent(p)
			if sc.hostID != "" {
				hostID = sc.hostID
			}
			st.apply(client, sc)
			// Checks privados (bancos na rede local) — não roda pausado.
			if !st.paused {
				runner.cycle(cfg, client, hostID)
			}
		}
		// Pausado: verifica o portal com mais frequência (para retomar rápido).
		next := st.current
		if st.paused && next > 30*time.Second {
			next = 30 * time.Second
		}
		timer.Reset(next)
	}
}
