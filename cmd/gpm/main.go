// Comando gpm: arranca el servidor GPM (modo manual) o administra el
// archivo de usuarios. El modo "panel" (conectado a v2board) es roadmap,
// ver README.md.
package main

import (
	"flag"
	"fmt"
	"os"

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

Uso:
  gpm serve -addr :80 -users users.json [-hostkey host_key.pem]
  gpm adduser -users users.json <uuid> <nombre>
  gpm deluser -users users.json <uuid>
  gpm listusers -users users.json

Ver README.md para el formato del payload/señuelo y cómo apuntar un perfil
SSH de la app a un servidor GPM.
`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":2222", "dirección:puerto donde escuchar, ej. :80 o 0.0.0.0:8022")
	usersPath := fs.String("users", "users.json", "archivo JSON de usuarios permitidos (uuid -> nombre)")
	hostKeyPath := fs.String("hostkey", "", "archivo donde persistir la host key RSA (vacío = efímera, nueva en cada arranque)")
	fs.Parse(args)

	store, err := server.NewFileUserStore(*usersPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	err = server.Run(server.Options{
		Addr:        *addr,
		HostKeyPath: *hostKeyPath,
		Users:       store,
	})
	if err != nil {
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
