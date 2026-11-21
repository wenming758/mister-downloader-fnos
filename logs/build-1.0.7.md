# 1.0.7 构建与验证记录（2026-11-21）

## 问题定位

用户反馈 `magnet:?xt=urn:btih:292e1a6a…&tr=https://004430.xyz/announce` 在迅雷稳定 8MB/s，在本应用里没有速度。逐一排查后确认四层原因：

1. **1.0.6 的缓存兜底产物没有 tracker。** `itorrents.org` 取回的 `.torrent` 只有 `info` 段，没有 `announce`/`announce-list`，引擎只能靠 DHT 找 peer；本机与飞牛所在网络到 `dht.libtorrent.org:25401` 的引导均失败，任务因此永久 0 速度。这是真正的代码缺陷。
2. **公开 tracker 列表质量差。** 旧的 8 个条目以 UDP 为主且多条已失效；测试环境下 UDP tracker 全部不可达，导致兜底注入也没有实际效果。
3. **资源本身只对迅雷供源。** `004430.xyz` 返回 192 字节 peer（端口集中在 15000/15001，多数不可达），引擎能建立 26–39 条连接但拿不到数据；同机 libtorrent 70 秒仅 84KB/s，而同网络 Debian 13.7 netinst 在 rqbit 下可达 17–22MB/s，说明引擎与网络正常，是该中文资源只给迅雷客户端供源。
4. **零速度无提示。** 这种情况此前界面只显示“下载中 0 B/s”，用户无法判断是等待还是失败。

## 变更

- 磁力取回的种子在交给引擎前，统一写入磁力 URL 里的 `tr` 与扩充后的公开 tracker：改写 `announce`/`announce-list`，**保持 `info` 段字节不变**（哈希不变），bencode 顶层键按字典序重排。
- 公开 tracker 从 8 个扩充为 14 个，HTTP/HTTPS 在前、UDP 在后：`004430.xyz`、`tracker.dler.com`、`bt1/bt2.archive.org`、`1337.abcvg.info`（80/443）、`tracker.nekomi.cn`、`tracker.renfei.net`、`tracker.dhitechnical.com`、`tracker.7471.top`、`tracker.opentrackr.org`、`open.stealth.si`、`tracker.torrent.eu.org`、`exodus.desync.com`。
- 任务运行超过 2 分钟、已下载为 0 且速率为 0 时，卡片显示提示：该资源可能只有迅雷等私有节点在供源，建议换资源或使用直链。

## 本地实测

| 场景 | 结果 |
| --- | --- |
| `ae31be06…` 无 tracker 磁力 | 第 8 秒出现在任务列表；保存的 `.torrent` 含 14 个 tracker |
| `292e1a6a…` 用户磁力 | 缓存回退后保存的种子包含 `004430.xyz` 及全部公开 tracker；tracker 在约 2 秒内返回 peers |
| 同资源对照 | rqbit 建立 26–39 条连接、速率≈0；libtorrent 70 秒 84KB/s、3 seeds |
| 引擎/网络对照 | Debian 13.7 netinst 在 rqbit 下 17–22MB/s |
| 零速度提示 | 超过 2 分钟无数据时出现提示，恢复有速度后自动消失 |
| 无效磁力 | 仍立即返回 HTTP 400 |

`gofmt -l`、`go vet ./...`、`go test ./...` 全部通过；测试覆盖 bencode 哈希解析、hex/base32 磁力解析、tracker 去重与“写入 tracker 后哈希不变”。

## 产物

| 文件 | 大小 | SHA-256 |
| --- | --- | --- |
| `mister-downloader_1.0.7_x86.fpk` | 52,448,015 | `8ede89545788bc68284c2d92993d126f5a25d46719158f6892335a9c480ca937` |

`tools/validate-fpk.py` 全部通过（新增检查：缓存种子写入 tracker、零速度提示）。

## 未验证 / 已知限制

- 真实飞牛设备尚未安装 1.0.7，`appcenter-cli install-fpk` 环节仍需设备验证。
- 仅由迅雷节点供源的中文资源，任何标准 BT 客户端都无法获得迅雷的加速带宽；本版本只能明确提示，不能绕过。
- 若网络同时阻断 `itorrents.org` 与全部 tracker，磁力仍需等待引擎自身的 DHT/本地 peer，界面会在 195 秒后给出失败原因。