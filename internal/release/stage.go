package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StageOptions descreve uma publicação.
type StageOptions struct {
	Version string
	// DistDir tem os binários gerados pelo build (upguard-agent-<os>-<arch>[.exe]).
	DistDir string
	// OutDir recebe o layout publicado (o endereço base).
	OutDir string
	// Extras são copiados para a raiz de OutDir (instaladores).
	Extras []string
	// PrivateKey assina o manifesto; nil publica sem assinatura.
	PrivateKey ed25519.PrivateKey
	Now        time.Time
}

// Stage monta OutDir com todos os Targets, SHA256SUMS, latest.json (+ .sig) e
// o link latest/. Falta de qualquer plataforma é erro: os agentes de todas
// precisam achar o binário.
func Stage(o StageOptions) (*Manifest, error) {
	if !ValidVersion(o.Version) {
		return nil, fmt.Errorf("versão inválida %q", o.Version)
	}
	verDir := "v" + o.Version
	if err := os.MkdirAll(filepath.Join(o.OutDir, verDir), 0o755); err != nil {
		return nil, err
	}
	m := &Manifest{Version: o.Version, PublishedAt: o.Now.UTC().Format(time.RFC3339), Files: map[string]File{}}
	var sums []string
	for _, t := range Targets {
		goos, goarch, _ := strings.Cut(t, "/")
		name := BinaryName(goos, goarch)
		sum, size, err := copyFile(filepath.Join(o.DistDir, name), filepath.Join(o.OutDir, verDir, name), 0o755)
		if err != nil {
			return nil, fmt.Errorf("binário de %s: %w", t, err)
		}
		m.Files[PlatformKey(goos, goarch)] = File{Name: name, URL: verDir + "/" + name, SHA256: sum, Size: size}
		sums = append(sums, sum+"  "+name)
	}
	sort.Strings(sums)
	if err := os.WriteFile(filepath.Join(o.OutDir, verDir, ChecksumsName), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	for _, extra := range o.Extras {
		if _, _, err := copyFile(extra, filepath.Join(o.OutDir, filepath.Base(extra)), 0o644); err != nil {
			return nil, fmt.Errorf("extra %s: %w", extra, err)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(o.OutDir, ManifestName), data, 0o644); err != nil {
		return nil, err
	}
	if o.PrivateKey != nil {
		if err := os.WriteFile(filepath.Join(o.OutDir, SignatureName), Sign(o.PrivateKey, data), 0o644); err != nil {
			return nil, err
		}
	}
	latest := filepath.Join(o.OutDir, LatestDir)
	_ = os.Remove(latest)
	if err := os.Symlink(verDir, latest); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyDir confere um layout publicado: manifesto válido, assinatura (exigida
// se pub != nil), SHA-256 e tamanho de cada binário e o link latest/.
func VerifyDir(dir string, pub ed25519.PublicKey) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	if pub != nil {
		sig, err := os.ReadFile(filepath.Join(dir, SignatureName))
		if err != nil {
			return nil, fmt.Errorf("assinatura exigida: %w", err)
		}
		if err := Verify(pub, data, sig); err != nil {
			return nil, err
		}
	}
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for _, t := range Targets {
		goos, goarch, _ := strings.Cut(t, "/")
		f, err := m.File(goos, goarch)
		if err != nil {
			return nil, err
		}
		sum, size, err := hashFile(filepath.Join(dir, filepath.FromSlash(f.URL)))
		if err != nil {
			return nil, err
		}
		if sum != f.SHA256 || size != f.Size {
			return nil, fmt.Errorf("%s não confere com o manifesto", f.URL)
		}
	}
	if target, err := os.Readlink(filepath.Join(dir, LatestDir)); err != nil || target != "v"+m.Version {
		return nil, errors.New("link latest/ ausente ou apontando para outra versão")
	}
	return m, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// copyFile copia src para dst e devolve o SHA-256 e o tamanho do conteúdo.
func copyFile(src, dst string, mode os.FileMode) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
