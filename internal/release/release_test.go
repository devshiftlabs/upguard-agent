package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidVersionAndNames(t *testing.T) {
	for v, want := range map[string]bool{
		"1.8.0": true, "10.20.30": true, "1.8.0-rc1": true, "1.8.0+build.2": true,
		"v1.8.0": false, "1.8": false, "": false, "dev": false, "1.8.0 ": false,
	} {
		if ValidVersion(v) != want {
			t.Errorf("ValidVersion(%q) != %v", v, want)
		}
	}
	if BinaryName("windows", "arm64") != "upguard-agent-windows-arm64.exe" ||
		BinaryName("linux", "amd64") != "upguard-agent-linux-amd64" ||
		PlatformKey("darwin", "arm64") != "darwin-arm64" {
		t.Fatal("nomes inesperados")
	}
}

const sumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParse(t *testing.T) {
	good := `{"version":"1.8.0","files":{"linux-amd64":{"name":"n","url":"v1.8.0/n","sha256":"` + sumA + `","size":3}}}`
	m, err := Parse([]byte(good))
	if err != nil || m.Version != "1.8.0" {
		t.Fatalf("Parse = %+v, %v", m, err)
	}
	if f, err := m.File("linux", "amd64"); err != nil || f.Size != 3 {
		t.Fatalf("File = %+v, %v", f, err)
	}
	if _, err := m.File("windows", "amd64"); err == nil || !strings.Contains(err.Error(), "windows-amd64") {
		t.Fatalf("plataforma ausente deveria falhar: %v", err)
	}
	bad := map[string]string{
		"json":         `{`,
		"versão":       `{"version":"latest","files":{"a":{}}}`,
		"sem binários": `{"version":"1.0.0","files":{}}`,
		"sha curto":    `{"version":"1.0.0","files":{"a":{"name":"n","url":"u","sha256":"abc"}}}`,
		"sha maiúsc.":  `{"version":"1.0.0","files":{"a":{"name":"n","url":"u","sha256":"` + strings.ToUpper(sumA) + `"}}}`,
		"sem url":      `{"version":"1.0.0","files":{"a":{"name":"n","sha256":"` + sumA + `"}}}`,
		"sem nome":     `{"version":"1.0.0","files":{"a":{"url":"u","sha256":"` + sumA + `"}}}`,
		"size<0":       `{"version":"1.0.0","files":{"a":{"name":"n","url":"u","sha256":"` + sumA + `","size":-1}}}`,
	}
	for name, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: deveria falhar", name)
		}
	}
}

