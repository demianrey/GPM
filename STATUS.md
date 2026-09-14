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

## Pendiente / conocido

- **Nodo de AWS con `node_id` compartido con producción**: un servidor de
  prueba nuevo (ver `LOCAL_NOTES.md`) quedó instalado con el mismo
  `node_id` que el nodo de producción de Telcel MX -- comparten usuarios y
  config de CDN sin querer. El panel lo tiene registrado como un nodo
  separado ("GPM-test", su propio `node_id`); falta corregir el
  `config.json` de ese servidor para que use el ID correcto en vez del de
  producción.
- **Campo `sni`** existe en el panel (reservado para una futura capa TLS)
  pero no implementado de este lado.
- **`UniProxy/alive`/`alivelist`** (reporte de usuarios online) --
  implementado del lado panel (`CacheKey SERVER_GPM_ONLINE_USER`), no
  implementado del lado GPM todavía.
