package main

import (
	"io"
	"log"
	"os"
	"testing"
)

// TestMain silencia o log do agente (cada ciclo loga) para a saída dos testes
// ficar legível; use -v com UPGUARD_TEST_LOG=1 para ver.
func TestMain(m *testing.M) {
	if os.Getenv("UPGUARD_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}
