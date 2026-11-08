// UpGuard local monitoring agent.
//
// Coleta métricas de sistema (CPU, memória, disco, load, rede, uptime) + infos
// (IP, kernel, processos, serviços) e as envia periodicamente para a conta
// UpGuard, autenticado por client-id / client-secret (Basic auth). O intervalo
// é definido pelo portal: o servidor devolve o intervalo desejado a cada envio
// e o agente se ajusta. Binário único, sem dependências, cross-platform.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
)

// main só faz a ligação com o processo (args, env, exit code); a lógica vive
// em parseConfig e agent.run, que têm testes.
func main() {
	cfg, showVersion, err := parseConfig(os.Args[1:], os.Getenv, os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case err != nil:
		os.Exit(2)
	case showVersion:
		fmt.Println(versionString())
		return
	}
	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}
	newAgent(cfg).run(context.Background())
}
