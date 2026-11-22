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
panel), si hay un reverse proxy local delante (ver "Detrás de un reverse
proxy" más abajo — si lo hay, escucha en loopback y fija el señuelo en
101), puerto, modo (panel o manual), y si es panel la URL/Communication
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
campos `bug_host`/`payload`/`split_pos` (y `sni`, usado por el modo TLS —
ver "Modo TLS" abajo) — el admin genera el link `ssh://` de suscripción
directo desde esos campos, coincide byte a byte con lo que `SSHFmt.kt`
(app VpnMax) espera. `node_type` que manda GPM: `GPM`
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
  (default 60s) para leer `server_port`, `behind_cdn` (modo señuelo 101/200)
  y `tls` (modo TLS on/off). Si cambia el puerto, GPM cierra el listener
  viejo y abre uno nuevo ahí mismo, sin reiniciar el proceso -- igual que
  hace v2node, ver `-panel-port-sync` (activo por default, desactivable).
  Los cambios de `behind_cdn`/`tls` se aplican en caliente sin reiniciar el
  listener (ver `-panel-tls-sync`).

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
- Modo TLS (stunnel embebido): implementado del lado GPM (ver abajo),
  pendiente el wiring del panel (booleano `tls` + params en el link) y del
  cliente (`SSHFmt.kt`). Estado y contrato completo en `STATUS.md`.

## Detrás de un reverse proxy (nginx): compartir el puerto 80

Si en el VPS ya hay un nginx ocupando el `:80` (otro panel, otro nodo, un
sitio real), GPM puede convivir ahí sin moverse de puerto para el cliente:
nginx escucha el 80 y hace reverse proxy del señuelo hacia GPM, que
escucha en loopback.

Esto funciona sin ningún cambio en GPM porque el señuelo HTTP-like **es**
una petición con `Connection: Upgrade` / `Upgrade: websocket`, y GPM
contesta `101 Switching Protocols` — exactamente lo que nginx necesita
para dejar de hablar HTTP y pasar a relay de bytes crudos. Para GPM,
nginx cumple el mismo rol que un CDN.

```
cliente ──:80── nginx (location /http9, upgrade) ── 127.0.0.1:18080 ── GPM
```

nginx (en el `server` que ya tenés en el 80; el mismo bloque sirve si
además hay un CDN real por delante):

```nginx
location /http9 {
    proxy_pass http://127.0.0.1:18080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host       $host;
    proxy_buffering  off;
    proxy_read_timeout 7d;   # el túnel vive horas: el default de 60s lo corta
    proxy_send_timeout 7d;
}
```

`config.json` del nodo:

```json
{
  "addr": "127.0.0.1:18080",
  "cdn": true,
  "tls": false,
  "panel": { "cdnSync": false, "tlsSync": false }
}
```

El `"addr"` en loopback es lo que evita que GPM quede alcanzable directo en
el puerto de servicio, saltándose el proxy (`install.sh` lo escribe así
solo si contestás que hay un reverse proxy delante; el default, `:<puerto>`,
bindea todas las interfaces). La interfaz no se sincroniza con el panel a
propósito: solo el puerto es dinámico, el host del `addr` se conserva.

Y en el panel, en la fila del nodo GPM:

| campo | valor |
|---|---|
| `port` | **80** — el que disca el cliente (va al link de suscripción) |
| `service_port` | **18080** — donde escucha el proceso GPM |
| `behind_cdn` | **true** |
| `tls` | 0 |
| `payload` | una sola etapa, con el path del `location` |

```
GET /http9 HTTP/1.1[crlf]Host: <host-que-rutea-al-origin>[crlf]Connection: Upgrade[crlf]Upgrade: websocket[crlf][crlf]
```

### Reglas que no son opcionales

- **El puerto de escucha tiene que venir de `service_port`, no de `port`.**
  GPM sincroniza su listener contra el `server_port` del panel; si el panel
  manda ahí el puerto del cliente (80), GPM rebindea al 80 que ya tiene
  nginx → `bind: address already in use`. Con `service_port` cargado no hay
  problema: el panel manda 18080 y GPM se queda donde está. En un panel que
  todavía no tenga ese campo, el equivalente es `"portSync": false` en el
  bloque `panel` del `config.json` — y ojo, **si esa línea está, hay que
  sacarla a mano para volver a sincronizar**: con `portSync: false` GPM no
  instala el sincronizador y no vuelve a consultar el puerto nunca, así que
  ningún cambio en el panel lo despierta.

  Desde v0.1.6, mientras haya sincronización con el panel un bind fallido
  **no es fatal**: GPM lo loguea, reintenta cada 10s y se muda al puerto
  que reporte el panel (persistiéndolo en el `config.json`), en vez de
  salir con error. Así un nodo que arrancó con un puerto imposible se
  arregla solo en cuanto el admin carga el `service_port` correcto, sin
  entrar al VPS. Antes de eso el proceso salía y systemd lo reintentaba en
  loop para siempre, porque el sincronizador solo arranca después de un
  listen exitoso.
