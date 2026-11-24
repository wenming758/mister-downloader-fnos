# Mister 下载器（飞牛 fnOS 原生应用）

把开源项目 [Mister Download Manager](https://github.com/anefzaoui/misterdownloadmanager)（MDM 0.5.2）封装为飞牛 fnOS 原生 `.fpk` 应用：内置官方 Linux x86_64 无界面下载引擎，配合中文 Web 界面、登录门禁与更新检测，安装即用，不依赖 Docker。

> 本项目是非官方打包，与 MDM 作者无隶属关系。上游以 **GPL-3.0** 发布，本仓库同样以 GPL-3.0 发布，并随包附带上游许可证、署名说明与对应源码。
>
> 仅支持 **x86 / x86_64** 飞牛设备（fnOS 平台标识为 `x86`），安装与升级脚本会明确拒绝 ARM 设备。

## 交付物

| 路径 | 说明 |
| --- | --- |
| `mister-downloader_1.0.11_x86.fpk` | 可直接安装的飞牛应用包（x86） |
| `SHA256SUMS.txt` | 安装包与许可证文件的 SHA-256 |
| `fpk/src/` | FPK 构建目录（manifest、生命周期脚本、网关入口、图标） |
| `app/` | Go 适配层源码（登录/角色、下载引擎守护、网关反代、更新检测） |
| `build.sh` | 可复现构建脚本（x86_64 交叉编译 + fnpack 打包） |
| `tools/patch-fpk.py` | 修正 Windows 下 fnpack 丢失可执行位，并重算 manifest 校验和 |
| `logs/` | 本次构建与打包日志 |

应用内“程序升级”固定读取 **Gitee** 仓库 `https://gitee.com/wenming760/mister-downloader-fnos` 的最新 Release；“内核升级”读取上游 GitHub 发布。

## 安装

1. 打开飞牛 fnOS 应用中心 → 手动安装，选择 `mister-downloader_1.0.11_x86.fpk`。
2. 安装完成后，在桌面打开“Mister 下载器”（任意能打开应用的飞牛用户均可）。
3. 首次打开会要求创建**应用管理员**（与飞牛系统账户相互独立），不限制飞牛账户身份。
4. 登录后进入应用自带的**优化版**下载面板：粘贴直链或磁力链接即可下载；顶部按钮可切换到上游的**原生版** MDM 页面。未登录时整个应用仅显示登录框，不暴露任何下载内容或接口。
5. 点击“添加下载”时会弹出对话框，可直接填写或修改**保存目录**（服务器绝对路径，如 `/vol2/1000/临时文件`）；目录不存在会自动创建，最近用过的目录会显示为可点击的路径标签，也可从输入框下拉选择。更换目录时引擎重启一次，运行中的任务会自动继续。
6. 任务卡片的“删除”会弹出两个选项：**删除下载** 只把任务移出列表，磁盘上已下载的文件保留；**无痕删除** 删除任务、磁盘上的全部文件（含未完成的分片与临时文件）以及对应的下载记录，不留痕迹。两者都会先暂停仍在运行的任务。
7. 顶部“记录”按钮打开下载记录：按最近更新时间列出任务的名字、状态、大小、保存目录与添加时间，最多保留 500 条；每条可“再次下载”，也可清空记录（只清记录，不删文件）。
8. 顶部“刷新”按钮会立即重新读取任务列表与下载进度，可用于查看其他设备新增的任务；刷新期间图标旋转，完成后提示当前任务数与等待获取种子的数量。
9. 顶部只保留 **刷新 / 记录 / 设置 / 退出**；“程序与内核更新”“账户管理”“切换原生版”都在“设置”对话框内，按“界面 / 账户 / 版本与更新”分区，运维员看不到账户管理。
10. 在“设置”中可修改默认下载目录，保存后同样会重启引擎。

### 磁力链接

- 添加磁力后会立即出现一张“获取种子”卡片，显示已经等待的秒数和当前阶段；拿到文件列表后卡片会自动变成正常任务。
- 从磁力取回的种子会保留磁力自带的 `tr`，并补齐一组共 14 个公开 tracker（HTTP/HTTPS 优先，兼顾国内网络）；经公开种子缓存（itorrents）兜底取回的 `.torrent` 也会写入同一组 tracker，不再出现“任务建好了但只能靠 DHT、永远 0 速度”的情况。
- 3 分钟内仍失败时，卡片会变成“获取失败”并说明原因，可直接“重试”或“移除”，不会再出现“添加后毫无反应”的情况。
- 任务已运行超过 2 分钟仍然没有任何数据时，卡片会提示“该资源可能只有迅雷等私有节点在供源”：这类中文资源对标准 BT 客户端基本不供速，属资源本身限制，换资源或改用直链即可。
- 公开缓存只提供种子元数据，实际下载仍由你的设备完成。

### 账户与密码

- 角色：**管理员**可管理下载目录、应用用户、查看服务日志；**运维员**可以使用下载功能和“立即升级”，但不能管理账户与设置。
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
- 下载面板由应用自身实现，直接调用引擎接口（`/fnapi/tasks`），不依赖内嵌原版页面；上游页面仍保留为可选的“原生版”视图，其 `Content-Security-Policy`/`X-Frame-Options` 由适配层剥离并改写 Host/Origin；`/api/forget`（解绑设备）被禁止，避免破坏自动配对。
- 面板请求日志写在 `logs/server.log`：只记录失败、慢请求和面板首页加载，便于在设备上排查“引擎没有响应”。

## 更新

更新对话框分为两条独立通道：

**程序升级（Gitee）**
- 请求 Gitee 最新 Release（缓存 10 分钟）并比较版本号，展示更新说明；所有已登录账户都可以升级。
- 点击“立即升级”后，应用在后台下载 `.fpk`，校验 SHA-256、manifest 版本、`app.tgz` 的 MD5 与主程序架构，然后暂存新文件、备份旧文件，再用“临时文件 + rename”原子替换，最后用 `syscall.Exec` 原地重启（进程号不变，不依赖应用中心，也不会出现 `Text file busy`）。
- 不再调用 `appcenter-cli install-fpk`；升级期间正在下载的任务会被记录，服务恢复后自动继续。
- 替换失败会自动回滚；启动时若发现版本已达到目标版本（例如手动装过新包），会直接标记升级完成。

**内核升级（上游 GitHub）**
- 读取上游 `anefzaoui/misterdownloadmanager` 的最新 Release，只使用 `-linux-x86_64.tar.gz` 与 `SHA256SUMS.txt` 两个资产。
- 下载后校验 SHA-256，只解出 `misterdownloadmanager` 与 `misterdownloadmanager-cli` 并确认是 x86-64 ELF，停止引擎、替换二进制、写入 `mdm/engine-version`，再启动引擎并等待就绪；失败自动回滚。
- 程序升级不会覆盖内核二进制，避免把新内核降级回包内自带的 0.5.2。

**从 1.0.9 及更早版本升级**
- 旧版本的应用内升级实现有缺陷（路径错误、依赖应用中心、覆盖运行中二进制失败），可能停在 92%。请先在应用中心**手动安装一次** `mister-downloader_1.0.10_x86.fpk`，从 1.0.10 起后续版本即可正常应用内升级。
- 发布新版本时，GitHub 与 Gitee 的 tag、Release、`.fpk` 资产保持同步。

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
python tools/patch-fpk.py fpk/mister-downloader.fpk mister-downloader_1.0.11_x86.fpk
sha256sum mister-downloader_1.0.11_x86.fpk
```

上游 Linux 发行包获取：<https://github.com/anefzaoui/misterdownloadmanager/releases/tag/v0.5.2>（文件 `MisterDownloadManager-0.5.2-linux-x86_64.tar.gz`）。

Windows 下 `fnpack` 会把文件写成 `0666` 且不会更新 `checksum`，因此**必须**执行 `tools/patch-fpk.py`。

## 验证状态

- 适配层已在本机用同一版本的上游 0.5.2 `--headless` 引擎完成端到端验证：首次初始化、错误/正确密码登录、原生面板加载、添加任务（HTTP 直链与磁力链接）、暂停/继续/彻底删除、文件落盘并校验、修改下载目录后重启并且运行中任务自动继续、角色限制、CSRF 头校验、配对令牌自动恢复。
- 1.0.4 变更点均已实测：下载中直接“删除”会在约 5 秒内清掉任务与 `.mdm.part`/`.mdm.json`；添加下载时可选择新目录，引擎重启并把运行中的任务续传；空目录场景会先确认再写入；同一目录保存不会重启引擎；界面按钮显示“优化版/原生版”，默认优化版。
- 1.0.5 新增能力已实测：目录历史会持久化并显示为可点击标签；应用内升级完成了真实下载（Gitee 最新 `.fpk`，52,362,127 字节）、SHA-256/manifest/MD5/ELF 架构校验与进度汇报，在没有 `appcenter-cli` 的 Windows 测试环境正确停在“不支持应用内升级”并给出提示。
- 1.0.6 磁力修复已实测：用户给出的磁力 `magnet:?xt=urn:btih:ae31be061ab6c298e785adfb92c3932c34a54013` 添加后先显示等待卡片，约 10 秒内经公开缓存取得文件列表并出现在任务列表（本机 DHT 被网络阻断时也能完成）；无 tracker 的磁力会自动补齐 8 个公开 tracker；无效磁力立即报错；无法解析的哈希在超时后显示“获取失败”并提供重试/移除；重试会重新进入等待状态。
- 1.0.6 更新页已实测：按钮始终可见，无更新时显示“已是最新版本”并禁用；运维员账户也能进入升级流程（无更新时返回“当前已是最新版本”，不再返回权限错误）。
- FPK 已用 `fnpack` 1.2.3 构建并校验：`platform=x86`、`checksum` 等于实际 `app.tgz` 的 MD5、`cmd/*` 与三个二进制均为 `0755`、全部 ELF 为 x86-64。
- 1.0.7 无速度修复已实测：`magnet:?xt=urn:btih:292e1a6a…` 与 `ae31be06…` 添加后经公开缓存取得文件列表，保存的 `.torrent` 已包含磁力自带 tracker 与 14 个公开 tracker（此前缓存种子没有任何 announce，引擎只能走被网络阻断的 DHT）；用户给出的中文资源在 rqbit 与 libtorrent 上均只得到接近 0 的速率，而同网络下 Debian 镜像可达 17–22MB/s，确认是该资源仅由迅雷节点供源，界面已加入 2 分钟零数据提示。
- 1.0.9 变更：“刷新”按钮调用带 force 的任务接口，即使正处于轮询或页面不可见也会立即请求；安装/升级脚本改为只修正数据目录顶层属主（不再递归 chown 整个下载目录），并在 `server.log` 记录安装阶段时间戳，便于定位应用中心停在 92% 的问题。
- 1.0.8 升级修复已在本地用两种故障注入实测：安装命令直接失败、安装命令挂起（缩短为 3 秒模拟）时，升级脚本都会在限时后改用包内文件覆盖并重新启动应用；覆盖后 `mdm-fnos-server` 与安装包内文件逐字节一致，服务能重新拉起。
- 1.0.10 已在本机（Windows 适配层 + 同一 0.5.2 引擎）实测：下载记录在重启后仍保留；“删除下载”保留磁盘文件并把记录标为已删除；“无痕删除”删除任务、`name`/`name.mdm.part`/`name.mdm.json` 等全部文件并彻底移除记录，其他任务的同名文件不受影响；内核升级通道完成下载、SHA-256 校验与 ELF 检查；程序升级改为原子替换 + `syscall.Exec` 原地重启，启动自愈与回滚逻辑均有单元测试。
- **1.0.10 尚未在真实飞牛设备上验证**。设备若停留在 1.0.8/1.0.9，其应用内升级可能停在 92%，请先在应用中心手动安装一次 `mister-downloader_1.0.10_x86.fpk`；之后即可使用新的应用内升级。若手动安装也失败，请提供应用数据目录下 `logs/server.log`。
- 1.0.11 仅调整界面布局：“更新、账户、原生版”收进“设置”，并在浏览器实测（管理员可见三个分区、运维员看不到账户管理、原生版切换与两个弹窗均正常）；下载、记录、删除与升级逻辑未改动。
- 应用内升级日志位于应用数据目录 `logs/update.log`，安装失败的原因会直接显示在升级页。

## 许可与署名

- 应用信息中的**开发者**为上游原作者：Ahmed Nefzaoui（<https://github.com/anefzaoui>）；**发布者**为本项目打包者：wenming758 / wenming760。
- 上游：Mister Download Manager 0.5.2，作者 anefzaoui，GPL-3.0。
- 随包文件：`licenses/LICENSE`、`licenses/ATTRIBUTION.md`、`licenses/SOURCE.txt`、`licenses/upstream-source-0.5.2.tar.gz`。
- 上游源码：<https://github.com/anefzaoui/misterdownloadmanager>
- 本项目源码：<https://github.com/wenming758/mister-downloader-fnos> / <https://gitee.com/wenming760/mister-downloader-fnos>
