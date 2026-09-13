#!/bin/bash
# Instalador de GPM -- descarga el binario del último release de GitHub,
# escribe el archivo de entorno y el servicio systemd, y arranca.
# Mismo patrón que usa v2node (github.com/wyx2685/v2node/script/install.sh):
# este script hace la instalación de una vez; el manejo diario (start/stop/
# restart/logs) queda en gpm.sh, que se instala como "gpm-cli".

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

INSTALL_DIR="/usr/local/gpm"
BIN_PATH="$INSTALL_DIR/gpm"
ENV_PATH="$INSTALL_DIR/gpm.conf"
SERVICE_PATH="/etc/systemd/system/gpm.service"
REPO="demianrey/GPM"

mkdir -p "$INSTALL_DIR"

echo -e "${green}Descargando GPM (linux-${arch})...${plain}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/gpm-linux-${arch}"
if ! curl -fsSL -o "$BIN_PATH.new" "$DOWNLOAD_URL"; then
    echo -e "${red}No se pudo descargar ${DOWNLOAD_URL}${plain}"
    echo -e "${yellow}¿Ya existe un release publicado? (git tag vX.Y.Z && git push --tags dispara el build)${plain}\n"
    exit 1
fi
mv "$BIN_PATH.new" "$BIN_PATH"
chmod +x "$BIN_PATH"

# --- Configuración interactiva (se salta si ya existe gpm.conf) ---
if [[ ! -f "$ENV_PATH" ]]; then
    echo
    echo -e "${green}Configuración inicial de GPM${plain}"
    read -rp "Puerto donde escuchar [80]: " gpm_addr_port
    gpm_addr_port=${gpm_addr_port:-80}

    echo
    echo "Modo de usuarios:"
    echo "  1) Panel v2board (recomendado, ver README del repo)"
    echo "  2) Manual (archivo users.json local)"
    read -rp "Elige [1]: " gpm_mode
    gpm_mode=${gpm_mode:-1}

    GPM_ARGS="-addr :${gpm_addr_port} -hostkey ${INSTALL_DIR}/host_key.pem"

    if [[ "$gpm_mode" == "2" ]]; then
        GPM_ARGS="${GPM_ARGS} -users ${INSTALL_DIR}/users.json"
        touch "${INSTALL_DIR}/users.json"
        [[ -s "${INSTALL_DIR}/users.json" ]] || echo '{}' > "${INSTALL_DIR}/users.json"
    else
        read -rp "URL del panel (ej. https://tu-panel.com): " panel_url
        read -rp "ID del nodo GPM en el panel: " panel_node_id
        read -rsp "Communication Key del panel (no queda en pantalla): " panel_token
        echo
        umask 077
        echo -n "$panel_token" > "${INSTALL_DIR}/panel-token"
        chmod 600 "${INSTALL_DIR}/panel-token"
        GPM_ARGS="${GPM_ARGS} -panel-url ${panel_url} -panel-node-id ${panel_node_id} -panel-token-file ${INSTALL_DIR}/panel-token"
    fi

    read -rp "Dirección udpgw embebida [127.0.0.1:7300, vacío para desactivar]: " udpgw_addr
    if [[ -n "$udpgw_addr" && "$udpgw_addr" != "127.0.0.1:7300" ]]; then
        GPM_ARGS="${GPM_ARGS} -udpgw-addr ${udpgw_addr}"
    fi

    cat > "$ENV_PATH" <<EOF
# Generado por install.sh -- edítalo con "gpm-cli config" o a mano y luego
# "gpm-cli restart". GPM_ARGS se pasa tal cual a "gpm serve".
GPM_ARGS="${GPM_ARGS}"
EOF
    chmod 600 "$ENV_PATH"
else
    echo -e "${yellow}Ya existe ${ENV_PATH}, se conserva la configuración actual.${plain}"
fi

cat > "$SERVICE_PATH" <<EOF
[Unit]
Description=GPM (Go Payload Multiplexer)
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
LimitNOFILE=999999
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${ENV_PATH}
ExecStart=${BIN_PATH} serve \$GPM_ARGS
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# Instala el script de administración como "gpm-cli" (mismo binario fuente
# que este install.sh, script/gpm.sh del repo).
curl -fsSL -o /usr/local/bin/gpm-cli "https://raw.githubusercontent.com/${REPO}/main/script/gpm.sh" \
    && chmod +x /usr/local/bin/gpm-cli

systemctl daemon-reload
systemctl enable gpm >/dev/null 2>&1
systemctl restart gpm

sleep 1
if systemctl is-active --quiet gpm; then
    echo -e "\n${green}GPM instalado y corriendo.${plain} Administralo con: ${green}gpm-cli${plain}"
else
    echo -e "\n${red}GPM no arrancó.${plain} Revisa: journalctl -u gpm -e --no-pager"
fi
