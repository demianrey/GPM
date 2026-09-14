// Package server implementa GPM (Go Payload Multiplexer): un servidor que
// habla el protocolo SSH real (golang.org/x/crypto/ssh) por delante de un
// señuelo HTTP-like opcional, pensado para el mismo patrón de bypass de
// zero-rating que usan apps tipo HTTP Injector -- pero autenticando por el
// UUID que ya usan los clientes VLESS (auth "none" de SSH, sin password) en
// vez de un login compartido, para poder medir consumo por usuario.
//
// Ver README.md para el detalle del protocolo de señuelo y por qué la
// detección de señuelo funciona como funciona (readDecoyUntilIdle).
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// Options configura una instancia de GPM.
type Options struct {
	// Addr es donde escuchar, ej. ":80" o "0.0.0.0:8022".
	Addr string
	// HostKeyPath es el archivo PEM donde persistir la host key RSA. Si no
	// existe, se genera una nueva y se guarda ahí. Vacío = efímera (nueva
	// en cada arranque -- solo para pruebas, cambia el fingerprint cada vez).
	HostKeyPath string
	// Users resuelve qué UUIDs pueden autenticarse. Ver users.go
	// (FileUserStore, modo manual) y panel.go (PanelUserStore, modo panel).
	Users UserStore
	// Usage acumula subida/bajada por uuid. Si es nil, Run crea uno interno
	// (suficiente para el modo manual, que hoy solo lo usa para loguear).
	// El modo panel necesita pasar el suyo aquí para poder drenarlo
	// (Snapshot) y reportarlo al panel -- ver panel.go RunUsageReporter.
	Usage *Usage
	// UdpgwAddr es la dirección "virtual" (host:puerto) que, al pedirse
	// como destino de un canal direct-tcpip, GPM atiende él mismo con el
	// protocolo udpgw embebido (ver udpgw.go) en vez de intentar conectarse
	// de verdad a esa dirección. Debe coincidir con el udpgwAddress
	// configurado en el perfil del cliente. Vacío = deshabilitado (un
	// cliente pidiendo UDP recibiría un intento de conexión real fallido a
	// esa dirección, como cualquier otro destino inexistente).
	UdpgwAddr string
	// PortProvider, si no es nil, se consulta periódicamente (cada
	// PortCheckInterval) para saber en qué puerto GPM DEBERÍA estar
	// escuchando -- ej. leyendo /api/v2/server/config del panel, igual que
	// hace v2node. Si devuelve un puerto distinto al actual, Run() cierra
	// el listener viejo y abre uno nuevo ahí, sin necesidad de reiniciar el
	// proceso (systemd/el operador no tiene que intervenir cuando cambian
	// el puerto desde el admin). El host (interfaz) de Options.Addr se
	// mantiene fijo -- solo el puerto es dinámico.
	PortProvider func(ctx context.Context) (int, error)
	// PortCheckInterval: cada cuánto consultar PortProvider. Default 60s.
	PortCheckInterval time.Duration
	// OnPortChange, si no es nil, se llama con la nueva dirección completa
	// (ej. ":81") justo después de un cambio de puerto exitoso -- pensado
	// para persistir el puerto nuevo en el archivo de config de quien
	// llama, así un reinicio del proceso arranca directo en el puerto
	// correcto en vez de depender del primer chequeo al panel.
	OnPortChange func(addr string)
	// Conns rastrea las conexiones SSH activas por uuid, para poder
	// cortarlas en caliente cuando un usuario deja de estar autorizado
	// (cuota agotada, vigencia vencida) -- ver ConnRegistry.Kick, usado por
	// PanelUserStore en modo panel. Si es nil, Run crea uno internamente
	// (el registro en sí no hace nada solo -- alguien tiene que llamar
	// Kick, que es lo que hace PanelUserStore al perder de vista un uuid
	// en un pull).
	Conns *ConnRegistry
	// DecoyStatus es el status HTTP inicial que se responde al señuelo: 101
	// (default, 0 se trata como 101) para nodos detrás de un CDN tipo
	// Cloudflare, que solo pasa a modo túnel crudo si ve ese status -- o 200
	// para conexión directa sin CDN, donde no hay nada esperando un upgrade
	// a WebSocket y un 200 llano es el camuflaje más discreto (mismo
	// criterio que usan proxy.py/open.py de SSHPlus, que traen uno u otro
	// según el modo). Si DecoyStatusProvider no es nil, este valor es solo
	// el arranque -- se pisa con lo que devuelva el provider en el primer
	// chequeo.
	DecoyStatus int
	// DecoyStatusProvider, si no es nil, se consulta cada
	// DecoyStatusCheckInterval para saber si el nodo debe responder 101 o
	// 200 -- ej. leyendo el campo "behind_cdn" de /api/v2/server/config del
	// panel (ver PanelUserStore.FetchNodeCdn), igual patrón que
	// PortProvider. A diferencia del puerto, un cambio acá no reinicia el
	// listener: aplica de inmediato en la próxima conexión nueva.
	DecoyStatusProvider func(ctx context.Context) (int, error)
	// DecoyStatusCheckInterval: cada cuánto consultar DecoyStatusProvider.
	// Default 60s.
	DecoyStatusCheckInterval time.Duration
	// TLSEnabled activa la capa "stunnel embebido": cada conexión aceptada
	// se envuelve en TLS 1.3 con SNI ANTES de la detección de señuelo/SSH
	// (ver handleConn), de modo que en el cable el tráfico parece HTTPS real
	// hacia el dominio del SNI. Reemplaza al señuelo HTTP-like (en modo TLS
	// el cliente no manda señuelo: abre TLS y adentro va el banner SSH
	// directo, que la detección de abajo reconoce sola). Cert self-signed
	// por SNI generado al vuelo (ver tls.go); el cliente conecta con
	// allowInsecure -- esta capa es camuflaje, no seguridad.
	TLSEnabled bool
	// TLSCAPath es el archivo PEM donde persistir la CA self-signed que
	// firma los leaf por SNI. Vacío = CA efímera (nueva en cada arranque;
	// da igual porque el cliente no valida la cadena). Análogo a
	// HostKeyPath.
	TLSCAPath string
	// TLSProvider, si no es nil, se consulta cada TLSCheckInterval para
	// saber si el nodo debe estar en modo TLS -- ej. leyendo el campo "tls"
	// de /api/v2/server/config del panel (ver PanelUserStore.FetchNodeTls),
	// mismo patrón atómico que DecoyStatusProvider. Un cambio acá NO reinicia
	// el listener: aplica en la próxima conexión nueva. Nota: si se pasa un
	// provider, la capa TLS se inicializa (se genera/lee la CA) aunque el
	// valor inicial sea false, para poder encenderla en caliente sin
	// reiniciar.
	TLSProvider func(ctx context.Context) (bool, error)
	// TLSCheckInterval: cada cuánto consultar TLSProvider. Default 60s.
	TLSCheckInterval time.Duration
}

