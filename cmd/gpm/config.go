package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// fileConfig es el formato de -config: JSON legible a mano, mismo espíritu
// que /etc/v2node/config.json -- campos con nombre en vez de una sola
// cadena de flags. gpm serve -config archivo.json es la forma recomendada
// de correr GPM en producción; los flags individuales (-addr, -panel-url,
// etc.) siguen existiendo para pruebas rápidas / scripting, pero si se da
// -config se usa ESE como única fuente de verdad (no se mezclan).
type fileConfig struct {
	Addr        string `json:"addr"`
	HostKeyPath string `json:"hostkey"`
	UdpgwAddr   string `json:"udpgwAddr,omitempty"`
	// Cdn indica si el nodo está detrás de un CDN tipo Cloudflare -- default
	// true (nil se trata como true). Decide qué status responde el señuelo:
	// 101 si true (lo que el CDN necesita para pasar a modo túnel crudo), o
	// 200 si false (conexión directa, sin nada esperando un upgrade a
	// WebSocket) -- ver Options.DecoyStatus en internal/server.
	Cdn *bool `json:"cdn,omitempty"`

	// Tls activa la capa "stunnel embebido": envuelve cada conexión en TLS
	// 1.3 con SNI (cert self-signed por SNI al vuelo, el cliente conecta con
	// allowInsecure) -- reemplaza al señuelo. Default false (nil = false). En
	// modo panel se puede sincronizar en caliente contra el campo "tls" del
	// nodo (ver Panel.TlsSync); este valor es solo el arranque en ese caso.
	Tls *bool `json:"tls,omitempty"`
	// TlsCAPath es el archivo PEM donde persistir la CA self-signed que firma
	// los leaf por SNI (0600, recomendado /etc/gpm/<nodeId>/tls_ca.pem).
	// Vacío = CA efímera (nueva en cada arranque; da igual, el cliente no
	// valida la cadena).
	TlsCAPath string `json:"tlsCa,omitempty"`

	// Modo manual (mutuamente excluyente con Panel).
	UsersPath string `json:"users,omitempty"`

	// Modo panel (mutuamente excluyente con UsersPath).
	Panel *filePanelConfig `json:"panel,omitempty"`
}

type filePanelConfig struct {
	URL    string `json:"url"`
	NodeID int    `json:"nodeId"`
	// TokenFile apunta al archivo con el Communication Key (0600,
	// recomendado /etc/gpm/<nodeId>/panel-token) -- el token en sí NUNCA
	// va en este archivo, a propósito, para que se pueda compartir/mostrar
	// esta config sin exponer el secreto maestro del panel. Administrar el
	// token con "gpm-cli token <nodeId>".
	TokenFile         string `json:"tokenFile"`
	PullInterval      string `json:"pullInterval,omitempty"`      // ej. "60s"
	PushInterval      string `json:"pushInterval,omitempty"`      // ej. "60s"
	PortSync          *bool  `json:"portSync,omitempty"`          // default true
	PortCheckInterval string `json:"portCheckInterval,omitempty"` // ej. "60s"
	// CdnSync (default true) sincroniza en caliente si el nodo responde 101
	// o 200 al señuelo contra el campo "behind_cdn" del panel
	// (/api/v2/server/config) -- si es false, se usa el "cdn" fijo de más
	// arriba en este archivo y nunca se vuelve a consultar. Reusa
	// PortCheckInterval (mismo endpoint).
	CdnSync *bool `json:"cdnSync,omitempty"`
	// TlsSync (default true) sincroniza en caliente si el nodo termina TLS o
	// no contra el campo "tls" del panel (/api/v2/server/config) -- si es
	// false, se usa el "tls" fijo de más arriba en este archivo y nunca se
	// vuelve a consultar. Reusa PortCheckInterval (mismo endpoint).
	TlsSync *bool `json:"tlsSync,omitempty"`
}

func loadFileConfig(path string) (*fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("leer %s: %w", path, err)
	}
	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsear %s: %w", path, err)
	}
	return &cfg, nil
}

// persistAddr reescribe SOLO el campo "addr" del archivo de config,
// preservando el resto tal cual -- usado para que un cambio de puerto
// detectado en caliente (ver Options.PortProvider) sobreviva un reinicio
// del proceso: sin esto, cada restart vuelve a arrancar en el puerto viejo
// guardado hasta el primer chequeo contra el panel.
func persistAddr(path, newAddr string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	encoded, err := json.Marshal(newAddr)
	if err != nil {
		return err
	}
	raw["addr"] = encoded

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
