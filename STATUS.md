# Estado del proyecto GPM

Documento vivo de arquitectura/decisiones/pendientes -- complementa el
README (que es más "cómo usarlo"). Pensado para que cualquiera (persona o
sesión de Claude nueva) que abra este repo entienda dónde está parado el
proyecto sin tener que reconstruir el historial de conversación.

**No hay secretos acá** (repo público) -- IPs/credenciales de servidores de
prueba, tokens del panel, etc viven en `LOCAL_NOTES.md` (gitignored, solo
en la máquina local).

## Qué es GPM, en una frase

Servidor SSH real (`golang.org/x/crypto/ssh`) con un señuelo HTTP-like
opcional antes del handshake, autenticando por el mismo UUID que VLESS --
pensado para el mismo bypass de zero-rating que usan apps tipo AlberVPN/
HTTP Injector, pero con medición de consumo por usuario real contra un
panel v2board (con un módulo GPM agregado, no es v2board stock).

## Arquitectura actual (multi-nodo, config JSON)

- Cada nodo vive en `/etc/gpm/<node_id>/{config.json,host_key.pem,panel-token}`,
  identificado por su `node_id` del panel -- varios nodos conviven en el
  mismo VPS, comparten el binario `/usr/local/bin/gpm`.
- `gpm serve -config /etc/gpm/<id>/config.json` es la forma real de correr
  esto en producción -- reemplazó el viejo formato de flags sueltos +
  systemd `EnvironmentFile`. Los flags individuales (`-addr`, `-panel-url`,
  etc) siguen existiendo para pruebas rápidas, pero si se da `-config` es
  la ÚNICA fuente de verdad.
- systemd template `gpm@.service` (`%i` = node_id) -- `gpm@1`, `gpm@2`, etc.
- `gpm-cli`: nano como editor default, multi-nodo (`gpm-cli list`,
  `gpm-cli add`, `gpm-cli token <id>`, y todos los subcomandos toman un
  node_id opcional -- si hay uno solo instalado lo usa directo).
- El puerto se sincroniza en caliente contra `/api/v2/server/config`
  (`server_port`) y, cuando cambia, se reescribe SOLO el campo `"addr"` del
  `config.json` (vía `Options.OnPortChange`) para que sobreviva un
  reinicio del proceso sin depender del primer chequeo al panel.

## Señuelo: 101 vs 200 según CDN, sincronizado en caliente

GPM responde distinto al señuelo según si el nodo está detrás de un CDN
real (Cloudflare) o es conexión directa:

- **Detrás de CDN** → `101 Switching Protocols` (Cloudflare exige verlo
  para pasar a modo túnel crudo).
- **Directo** → **dos** respuestas HTTP apiladas: `200 <marca>` +
  `200 Connection Established` -- confirmado byte a byte comparando contra
  un servidor AlberVPN real que sí funciona bien con el clasificador
  zero-rating de Altice/Claro RD. Un solo `200 OK` (lo que había antes) es
  más genérico y más fácil de distinguir para el clasificador que el
  patrón real de un injector de dos saltos.

Esto **no se puede derivar** de otros campos del nodo (se intentó con
`bug_host`, confirmado con casos reales que no correlaciona -- puede estar
seteado tanto en modo directo como en modo CDN real de domain-fronting).
El panel tiene un campo dedicado **`behind_cdn`** (boolean JSON estricto,
default `true`) en la tabla del nodo GPM, expuesto en
`GET /api/v2/server/config` junto al `server_port`. GPM lo sincroniza cada
`panel.portCheckInterval` (reusa el mismo endpoint) vía
`PanelUserStore.FetchNodeCdn` + `Options.DecoyStatusProvider` -- a
diferencia del puerto, un cambio acá NO reinicia el listener, solo
actualiza un valor atómico que cada conexión nueva lee al vuelo.

Desactivable con `"cdnSync": false` en el bloque `panel` del
`config.json` (en cuyo caso se queda fijo en el `"cdn"` estático de ese
mismo archivo).

## Kick-on-revocación y kick-on-cambio-de-puerto

`ConnRegistry` (`internal/server/server.go`) rastrea conexiones SSH
activas por uuid:

- Si un uuid desaparece de un pull del panel (cuota agotada, vigencia
  vencida, usuario borrado) → se cortan sus sesiones YA ABIERTAS, no solo
  se bloquean reconexiones nuevas. Mismo comportamiento que
  `RemoveUser`+`LinkManager.CloseAll()` en v2node.
- Si cambia el puerto del nodo → se cortan TODAS las conexiones activas
  (decisión explícita: así se comporta v2node/VLESS en producción real,
  no hay motivo para tratar GPM distinto).