// ConnRegistry rastrea qué conexiones SSH activas corresponden a cada uuid,
// para poder cerrarlas en caliente cuando el usuario deja de estar
// autorizado. Sin esto, quitar a un usuario de la lista del panel (cuota
// agotada, vigencia vencida) solo bloquea RECONEXIONES -- una sesión ya
// abierta se queda viva indefinidamente mientras el cliente no la cierre
// él mismo (y no tiene ningún incentivo para hacerlo).
type ConnRegistry struct {
	mu    sync.Mutex
	conns map[string]map[*ssh.ServerConn]struct{}
}

func NewConnRegistry() *ConnRegistry {
	return &ConnRegistry{conns: map[string]map[*ssh.ServerConn]struct{}{}}
}

func (r *ConnRegistry) add(uuid string, conn *ssh.ServerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[uuid] == nil {
		r.conns[uuid] = map[*ssh.ServerConn]struct{}{}
	}
	r.conns[uuid][conn] = struct{}{}
}

func (r *ConnRegistry) remove(uuid string, conn *ssh.ServerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.conns[uuid]; ok {
		delete(m, conn)
		if len(m) == 0 {
			delete(r.conns, uuid)
		}
	}
}

// Kick cierra TODAS las conexiones SSH activas de un uuid (si tiene
// alguna) y devuelve cuántas cerró. Cada conexión cerrada dispara su
// propio cleanup normal en handleConn (cierre de canales, etc), como
// cualquier desconexión.
func (r *ConnRegistry) Kick(uuid string) int {
	r.mu.Lock()
	conns := r.conns[uuid]
	delete(r.conns, uuid)
	r.mu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
	return len(conns)
}

