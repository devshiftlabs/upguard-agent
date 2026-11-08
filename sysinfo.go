package main

// Coleta de métricas e infos do host. As fontes (gopsutil, comandos do SO,
// rede) ficam atrás de sysSource para que a lógica de montagem do payload seja
// testável com fakes; defaultSysSource liga tudo às implementações reais.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	goosWindows = "windows"
	networkTCP  = "tcp"
	// topProcessLimit é quantos processos (por CPU) vão no payload.
	topProcessLimit = 50
)

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

// procHandle é o subconjunto de *process.Process usado no ranking de processos.
type procHandle interface {
	Name() (string, error)
	CPUPercent() (float64, error)
	MemoryPercent() (float32, error)
}

type procEntry struct {
	pid int32
	h   procHandle
}

// cmdOutput executa um comando (sem shell) e devolve o stdout.
type cmdOutput func(name string, args ...string) ([]byte, error)

// sysSource reúne as fontes de dados do host.
type sysSource struct {
	goos          string
	hostInfo      func() (*host.InfoStat, error)
	hostname      func() (string, error)
	cpuCounts     func(logical bool) (int, error)
	cpuPercent    func(interval time.Duration, percpu bool) ([]float64, error)
	virtualMemory func() (*mem.VirtualMemoryStat, error)
	diskUsage     func(path string) (*disk.UsageStat, error)
	loadAvg       func() (*load.AvgStat, error)
	netIO         func(pernic bool) ([]psnet.IOCountersStat, error)
	processes     func() ([]procEntry, error)
	primaryIP     func() string
	services      func() []string
}

// defaultSysSource liga sysSource ao sistema real.
func defaultSysSource() sysSource {
	return sysSource{
		goos:          runtime.GOOS,
		hostInfo:      host.Info,
		hostname:      os.Hostname,
		cpuCounts:     cpu.Counts,
		cpuPercent:    cpu.Percent,
		virtualMemory: mem.VirtualMemory,
		diskUsage:     disk.Usage,
		loadAvg:       load.Avg,
		netIO:         psnet.IOCounters,
		processes:     listProcesses,
		primaryIP:     func() string { return primaryIP(net.Dial, net.InterfaceAddrs) },
		services:      func() []string { return runningServices(runtime.GOOS, commandOutput) },
	}
}

// commandOutput é a implementação real de cmdOutput.
func commandOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// listProcesses lista os processos do host via gopsutil.
func listProcesses() ([]procEntry, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	out := make([]procEntry, len(procs))
	for i, p := range procs {
		out[i] = procEntry{pid: p.Pid, h: p}
	}
	return out, nil
}

func rootPath(goos string) string {
	if goos == goosWindows {
		return "C:\\"
	}
	return "/"
}

// primaryIP descobre o IP de saída (sem enviar pacotes) com fallback para a
// primeira interface IPv4 não-loopback.
func primaryIP(dial func(network, address string) (net.Conn, error), interfaceAddrs func() ([]net.Addr, error)) string {
	if conn, err := dial("udp", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return a.IP.String()
		}
	}
	addrs, _ := interfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}

// runningServices lista os serviços do host. systemd no Linux, rc.d no
// FreeBSD/pfSense; em outros SOs, nil.
func runningServices(goos string, run cmdOutput) []string {
	switch goos {
	case "linux":
		return systemdServices(run)
	case "freebsd":
		return rcServices(run)
	default:
		return nil
	}
}

// systemdServices lista os serviços systemd ativos (Linux).
func systemdServices(run cmdOutput) []string {
	out, err := run("systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--no-pager", "--plain")
	if err != nil {
		return nil
	}
	return parseSystemdUnits(string(out))
}

// parseSystemdUnits extrai o nome (sem ".service") da 1ª coluna de cada linha.
func parseSystemdUnits(out string) []string {
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		svcs = append(svcs, strings.TrimSuffix(fields[0], ".service"))
	}
	sort.Strings(svcs)
	return svcs
}

