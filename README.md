# Mister 下载器（飞牛 fnOS 原生应用）

把开源项目 [Mister Download Manager](https://github.com/anefzaoui/misterdownloadmanager)（MDM 0.5.2）封装为飞牛 fnOS 原生 `.fpk` 应用：内置官方 Linux x86_64 无界面下载引擎，配合中文 Web 界面、登录门禁与更新检测，安装即用，不依赖 Docker。

> 本项目是非官方打包，与 MDM 作者无隶属关系。上游以 **GPL-3.0** 发布，本仓库同样以 GPL-3.0 发布，并随包附带上游许可证、署名说明与对应源码。
>
> 仅支持 **x86 / x86_64** 飞牛设备（fnOS 平台标识为 `x86`），安装与升级脚本会明确拒绝 ARM 设备。

## 交付物

| 路径 | 说明 |
| --- | --- |
| `mister-downloader_1.0.3_x86.fpk` | 可直接安装的飞牛应用包（x86） |
| `SHA256SUMS.txt` | 安装包与许可证文件的 SHA-256 |
| `fpk/src/` | FPK 构建目录（manifest、生命周期脚本、网关入口、图标） |
| `app/` | Go 适配层源码（登录/角色、下载引擎守护、网关反代、更新检测） |
| `build.sh` | 可复现构建脚本（x86_64 交叉编译 + fnpack 打包） |
| `tools/patch-fpk.py` | 修正 Windows 下 fnpack 丢失可执行位，并重算 manifest 校验和 |
| `logs/` | 本次构建与打包日志 |

应用内更新检测固定读取 **Gitee** 仓库 `https://gitee.com/wenming760/mister-downloader-fnos` 的最新 Release，不会访问 GitHub。

## 安装

1. 打开飞牛 fnOS 应用中心 → 手动安装，选择 `mister-downloader_1.0.3_x86.fpk`。
2. 安装完成后，在桌面打开“Mister 下载器”（任意能打开应用的飞牛用户均可）。
3. 首次打开会要求创建**应用管理员**（与飞牛系统账户相互独立），不限制飞牛账户身份。
4. 登录后进入应用自带的下载面板：粘贴直链或磁力链接即可下载，顶部可切换“原版”MDM 页面；未登录时整个应用仅显示登录框，不暴露任何下载内容或接口。
5. 在“设置”中填写下载目录，例如 `/vol2/1000/临时文件`，保存后引擎会自动重启。

### 账户与密码

- 角色：**管理员**可管理下载目录、应用用户、查看服务日志；**运维员**只能使用下载功能。
- 密码**不限制长度与字符组合**，非空即可；界面提供“小眼睛”切换明文。
- 密码以 PBKDF2-HMAC-SHA256（210000 轮）加盐存储，会话 Cookie 为 HttpOnly + SameSite=Lax，7 天滑动有效期。
- 若浏览器或内嵌窗口丢弃 Cookie，界面会自动改用会话令牌请求头兜底，保证登录后仍能进入。
- 登录失败限制：同一 IP 每分钟最多 8 次。

## 目录、端口与权限

| 项目 | 位置 |
| --- | --- |
| 应用文件 | fnOS 应用目标目录（`TRIM_APPDEST`），含 `mdm-fnos-server`、`misterdownloadmanager`、`misterdownloadmanager-cli` |
| 数据目录 | `TRIM_PKGVAR`：账户库、`mdm/`（引擎数据与配对令牌）、`run/server.pid`、`logs/server.log`、`logs/mdm.log` |
| 下载目录 | 默认 `TRIM_PKGVAR/downloads`，可在“设置”中改为任意绝对路径（含 `/vol2/1000/...` 中文目录；不存在会自动创建，并沿用父目录属主） |
| 运行身份 | `root`（`config/privilege` 中 `run-as: root`），以便直接写入飞牛共享文件夹 |
| 网关 | Unix Socket `app.sock`，飞牛网关前缀 `/app/mister-downloader` |
| 本机监听 | `127.0.0.1:8089`（备用直连）；下载引擎 `127.0.0.1:18643`（仅回环） |

说明：

- 下载引擎以 `--headless` 方式运行，只依赖 `libc`/`libm`/`libgcc_s`（需要 glibc ≥ 2.35，fnOS 1.x 满足）；Go 适配层为静态链接。
- 应用进程启动时会清理 `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`，避免 NAS 上的代理变量影响下载速度或导致下载卡住。
- 适配层会自动完成“应用 ↔ 下载引擎”的配对并持久化令牌，用户无需手动输入配对码。
- 下载面板由应用自身实现，直接调用引擎接口（`/fnapi/tasks`），不依赖内嵌原版页面；原版页面仍保留为可选视图，其 `Content-Security-Policy`/`X-Frame-Options` 由适配层剥离并改写 Host/Origin；`/api/forget`（解绑设备）被禁止，避免破坏自动配对。
- 面板请求日志写在 `logs/server.log`：只记录失败、慢请求和面板首页加载，便于在设备上排查“引擎没有响应”。

## 更新

- “更新”对话框会请求 Gitee 最新 Release（缓存 10 分钟）并比较版本号，展示更新说明与该 Release 中的 `.fpk` 下载链接。
- fnOS 不会自动替换应用文件，检测到新版本后请下载对应 `.fpk`，在应用中心手动安装升级；升级脚本会保留数据目录并重启服务。
- 发布新版本时，GitHub 与 Gitee 的 tag、Release、`.fpk` 资产保持同步，Gitee 作为应用内更新源。

## 构建（x86_64）

环境要求：Go ≥ 1.24、Linux 或可交叉编译的环境、`fnpack` 1.2.3、Python 3。

```bash
# 1. 构建 Linux x86_64 适配层（静态）
cd app
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o ../fpk/src/app/mdm-fnos-server .

# 2. 放入上游 0.5.2 Linux x86_64 二进制
cp <upstream>/misterdownloadmanager     ../fpk/src/app/
cp <upstream>/misterdownloadmanager-cli ../fpk/src/app/
chmod 0755 ../fpk/src/app/{mdm-fnos-server,misterdownloadmanager,misterdownloadmanager-cli}

# 3. 打包并修正权限/校验和
cd ../fpk
fnpack build -d src
cd ..
python tools/patch-fpk.py fpk/mister-downloader.fpk mister-downloader_1.0.3_x86.fpk
sha256sum mister-downloader_1.0.3_x86.fpk
```

上游 Linux 发行包获取：<https://github.com/anefzaoui/misterdownloadmanager/releases/tag/v0.5.2>（文件 `MisterDownloadManager-0.5.2-linux-x86_64.tar.gz`）。

Windows 下 `fnpack` 会把文件写成 `0666` 且不会更新 `checksum`，因此**必须**执行 `tools/patch-fpk.py`。

## 验证状态

- 适配层已在本机用同一版本的上游 0.5.2 `--headless` 引擎完成端到端验证：首次初始化、错误/正确密码登录、原生面板加载、添加任务（HTTP 直链与磁力链接）、暂停/继续/移除、文件落盘并校验、修改下载目录后重启、角色限制、CSRF 头校验、配对令牌自动恢复。
- FPK 已用 `fnpack` 1.2.3 构建并校验：`platform=x86`、`checksum` 等于实际 `app.tgz` 的 MD5、`cmd/*` 与三个二进制均为 `0755`、全部 ELF 为 x86-64。
- **尚未在真实飞牛设备上重新安装 1.0.3 验证**。若原生面板仍提示引擎未就绪，请提供应用数据目录下 `logs/server.log`（已包含面板请求与引擎配对记录）。

## 许可与署名

- 上游：Mister Download Manager 0.5.2，作者 anefzaoui，GPL-3.0。
- 随包文件：`licenses/LICENSE`、`licenses/ATTRIBUTION.md`、`licenses/SOURCE.txt`、`licenses/upstream-source-0.5.2.tar.gz`。
- 上游源码：<https://github.com/anefzaoui/misterdownloadmanager>
- 本项目源码：<https://github.com/wenming758/mister-downloader-fnos> / <https://gitee.com/wenming760/mister-downloader-fnos>