// KickAll cierra TODAS las conexiones activas, de cualquier uuid, y
// devuelve cuántas cerró. Usado cuando el puerto cambia (ver Run): mismo
// comportamiento que v2node/VLESS -- un cambio de puerto es una ruptura
// esperada, todo cliente conectado se cae y reconecta con su config
// actualizada, en vez de dejar sesiones viejas colgando indefinidamente en
// un puerto que el admin ya no considera el oficial.
func (r *ConnRegistry) KickAll() int {
	r.mu.Lock()
	var all []*ssh.ServerConn
	for _, m := range r.conns {
		for conn := range m {
			all = append(all, conn)
		}
	}
	r.conns = map[string]map[*ssh.ServerConn]struct{}{}
	r.mu.Unlock()
	for _, conn := range all {
		_ = conn.Close()
	}
	return len(all)
}

// Usage acumula bytes de subida/bajada por uuid desde la última vez que se
// drenó con Snapshot. Exportado para que el modo panel (panel.go) pueda
// compartir la misma instancia entre las conexiones activas (que llaman
// Add) y el reportador periódico (que llama Snapshot).
type Usage struct {
	mu   sync.Mutex
	up   map[string]int64
	down map[string]int64
}

func NewUsage() *Usage {
	return &Usage{up: map[string]int64{}, down: map[string]int64{}}
}

func (u *Usage) Add(uuid string, up, down int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.up[uuid] += up
	u.down[uuid] += down
}

// UsageDelta es cuánto subió/bajó un uuid desde el último Snapshot.
type UsageDelta struct {
	Up   int64
	Down int64
}

// Snapshot devuelve los deltas acumulados y los resetea a cero -- para que
// cada reporte al panel mande solo lo NUEVO desde el reporte anterior, no
// el acumulado histórico completo.
func (u *Usage) Snapshot() map[string]UsageDelta {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]UsageDelta, len(u.up))
	for uuid, up := range u.up {
		out[uuid] = UsageDelta{Up: up, Down: u.down[uuid]}
	}
	u.up = map[string]int64{}
	u.down = map[string]int64{}
	return out
}

