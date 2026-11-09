// Package release define o manifesto das versões do agente publicado no
// endereço de download próprio do UpGuard (https://upguard.com.br/downloads/agent)
// e a verificação dele: SHA-256 de cada binário e, quando há chave, assinatura
// ed25519 do manifesto. É usado pelo agente (auto-update) e pela ferramenta
// de release (cmd/upguard-release), para os dois lados concordarem no formato.
//
// Layout publicado (relativo ao endereço base):
//
//	latest.json             manifesto da versão atual
//	latest.json.sig         assinatura ed25519 (base64) dos bytes de latest.json
//	install.sh, install.ps1 instaladores
//	v<versão>/upguard-agent-<os>-<arch>[.exe] + SHA256SUMS
//	latest/ -> v<versão>    link usado pelos instaladores
package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	// ManifestName é o manifesto da última versão.
	ManifestName = "latest.json"
	// SignatureName é a assinatura ed25519 (base64) de ManifestName.
	SignatureName = "latest.json.sig"
	// ChecksumsName é a lista "sha256  arquivo" de cada versão (formato do sha256sum).
	ChecksumsName = "SHA256SUMS"
	// LatestDir aponta para o diretório da última versão.
	LatestDir = "latest"
	// MaxManifestSize limita o que o agente lê do manifesto e da assinatura.
	MaxManifestSize = 1 << 20
)

// PublicKeyB64 é a chave pública ed25519 (32 bytes em base64) que assina o
// manifesto. Vazia, o agente confere só o SHA-256 (o manifesto vem por HTTPS);
// preenchida, o agente passa a EXIGIR a assinatura. Gere o par com
// `go run ./cmd/upguard-release genkey`: a pública entra aqui (commit) e a
// privada fica só no segredo AGENT_SIGNING_KEY do repositório.
var PublicKeyB64 = ""

// Targets são as plataformas publicadas — a mesma lista do build.sh e do release.yml.
var Targets = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
	"freebsd/amd64", "freebsd/arm64",
}

// File descreve o binário de uma plataforma.
type File struct {
	Name string `json:"name"`
	// URL é relativa ao endereço base (ex.: "v1.8.0/upguard-agent-linux-amd64").
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest é o conteúdo de latest.json.
type Manifest struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
	// Files é indexado por PlatformKey ("linux-amd64").
	Files map[string]File `json:"files"`
}

var (
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidVersion informa se v é uma versão x.y.z (sem o "v").
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// PlatformKey é a chave de Files para goos/goarch.
func PlatformKey(goos, goarch string) string { return goos + "-" + goarch }

// BinaryName é o nome do binário publicado para goos/goarch.
func BinaryName(goos, goarch string) string {
	name := "upguard-agent-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Parse decodifica e valida um manifesto.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifesto inválido: %w", err)
	}
	if !ValidVersion(m.Version) {
		return nil, fmt.Errorf("manifesto com versão inválida %q", m.Version)
	}
	if len(m.Files) == 0 {
		return nil, errors.New("manifesto sem binários")
	}
	for key, f := range m.Files {
		if f.Name == "" || f.URL == "" || !sha256Re.MatchString(f.SHA256) || f.Size < 0 {
			return nil, fmt.Errorf("manifesto com entrada inválida para %s", key)
		}
	}
	return &m, nil
}

// File devolve o binário de goos/goarch.
func (m *Manifest) File(goos, goarch string) (File, error) {
	f, ok := m.Files[PlatformKey(goos, goarch)]
	if !ok {
		return File{}, fmt.Errorf("a versão %s não tem binário para %s", m.Version, PlatformKey(goos, goarch))
	}
	return f, nil
}

// ParsePublicKey decodifica a chave pública em base64; vazia devolve nil.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("chave pública ed25519 inválida (esperado base64 de 32 bytes)")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey decodifica a semente ed25519 (32 bytes em base64), formato
// do segredo AGENT_SIGNING_KEY.
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("chave privada ed25519 inválida (esperado base64 da semente de 32 bytes)")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// GenerateKey gera um par novo: pública e semente privada, ambas em base64.
func GenerateKey(rand io.Reader) (pubB64, privB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

// Sign devolve a assinatura de data em base64 (com quebra de linha final).
func Sign(priv ed25519.PrivateKey, data []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n")
}

// Verify confere a assinatura (base64) de data com a chave pública.
func Verify(pub ed25519.PublicKey, data, sigB64 []byte) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("assinatura do manifesto mal formada")
	}
	if !ed25519.Verify(pub, data, sig) {
		return errors.New("assinatura do manifesto não confere")
	}
	return nil
}
