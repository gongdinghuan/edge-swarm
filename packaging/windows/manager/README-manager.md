# Windows 管理端

解压整个 ZIP 到当前 Windows 用户可写的文件夹，双击 `manager.cmd`。启动完成后会打开本机浏览器，地址默认是 `http://127.0.0.1:18080/`。管理端使用已打包的 `bin/control.exe` 和本地 SQLite，不需要 Go、模型、GPU 或 Agent。

在解压目录打开 PowerShell 或命令提示符，运行以下命令。复制一个密钥后，将它粘贴到控制台对应字段，再复制另一个。

```powershell
.\manager.cmd credentials -Copy         # 业务密钥：提交和查看任务
.\manager.cmd credentials -Admin -Copy  # 管理员密钥：管理和注册节点
.\manager.cmd status
.\manager.cmd stop
```

首次启动会生成两把不同的密钥。它们用 Windows DPAPI 加密保存在 `data/manager/credentials.json`，绑定首次启动的 Windows 用户和机器。停止或重新启动会保留密钥与数据库。可从同一目录运行 `manager.cmd help` 查看参数。若端口被占用，改用 `manager.cmd start -ControlPort 18090`。诊断日志在 `data/manager/control.err`。

只有连接就绪的 Agent 后，模型才会出现在控制台并执行任务。本包的管理端仅监听本机回环地址。跨设备和公网部署请参照项目的 Linux、PostgreSQL 和 TLS 部署文档。
