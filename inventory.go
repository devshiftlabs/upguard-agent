package main

// Inventário de software (probes definidos no servidor): cada probe diz qual
// binário rodar e com quais argumentos para extrair a versão instalada.

import (
	"bytes"
	"context"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	probeTimeout    = 5 * time.Second
	inventoryMaxAge = time.Hour
)

type probe struct {
	Name      string   `json:"name"`
	Bin       string   `json:"bin"`
	Args      []string `json:"args"`
	UseStderr bool     `json:"use_stderr"`
}

var (
	binNameRe     = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	versionLineRe = regexp.MustCompile(`\d+\.\d+(\.\d+)*`)
)

// probeExec roda o binário (caminho absoluto) e devolve o stdout ou o stderr.
type probeExec func(path string, args []string, useStderr bool) string

// runProbeCmd é a implementação real de probeExec: sem shell, timeout de 5s.
func runProbeCmd(path string, args []string, useStderr bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var buf bytes.Buffer
	if useStderr {
		cmd.Stderr = &buf
	} else {
		cmd.Stdout = &buf
	}
	_ = cmd.Run()
	return buf.String()
}

// runProbes executa cada probe e coleta a versão. Só roda binários com nome
// simples presentes no PATH; ignora o resto (segurança).
func runProbes(probes []probe, lookPath func(string) (string, error), run probeExec) []sw {
	var out []sw
	for _, pr := range probes {
		if !binNameRe.MatchString(pr.Bin) {
			continue // nome de binário inválido — ignora
		}
		path, err := lookPath(pr.Bin)
		if err != nil {
			continue // não instalado / fora do PATH
		}
		out = append(out, sw{Name: pr.Name, Version: parseVersion(run(path, pr.Args, pr.UseStderr))})
	}
	return out
}

// parseVersion pega a primeira linha não vazia e extrai "x.y[.z...]"; sem
// número de versão, devolve a linha inteira (ou "instalado" se não houve saída).
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

// swInventory guarda o inventário em cache por 1h.
type swInventory struct {
	fetch    func() ([]probe, bool)
	run      func([]probe) []sw
	now      func() time.Time
	cache    []sw
	cachedAt time.Time
}

func newSWInventory(cfg config, client *http.Client) *swInventory {
	return &swInventory{
		fetch: func() ([]probe, bool) { return fetchProbes(cfg, client) },
		run:   func(p []probe) []sw { return runProbes(p, exec.LookPath, runProbeCmd) },
		now:   time.Now,
	}
}

// get devolve o inventário (cache de 1h); rebusca probes + reexecuta. Se a
// busca falhar, mantém o inventário anterior.
func (s *swInventory) get() []sw {
	if s.cache != nil && s.now().Sub(s.cachedAt) < inventoryMaxAge {
		return s.cache
	}
	probes, ok := s.fetch()
	if !ok && s.cache != nil {
		return s.cache
	}
	s.cache = s.run(probes)
	s.cachedAt = s.now()
	return s.cache
}