// Run arranca GPM y bloquea sirviendo conexiones hasta que listener.Accept
// falle de forma permanente (ej. el listener se cierra).
func Run(opts Options) error {
	if opts.Users == nil {
		return fmt.Errorf("Options.Users es requerido")
	}

	hostKey, err := loadOrCreateHostKey(opts.HostKeyPath)
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}

	usage := opts.Usage
	if usage == nil {
		usage = NewUsage()
	}

	conns := opts.Conns
	if conns == nil {
		conns = NewConnRegistry()
	}

	config := &ssh.ServerConfig{
		// Auth "none" real de SSH (RFC 4252): el cliente no manda password
		// ni llave, el servidor autentica solo con el username -- el UUID
		// ya es el secreto, igual que en VLESS.
		NoClientAuth: true,
		NoClientAuthCallback: func(conn ssh.ConnMetadata) (*ssh.Permissions, error) {
			uuid := conn.User()
			name, ok := opts.Users.Lookup(uuid)
			if !ok {
				return nil, fmt.Errorf("uuid desconocido: %s", uuid)
			}
			return &ssh.Permissions{
				Extensions: map[string]string{"uuid": uuid, "name": name},
			}, nil
		},
	}
	config.AddHostKey(hostKey)

	host, port, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return fmt.Errorf("Options.Addr inválido (%q): %w", opts.Addr, err)
	}

	// decoyStatus es atómico porque, a diferencia del puerto, un cambio acá
	// NO reinicia el listener -- cada conexión nueva simplemente lee el
	// valor vigente al armar su respuesta al señuelo (ver handleConn), así
	// que el cambio aplica de inmediato sin cortar nada.
	decoyStatus := &atomic.Int32{}
	initDecoy := opts.DecoyStatus
	if initDecoy == 0 {
		initDecoy = 101
	}
	decoyStatus.Store(int32(initDecoy))

	// runCtx vive lo que vive Run -- lo comparten los watchers de estado que
	// se aplican EN CALIENTE sin reiniciar el listener (modo señuelo y modo
	// TLS). El watcher de puerto NO usa este ctx: tiene su propio ciclo por
	// listener (ver más abajo), porque un cambio de puerto SÍ reinicia.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	if opts.DecoyStatusProvider != nil {
		interval := opts.DecoyStatusCheckInterval
		if interval <= 0 {
			interval = 60 * time.Second
		}
		go watchDecoyStatus(runCtx, opts.DecoyStatusProvider, decoyStatus, interval)
	}

	// Capa TLS ("stunnel embebido"): si está activa, cada conexión aceptada
	// se envuelve en TLS antes de la detección de señuelo/SSH (ver
	// handleConn). tlsOn es atómico por el mismo motivo que decoyStatus: un
	// cambio (sincronizado desde el panel vía TLSProvider) aplica en la
	// próxima conexión nueva, sin reiniciar el listener ni cortar sesiones.
	tlsOn := &atomic.Bool{}
	tlsOn.Store(opts.TLSEnabled)
	var tlsCfg *tls.Config
	if opts.TLSEnabled || opts.TLSProvider != nil {
		tm, terr := newTLSManager(opts.TLSCAPath)
		if terr != nil {
			return fmt.Errorf("tls: %w", terr)
		}
		tlsCfg = tm.serverConfig()
		if opts.TLSProvider != nil {
			interval := opts.TLSCheckInterval
			if interval <= 0 {
				interval = 60 * time.Second
			}
			go watchTLS(runCtx, opts.TLSProvider, tlsOn, interval)
		}
	}

	for {
		addr := net.JoinHostPort(host, port)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen en %s: %w", addr, err)
		}
		log.Println("GPM escuchando en", addr, "(protocolo SSH, auth por UUID)")

		acceptErrCh := make(chan error, 1)
		go func() {
			for {
				rawConn, err := listener.Accept()
				if err != nil {
					acceptErrCh <- err
					return
				}
				go handleConn(rawConn, config, usage, conns, opts.UdpgwAddr, int(decoyStatus.Load()), tlsOn.Load(), tlsCfg)
			}
		}()

		var restartCh <-chan string
		var stopWatch func()
		if opts.PortProvider != nil {
			ch := make(chan string, 1)
			ctx, cancel := context.WithCancel(context.Background())
			go watchPort(ctx, opts.PortProvider, port, opts.PortCheckInterval, ch)
			restartCh = ch
			stopWatch = cancel
		}

		select {
		case newPort := <-restartCh:
			if stopWatch != nil {
				stopWatch()
			}
			_ = listener.Close()
			if n := conns.KickAll(); n > 0 {
				log.Println("GPM: cambio de puerto -- cortando", n, "conexión(es) activa(s)")
			}
			log.Println("GPM: el panel cambió el puerto a", newPort, "-- reiniciando el listener")
			port = newPort
			if opts.OnPortChange != nil {
				opts.OnPortChange(net.JoinHostPort(host, port))
			}
			continue
		case err := <-acceptErrCh:
			if stopWatch != nil {
				stopWatch()
			}
			return fmt.Errorf("accept: %w", err)
		}
	}
}

// watchPort consulta provider cada interval (default 60s) y manda por ch el
// puerto nuevo la PRIMERA vez que difiere del actual, luego se detiene (Run
// vuelve a lanzar un watchPort fresco al reiniciar el listener). Errores de
// PortProvider se loguean y se ignoran -- un panel caído momentáneamente no
// debe tirar el servidor.
// watchDecoyStatus sincroniza en caliente si el nodo responde 101 (CDN) o
// 200 (directo) contra provider (ver PanelUserStore.FetchNodeCdn) -- a
// diferencia de watchPort, nunca reinicia nada: solo actualiza el valor
// atómico que cada conexión nueva lee en handleConn.
func watchDecoyStatus(ctx context.Context, provider func(context.Context) (int, error), current *atomic.Int32, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := provider(ctx)
			if err != nil {
				log.Println("GPM: error consultando modo CDN del panel:", err)
				continue
			}
			if status != 101 && status != 200 {
				continue
			}
			if int32(status) != current.Load() {
				log.Println("GPM: el panel cambió el modo del señuelo a", status)
				current.Store(int32(status))
			}
		}
	}
}

