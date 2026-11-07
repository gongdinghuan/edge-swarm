# 蜂群部署与运行手册

系统由单个中心和多个出站 Agent 组成。每个 Agent 连接一个已经能独立运行模型的后端。Windows 本地运行使用 SQLite。公网部署使用 PostgreSQL 和 Caddy，设备可运行在不同地点。DeepInfer 原生接入见 [适配状态](../../adapters/deepinfer/README.md)。

## Windows 常驻本地环境

在仓库根目录使用 Windows PowerShell 或 Windows 上的 PowerShell 7。安装 `go.mod` 要求的 Go 版本，或者使用仓库本地 `.tools/go/bin/go.exe`。执行脚本的 Windows 用户负责保管状态目录。

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/start-local.ps1
$key = & .\scripts\local-credentials.ps1
$base = 'http://127.0.0.1:18080'
$headers = @{ Authorization = "Bearer $key" }
Invoke-RestMethod "$base/v1/chat/completions" -Method Post -Headers $headers `
  -ContentType application/json `
  -Body '{"model":"demo","messages":[{"role":"user","content":"hello"}],"max_tokens":32}'
```

启动完成后，中心、Agent 和模拟模型持续在后台运行。输出含 `[SIMULATED]` 时是在验证传输链路。`data/local` 保存 SQLite、进程记录、日志和凭据。管理员密钥、API 密钥、节点 token 使用 Windows DPAPI 绑定当前用户及机器。`local-credentials.ps1 -Admin` 只用于管理员操作。将脚本输出直接赋给变量，避免在终端或共享日志中打印。

使用 `-ControlPort`、`-ModelPort` 或 `-StateDirectory` 可调整配置。同一状态目录绑定第一次注册的模型名，换模型使用独立目录。已有进程正在使用仓库 `bin` 中的二进制时，用 `-SkipBuild` 复用现有二进制，或先停止该进程再重建。

```powershell
& .\scripts\stop-local.ps1
```

停止时会校验 PID、可执行文件路径和启动时间，只终止已记录的本地进程。停止保留数据库、身份与凭据，下次使用同一目录会恢复。此脚本适合本地开发和演示，系统重启后需要重新执行启动命令。`smoke.ps1` 是一次性测试，执行后清理自己的进程。

在 Windows 上接入已经就绪的真实本地后端时，先按下文验证其文本流式接口，再启动。需要后端密钥时通过当前会话的 `SWARM_BACKEND_KEY` 提供，启动器仅向 Agent 注入该密钥。

```powershell
& .\scripts\start-local.ps1 -BackendUrl http://127.0.0.1:8000/v1 `
  -Model my-model -StateDirectory data/local-real
$key = & .\scripts\local-credentials.ps1 -StateDirectory data/local-real
# 停止时使用同一个目录。
& .\scripts\stop-local.ps1 -StateDirectory data/local-real
```

## Windows 本机 GPU 常驻模式

该路径使用经过固定 SHA256 校验的 llama.cpp b12110 Windows x64 Vulkan 包，以及固定 revision 的 Qwen2.5 0.5B Instruct Q4_K_M GGUF。需要支持 Vulkan 的 GPU 驱动。安装文件存入被 Git 忽略的 `.tools/gpu-smoke`。下载或已有文件校验失败会停止安装，保留现有文件供检查。

```powershell
& .\scripts\install-local-gpu.ps1
& .\scripts\start-local.ps1 -LocalGpu
$key = & .\scripts\local-credentials.ps1 -LocalGpu
$headers = @{ Authorization = "Bearer $key" }
Invoke-RestMethod http://127.0.0.1:18080/v1/chat/completions -Method Post `
  -Headers $headers -ContentType application/json `
  -Body '{"model":"qwen-05b","messages":[{"role":"user","content":"Name the capital of France. Answer in one word."}],"temperature":0,"max_tokens":8}'
```

启动器管理中心、llama-server 和 Agent 三个进程。后端只绑定本机 18081，启用随机 API key，经进程环境注入 llama-server 与 Agent。启动时确认 `/health` 就绪，以及日志报告 Vulkan GPU 层 offload，之后才登记节点。固定模型 alias 是 `qwen-05b`，上下文 2048，并发 1。启动器使用 `Vulkan0`，具体 GPU 名称与 offload 信息保存在 `data/local-gpu/llama-server.err`。