- Salvaguarda: un pull EXITOSO con 0 usuarios (indistinguible de "se le
  cambió el grupo al nodo por error" en el admin) necesita confirmarse en
  DOS pulls consecutivos antes de aplicarse -- evita cortar a todo el
  mundo por una señal ambigua. Un pull FALLIDO (red/5xx/JSON inválido)
  nunca toca nada, ya estaba bien manejado desde el principio.

## UDP vía udpgw, fusionado en el binario

SSH (`direct-tcpip`) solo reenvía TCP. `internal/server/udpgw.go`
reimplementa el protocolo real de badvpn-udpgw DENTRO de GPM: un canal
`direct-tcpip` que pide conectarse a `Options.UdpgwAddr` (default
`127.0.0.1:7300`, mismo valor que ya manda el cliente) se atiende
internamente -- no hace falta ningún proceso externo escuchando ahí, es
puramente una dirección de reconocimiento interno. El consumo UDP se
reporta con un flush periódico (30s) por canal, no solo al cerrar --
importante porque el canal udpgw dura toda la sesión VPN (un solo canal
multiplexa TODOS los flujos UDP, a diferencia de TCP donde cada conexión
abre y cierra su propio canal seguido).

## Port a iOS: hecho, verificado end-to-end

El protocolo SSH/GPM (incluido el señuelo) está portado al core de iOS
(fork propio de Xray-core vanilla, NO exclave-core -- APIs distintas:
logging por funciones libres en vez de `WriteToLog`, sin los atajos
mux/TLS del Handler que exclave-core sí tiene, etc). Vive en
`Xray-core-libXray/proxy/ssh/` + `infra/conf/ssh.go`, y el parseo del link
`ssh://` de suscripción vive en `libXray-build/share/parse_share.go`
(`case "ssh"`). Detalles y ubicación exacta de esos otros dos repos en
`LOCAL_NOTES.md`.

Bug real encontrado y arreglado ahí: el decoder Swift de la app declara
varios campos NO opcionales (`streamSettings` completo, y casi todos los
campos de la config SSH salvo `tcpSettings`) -- un solo `null` en
cualquiera de ellos hacía fallar la decodificación de TODO el array de
outbounds de la suscripción, no solo el nodo SSH (ningún servidor se
mostraba). Fix del lado Go: `streamSettings` siempre no-nil con todos los
sub-objetos que Swift espera, y sin `omitempty` en los campos de
`SSHClientConfig`.

## stunnel embebido (TLS + SNI): CONTRATO entre GPM, panel y cliente

**Estado: PROPUESTA acordada, todavía NO implementada en ningún lado.**
Esta sección es el contrato compartido para que las tres sesiones (esta =
GPM/server, la del panel `v2board_mod`, y la del core del cliente
Android/iOS) implementen lo mismo sin pisarse. Cualquier cambio a lo de
acá hay que reflejarlo en las tres.

### Qué es y por qué

Tercera técnica de camuflaje, además del señuelo HTTP-like y udpgw, y
como esas, **embebida en el mismo binario** (sin depender de un `stunnel`
externo, para no perder control de lo que se reporta al panel). Envuelve
la conexión GPM en una capa **TLS 1.3 con SNI**, de modo que en el cable
el tráfico es indistinguible de una navegación HTTPS real hacia el
dominio del SNI. Reemplaza al señuelo HTTP-like: con SNI el camuflaje ya
lo da el "HTTPS", meter el señuelo adentro sería redundante y agregaría
superficie de detección.

### Decisión sobre go-tunnel (referencia, NO dependencia)

Se evaluó `github.com/opencoff/go-tunnel` (reemplazo genérico de stunnel,
~4500 líneas). **No se vendoriza ni se importa** -- trae QUIC, SOCKS,
ratelimits, proxy-protocol, YAML, client certs, etc, nada de lo cual
usamos, y arrastraría un árbol de dependencias enorme (quic-go incluido)
para lo que en realidad son ~15 líneas de `crypto/tls` de stdlib
(`tls.Server(conn, cfg).Handshake()` + un `GetCertificate` para SNI).
Sirve como referencia conceptual, no como código.

### Layering en el cable

TLS es la capa MÁS externa. Se envuelve la `net.Conn` recién aceptada en
`tls.Server()` ANTES del `Peek`/señuelo actual (`handleConn`), así todo
lo de abajo (detección SSH, handshake real, udpgw, medición por uuid)
funciona sin cambios -- solo recibe una conn ya descifrada.

```
[ TLS 1.3 + SNI ]  ← nuevo, opcional por nodo, REEMPLAZA al señuelo
     └─ [ SSH real / udpgw ]  ← ya existe, intacto
```

Cuando un nodo está en modo TLS, NO se corre el señuelo HTTP-like (son
modos mutuamente excluyentes por nodo, no se apilan).

### Certificados: self-signed, sin validación del cliente

Decisión tomada: el cliente conecta en modo **inseguro
(`allowInsecure`/`InsecureSkipVerify`)**, NO valida la cadena. Por lo
tanto GPM NO necesita dominio real, ni Let's Encrypt, ni gestión de
certs. Genera un self-signed él mismo, igual que ya hace con la host key
SSH (`loadOrCreateHostKey`).

Estrategia elegida: **cert self-signed por SNI, generado al vuelo y
cacheado** vía `tls.Config.GetCertificate`. Cuando llega un ClientHello
con `ServerName: X`, GPM emite en el momento un leaf con `SAN=X` (firmado
por una CA self-signed interna persistida en `/etc/gpm/<id>/`) y lo
cachea en memoria. Mejor camuflaje que un cert fijo: si un DPI compara el
SNI del ClientHello contra el `SAN` del certificado, coinciden. Costo
operativo cero (no hay que conseguir cert para cada dominio-carnada).

### Contrato del link de suscripción (reusar convención Xray)

El core del cliente iOS es un fork de **Xray-core**, que YA entiende
`streamSettings.security=tls` + `tlsSettings.serverName` +
`tlsSettings.allowInsecure`. **NO inventar params nuevos** (`tls=1&sni=`):
el panel emite el nodo SSH con esos `streamSettings` estándar, y el
cliente reusa su capa TLS existente conectada al outbound SSH, en vez de
escribir un parser nuevo. Concretamente el link/JSON del nodo lleva:

- `security: "tls"` (vs sin TLS = comportamiento actual con señuelo)
- `serverName: <dominio-SNI-carnada>` (lo que ve el DPI)
- `allowInsecure: true`

### Responsabilidades por componente

- **GPM (este repo):** envolver en `tls.Server()` en el accept loop de
  `Run` cuando el nodo esté en modo TLS; `GetCertificate` self-signed por
  SNI + CA persistida; nuevo campo en `config.json` (`"tls": true` o
  bloque); sincronizar el flag desde el panel EN CALIENTE con el mismo
  patrón atómico que `behind_cdn` (cambio no reinicia el listener). El
  campo `sni` que ya existe reservado en el panel encaja acá.
- **Panel (`v2board_mod`):** guardar el flag TLS + el SNI en la tabla del
  nodo GPM; exponerlo en `GET /api/v2/server/config` (junto a
  `server_port`/`behind_cdn`) para que GPM lo lea; emitir el nodo en la
  suscripción con `streamSettings.security=tls` + `serverName` +
  `allowInsecure` según lo de arriba.
- **Cliente core (Android exclave-core / iOS Xray-core):** conectar la
  capa TLS existente del stream al outbound SSH cuando `security=tls`;
  `allowInsecure=true`; mandar el SNI en el ClientHello. Cuidado con el
  decoder Swift (ver sección iOS): cualquier campo nuevo que se agregue
  al JSON del nodo tiene que ser opcional o venir siempre presente, o
  rompe la decodificación de TODO el array de outbounds.

### Orden de trabajo acordado

1. Este contrato (hecho, en este archivo).
2. GPM server (autónomo, no depende de nadie -- testeable con
   `openssl s_client -servername` o un cliente Go mínimo).
3. Cliente core (long pole, prueba contra el GPM del paso 2).
4. Panel (capa más fina, depende del formato final del link).

## Pendiente / conocido

- **Nodo de AWS con `node_id` compartido con producción**: un servidor de
  prueba nuevo (ver `LOCAL_NOTES.md`) quedó instalado con el mismo
  `node_id` que el nodo de producción de Telcel MX -- comparten usuarios y
  config de CDN sin querer. El panel lo tiene registrado como un nodo
  separado ("GPM-test", su propio `node_id`); falta corregir el
  `config.json` de ese servidor para que use el ID correcto en vez del de
  producción.
- **Campo `sni` / capa TLS (stunnel embebido)**: contrato acordado entre
  las tres sesiones (ver sección "stunnel embebido (TLS + SNI)" arriba),
  todavía SIN implementar en ningún lado. El campo `sni` del panel se
  reusa para esto.
- **`UniProxy/alive`/`alivelist`** (reporte de usuarios online) --
  implementado del lado panel (`CacheKey SERVER_GPM_ONLINE_USER`), no
  implementado del lado GPM todavía.
