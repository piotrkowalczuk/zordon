#!/usr/bin/env python3
"""Package the released binaries as MCP Bundles (.mcpb).

Produces two flavours, both self-contained:

  - One bundle per platform (`zordon_<v>_<os>_<arch>.mcpb`) — the three
    binaries plus a manifest that launches `zordon mcp` as a stdio server.
    Smallest download; the consumer picks their platform.

  - One universal bundle (`zordon_<v>_universal.mcpb`) — every platform's
    binaries plus a launcher that detects OS+arch at startup and execs the
    right one. This is what the MCP Registry needs: its package model has a
    single `identifier` with no os/arch field, and mcpb's own platform
    overrides key on OS only (not arch), so neither can select between
    darwin arm64/amd64 or linux amd64/arm64 — the launcher does.

The binaries are taken from the release tarballs goreleaser already produced,
so they are the same signed, notarized files that ship — a bundle never
contains a differently-built binary than the tarball for the same platform.
"""

import argparse
import glob
import json
import os
import stat
import tarfile
import tempfile
import zipfile
from pathlib import Path

BINARIES = ["zordon", "alpha", "tommy"]

# (goreleaser os, goreleaser arch, mcpb platform id)
PLATFORMS = [
    ("darwin", "arm64", "darwin"),
    ("darwin", "amd64", "darwin"),
    ("linux", "amd64", "linux"),
    ("linux", "arm64", "linux"),
]

# Launcher for the universal bundle. mcpb's platform_overrides distinguish OS
# but not CPU arch, so a single bundle cannot pick arm64 vs amd64 on its own —
# this dispatches to the right platform subdir and wires alpha/tommy (which are
# otherwise resolved via $PATH / sibling lookup) to that subdir's copies.
LAUNCHER = """#!/bin/sh
set -e
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)               p=darwin_arm64 ;;
  Darwin/x86_64)              p=darwin_amd64 ;;
  Linux/aarch64|Linux/arm64)  p=linux_arm64 ;;
  Linux/x86_64)               p=linux_amd64 ;;
  *) echo "zordon: unsupported platform $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac
export ZORDON_ALPHA="$here/$p/alpha"
export ZORDON_TOMMY_BIN="$here/$p/tommy"
exec "$here/$p/zordon" "$@"
"""

# The stable command tools. Provisions are declared per-Alphasfile, so the full
# tool set is runtime-determined — tools_generated records that.
TOOLS = [
    {"name": "start", "description": "Bring the stack up."},
    {"name": "stop", "description": "Bring the stack down."},
    {"name": "status", "description": "Report what is running and on which ports."},
    {"name": "get", "description": "Read a single resolved value (port, url, path)."},
    {"name": "plan", "description": "Render the resolved configuration without running it."},
    {"name": "clean", "description": "Tear down provision side effects."},
    {"name": "sudo", "description": "Run a command against a federated parent stack."},
    {"name": "workspace", "description": "Manage isolated per-workspace copies of the stack."},
]


def _manifest(version: str, server: dict, platforms: list[str]) -> dict:
    return {
        "$schema": "https://raw.githubusercontent.com/anthropics/mcpb/main/schemas/mcpb-manifest-latest.schema.json",
        "manifest_version": "0.3",
        "name": "zordon",
        "display_name": "Zordon",
        "version": version,
        "description": "Supervise a local dev stack — databases, brokers and services declared in an Alphasfile — over MCP.",
        "long_description": (
            "Exposes `zordon mcp` as a stdio MCP server so an agent can start, "
            "stop and inspect the stack and run declared provisions. The server "
            "operates on the Alphasfile-managed project in its working directory, "
            "so launch it from a client that runs in your project root; outside "
            "such a project the tools report that no Alphasfile was found."
        ),
        "author": {"name": "piotrkowalczuk", "url": "https://github.com/piotrkowalczuk"},
        "homepage": "https://zordon.io",
        "repository": {"type": "git", "url": "https://github.com/piotrkowalczuk/zordon"},
        "license": "GPL-3.0",
        "keywords": ["mcp", "dev-stack", "supervisor", "alphasfile", "local-development"],
        "server": server,
        "tools": TOOLS,
        "tools_generated": True,
        # A client installing the bundle usually does not launch inside the
        # project, so the tools would otherwise find no Alphasfile in the
        # working directory. The project directory is collected at install time
        # and passed as `zordon mcp --dir`.
        "user_config": {
            "project": {
                "type": "directory",
                "title": "Project directory",
                "description": "A directory containing an Alphasfile (the local dev stack to manage).",
                "required": True,
            }
        },
        "compatibility": {"platforms": platforms},
    }


