// upguard-release monta e confere a publicação do agente no endereço de
// download próprio (https://upguard.com.br/downloads/agent).
//
//	upguard-release genkey
//	    gera o par ed25519: a pública vai em internal/release (PublicKeyB64) e
//	    a privada no segredo AGENT_SIGNING_KEY do repositório.
//	upguard-release stage -version 1.8.0 -dist dist -out site/downloads/agent [-extra arquivo]...
//	    monta o layout publicado; assina com AGENT_SIGNING_KEY se houver.
//	upguard-release verify -dir site/downloads/agent [-pubkey base64]
//	    confere manifesto, assinatura e SHA-256 de cada binário.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/devshiftlabs/upguard-agent/internal/release"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "uso: upguard-release genkey | stage | verify")
		return 2
	}
	var err error
	switch args[0] {
	case "genkey":
		err = genkey(stdout)
	case "stage":
		err = stage(args[1:], getenv, stdout, stderr)
	case "verify":
		err = verify(args[1:], stdout, stderr)
	default:
		err = fmt.Errorf("comando desconhecido: %s", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "erro:", err)
		return 1
	}
	return 0
}

var randReader io.Reader = rand.Reader

func genkey(stdout io.Writer) error {
	pub, priv, err := release.GenerateKey(randReader)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "# Pública — cole em internal/release/release.go (PublicKeyB64) e faça commit:\n%s\n", pub)
	fmt.Fprintf(stdout, "# Privada — grave SÓ como segredo AGENT_SIGNING_KEY do repositório (não salve em arquivo):\n%s\n", priv)
	return nil
}

// signingKey lê AGENT_SIGNING_KEY e confere a coerência com a chave embutida:
// se o agente exige assinatura, publicar sem ela (ou com outra chave) faria
// todos os agentes recusarem a versão.
func signingKey(getenv func(string) string, embedded ed25519.PublicKey) (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(getenv("AGENT_SIGNING_KEY"))
	if raw == "" {
		if embedded != nil {
			return nil, errors.New("o agente exige assinatura (PublicKeyB64 preenchida) mas AGENT_SIGNING_KEY está vazio")
		}
		return nil, nil
	}
	priv, err := release.ParsePrivateKey(raw)
	if err != nil {
		return nil, err
	}
	if embedded != nil && !embedded.Equal(priv.Public()) {
		return nil, errors.New("AGENT_SIGNING_KEY não corresponde à PublicKeyB64 embutida no agente")
	}
	return priv, nil
}

func stage(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "versão (sem o v)")
	dist := fs.String("dist", "dist", "diretório dos binários")
	out := fs.String("out", "", "diretório publicado")
	var extras multiFlag
	fs.Var(&extras, "extra", "arquivo copiado para a raiz (repetível)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out é obrigatório")
	}
	embedded, err := release.ParsePublicKey(release.PublicKeyB64)
	if err != nil {
		return err
	}
	priv, err := signingKey(getenv, embedded)
	if err != nil {
		return err
	}
	m, err := release.Stage(release.StageOptions{
		Version: strings.TrimPrefix(*version, "v"), DistDir: *dist, OutDir: *out,
		Extras: extras, PrivateKey: priv, Now: time.Now(),
	})
	if err != nil {
		return err
	}
	signed := "sem assinatura"
	if priv != nil {
		signed = "assinado"
	}
	fmt.Fprintf(stdout, "publicação %s montada em %s (%d binários, %s)\n", m.Version, *out, len(m.Files), signed)
	return nil
}

func verify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "diretório publicado")
	pubFlag := fs.String("pubkey", release.PublicKeyB64, "chave pública ed25519 (base64); vazia = sem assinatura")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("-dir é obrigatório")
	}
	pub, err := release.ParsePublicKey(*pubFlag)
	if err != nil {
		return err
	}
	m, err := release.VerifyDir(*dir, pub)
	if err != nil {
		return err
	}
	how := "SHA-256 conferido"
	if pub != nil {
		how += " + assinatura ed25519"
	}
	fmt.Fprintf(stdout, "ok: versão %s, %d binários (%s)\n", m.Version, len(m.Files), how)
	return nil
}
