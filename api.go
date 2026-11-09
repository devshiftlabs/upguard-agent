package main

// Comunicação com a API do UpGuard: todas as chamadas autenticam com Basic
// auth (client-id:client-secret) e têm timeout próprio.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	headerUserAgent   = "User-Agent"
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"
	requestTimeout    = 15 * time.Second
)

type ingestResponse struct {
	HostID string `json:"host_id"`
	Config struct {
		IntervalSeconds int  `json:"interval_seconds"`
		Paused          bool `json:"paused"`
		UpdateRequested bool `json:"update_requested"`
	} `json:"config"`
}

// serverConfig é a config server-driven devolvida a cada envio de métricas.
type serverConfig struct {
	hostID          string
	interval        int
	paused          bool
	updateRequested bool
}

// statusError descreve uma resposta HTTP não-2xx.
type statusError struct {
	who  string
	code int
}

func (e *statusError) Error() string { return fmt.Sprintf("%s respondeu %d", e.who, e.code) }

// apiCall faz uma requisição autenticada à API. body nil = sem corpo; com
// corpo, envia JSON. decodeInto (opcional) recebe o JSON da resposta; erro de
// decodificação só é devolvido se strictDecode.
func apiCall(cfg config, client *http.Client, method, path string, body []byte, decodeInto any, strictDecode bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.server+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set(headerContentType, contentTypeJSON)
	}
	req.Header.Set(headerUserAgent, userAgent)
	req.SetBasicAuth(cfg.clientID, cfg.clientSecret)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &statusError{who: "servidor", code: resp.StatusCode}
	}
	if decodeInto == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(decodeInto); err != nil && strictDecode {
		return err
	}
	return nil
}

// send envia a amostra e devolve a config desejada pelo servidor. Uma resposta
// 2xx com corpo inválido vale como sucesso com config vazia.
func send(cfg config, client *http.Client, p payload) (serverConfig, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return serverConfig{}, err
	}
	var out ingestResponse
	if err := apiCall(cfg, client, http.MethodPost, "/api/agent/metrics", body, &out, false); err != nil {
		return serverConfig{}, err
	}
	return serverConfig{out.HostID, out.Config.IntervalSeconds, out.Config.Paused, out.Config.UpdateRequested}, nil
}

// fetchProbes baixa a lista de probes do servidor (editável no portal).
func fetchProbes(cfg config, client *http.Client) ([]probe, bool) {
	var body struct {
		Probes []probe `json:"probes"`
	}
	if err := apiCall(cfg, client, http.MethodGet, "/api/agent/probes", nil, &body, true); err != nil {
		return nil, false
	}
	return body.Probes, true
}
