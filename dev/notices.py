#!/usr/bin/env python3
"""Regenerates THIRD_PARTY_NOTICES.txt: the licenses of everything built into
Rowsmith (the Go standard library and modules in the binary, and the npm
packages bundled into the web app). Run it after changing dependencies:

    python3 dev/notices.py

Go runs in Docker, as with dev/go.sh; the web app's node_modules must be installed.
"""
import json, os, re, subprocess, sys, tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "THIRD_PARTY_NOTICES.txt")
NAMES = re.compile(r"^(licen[cs]e|copying|notice)", re.I)

GO_SCRIPT = r"""
set -eu
go list -deps -f '{{if not .Standard}}{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/rowsmith \
  | sort -u | grep -v '^rowsmith ' > /out/modules.txt
while read -r path version dir; do
  mkdir -p "/out/m/$path"
  for f in "$dir"/*; do
    case "$(basename "$f" | tr 'A-Z' 'a-z')" in licen[cs]e*|copying*|notice*) [ -f "$f" ] && cp "$f" "/out/m/$path/" ;; esac
  done
done < /out/modules.txt
cp "$(go env GOROOT)/LICENSE" /out/go-LICENSE
go env GOVERSION > /out/go-version
"""


def kind(text, declared=""):
    if declared:
        return declared
    head = text[:4000]
    for needle, name in [("GNU AFFERO", "AGPL-3.0"), ("GNU LESSER", "LGPL"), ("GNU GENERAL PUBLIC", "GPL"),
                         ("Mozilla Public License", "MPL-2.0"), ("Apache License", "Apache-2.0"),
                         ("Permission is hereby granted, free of charge", "MIT"),
                         ("Redistribution and use in source and binary forms", "BSD"),
                         ("Permission to use, copy, modify, and/or distribute", "ISC")]:
        if needle in head:
            return name
    return "see text"


def apache_extra(text):
    """What an Apache-2.0 license file adds to the standard text, if anything."""
    if "END OF TERMS AND CONDITIONS" not in text:
        return text
    before = text.split("TERMS AND CONDITIONS FOR USE", 1)[0]
    after = text.split("END OF TERMS AND CONDITIONS", 1)[1]
    if "APPENDIX: How to apply the Apache License" in after:
        after = ""
    before = "\n".join(l for l in before.splitlines() if l.strip() and not re.search(
        r"Apache License|Version 2\.0, January 2004|https?://www\.apache\.org/licenses/?", l))
    return (before.strip() + "\n\n" + after.strip()).strip()


def read(path):
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read().replace("\r\n", "\n").strip()


def entry(name, version, files, declared=""):
    licenses = [(f, read(p)) for f, p in files if not f.lower().startswith("notice")]
    notices = [(f, read(p)) for f, p in files if f.lower().startswith("notice")]
    lic = kind(licenses[0][1] if licenses else "", declared)
    if not licenses and not declared:
        sys.exit(f"no license file for {name} {version}: check it by hand before shipping")
    parts = []
    for f, text in licenses:
        if "Apache License" in text[:600] and "Version 2.0" in text[:600]:
            extra = apache_extra(text)
            parts.append("Licensed under the Apache License, Version 2.0 (the full text is at the end of this file)."
                         + ("\n\n" + extra if extra else ""))
        else:
            parts.append(text)
    if not licenses:
        parts.append(f"The package declares the {declared} license and includes no license file.")
    for f, text in notices:
        parts.append(f"{f}:\n\n{text}")
    return {"name": name, "version": version, "license": lic, "text": "\n\n".join(parts)}


def go_entries():
    with tempfile.TemporaryDirectory() as tmp:
        subprocess.run(["docker", "run", "--rm", "-v", f"{ROOT}:/src", "-w", "/src", "-v", "rowsmith-gomod:/go/pkg/mod",
                        "-v", f"{tmp}:/out", "-e", "GOFLAGS=-buildvcs=false", "golang:1.27-bookworm", "sh", "-c", GO_SCRIPT], check=True)
        version = read(os.path.join(tmp, "go-version")).removeprefix("go")
        out = [entry("Go standard library", version, [("LICENSE", os.path.join(tmp, "go-LICENSE"))])]
        for line in read(os.path.join(tmp, "modules.txt")).splitlines():
            path, ver, _ = line.split(" ", 2)
            d = os.path.join(tmp, "m", path)
            # nested modules share directories, so take files only
            files = sorted((f, os.path.join(d, f)) for f in os.listdir(d) if os.path.isfile(os.path.join(d, f))) if os.path.isdir(d) else []
            out.append(entry(path, ver, files))
        return out


def npm_entries():
    web = os.path.join(ROOT, "web")
    dirs = subprocess.run(["npm", "ls", "--omit=dev", "--all", "--parseable"], cwd=web, capture_output=True, text=True).stdout.split()
    seen, out = set(), []
    for d in dirs:
        if os.path.abspath(d) == web:
            continue
        with open(os.path.join(d, "package.json")) as f:
            pkg = json.load(f)
        key = (pkg["name"], pkg.get("version", ""))
        if key in seen:
            continue
        seen.add(key)
        declared = pkg.get("license") or ""
        if isinstance(declared, dict):
            declared = declared.get("type", "")
        files = sorted((f, os.path.join(d, f)) for f in os.listdir(d) if NAMES.match(f) and os.path.isfile(os.path.join(d, f)))
        out.append(entry(pkg["name"], key[1], files, declared))
    return sorted(out, key=lambda e: e["name"])


def main():
    go, npm = go_entries(), npm_entries()
    rule = "=" * 80
    lines = ["Third-party notices for Rowsmith", "",
             "Rowsmith is built with the open-source software listed below. Each is used",
             "under its own license, reproduced here. Generated by dev/notices.py.", ""]
    for title, group in [("In the server binary (Go)", go), ("In the web app (npm)", npm)]:
        lines += [f"{title}:", ""]
        width = max(len(e["name"]) for e in group)
        lines += [f"  {e['name']:<{width}}  {e['version']:<14} {e['license']}" for e in group]
        lines.append("")
    for e in go + npm:
        lines += [rule, f"{e['name']} {e['version']}", f"License: {e['license']}", "-" * 80, "", e["text"], ""]
    lines += [rule, "Apache License, Version 2.0", rule, "", read(os.path.join(ROOT, "LICENSE")), ""]
    with open(OUT, "w") as f:
        f.write("\n".join(lines))
    print(f"wrote {os.path.relpath(OUT, ROOT)}: {len(go)} Go and {len(npm)} npm components")


if __name__ == "__main__":
    main()
