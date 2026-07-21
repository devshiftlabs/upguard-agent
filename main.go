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
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
)

var version = "dev"

const defaultServer = "https://api.upguard.com.br"

type sw struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type hostInfo struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	Kernel          string `json:"kernel"`
	AgentVersion    string `json:"agent_version"`
	CPUCores        int    `json:"cpu_cores"`
	MemTotal        uint64 `json:"mem_total_bytes"`
	IPAddress       string `json:"ip_address"`
	Processes       uint64 `json:"processes"`
	ServicesRunning int    `json:"services_running"`
	Software        []sw   `json:"software"`
}

type metrics struct {
	CPUPercent  float64 `json:"cpu_percent"`
	MemPercent  float64 `json:"mem_percent"`
	MemUsed     uint64  `json:"mem_used_bytes"`
	DiskPercent float64 `json:"disk_percent"`
	DiskUsed    uint64  `json:"disk_used_bytes"`
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	NetRx       uint64  `json:"net_rx_bytes"`
	NetTx       uint64  `json:"net_tx_bytes"`
	Uptime      uint64  `json:"uptime_seconds"`
}

type payload struct {
	Host    hostInfo `json:"host"`
	Metrics metrics  `json:"metrics"`
}

type ingestResponse struct {
	Config struct {
		IntervalSeconds int `json:"interval_seconds"`
	} `json:"config"`
}

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

// runningServices conta serviços systemd ativos (Linux). Em outros SOs, 0.
func runningServices() int {
	if runtime.GOOS != "linux" {
		return 0
	}
	out, err := exec.Command("systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--no-pager", "--plain").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
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
func collect(cfg config, client *http.Client) payload {
	hostnameOverride := cfg.hostname
	p := payload{}
	p.Host.AgentVersion = version
	p.Host.OS = runtime.GOOS
	p.Host.Software = softwareInventory(cfg, client)

	if hi, err := host.Info(); err == nil {
		p.Host.Hostname = hi.Hostname
		p.Host.Platform = strings.TrimSpace(fmt.Sprintf("%s %s", hi.Platform, hi.PlatformVersion))
		p.Host.Kernel = hi.KernelVersion
		p.Host.Processes = hi.Procs
		p.Metrics.Uptime = hi.Uptime
	}
	if hostnameOverride != "" {
		p.Host.Hostname = hostnameOverride
	}
	if p.Host.Hostname == "" {
		p.Host.Hostname, _ = os.Hostname()
	}
	p.Host.IPAddress = primaryIP()
	p.Host.ServicesRunning = runningServices()

	if c, err := cpu.Counts(true); err == nil {
		p.Host.CPUCores = c
	}
	if pct, err := cpu.Percent(time.Second, false); err == nil && len(pct) > 0 {
		p.Metrics.CPUPercent = round2(pct[0])
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		p.Host.MemTotal = vm.Total
		p.Metrics.MemPercent = round2(vm.UsedPercent)
		p.Metrics.MemUsed = vm.Used
	}
	if du, err := disk.Usage(rootPath()); err == nil {
		p.Metrics.DiskPercent = round2(du.UsedPercent)
		p.Metrics.DiskUsed = du.Used
	}
	if la, err := load.Avg(); err == nil {
		p.Metrics.Load1 = round2(la.Load1)
		p.Metrics.Load5 = round2(la.Load5)
		p.Metrics.Load15 = round2(la.Load15)
	}
	if io, err := psnet.IOCounters(false); err == nil && len(io) > 0 {
		p.Metrics.NetRx = io[0].BytesRecv
		p.Metrics.NetTx = io[0].BytesSent
	}
	return p
}

// send envia a amostra e retorna o intervalo desejado pelo servidor (0 se n/d).
func send(cfg config, client *http.Client, p payload) (int, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.server+"/api/agent/metrics", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "upguard-agent/"+version)
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("servidor respondeu %d", resp.StatusCode)
	}
	var out ingestResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Config.IntervalSeconds, nil
}

func main() {
	cfg := loadConfig()
	if cfg.clientID == "" || cfg.clientSecret == "" {
		log.Fatal("client-id e client-secret são obrigatórios (flags ou UPGUARD_CLIENT_ID/UPGUARD_CLIENT_SECRET)")
	}

	log.Printf("upguard-agent %s iniciando — servidor=%s intervalo(inicial)=%s", version, cfg.server, cfg.interval)
	client := &http.Client{Timeout: 20 * time.Second}
	current := cfg.interval

	timer := time.NewTimer(0) // dispara imediatamente no início
	defer timer.Stop()
	for range timer.C {
		p := collect(cfg, client)
		serverInterval, err := send(cfg, client, p)
		if err != nil {
			log.Printf("erro ao enviar métricas: %v", err)
		} else {
			log.Printf("métricas enviadas: cpu=%.1f%% mem=%.1f%% disk=%.1f%% svc=%d",
				p.Metrics.CPUPercent, p.Metrics.MemPercent, p.Metrics.DiskPercent, p.Host.ServicesRunning)
			// Intervalo server-driven: ajusta se o portal mudou.
			if serverInterval >= 10 {
				want := time.Duration(serverInterval) * time.Second
				if want != current {
					log.Printf("intervalo ajustado pelo portal: %s -> %s", current, want)
					current = want
				}
			}
		}
		timer.Reset(current)
	}
}
