# 蜂群 Linux 部署手册

本手册适用于蜂群 0.1.1 Linux 部署包。管理端提供中文控制台、OpenAI 兼容 API、任务调度和状态存储。各地点的工作端用出站 HTTPS WebSocket 接入中心，并把推理任务发给设备上的模型服务。一个工作端进程提供一个执行槽。

## 1. 选包与验包

| 目标机器 `uname -m` | 管理端 | 工作端 |
| --- | --- | --- |
| `x86_64` | `Swarm-manager-0.1.1-linux-amd64.tar.gz` | `Swarm-worker-0.1.1-linux-amd64.tar.gz` |
| `aarch64` | `Swarm-manager-0.1.1-linux-arm64.tar.gz` | `Swarm-worker-0.1.1-linux-arm64.tar.gz` |

四个包可按机器架构独立选用，中心与工作端可以是不同架构。每包包括静态可执行文件、`swarm.sh`、配置样例、systemd 单元、中文手册与依赖声明。蜂群程序无需在部署机安装 Go 或 Docker。工作端需要已有模型后端、兼容 GPU 驱动和模型权重，按设备分别准备。DeepInfer 接入需要另外提供兼容的模型服务，本包的工作端使用 OpenAI 兼容接口。

常驻部署要求运行中的 systemd 247 或更新版本。验包和解压需要 Bash、GNU coreutils、tar 和 gzip。`swarm.sh install` 会检测其余运行依赖，在 Debian、Ubuntu 上调用 APT，在 RHEL、Rocky Linux、AlmaLinux 上调用 DNF，只向已配置的软件源请求缺失的依赖。依赖齐全时直接安装服务，适用于离线部署。其他发行版可按提示安装缺少的系统组件。 [APT 文档](https://manpages.debian.org/bookworm/apt/apt-get.8.en.html) [DNF 文档](https://docs.redhat.com/en/documentation/red_hat_enterprise_linux/9/html/managing_software_with_the_dnf_tool/assembly_installing-rhel-9-content_managing-software-with-the-dnf-tool)

精简镜像如果连验包工具也缺少，先用发行版包管理器补齐。例如在 Debian 或 Ubuntu 执行下列命令。

```bash
sudo apt-get update
sudo apt-get install bash coreutils tar gzip
```

从可信发布渠道取得所需归档和对应 `.sha256`，将两者放在同一目录。以下以 x86_64 管理端为例。工作端把文件名的 `manager` 改为 `worker`。ARM64 把 `amd64` 改为 `arm64`。

```bash
archive=Swarm-manager-0.1.1-linux-amd64.tar.gz
sha256sum -c "$archive.sha256" && tar -xzf "$archive" &&
  cd "${archive%.tar.gz}" && bash ./swarm.sh verify
```

`verify` 检查系统、架构、包内 SHA-256 清单和二进制执行权限。`check` 进一步读取实际配置并探测部署依赖。配置使用逐行的 `KEY=value`，`=` 后原样读取，注释须以 `#` 开头。JSON 保留内部双引号，值外面无需加 shell 引号。不要在配置中写 `export`、插值表达式或行末注释。实际配置文件权限设为 `0600` 或 `0400`。

可用 `bash ./swarm.sh deps --check` 单独检查前台运行依赖，用 `sudo bash ./swarm.sh deps` 补齐这些依赖。容器或其他无 systemd 的环境可使用这个入口，再执行 `swarm.sh run`。软件包安装失败时，安装器保留软件源工具的原始输出，修复后可重试。

## 2. 管理端

### 前台试运行

在已解压的管理端目录执行下列命令。`init` 用系统随机源生成独立的管理员密钥和业务密钥，保存在 `manager.env`，已有文件会保留。首次前台运行默认使用 SQLite，状态位于包目录的 `state/swarm.db`。前台依赖缺少时，先执行 `sudo bash ./swarm.sh deps`。

```bash
bash ./swarm.sh init
bash ./swarm.sh check
bash ./swarm.sh run
```

在另一终端执行 `curl -fsS http://127.0.0.1:8080/readyz`。预期得到 `{"status":"ready"}`。打开 `http://127.0.0.1:8080/`。用受限编辑器查看 `manager.env`，将 `SWARM_ADMIN_TOKEN` 的值填入「管理员密钥」，将 `SWARM_API_KEYS` JSON 中作为属性名的随机字符串填入「业务密钥」，点击「应用密钥」。前台用 Ctrl+C 停止。

### 安装 systemd 服务

在解压目录执行以下命令。先停止占用相同端口的前台实例。安装器会补齐依赖、生成管理凭据、部署包和单元，并等待中心就绪后设为开机启动。

```bash
sudo bash ./swarm.sh install
sudo systemctl status edge-swarm-manager --no-pager
curl -fsS http://127.0.0.1:8080/readyz
```

需要先修改端口或使用 PostgreSQL 时，执行 `sudo bash ./swarm.sh install --no-start`，用 `sudoedit /etc/edge-swarm/manager.env` 修改配置，再执行 `sudo bash /opt/edge-swarm-manager/swarm.sh install`。已有私有配置会保留。首次安装也可在 `install` 命令最后附加一个已有配置文件的路径，安装器将它复制到 `/etc/edge-swarm/manager.env`。密钥只通过配置文件传递。

`check` 用于启动前，运行中的中心占用本端口属于预期。服务通过 systemd `LoadCredential` 读取私有配置，并在 `/readyz` 成功后完成启动。SQLite 服务状态位于 `/var/lib/edge-swarm`，systemd 管理其权限。前台实例与 systemd 实例默认使用不同的配置和数据库，设备身份随数据库保存。 [systemd 服务配置文档](https://www.freedesktop.org/software/systemd/man/249/systemd.exec.html)

### 接通公网 HTTPS

1. 为中心准备 PostgreSQL 数据库和有权建表及维护该数据库对象的账号。编辑 `manager.env`，加入 `SWARM_DATABASE_URL=postgres://swarm:URL_ENCODED_PASSWORD@db.example.org:5432/swarm?sslmode=verify-full`，替换主机、凭据和库名。远程数据库的证书须受管理机信任，证书域名要匹配连接地址。管理端会初始化表。将已有 SQLite 数据迁移到 PostgreSQL 需要单独迁移；全新库需要重新注册节点。
2. 为中心分配域名并指向管理机。让 Caddy 监听公网 TCP 80 和 443。将包内 `Caddyfile.example` 的站点配置合并到 `/etc/caddy/Caddyfile`，替换域名与实际 `SWARM_PORT`，保留 `reverse_proxy 127.0.0.1:8080` 的回环地址用法。Caddy 提供 HTTPS、WebSocket 代理和 SSE 回传。 [Caddy HTTPS 文档](https://caddyserver.com/docs/automatic-https) [Caddy 代理文档](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
3. 校验配置、重启管理端并加载 Caddy。若 Caddy 是首次部署，先按发行版的服务安装方式启用 Caddy。访问 `https://swarm.example.org/`，从另一网络验证 `/readyz`。

```bash
sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl restart edge-swarm-manager
sudo systemctl reload caddy
curl -fsS https://swarm.example.org/readyz
```

## 3. 每个地点安装工作端

已安装 GPU 版 llama.cpp 并准备好兼容的 GGUF 模型时，可在本地试运行如下。按实际权重路径和显存调整 `--model` 与 `-ngl`，并从引擎日志确认 GPU 层加载成功。

```bash
llama-server --host 127.0.0.1 --port 8000 \
  --model /srv/models/my-model.gguf --alias my-model -ngl 99
```

1. 启动本机模型服务，例如 llama.cpp 或 vLLM。绑定 `127.0.0.1` 并确认支持 `GET /v1/models` 与 `POST /v1/chat/completions` 的文本 SSE。下面以已在本机运行的 `my-model` 为例。无鉴权的本地后端可先执行 `curl -fsS http://127.0.0.1:8000/v1/models`，以返回的 `data[].id` 为准。使用后端密钥时，填入工作端配置后用 `swarm.sh check` 验证。
2. 打开管理端 HTTPS 控制台，应用管理员密钥。在「设备管理」输入精确模型名，点击「注册设备」。将返回的节点 ID 和节点令牌交给该设备的管理员。每个工作端使用单独身份。
3. 解压匹配架构的工作端包，在解压目录执行下列命令。第一次执行 `install` 会补齐依赖并生成待填写的配置。编辑时填入中心 HTTPS 地址、节点 ID、节点令牌、本地后端 URL 与模型名。模型后端启用认证时，配置 `SWARM_BACKEND_KEY`。可选的 `SWARM_GPU` 和 `SWARM_VRAM_MIB` 用于控制台展示。

```bash
sudo bash ./swarm.sh install
sudoedit /etc/edge-swarm/worker.env
sudo bash /opt/edge-swarm-worker/swarm.sh install
sudo journalctl -u edge-swarm-worker -n 30 --no-pager
```

批量部署时，可以为每台设备单独准备私有配置文件。第一次安装执行 `sudo bash ./swarm.sh install /path/to/worker.env`，安装器会复制配置、验证依赖和后端，再启动服务。

仅做前台试运行时，可以在解压目录执行 `install -m 0600 worker.env.example worker.env`，编辑 `worker.env`，再运行 `bash ./swarm.sh check` 和 `bash ./swarm.sh run`。同一个节点 ID 同时只运行一个工作端进程。

公网工作端只需要出站访问中心 TCP 443。`check` 验证中心证书与 `/readyz`、后端鉴权以及模型名。实际接入后，在控制台确认该节点状态为「可接单」，随后在「推理」页面发一条请求，确认收到正文。也可以从终端发起一次流式请求。

```bash
set +x
read -rs -p '蜂群业务密钥: ' swarm_key
printf '\n'
printf 'header = "Authorization: Bearer %s"\n' "$swarm_key" | \
  curl -q --config - --fail --silent --show-error --no-buffer \
  https://swarm.example.org/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"my-model","messages":[{"role":"user","content":"请回复一句简短问候"}],"stream":true,"max_tokens":32}'
unset swarm_key
```

验收时确认流里出现正文 `delta.content`，最后收到 `data: [DONE]`。如需验证确实使用 GPU，查看模型引擎的 GPU 加载日志和推理时的显存占用。蜂群控制台显示的是工作端上报的硬件信息。

## 4. 部署故障处理

| 现象 | 排查和恢复 |
| --- | --- |
| `missing command` | 用 `sudo bash ./swarm.sh deps` 补齐。`uname` 或 `sha256sum` 缺失时先补装 GNU coreutils，安装器需要先校验包。 |
| APT 或 DNF 安装失败 | 按原始错误检查 DNS、镜像、软件源订阅或正在运行的包管理器。待锁释放或软件源恢复后重跑同一条命令。 |
| `systemd is not reachable` 或版本过低 | 在运行 systemd 247 或更新版本的系统安装服务。容器环境可用 `deps` 和 `run` 前台部署。 |
| `wrong architecture`、`Exec format error` | 对照 `uname -m` 重新下载对应架构，先验证归档哈希，再解压。 |
| `files are missing or corrupt` | 对归档执行 `sha256sum -c`，确认归档可信后解压到新目录。若归档本身损坏，重新获取。 |
| `binary is not executable` | 在支持 Linux 权限的文件系统中用 `tar -xzf` 解压，检查文件模式和挂载是否为 `noexec`。确认包内哈希后恢复 `chmod 0755 bin/control` 或 `chmod 0755 bin/agent`。 |
| `configuration not readable`、`mode 0600 or 0400` | 检查文件路径及权限。systemd 方式用 `sudo chown root:root /etc/edge-swarm/角色.env` 和 `sudo chmod 0600 /etc/edge-swarm/角色.env`，把角色替换为 `manager` 或 `worker`。 |
| `listen address unavailable` | 执行 `ss -ltnp` 查占用进程。停止自己启动的旧中心，或把 `SWARM_PORT` 改成空闲端口并同步修改 Caddy。 |
| PostgreSQL 连接检查失败，或 `/readyz` 返回 503 | 查数据库服务、DNS、端口、账号、库名、证书和 DSN。密码中的特殊字符要做 URL 编码。首次启动还要有建表权限，查看管理端日志。 |
| 管理端启动就绪超时 | 执行 `sudo journalctl -u edge-swarm-manager -b -n 80 --no-pager`。核对管理端配置和数据库，检查本机 `/readyz`。 |
| `control readiness is unreachable` 或 HTTP 502 | 从工作端运行 `curl -fsS https://你的域名/readyz`。核对 DNS、中心监听、代理上游、出站 443、系统时间和 CA 信任。 |
| `/readyz` 正常，设备仍不在线 | 看工作端日志中的 WebSocket 状态。401/403 检查节点 ID、令牌和撤销状态。404/502 检查代理是否转发 `/swarm/v1/agent/ws`，并允许 WebSocket 升级。 |
| `backend model list is unreachable` | 先让模型服务监听配置中的地址。检查服务日志、端口、驱动、模型文件和显存。工作端恢复探测后会重新上报就绪。 |
| 后端模型列表 HTTP 401/403 或模型不存在 | 核对 `SWARM_BACKEND_KEY`、`SWARM_BACKEND_URL` 的 `/v1` 路径、`SWARM_MODEL` 和注册时的模型名。 |
| 修复后 systemd 提示 `start-limit-hit` | 执行 `sudo systemctl reset-failed edge-swarm-manager` 或 `sudo systemctl reset-failed edge-swarm-worker`，再启动对应服务。 |

## 5. 启停、备份与升级

```bash
# 在各自机器上执行对应角色的命令。
sudo systemctl stop edge-swarm-worker
sudo systemctl start edge-swarm-worker
sudo systemctl stop edge-swarm-manager
sudo systemctl start edge-swarm-manager
```

升级前验证新包，安排维护窗口。备份 `/etc/edge-swarm` 的配置，SQLite 在服务停止时备份 `/var/lib/edge-swarm` 的完整状态目录，PostgreSQL 则备份数据库。配置、备份与节点令牌均按凭据或租户数据保管。

逐台停止对应服务，然后在解压的新包目录执行 `sudo bash ./swarm.sh install`。安装器先用现有配置做启动前检查，再将旧程序和单元备份到输出中显示的 `/opt/.edge-swarm-角色-backup.*` 目录。相同版本重复安装会沿用现有配置，已在运行的服务会继续运行。配置修改后，如需重新读取配置，执行 `sudo systemctl restart` 加对应服务名。

升级后依次检查 `/readyz`、节点就绪和一次真实推理。需要回滚时停止服务，将当前程序目录移到单独位置，把备份目录中的 `package/` 恢复到 `/opt/edge-swarm-角色`，再将备份的 `.service` 文件复制回 `/etc/systemd/system/`。若版本升级涉及数据库结构变化，同时恢复配套的数据库备份。执行 `sudo systemctl daemon-reload` 后启动旧服务。
