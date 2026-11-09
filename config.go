package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"time"
)

// version vem de -ldflags "-X main.version=..." no build.
var version = "dev"

// userAgent é resolvido em runtime (version vem de -ldflags no build).
var userAgent = "upguard-agent/" + version

const (
	defaultServer   = "https://api.upguard.com.br"
	defaultInterval = 60 // segundos
	minInterval     = 10 // segundos
)

type config struct {
	clientID, clientSecret, server, hostname string
	interval                                 time.Duration
	// updateURL é o endereço base do auto-update ("off" desliga).
	updateURL string
}

// errMissingCredentials é devolvido quando client-id/secret não foram informados.
var errMissingCredentials = errors.New("client-id e client-secret são obrigatórios (flags ou UPGUARD_CLIENT_ID/UPGUARD_CLIENT_SECRET)")

// envOr devolve getenv(key) ou def quando vazio.
func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// parseConfig lê flags (args, sem o nome do programa) com fallback nas
// variáveis de ambiente. showVersion indica que foi pedido --version. Erros de
// parse (inclusive flag.ErrHelp) já foram impressos em out pelo FlagSet.
func parseConfig(args []string, getenv func(string) string, out io.Writer) (cfg config, showVersion bool, err error) {
	fs := flag.NewFlagSet("upguard-agent", flag.ContinueOnError)
	fs.SetOutput(out)
	clientID := fs.String("client-id", envOr(getenv, "UPGUARD_CLIENT_ID", ""), "client id do agente")
	secret := fs.String("client-secret", envOr(getenv, "UPGUARD_CLIENT_SECRET", ""), "client secret do agente")
	server := fs.String("server", envOr(getenv, "UPGUARD_SERVER_URL", defaultServer), "URL base da API do UpGuard")
	hostname := fs.String("hostname", envOr(getenv, "UPGUARD_HOSTNAME", ""), "nome do host (default: hostname do sistema)")
	updateURL := fs.String("update-url", envOr(getenv, "UPGUARD_UPDATE_URL", defaultUpdateURL), "endereço base do auto-update (off desliga)")
	intervalS := fs.Int("interval", 0, "intervalo inicial em segundos (o portal pode sobrescrever)")
	showVer := fs.Bool("version", false, "imprime a versão e sai")
	if err := fs.Parse(args); err != nil {
		return config{}, false, err
	}
	if *showVer {
		return config{}, true, nil
	}

	interval := defaultInterval
	if *intervalS > 0 {
		interval = *intervalS
	} else if v := getenv("UPGUARD_INTERVAL"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &interval)
	}
	if interval < minInterval {
		interval = minInterval
	}
	return config{
		clientID: *clientID, clientSecret: *secret, server: *server, hostname: *hostname,
		interval: time.Duration(interval) * time.Second, updateURL: *updateURL,
	}, false, nil
}

// validate confere os campos obrigatórios.
func (c config) validate() error {
	if c.clientID == "" || c.clientSecret == "" {
		return errMissingCredentials
	}
	return nil
}

// versionString é a linha impressa por --version.
func versionString() string {
	return fmt.Sprintf("upguard-agent %s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}