// rcServices lista os serviços rc.d habilitados (FreeBSD/pfSense).
func rcServices(run cmdOutput) []string {
	out, err := run("service", "-e")
	if err != nil {
		return nil
	}
	return parseRCServices(string(out))
}

// parseRCServices: `service -e` devolve o caminho de cada script habilitado;
// reduzimos ao nome-base.
func parseRCServices(out string) []string {
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
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
func topProcesses(list func() ([]procEntry, error), limit int) []procInfo {
	procs, err := list()
	if err != nil {
		return nil
	}
	out := make([]procInfo, 0, len(procs))
	for _, pr := range procs {
		name, err := pr.h.Name()
		if err != nil || name == "" {
			continue
		}
		cpuPct, _ := pr.h.CPUPercent()
		memPct, _ := pr.h.MemoryPercent()
		out = append(out, procInfo{
			PID:  pr.pid,
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

// collect coleta uma amostra. Erros por métrica são tolerados (campo fica zero).
// Pausado, devolve só as infos do host (heartbeat), sem o bloco de métricas.
func collect(cfg config, src sysSource, software []sw, paused bool) payload {
	p := payload{}
	uptime, vm := fillHostInfo(&p.Host, cfg, src, software)
	if paused {
		// O campo metrics é omitido: o servidor não grava amostra nem avalia alertas.
		return p
	}
	p.Metrics = collectMetrics(src, uptime, vm)
	return p
}

// fillHostInfo preenche as infos do host e devolve o uptime e a leitura de
// memória (nil se falhou), reaproveitada nas métricas.
func fillHostInfo(h *hostInfo, cfg config, src sysSource, software []sw) (uint64, *mem.VirtualMemoryStat) {
	h.AgentVersion = version
	h.OS = src.goos
	h.Software = software

	var uptime uint64
	if hi, err := src.hostInfo(); err == nil {
		h.Hostname = hi.Hostname
		h.Platform = strings.TrimSpace(fmt.Sprintf("%s %s", hi.Platform, hi.PlatformVersion))
		h.Kernel = hi.KernelVersion
		h.Processes = hi.Procs
		uptime = hi.Uptime
	}
	if cfg.hostname != "" {
		h.Hostname = cfg.hostname
	}
	if h.Hostname == "" {
		h.Hostname, _ = src.hostname()
	}
	h.IPAddress = src.primaryIP()
	svcs := src.services()
	h.ServicesList = svcs
	h.ServicesRunning = len(svcs)
	h.TopProcesses = topProcesses(src.processes, topProcessLimit)

	if c, err := src.cpuCounts(true); err == nil {
		h.CPUCores = c
	}
	vm, err := src.virtualMemory()
	if err != nil {
		return uptime, nil
	}
	h.MemTotal = vm.Total
	return uptime, vm
}

// collectMetrics lê CPU (amostra de 1s), disco da raiz, load e rede; a memória
// vem da leitura já feita (vm nil = indisponível).
func collectMetrics(src sysSource, uptime uint64, vm *mem.VirtualMemoryStat) *metrics {
	m := &metrics{Uptime: uptime}
	if pct, err := src.cpuPercent(time.Second, false); err == nil && len(pct) > 0 {
		m.CPUPercent = round2(pct[0])
	}
	if vm != nil {
		m.MemPercent = round2(vm.UsedPercent)
		m.MemUsed = vm.Used
	}
	if du, err := src.diskUsage(rootPath(src.goos)); err == nil {
		m.DiskPercent = round2(du.UsedPercent)
		m.DiskUsed = du.Used
		m.DiskTotal = du.Total
	}
	if la, err := src.loadAvg(); err == nil {
		m.Load1 = round2(la.Load1)
		m.Load5 = round2(la.Load5)
		m.Load15 = round2(la.Load15)
	}
	if nio, err := src.netIO(false); err == nil && len(nio) > 0 {
		m.NetRx = nio[0].BytesRecv
		m.NetTx = nio[0].BytesSent
	}
	return m
}
