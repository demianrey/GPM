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
	addr := fs.String("addr", ":2222", "dirección:puerto donde escuchar, ej. :80 o 0.0.0.0:8022")
	hostKeyPath := fs.String("hostkey", "", "archivo donde persistir la host key RSA (vacío = efímera, nueva en cada arranque)")

	// Modo manual.
	usersPath := fs.String("users", "", "modo manual: archivo JSON de usuarios permitidos (uuid -> nombre)")

	// Modo panel (v2board, ver README "Roadmap: modo panel").
	panelURL := fs.String("panel-url", "", "modo panel: URL base del panel v2board (ej. https://tu-panel.com)")
	panelNodeID := fs.Int("panel-node-id", 0, "modo panel: id del nodo en el panel")
	panelToken := fs.String("panel-token", "", "modo panel: Communication Key del panel -- EVITAR, queda visible en texto plano vía 'ps aux'/argv para cualquiera con acceso a la máquina (es el secreto MAESTRO del panel entero, no algo acotado a este nodo). Preferir -panel-token-file.")
	panelTokenFile := fs.String("panel-token-file", "", "modo panel: archivo con el Communication Key (recomendado, permisos 0600, sin salto de línea al final o se recorta solo)")
	panelPull := fs.Duration("panel-pull-interval", 60*time.Second, "modo panel: cada cuánto sincronizar la lista de usuarios")
	panelPush := fs.Duration("panel-push-interval", 60*time.Second, "modo panel: cada cuánto reportar consumo")
	fs.Parse(args)

	usingPanel := *panelURL != ""
	usingManual := *usersPath != ""
	if usingPanel == usingManual {
		fmt.Fprintln(os.Stderr, "error: usa exactamente uno de -users (modo manual) o -panel-url (modo panel)")
		os.Exit(2)
	}

	opts := server.Options{
		Addr:        *addr,
		HostKeyPath: *hostKeyPath,
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

		ctx := context.Background()
		panelStore, perr := server.NewPanelUserStore(ctx, server.PanelConfig{
			APIHost:      *panelURL,
			NodeID:       *panelNodeID,
			Token:        token,
			PullInterval: *panelPull,
			PushInterval: *panelPush,
		})
		if perr != nil {
			fmt.Fprintln(os.Stderr, "error:", perr)
			os.Exit(1)
		}
		opts.Users = panelStore
		opts.Usage = server.NewUsage()
		go panelStore.RunUsageReporter(ctx, opts.Usage)
	}

	if err := server.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
