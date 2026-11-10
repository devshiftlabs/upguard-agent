package main

// Laço principal do agente: coleta, envia, aplica a config devolvida pelo
// portal (intervalo, pausa, update forçado) e roda os checks privados.

import (
	"context"
	"log"
	"net/http"
	"time"
)

const (
	httpTimeout         = 20 * time.Second
	updateCheckInterval = 6 * time.Hour
	// pausedPollInterval: pausado, o agente consulta o portal com mais
	// frequência para retomar rápido.
	pausedPollInterval = 30 * time.Second
)

// selfUpdater é o que o laço usa do updater (fake nos testes).
type selfUpdater interface {
	maybeUpdate(force bool)
}

type loopState struct {
	current time.Duration
	paused  bool
}

// apply processa a config server-driven: update forçado, pausa e intervalo.
func (st *loopState) apply(upd selfUpdater, sc serverConfig) {
	if sc.updateRequested {
		upd.maybeUpdate(true) // força; re-executa e não retorna
	}
	if sc.paused != st.paused {
		log.Printf("estado alterado pelo portal: paused=%v", sc.paused)
		st.paused = sc.paused
	}
	if sc.interval >= minInterval {
		want := time.Duration(sc.interval) * time.Second
		if want != st.current {
			log.Printf("intervalo ajustado pelo portal: %s -> %s", st.current, want)
			st.current = want
		}
	}
}

// nextDelay é a espera até o próximo ciclo.
func (st *loopState) nextDelay() time.Duration {
	if st.paused && st.current > pausedPollInterval {
		return pausedPollInterval
	}
	return st.current
}

func logSent(p payload) {
	if p.Metrics != nil {
		log.Printf("métricas enviadas: cpu=%.1f%% mem=%.1f%% disk=%.1f%% svc=%d",
			p.Metrics.CPUPercent, p.Metrics.MemPercent, p.Metrics.DiskPercent, p.Host.ServicesRunning)
	} else {
		log.Printf("pausado — heartbeat enviado (sem métricas)")
	}
}

type agent struct {
	cfg             config
	client          *http.Client
	sys             sysSource
	inv             *swInventory
	upd             selfUpdater
	checks          *checkRunner
	state           loopState
	hostID          string
	auth            authBackoff
	lastUpdateCheck time.Time
	now             func() time.Time
}

func newAgent(cfg config) *agent {
	client := &http.Client{Timeout: httpTimeout}
	return &agent{
		cfg:    cfg,
		client: client,
		sys:    defaultSysSource(),
		inv:    newSWInventory(cfg, client),
		upd:    newUpdater(client, cfg.updateURL),
		checks: newCheckRunner(),
		state:  loopState{current: cfg.interval},
		now:    time.Now,
	}
}

// tick executa um ciclo e devolve a espera até o próximo.
func (a *agent) tick() time.Duration {
	// Auto-update periódico (a cada 6h; também no primeiro ciclo). Se houver
	// versão nova, selfUpdate re-executa o processo e não retorna.
	if a.now().Sub(a.lastUpdateCheck) > updateCheckInterval {
		a.lastUpdateCheck = a.now()
		a.upd.maybeUpdate(false)
	}
	p := collect(a.cfg, a.sys, a.inv.get(), a.state.paused)
	sc, err := send(a.cfg, a.client, p)
	if err != nil {
		if code, rejected := authRejection(err); rejected {
			return a.onAuthRejected(code)
		}
		log.Printf("erro ao enviar métricas: %v", err)
		return a.state.nextDelay()
	}
	a.onAuthAccepted()
	logSent(p)
	if sc.hostID != "" {
		a.hostID = sc.hostID
	}
	a.state.apply(a.upd, sc)
	// Checks privados (bancos na rede local) — não roda pausado.
	if !a.state.paused {
		a.checks.cycle(a.cfg, a.client, a.hostID)
	}
	return a.state.nextDelay()
}

// run executa ciclos até ctx ser cancelado; o primeiro é imediato.
func (a *agent) run(ctx context.Context) {
	log.Printf("upguard-agent %s iniciando — servidor=%s intervalo(inicial)=%s auto-update=%s", version, a.cfg.server, a.cfg.interval, a.cfg.updateURL)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(a.tick())
		}
	}
}