// watchTLS sincroniza en caliente si el nodo termina TLS o no contra
// provider (ver PanelUserStore.FetchNodeTls) -- igual que watchDecoyStatus,
// nunca reinicia nada: solo actualiza el bool atómico que el accept loop lee
// para decidir si envolver cada conexión nueva en TLS. Las sesiones ya
// abiertas no se tocan; el cambio aplica desde la próxima conexión.
func watchTLS(ctx context.Context, provider func(context.Context) (bool, error), current *atomic.Bool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			on, err := provider(ctx)
			if err != nil {
				log.Println("GPM: error consultando modo TLS del panel:", err)
				continue
			}
			if on != current.Load() {
				log.Println("GPM: el panel cambió el modo TLS a", on)
				current.Store(on)
			}
		}
	}
}

func watchPort(ctx context.Context, provider func(context.Context) (int, error), current string, interval time.Duration, ch chan<- string) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			port, err := provider(ctx)
			if err != nil {
				log.Println("GPM: error consultando puerto del panel:", err)
				continue
			}
			if port <= 0 {
				continue
			}
			newPort := strconv.Itoa(port)
			if newPort != current {
				select {
				case ch <- newPort:
				default:
				}
				return
			}
		}
	}
}

// bufConn envuelve la conexión cruda con el bufio.Reader que usamos para
// detectar/consumir el señuelo -- todas las lecturas posteriores (incluido
// el handshake SSH real) tienen que pasar por el MISMO reader, si no,
// cualquier byte que el sistema operativo ya haya entregado al buffer
// interno de bufio se perdería.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

// wsMagicGUID es el GUID fijo del protocolo WebSocket (RFC 6455 sección
// 1.3) para calcular Sec-WebSocket-Accept a partir de Sec-WebSocket-Key.
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// decoyIdleGap es cuánto esperamos sin recibir bytes nuevos del cliente
// para decidir que terminó de mandar el señuelo (una o varias etapas
// [split]) y toca responder. Tiene que ser mayor al delay entre etapas que
// use el cliente (20ms en el injector de exclave-core) para no cortar a
// media etapa, pero corto para no meter latencia perceptible.
//
// Por qué respondemos por INACTIVIDAD y no por ver un marcador tipo "SSH-"
// en lo que manda el cliente: un cliente real (el nuestro, y el de apps
// tipo HTTP Injector) manda el señuelo y se QUEDA ESPERANDO la respuesta
// antes de mandar su propio banner SSH. Si el servidor también espera ver
// "SSH-" viniendo del cliente antes de responder, ninguno de los dos habla
// primero y la conexión se cuelga. La respuesta correcta es la misma que
// usan proxies reales de este tipo (ej. SSHPlus): contestar en cuanto el
// cliente se queda callado.
const decoyIdleGap = 80 * time.Millisecond

// decoyPeekTimeout es cuánto esperamos ver el primer byte del todo antes de
// asumir que no viene ningún señuelo. Dos casos legítimos dan CERO bytes en
// este margen: (a) un cliente SSH real sin señuelo, que por protocolo
// espera leer NUESTRO banner antes de mandar el suyo, y (b) llegar detrás
// de un relay externo que ya consumió/descartó el señuelo del cliente antes
// de reenviar la conexión. En ambos casos hablamos nosotros primero.
const decoyPeekTimeout = 400 * time.Millisecond

// tlsHandshakeTimeout acota el handshake TLS de entrada (modo "stunnel
// embebido") para que un cliente que abre la conexión y no completa el
// handshake no deje la goroutine colgada. Generoso a propósito -- un
// handshake TLS puede necesitar un par de round-trips sobre un enlace lento.
const tlsHandshakeTimeout = 10 * time.Second

