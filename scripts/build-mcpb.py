#!/usr/bin/env python3
"""Package the released binaries as MCP Bundles (.mcpb).

One bundle per platform, each self-contained: the three binaries plus a
manifest that launches `zordon mcp` as a stdio server. mcpb's platform
overrides only distinguish OS, not architecture, so a single cross-platform
bundle could not pick between darwin arm64/amd64 or linux amd64/arm64 — hence
one bundle per OS+arch, mirroring the release tarballs.

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


def manifest(version: str, mcpb_platform: str) -> dict:
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
        "server": {
            "type": "binary",
            "entry_point": "server/zordon",
            "mcp_config": {
                "command": "${__dirname}/server/zordon",
                "args": ["mcp"],
                # zordon finds alpha via $PATH and alpha finds tommy as its own
                # sibling; inside the bundle neither is on $PATH, so both are
                # pointed at the bundled copies explicitly. This is what makes
                # the bundle self-contained.
                "env": {
                    "ZORDON_ALPHA": "${__dirname}/server/alpha",
                    "ZORDON_TOMMY_BIN": "${__dirname}/server/tommy",
                },
            },
        },
        "tools": TOOLS,
        "tools_generated": True,
        "compatibility": {"platforms": [mcpb_platform]},
    }


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


def build_one(version: str, tarball: Path, out: Path, goos: str, goarch: str, mcpb_platform: str) -> Path:
    with tempfile.TemporaryDirectory() as tmp:
        server = Path(tmp) / "server"
        server.mkdir()
        with tarfile.open(tarball) as tar:
            for name in BINARIES:
                member = tar.extractfile(name)
                if member is None:
                    raise SystemExit(f"{tarball} is missing {name}")
                dest = server / name
                dest.write_bytes(member.read())
                dest.chmod(0o755)

        (Path(tmp) / "manifest.json").write_text(
            json.dumps(manifest(version, mcpb_platform), indent=2) + "\n", encoding="utf-8"
        )

        bundle = out / f"zordon_{version}_{goos}_{goarch}.mcpb"
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
    for goos, goarch, mcpb_platform in PLATFORMS:
        tarball = find_tarball(args.dist, goos, goarch)
        version = version_of(tarball, goos, goarch)
        versions.add(version)
        bundle = build_one(version, tarball, out, goos, goarch, mcpb_platform)
        print(f"mcpb: wrote {bundle}")

    if len(versions) != 1:
        raise SystemExit(f"tarballs disagree on version: {sorted(versions)}")


if __name__ == "__main__":
    main()