- **`behind_cdn` tiene que estar en `true`.** nginx solo tuneliza si ve el
  `101`; en modo directo GPM responde los dos `200` apilados y nginx da la
  petición por terminada. `"cdnSync": false` + `"cdn": true` evita que un
  cambio en el admin baje el nodo sin que nadie haya tocado el servidor.
- **Sin TLS en este camino.** nginx le manda HTTP en claro al upstream; si
  se prende `tls`, GPM espera un ClientHello y todas las conexiones mueren.
  `"tlsSync": false` + `"tls": false` lo fija.
- **Señuelo de una sola etapa**, sin `[split]`: nginx parsea *una* petición
  HTTP. Esto significa que este montaje sirve para el caso VPS directa con
  bug-host (o un CDN real por delante que termine la petición él), no para
  el domain-fronting de dos saltos hecho por el propio cliente.
- **El path importa**: es lo que le permite a nginx distinguir el tráfico
  GPM del resto que vive en ese mismo `:80`. Tiene que coincidir entre el
  `location` y el `payload` del panel.

Lo único que se pierde es la IP real del cliente en los logs de GPM
(`conectado: ... desde 127.0.0.1`): nginx en modo HTTP no puede mandar
PROXY protocol al upstream. No afecta nada funcional — la contabilidad es
por uuid, no por IP.

### Variante con TLS en el 443

Para el modo TLS el patrón es otro, porque GPM termina su propio TLS y no
hay que desarmarlo: `stream` + `ssl_preread`, ruteando por SNI.

```nginx
stream {
    map $ssl_preread_server_name $backend {
        <sni-del-nodo>  127.0.0.1:8443;   # GPM con "tls": true
        default         127.0.0.1:4443;   # el sitio HTTPS real
    }
    server { listen 443; proxy_pass $backend; ssl_preread on; }
}
```

(con el `server` HTTPS de nginx movido a `listen 127.0.0.1:4443 ssl`).
Acá sí se podría conservar la IP real con `proxy_protocol on`, pero GPM
todavía no parsea PROXY protocol.

## Modo TLS (stunnel embebido)

Además del señuelo HTTP-like, GPM puede envolver cada conexión en **TLS
1.3 con SNI** antes del handshake SSH, para que en el cable el tráfico
parezca una navegación HTTPS real hacia el dominio del SNI (misma idea que
un `stunnel` por delante de SSH, pero embebido en el binario). Reemplaza
al señuelo: en modo TLS el cliente abre TLS y adentro va el SSH directo,
sin payload.

El cert es **self-signed, generado por SNI al vuelo** (un leaf con
`SAN = ServerName` del ClientHello, firmado por una CA interna que se
persiste). El cliente conecta con `allowInsecure` (no valida la cadena):
esta capa TLS es **camuflaje, no autenticación** — el cifrado/auth reales
los da el SSH de adentro (el uuid como credencial).

```
# modo manual, TLS con CA persistida
gpm serve -addr :443 -tls -tls-ca tls_ca.pem -users users.json

# probar el endpoint
openssl s_client -connect HOST:443 -servername www.microsoft.com
```

En `-config`, los campos son `"tls": true` (+ `"tlsCa": "..."`), y en el
bloque `panel`, `"tlsSync": true` (default) sincroniza el modo TLS en
caliente contra el campo `tls` de `/api/v2/server/config` — igual que
`cdnSync`/`behind_cdn`, un cambio no reinicia el listener. El SNI que
manda el cliente lo define el panel en el campo `sni` del nodo.

## Seguridad

Esto es una herramienta de investigación sobre técnicas de bypass de
zero-rating / degradación de velocidad para uso propio y educativo. No
implementa ninguna protección contra abuso más allá de la lista de UUIDs
permitidos — no lo expongas sin entender las implicaciones de correr un
relay TCP arbitrario (`direct-tcpip`) hacia cualquier destino que pida un
cliente autenticado.
