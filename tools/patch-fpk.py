import hashlib
import io
import os
import re
import sys
import tarfile

src, dst = sys.argv[1], sys.argv[2]

EXECUTABLES = {"mdm-fnos-server", "misterdownloadmanager", "misterdownloadmanager-cli"}


def outer_mode(name, isdir):
    if isdir:
        return 0o755
    if name.startswith("cmd/"):
        return 0o755
    return 0o644


def inner_mode(name, isdir):
    if isdir:
        return 0o755
    if name in EXECUTABLES:
        return 0o755
    return 0o644


def rebuild_inner(raw):
    buf = io.BytesIO()
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:gz") as it:
        with tarfile.open(fileobj=buf, mode="w:gz", format=tarfile.GNU_FORMAT) as ot:
            for m in it.getmembers():
                m.mode = inner_mode(m.name, m.isdir())
                m.uid = m.gid = 0
                m.uname = m.gname = ""
                data = it.extractfile(m) if m.isfile() else None
                ot.addfile(m, data)
    return buf.getvalue()


app_md5 = None

with tarfile.open(src, "r:gz") as outer:
    with tarfile.open(dst, "w:gz", format=tarfile.GNU_FORMAT) as out:
        for m in outer.getmembers():
            raw = None
            if m.name == "app.tgz":
                raw = io.BytesIO(rebuild_inner(outer.extractfile(m).read()))
                app_md5 = hashlib.md5(raw.getvalue()).hexdigest()
            elif m.name == "manifest" and app_md5:
                text = outer.extractfile(m).read().decode("utf-8")
                text = text.replace("\r\n", "\n").replace("\r", "\n")
                if re.search(r"(?m)^checksum\s*=", text):
                    text = re.sub(r"(?m)^checksum\s*=\s*[0-9a-fA-F]{32}\s*$",
                                  "checksum                   = " + app_md5, text)
                else:
                    text = text.rstrip("\n") + "\nchecksum                   = " + app_md5 + "\n"
                raw = io.BytesIO(text.encode("utf-8"))
            elif m.isfile():
                raw = outer.extractfile(m)
            m.mode = outer_mode(m.name, m.isdir())
            m.uid = m.gid = 0
            m.uname = m.gname = ""
            if isinstance(raw, io.BytesIO):
                m.size = raw.getbuffer().nbytes
            out.addfile(m, raw)

print("patched:", dst, os.path.getsize(dst), "bytes", "app.tgz md5:", app_md5)