本模式默认使用独立的 `data/local-gpu` 数据目录。中心端口仍为 18080，若演示栈正在运行，先停止或给 GPU 模式指定其他 `-ControlPort` 和 `-ModelPort`。启动返回的 `simulated` 为 false、`local_gpu` 为 true。停止时使用下列命令，三个受管理进程均会停止，数据库和 DPAPI 凭据保留。

```powershell
& .\scripts\stop-local.ps1 -LocalGpu
```

模型与运行时原始来源分别为 [Qwen 固定 revision](https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/tree/9217f5db79a29953eb74d5343926648285ec7e67) 和 [llama.cpp b12110](https://github.com/ggml-org/llama.cpp/releases/tag/b12110)。安装器固定文件哈希，正式运行前也会检查可执行文件、Vulkan 库和 GGUF 哈希。

## 公网中心

在 Linux 主机准备 Docker Compose v2、OpenSSL 和指向该主机的公共域名。开放 TCP 80、443。镜像用经过审查的 digest 固定。示例数据卷布局针对 PostgreSQL 17。同一数据卷升级大版本要走 PostgreSQL 迁移流程。

```bash
cp deploy/control/compose.env.example deploy/control/.env
# 编辑该 .env，设置域名、镜像 digest 和绝对 secrets 目录。
sudo install -d -m 0700 /etc/edge-swarm
sudo sh deploy/control/init-secrets.sh /etc/edge-swarm/secrets
sudo docker compose --env-file deploy/control/.env -f deploy/control/compose.yaml config --quiet
sudo docker compose --env-file deploy/control/.env -f deploy/control/compose.yaml up -d --build
sudo docker compose --env-file deploy/control/.env -f deploy/control/compose.yaml ps
curl --fail --silent --show-error https://swarm.example.org/healthz
```

`init-secrets.sh` 只创建新目录。秘密值保存在目录内，Compose 仅为需要的服务挂载文件，Caddy 无法读取。宿主目录保持 0700，挂载文件 0444 使容器内非 root 用户可读。宿主目录需处于仓库和构建上下文之外。目录保留在指定主机，备份时按密钥材料保护。

中心进程通过启动包装器读取密钥。`swarm_api_keys` 是 API key 到租户的 JSON 映射。每个业务 key 和管理员密钥应独立随机生成。数据库密码使用 64 位以上小写十六进制，避免 DSN 拼接误解析。只有中心和 PostgreSQL 共享隔离数据库网络。数据库密码轮换需先协调数据库角色与挂载文件，再重启相关服务。替换文件不会为已有数据卷修改角色密码。

中心进程的 `/readyz` 数据库检查通过后，Compose 启动 Caddy。`/healthz` 反映中心进程存活。再通过控制台确认 Agent 和模型就绪。Caddy 原生代理 WebSocket 并对 SSE 及时刷新。涉及 Caddy 重启或重载时，按可能中断正在执行的连接安排维护。 [Caddy 代理文档](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)

停止与备份时，保留 `database`、`certificates` 和 `caddy-config` 卷。`docker compose stop` 保留卷和容器配置。数据库备份包含租户输入和结果，应限权并加密。启动后查验 `ps` 及 `logs control postgres caddy`，避免把日志直接传给外部人员。

## Linux 真实后端与常驻 Agent

### 1. 启动并探测本地模型

按机器的 GPU、驱动及模型格式锁定兼容 vLLM 版本和权重 revision，在专用 Python 环境或容器安装。示例假设模型已下载到 `/srv/models/approved-model`，用该地点实测的上下文和显存配置替换示例值。通过管理服务或设备现有进程管理器保持模型运行。 [vLLM 安装与服务文档](https://docs.vllm.ai/en/latest/getting_started/installation/) [vLLM API 文档](https://docs.vllm.ai/en/latest/serving/openai_compatible_server/)

```bash
vllm serve /srv/models/approved-model --served-model-name my-model \
  --host 127.0.0.1 --port 8000 --max-model-len 4096 --max-num-seqs 1
```

本机存在其他用户时应加后端密钥。vLLM 可从进程环境 `VLLM_API_KEY` 读取，Agent 通过 `SWARM_BACKEND_KEY` 获取同一个值。两个进程各自从受限的密钥配置读取。公网仅允许 Agent 向中心出站访问 443。 [vLLM 环境变量文档](https://docs.vllm.ai/en/latest/configuration/env_vars/)

以下命令适用于未配置后端鉴权的可信本机。启用鉴权时给测试请求提供对应 Authorization。确认模型名一致、能生成正文、SSE 返回 `data: [DONE]`。接入其他本地引擎时使用相同的协议检查。带推理专有字段的模型还应确认产生可用于文本兼容接口的 `delta.content`。

```bash
curl --fail --silent --show-error --no-buffer http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"my-model","messages":[{"role":"user","content":"Hello"}],"max_tokens":32,"stream":true}'
```

### 2. 注册设备身份

在可信管理机使用管理员 token 调用中心。可将密钥从密码管理器粘贴到 `read -rs`，然后通过 curl 配置标准输入提供认证头。请使用 Bash 并关闭命令跟踪。响应含节点密钥，需要通过受限渠道交给设备主人。

```bash
read -rs -p 'Swarm admin token: ' swarm_admin_token
printf '\n'
swarm_identity=$(printf 'header = "Authorization: Bearer %s"\n' "$swarm_admin_token" | \
  curl --config - --fail --silent --show-error https://swarm.example.org/swarm/v1/nodes/enroll \
    -H 'Content-Type: application/json' -d '{"model":"my-model"}')
unset swarm_admin_token
# 将响应中的 node_id 和 token 安全交给设备管理员，随后 unset swarm_identity。
```

设备管理员用 `deploy/worker/agent.env.example` 填写 `/etc/edge-swarm/agent.env`。配置节点 ID、节点 token、HTTPS 中心 URL、准确模型名和本地后端地址。需要后端鉴权时添加 `SWARM_BACKEND_KEY`。文件必须由 root 持有且为 0600，由 systemd 读取后注入进程。建议初次用 `sudoedit` 编辑，避免将密钥写进命令历史。

### 3. 安装、启停及验证

在目标 Linux 机器或同架构构建环境，使用仓库要求的 Go 版本。首次安装前确认目标路径未放置不同配置。

```bash
CGO_ENABLED=0 go build -trimpath -o bin/swarm-agent ./cmd/agent
sudo install -m 0755 bin/swarm-agent /usr/local/bin/swarm-agent
sudo install -d -m 0700 /etc/edge-swarm
# 初次安装，用示例建立 /etc/edge-swarm/agent.env，再 sudoedit 写入真实配置。
sudo install -m 0600 deploy/worker/agent.env.example /etc/edge-swarm/agent.env
sudoedit /etc/edge-swarm/agent.env
sudo install -m 0644 deploy/worker/swarm-agent.service /etc/systemd/system/swarm-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now swarm-agent
sudo systemctl status swarm-agent
sudo journalctl -u swarm-agent -n 50 --no-pager
```

使用普通业务 key 请求中心 `/v1/models`，确认能看到 `my-model`。再对中心 `/v1/chat/completions` 发一条 `stream:true` 的文本请求，确认收到正常正文和 SSE 结束标志。最终验收同时检查模型进程及设备侧 `nvidia-smi` 遥测，以确认有真实 GPU 执行。注册成功只表明 Agent 在线。

停机前，在控制台对该节点执行排空（或管理员 POST `/swarm/v1/nodes/{id}/drain`），等 `active=false` 后用 `sudo systemctl stop swarm-agent` 停止。再次启动后点击恢复（或 POST `/swarm/v1/nodes/{id}/resume`）。节点退役或凭据泄露时，调用 `/swarm/v1/nodes/{id}/revoke` 并停服务；重新加入须注册新身份并安全替换本地配置。

## 排查

| 现象 | 先检查 |
|---|---|
| 本地启动报端口占用 | 旧进程是否仍在运行，或用另一组端口 |
| 模型未出现在中心 | 节点 ID/token、注册模型名、中心证书和设备出站访问 |
| 模型已注册但推理失败 | 本地 SSE 请求、后端鉴权、模型服务日志、上下文和显存余量 |
| 中心启动数据库失败 | PostgreSQL 健康状态、旧卷与角色密码、容器能否读取挂载密钥 |
| 证书申请失败 | DNS、IPv4/IPv6 记录、80/443 可达性及主机时间 |

Compose 文件密钥行为见 [Docker 官方文档](https://docs.docker.com/compose/how-tos/use-secrets/)。当前 Windows 环境无 Docker 和 Linux 运行时。Windows 常驻流程可实测，Compose 公网联通、systemd 和真实 GPU 需在目标机器按本手册验收。
