#!/bin/bash
# ============================================================
# EdgeLite Gateway (Go Edition) - 一键安装脚本
# 用法: sudo bash install.sh [--port 8080] [--dir /opt/edgelite]
# 适用: Linux amd64 / arm64 / arm32(ARMv7), systemd 或 sysvinit
# ============================================================
set -euo pipefail

APP_NAME="edgelite"
INSTALL_DIR="/opt/edgelite"
PORT="8080"
SERVICE_USER="edgelite"

# ---------- 解析参数 ----------
while [[ $# -gt 0 ]]; do
    case $1 in
        --port)  PORT="$2"; shift 2 ;;
        --dir)   INSTALL_DIR="$2"; shift 2 ;;
        --help|-h)
            echo "用法: sudo bash install.sh [--port 8080] [--dir /opt/edgelite]"
            exit 0 ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "错误: 请使用 root 权限运行 (sudo bash install.sh)"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_SRC="$SCRIPT_DIR/edgelite"
FRONTEND_SRC="$SCRIPT_DIR/frontend"

if [[ ! -f "$BIN_SRC" ]]; then
    echo "错误: 未找到二进制文件 $BIN_SRC，请在解压后的发布包目录内执行"
    exit 1
fi

echo "=========================================="
echo "  EdgeLite Gateway 安装程序"
echo "  安装目录: $INSTALL_DIR"
echo "  服务端口: $PORT"
echo "=========================================="

# ---------- 1. 停止旧服务（覆盖升级场景） ----------
echo "[1/7] 停止旧服务..."
if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q "^${APP_NAME}.service"; then
    systemctl stop "$APP_NAME" 2>/dev/null || true
elif [ -f /etc/init.d/"$APP_NAME" ]; then
    /etc/init.d/"$APP_NAME" stop 2>/dev/null || true
elif pgrep -x "$APP_NAME" >/dev/null 2>&1; then
    pkill -x "$APP_NAME" || true
    sleep 1
fi

# ---------- 2. 创建目录与服务用户 ----------
echo "[2/7] 创建目录与服务用户..."
mkdir -p "$INSTALL_DIR"/{configs,data,logs,frontend}
if id "$SERVICE_USER" >/dev/null 2>&1; then
    echo "  用户 $SERVICE_USER 已存在"
else
    # 嵌入式系统可能没有 useradd，降级用 adduser 或直接用 root 运行
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null \
            || useradd --system --no-create-home --shell /bin/false "$SERVICE_USER" 2>/dev/null \
            || SERVICE_USER="root"
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -H -s /bin/false "$SERVICE_USER" 2>/dev/null || SERVICE_USER="root"
    else
        SERVICE_USER="root"
    fi
    echo "  服务用户: $SERVICE_USER"
fi

# ---------- 3. 安装二进制与前端 ----------
echo "[3/7] 安装二进制与前端资源..."
install -m 755 "$BIN_SRC" "$INSTALL_DIR/edgelite"
if [ -d "$FRONTEND_SRC" ] && [ -f "$FRONTEND_SRC/index.html" ]; then
    # 程序默认在前端目录下找 dist 子目录（frontend/dist），照此布局安装双保险
    mkdir -p "$INSTALL_DIR/frontend/dist"
    cp -r "$FRONTEND_SRC"/. "$INSTALL_DIR/frontend/dist/"
    echo "  前端资源已安装"
else
    echo "  警告: 发布包内未包含前端资源，将只提供 API 服务" >&2
fi

# ---------- 4. 安装配置 ----------
echo "[4/7] 安装配置文件..."
# 发布包内配置位于 configs/config.yaml（兼容包根目录 config.yaml 的旧布局）
if [ -f "$SCRIPT_DIR/configs/config.yaml" ]; then
    PKG_CONFIG="$SCRIPT_DIR/configs/config.yaml"
else
    PKG_CONFIG="$SCRIPT_DIR/config.yaml"
fi
if [ -f "$INSTALL_DIR/configs/config.yaml" ]; then
    # 保留现有配置，新配置放到 .new 供对比
    if [ -f "$PKG_CONFIG" ]; then
        cp "$PKG_CONFIG" "$INSTALL_DIR/configs/config.yaml.new"
        echo "  检测到已有配置，新版本配置保存为 config.yaml.new"
    fi
else
    if [ -f "$PKG_CONFIG" ]; then
        mkdir -p "$INSTALL_DIR/configs"
        cp "$PKG_CONFIG" "$INSTALL_DIR/configs/config.yaml"
        echo "  已安装默认配置 configs/config.yaml"
    else
        echo "  警告: 未找到默认配置文件，服务将使用程序内置默认值" >&2
    fi
fi

# 通过环境变量覆盖监听地址与端口（不污染用户配置文件）
ENV_FILE="$INSTALL_DIR/edgelite.env"
cat > "$ENV_FILE" <<EOF
EDGELITE_SERVER__HOST=0.0.0.0
EDGELITE_SERVER__PORT=$PORT
EDGELITE_FRONTEND_DIST=$INSTALL_DIR/frontend/dist
EDGELITE_CONFIG=$INSTALL_DIR/configs/config.yaml
EOF
chmod 600 "$ENV_FILE"

# ---------- 5. 目录权限（必须在服务启动前完成，否则非 root 运行时数据目录无写权限导致启动失败） ----------
echo "[5/7] 设置目录权限..."
chown -R "$SERVICE_USER":"$SERVICE_USER" "$INSTALL_DIR" 2>/dev/null || true
chmod 755 "$INSTALL_DIR/edgelite"

