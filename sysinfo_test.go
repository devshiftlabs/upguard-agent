package main

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
)

var errFake = errors.New("falha simulada")

type fakeProc struct {
	name    string
	nameErr error
	cpu     float64
	mem     float32
}

func (p fakeProc) Name() (string, error)           { return p.name, p.nameErr }
func (p fakeProc) CPUPercent() (float64, error)    { return p.cpu, nil }
func (p fakeProc) MemoryPercent() (float32, error) { return p.mem, nil }

// fakeSys devolve uma sysSource determinística com todas as leituras OK.
func fakeSys() sysSource {
	return sysSource{
		goos: "linux",
		hostInfo: func() (*host.InfoStat, error) {
			return &host.InfoStat{Hostname: "srv-a", Platform: "ubuntu", PlatformVersion: "24.04",
				KernelVersion: "6.8.0", Procs: 321, Uptime: 3600}, nil
		},
		hostname:  func() (string, error) { return "os-hostname", nil },
		cpuCounts: func(bool) (int, error) { return 8, nil },
		cpuPercent: func(iv time.Duration, percpu bool) ([]float64, error) {
			if iv != time.Second || percpu {
				return nil, errors.New("parâmetros inesperados")
			}
			return []float64{12.346}, nil
		},
		virtualMemory: func() (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Total: 16 << 30, Used: 4 << 30, UsedPercent: 25.004}, nil
		},
		diskUsage: func(path string) (*disk.UsageStat, error) {
			if path != "/" {
				return nil, errors.New("path inesperado " + path)
			}
			return &disk.UsageStat{Total: 100, Used: 40, UsedPercent: 40.126}, nil
		},
		loadAvg: func() (*load.AvgStat, error) { return &load.AvgStat{Load1: 1.111, Load5: 0.5, Load15: 0.25}, nil },
		netIO: func(bool) ([]psnet.IOCountersStat, error) {
			return []psnet.IOCountersStat{{BytesRecv: 10, BytesSent: 20}}, nil
		},
		processes: func() ([]procEntry, error) { return []procEntry{{1, fakeProc{name: "init", cpu: 1}}}, nil },
		primaryIP: func() string { return "10.0.0.5" },
		services:  func() []string { return []string{"nginx", "sshd"} },
	}
}

func TestCollectFull(t *testing.T) {
	software := []sw{{Name: "nginx", Version: "1.25.3"}}
	p := collect(config{}, fakeSys(), software, false)
	h := p.Host
	if h.Hostname != "srv-a" || h.OS != "linux" || h.Platform != "ubuntu 24.04" || h.Kernel != "6.8.0" ||
		h.Processes != 321 || h.CPUCores != 8 || h.MemTotal != 16<<30 || h.IPAddress != "10.0.0.5" ||
		h.ServicesRunning != 2 || h.AgentVersion != version {
		t.Fatalf("host inesperado: %+v", h)
	}
	if !reflect.DeepEqual(h.Software, software) || len(h.TopProcesses) != 1 {
		t.Fatalf("software/processos inesperados: %+v", h)
	}
	want := metrics{CPUPercent: 12.35, MemPercent: 25, MemUsed: 4 << 30, DiskPercent: 40.13, DiskUsed: 40,
		DiskTotal: 100, Load1: 1.11, Load5: 0.5, Load15: 0.25, NetRx: 10, NetTx: 20, Uptime: 3600}
	if p.Metrics == nil || *p.Metrics != want {
		t.Fatalf("métricas = %+v, want %+v", p.Metrics, want)
	}
}

func TestCollectPausedOmitsMetrics(t *testing.T) {
	src := fakeSys()
	src.cpuPercent = func(time.Duration, bool) ([]float64, error) {
		t.Fatal("pausado não deve amostrar CPU")
		return nil, nil
	}
	p := collect(config{}, src, nil, true)
	if p.Metrics != nil {
		t.Fatalf("pausado deveria omitir métricas, got %+v", p.Metrics)
	}
	if p.Host.Hostname != "srv-a" {
		t.Fatalf("heartbeat deveria levar infos do host: %+v", p.Host)
	}
}

