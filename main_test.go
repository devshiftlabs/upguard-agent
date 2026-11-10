package main

import (
	"io"
	"log"
	"os"
	"testing"

	"github.com/devshiftlabs/upguard-agent/internal/release"
)

// TestMain silencia o log do agente (cada ciclo loga) para a saída dos testes
// ficar legível; use -v com UPGUARD_TEST_LOG=1 para ver.
//
// Também zera a chave pública embutida: os testes do auto-update montam
// manifestos com uma chave descartável própria (ou sem assinatura) e não podem
// depender da chave de produção em internal/release.PublicKeyB64. Quem precisa
// de chave embutida a define no próprio teste (TestNewUpdaterEmbeddedKey).
func TestMain(m *testing.M) {
	if os.Getenv("UPGUARD_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	release.PublicKeyB64 = ""
	os.Exit(m.Run())
}
