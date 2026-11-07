# 蜂群 Windows Worker

将安装包完整解压到当前 Windows 用户可写的目录，保留 `bin/agent.exe`。使用 Windows PowerShell 5.1 或 PowerShell 7。Worker 主动连接中心，无需开放入站端口。模型由你现有的 vLLM、llama.cpp 或其他兼容服务运行。

1. 先启动模型服务，并确认其 `/v1/models` 列出你要注册的模型，`/v1/chat/completions` 支持 SSE 流式输出。
2. 在蜂群控制台注册节点，记下节点 ID、节点 token 和注册模型名。
3. 在解压目录执行下列命令。配置脚本会提示输入缺少的参数，节点 token 和可选的后端密钥在输入时隐藏。

```powershell
.\Install-Worker.ps1 -ControlUrl https://swarm.example.org -Model my-model -BackendUrl http://127.0.0.1:8000/v1
.\Start-Worker.ps1 -Check
.\Start-Worker.ps1
```

若本机 PowerShell 阻止从 ZIP 解压的脚本，先确认安装包来源，再对这些脚本逐个执行 `Unblock-File -LiteralPath`。配置命令支持 `-NodeId`，后端无需鉴权时可以加 `-NoBackendKey`。

`-Check` 检查中心的 `/readyz`、后端模型列表和模型名。启动后在中心控制台确认节点上线。中心断开时，Worker 自动重连。若节点凭据被撤销或拒绝，先检查 `state/worker.err`，再在中心注册新身份并重新配置。

```powershell
.\Stop-Worker.ps1
# 更换节点身份、中心 URL 或模型时，再运行配置和启动脚本。
.\Install-Worker.ps1
.\Start-Worker.ps1
```

配置和日志保存在安装包的 `state/`。密钥由 Windows DPAPI 加密，绑定本机及当前用户。换机器或 Windows 用户时重新配置。停止脚本只结束本包记录的 Agent 进程。停止 Worker 会中断它正在执行的请求，模型后端由你原有的管理方式继续运行。Windows 重启后，重新执行启动命令。
