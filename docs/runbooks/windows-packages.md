# 蜂群 Windows 安装包

三个包都是 Windows x64 便携 ZIP。完整解压到可写目录，再运行包内入口。目标机使用 Windows PowerShell 5.1 或 PowerShell 7，无需安装 Go。包内程序、脚本及依赖文件有 SHA-256 清单，ZIP 旁另有 `.sha256` 文件用于核对下载。

| 包 | 适用机器 | 启动入口 |
|---|---|---|
| `Swarm-manager-0.1.1-windows-amd64.zip` | 管理中心，普通 Windows 主机即可 | 双击 `manager.cmd`，或运行 `manager.cmd start -ControlPort 18082` |
| `Swarm-worker-0.1.1-windows-amd64.zip` | 有现成模型服务的工作机 | `Install-Worker.ps1`、`Start-Worker.ps1 -Check`、`Start-Worker.ps1` |
| `Swarm-gpu-0.1.1-windows-amd64.zip` | 单机真实 GPU 体验 | 双击 `gpu.cmd`，或运行 `gpu.cmd -Check -InstallDependencies` 检查并补齐运行库 |

如果这台电脑已运行蜂群，默认 18080 或 18081 可能已占用。可以停止原实例，或使用其他端口与独立数据目录。管理包默认只监听 `127.0.0.1`。跨地点部署按 [部署手册](deployment.md) 配置 HTTPS 中心，工作节点填写该域名并出站连接。

## 节点如何分工

管理节点运行 `control.exe`，提供浏览器界面、业务 API、模型发现和设备注册。它保存任务、结果和租约，从已就绪且有容量的节点中选出执行者。管理员可查看设备、排空、恢复和撤销。管理机可与工作机分开。

工作节点运行 `agent.exe`。Agent 持有专属节点凭据，主动通过 WebSocket 连接中心，报告模型与就绪状态。领到任务后，它调用本机配置的模型服务，转发输出和任务状态。实际 GPU 计算在模型服务进程中完成。工作包以脚本和日志管理，统一网页在中心。

控制台上方是可用模型、最近任务、已连接设备。业务密钥打开推理和任务区，管理员密钥打开设备区。设备注册返回节点 ID 和 token，供对应工作机配置。页面包含流式推理、离线批量任务、任务详情、取消和按批组 ID 查询。

## 依赖故障与恢复

| 现象 | 检查及修复 |
|---|---|
| `PACKAGE_MISSING` 或 `PACKAGE_CHECKSUM` | 核对 ZIP 的 SHA-256，重新完整解压到可写目录，保留已有数据目录。若是可执行文件被隔离，在 Windows 安全中心检查记录 |
| 端口已占用 | 停止已知旧实例，或用 `-ControlPort` 和 `-ModelPort` 选择空闲端口 |
| 中心不可达、证书错误或 503 | 使用 `Start-Worker.ps1 -Check`，确认中心域名、系统证书信任、出站防火墙和 `/readyz` |
| 后端未启动、密钥错误、模型名不同 | 先启动模型服务并检查 `/v1/models`。重新运行 `Install-Worker.ps1` 修正后端地址、密钥或模型名 |
| 节点凭据失效 | 检查 `state/worker.err`。在管理界面注册新身份，再配置并启动 Worker |
| GPU 包缺 VC++ DLL | 联网运行 `gpu.cmd -Check -InstallDependencies`，按提示授权管理员权限，查看安装日志或重启提示。离线环境可由管理员准备官方 x64 安装程序。 [Microsoft 官方 VC++ 下载](https://learn.microsoft.com/en-us/cpp/windows/latest-supported-vc-redist) |
| GPU 包缺 `vulkan-1.dll` | 根据显卡型号安装或修复厂商提供的 Vulkan 驱动，再运行 `gpu.cmd -Check` |
| GPU 加载时退出或未成功卸载 | 查 `data/local-gpu/llama-server.err`，核对驱动和显存。启动脚本会尝试结束本次已启动的进程，保存日志及状态 |

管理端的 `manager.cmd stop`、工作端的 `Stop-Worker.ps1`、离线 GPU 包的 `Stop-Gpu.ps1` 用于结束各自进程。启动和停止可重复使用，凭据和任务数据保留在原状态目录。换 Windows 用户或设备时，DPAPI 凭据需重新配置。

GPU 包附带固定版本的 llama.cpp Vulkan 和 Qwen2.5 0.5B Instruct Q4_K_M，模型及运行时安装可离线。工作包接入已有推理服务，连远端中心时仍需要出站网络。包内附有第三方许可。 [llama.cpp 发行版](https://github.com/ggml-org/llama.cpp/releases/tag/b12110) · [Qwen 模型卡](https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF)

`gpu.cmd -Check` 只校验包及系统依赖。运行 `gpu.cmd -InstallDependencies` 可在缺失 Visual C++ DLL 时自动下载、验证 Microsoft Authenticode 签名并安装，随后启动蜂群。此选项使用 Microsoft 的 x64 下载入口，并在安装后复查运行库。 `-Check -InstallDependencies` 执行同样的依赖补齐后退出，适用于部署预检。 [Microsoft 安装参数](https://learn.microsoft.com/en-us/cpp/windows/redistributing-visual-cpp-files)

## 成品验收

三个 0.1.1 ZIP 已用 Windows PowerShell 5.1 在含中文和空格的目录解压验收。管理包验证了缺文件、端口占用和重启保留凭据。工作包在模型服务不可达时拒绝启动，服务恢复后连接管理节点并完成模拟请求。GPU 包通过 `-Check` 预检，拦截了缺失的包内 `ggml-vulkan.dll`，恢复文件后用 RTX 5060 完成真实 Qwen 推理，25 层全部在 GPU 上加载。测试结束后清理了本次启动的进程。

VC++ 下载、安装退出码、签名和安装后检查通过替身命令模拟。验收机已有 VC++ 运行库，真实 GPU 测试使用该运行库。

## 从源码构建

在有 Go 和项目依赖缓存的构建机运行以下命令。GPU 包还要求工作区有 `scripts/install-local-gpu.ps1` 下载并校验过的官方 ZIP 和模型。构建输出位于 `dist/windows`。

```powershell
.\scripts\build-windows-packages.ps1 -IncludeGpu
```