func TestKeysSignVerify(t *testing.T) {
	pubB64, privB64, err := GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(" " + pubB64 + "\n")
	if err != nil || pub == nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	priv, err := ParsePrivateKey(privB64 + "\n")
	if err != nil || !pub.Equal(priv.Public()) {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	data := []byte(`{"version":"1.8.0"}`)
	sig := Sign(priv, data)
	if err := Verify(pub, data, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := Verify(pub, []byte(`{"version":"9.9.9"}`), sig); err == nil {
		t.Fatal("manifesto alterado deveria falhar")
	}
	if err := Verify(pub, data, []byte("!!!")); err == nil {
		t.Fatal("assinatura mal formada deveria falhar")
	}
	if k, err := ParsePublicKey(""); k != nil || err != nil {
		t.Fatal("chave vazia = sem assinatura")
	}
	for _, bad := range []string{"nao-base64!", "YWJj"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("pública %q deveria falhar", bad)
		}
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Errorf("privada %q deveria falhar", bad)
		}
	}
	if _, _, err := GenerateKey(errReader{}); err == nil {
		t.Fatal("falha do gerador deveria propagar")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("sem entropia") }

// fakeDist cria os binários de todas as plataformas.
func fakeDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	for _, tg := range Targets {
		goos, goarch, _ := strings.Cut(tg, "/")
		if err := os.WriteFile(filepath.Join(dist, BinaryName(goos, goarch)), []byte("bin "+tg), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func TestStageAndVerifyDir(t *testing.T) {
	dist := fakeDist(t)
	extra := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(extra, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pubB64, privB64, _ := GenerateKey(nil)
	pub, _ := ParsePublicKey(pubB64)
	priv, _ := ParsePrivateKey(privB64)
	out := filepath.Join(t.TempDir(), "agent")
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	m, err := Stage(StageOptions{Version: "1.8.0", DistDir: dist, OutDir: out, Extras: []string{extra}, PrivateKey: priv, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != len(Targets) || m.PublishedAt != "2026-11-08T12:00:00Z" {
		t.Fatalf("manifesto inesperado: %+v", m)
	}
	lin := m.Files["linux-amd64"]
	h := sha256.Sum256([]byte("bin linux/amd64"))
	if lin.URL != "v1.8.0/upguard-agent-linux-amd64" || lin.SHA256 != hex.EncodeToString(h[:]) {
		t.Fatalf("entrada linux: %+v", lin)
	}
	sums, _ := os.ReadFile(filepath.Join(out, "v1.8.0", ChecksumsName))
	if !strings.Contains(string(sums), lin.SHA256+"  upguard-agent-linux-amd64\n") {
		t.Fatalf("SHA256SUMS sem a linha do linux: %s", sums)
	}
	if _, err := os.Stat(filepath.Join(out, "install.sh")); err != nil {
		t.Fatal("extra não copiado")
	}
	if _, err := os.Stat(filepath.Join(out, LatestDir, "upguard-agent-linux-amd64")); err != nil {
		t.Fatal("latest/ deveria levar à versão")
	}
	if _, err := VerifyDir(out, pub); err != nil {
		t.Fatalf("VerifyDir assinado: %v", err)
	}

	// Republicar por cima (re-run do pipeline) troca o link latest/.
	if _, err := Stage(StageOptions{Version: "1.8.1", DistDir: dist, OutDir: out, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDir(out, nil); err != nil {
		t.Fatalf("VerifyDir sem assinatura: %v", err)
	}
	// A .sig que sobrou é da 1.8.0: com chave a verificação recusa.
	if _, err := VerifyDir(out, pub); err == nil {
		t.Fatal("assinatura de outro manifesto deveria falhar")
	}
}

func TestVerifyDirDetectsTampering(t *testing.T) {
	dist := fakeDist(t)
	pubB64, privB64, _ := GenerateKey(nil)
	pub, _ := ParsePublicKey(pubB64)
	priv, _ := ParsePrivateKey(privB64)
	stage := func(t *testing.T) string {
		out := filepath.Join(t.TempDir(), "agent")
		if _, err := Stage(StageOptions{Version: "1.8.0", DistDir: dist, OutDir: out, PrivateKey: priv}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := map[string]func(out string){
		"binário adulterado": func(out string) {
			_ = os.WriteFile(filepath.Join(out, "v1.8.0", "upguard-agent-linux-amd64"), []byte("bin linux/amd65"), 0o755)
		},
		"binário ausente": func(out string) { _ = os.Remove(filepath.Join(out, "v1.8.0", "upguard-agent-freebsd-arm64")) },
		"sem assinatura":  func(out string) { _ = os.Remove(filepath.Join(out, SignatureName)) },
		"sem manifesto":   func(out string) { _ = os.Remove(filepath.Join(out, ManifestName)) },
		"manifesto inválido": func(out string) {
			_ = os.WriteFile(filepath.Join(out, ManifestName), []byte("{}"), 0o644)
			_ = os.WriteFile(filepath.Join(out, SignatureName), Sign(priv, []byte("{}")), 0o644)
		},
		"plataforma fora do manifesto": func(out string) {
			data := []byte(`{"version":"1.8.0","files":{"linux-amd64":{"name":"n","url":"u","sha256":"` + sumA + `"}}}`)
			_ = os.WriteFile(filepath.Join(out, ManifestName), data, 0o644)
			_ = os.WriteFile(filepath.Join(out, SignatureName), Sign(priv, data), 0o644)
		},
		"latest para outra versão": func(out string) {
			_ = os.Remove(filepath.Join(out, LatestDir))
			_ = os.Symlink("v0.0.1", filepath.Join(out, LatestDir))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			out := stage(t)
			mutate(out)
			if _, err := VerifyDir(out, pub); err == nil {
				t.Fatal("deveria falhar")
			}
		})
	}
}

func TestStageErrors(t *testing.T) {
	dist := fakeDist(t)
	if _, err := Stage(StageOptions{Version: "v1", DistDir: dist, OutDir: t.TempDir()}); err == nil {
		t.Fatal("versão inválida deveria falhar")
	}
	missing := fakeDist(t)
	_ = os.Remove(filepath.Join(missing, "upguard-agent-windows-arm64.exe"))
	if _, err := Stage(StageOptions{Version: "1.0.0", DistDir: missing, OutDir: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "windows/arm64") {
		t.Fatalf("plataforma faltando deveria falhar: %v", err)
	}
	if _, err := Stage(StageOptions{Version: "1.0.0", DistDir: dist, OutDir: t.TempDir(), Extras: []string{"/nao/existe"}}); err == nil {
		t.Fatal("extra inexistente deveria falhar")
	}
	file := filepath.Join(t.TempDir(), "arquivo")
	_ = os.WriteFile(file, nil, 0o644)
	if _, err := Stage(StageOptions{Version: "1.0.0", DistDir: dist, OutDir: filepath.Join(file, "sub")}); err == nil {
		t.Fatal("destino inválido deveria falhar")
	}
	if _, _, err := hashFile(t.TempDir()); err == nil {
		t.Fatal("hash de diretório deveria falhar")
	}
	if _, _, err := copyFile(filepath.Join(dist, "upguard-agent-linux-amd64"), filepath.Join(file, "x"), 0o644); err == nil {
		t.Fatal("cópia para destino inválido deveria falhar")
	}
}
