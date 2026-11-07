# 蜂群运行验收记录

验收日期为 2026 年 11 月 7 日。当前部署单元为一个中心和多个已注册的可信节点。

| 项目 | 已验证结果 |
|---|---|
| 真实 GPU | Windows RTX 5060 运行 llama.cpp Vulkan b12110 和 Qwen2.5 0.5B Instruct Q4_K_M。后端记录 25/25 层卸载和非零 Vulkan 模型缓冲区 |
| 端到端推理 | 控制中心通过 WebSocket Agent 把请求发给真实模型。同步回答 `Paris`，流式输出带 `[DONE]`，两个离线子任务完成 |
| 持久启停 | 停止并重新启动中心、Agent 和模型，SQLite 保留节点身份和同一批组结果 |
| 数据库 | 使用校验过的 PostgreSQL 17.11 便携包运行真实事务测试。涵盖幂等、批组、租约、代次隔离、数据库锁超时和 HTTPS/WSS 端到端请求 |
| 多节点恢复 | 测试启动两个 WebSocket 节点。首个节点断开并以新代次重连，原任务退避后交给另一节点完成 |
| 并发与网络 | 带真实 PostgreSQL 的全套 `go test -race` 通过。包含本地可信证书 HTTPS/WSS、取消、流式故障和错误关闭 |
| 运维 | Windows 启动和停止脚本管理其创建的三个本机进程。控制 API 提供就绪探测、节点目录、排空、恢复和撤销 |
| 控制台 | 本机真实服务在 Codex 内置浏览器显示就绪。Edge 使用测试数据验证键盘导航、控件绑定、CSP 和 320–1440 像素布局。三项 Node 测试验证 SSE 分段、异常断流和批组输入 |

Windows 真实 GPU 验收使用工作区已校验的运行时和模型文件。脚本会启动、提交请求、停止、重启，并确认批组结果持久化。

```powershell
& .\scripts\install-local-gpu.ps1
& .\scripts\verify-local-gpu.ps1
```

使用 `-KeepRunning` 可在通过后保留本机控制台。停止命令为 `& .\scripts\stop-local.ps1 -LocalGpu`。

真实 PostgreSQL 测试需要专用测试库。通过进程环境设置 `SWARM_TEST_POSTGRES_DSN` 和 `SWARM_TEST_E2E_POSTGRES_DSN` 后运行下列命令。测试会在该数据库中写入测试数据。

```powershell
& .\.tools\go\bin\go.exe test -race -p 1 ./... -count=1 -timeout 90s -v
```

后续现场验收覆盖公网 DNS 和证书、Docker Compose、Linux systemd 及多地点长时间运行。DeepInfer 原生执行需要上游支持的设备，当前已提供固定版本的硬件预检和离线参考入口。 [部署手册](deployment.md) · [DeepInfer 预检](../../adapters/deepinfer/README.md)

本次运行时和模型的固定来源及哈希见 [安装脚本](../../scripts/install-local-gpu.ps1)。 [llama.cpp b12110](https://github.com/ggml-org/llama.cpp/releases/tag/b12110) · [Qwen 权重 revision](https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/tree/9217f5db79a29953eb74d5343926648285ec7e67) · [EDB PostgreSQL 校验](https://github.com/EnterpriseDB/edb-installers/issues/706)
