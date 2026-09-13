package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// PanelConfig configura la conexión con el panel v2board. El contrato
// (endpoints, query params, formato del body) está tomado directo del
// cliente real de v2node (api/v2board/*.go en github.com/wyx2685/v2node) --
// v2board expone los mismos endpoints "UniProxy" sin importar el protocolo
// del nodo, así que GPM los reutiliza tal cual aunque su protocolo real
// (SSH + señuelo) no sea uno de los que el panel reconoce nativamente.
//
// node_type: v2board no parece validar este valor contra nada del lado del
// nodo -- es solo el nombre del agente que llama. Se manda "v2node" a
// propósito (no "gpm") para máxima compatibilidad con paneles que sí lo
// validen contra una lista conocida; si en el futuro se confirma que da
// igual, cambiar es trivial (NodeTypeOverride).
type PanelConfig struct {
	// APIHost es la URL base del panel, ej. "https://panel.tudominio.com".
	APIHost string
	// NodeID es el id numérico del nodo en el panel (Server Management).
	// Como v2board no soporta "ssh" como protocolo, este nodo debe darse de
	// alta como algún tipo soportado (trojan recomendado, ver README) --
	// GPM solo usa este id para las llamadas de usuarios/reporte, ignora
	// por completo la config de protocolo/TLS que devolvería
	// /api/v2/server/config.
	NodeID int
	// Token es la API key del nodo (columna "ApiKey"/"Key" en v2board).
	Token string
	// PullInterval: cada cuánto sincronizar la lista de usuarios. Default 60s.
	PullInterval time.Duration
	// PushInterval: cada cuánto reportar consumo acumulado. Default 60s.
	PushInterval time.Duration
	// NodeTypeOverride reemplaza el "v2node" default del query param
	// node_type, por si algún panel lo valida distinto.
	NodeTypeOverride string

	HTTPClient *http.Client
}

type panelUserEntry struct {
	ID          int
	SpeedLimit  int
	DeviceLimit int
}

// PanelUserStore implementa UserStore sincronizando contra un panel v2board
// -- el modo "backend" de GPM, equivalente a lo que hace v2node para VLESS.
type PanelUserStore struct {
	cfg      PanelConfig
	http     *http.Client
	nodeType string

	mu    sync.RWMutex
	users map[string]panelUserEntry // uuid -> entry
}

// NewPanelUserStore hace una sincronización inicial BLOQUEANTE (para que el
// servidor no arranque aceptando conexiones con la lista vacía) y arranca
// la sincronización periódica en background.
func NewPanelUserStore(ctx context.Context, cfg PanelConfig) (*PanelUserStore, error) {
	if cfg.APIHost == "" || cfg.Token == "" || cfg.NodeID == 0 {
		return nil, fmt.Errorf("PanelConfig incompleto: APIHost/NodeID/Token son requeridos")
	}
	if cfg.PullInterval <= 0 {
		cfg.PullInterval = 60 * time.Second
	}
	if cfg.PushInterval <= 0 {
		cfg.PushInterval = 60 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	nodeType := cfg.NodeTypeOverride
	if nodeType == "" {
		nodeType = "v2node"
	}

	s := &PanelUserStore{
		cfg:      cfg,
		http:     cfg.HTTPClient,
		nodeType: nodeType,
		users:    map[string]panelUserEntry{},
	}

	if err := s.pullOnce(ctx); err != nil {
		return nil, fmt.Errorf("sincronización inicial de usuarios con el panel: %w", err)
	}

	go s.pullLoop(ctx)

	return s, nil
}

// Lookup implementa UserStore. El "nombre" que devolvemos es solo para
// logs (el panel no manda un nombre/email en este endpoint, solo id/uuid).
func (s *PanelUserStore) Lookup(uuid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.users[uuid]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("panel-uid-%d", e.ID), true
}

func (s *PanelUserStore) pullLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PullInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.pullOnce(ctx); err != nil {
				log.Println("panel: error sincronizando usuarios (se conserva la lista anterior):", err)
			}
		}
	}
}

func (s *PanelUserStore) pullOnce(ctx context.Context) error {
	var body struct {
		Users []struct {
			ID          int    `json:"id"`
			UUID        string `json:"uuid"`
			SpeedLimit  int    `json:"speed_limit"`
			DeviceLimit int    `json:"device_limit"`
		} `json:"users"`
	}
	if err := s.getJSON(ctx, "/api/v1/server/UniProxy/user", &body); err != nil {
		return err
	}

	users := make(map[string]panelUserEntry, len(body.Users))
	for _, u := range body.Users {
		if u.UUID == "" {
			continue
		}
		users[u.UUID] = panelUserEntry{ID: u.ID, SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit}
	}

	s.mu.Lock()
	s.users = users
	s.mu.Unlock()

	log.Printf("panel: %d usuarios sincronizados", len(users))
	return nil
}

// RunUsageReporter drena `usage` cada PushInterval y lo reporta al panel.
// Bloquea -- correr en su propia goroutine. Los deltas de uuids que ya no
// están en la lista de usuarios actual (ej. se eliminaron en el panel entre
// sincronizaciones) se descartan silenciosamente.
func (s *PanelUserStore) RunUsageReporter(ctx context.Context, usage *Usage) {
	ticker := time.NewTicker(s.cfg.PushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pushUsage(ctx, usage)
		}
	}
}

func (s *PanelUserStore) pushUsage(ctx context.Context, usage *Usage) {
	deltas := usage.Snapshot()
	if len(deltas) == 0 {
		return
	}

	s.mu.RLock()
	payload := make(map[int][2]int64, len(deltas))
	for uuid, d := range deltas {
		if d.Up == 0 && d.Down == 0 {
			continue
		}
		if e, ok := s.users[uuid]; ok {
			payload[e.ID] = [2]int64{d.Up, d.Down}
		}
	}
	s.mu.RUnlock()

	if len(payload) == 0 {
		return
	}
	if err := s.postJSON(ctx, "/api/v1/server/UniProxy/push", payload); err != nil {
		log.Println("panel: error reportando consumo:", err)
		return
	}
	log.Printf("panel: consumo reportado para %d usuarios", len(payload))
}

func (s *PanelUserStore) baseQuery() url.Values {
	q := url.Values{}
	q.Set("node_type", s.nodeType)
	q.Set("node_id", fmt.Sprint(s.cfg.NodeID))
	q.Set("token", s.cfg.Token)
	return q
}

func (s *PanelUserStore) getJSON(ctx context.Context, path string, out any) error {
	u := s.cfg.APIHost + path + "?" + s.baseQuery().Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *PanelUserStore) postJSON(ctx context.Context, path string, in any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	u := s.cfg.APIHost + path + "?" + s.baseQuery().Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return nil
}
