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

// PanelConfig configura la conexión con un panel v2board CON el módulo GPM
// instalado (tabla/modelo/controlador propios "server/gpm/*", ver README) --
// no es v2board stock. El contrato de los endpoints "UniProxy" (user/push)
// es el mismo que usa v2node para cualquier otro protocolo.
//
// node_type: el módulo GPM del panel reconoce "gpm" (case-insensitive) y
// mantiene sus propias CacheKey de estado (online/last-check/last-push)
// separadas de v2node real -- no reusar node_type=v2node contra un nodo GPM,
// las estadísticas del admin quedarían mezcladas con nodos que no son este.
type PanelConfig struct {
	// APIHost es la URL base del panel, ej. "https://panel.tudominio.com".
	APIHost string
	// NodeID es el id numérico del nodo GPM en el panel (Nodos > GPM).
	NodeID int
	// Token es la API key del nodo.
	Token string
	// PullInterval: cada cuánto sincronizar la lista de usuarios. Default 60s.
	PullInterval time.Duration
	// PushInterval: cada cuánto reportar consumo acumulado. Default 60s.
	PushInterval time.Duration
	// NodeTypeOverride reemplaza el "GPM" default del query param node_type,
	// por si hace falta apuntar a un panel con otra convención.
	NodeTypeOverride string
	// Conns, si no es nil, se usa para CORTAR en caliente las conexiones de
	// cualquier uuid que desaparezca de un pull (cuota agotada, vigencia
	// vencida, usuario borrado) -- sin esto, quitar a alguien de la lista
	// del panel solo bloquea reconexiones, una sesión ya abierta se queda
	// viva indefinidamente. Mismo comportamiento que RemoveUser+
	// LinkManager.CloseAll() en v2node.
	Conns *ConnRegistry

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

	mu         sync.RWMutex
	users      map[string]panelUserEntry // uuid -> entry
	pulled     bool                      // false hasta el primer pullOnce exitoso
	emptyPulls int                       // pulls consecutivos con 0 usuarios sin aplicar todavía
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
		nodeType = "GPM"
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

// FetchNodePort lee /api/v2/server/config y devuelve el server_port que
// tiene configurado el nodo en el panel -- mismo endpoint que usa v2node
// para leer su propia config, aquí solo se usa el campo de puerto (el resto
// de la config de ese endpoint, protocolo/TLS/etc, no aplica a GPM). Pensado
// para pasarse como Options.PortProvider en server.Run.
func (s *PanelUserStore) FetchNodePort(ctx context.Context) (int, error) {
	var body struct {
		ServerPort int `json:"server_port"`
	}
	if err := s.getJSON(ctx, "/api/v2/server/config", &body); err != nil {
		return 0, err
	}
	return body.ServerPort, nil
}

// FetchNodeCdn lee /api/v2/server/config y devuelve 101 si el nodo está
// marcado "detrás de CDN" en el panel (campo behind_cdn, booleano JSON,
// default true del lado panel -- NO es "cdn", nombre distinto a propósito
// del lado panel para no confundirlo con nada que viaje en el link de
// suscripción) o 200 si no. Pensado para pasarse como
// Options.DecoyStatusProvider en server.Run.
func (s *PanelUserStore) FetchNodeCdn(ctx context.Context) (int, error) {
	var body struct {
		BehindCDN bool `json:"behind_cdn"`
	}
	if err := s.getJSON(ctx, "/api/v2/server/config", &body); err != nil {
		return 0, err
	}
	if body.BehindCDN {
		return 101, nil
	}
	return 200, nil
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
	// Una respuesta VÁLIDA (200) con 0 usuarios es indistinguible, a este
	// nivel, de "se le cambió el grupo al nodo por error" o "el grupo
	// quedó vacío" -- no necesariamente significa que de verdad ya nadie
	// tiene acceso. Actuar sobre esto de inmediato (reemplazar la lista y
	// cortar a TODOS los conectados) es una acción de radio muy amplio
	// sobre una señal ambigua. Se exige confirmación en un segundo pull
	// consecutivo antes de aplicarlo -- si el pull siguiente ya no viene
	// vacío, se descarta como ruido/transitorio, sin haber tocado a nadie.
	if len(users) == 0 && len(s.users) > 0 {
		s.emptyPulls++
		if s.emptyPulls < 2 {
			previousCount := len(s.users)
			attempt := s.emptyPulls
			s.mu.Unlock()
			log.Printf("panel: ADVERTENCIA -- el pull devolvió 0 usuarios (antes había %d). Puede ser un grupo vacío/mal configurado en el panel o una revocación real -- esperando confirmación en el próximo pull antes de tocar a nadie (%d/2)", previousCount, attempt)
			return nil
		}
		log.Println("panel: 0 usuarios confirmado en 2 pulls consecutivos -- aplicando (puede cortar TODAS las conexiones activas del nodo)")
	} else {
		s.emptyPulls = 0
	}

	previous := s.users
	s.users = users
	firstPull := !s.pulled
	s.pulled = true
	s.mu.Unlock()

	log.Printf("panel: %d usuarios sincronizados", len(users))

	// Cortar en caliente a cualquiera que haya desaparecido (cuota
	// agotada, vigencia vencida, usuario borrado) -- sin esto, solo se
	// bloquean RECONEXIONES, una sesión ya abierta se queda viva. Se
	// salta en el primer pull (no hay "antes" real con qué comparar, y
	// nadie tuvo tiempo de conectarse todavía).
	if s.cfg.Conns != nil && !firstPull {
		for uuid := range previous {
			if _, stillThere := users[uuid]; !stillThere {
				if n := s.cfg.Conns.Kick(uuid); n > 0 {
					log.Printf("panel: %s ya no está autorizado -- %d conexión(es) cortada(s)", uuid, n)
				}
			}
		}
	}

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
