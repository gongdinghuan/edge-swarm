# 蜂群 Windows GPU 离线包

在 Windows x64 上解压整个包到可写目录。包内含预编译中心与 Agent、llama.cpp Vulkan 运行时和 Qwen2.5 0.5B 模型。运行需要支持 Vulkan 的显卡驱动，以及 Microsoft Visual C++ v14 x64 运行库。启动会校验包内文件并检查 GPU offload。[Microsoft VC++ 运行库](https://learn.microsoft.com/en-us/cpp/windows/latest-supported-vc-redist)

从解压目录打开 PowerShell，运行 `.\gpu.cmd`。默认访问 `http://127.0.0.1:18080/`，模型名为 `qwen-05b`，本机后端端口为 `18081`。需要换端口时，运行 `.\gpu.cmd -ControlPort 28080 -ModelPort 28081`。

初次部署时，可以先检查依赖。如果缺少 Visual C++ x64 运行库，第二条命令会联网获取 Microsoft 官方安装程序，验证签名及安装包类型后申请管理员权限。安装结束后会重新检查 DLL。需要重启时，脚本会提示并保留安装日志。第三条命令在依赖检查通过后直接启动推理。

```powershell
.\gpu.cmd -Check
.\gpu.cmd -Check -InstallDependencies
.\gpu.cmd -InstallDependencies
```

```powershell
# 将业务密钥复制到剪贴板，再粘贴到控制台。
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\local-credentials.ps1 -LocalGpu -Copy
# 停止本机进程，保留数据库、凭据和日志。
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Stop-Gpu.ps1
```

需要设备管理时，向凭据脚本传入 `-Admin`。默认状态与日志位于 `data/local-gpu`，凭据由 Windows DPAPI 绑定当前用户和机器。自定义 `-StateDirectory` 时，启动、查看凭据与停止要使用同一个目录。迁移包位置前先停止进程。

- 文件缺失或校验失败时，从可信归档重新解压到新目录。保留原状态目录，需要继续使用时传入 `-StateDirectory`。
- 如果 Vulkan 加载器缺失，安装或修复显卡厂商的驱动，再运行检查。如果 Microsoft 安装程序失败，查看脚本报告的日志路径。离线机器可通过受信任渠道取得 Microsoft 官方 x64 运行库安装程序。
- 启动失败时检查状态目录的 `*.err`。若端口冲突，停止自己的旧实例或改用空闲端口。若 Vulkan 无法加载模型，检查 `llama-server.err` 中的设备与 offload 信息。
