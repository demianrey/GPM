// Comando gpm: arranca el servidor GPM, en modo manual (archivo de
// usuarios local) o modo panel (sincronizado contra v2board, ver README).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/demianrey/GPM/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "adduser":
		cmdAddUser(os.Args[2:])
	case "deluser":
		cmdDelUser(os.Args[2:])
	case "listusers":
		cmdListUsers(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "comando desconocido: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gpm -- Go Payload Multiplexer

Uso (modo manual, archivo de usuarios local):
  gpm serve -addr :80 -users users.json [-hostkey host_key.pem]
  gpm adduser -users users.json <uuid> <nombre>
  gpm deluser -users users.json <uuid>
  gpm listusers -users users.json

Uso (modo panel, sincronizado contra v2board -- ver README):
  gpm serve -addr :80 -panel-url https://tu-panel.com \
      -panel-node-id 1 -panel-token TU_API_KEY [-hostkey host_key.pem]

Ver README.md para el formato del payload/señuelo y cómo apuntar un perfil
SSH de la app a un servidor GPM.
`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "archivo JSON de configuración (recomendado en producción, ver README) -- si se da, es la ÚNICA fuente de verdad, se ignoran los demás flags")
	addr := fs.String("addr", ":2222", "dirección:puerto donde escuchar, ej. :80 o 0.0.0.0:8022")
	hostKeyPath := fs.String("hostkey", "", "archivo donde persistir la host key RSA (vacío = efímera, nueva en cada arranque)")
	udpgwAddr := fs.String("udpgw-addr", "127.0.0.1:7300", "dirección virtual para soporte UDP embebido (protocolo udpgw) -- debe coincidir con el udpgwAddress del perfil cliente. Vacío = deshabilitado")
	cdn := fs.Bool("cdn", true, "el nodo está detrás de un CDN tipo Cloudflare -- si true, el señuelo responde 101 (lo que el CDN necesita para pasar a modo túnel crudo); si false (conexión directa, sin CDN), responde 200")

	// Modo manual.
	usersPath := fs.String("users", "", "modo manual: archivo JSON de usuarios permitidos (uuid -> nombre)")

	// Modo panel (v2board, ver README "Roadmap: modo panel").
	panelURL := fs.String("panel-url", "", "modo panel: URL base del panel v2board (ej. https://tu-panel.com)")
	panelNodeID := fs.Int("panel-node-id", 0, "modo panel: id del nodo en el panel")
	panelToken := fs.String("panel-token", "", "modo panel: Communication Key del panel -- EVITAR, queda visible en texto plano vía 'ps aux'/argv para cualquiera con acceso a la máquina (es el secreto MAESTRO del panel entero, no algo acotado a este nodo). Preferir -panel-token-file.")
	panelTokenFile := fs.String("panel-token-file", "", "modo panel: archivo con el Communication Key (recomendado, permisos 0600, sin salto de línea al final o se recorta solo)")
	panelPull := fs.Duration("panel-pull-interval", 60*time.Second, "modo panel: cada cuánto sincronizar la lista de usuarios")
	panelPush := fs.Duration("panel-push-interval", 60*time.Second, "modo panel: cada cuánto reportar consumo")
	panelPortSync := fs.Bool("panel-port-sync", true, "modo panel: seguir el puerto configurado en el nodo del panel (/api/v2/server/config), reiniciando el listener solo si cambia -- igual que v2node")
	panelPortCheck := fs.Duration("panel-port-check-interval", 60*time.Second, "modo panel: cada cuánto consultar el puerto del nodo en el panel")
	fs.Parse(args)

	if *configPath != "" {
		serveFromConfig(*configPath)
		return
	}

	usingPanel := *panelURL != ""
	usingManual := *usersPath != ""
	if usingPanel == usingManual {
		fmt.Fprintln(os.Stderr, "error: usa exactamente uno de -users (modo manual) o -panel-url (modo panel), o -config")
		os.Exit(2)
	}

	decoyStatus := 101
	if !*cdn {
		decoyStatus = 200
	}

	opts := server.Options{
		Addr:        *addr,
		HostKeyPath: *hostKeyPath,
		UdpgwAddr:   *udpgwAddr,
		DecoyStatus: decoyStatus,
	}

	var err error
	if usingManual {
		opts.Users, err = server.NewFileUserStore(*usersPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	} else {
		token := *panelToken
		if *panelTokenFile != "" {
			data, rerr := os.ReadFile(*panelTokenFile)
			if rerr != nil {
				fmt.Fprintln(os.Stderr, "error leyendo -panel-token-file:", rerr)
				os.Exit(1)
			}
			token = strings.TrimSpace(string(data))
		} else if token == "" {
			// Última opción antes de fallar: variable de entorno, tampoco
			// queda en argv (aunque sí en /proc/PID/environ para el mismo
			// usuario o root -- sigue siendo mejor que un flag en claro).
			token = os.Getenv("GPM_PANEL_TOKEN")
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "error: falta el Communication Key -- usa -panel-token-file (recomendado), -panel-token, o la variable de entorno GPM_PANEL_TOKEN")
			os.Exit(2)
		}

		opts.Conns = server.NewConnRegistry()

		ctx := context.Background()
		panelStore, perr := server.NewPanelUserStore(ctx, server.PanelConfig{
			APIHost:      *panelURL,
			NodeID:       *panelNodeID,
			Token:        token,
			PullInterval: *panelPull,
			PushInterval: *panelPush,
			Conns:        opts.Conns,
		})
		if perr != nil {
			fmt.Fprintln(os.Stderr, "error:", perr)
			os.Exit(1)
		}
		opts.Users = panelStore
		opts.Usage = server.NewUsage()
		go panelStore.RunUsageReporter(ctx, opts.Usage)

		if *panelPortSync {
			opts.PortProvider = panelStore.FetchNodePort
			opts.PortCheckInterval = *panelPortCheck
		}
	}

	if err := server.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serveFromConfig(path string) {
	cfg, err := loadFileConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	usingManual := cfg.UsersPath != ""
	usingPanel := cfg.Panel != nil
	if usingManual == usingPanel {
		fmt.Fprintf(os.Stderr, "error: %s debe traer exactamente uno de \"users\" o \"panel\"\n", path)
		os.Exit(2)
	}

	decoyStatus := 101
	if cfg.Cdn != nil && !*cfg.Cdn {
		decoyStatus = 200
	}

	opts := server.Options{
		Addr:        cfg.Addr,
		HostKeyPath: cfg.HostKeyPath,
		UdpgwAddr:   cfg.UdpgwAddr,
		DecoyStatus: decoyStatus,
	}

	if usingManual {
		var err error
		opts.Users, err = server.NewFileUserStore(cfg.UsersPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	} else {
		p := cfg.Panel
		if p.TokenFile == "" {
			fmt.Fprintf(os.Stderr, "error: %s: panel.tokenFile es requerido\n", path)
			os.Exit(2)
		}
		data, rerr := os.ReadFile(p.TokenFile)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "error leyendo panel.tokenFile:", rerr)
			os.Exit(1)
		}
		token := strings.TrimSpace(string(data))

		pull := parseDurationOr(p.PullInterval, 60*time.Second)
		push := parseDurationOr(p.PushInterval, 60*time.Second)
		portCheck := parseDurationOr(p.PortCheckInterval, 60*time.Second)
		portSync := p.PortSync == nil || *p.PortSync

		opts.Conns = server.NewConnRegistry()

		ctx := context.Background()
		panelStore, perr := server.NewPanelUserStore(ctx, server.PanelConfig{
			APIHost:      p.URL,
			NodeID:       p.NodeID,
			Token:        token,
			PullInterval: pull,
			PushInterval: push,
			Conns:        opts.Conns,
		})
		if perr != nil {
			fmt.Fprintln(os.Stderr, "error:", perr)
			os.Exit(1)
		}
		opts.Users = panelStore
		opts.Usage = server.NewUsage()
		go panelStore.RunUsageReporter(ctx, opts.Usage)

		if portSync {
			opts.PortProvider = panelStore.FetchNodePort
			opts.PortCheckInterval = portCheck
			opts.OnPortChange = func(newAddr string) {
				if perr := persistAddr(path, newAddr); perr != nil {
					fmt.Fprintln(os.Stderr, "aviso: no se pudo guardar el puerto nuevo en", path, ":", perr)
				}
			}
		}

		cdnSync := p.CdnSync == nil || *p.CdnSync
		if cdnSync {
			opts.DecoyStatusProvider = panelStore.FetchNodeCdn
			opts.DecoyStatusCheckInterval = portCheck
		}
	}

	if err := server.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func cmdAddUser(args []string) {
	fs := flag.NewFlagSet("adduser", flag.ExitOnError)
	usersPath := fs.String("users", "users.json", "archivo JSON de usuarios permitidos")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "uso: gpm adduser -users users.json <uuid> <nombre>")
		os.Exit(2)
	}
	uuid, name := rest[0], rest[1]

	users, err := server.LoadUsersFile(*usersPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	users[uuid] = name
	if err := server.SaveUsersFile(*usersPath, users); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("agregado: %s (%s)\n", uuid, name)
}

func cmdDelUser(args []string) {
	fs := flag.NewFlagSet("deluser", flag.ExitOnError)
	usersPath := fs.String("users", "users.json", "archivo JSON de usuarios permitidos")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "uso: gpm deluser -users users.json <uuid>")
		os.Exit(2)
	}
	uuid := rest[0]

	users, err := server.LoadUsersFile(*usersPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if _, ok := users[uuid]; !ok {
		fmt.Fprintln(os.Stderr, "no existe:", uuid)
		os.Exit(1)
	}
	delete(users, uuid)
	if err := server.SaveUsersFile(*usersPath, users); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("eliminado:", uuid)
}

func cmdListUsers(args []string) {
	fs := flag.NewFlagSet("listusers", flag.ExitOnError)
	usersPath := fs.String("users", "users.json", "archivo JSON de usuarios permitidos")
	fs.Parse(args)

	users, err := server.LoadUsersFile(*usersPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(users) == 0 {
		fmt.Println("(sin usuarios)")
		return
	}
	for uuid, name := range users {
		fmt.Printf("%s  %s\n", uuid, name)
	}
}
