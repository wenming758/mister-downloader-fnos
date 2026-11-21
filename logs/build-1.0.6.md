# 1.0.6 构建与验证记录（2026-11-21）

## 变更

- 磁力添加不再“没反应”：适配层在 `POST /fnapi/tasks` 里识别 magnet，记录一个等待中的磁力任务，并把 `pending` 列表并进 `GET /fnapi/tasks` 响应；界面显示“获取种子”卡片、已等待秒数与当前阶段。
- 自动补 tracker：磁力缺少 tracker 时补齐 8 个公开 tracker（含 `tracker.opentrackr.org`、`open.stealth.si`、`tracker.torrent.eu.org` 等 UDP/HTTP 入口）。
- 公开缓存兜底：等待 6 秒仍未拿到文件列表时，从 `https://itorrents.org/torrent/<HASH>.torrent` 取回 `.torrent`，校验 bencode 的 `info` 段 SHA-1 与磁力哈希一致后，交给引擎的 `/api/torrent` 直接建立任务；不依赖 DHT。
- 失败可见：引擎单次元数据等待为 180 秒，适配层 195 秒后把卡片标成“获取失败”，说明原因，并提供“重试”（重新交给引擎查找）与“移除”。
- 等待期间即使引擎重启（例如切换下载目录），缓存兜底仍会把任务加进新目录；添加请求单独使用 30 秒超时，避免目录切换耗时导致误报“引擎未就绪”。
- 更新页：`立即升级` 按钮始终显示并显示当前状态（可升级到 vX / 已是最新版本 / 更新源异常 / 安装包缺失），不再隐藏；升级不再要求管理员，所有已登录账户都可执行，运维员亦可。

## 本地实测（Windows + 上游 0.5.2 headless 引擎）

| 场景 | 结果 |
| --- | --- |
| 用户磁力 `btih:ae31be061ab6c298e785adfb92c3932c34a54013` | 添加后返回 `pending`，界面显示“获取种子”；第 6 秒转入缓存阶段，约第 10 秒任务出现在列表并开始下载，pending 自动清除 |
| 无 tracker 磁力 | 服务端自动补齐 8 个公开 tracker 后交给引擎 |
| 无效磁力（无 btih） | 立即返回 HTTP 400“磁力链接里没有 btih 信息” |
| 无法解析的哈希 | 6 秒缓存回退失败后回到“等待对等方”，界面持续显示已等待秒数；取消可立即移除 |
| 短超时测试构建（12 秒） | 卡片变为“获取失败”，显示原因、弹出提示，并提供“重试/移除”；重试后回到“正在重新查找文件列表…” |
| 切换新下载目录 + 磁力 | 引擎重启后磁力仍经缓存回退加入新目录（`run-107f/dl`） |
| 直链添加（`start:false`） | 返回任务 id，随后可正常删除 |
| 更新页（1.0.6，Gitee 最新 v1.0.5） | 按钮可见且禁用，文案“已是最新版本”，横幅说明当前已是最新 |
| 运维员账户调用升级 | 不再 403，返回“当前已是最新版本” |

`go test ./...` 覆盖 bencode 哈希解析、hex/base32 磁力解析与 tracker 去重，全部通过。

## 产物

| 文件 | 大小 | SHA-256 |
| --- | --- | --- |
| `mdm-fnos-server`（Go 适配层，linux/amd64） | 8,081,568 | `1b1a4ef81ca6b46a1f22e947a3b4d9b5f71790d17c57ebe5ce99e351026e99ac` |
| `misterdownloadmanager`（上游引擎） | 40,464,536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `misterdownloadmanager-cli` | 317,200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.6_x86.fpk` | 52,442,592 | `31913274d929513bd9472487a97a773716b81a0d1fee9d9dcbc18899c20d8ea6` |

`tools/validate-fpk.py` 全部通过（新增检查：更新按钮始终显示状态、磁力 tracker 与缓存回退、等待/失败提示、重试与移除接口）。

## 未验证

- `appcenter-cli install-fpk` 需在真实飞牛设备上执行；本机只验证到下载与校验完成。
- 部分网络会阻断 `itorrents.org`；此时行为退化为 tracker/DHT 查找，失败时界面会明确提示，不再静默。