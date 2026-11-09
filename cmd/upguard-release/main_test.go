package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devshiftlabs/upguard-agent/internal/release"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func runCmd(t *testing.T, e map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := run(args, env(e), &out, &errb)
	return code, out.String(), errb.String()
}

func fakeDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	for _, tg := range release.Targets {
		goos, goarch, _ := strings.Cut(tg, "/")
		if err := os.WriteFile(filepath.Join(dist, release.BinaryName(goos, goarch)), []byte(tg), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func withEmbeddedKey(t *testing.T, pub string) {
	t.Helper()
	old := release.PublicKeyB64
	release.PublicKeyB64 = pub
	t.Cleanup(func() { release.PublicKeyB64 = old })
}

func TestUsageAndUnknown(t *testing.T) {
	if code, _, e := runCmd(t, nil); code != 2 || !strings.Contains(e, "uso:") {
		t.Fatalf("sem args: %d %q", code, e)
	}
	if code, _, e := runCmd(t, nil, "nada"); code != 1 || !strings.Contains(e, "desconhecido") {
		t.Fatalf("desconhecido: %d %q", code, e)
	}
	if code, _, _ := runCmd(t, nil, "stage", "-h"); code != 0 {
		t.Fatalf("-h deveria sair com 0, got %d", code)
	}
	if code, _, _ := runCmd(t, nil, "verify", "--nope"); code != 1 {
		t.Fatalf("flag inválida deveria falhar, got %d", code)
	}
	var m multiFlag
	_ = m.Set("a")
	_ = m.Set("b")
	if m.String() != "a,b" {
		t.Fatalf("multiFlag = %q", m.String())
	}
}

func TestGenkey(t *testing.T) {
	code, out, _ := runCmd(t, nil, "genkey")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 4 {
		t.Fatalf("genkey: %d %q", code, out)
	}
	pub, err := release.ParsePublicKey(lines[1])
	if err != nil || pub == nil {
		t.Fatalf("pública inválida: %v", err)
	}
	priv, err := release.ParsePrivateKey(lines[3])
	if err != nil || !pub.Equal(priv.Public()) {
		t.Fatalf("privada não corresponde: %v", err)
	}

	old := randReader
	randReader = failing{}
	defer func() { randReader = old }()
	if code, _, _ := runCmd(t, nil, "genkey"); code != 1 {
		t.Fatal("falha do gerador deveria sair com 1")
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("sem entropia") }

func TestStageVerifyUnsigned(t *testing.T) {
	withEmbeddedKey(t, "")
	dist := fakeDist(t)
	out := filepath.Join(t.TempDir(), "agent")
	extra := filepath.Join(t.TempDir(), "install.ps1")
	_ = os.WriteFile(extra, []byte("# ps"), 0o644)
	code, o, e := runCmd(t, nil, "stage", "-version", "v1.8.0", "-dist", dist, "-out", out, "-extra", extra)
	if code != 0 || !strings.Contains(o, "sem assinatura") {
		t.Fatalf("stage: %d %q %q", code, o, e)
	}
	if code, o, e := runCmd(t, nil, "verify", "-dir", out); code != 0 || !strings.Contains(o, "1.8.0") {
		t.Fatalf("verify: %d %q %q", code, o, e)
	}
	if code, _, _ := runCmd(t, nil, "stage", "-version", "1.8.0", "-dist", dist); code != 1 {
		t.Fatal("sem -out deveria falhar")
	}
	if code, _, _ := runCmd(t, nil, "verify"); code != 1 {
		t.Fatal("sem -dir deveria falhar")
	}
	if code, _, _ := runCmd(t, nil, "stage", "-version", "1.8.0", "-dist", t.TempDir(), "-out", out); code != 1 {
		t.Fatal("dist vazio deveria falhar")
	}
	_ = os.WriteFile(filepath.Join(out, "v1.8.0", "upguard-agent-linux-arm64"), []byte("adulterado"), 0o755)
	if code, _, e := runCmd(t, nil, "verify", "-dir", out); code != 1 || !strings.Contains(e, "não confere") {
		t.Fatalf("binário adulterado deveria falhar: %d %q", code, e)
	}
}

func TestStageVerifySigned(t *testing.T) {
	pub, priv, _ := release.GenerateKey(nil)
	otherPub, otherPriv, _ := release.GenerateKey(nil)
	withEmbeddedKey(t, pub)
	dist := fakeDist(t)
	out := filepath.Join(t.TempDir(), "agent")

	if code, _, e := runCmd(t, nil, "stage", "-version", "1.8.0", "-dist", dist, "-out", out); code != 1 || !strings.Contains(e, "AGENT_SIGNING_KEY") {
		t.Fatalf("com chave embutida e sem segredo deveria falhar: %d %q", code, e)
	}
	if code, _, e := runCmd(t, map[string]string{"AGENT_SIGNING_KEY": otherPriv}, "stage", "-version", "1.8.0", "-dist", dist, "-out", out); code != 1 || !strings.Contains(e, "não corresponde") {
		t.Fatalf("chave diferente deveria falhar: %d %q", code, e)
	}
	if code, _, _ := runCmd(t, map[string]string{"AGENT_SIGNING_KEY": "lixo"}, "stage", "-version", "1.8.0", "-dist", dist, "-out", out); code != 1 {
		t.Fatal("chave mal formada deveria falhar")
	}
	code, o, e := runCmd(t, map[string]string{"AGENT_SIGNING_KEY": priv}, "stage", "-version", "1.8.0", "-dist", dist, "-out", out)
	if code != 0 || !strings.Contains(o, "assinado") {
		t.Fatalf("stage assinado: %d %q %q", code, o, e)
	}
	if code, o, _ := runCmd(t, nil, "verify", "-dir", out); code != 0 || !strings.Contains(o, "ed25519") {
		t.Fatalf("verify com a chave embutida: %d %q", code, o)
	}
	if code, _, _ := runCmd(t, nil, "verify", "-dir", out, "-pubkey", otherPub); code != 1 {
		t.Fatal("verify com outra chave deveria falhar")
	}
	if code, _, _ := runCmd(t, nil, "verify", "-dir", out, "-pubkey", "lixo"); code != 1 {
		t.Fatal("pubkey mal formada deveria falhar")
	}

	withEmbeddedKey(t, "lixo")
	if code, _, _ := runCmd(t, map[string]string{"AGENT_SIGNING_KEY": priv}, "stage", "-version", "1.8.0", "-dist", dist, "-out", out); code != 1 {
		t.Fatal("chave embutida mal formada deveria falhar")
	}
}
