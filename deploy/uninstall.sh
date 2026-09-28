#!/bin/bash
# ============================================================
# EdgeLite Gateway (Go Edition) - 卸载脚本
# 用法: sudo bash uninstall.sh [--purge]  (--purge 连数据目录一起删除)
# ============================================================
set -euo pipefail

APP_NAME="edgelite"
INSTALL_DIR="/opt/edgelite"
PURGE=0

while [[ $# -gt 0 ]]; do
    case $1 in
        --purge) PURGE=1; shift ;;
        --help|-h) echo "用法: sudo bash uninstall.sh [--purge]"; exit 0 ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "错误: 请使用 root 权限运行 (sudo bash uninstall.sh)"
    exit 1
fi

echo "停止服务..."
if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q "^${APP_NAME}.service"; then
    systemctl stop "$APP_NAME" 2>/dev/null || true
    systemctl disable "$APP_NAME" 2>/dev/null || true
    rm -f /etc/systemd/system/"$APP_NAME".service
    systemctl daemon-reload
fi
if [ -f /etc/init.d/"$APP_NAME" ]; then
    /etc/init.d/"$APP_NAME" stop 2>/dev/null || true
    if command -v update-rc.d >/dev/null 2>&1; then
        update-rc.d -f "$APP_NAME" remove 2>/dev/null || true
    fi
    rm -f /etc/init.d/"$APP_NAME"
fi
pkill -x "$APP_NAME" 2>/dev/null || true

echo "删除程序文件..."
rm -f "$INSTALL_DIR/edgelite" /var/run/"$APP_NAME".pid

if [ "$PURGE" = "1" ]; then
    echo "删除数据与配置 ($INSTALL_DIR)..."
    rm -rf "$INSTALL_DIR"
else
    echo "保留数据与配置目录: $INSTALL_DIR (使用 --purge 一并删除)"
fi

if id "$SERVICE_USER" >/dev/null 2>&1 && [ "$PURGE" = "1" ]; then
    userdel "$SERVICE_USER" 2>/dev/null || true
fi

echo "卸载完成。"
