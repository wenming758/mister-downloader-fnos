#!/usr/bin/env python3
"""校验 fnOS FPK 包结构与内容（x86）。用法: python tools/validate-fpk.py <fpk> [期望版本]"""
import hashlib
import io
import struct
import sys
import tarfile

fails = []


def check(cond, label):
    print(("PASS " if cond else "FAIL ") + label)
    if not cond:
        fails.append(label)


def png_size(data):
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        return (0, 0)
    w, h = struct.unpack(">II", data[16:24])
    return (w, h)


def elf_arch(data):
    if data[:4] != b"\x7fELF":
        return "not-elf"
    return "x86-64" if data[4] == 2 and struct.unpack("<H", data[18:20])[0] == 62 else "other"


def main():
    path = sys.argv[1]
    want = sys.argv[2] if len(sys.argv) > 2 else ""
    with tarfile.open(path, "r:gz") as outer:
        members = {m.name: m for m in outer.getmembers()}
        for name in ["manifest", "ICON.PNG", "ICON_256.PNG", "app.tgz",
                     "config/privilege", "config/resource"]:
            check(name in members, "外层包含 " + name)
        for name in ["config_init", "config_callback", "install_init", "install_callback",
                     "upgrade_init", "upgrade_callback", "uninstall_init", "uninstall_callback", "main"]:
            m = members.get("cmd/" + name)
            check(m is not None and m.mode & 0o777 == 0o755, "cmd/%s 存在且 0755" % name)
        for name in ["install_callback", "upgrade_callback"]:
            m = members.get("cmd/" + name)
            raw = outer.extractfile(m).read() if m is not None else b""
            check(b"chown -R" not in raw, "%s 不递归 chown 下载目录" % name)

        manifest = outer.extractfile("manifest").read()
        check(b"\r" not in manifest, "manifest 为 LF 换行")
        text = manifest.decode("utf-8")
        fields = {}
        for line in text.splitlines():
            if "=" in line and not line.strip().startswith("#"):
                k, v = line.split("=", 1)
                fields[k.strip()] = v.strip()
        check(fields.get("appname") == "mister-downloader", "appname")
        check(fields.get("version") == want if want else bool(fields.get("version")),
              "version=%s" % fields.get("version"))
        check(fields.get("platform") == "x86", "platform=x86")
        check(fields.get("desktop_applaunchname") == "mister-downloader.main", "applaunch")
        check(fields.get("desktop_uidir") == "ui", "desktop_uidir")

        app = outer.extractfile("app.tgz").read()
        check(fields.get("checksum", "").lower() == hashlib.md5(app).hexdigest(), "checksum == app.tgz MD5")

        priv = outer.extractfile("config/privilege").read().decode("utf-8")
        check('"run-as": "root"' in priv.replace("\n", " "), "privilege run-as root")
        check('"username"' in priv, "privilege username")

        check(png_size(outer.extractfile("ICON.PNG").read()) == (64, 64), "ICON.PNG 64x64")
        check(png_size(outer.extractfile("ICON_256.PNG").read()) == (256, 256), "ICON_256.PNG 256x256")

        with tarfile.open(fileobj=io.BytesIO(app), mode="r:gz") as inner:
            im = {m.name: m for m in inner.getmembers()}
            for binname in ["mdm-fnos-server", "misterdownloadmanager", "misterdownloadmanager-cli"]:
                m = im.get(binname)
                ok = m is not None and m.size > 0 and m.mode & 0o777 == 0o755
                check(ok, binname + " 存在、非空、0755")
                if ok:
                    data = inner.extractfile(m).read(64)
                    check(elf_arch(data) == "x86-64", binname + " ELF64 x86-64")
            for lic in ["licenses/LICENSE", "licenses/ATTRIBUTION.md",
                        "licenses/SOURCE.txt", "licenses/upstream-source-0.5.2.tar.gz"]:
                check(lic in im, "包含 " + lic)
            ui = im.get("ui/config")
            check(ui is not None, "包含 ui/config")
            if ui is not None:
                uitext = inner.extractfile(ui).read().decode("utf-8")
                check('"gatewayPrefix": "/app/mister-downloader"' in uitext, "网关前缀")
                check('"gatewaySocket": "app.sock"' in uitext, "网关 Socket")
                check('"allUsers": true' in uitext, "所有用户可见")
            server = im.get("mdm-fnos-server")
            if server is not None:
                blob = inner.extractfile(server).read()
                check(b"magnet:?xt=" in blob and b"tasks/act" in blob, "内置原生下载面板")
                check(b"askConfirm" in blob, "应用内确认弹窗")
                check(b"download_dir" in blob, "下载目录设置接口")
                check("优化版".encode("utf-8") in blob and "原生版".encode("utf-8") in blob,
                      "优化版/原生版视图切换")
                check(b"add-dlg-dir" in blob, "添加下载时选择保存目录")
                check("删除下载".encode("utf-8") in blob and "磁盘上已下载的文件保留".encode("utf-8") in blob
                      and "无痕删除".encode("utf-8") in blob, "删除下载/无痕删除双选项")
                check(b".mdm.part" in blob and "未完成的分片".encode("utf-8") in blob,
                      "无痕删除清理未完成分片")
                check(b"appcenter-cli" not in blob and b"install-fpk" not in blob
                      and b"update_resume.json" in blob, "应用内升级不再依赖应用中心")
                check("立即升级".encode("utf-8") in blob and b"upd-fill" in blob, "升级进度条")
                check(b"/fnapi/engine" in blob and "内核升级".encode("utf-8") in blob
                      and b"SHA256SUMS.txt" in blob, "独立内核升级（上游 GitHub）")
                check(b"history.json" in blob and "下载记录".encode("utf-8") in blob
                      and "再次下载".encode("utf-8") in blob, "下载记录与再次下载")
                check(b"dir-options" in blob and "dir_history".encode("utf-8") in blob, "下载目录历史")
                check("已是最新版本".encode("utf-8") in blob, "更新按钮始终显示当前状态")
                check(b"itorrents.org" in blob and b"tracker.opentrackr.org" in blob,
                      "磁力补齐 tracker 与公开种子缓存回退")
                check("获取种子".encode("utf-8") in blob and "获取失败".encode("utf-8") in blob,
                      "磁力等待/失败提示")
                check(b"pending/dismiss" in blob and b"pending/retry" in blob, "磁力任务重试与移除")
                check(b"torrentWithTrackers" in blob and b"tracker.dler.com" in blob
                      and b"004430.xyz" in blob, "缓存种子写入磁力与公开 tracker")
                check("迅雷等私有节点".encode("utf-8") in blob, "长时间零速度提示")
                check("已升级到 v".encode("utf-8") in blob and b"backup-" in blob,
                      "应用内升级可回滚自愈")
                check(b"btn-refresh" in blob and "刷新".encode("utf-8") in blob, "顶部刷新按钮")
        check(fields.get("maintainer") == "Ahmed Nefzaoui", "开发者 = 上游原作者")
        check(fields.get("maintainer_url") == "https://github.com/anefzaoui", "开发者链接")
        check(fields.get("distributor") == "wenming758", "发布者 = wenming758")
        check(fields.get("distributor_url") == "https://gitee.com/wenming760", "发布者链接")

    print()
    if fails:
        print("RESULT: FAIL (%d)" % len(fails))
        return 1
    print("RESULT: OK")
    data = open(path, "rb").read()
    print("sha256: %s size: %d" % (hashlib.sha256(data).hexdigest(), len(data)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
