package main

// Espera crescente depois de credencial recusada (401/403).
//
// Sem isto, um agente com credencial revogada tentava de novo a cada intervalo
// (1 min) para sempre: ~2.900 requisições recusadas por dia por servidor, sem
// nada no log local que explicasse o motivo. Agora a espera dobra a cada recusa
// seguida (a partir do intervalo atual) até ~1 h, com jitter para os agentes de
// um mesmo cliente não baterem juntos, e volta ao intervalo normal assim que o
// UpGuard aceitar a credencial.

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"
)

const (
	// authBackoffMax é o teto da espera depois de credencial recusada.
	authBackoffMax = time.Hour
	// authBackoffJitter: a espera sorteada fica entre (1-jitter)×d e d.
	authBackoffJitter = 0.2
	// clientIDHintLen: quantos caracteres do client_id aparecem no log ("agt_2a63…").
	clientIDHintLen = 8
)

// authRejection extrai o status quando o erro é credencial recusada (401/403).
func authRejection(err error) (int, bool) {
	var se *statusError
	if errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
		return se.code, true
	}
	return 0, false
}

// authBackoff conta as recusas seguidas e calcula a próxima espera.
type authBackoff struct {
	failures int
	// jitter devolve um número em [0, 1); trocado nos testes.
	jitter func() float64
}

// next registra mais uma recusa e devolve a espera até a próxima tentativa:
// base×2^falhas, limitada a authBackoffMax, menos até 20% de jitter.
func (b *authBackoff) next(base time.Duration) time.Duration {
	b.failures++
	if base <= 0 {
		base = time.Duration(defaultInterval) * time.Second
	}
	d := base
	for i := 0; i < b.failures && d < authBackoffMax; i++ {
		d *= 2
	}
	if d > authBackoffMax {
		d = authBackoffMax
	}
	j := b.jitter
	if j == nil {
		j = cryptoJitter
	}
	return d - time.Duration(float64(d)*authBackoffJitter*j())
}

// reset zera o contador e diz quantas recusas seguidas havia.
func (b *authBackoff) reset() int {
	n := b.failures
	b.failures = 0
	return n
}

// cryptoJitter é um float em [0, 1) de crypto/rand (sem gerador previsível).
func cryptoJitter() float64 {
	var buf [8]byte
	_, _ = cryptorand.Read(buf[:]) // nunca falha desde o Go 1.24
	return float64(binary.BigEndian.Uint64(buf[:])>>11) / (1 << 53)
}

// clientIDHint é o começo do client_id para o log; o secret nunca é impresso.
func clientIDHint(id string) string {
	if len(id) <= clientIDHintLen {
		return id
	}
	return id[:clientIDHintLen] + "…"
}

// credentialHint diz onde conferir a credencial em cada sistema.
func credentialHint(goos string) string {
	if goos == "windows" {
		return "confira CLIENT_ID/SECRET do serviço (UPGUARD_CLIENT_ID/UPGUARD_CLIENT_SECRET ou --client-id/--client-secret)"
	}
	return "confira CLIENT_ID/SECRET em /etc/upguard-agent/agent.env (UPGUARD_CLIENT_ID/UPGUARD_CLIENT_SECRET)"
}

// authRejectedMessage é a linha de log de uma recusa (sem o secret).
func authRejectedMessage(code int, clientID string, failures int, wait time.Duration, goos string) string {
	reason := "credencial recusada pelo UpGuard"
	if code == http.StatusForbidden {
		reason = "credencial recusada pelo UpGuard (sem permissão ou servidor novo acima do limite de itens do plano)"
	}
	return fmt.Sprintf("%s: HTTP %d, client_id %s; %s. Recusa %d seguida, próxima tentativa em %s",
		reason, code, clientIDHint(clientID), credentialHint(goos), failures, wait.Round(time.Second))
}

// onAuthRejected registra a recusa e devolve a espera crescente.
func (a *agent) onAuthRejected(code int) time.Duration {
	wait := a.auth.next(a.state.nextDelay())
	log.Print(authRejectedMessage(code, a.cfg.clientID, a.auth.failures, wait, runtime.GOOS))
	return wait
}

// onAuthAccepted volta ao intervalo normal depois de uma sequência de recusas.
func (a *agent) onAuthAccepted() {
	if n := a.auth.reset(); n > 0 {
		log.Printf("credencial aceita pelo UpGuard depois de %d recusa(s); voltando ao intervalo normal", n)
	}
}
