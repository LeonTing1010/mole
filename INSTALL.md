# mole 安装指南

## 环境要求

- macOS 或 Linux
- Go 1.21+ (仅编译时需要)
- 管理员权限 (用于 TUN 设备)

## 快速安装

### 1. 克隆仓库

```bash
git clone https://github.com/LeonTing1010/mole.git
cd mole/mole-go
```

### 2. 安装 HiddifyCli

```bash
# 使用官方安装脚本
bash <(curl -fsSL https://i.hiddify.com)

# 验证安装
HiddifyCli version
```

### 3. 编译安装 mole

```bash
# 编译
go build -o ~/.mole/bin/mole .

# 创建符号链接
sudo ln -s ~/.mole/bin/mole /usr/local/bin/mole

# 或者使用安装脚本
chmod +x install.sh
./install.sh
```

### 4. 配置

创建配置文件 `~/.mole/config.yaml`：

```yaml
server: "vless://your-uuid@your-server.com:443?security=tls&sni=your-server.com&flow=xtls-rprx-vision"
dns:
  - "1.1.1.1"
  - "8.8.8.8"
log_level: "info"
tun:
  enabled: true
  mtu: 1500
```

### 5. 验证安装

```bash
# 验证配置
mole validate

# 查看配置
mole config show
```

## 使用

### 启动 VPN

```bash
sudo mole up
```

### 停止 VPN

```bash
mole down
```

### 查看状态

```bash
mole status
```

### 查看日志

```bash
mole logs -f
```

## 故障排除

### 错误：HiddifyCli not found

```bash
# 重新安装 HiddifyCli
bash <(curl -fsSL https://i.hiddify.com)
```

### 错误：missing LC_UUID (仅沙箱环境)

这是沙箱环境限制，在真实 macOS 上不会出现。

### 错误：Permission denied

```bash
# 确保使用 sudo 运行
sudo mole up
```

### 错误：Config file not found

```bash
# 创建配置目录和文件
mkdir -p ~/.mole
cat > ~/.mole/config.yaml << 'EOF'
server: "vless://your-uuid@your-server.com:443?security=tls&sni=your-server.com"
dns:
  - "1.1.1.1"
log_level: "info"
EOF
```

## 目录结构

```
~/.mole/
├── bin/
│   ├── mole              # mole 二进制
│   └── HiddifyCli        # HiddifyCli 二进制
├── config.yaml           # 配置文件
├── hiddify-config.json   # 生成的 Hiddify 配置
├── mole.pid              # PID 文件
└── mole.log              # 日志文件
```

## 卸载

```bash
# 停止 VPN
mole down

# 删除二进制
rm -f /usr/local/bin/mole
rm -rf ~/.mole/bin

# 删除配置 (可选)
rm -rf ~/.mole
```

## 测试 TUN 功能

```bash
# 1. 启动 VPN
sudo mole up

# 2. 检查 TUN 接口
ifconfig | grep utun

# 3. 检查路由
netstat -rn | grep 172.19

# 4. 测试 IP
curl https://ipinfo.io

# 5. 测试 Google
curl -I https://www.google.com

# 6. 停止 VPN
mole down
```
