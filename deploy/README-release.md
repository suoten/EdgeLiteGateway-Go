# EdgeLite 网关 部署运维说明

工业物联网边缘网关（Go 版），南向多协议数据采集与协议解析转换，北向通过 MQTT 上送 IoT 平台。
本包为 **Linux ARMv7（32 位，适用 Cortex-A7/A8/A9）** 版本，静态编译，无需安装任何运行时依赖。

## 一、包内容

| 文件/目录 | 说明 |
|---|---|
| `edgelite` | 网关主程序（静态链接，解压即可运行） |
| `frontend/` | Web 管理控制台（Vue3 构建产物） |
| `configs/config.yaml` | 默认配置文件 |
| `install.sh` | 一键安装脚本（自动注册 systemd 或 sysvinit 服务并开机自启） |
| `uninstall.sh` | 卸载脚本 |

## 二、一键安装

要求：Linux 系统，root 权限。约 10 秒完成。

```bash
# 1. 解压到临时目录
mkdir -p /tmp/edgelite-pkg
tar -xzf edgelite-1.0.0-linux-arm7.tar.gz -C /tmp/edgelite-pkg

# 2. 进入目录执行安装（默认安装到 /opt/edgelite，监听 8080 端口）
cd /tmp/edgelite-pkg/edgelite-1.0.0
sudo bash install.sh

# 自定义端口与安装目录
sudo bash install.sh --port 8080 --dir /opt/edgelite
```

安装脚本会自动完成：创建目录与运行用户 → 安装程序与前端 → 安装配置 →
注册系统服务（自动检测 systemd / sysvinit）→ 开机自启 → 健康检查。

## 三、首次登录（重要）

安装完成后浏览器访问 `http://<设备IP>:8080`

- 默认账号：`admin`
- 默认密码：`admin123`

**首次登录后请立即修改密码**（右上角 → 用户设置），并建议：
- 生产环境将 Web 端口限制在内网访问（防火墙/安全组）
- 北向 MQTT 对接时在配置中启用账号认证与 TLS（视平台要求）

## 四、常用运维命令

```bash
# systemd 环境
systemctl status edgelite        # 查看状态
systemctl restart edgelite       # 重启
journalctl -u edgelite -f        # 实时日志

# sysvinit / BusyBox 环境
/etc/init.d/edgelite status
/etc/init.d/edgelite restart
tail -f /opt/edgelite/logs/*.log
```

## 五、目录与配置

| 路径 | 说明 |
|---|---|
| `/opt/edgelite/edgelite` | 主程序 |
| `/opt/edgelite/configs/config.yaml` | 配置文件（修改后重启服务生效） |
| `/opt/edgelite/data/` | SQLite 数据库、时序数据、离线缓存 |
| `/opt/edgelite/logs/` | 运行日志 |

北向 MQTT 上送：修改 `configs/config.yaml` 中 `mqtt.broker`（IoT 平台地址）、
`username` / `password`，然后 `systemctl restart edgelite`。
南向采集：登录 Web 控制台 → 设备管理 → 添加设备（Modbus/S7/FINS/MC/AB 等协议）。

覆盖升级：直接以新包重跑 `install.sh`，已有配置自动保留（新版配置保存为 `config.yaml.new` 供比对）。
卸载：`sudo bash uninstall.sh`（加 `--purge` 连数据目录一并删除）。

## 六、常见问题

**Q: 安装后页面打不开？**
依次检查：服务是否在运行（`systemctl status edgelite`）；防火墙是否放行 8080 端口；
访问的 IP 是否为设备与本机同网段的网卡地址。

**Q: MQTT 数据没有上送到平台？**
确认 config.yaml 中 `mqtt.broker` 指向平台地址且账号密码正确；
设备离线时数据会进入本地离线队列（SQLite），恢复连接后自动补传。

**Q: 串口设备采集不到数据？**
确认设备存在对应串口节点（如 `/dev/ttyS0`、`/dev/ttyUSB0`）且运行用户有读写权限。

**Q: 数据库/日志占满存储？**
8G 存储设备建议将配置中时序数据保留天数收紧（`ts.retention_days`），
并开启降采样（项目已内置三级降采样）。
