#!/bin/bash
# Administración de GPM ya instalado -- mismo patrón que
# github.com/wyx2685/v2node/script/v2node.sh, adaptado a GPM (sin
# generación de config multi-protocolo, GPM se configura por flags/env).
# Se instala como /usr/local/bin/gpm-cli vía install.sh.

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

INSTALL_DIR="/usr/local/gpm"
ENV_PATH="$INSTALL_DIR/gpm.conf"
REPO="demianrey/GPM"

[[ $EUID -ne 0 ]] && echo -e "${red}Error:${plain} corre este script como root\n" && exit 1

check_install() {
    if [[ ! -f /etc/systemd/system/gpm.service ]]; then
        echo -e "${red}GPM no está instalado.${plain} Corre: bash <(curl -Ls https://raw.githubusercontent.com/${REPO}/main/script/install.sh)"
        return 1
    fi
    return 0
}

before_show_menu() {
    echo && read -rp "Presiona enter para volver al menú: " temp
    show_menu
}

start() {
    systemctl start gpm
    sleep 1
    status_line
    [[ $# == 0 ]] && before_show_menu
}

stop() {
    systemctl stop gpm
    echo -e "${green}GPM detenido.${plain}"
    [[ $# == 0 ]] && before_show_menu
}

restart() {
    systemctl restart gpm
    sleep 1
    status_line
    [[ $# == 0 ]] && before_show_menu
}

status_line() {
    if systemctl is-active --quiet gpm; then
        echo -e "Estado: ${green}corriendo${plain}"
    else
        echo -e "Estado: ${red}detenido${plain}"
    fi
}

status() {
    systemctl status gpm --no-pager -l
    [[ $# == 0 ]] && before_show_menu
}

show_log() {
    echo -e "${yellow}Ctrl+C para salir del log en vivo.${plain}"
    journalctl -u gpm.service -e --no-pager -f
    [[ $# == 0 ]] && before_show_menu
}

enable() {
    systemctl enable gpm
    echo -e "${green}Arranque automático habilitado.${plain}"
    [[ $# == 0 ]] && before_show_menu
}

disable() {
    systemctl disable gpm
    echo -e "${yellow}Arranque automático deshabilitado.${plain}"
    [[ $# == 0 ]] && before_show_menu
}

config() {
    echo "GPM va a reiniciarse después de editar la configuración."
    ${EDITOR:-vi} "$ENV_PATH"
    sleep 1
    systemctl restart gpm
    status_line
    [[ $# == 0 ]] && before_show_menu
}

update() {
    bash <(curl -Ls "https://raw.githubusercontent.com/${REPO}/main/script/install.sh")
    [[ $# == 0 ]] && before_show_menu
}

uninstall() {
    read -rp "¿Seguro que quieres desinstalar GPM? [y/N]: " confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
        before_show_menu
        return
    fi
    systemctl stop gpm 2>/dev/null
    systemctl disable gpm 2>/dev/null
    rm -f /etc/systemd/system/gpm.service
    systemctl daemon-reload
    systemctl reset-failed 2>/dev/null
    echo -e "${yellow}Servicio eliminado. Los archivos en ${INSTALL_DIR} (incluido el token del panel) NO se borraron -- bórralos a mano si ya no los necesitas:${plain}"
    echo "  rm -rf ${INSTALL_DIR} /usr/local/bin/gpm-cli"
    [[ $# == 0 ]] && exit 0
}

show_version() {
    "$INSTALL_DIR/gpm" -h 2>&1 | head -1
    [[ $# == 0 ]] && before_show_menu
}

show_menu() {
    echo -e "
  ${green}Administración de GPM${plain}
--- https://github.com/${REPO} ---
  ${green}0.${plain} Editar configuración (reinicia solo)
————————————————
  ${green}1.${plain} Instalar / actualizar GPM
  ${green}2.${plain} Desinstalar GPM
————————————————
  ${green}3.${plain} Iniciar GPM
  ${green}4.${plain} Detener GPM
  ${green}5.${plain} Reiniciar GPM
  ${green}6.${plain} Ver estado
  ${green}7.${plain} Ver logs en vivo
————————————————
  ${green}8.${plain} Habilitar arranque automático
  ${green}9.${plain} Deshabilitar arranque automático
————————————————
  ${green}10.${plain} Salir
 "
    check_install && status_line
    echo && read -rp "Elige una opción [0-10]: " num
    case "${num}" in
        0) check_install && config ;;
        1) update ;;
        2) check_install && uninstall ;;
        3) check_install && start ;;
        4) check_install && stop ;;
        5) check_install && restart ;;
        6) check_install && status ;;
        7) check_install && show_log ;;
        8) check_install && enable ;;
        9) check_install && disable ;;
        10) exit 0 ;;
        *) echo -e "${red}Opción inválida${plain}" && before_show_menu ;;
    esac
}

if [[ $# -gt 0 ]]; then
    case "$1" in
        start) check_install && start 0 ;;
        stop) check_install && stop 0 ;;
        restart) check_install && restart 0 ;;
        status) check_install && status 0 ;;
        log) check_install && show_log 0 ;;
        enable) check_install && enable 0 ;;
        disable) check_install && disable 0 ;;
        config) check_install && config 0 ;;
        update) update 0 ;;
        uninstall) check_install && uninstall 0 ;;
        version) check_install && show_version 0 ;;
        *) echo "Uso: gpm-cli [start|stop|restart|status|log|enable|disable|config|update|uninstall|version]" ;;
    esac
else
    show_menu
fi