func TestCollectHostnameOverrideAndFallback(t *testing.T) {
	p := collect(config{hostname: "custom"}, fakeSys(), nil, true)
	if p.Host.Hostname != "custom" {
		t.Fatalf("override ignorado: %q", p.Host.Hostname)
	}
	src := fakeSys()
	src.hostInfo = func() (*host.InfoStat, error) { return nil, errFake }
	p = collect(config{}, src, nil, true)
	if p.Host.Hostname != "os-hostname" || p.Host.Kernel != "" {
		t.Fatalf("fallback do hostname falhou: %+v", p.Host)
	}
}

func TestCollectToleratesErrors(t *testing.T) {
	src := fakeSys()
	src.goos = goosWindows
	src.hostInfo = func() (*host.InfoStat, error) { return nil, errFake }
	src.cpuCounts = func(bool) (int, error) { return 0, errFake }
	src.cpuPercent = func(time.Duration, bool) ([]float64, error) { return nil, nil } // vazio
	src.virtualMemory = func() (*mem.VirtualMemoryStat, error) { return nil, errFake }
	var gotPath string
	src.diskUsage = func(path string) (*disk.UsageStat, error) { gotPath = path; return nil, errFake }
	src.loadAvg = func() (*load.AvgStat, error) { return nil, errFake }
	src.netIO = func(bool) ([]psnet.IOCountersStat, error) { return nil, errFake }
	src.processes = func() ([]procEntry, error) { return nil, errFake }
	src.services = func() []string { return nil }

	p := collect(config{}, src, nil, false)
	if gotPath != `C:\` {
		t.Fatalf("Windows deveria medir C:\\, mediu %q", gotPath)
	}
	if p.Metrics == nil || *p.Metrics != (metrics{}) {
		t.Fatalf("com tudo falhando as métricas deveriam ficar zeradas: %+v", p.Metrics)
	}
	if p.Host.CPUCores != 0 || p.Host.MemTotal != 0 || p.Host.TopProcesses != nil || p.Host.ServicesRunning != 0 {
		t.Fatalf("host deveria ficar zerado: %+v", p.Host)
	}
}

func TestTopProcesses(t *testing.T) {
	list := func() ([]procEntry, error) {
		return []procEntry{
			{1, fakeProc{name: "low", cpu: 1, mem: 50}},
			{2, fakeProc{name: "high", cpu: 90.456, mem: 1}},
			{3, fakeProc{name: "", cpu: 99}},                       // sem nome: ignorado
			{4, fakeProc{name: "gone", nameErr: errFake, cpu: 99}}, // processo sumiu
			{5, fakeProc{name: "tie-more-mem", cpu: 1, mem: 60}},
		}, nil
	}
	got := topProcesses(list, 2)
	want := []procInfo{{PID: 2, Name: "high", CPU: 90.46, Mem: 1}, {PID: 5, Name: "tie-more-mem", CPU: 1, Mem: 60}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if all := topProcesses(list, 10); len(all) != 3 {
		t.Fatalf("sem corte deveria ter 3, got %d", len(all))
	}
	if topProcesses(func() ([]procEntry, error) { return nil, errFake }, 5) != nil {
		t.Fatal("erro na listagem deveria devolver nil")
	}
}

func TestRound2(t *testing.T) {
	for in, want := range map[float64]float64{0: 0, 1.004: 1, 1.006: 1.01, 99.999: 100, 12.3456: 12.35} {
		if got := round2(in); got != want {
			t.Errorf("round2(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestRootPath(t *testing.T) {
	if rootPath(goosWindows) != `C:\` || rootPath("linux") != "/" || rootPath("darwin") != "/" {
		t.Fatal("rootPath inesperado")
	}
}

type fakeConn struct {
	net.Conn
	local net.Addr
}

func (c fakeConn) LocalAddr() net.Addr { return c.local }
func (c fakeConn) Close() error        { return nil }

func TestPrimaryIP(t *testing.T) {
	udpOK := func(string, string) (net.Conn, error) {
		return fakeConn{local: &net.UDPAddr{IP: net.ParseIP("192.168.1.10"), Port: 5555}}, nil
	}
	dialFail := func(string, string) (net.Conn, error) { return nil, errFake }
	notUDP := func(string, string) (net.Conn, error) {
		return fakeConn{local: &net.TCPAddr{IP: net.ParseIP("1.2.3.4")}}, nil
	}
	ifaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPAddr{IP: net.ParseIP("9.9.9.9")},
			&net.IPNet{IP: net.ParseIP("10.1.2.3"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	noIfaces := func() ([]net.Addr, error) { return nil, errFake }
	tests := []struct {
		name   string
		dial   func(string, string) (net.Conn, error)
		ifaces func() ([]net.Addr, error)
		want   string
	}{
		{"ip de saída via udp", udpOK, noIfaces, "192.168.1.10"},
		{"dial falha: 1ª IPv4 não-loopback", dialFail, ifaces, "10.1.2.3"},
		{"endereço local não-UDP cai no fallback", notUDP, ifaces, "10.1.2.3"},
		{"nada disponível", dialFail, noIfaces, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := primaryIP(tt.dial, tt.ifaces); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunningServices(t *testing.T) {
	var calls []string
	run := func(out string, err error) cmdOutput {
		return func(name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte(out), err
		}
	}
	systemd := "sshd.service loaded active running OpenSSH\n\n  nginx.service loaded active running nginx\ncron.service x\n"
	if got := runningServices("linux", run(systemd, nil)); !reflect.DeepEqual(got, []string{"cron", "nginx", "sshd"}) {
		t.Fatalf("systemd: %v", got)
	}
	if !strings.HasPrefix(calls[0], "systemctl list-units --type=service --state=running") {
		t.Fatalf("comando systemd inesperado: %q", calls[0])
	}
	rc := "/etc/rc.d/sshd\n/usr/local/etc/rc.d/upguard_agent.sh\n\n/etc/rc.d/cron\n"
	if got := runningServices("freebsd", run(rc, nil)); !reflect.DeepEqual(got, []string{"cron", "sshd", "upguard_agent"}) {
		t.Fatalf("rc.d: %v", got)
	}
	if calls[1] != "service -e" {
		t.Fatalf("comando rc.d inesperado: %q", calls[1])
	}
	if runningServices("linux", run("", errFake)) != nil || runningServices("freebsd", run("", errFake)) != nil {
		t.Fatal("erro do comando deveria devolver nil")
	}
	n := len(calls)
	if runningServices("darwin", run("x", nil)) != nil || runningServices(goosWindows, run("x", nil)) != nil {
		t.Fatal("SO sem suporte deveria devolver nil")
	}
	if len(calls) != n {
		t.Fatal("SO sem suporte não deveria executar comando")
	}
}

func TestCommandOutputReal(t *testing.T) {
	out, err := commandOutput("go", "env", "GOOS")
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("commandOutput: %q, %v", out, err)
	}
	if _, err := commandOutput("upguard-binario-que-nao-existe"); err == nil {
		t.Fatal("binário inexistente deveria falhar")
	}
}

// TestDefaultSysSourceSmoke roda a coleta real (gopsutil) no host do teste.
func TestDefaultSysSourceSmoke(t *testing.T) {
	src := defaultSysSource()
	p := collect(config{}, src, nil, false)
	if p.Host.Hostname == "" || p.Host.CPUCores <= 0 || p.Host.MemTotal == 0 || p.Metrics == nil {
		t.Fatalf("coleta real incompleta: %+v %+v", p.Host, p.Metrics)
	}
	if p.Metrics.CPUPercent < 0 || p.Metrics.CPUPercent > 100 || p.Metrics.DiskTotal == 0 {
		t.Fatalf("métricas reais fora da faixa: %+v", p.Metrics)
	}
	if procs, err := listProcesses(); err != nil || len(procs) == 0 {
		t.Fatalf("listProcesses: %d, %v", len(procs), err)
	}
}