# ---------- 6. 配置系统服务 ----------
echo "[6/7] 配置系统服务..."
if command -v systemctl >/dev/null 2>&1 && [ -d /etc/systemd/system ]; then
    cat > /etc/systemd/system/"$APP_NAME".service <<EOF
[Unit]
Description=EdgeLite Gateway (Go Edition)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$ENV_FILE
ExecStart=$INSTALL_DIR/edgelite --config $INSTALL_DIR/configs/config.yaml
Restart=always
RestartSec=5
# 资源限制: 内存紧张时保护系统（512MB 设备建议不超过 256MB）
MemoryMax=256M
LimitNOFILE=65536
# 稳定化: 禁止内存过量分配导致 OOM killer 误杀
OOMScoreAdjust=-500

[Install]
WantedBy=multi-user.target
EOF
    # sysvinit 兼容: systemd 环境下不需要 init.d 脚本
    rm -f /etc/init.d/"$APP_NAME"
    systemctl daemon-reload
    systemctl enable "$APP_NAME".service
    systemctl start "$APP_NAME".service
    START_MODE="systemd"
else
    # sysvinit / BusyBox init 回退方案（嵌入式 Linux 4.1 常见）
    cat > /etc/init.d/"$APP_NAME" <<EOF
#!/bin/sh
### BEGIN INIT INFO
# Provides:          $APP_NAME
# Required-Start:    \$network \$remote_fs
# Required-Stop:     \$network \$remote_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: EdgeLite Gateway
### END INIT INFO
DAEMON=$INSTALL_DIR/edgelite
PIDFILE=/var/run/$APP_NAME.pid
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
export EDGELITE_SERVER__HOST EDGELITE_SERVER__PORT EDGELITE_FRONTEND_DIST EDGELITE_CONFIG

case "\$1" in
    start)
        echo "Starting $APP_NAME..."
        # 工作目录必须是安装目录：程序用相对路径读写 data/ logs/ configs/
        # 注意：start-stop-daemon 默认将守护进程 cwd 改为 /，必须用 -d 显式指定
        cd $INSTALL_DIR || exit 1
        start-stop-daemon -S -b -m -d $INSTALL_DIR -p \$PIDFILE -x \$DAEMON -- --config $INSTALL_DIR/configs/config.yaml \
            || \$DAEMON --config $INSTALL_DIR/configs/config.yaml >/dev/null 2>&1 &
        ;;
    stop)
        echo "Stopping $APP_NAME..."
        if [ -f \$PIDFILE ]; then
            kill \$(cat \$PIDFILE) 2>/dev/null || true
            rm -f \$PIDFILE
        else
            pkill -x $APP_NAME 2>/dev/null || true
        fi
        ;;
    restart)
        \$0 stop
        sleep 2
        \$0 start
        ;;
    status)
        if pgrep -x $APP_NAME >/dev/null 2>&1; then
            echo "$APP_NAME is running"
        else
            echo "$APP_NAME is stopped"
        fi
        ;;
    *)
        echo "Usage: \$0 {start|stop|restart|status}"
        exit 1
        ;;
esac
exit 0
EOF
    chmod 755 /etc/init.d/"$APP_NAME"
    # 尝试注册自启动（update-rc.d 或 rc.d；BusyBox 直接靠 init.d 默认运行级）
    if command -v update-rc.d >/dev/null 2>&1; then
        update-rc.d "$APP_NAME" defaults
    elif command -v chkconfig >/dev/null 2>&1; then
        chkconfig --add "$APP_NAME" 2>/dev/null || true
    fi
    /etc/init.d/"$APP_NAME" start || true
    START_MODE="sysvinit"
fi

# ---------- 7. 健康检查 ----------
echo "[7/7] 启动健康检查..."
HEALTH_OK=0
for i in $(seq 1 15); do
    # 系统无免认证健康端点，用 Web 根路径探测：任何 HTTP 响应都证明服务已监听
    if command -v curl >/dev/null 2>&1; then
        HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$PORT/" 2>/dev/null || echo "000")
    else
        HTTP_CODE=$(wget -q -O /dev/null --server-response "http://127.0.0.1:$PORT/" 2>&1 | awk '/HTTP\//{print $2; exit}' 2>/dev/null || echo "000")
    fi
    if [ "$HTTP_CODE" != "000" ] && [ -n "$HTTP_CODE" ]; then
        HEALTH_OK=1
        break
    fi
    sleep 2
done

echo ""
if [ "$HEALTH_OK" = "1" ]; then
    echo "=========================================="
    echo "  安装成功! 服务已启动 ($START_MODE)"
    echo "=========================================="
else
    echo "=========================================="
    echo "  安装完成，但健康检查未通过 (服务可能仍在启动)"
    echo "  查看日志: journalctl -u $APP_NAME -f  (systemd)"
    echo "            tail -f $INSTALL_DIR/logs/*.log (文件日志)"
    echo "=========================================="
fi
echo ""
echo "  访问地址:  http://<设备IP>:$PORT"
echo "  默认账号:  admin / admin123 (首次登录后请立即修改!)"
echo ""
echo "  服务管理:  systemctl {start|stop|restart|status} $APP_NAME"
echo "             或 /etc/init.d/$APP_NAME {start|stop|restart|status}"
echo "  卸载:      sudo bash $(dirname "$0")/uninstall.sh"
