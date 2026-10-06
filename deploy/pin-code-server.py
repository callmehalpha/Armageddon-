#!/usr/bin/env python3
"""Resolve the pinned code-server tarballs for a release manifest.

    deploy/pin-code-server.py deploy/code-server.json OUT.json

Downloads each upstream tarball listed in the pin file and records its
sha256 in OUT.json (the manifest's code_server entry, used by Phase 5). When
the pin file already carries a sha256, the download must match it; filling
those in turns the pin from "checked at release time" into "checked
against the repository".
"""
import hashlib
import json
import sys
import urllib.request


def sha256_of(url):
    h = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=600) as r:
        while True:
            chunk = r.read(1 << 20)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


def main(src, dst):
    pin = json.load(open(src))
    for f in pin["files"]:
        got = sha256_of(f["url"])
        want = f.get("sha256", "")
        if want and want.lower() != got:
            sys.exit(f"code-server {f['os']}/{f['arch']}: sha256 {got} does not match the pinned {want}")
        f["sha256"] = got
        print(f"code-server {pin['version']} {f['os']}/{f['arch']}: {got}")
    json.dump(pin, open(dst, "w"), indent=2)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
