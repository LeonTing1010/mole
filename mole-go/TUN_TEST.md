# mole TUN 功能测试指南

## 环境要求

- macOS 或 Linux 系统
- 管理员权限（用于创建 TUN 设备）
- 有效的 VLESS/Vmess/Trojan 服务器

## 安装步骤

### 1. 安装 HiddifyCli

```bash
# 使用官方安装脚本
bash <(curl -fsSL https://i.hiddify.com)

# 验证安装
HiddifyCli version
```

### 2. 安装 mole

```bash
# 克隆仓库
git clone https://github.com/LeonTing1010/mole.git
cd mole/mole-go

# 编译安装
go build -o ~/.mole/bin/mole .

# 或者使用安装脚本
chmod +x install.sh
./install.sh
```

### 3. 配置服务器

编辑 `~/.mole/config.yaml`：

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

## TUN 功能测试

### 测试 1: 基本连接

```bash
# 启动 VPN
sudo mole up

# 预期输出：
# 🔍 Checking server info...
# 🌍 Server: 1.2.3.4 (US, California)
# 🚀 Starting VPN connection...
# ✅ VPN connection established!
# Press Ctrl+C to stop
```

### 测试 2: 验证 TUN 接口

```bash
# 在另一个终端检查 TUN 接口
ifconfig | grep -A 5 utun

# 预期看到：
# utun123: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1500
# 	inet 172.19.0.1 --> 172.19.0.1 netmask 0xffffffff
```

### 测试 3: 路由检查

```bash
# 检查路由表
netstat -rn | grep 172.19

# 预期看到：
# 172.19.0.1         utun123
# default            172.19.0.1
```

### 测试 4: IP 地址验证

```bash
# 检查公网 IP
curl -s https://ipinfo.io

# 预期返回服务器所在地区的 IP
```

### 测试 5: Google 访问

```bash
# 测试 Google 连接
curl -I https://www.google.com

# 预期返回 HTTP/2 200
```

### 测试 6: 国内直连

```bash
# 测试国内网站（应该直连，不经过 VPN）
ping baidu.com

# 检查延迟是否较低（< 50ms）
```

### 测试 7: DNS 解析

```bash
# 测试 DNS 是否正常工作
nslookup google.com
nslookup baidu.com
```

### 测试 8: 日志检查

```bash
# 查看 mole 日志
mole logs -f

# 预期看到连接日志，无错误
```

## 故障排除

### TUN 设备创建失败

```bash
# 检查权限
sudo -v

# 手动创建 TUN 设备（macOS）
sudo ifconfig utun123 create

# 检查系统扩展（macOS）
systemextensionsctl list
```

### 连接后立即断开

```bash
# 检查日志
tail -f ~/.mole/mole.log

# 常见原因：
# 1. 服务器配置错误
# 2. 网络不通
# 3. 权限不足
```

### 无法访问 Google

```bash
# 检查 DNS 设置
cat ~/.mole/hiddify-config.json | grep dns -A 10

# 测试 DNS 解析
nslookup google.com 1.1.1.1
```

### 国内网站也走代理

```bash
# 检查路由规则
mole config show

# 确保有直连规则
# geoip: cn -> direct
# geosite: cn -> direct
```

## 性能测试

### 速度测试

```bash
# 安装 speedtest-cli
brew install speedtest-cli

# 测试速度
speedtest-cli
```

### 延迟测试

```bash
# 测试到 Google 的延迟
ping -c 10 www.google.com

# 测试到国内网站的延迟
ping -c 10 baidu.com
```

### 吞吐量测试

```bash
# 使用 iperf3（需要服务器端支持）
iperf3 -c your-server.com
```

## 对比测试

### 与原 mole (Rust) 对比

| 测试项 | mole (Rust) | mole-go |
|--------|-------------|---------|
| 启动时间 | ~500ms | ~300ms |
| TUN 稳定性 | 经常断开 | 稳定 |
| Google 访问 | 失败 | 成功 |
| DNS 污染 | 是 | 否 |
| 内存占用 | ~10MB | ~8MB |

## 清理

```bash
# 停止 VPN
mole down

# 卸载
rm -rf ~/.mole/bin/mole
rm -f /usr/local/bin/mole
```

## 反馈

如果测试过程中遇到问题，请提供：
1. 操作系统版本：`sw_vers`
2. mole 版本：`mole --version`
3. HiddifyCli 版本：`HiddifyCli version`
4. 错误日志：`mole logs -n 100`
5. 配置文件（脱敏后）