def manifest(version: str, mcpb_platform: str) -> dict:
    # Per-platform: the binaries sit flat in server/. zordon finds alpha via
    # $PATH and alpha finds tommy as its own sibling; inside the bundle neither
    # is on $PATH, so both are pointed at the bundled copies explicitly.
    return _manifest(
        version,
        {
            "type": "binary",
            "entry_point": "server/zordon",
            "mcp_config": {
                "command": "${__dirname}/server/zordon",
                "args": ["mcp", "--dir", "${user_config.project}"],
                "env": {
                    "ZORDON_ALPHA": "${__dirname}/server/alpha",
                    "ZORDON_TOMMY_BIN": "${__dirname}/server/tommy",
                },
            },
        },
        [mcpb_platform],
    )


def mono_manifest(version: str) -> dict:
    # Universal: the entry point is the launcher, which resolves the platform
    # subdir and exports ZORDON_ALPHA/ZORDON_TOMMY_BIN itself — so mcp_config
    # needs no per-arch env (it could not carry one anyway).
    return _manifest(
        version,
        {
            "type": "binary",
            "entry_point": "server/launch",
            "mcp_config": {"command": "${__dirname}/server/launch", "args": ["mcp", "--dir", "${user_config.project}"], "env": {}},
        },
        ["darwin", "linux"],
    )


def find_tarball(dist: Path, goos: str, goarch: str) -> Path:
    matches = glob.glob(str(dist / f"zordon_*_{goos}_{goarch}.tar.gz"))
    if len(matches) != 1:
        raise SystemExit(f"expected exactly one tarball for {goos}_{goarch}, found {matches}")
    return Path(matches[0])


def version_of(tarball: Path, goos: str, goarch: str) -> str:
    # zordon_<version>_<goos>_<goarch>.tar.gz — the version carries whatever
    # goreleaser stamped (a tag, or a "<x>-next" snapshot), so it always
    # matches the binaries being packaged.
    name = tarball.name
    return name[len("zordon_") : -len(f"_{goos}_{goarch}.tar.gz")]


def extract_binaries(tarball: Path, dest: Path) -> None:
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(tarball) as tar:
        for name in BINARIES:
            member = tar.extractfile(name)
            if member is None:
                raise SystemExit(f"{tarball} is missing {name}")
            out = dest / name
            out.write_bytes(member.read())
            out.chmod(0o755)


def build_one(version: str, tarball: Path, out: Path, goos: str, goarch: str, mcpb_platform: str) -> Path:
    with tempfile.TemporaryDirectory() as tmp:
        extract_binaries(tarball, Path(tmp) / "server")
        (Path(tmp) / "manifest.json").write_text(
            json.dumps(manifest(version, mcpb_platform), indent=2) + "\n", encoding="utf-8"
        )
        bundle = out / f"zordon_{version}_{goos}_{goarch}.mcpb"
        write_zip(Path(tmp), bundle)
        return bundle


def build_mono(version: str, tarballs: dict, out: Path) -> Path:
    with tempfile.TemporaryDirectory() as tmp:
        server = Path(tmp) / "server"
        server.mkdir()
        for (goos, goarch), tarball in tarballs.items():
            extract_binaries(tarball, server / f"{goos}_{goarch}")
        launch = server / "launch"
        launch.write_text(LAUNCHER, encoding="utf-8")
        launch.chmod(0o755)
        (Path(tmp) / "manifest.json").write_text(
            json.dumps(mono_manifest(version), indent=2) + "\n", encoding="utf-8"
        )
        bundle = out / f"zordon_{version}_universal.mcpb"
        write_zip(Path(tmp), bundle)
        return bundle


def write_zip(root: Path, bundle: Path) -> None:
    with zipfile.ZipFile(bundle, "w", zipfile.ZIP_DEFLATED) as zf:
        for path in sorted(root.rglob("*")):
            if not path.is_file():
                continue
            info = zipfile.ZipInfo(str(path.relative_to(root)))
            mode = 0o755 if os.access(path, os.X_OK) else 0o644
            info.external_attr = (stat.S_IFREG | mode) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(info, path.read_bytes())


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dist", default="dist", type=Path, help="directory holding the release tarballs")
    ap.add_argument("--out", type=Path, help="output directory (defaults to --dist)")
    args = ap.parse_args()
    out = args.out or args.dist
    out.mkdir(parents=True, exist_ok=True)

    versions = set()
    tarballs = {}
    for goos, goarch, mcpb_platform in PLATFORMS:
        tarball = find_tarball(args.dist, goos, goarch)
        version = version_of(tarball, goos, goarch)
        versions.add(version)
        tarballs[(goos, goarch)] = tarball
        bundle = build_one(version, tarball, out, goos, goarch, mcpb_platform)
        print(f"mcpb: wrote {bundle}")

    if len(versions) != 1:
        raise SystemExit(f"tarballs disagree on version: {sorted(versions)}")

    bundle = build_mono(versions.pop(), tarballs, out)
    print(f"mcpb: wrote {bundle}")


if __name__ == "__main__":
    main()
