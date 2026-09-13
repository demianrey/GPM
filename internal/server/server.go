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
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
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

	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Println("GPM escuchando en", opts.Addr, "(protocolo SSH, auth por UUID)")

	for {
		rawConn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go handleConn(rawConn, config, usage)
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

func handleConn(rawConn net.Conn, config *ssh.ServerConfig, usage *Usage) {
	defer rawConn.Close()

	br := bufio.NewReader(rawConn)

	_ = rawConn.SetReadDeadline(time.Now().Add(decoyPeekTimeout))
	prefix, peekErr := br.Peek(4)

	if peekErr == nil && !bytes.Equal(prefix, []byte("SSH-")) {
		// Llegó algo rápido y no es un banner SSH -- se asume señuelo
		// HTTP-like (mismo patrón que el injector de ws/xhttp del cliente
		// en exclave-core: tokens [crlf]/[cr]/[lf]/[split]).
		wsKey, derr := readDecoyUntilIdle(rawConn, br)
		if derr != nil {
			log.Println("señuelo: error leyendo headers:", derr)
			return
		}
		// SIEMPRE 101, tenga o no el señuelo un Sec-WebSocket-Key real. Un
		// CDN como Cloudflare solo pasa a modo túnel crudo (reenvía lo que
		// sigue tal cual, sin re-interpretarlo como otra petición HTTP) si
		// ve status 101 en la respuesta del origen -- da igual si el
		// señuelo trae un upgrade de WebSocket genuino o solo el texto de
		// adorno (como en payloads reales tipo HTTP Injector, que nunca
		// completan un handshake WS de verdad). Con 200 el CDN da la
		// petición por terminada y no entrega nada más.
		var resp string
		if wsKey != "" {
			resp = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAcceptFor(wsKey) + "\r\n\r\n"
		} else {
			resp = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
		}
		if _, err := rawConn.Write([]byte(resp)); err != nil {
			log.Println("señuelo: error escribiendo respuesta:", err)
			return
		}
	}
	// Si peekErr fue timeout (nada llegó a tiempo) o si ya vimos "SSH-" de
	// entrada, no hacemos nada más aquí: seguimos directo al handshake
	// real, y ssh.NewServerConn manda NUESTRO banner primero -- lo cual
	// además es lo que rompe cualquier estancamiento con el otro lado.
	_ = rawConn.SetReadDeadline(time.Time{})

	conn := &bufConn{Conn: rawConn, r: br}
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Println("handshake fallido:", err)
		return
	}
	uuid := sshConn.Permissions.Extensions["uuid"]
	name := sshConn.Permissions.Extensions["name"]
	log.Printf("conectado: uuid=%s (%s) desde %s", uuid, name, rawConn.RemoteAddr())

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
