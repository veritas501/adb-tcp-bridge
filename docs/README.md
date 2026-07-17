# 文档索引

`adb-tcp-bridge` 将 USB 侧的 Android ADB 或 OpenHarmony HDC 设备暴露为标准 ADB-over-TCP 端点。外部 `adb` 客户端只连接本桥接进程；真实 USB 传输仍由本机 `adb server` 或 `hdc server` 持有。

## 文档地图

| 文档 | 读者 | 内容 |
|------|------|------|
| [使用指导](usage.md) | 使用者、运维脚本作者 | 构建、启动、ADB/HDC 后端用法、参数、排障。 |
| [代码架构](architecture.md) | 维护者、新贡献者 | 包职责、核心对象、数据流、并发边界和扩展点。 |
| [软件实现](implementation.md) | 需要修改协议逻辑的开发者 | ADB wire、adb host、HDC channel/sync、auth、reverse 的实现细节。 |

## 快速定位

- 命令入口：`src/cmd/adb-tcp-bridge/main.go`
- TCP 桥接会话：`src/internal/bridge/`
- 本机 adb server 后端：`src/internal/adbhost/`
- HDC server 翻译后端：`src/internal/hdcserver/`
- ADB wire 包编解码：`src/internal/adbwire/`
- 构建命令：`Makefile`

## 约束边界

已实现的核心能力包括 ADB packet 编解码、ADB host protocol 转发、HDC shell/sync 翻译、`adb reverse` 控制与反向连接代理，以及 bridge 侧的简化 auth。未实现直接 USB 访问、Wireless Debugging TLS transport、完整 adb server host-side 命令和 RSA 签名验签。
