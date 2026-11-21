# 构建记录 1.0.3（x86_64）

- 日期：2026-11-21
- 宿主：Windows 10 x64（Git Bash）
- Go：1.27.3 交叉编译 linux/amd64，CGO 关闭
- fnpack：1.2.3；打包后由 tools/patch-fpk.py 修正 0755、重算 checksum、manifest 统一 LF
- 上游引擎：Mister Download Manager 0.5.2 Linux x86_64

## 变更

- 设备上内嵌的原版页面提示“MDM 没有响应”，整个应用不可用；现改为**应用原生下载界面**，直接经 `/fnapi/tasks` 调用引擎 `/api/list`、`/api/add`、`/api/act`，不再依赖 iframe，原版面板仍可一键切换。
- 原生界面支持添加 http(s)/磁力/FTP 链接、实时进度与速度、暂停/继续/移除/删除文件；确认框改为应用内弹窗。
- 应用运行身份改为 root，可直接写入 `/vol2/1000/临时文件` 等共享目录；下载目录不存在时自动创建并沿用父目录属主。
- 新增面板请求日志（`/panel*`、`/fnapi/` 失败或超 3 秒时记录），便于在设备上排查。

## 本机会话测试（Windows + 同一适配层）

- 同一适配层 + 上游 0.5.2 `--headless` 本地端到端：初始化、登录、任务列表、添加 GHO 直链（约 60–109 MB/s）、添加磁力（返回 `pending` 等待元数据）、暂停（约 3 秒延迟）、继续、暂停后移除、非法链接返回中文 400。
- 网关前缀 `/app/mister-downloader/` 路径下访问正常；原版面板切换可加载。
- 截图：`logs/ui-1.0.3-native.png`（原生界面）、`logs/ui-1.0.3-legacy.png`（原版面板）。

## 产物

| 文件 | 字节 | SHA-256 |
| --- | --- | --- |
| `fpk/src/app/mdm-fnos-server` | 7880864 | `20c84b4cfa4b232ebba4d3a43e51e325a4d079eb7bec06d291267cc236848277` |
| `fpk/src/app/misterdownloadmanager` | 40464536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `fpk/src/app/misterdownloadmanager-cli` | 317200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.3_x86.fpk` | 52357993 | `4fdfd71c55163ee0fbec6a9d4bafad0f1b3bf06d0fa5fec6e76f036ee98158e4` |

- 打包校验：`logs/validate-1.0.3.txt`（RESULT: OK，含外层结构/权限/checksum、内层 ELF64、内嵌原生界面标记）。

## 未验证项

- 未在真实飞牛 fnOS 设备/虚拟机上安装 1.0.3 验证（构建环境无 fnOS 主机）。
- 上游二进制依赖 glibc ≥ 2.35，需 fnOS 1.x（Debian 12 基线）。