func handleConn(rawConn net.Conn, config *ssh.ServerConfig, usage *Usage, conns *ConnRegistry, udpgwAddr string, decoyStatus int, tlsOn bool, tlsCfg *tls.Config) {
	defer rawConn.Close()

	// Capa TLS más externa ("stunnel embebido", ver Options.TLSEnabled): si
	// el nodo está en modo TLS, el handshake se hace acá y todo lo de abajo
	// (detección de señuelo, handshake SSH, udpgw, medición por uuid) corre
	// sobre la conexión ya descifrada, sin enterarse. En modo TLS el cliente
	// NO manda señuelo: abre TLS y adentro va el banner SSH directo, que la
	// detección de más abajo reconoce sola (Peek ve "SSH-").
	netConn := rawConn
	if tlsOn && tlsCfg != nil {
		tconn := tls.Server(rawConn, tlsCfg)
		_ = tconn.SetReadDeadline(time.Now().Add(tlsHandshakeTimeout))
		if err := tconn.Handshake(); err != nil {
			log.Println("tls: handshake fallido:", err)
			return
		}
		_ = tconn.SetReadDeadline(time.Time{})
		netConn = tconn
	}

	br := bufio.NewReader(netConn)

	_ = netConn.SetReadDeadline(time.Now().Add(decoyPeekTimeout))
	prefix, peekErr := br.Peek(4)

	if peekErr == nil && !bytes.Equal(prefix, []byte("SSH-")) {
		// Llegó algo rápido y no es un banner SSH -- se asume señuelo
		// HTTP-like (mismo patrón que el injector de ws/xhttp del cliente
		// en exclave-core: tokens [crlf]/[cr]/[lf]/[split]).
		wsKey, derr := readDecoyUntilIdle(netConn, br)
		if derr != nil {
			log.Println("señuelo: error leyendo headers:", derr)
			return
		}
		// 101 (default) para nodos detrás de un CDN tipo Cloudflare, que solo
		// pasa a modo túnel crudo (reenvía lo que sigue tal cual, sin
		// re-interpretarlo como otra petición HTTP) si ve status 101 en la
		// respuesta del origen -- da igual si el señuelo trae un upgrade de
		// WebSocket genuino o solo el texto de adorno (como en payloads
		// reales tipo HTTP Injector, que nunca completan un handshake WS de
		// verdad). Con 200 el CDN da la petición por terminada y no entrega
		// nada más.
		//
		// 200 para conexión directa sin CDN de por medio (Options.DecoyStatus
		// == 200): ahí no hay nada esperando un upgrade a WebSocket. Dos
		// respuestas HTTP apiladas -- no una sola -- porque así responde un
		// injector real de dos saltos (señuelo público + túnel CONNECT al
		// backend SSH), confirmado comparando bytes contra un server
		// AlberVPN real: "200 <marca>" (acepta el señuelo) seguido de
		// "200 Connection Established" (el clásico de un proxy HTTP
		// CONNECT). Un "200 OK" solo es más genérico y más fácil de
		// distinguir para el clasificador de la operadora que el patrón que
		// ya sabemos que funciona.
		var resp string
		switch {
		case decoyStatus == 200:
			resp = "HTTP/1.1 200 @DemianRed\r\nContent-length: 0\r\n\r\nHTTP/1.1 200 Connection Established\r\n\r\n"
		case wsKey != "":
			resp = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAcceptFor(wsKey) + "\r\n\r\n"
		default:
			resp = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
		}
		if _, err := netConn.Write([]byte(resp)); err != nil {
			log.Println("señuelo: error escribiendo respuesta:", err)
			return
		}
	}
	// Si peekErr fue timeout (nada llegó a tiempo) o si ya vimos "SSH-" de
	// entrada, no hacemos nada más aquí: seguimos directo al handshake
	// real, y ssh.NewServerConn manda NUESTRO banner primero -- lo cual
	// además es lo que rompe cualquier estancamiento con el otro lado.
	_ = netConn.SetReadDeadline(time.Time{})

	conn := &bufConn{Conn: netConn, r: br}
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Println("handshake fallido:", err)
		return
	}
	uuid := sshConn.Permissions.Extensions["uuid"]
	name := sshConn.Permissions.Extensions["name"]
	log.Printf("conectado: uuid=%s (%s) desde %s", uuid, name, rawConn.RemoteAddr())

	conns.add(uuid, sshConn)
	defer conns.remove(uuid, sshConn)

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "direct-tcpip" {
			newChannel.Reject(ssh.UnknownChannelType, "solo se soporta direct-tcpip")
			continue
		}

		var payload struct {
			DestAddr   string
			DestPort   uint32
			OriginAddr string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
			newChannel.Reject(ssh.ConnectionFailed, "payload inválido")
			continue
		}

		target := net.JoinHostPort(payload.DestAddr, fmt.Sprint(payload.DestPort))

		if udpgwAddr != "" && target == udpgwAddr {
			// El cliente pide "conectarse" a la dirección virtual de udpgw
			// -- GPM atiende el protocolo él mismo, no hay nada real que
			// discar aquí. Ver udpgw.go.
			channel, requests, err := newChannel.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(requests)
			log.Printf("[%s] canal udpgw embebido abierto", name)
			go handleUdpgwChannel(channel, uuid, name, usage)
			continue
		}

		targetConn, err := net.Dial("tcp", target)
		if err != nil {
			log.Printf("[%s] no se pudo conectar a %s: %v", name, target, err)
			newChannel.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			targetConn.Close()
			continue
		}
		go ssh.DiscardRequests(requests)

		log.Printf("[%s] canal abierto -> %s", name, target)
		go relay(uuid, name, channel, targetConn, usage)
	}
	log.Printf("desconectado: uuid=%s (%s)", uuid, name)
}

