package main

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want config
	}{
		{
			name: "defaults",
			want: config{server: defaultServer, interval: 60 * time.Second},
		},
		{
			name: "env",
			env: map[string]string{
				"UPGUARD_CLIENT_ID": "agt_1", "UPGUARD_CLIENT_SECRET": "sk_1",
				"UPGUARD_SERVER_URL": "https://x.example", "UPGUARD_HOSTNAME": "srv1",
				"UPGUARD_INTERVAL": "120",
			},
			want: config{"agt_1", "sk_1", "https://x.example", "srv1", 120 * time.Second},
		},
		{
			name: "flags ganham do env",
			args: []string{"--client-id", "a", "--client-secret", "s", "--server", "https://f", "--hostname", "h", "--interval", "30"},
			env:  map[string]string{"UPGUARD_CLIENT_ID": "env", "UPGUARD_INTERVAL": "300"},
			want: config{"a", "s", "https://f", "h", 30 * time.Second},
		},
		{
			name: "intervalo abaixo do mínimo sobe para 10s",
			args: []string{"--interval", "3"},
			want: config{server: defaultServer, interval: 10 * time.Second},
		},
		{
			name: "env de intervalo abaixo do mínimo",
			env:  map[string]string{"UPGUARD_INTERVAL": "5"},
			want: config{server: defaultServer, interval: 10 * time.Second},
		},
		{
			name: "env de intervalo inválido mantém o default",
			env:  map[string]string{"UPGUARD_INTERVAL": "abc"},
			want: config{server: defaultServer, interval: 60 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, showVer, err := parseConfig(tt.args, envMap(tt.env), io.Discard)
			if err != nil || showVer {
				t.Fatalf("err=%v showVer=%v", err, showVer)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseConfigVersionAndErrors(t *testing.T) {
	if _, showVer, err := parseConfig([]string{"--version"}, envMap(nil), io.Discard); err != nil || !showVer {
		t.Fatalf("--version: showVer=%v err=%v", showVer, err)
	}
	if _, _, err := parseConfig([]string{"-h"}, envMap(nil), io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: want ErrHelp, got %v", err)
	}
	var out strings.Builder
	if _, _, err := parseConfig([]string{"--nope"}, envMap(nil), &out); err == nil {
		t.Fatal("flag desconhecida deveria falhar")
	}
	if !strings.Contains(out.String(), "nope") {
		t.Fatalf("erro deveria ser impresso no output, got %q", out.String())
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		cfg     config
		wantErr bool
	}{
		{config{clientID: "a", clientSecret: "b"}, false},
		{config{clientID: "a"}, true},
		{config{clientSecret: "b"}, true},
		{config{}, true},
	}
	for _, tt := range tests {
		err := tt.cfg.validate()
		if (err != nil) != tt.wantErr {
			t.Errorf("validate(%+v) = %v, wantErr %v", tt.cfg, err, tt.wantErr)
		}
		if err != nil && !errors.Is(err, errMissingCredentials) {
			t.Errorf("erro inesperado: %v", err)
		}
	}
}

func TestVersionString(t *testing.T) {
	s := versionString()
	if !strings.HasPrefix(s, "upguard-agent "+version+" (") || !strings.Contains(s, "/") {
		t.Fatalf("versionString() = %q", s)
	}
}
