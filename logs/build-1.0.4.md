# 1.0.4 构建与验证记录（2026-11-21）

## 变更

- 删除任务：面板“删除”改为先暂停，等待任务真正停止（最长 15 秒）后调用引擎 `delete`，由引擎清除磁盘上的已下载部分；按钮文案明确“并清除磁盘上已下载的部分？此操作不可恢复”。
- 添加下载：点击“添加下载”弹出对话框，可直接填写/修改保存目录（服务器绝对路径）；目录不存在自动创建；目录变化时写入授权库并重启引擎，重启后自动续传此前处于下载中的任务；同目录不重启。
- 视图更名：上游 GitHub 页面称“原生版”，本应用改造的中文面板称“优化版”；默认显示优化版，顶栏按钮在两者间切换。
- manifest：开发者改为上游原作者 Ahmed Nefzaoui / https://github.com/anefzaoui；发布者仍为 wenming758 / https://gitee.com/wenming760。
- 确认弹窗改为紧凑布局，短窗口中按钮不再被裁切；添加对话框使用固定底栏。

## 本地实测（Windows + 上游 0.5.2 headless 引擎）

| 场景 | 结果 |
| --- | --- |
| 添加时切换到 `...\run-104\target3` | 引擎重启，日志记录新目录并续传，文件实际落在 target3 |
| 同一路径保存设置 | 不重启引擎，直接返回“已保存” |
| 下载中点击“删除” | ~4.9 s 返回 `{"done":[1]}`，`.mdm.part` 与 `.mdm.json` 均被移除 |
| 磁力链接 | 解析为任务并落入 `<目录>\Torrents` |
| 顶栏切换 | 默认“优化版”，点击切换为上游“原生版”页面 |
| 确认弹窗 / 添加弹窗 | 460px 高窗口下底栏按钮完整可见，正文可滚动（截图见 `ui-1.0.4-confirm.png`、`ui-1.0.4-add.png`） |

## 产物

| 文件 | 大小 | SHA-256 |
| --- | --- | --- |
| `mdm-fnos-server`（Go 适配层，linux/amd64） | 7,893,152 | `b84a5c5ebcd12607010fecf1dee2bd13a0e54b74a47f7e9f2f15210019696194` |
| `misterdownloadmanager`（上游引擎） | 40,464,536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `misterdownloadmanager-cli` | 317,200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.4_x86.fpk` | 52,362,127 | `4c8a2149c059c07663cd2ee4479daafc24f8361daae9c137e7bd8be1312079a6` |

`tools/validate-fpk.py` 全部通过（含 `platform=x86`、checksum 等于 app.tgz MD5、ELF64 x86-64、优化版/原生版标记、添加目录对话框、开发者与发布者字段）。

## 未验证

- 尚未在真实飞牛设备上安装 1.0.4；安装后如需排查请附 `TRIM_PKGVAR/logs/server.log`。