// readDecoyUntilIdle drena el señuelo del cliente (una o varias etapas
// [split], con o sin relleno) hasta que deja de llegar tráfico nuevo
// durante decoyIdleGap. Devuelve el Sec-WebSocket-Key visto en cualquier
// etapa, si lo hubo.
func readDecoyUntilIdle(rawConn net.Conn, r *bufio.Reader) (wsKey string, err error) {
	buf := make([]byte, 0, 512)
	line := make([]byte, 0, 256)
	one := make([]byte, 1)
	for len(buf) < 64*1024 {
		_ = rawConn.SetReadDeadline(time.Now().Add(decoyIdleGap))
		n, rerr := r.Read(one)
		if n > 0 {
			b := one[0]
			buf = append(buf, b)
			line = append(line, b)
			if b == '\n' {
				trimmed := strings.TrimRight(string(line), "\r\n")
				if idx := strings.IndexByte(trimmed, ':'); idx > 0 {
					if strings.EqualFold(strings.TrimSpace(trimmed[:idx]), "Sec-WebSocket-Key") {
						wsKey = strings.TrimSpace(trimmed[idx+1:])
					}
				}
				line = line[:0]
			}
			continue
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				return wsKey, nil
			}
			return wsKey, rerr
		}
	}
	return wsKey, nil
}

func wsAcceptFor(key string) string {
	h := sha1.Sum([]byte(key + wsMagicGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func relay(uuid, name string, channel ssh.Channel, target net.Conn, usage *Usage) {
	defer channel.Close()
	defer target.Close()

	var up, down int64
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, _ := io.Copy(target, channel)
		atomic.AddInt64(&up, n)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(channel, target)
		atomic.AddInt64(&down, n)
	}()
	wg.Wait()

	usage.Add(uuid, up, down)
	log.Printf("[%s] canal cerrado, subida=%dB bajada=%dB", name, up, down)
}

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			block, _ := pem.Decode(data)
			if block == nil {
				return nil, fmt.Errorf("%s: PEM inválido", path)
			}
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return ssh.NewSignerFromKey(key)
		}
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	if path != "" {
		block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			return nil, fmt.Errorf("guardar %s: %w", path, err)
		}
		log.Println("host key nueva generada y guardada en", path)
	}

	return ssh.NewSignerFromKey(key)
}
