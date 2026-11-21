# 1.0.5 构建与验证记录（2026-11-21）

## 变更

- 应用内升级：管理员在“检查更新”里点击“立即升级”，应用从 Gitee Release 下载 `.fpk`，按 `SHA256SUMS.txt` 校验 SHA-256，再校验 manifest 版本、`app.tgz` 的 MD5 与主程序 ELF 架构，最后调用飞牛自带的 `appcenter-cli install-fpk` 完成安装并重启。界面显示下载/校验/安装进度，重启后继续显示完成状态。
- 升级前后运行中的任务会记录到 `update_resume.json`，服务恢复后自动继续。
- 升级状态持久化在 `updates/status.json`，重启期间仍可通过公开的 `/fnapi/update/status` 读取。
- 目录历史：账户配置新增 `dir_history`（最多 8 条），添加下载/settings 时记录；添加对话框在输入框下方显示可点击的最近目录标签，并给输入框挂 `datalist` 下拉。
- 登录页版本号改为从 `/fnapi/me` 的 401 响应读取，避免静态占位过期。

## 本地实测（Windows + 上游 0.5.2 headless 引擎）

| 场景 | 结果 |
| --- | --- |
| 设置两个目录后 `/fnapi/me` | `dir_history` 依次为 d2、d1 |
| 添加对话框 | 输入框预填当前目录，下方显示两条目录标签，输入框带下拉建议 |
| 点击“立即升级”（测试版本 1.0.3 → 1.0.4） | 界面进度 22% → 下载完成 52,362,127 B → SHA-256/manifest/MD5/ELF 校验通过 → 显示“正在安装” |
| 无 appcenter-cli 的 Windows 环境 | 停在“当前系统不支持应用内升级”，按钮恢复可用，状态接口返回 `error` 与原因 |

## 产物

| 文件 | 大小 | SHA-256 |
| --- | --- | --- |
| `mdm-fnos-server`（Go 适配层，linux/amd64） | 8,016,032 | `0c5f6b503f9a310c34a999e0bd1e7347b8fcad4861adddaa432e5e3eee19eab0` |
| `misterdownloadmanager`（上游引擎） | 40,464,536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `misterdownloadmanager-cli` | 317,200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.5_x86.fpk` | 52,413,661 | `4af53c3775fd3495fb360e2f62ca5a1d86bcc4cfec4d52a423327296e5e1249d` |

`tools/validate-fpk.py` 全部通过（含 `platform=x86`、checksum 等于 app.tgz MD5、ELF64 x86-64、立即升级/进度条、目录历史、开发者与发布者字段）。

## 未验证

- `appcenter-cli install-fpk` 需在真实飞牛设备上执行；本机只验证到下载与校验完成。
- 升级后的任务自动续传同样依赖设备实测。