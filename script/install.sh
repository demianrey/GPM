#!/bin/bash
# Instalador de GPM -- descarga el binario del último release de GitHub y
# da de alta un NODO nuevo, identificado por su node_id del panel (o un
# nombre corto en modo manual). Varios nodos pueden convivir en el mismo
# VPS: cada uno es su propia carpeta /etc/gpm/<id>/ + su propia instancia
# systemd (gpm@<id>.service), compartiendo el mismo binario.
# Mismo patrón que github.com/wyx2685/v2node/script/install.sh.

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

[[ $EUID -ne 0 ]] && echo -e "${red}Error:${plain} corre este script como root\n" && exit 1

arch=$(uname -m)
case "$arch" in
    x86_64|x64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo -e "${red}Arquitectura no soportada: ${arch}${plain} (solo amd64/arm64)\n" && exit 1 ;;
esac

if ! command -v systemctl >/dev/null 2>&1; then
    echo -e "${red}Este instalador requiere systemd.${plain}\n" && exit 1
fi

REPO="demianrey/GPM"
BIN_PATH="/usr/local/bin/gpm"
TEMPLATE_PATH="/etc/systemd/system/gpm@.service"

echo -e "${green}Descargando GPM (linux-${arch})...${plain}"
if ! curl -fsSL -o "${BIN_PATH}.new" "https://github.com/${REPO}/releases/latest/download/gpm-linux-${arch}"; then
    echo -e "${red}No se pudo descargar el binario.${plain}"
    echo -e "${yellow}¿Ya existe un release publicado? (git tag vX.Y.Z && git push --tags dispara el build)${plain}\n"
    exit 1
fi
mv "${BIN_PATH}.new" "$BIN_PATH"
chmod +x "$BIN_PATH"

curl -fsSL -o /usr/local/bin/gpm-cli "https://raw.githubusercontent.com/${REPO}/main/script/gpm.sh" \
    && chmod +x /usr/local/bin/gpm-cli

cat > "$TEMPLATE_PATH" <<'EOF'
[Unit]
Description=GPM (Go Payload Multiplexer) -- nodo %i
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
LimitNOFILE=999999
WorkingDirectory=/etc/gpm/%i
ExecStart=/usr/local/bin/gpm serve -config /etc/gpm/%i/config.json
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload

echo
echo -e "${green}Binario y plantilla systemd instalados.${plain} Ahora demos de alta un nodo."
echo

echo "Modo de usuarios:"
echo "  1) Panel v2board (recomendado, ver README del repo)"
echo "  2) Manual (archivo users.json local)"
read -rp "Elige [1]: " gpm_mode
gpm_mode=${gpm_mode:-1}

if [[ "$gpm_mode" == "2" ]]; then
    read -rp "Nombre corto para este nodo (ej. manual1): " node_id
else
    read -rp "ID del nodo GPM en el panel: " node_id
fi
if [[ -z "$node_id" ]]; then
    echo -e "${red}Necesito un id/nombre.${plain}" && exit 1
fi

NODE_DIR="/etc/gpm/${node_id}"
if [[ -f "${NODE_DIR}/config.json" ]]; then
    echo -e "${yellow}Ya existe un nodo con ese id en ${NODE_DIR} -- se conserva tal cual. Usa \"gpm-cli config ${node_id}\" para editarlo.${plain}"
    systemctl enable "gpm@${node_id}" >/dev/null 2>&1
    systemctl restart "gpm@${node_id}"
    exit 0
fi
mkdir -p "$NODE_DIR"

echo
echo "Si en este VPS hay un nginx (u otro reverse proxy) ocupando el puerto al"
echo "que conecta el cliente, GPM escucha en loopback detrás de él y ese puerto"
echo "se carga en el panel como PUERTO DE SERVICIO (service_port), distinto del"
echo "puerto de conexión. Ver \"Detrás de un reverse proxy\" en el README."
read -rp "¿Hay un reverse proxy local delante de GPM? [y/N]: " gpm_rp
if [[ "$gpm_rp" == "y" || "$gpm_rp" == "Y" ]]; then
    gpm_behind_rp="yes"
else
    gpm_behind_rp="no"
