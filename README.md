# GPM — Go Payload Multiplexer

GPM es un servidor que habla el protocolo **SSH real** (vía
[`golang.org/x/crypto/ssh`](https://pkg.go.dev/golang.org/x/crypto/ssh)) por
delante de un **señuelo HTTP-like opcional**, pensado para el mismo tipo de
bypass de degradación de velocidad / zero-rating que usan apps del estilo
"HTTP Injector" (payload en claro antes del handshake real) — pero con una
diferencia clave: **autentica con el mismo UUID que ya usan los clientes
VLESS** (auth `"none"` de SSH, sin password compartido), para poder medir
consumo **por usuario** en vez de compartir un solo login entre todos los
revendidos, como hacen esas apps.

Nace de una investigación real sobre cómo apps tipo AlberVPN/SSHPlus logran
saltarse la degradación de datos de ciertas operadoras (Telcel/México, Claro
RD), documentada a fondo en el proyecto
[VpnMax](https://github.com/demianrey) (no público). El cliente de
referencia es el outbound `ssh` de un fork de
[`exclave-core`](https://github.com/exclavenetwork/exclave-core) (a su vez
fork de Xray-core), que ya implementa el mismo señuelo del lado cliente.

## Cómo funciona el señuelo

1. El cliente conecta y manda un texto HTTP-like (el "señuelo") — puede ser
   una sola petición o varias etapas separadas en el tiempo (para casos
   donde la operadora/CDN espera ver más de una petición, ej. domain
   fronting real contra Cloudflare).
2. GPM detecta si lo que llega es un señuelo o ya es un banner SSH real
   (`Peek` de los primeros 4 bytes, con un timeout corto). Si no llega nada
   a tiempo, asumimos que estamos detrás de un relay externo que ya se comió
   el señuelo (o que es un cliente SSH real esperando que hablemos primero)
   y arrancamos el handshake directo.
3. Si hay señuelo, GPM lo drena **por inactividad**: espera hasta que el
   cliente deja de mandar bytes nuevos (con un margen que tolera etapas
   `[split]` separadas por unos milisegundos), y ahí responde con
   `HTTP/1.1 101 Switching Protocols` o `HTTP/1.1 200 OK` según el nodo esté
   detrás de un CDN o no (`-cdn`/`"cdn"` en `-config`, default `true` = 101).
   Esto no es arbitrario en ninguno de los dos casos: nuestro propio cliente
   (y los de apps reales tipo HTTP Injector) mandan el señuelo y se quedan
   esperando esta respuesta antes de mandar su banner SSH — si el servidor
   esperara ver el banner del cliente antes de responder, ambos lados se
   quedarían esperando el uno al otro. El `101` específicamente importa
   cuando el destino real está detrás de un CDN real (Cloudflare, Fastly):
   sin ese status, el CDN da la petición por terminada y no relaya nada más.
   Sin CDN de por medio (conexión directa), no hace falta simular ningún
   upgrade — un `200` llano es el camuflaje más discreto, igual que hace
   `open.py` de SSHPlus en su modo directo (`proxy.py`, el modo con CDN,
   siempre usa `101`).
4. Una vez respondido (o de entrada, si no había señuelo), arranca el
   handshake SSH real. GPM manda su banner primero, lo cual además es lo
   que evita cualquier bloqueo mutuo con el otro lado.
5. Autenticación: `NoClientAuth: true` + `NoClientAuthCallback`, que valida
   el username (el UUID) contra la lista de usuarios permitidos. Sin
   password, sin llave — el UUID es el secreto, igual que en VLESS.
6. Solo se aceptan canales `direct-tcpip` (RFC 4254) — se rechaza `session`
   explícitamente, así que no hay shell real expuesta, solo forwarding.
7. Cada canal cuenta bytes de subida/bajada por UUID (`internal/server`,
   `userUsage`) — la base para medir consumo real por usuario.

## Instalación

```
go install github.com/demianrey/GPM/cmd/gpm@latest
```

O compilar desde el repo:

```
git clone https://github.com/demianrey/GPM.git
cd GPM
go build -o gpm ./cmd/gpm
```

También hay binarios precompilados para Linux (amd64/arm64) en cada
[release](https://github.com/demianrey/GPM/releases), generados por el
GitHub Action (`.github/workflows/build.yml`).

### Instalación con systemd (recomendado en producción)

Igual que [v2node](https://github.com/wyx2685/v2node): un instalador que
descarga el binario, arma el servicio systemd y deja un comando de
administración (`gpm-cli`) con menú interactivo para start/stop/restart/
logs/etc, sin tener que acordarse de flags ni de `journalctl` a mano.

```
bash <(curl -Ls https://raw.githubusercontent.com/demianrey/GPM/main/script/install.sh)
```

Pide interactivamente: id del nodo (el mismo `node_id` que tiene en el
panel), puerto, modo (panel o manual), y si es panel la URL/Communication
Key. Varios nodos pueden convivir en el mismo VPS: cada `install.sh`
adicional da de alta uno nuevo sin tocar los que ya corren, todos comparten
el mismo binario `/usr/local/bin/gpm`.

Cada nodo vive en `/etc/gpm/<node_id>/`:

```
/etc/gpm/1/config.json     # config legible, formato ver abajo
/etc/gpm/1/host_key.pem    # host key persistida
/etc/gpm/1/panel-token     # Communication Key, 0600, nunca en el comando
```

...y corre como su propia instancia systemd `gpm@1.service` (plantilla
`/etc/systemd/system/gpm@.service`, instalada una sola vez).

```
gpm-cli                 # menú interactivo (pide el nodo si hay varios)
gpm-cli 1                # menú directo del nodo 1
gpm-cli restart 1         # o por subcomando: start/stop/restart/status/log/enable/disable/config/token/uninstall [node_id]
gpm-cli list              # estado de todos los nodos instalados
gpm-cli add                # da de alta otro nodo (re-corre install.sh)
```

`gpm-cli config <id>` abre `/etc/gpm/<id>/config.json` con `nano` (o
`$EDITOR` si está seteado) y reinicia ese nodo al guardar. `gpm-cli token
<id>` muestra y permite cambiar el Communication Key de ese nodo sin editar
archivos a mano. `gpm-cli log <id>` sigue `journalctl -u gpm@<id> -f` en
vivo. El campo `"portSync"` (default `true`, ver formato abajo) ya viene
activo -- si cambias el puerto desde el panel, GPM reinicia su propio
listener solo y actualiza `"addr"` en el `config.json` para que sobreviva
un reinicio del proceso, sin que haga falta tocar nada aquí.

`config.json` de un nodo en modo panel se ve así:

```json
{
  "addr": ":80",
  "hostkey": "/etc/gpm/1/host_key.pem",
  "cdn": true,
  "panel": {
    "url": "https://tu-panel.com",
    "nodeId": 1,
    "tokenFile": "/etc/gpm/1/panel-token",
    "pullInterval": "60s",
    "pushInterval": "60s",
    "portCheckInterval": "60s"
  }
}
```

Es la única fuente de verdad para `gpm serve -config /etc/gpm/1/config.json`
-- si se pasa `-config`, los demás flags de `gpm serve` se ignoran.

## Uso — modo manual

Este es el único modo implementado por ahora. El servidor lee la lista de
usuarios permitidos de un archivo JSON simple (`uuid -> nombre`), con
recarga en caliente (no hace falta reiniciar para que un `adduser` surta
efecto).

```
# agregar un usuario
gpm adduser -users users.json c51c0821-2df6-4982-894a-7468d7b92cc9 demianrey

# ver la lista
gpm listusers -users users.json

# quitar un usuario
gpm deluser -users users.json c51c0821-2df6-4982-894a-7468d7b92cc9

# arrancar el servidor
gpm serve -addr :80 -users users.json -hostkey host_key.pem
```

`-hostkey` es opcional pero recomendado en producción: persiste la host key
RSA en disco para que el fingerprint no cambie en cada reinicio. Sin esa
flag se genera una nueva host key efímera cada vez que arranca (solo para
pruebas rápidas).

`users.json` es texto plano, editable a mano si hace falta:

```json
{
  "c51c0821-2df6-4982-894a-7468d7b92cc9": "demianrey",
  "cccbf096-5e99-403c-8a78-4cba85b4af82": "levi"
}
```

## Configurar el cliente (perfil SSH en la app VpnMax / exclave-core)

El campo `payload` del perfil SSH acepta un template con tokens `[crlf]`,
`[cr]`, `[lf]` y `[split]` (para señuelos multi-etapa). Ejemplo mínimo,
directo a un GPM sin CDN de por medio:

```
GET / HTTP/1.1[crlf]Host: <bug-host>[crlf]Connection: Upgrade[crlf]Upgrade: websocket[crlf][crlf]
```

Link `ssh://` completo:

```
ssh://<uuid>@<host>:<puerto>?bugHost=<bug-host>&payload=<template-urlencoded>&splitPos=<int>#<nombre>
```

Con CDN real de por medio (ej. Cloudflare), un payload de dos etapas
funciona así: la primera etapa apunta a un dominio que SÍ vive en ese CDN
(carnada para el clasificador de la operadora), la segunda apunta al
dominio propio (también proxied por el mismo CDN, origin = este servidor):

```
HEAD / HTTP/1.1[crlf]Host: <dominio-carnada-en-el-cdn>[crlf][crlf][split]GET / HTTP/1.1[crlf]Host: <tu-dominio-propio-en-el-mismo-cdn>[crlf]Connection: Upgrade[crlf]Upgrade: websocket[crlf][crlf]
```

## Modo panel (conectado a v2board + módulo GPM)

GPM corre como backend de un panel [v2board](https://github.com/v2board/v2board)
**con el módulo GPM instalado** (tabla/modelo/controlador/rutas propios
`server/gpm/*`, "GPM" como su propio item en el menú de Nodos, junto a
v2node/shadowsocks/vmess/trojan/hysteria/tuic/vless/anytls — NO es v2board
stock, requiere ese módulo agregado al panel). Sincroniza la lista de
usuarios/UUIDs contra la API del panel (en vez de `users.json`) y reporta el
consumo de vuelta, usando los mismos endpoints "UniProxy" que usa v2node
para cualquier otro protocolo.

Vía `install.sh`/`gpm-cli` (recomendado, ver arriba) ya queda todo armado en
`/etc/gpm/<node_id>/`. A mano, equivale a:

```
echo "TU_COMMUNICATION_KEY" > /etc/gpm/1/panel-token && chmod 600 /etc/gpm/1/panel-token

gpm serve -addr :80 -panel-url https://tu-panel.com \
    -panel-node-id 1 -panel-token-file /etc/gpm/1/panel-token \
    [-panel-pull-interval 60s] [-panel-push-interval 60s] \
    [-hostkey host_key.pem]
```

...o, con el formato `-config` (lo que arma `gpm-cli` en realidad):

```
gpm serve -config /etc/gpm/1/config.json
```

**Usa `-panel-token-file` (o `panel.tokenFile` en `-config`), no
`-panel-token`.** El Communication Key de
v2board NO es una credencial acotada a este nodo — es el secreto MAESTRO
compartido por TODOS los nodos del panel (`UniProxyController` lo valida
contra `config('v2board.server_token')`, un único valor global). Con él se
puede leer el uuid de cualquier usuario de cualquier grupo y postear
consumo falso contra cualquier nodo. Pasarlo por `-panel-token` lo deja
visible en texto plano vía `ps aux`/`/proc/PID/cmdline` para cualquiera con
acceso a la máquina. `-panel-token-file` (archivo 0600) o la variable de
entorno `GPM_PANEL_TOKEN` evitan eso.

El nodo se crea desde el admin del panel (Nodos → GPM), con sus propios
campos `bug_host`/`payload`/`split_pos` (y `sni`, reservado para una futura
capa TLS, todavía no implementada de este lado) — el admin genera el link
`ssh://` de suscripción directo desde esos campos, coincide byte a byte con
lo que `SSHFmt.kt` (app VpnMax) espera. `node_type` que manda GPM: `GPM`
(configurable con `PanelConfig.NodeTypeOverride`), con CacheKey de
online/stats propias en el panel, separadas de v2node real.

Endpoints usados:
- `GET /api/v1/server/UniProxy/user` — sincroniza `uuid -> id` cada
  `-panel-pull-interval` (default 60s). Sincronización inicial bloqueante
  al arrancar (si falla, el servidor no arranca).
- `POST /api/v1/server/UniProxy/push` — reporta subida/bajada acumulada por
  usuario cada `-panel-push-interval` (default 60s), como delta desde el
  último reporte (no acumulado histórico).
- `GET /api/v2/server/config` — se consulta cada `-panel-port-check-interval`
  (default 60s) solo para leer `server_port`. Si cambia respecto al puerto
  actual, GPM cierra el listener viejo y abre uno nuevo ahí mismo, sin
  reiniciar el proceso -- igual que hace v2node, ver `-panel-port-sync`
  (activo por default, desactivable).

Arquitectura: `server.Run` recibe cualquier implementación de
`server.UserStore` (interfaz de una sola función,
`Lookup(uuid) (name string, ok bool)`) — `FileUserStore` (modo manual) y
`PanelUserStore` (este modo, `internal/server/panel.go`) son intercambiables
sin que el resto del servidor sepa de dónde salen los usuarios.

**Pendiente:**
- Reporte de usuarios online (`UniProxy/alive`/`alivelist`) — implementado
  del lado panel (CacheKey `SERVER_GPM_ONLINE_USER`), no implementado del
  lado GPM todavía.
- Prueba end-to-end contra un nodo real creado desde el admin (hay uno de
  prueba, `show=0` hasta aprobarlo).
- Soporte SNI/TLS real — el campo existe en el panel (reservado), sin
  implementación de este lado.

## Seguridad

Esto es una herramienta de investigación sobre técnicas de bypass de
zero-rating / degradación de velocidad para uso propio y educativo. No
implementa ninguna protección contra abuso más allá de la lista de UUIDs
permitidos — no lo expongas sin entender las implicaciones de correr un
relay TCP arbitrario (`direct-tcpip`) hacia cualquier destino que pida un
cliente autenticado.
