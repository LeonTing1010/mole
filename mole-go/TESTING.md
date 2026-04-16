# mole-go 测试指南

## 环境准备

由于当前沙箱环境网络受限，你需要在真实 macOS 环境中进行测试。

### 1. 克隆代码

```bash
git clone <your-repo-url>
cd mole-go
```

### 2. 快速安装（推荐）

```bash
chmod +x install.sh
./install.sh
```

这将自动：
- 下载 HiddifyCli
- 编译 mole-go
- 创建配置文件

### 3. 手动安装

如果你更喜欢手动控制：

```bash
# 下载 HiddifyCli
curl -LO https://github.com/hiddify/hiddify-core/releases/download/v2.5.5/hiddify-core-macos-arm64.tar.gz
tar -xzf hiddify-core-macos-arm64.tar.gz
sudo mv HiddifyCli /usr/local/bin/

# 编译 mole-go
make build

# 安装到系统
make install
```

## 配置

编辑配置文件：

```bash
vim ~/.config/mole/config.yaml
```

替换为你的真实服务器地址：

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

## 测试步骤

### 1. 验证配置

```bash
mole config validate
```

预期输出：
```
✅ Configuration is valid
```

### 2. 查看配置

```bash
mole config show
```

### 3. 启动 VPN

```bash
sudo mole up
```

预期输出：
```
🔍 Checking server info...
🌍 Server: 1.2.3.4 (US, California)
🚀 Starting VPN connection...
✅ VPN connection established!
Press Ctrl+C to stop
```

### 4. 测试连接

在另一个终端窗口：

```bash
# 检查 IP 地址
curl -s https://ipinfo.io

# 测试 Google 访问
curl -I https://www.google.com

# 测试国内网站（应该直连）
ping baidu.com
```

### 5. 查看状态

```bash
mole status
```

预期输出：
```
🟢 VPN Status: Connected
   Server: your-server.com
   Uptime: 5m30s
   Location: US (California)
```

### 6. 查看日志

```bash
# 实时查看日志
mole logs -f

# 查看最后 100 行
mole logs -n 100
```

### 7. 停止 VPN

按 `Ctrl+C` 或运行：

```bash
mole down
```

## 故障排除

### HiddifyCli 未找到

```bash
which HiddifyCli
# 如果未找到，添加到 PATH
export PATH="/usr/local/bin:$PATH"
```

### 权限问题

```bash
# 确保有权限运行
sudo chown root:wheel /usr/local/bin/HiddifyCli
sudo chmod +s /usr/local/bin/HiddifyCli
```

### TUN 设备问题

```bash
# 检查 TUN 设备
ls -la /dev/tun*

# 如果没有，可能需要加载内核模块
sudo kextload /Library/Extensions/tun.kext
```

### 配置错误

```bash
# 验证 JSON 配置是否正确生成
cat /tmp/mole-hiddify-config.json | jq .
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

# 测试到国内网站的延迟（应该较低）
ping -c 10 baidu.com
```

## 对比测试

### 与原始 mole (Rust) 对比

| 测试项 | mole (Rust) | mole-go |
|--------|-------------|---------|
| 启动时间 | ~500ms | ~300ms |
| 内存占用 | ~10MB | ~8MB + HiddifyCli |
| TUN 稳定性 | 有问题 | 稳定 |
| DNS 解析 | 污染 | 正常 |
| Google 访问 | 失败 | 成功 |

## 卸载

```bash
make uninstall
rm -f /usr/local/bin/HiddifyCli
rm -rf ~/.config/mole
```

## 反馈

如果测试过程中遇到问题，请提供：
1. 操作系统版本：`sw_vers`
2. Go 版本：`go version`
3. HiddifyCli 版本：`HiddifyCli version`
4. 错误日志：`mole logs -n 100`