fi

if [[ "$gpm_behind_rp" == "yes" ]]; then
    read -rp "Puerto de SERVICIO donde escuchar (el que apunta el reverse proxy) [18080]: " gpm_port
    gpm_port=${gpm_port:-18080}
    gpm_addr="127.0.0.1:${gpm_port}"
else
    read -rp "Puerto donde escuchar [80]: " gpm_port
    gpm_port=${gpm_port:-80}
    gpm_addr=":${gpm_port}"
fi
read -rp "Dirección udpgw embebida [127.0.0.1:7300, vacío para desactivar]: " udpgw_addr
if [[ "$gpm_behind_rp" == "yes" ]]; then
    # Detrás de un reverse proxy el señuelo TIENE que responder 101: nginx solo
    # pasa a modo túnel crudo si ve ese status (con 200 da la petición por
    # terminada y no relaya nada). O sea el proxy cumple el mismo rol que un
    # CDN, así que no se pregunta -- se fija en true.
    gpm_cdn_json="true"
    echo -e "${yellow}Modo señuelo: 101 (fijo). Detrás de un reverse proxy es obligatorio -- nginx necesita ese status para tunelizar.${plain}"
else
    read -rp "¿Este nodo está detrás de un CDN tipo Cloudflare? [Y/n]: " gpm_cdn
    if [[ "$gpm_cdn" == "n" || "$gpm_cdn" == "N" ]]; then
        gpm_cdn_json="false"
    else
        gpm_cdn_json="true"
    fi
fi

if [[ "$gpm_mode" == "2" ]]; then
    touch "${NODE_DIR}/users.json"
    [[ -s "${NODE_DIR}/users.json" ]] || echo '{}' > "${NODE_DIR}/users.json"
    cat > "${NODE_DIR}/config.json" <<EOF
{
  "addr": "${gpm_addr}",
  "hostkey": "${NODE_DIR}/host_key.pem",
  "udpgwAddr": "${udpgw_addr}",
  "cdn": ${gpm_cdn_json},
  "users": "${NODE_DIR}/users.json"
}
EOF
else
    read -rp "URL del panel (ej. https://tu-panel.com): " panel_url
    read -rsp "Communication Key del panel (no queda en pantalla): " panel_token
    echo
    umask 077
    echo -n "$panel_token" > "${NODE_DIR}/panel-token"
    chmod 600 "${NODE_DIR}/panel-token"
    cat > "${NODE_DIR}/config.json" <<EOF
{
  "addr": "${gpm_addr}",
  "hostkey": "${NODE_DIR}/host_key.pem",
  "udpgwAddr": "${udpgw_addr}",
  "cdn": ${gpm_cdn_json},
  "panel": {
    "url": "${panel_url}",
    "nodeId": ${node_id},
    "tokenFile": "${NODE_DIR}/panel-token"
  }
}
EOF
fi
chmod 600 "${NODE_DIR}/config.json"

systemctl enable "gpm@${node_id}" >/dev/null 2>&1
systemctl restart "gpm@${node_id}"

sleep 1
if [[ "$gpm_behind_rp" == "yes" ]]; then
    echo
    echo -e "${yellow}Reverse proxy: falta hacer dos cosas FUERA de este instalador:${plain}"
    echo -e "${yellow}  1) en el panel, cargar ${gpm_port} como puerto de servicio (service_port) del nodo${plain}"
    echo -e "${yellow}     -- el puerto de conexión sigue siendo el que disca el cliente (el del proxy);${plain}"
    echo -e "${yellow}  2) en el reverse proxy, apuntar el location/upstream a 127.0.0.1:${gpm_port}${plain}"
    echo -e "${yellow}     con upgrade de WebSocket y timeouts largos (receta en el README).${plain}"
fi
if systemctl is-active --quiet "gpm@${node_id}"; then
    echo -e "\n${green}Nodo ${node_id} instalado y corriendo.${plain} Administralo con: ${green}gpm-cli ${node_id}${plain}"
else
    echo -e "\n${red}El nodo ${node_id} no arrancó.${plain} Revisa: journalctl -u gpm@${node_id} -e --no-pager"
fi
