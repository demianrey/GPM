#!/bin/bash
# Administración de GPM ya instalado -- soporta varios nodos en el mismo
# VPS, cada uno identificado por su node_id del panel (o nombre corto en
# modo manual): /etc/gpm/<id>/config.json + servicio gpm@<id>.service.
# Mismo patrón que github.com/wyx2685/v2node/script/v2node.sh.
# Se instala como /usr/local/bin/gpm-cli vía install.sh.

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

GPM_DIR="/etc/gpm"
REPO="demianrey/GPM"

[[ $EUID -ne 0 ]] && echo -e "${red}Error:${plain} corre este script como root\n" && exit 1

list_nodes() {
    [[ -d "$GPM_DIR" ]] || return 0
    for d in "$GPM_DIR"/*/; do
        [[ -f "${d}config.json" ]] && basename "$d"
    done
}

# Resuelve qué nodo usar: si se dio explícito lo valida, si no y hay uno
# solo lo usa solo, si hay varios y no se dio pide elegir.
resolve_node() {
    local given="$1"
    local nodes
    nodes=$(list_nodes)
    if [[ -z "$nodes" ]]; then
        echo -e "${red}No hay ningún nodo GPM instalado.${plain} Corre: bash <(curl -Ls https://raw.githubusercontent.com/${REPO}/main/script/install.sh)" >&2
        return 1
    fi
    if [[ -n "$given" ]]; then
        if [[ -f "${GPM_DIR}/${given}/config.json" ]]; then
            echo "$given"
            return 0
        fi
        echo -e "${red}No existe el nodo '${given}'.${plain} Nodos instalados:" >&2
        echo "$nodes" >&2
        return 1
    fi
    local count
    count=$(echo "$nodes" | wc -l | tr -d ' ')
    if [[ "$count" == "1" ]]; then
        echo "$nodes"
        return 0
    fi
    echo -e "${yellow}Hay varios nodos instalados, elige uno:${plain}" >&2
    select n in $nodes; do
        [[ -n "$n" ]] && echo "$n" && return 0
        echo "Opción inválida." >&2
    done
}

svc() { echo "gpm@${1}"; }

before_show_menu() {
    echo && read -rp "Presiona enter para volver al menú: " temp
    show_menu "$NODE"
}

status_line() {
    if systemctl is-active --quiet "$(svc "$1")"; then
        echo -e "Estado: ${green}corriendo${plain}"
    else
        echo -e "Estado: ${red}detenido${plain}"
    fi
}

start() {
    systemctl start "$(svc "$1")"
    sleep 1
    status_line "$1"
    [[ $# -lt 2 ]] && before_show_menu
}

stop() {
    systemctl stop "$(svc "$1")"
    echo -e "${green}Nodo ${1} detenido.${plain}"
    [[ $# -lt 2 ]] && before_show_menu
}

restart() {
    systemctl restart "$(svc "$1")"
    sleep 1
    status_line "$1"
    [[ $# -lt 2 ]] && before_show_menu
}

status() {
    systemctl status "$(svc "$1")" --no-pager -l
    [[ $# -lt 2 ]] && before_show_menu
}

show_log() {
    echo -e "${yellow}Ctrl+C para salir del log en vivo.${plain}"
    journalctl -u "$(svc "$1").service" -e --no-pager -f
    [[ $# -lt 2 ]] && before_show_menu
}

enable() {
    systemctl enable "$(svc "$1")"
    echo -e "${green}Arranque automático habilitado para el nodo ${1}.${plain}"
    [[ $# -lt 2 ]] && before_show_menu
}

disable() {
    systemctl disable "$(svc "$1")"
    echo -e "${yellow}Arranque automático deshabilitado para el nodo ${1}.${plain}"
    [[ $# -lt 2 ]] && before_show_menu
}

config() {
    local path="${GPM_DIR}/${1}/config.json"
    echo "GPM va a reiniciarse después de editar la configuración del nodo ${1}."
    ${EDITOR:-nano} "$path"
    sleep 1
    systemctl restart "$(svc "$1")"
    status_line "$1"
    [[ $# -lt 2 ]] && before_show_menu
}

token() {
    local path="${GPM_DIR}/${1}/panel-token"
    if [[ ! -f "$path" ]]; then
        echo -e "${red}Este nodo no usa modo panel (no tiene panel-token).${plain}"
        [[ $# -lt 2 ]] && before_show_menu
        return
    fi
    echo "Communication Key actual (nodo ${1}):"
    cat "$path"; echo
    echo
    read -rsp "Nuevo valor (enter para dejar igual): " newtok
    echo
    if [[ -n "$newtok" ]]; then
        umask 077
        echo -n "$newtok" > "$path"
        chmod 600 "$path"
        echo -e "${green}Token actualizado.${plain} Reiniciando nodo ${1}..."
        systemctl restart "$(svc "$1")"
        status_line "$1"
    fi
    [[ $# -lt 2 ]] && before_show_menu
}

list_cmd() {
    local nodes
    nodes=$(list_nodes)
    if [[ -z "$nodes" ]]; then
        echo "(sin nodos instalados)"
    else
        echo -e "${green}Nodos instalados:${plain}"
        while read -r n; do
            printf "  %-10s " "$n"
            status_line "$n"
        done <<< "$nodes"
    fi
    [[ $# == 0 ]] && before_show_menu
}

add_node() {
    bash <(curl -Ls "https://raw.githubusercontent.com/${REPO}/main/script/install.sh")
    [[ $# == 0 ]] && before_show_menu
}

update() {
    curl -fsSL -o /usr/local/bin/gpm.new "https://github.com/${REPO}/releases/latest/download/gpm-linux-$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')" \
        && mv /usr/local/bin/gpm.new /usr/local/bin/gpm && chmod +x /usr/local/bin/gpm \
        && echo -e "${green}Binario actualizado.${plain} Reinicia cada nodo (gpm-cli restart <id>) para aplicar." \
        || echo -e "${red}Falló la descarga.${plain}"
    [[ $# == 0 ]] && before_show_menu
}

uninstall_node() {
    read -rp "¿Seguro que quieres desinstalar el nodo ${1}? [y/N]: " confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
        [[ $# -lt 2 ]] && before_show_menu
        return
    fi
    systemctl stop "$(svc "$1")" 2>/dev/null
    systemctl disable "$(svc "$1")" 2>/dev/null
    systemctl reset-failed 2>/dev/null
    echo -e "${yellow}Servicio del nodo ${1} detenido y deshabilitado. Sus archivos (incluido el token) NO se borraron -- bórralos a mano si ya no los necesitas:${plain}"
    echo "  rm -rf ${GPM_DIR}/${1}"
    [[ $# -lt 2 ]] && exit 0
}

show_version() {
    /usr/local/bin/gpm -h 2>&1 | head -1
    [[ $# == 0 ]] && before_show_menu
}

show_menu() {
    NODE=$(resolve_node "$1") || exit 1
    echo -e "
  ${green}Administración de GPM -- nodo ${NODE}${plain}
--- https://github.com/${REPO} ---
  ${green}0.${plain} Editar configuración de este nodo (reinicia solo)
  ${green}t.${plain} Ver/cambiar Communication Key de este nodo
————————————————
  ${green}1.${plain} Agregar otro nodo
  ${green}l.${plain} Listar nodos instalados
  ${green}u.${plain} Actualizar binario de GPM
  ${green}2.${plain} Desinstalar este nodo
————————————————
  ${green}3.${plain} Iniciar
  ${green}4.${plain} Detener
  ${green}5.${plain} Reiniciar
  ${green}6.${plain} Ver estado
  ${green}7.${plain} Ver logs en vivo
————————————————
  ${green}8.${plain} Habilitar arranque automático
  ${green}9.${plain} Deshabilitar arranque automático
————————————————
  ${green}10.${plain} Salir
 "
    status_line "$NODE"
    echo && read -rp "Elige una opción [0-10]: " num
    case "${num}" in
        0) config "$NODE" ;;
        t) token "$NODE" ;;
        1) add_node ;;
        l) list_cmd ;;
        u) update ;;
        2) uninstall_node "$NODE" ;;
        3) start "$NODE" ;;
        4) stop "$NODE" ;;
        5) restart "$NODE" ;;
        6) status "$NODE" ;;
        7) show_log "$NODE" ;;
        8) enable "$NODE" ;;
        9) disable "$NODE" ;;
        10) exit 0 ;;
        *) echo -e "${red}Opción inválida${plain}" && before_show_menu ;;
    esac
}

if [[ $# -gt 0 ]]; then
    case "$1" in
        list) list_cmd 0 ;;
        add) add_node 0 ;;
        update) update 0 ;;
        start) node=$(resolve_node "$2") && start "$node" 0 ;;
        stop) node=$(resolve_node "$2") && stop "$node" 0 ;;
        restart) node=$(resolve_node "$2") && restart "$node" 0 ;;
        status) node=$(resolve_node "$2") && status "$node" 0 ;;
        log) node=$(resolve_node "$2") && show_log "$node" 0 ;;
        enable) node=$(resolve_node "$2") && enable "$node" 0 ;;
        disable) node=$(resolve_node "$2") && disable "$node" 0 ;;
        config) node=$(resolve_node "$2") && config "$node" 0 ;;
        token) node=$(resolve_node "$2") && token "$node" 0 ;;
        uninstall) node=$(resolve_node "$2") && uninstall_node "$node" 0 ;;
        version) show_version 0 ;;
        *)
            node=$(resolve_node "$1" 2>/dev/null) && show_menu "$node" && exit 0
            echo "Uso: gpm-cli [list|add|update|start|stop|restart|status|log|enable|disable|config|token|uninstall|version] [node_id]"
            echo "Sin argumentos: menú interactivo. gpm-cli <node_id>: menú de ese nodo directamente."
            ;;
    esac
else
    show_menu
fi
