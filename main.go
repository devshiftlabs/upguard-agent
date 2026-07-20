// UpGuard local monitoring agent.
//
// Coleta métricas de sistema (CPU, memória, disco, load, rede, uptime) e as
// envia periodicamente para a conta UpGuard, autenticado por client-id /
// client-secret (Basic auth). Binário único, sem dependências de runtime,
// cross-platform (Linux, macOS, Windows).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
)

// version é injetada no build via -ldflags "-X main.version=...".
var version = "dev"

const defaultServer = "https://api.upguard.com.br"

type hostInfo struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Platform     string `json:"platform"`
	AgentVersion string `json:"agent_version"`
	CPUCores     int    `json:"cpu_cores"`
	MemTotal     uint64 `json:"mem_total_bytes"`
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

type config struct {
	clientID     string
	clientSecret string
	server       string
	hostname     string
	interval     time.Duration
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	var (
		clientID  = flag.String("client-id", env("UPGUARD_CLIENT_ID", ""), "client id da credencial do agente")
		secret    = flag.String("client-secret", env("UPGUARD_CLIENT_SECRET", ""), "client secret da credencial do agente")
		server    = flag.String("server", env("UPGUARD_SERVER_URL", defaultServer), "URL base da API do UpGuard")
		hostname  = flag.String("hostname", env("UPGUARD_HOSTNAME", ""), "nome do host (default: hostname do sistema)")
		intervalS = flag.Int("interval", 0, "intervalo em segundos entre envios (default 60)")
		showVer   = flag.Bool("version", false, "imprime a versão e sai")
	)
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

	return config{
		clientID:     *clientID,
		clientSecret: *secret,
		server:       *server,
		hostname:     *hostname,
		interval:     time.Duration(interval) * time.Second,
	}
}

func rootPath() string {
	if runtime.GOOS == "windows" {
		return "C:\\"
	}
	return "/"
}

// collect coleta uma amostra de métricas. Erros por métrica são tolerados
// (o campo fica zerado) para não derrubar o ciclo inteiro.
func collect(hostnameOverride string) payload {
	p := payload{}
	p.Host.AgentVersion = version
	p.Host.OS = runtime.GOOS

	if hi, err := host.Info(); err == nil {
		p.Host.Hostname = hi.Hostname
		p.Host.Platform = fmt.Sprintf("%s %s", hi.Platform, hi.PlatformVersion)
		p.Metrics.Uptime = hi.Uptime
	}
	if hostnameOverride != "" {
		p.Host.Hostname = hostnameOverride
	}
	if p.Host.Hostname == "" {
		p.Host.Hostname, _ = os.Hostname()
	}

	if c, err := cpu.Counts(true); err == nil {
		p.Host.CPUCores = c
	}
	// cpu.Percent com janela de 1s para uma medição real.
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

	// load average só existe em Unix; em Windows retorna erro e fica 0.
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

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func send(cfg config, client *http.Client, p payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.server+"/api/agent/metrics", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "upguard-agent/"+version)
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("servidor respondeu %d", resp.StatusCode)
	}
	return nil
}

func main() {
	cfg := loadConfig()
	if cfg.clientID == "" || cfg.clientSecret == "" {
		log.Fatal("client-id e client-secret são obrigatórios (flags ou UPGUARD_CLIENT_ID/UPGUARD_CLIENT_SECRET)")
	}

	log.Printf("upguard-agent %s iniciando — servidor=%s intervalo=%s", version, cfg.server, cfg.interval)
	client := &http.Client{Timeout: 20 * time.Second}

	// Primeiro envio imediato, depois a cada intervalo.
	tick := func() {
		p := collect(cfg.hostname)
		if err := send(cfg, client, p); err != nil {
			log.Printf("erro ao enviar métricas: %v", err)
		} else {
			log.Printf("métricas enviadas: cpu=%.1f%% mem=%.1f%% disk=%.1f%%",
				p.Metrics.CPUPercent, p.Metrics.MemPercent, p.Metrics.DiskPercent)
		}
	}
	tick()

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		tick()
	}
}
