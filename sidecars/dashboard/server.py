#!/usr/bin/env python3
"""Zordon Antigravity sidecar dashboard and lifecycle hook handler.

Mirrors the Claude Code mod (`claude/hooks/register.tsx` and `claude/hooks/diff.tsx`)
for Antigravity:
  1. As a lifecycle hook (`--hook session-start|pre-tool|post-tool`), tracks the
     active workspace directory and MCP tool calls in a shared state file, and
     injects a concise stack summary at session start.
  2. As a web UI sidecar (`sidecars/dashboard/sidecar.json`), serves the live
     Zordon dashboard (Workspace, Runtime, Logs, and per-service Git Diffs) in
     Antigravity's auxiliary side pane on `ANTIGRAVITY_SIDECAR_WEB_PORT`.
"""

from __future__ import annotations

import argparse
import hashlib
import hmac
import html
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse
from urllib.request import Request, urlopen

POLL_MS = 5000
SIZE_EVERY_S = 60.0
PR_EVERY_S = 60.0
VERSION_EVERY_S = 60.0
UPDATE_TTL_S = 6 * 3600.0
UPDATE_FAIL_TTL_S = 15 * 60.0
MAX_CALLS = 30
MAX_OUTPUT = 4000
CODE_LIMIT = 10_000
MAX_FILES = 40
RELEASE_API_URL = "https://api.github.com/repos/piotrkowalczuk/zordon/releases/latest"
RELEASE_TAG_BASE = "https://github.com/piotrkowalczuk/zordon/releases/tag/v"
RELEASE_DOWNLOAD_BASE = "https://github.com/piotrkowalczuk/zordon/releases/download"
BREW_UPGRADE_CMD = "brew upgrade piotrkowalczuk/tap/zordon"
AGY_PLUGIN_UPGRADE_CMD = "agy plugin install <path-to-zordon>"
REQUIRED_BINARIES = ("tommy", "alpha", "zordon")

HEADER_LINE = re.compile(r"^# \[[0-9a-f]+\] (.+?) \(invocation, workspace=([^)]+)\)$")
LEVEL_LINE = re.compile(r"^# \[[0-9a-f]+\] ")
ALPHA_LINE = re.compile(r"^\s+alpha pid=(\d+)")
SERVICE_LINE = re.compile(r"^\s+- \[([^\]]*)\] (.+?) — (.+)$")
DETAIL_LINE = re.compile(r"^ {8}(.*)$")
CHECKOUT_LINE = re.compile(r"^checkout: (.+?) \((?:branch (\S+)[^)]*|detached @ [^)]+)\)$")
ERROR_LINE = re.compile(r"\[ERROR\]\s*-?\s*(.*)$")
MCP_TOOL_RE = re.compile(r"^mcp__?(?:plugin_zordon_)?(?:zordon_)?zordon__?(.+)$")
FILE_HEADER_RE = re.compile(r"^diff --git a/.+? b/(.+)$")
SEMVER_RE = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$")
ZORDON_VER_RE = re.compile(r"^zordon\s+(\S+)")

SemverTuple = tuple[int, int, int, tuple[int | str, ...] | None]

_size_cache: dict[str, tuple[int | None, float]] = {}
_pr_cache: dict[str, tuple[dict[str, Any] | None, float]] = {}
_version_cache: dict[str, tuple[str | None, str | None, float]] = {}
_upgrade_lock = threading.Lock()


def state_file_path() -> Path:
    explicit = os.environ.get("ZORDON_AGY_STATE", "").strip()
    if explicit:
        return Path(explicit)
    plugin_data = os.environ.get("PLUGIN_DATA", "").strip()
    if plugin_data:
        return Path(plugin_data) / "state.json"
    home = os.environ.get("ZORDON_HOME", "").strip() or os.path.expanduser("~/.zordon")
    return Path(home) / "antigravity" / "state.json"


def load_state(path: Path | None = None) -> dict[str, Any]:
    target = path or state_file_path()
    try:
        data = json.loads(target.read_text(encoding="utf-8"))
        if isinstance(data, dict):
            return {
                "cwd": data.get("cwd") if isinstance(data.get("cwd"), str) else "",
                "workspaces": data.get("workspaces") if isinstance(data.get("workspaces"), list) else [],
                "calls": data.get("calls") if isinstance(data.get("calls"), list) else [],
                "updateCache": data.get("updateCache") if isinstance(data.get("updateCache"), dict) else {},
                "dismissedUpdate": data.get("dismissedUpdate") if isinstance(data.get("dismissedUpdate"), str) else "",
            }
    except Exception:
        pass
    return {"cwd": "", "workspaces": [], "calls": [], "updateCache": {}, "dismissedUpdate": ""}


def save_state(state: dict[str, Any], path: Path | None = None) -> None:
    target = path or state_file_path()
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        tmp = target.with_suffix(f".tmp.{os.getpid()}")
        tmp.write_text(json.dumps(state, indent=2) + "\n", encoding="utf-8")
        tmp.replace(target)
    except Exception:
        pass


def record_workspace(state: dict[str, Any], cwd: str) -> None:
    if not cwd:
        return
    state["cwd"] = cwd
    ws = [w for w in state.get("workspaces", []) if isinstance(w, str) and w != cwd]
    state["workspaces"] = [cwd] + ws[:9]


def is_picked_checkout(checkout: str | None, name: str, workspace: str) -> bool:
    if checkout is None or workspace == "main":
        return False
    return checkout == f"src/{name}" or checkout.endswith(f"/workspaces/{workspace}/src/{name}")


def parse_agent_status(stdout: str) -> dict[str, Any] | None:
    try:
        st = json.loads(stdout)
    except Exception:
        return None
    if not isinstance(st, dict) or not isinstance(st.get("state"), str):
        return None

    state_val = st["state"]
    if state_val == "no-alphasfile":
        return {"kind": "inactive"}

    workspace = None
    if st.get("workspace") and st.get("alphasfile") and st.get("state_dir"):
        workspace = {
            "name": st["workspace"],
            "alphasfile": st["alphasfile"],
            "stateDir": st["state_dir"],
            "sizeKB": None,
        }
    if state_val == "error":
        return {
            "kind": "error",
            "message": st.get("error") or "zordon failed",
            "workspace": workspace,
        }

    services = []
    for x in st.get("services") or []:
        if not isinstance(x, dict):
            continue
        raw_state = str(x.get("state", ""))
        health = x.get("health")
        full_state = f"{raw_state}: {health}" if health else raw_state
        services.append(
            {
                "name": str(x.get("name", "")),
                "toolchain": "",
                "state": full_state,
                "isUp": raw_state == "ready",
                "isFailed": raw_state in ("failed", "unhealthy"),
                "print": [x["print"]] if x.get("print") else [],
                "checkout": x.get("checkout_path"),
                "branch": x.get("branch"),
                "sourceDir": x.get("source_dir"),
                "isShared": x.get("shared") is True,
                "isPicked": x.get("picked") is True,
            }
        )
    return {
        "kind": "running" if state_val == "running" else "stopped",
        "services": services,
        "workspace": workspace,
    }


def parse_text_status(exit_code: int, stdout: str, stderr: str, cwd: str = "/") -> dict[str, Any]:
    combined = f"{stdout}\n{stderr}"
    if "no Alphasfile" in combined:
        return {"kind": "inactive"}

    workspace = None
    for line in stdout.splitlines():
        m = HEADER_LINE.match(line)
        if m:
            alphasfile, name = m.group(1), m.group(2)
            parent = alphasfile.rsplit("/", 1)[0] or "/"
            workspace = {
                "name": name,
                "alphasfile": alphasfile,
                "stateDir": f"{parent}/workspaces/{name}",
                "sizeKB": None,
            }
            break

    services: list[dict[str, Any]] = []
    is_alpha_up = False
    is_leaf = False

    for line in stdout.splitlines():
        if LEVEL_LINE.match(line):
            is_leaf = bool(HEADER_LINE.match(line))
            continue
        alpha = ALPHA_LINE.match(line)
        if alpha:
            is_alpha_up = is_alpha_up or (is_leaf and int(alpha.group(1)) > 0)
            continue
        svc = SERVICE_LINE.match(line)
        if svc:
            toolchain, name, raw_state = svc.group(1), svc.group(2), svc.group(3).strip()
            is_failed = raw_state == "failed" or "[unhealthy" in raw_state
            services.append(
                {
                    "toolchain": toolchain,
                    "name": name,
                    "state": raw_state,
                    "isFailed": is_failed,
                    "isUp": raw_state.startswith("running") and "[ready]" in raw_state,
                    "print": [],
                    "checkout": None,
                    "branch": None,
                    "sourceDir": None,
                    "isShared": not is_leaf,
                    "isPicked": False,
                }
            )
            continue
        detail = DETAIL_LINE.match(line)
        if detail and services:
            text = detail.group(1)
            last = services[-1]
            co = CHECKOUT_LINE.match(text)
            if co:
                last["checkout"] = co.group(1)
                last["branch"] = co.group(2)
            elif not text.startswith("checkout:"):
                last["print"].append(text)

    base = cwd.rstrip("/")
    ws_name = workspace["name"] if workspace else "main"
    for svc in services:
        co_path = svc["checkout"]
        if co_path is not None and not co_path.startswith("/"):
            co_path = f"{base}/{co_path}"
        svc["checkout"] = co_path
        svc["isPicked"] = (not svc["isShared"]) and is_picked_checkout(co_path, svc["name"], ws_name)

    if exit_code != 0 and "no alpha running" not in combined:
        errors = []
        for l in stderr.splitlines():
            em = ERROR_LINE.search(l)
            if em and em.group(1).strip():
                errors.append(em.group(1).strip())
        fallback = stderr.strip().splitlines()[-1] if stderr.strip() else f"zordon status exited {exit_code}"
        return {
            "kind": "error",
            "message": "\n".join(errors) if errors else fallback,
            "workspace": workspace,
        }

    return {
        "kind": "running" if is_alpha_up else "stopped",
        "services": services,
        "workspace": workspace,
    }


def state_rank(svc: dict[str, Any]) -> int:
    if svc.get("isFailed"):
        return 0
    if svc.get("isUp"):
        return 2
    if svc.get("state") == "stopped":
        return 3
    return 1


def sort_services(services: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return sorted(services, key=lambda x: (state_rank(x), x.get("name", "")))


def service_mark(svc: dict[str, Any]) -> dict[str, str | None]:
    state = str(svc.get("state", ""))
    if svc.get("isFailed"):
        return {"glyph": "●", "color": "red", "note": "unhealthy" if "unhealthy" in state else "failed"}
    if svc.get("isUp"):
        return {"glyph": "●", "color": "green", "note": None}
    if state == "stopped":
        return {"glyph": "○", "color": "gray", "note": "stopped"}
    return {"glyph": "◐", "color": "yellow", "note": "probing" if "probing" in state else "starting"}


def format_size(kb: int) -> str:
    units = ["KB", "MB", "GB", "TB"]
    value = float(kb)
    unit = 0
    while value >= 1024.0 and unit < len(units) - 1:
        value /= 1024.0
        unit += 1
    formatted = f"{value:.1f}" if (value < 10.0 and unit > 0) else str(round(value))
    return f"{formatted} {units[unit]}"


def summary_line(snapshot: dict[str, Any]) -> str | None:
    kind = snapshot.get("kind")
    if kind in ("loading", "inactive", None):
        return None
    if kind == "error":
        return "zordon ✗ error"
    if kind == "stopped":
        return "zordon stopped"
    if kind == "running":
        svcs = snapshot.get("services") or []
        up = sum(1 for x in svcs if x.get("isUp"))
        failed = sum(1 for x in svcs if x.get("isFailed"))
        suffix = f" · {failed} failed" if failed > 0 else ""
        return f"zordon {up}/{len(svcs)} up{suffix}"
    return None


def group_checkouts(services: list[dict[str, Any]], root: str) -> list[dict[str, Any]]:
    by_path: dict[str, dict[str, Any]] = {}
    root_prefix = root.rstrip("/") + "/"
    for svc in services:
        if svc.get("isShared") or not svc.get("checkout"):
            continue
        path = str(svc["checkout"])
        group = by_path.get(path)
        if group is None:
            label = path[len(root_prefix) :] if path.startswith(root_prefix) else (path.rsplit("/", 1)[-1] or path)
            group = {"path": path, "label": label, "branch": svc.get("branch"), "apps": [], "pr": None}
            by_path[path] = group
        src_dir = svc.get("sourceDir")
        rel_dir = src_dir[len(path) + 1 :] if isinstance(src_dir, str) and src_dir.startswith(path + "/") else None
        group["apps"].append({"name": svc.get("name", ""), "dir": rel_dir})
    return sorted(by_path.values(), key=lambda g: g["label"])


def disk_usage_kb(dir_path: str) -> int | None:
    now = time.monotonic()
    cached = _size_cache.get(dir_path)
    if cached and (now - cached[1]) < SIZE_EVERY_S:
        return cached[0]
    kb = None
    try:
        res = subprocess.run(["du", "-sk", dir_path], capture_output=True, text=True, timeout=10)
        if res.returncode == 0 and res.stdout.strip():
            kb = int(res.stdout.strip().split()[0])
    except Exception:
        kb = None
    _size_cache[dir_path] = (kb, now)
    return kb


def pull_request(dir_path: str, branch: str | None) -> dict[str, Any] | None:
    if not branch:
        return None
    key = f"{dir_path}\0{branch}"
    now = time.monotonic()
    cached = _pr_cache.get(key)
    if cached and (now - cached[1]) < PR_EVERY_S:
        return cached[0]
    pr = None
    try:
        res = subprocess.run(
            ["gh", "pr", "list", "--head", branch, "--state", "all", "--json", "url,number", "--limit", "1"],
            cwd=dir_path,
            capture_output=True,
            text=True,
            timeout=10,
        )
        if res.returncode == 0:
            items = json.loads(res.stdout)
            if isinstance(items, list) and items:
                first = items[0]
                url = str(first.get("url", ""))
                if url.startswith("https://"):
                    pr = {"url": url, "number": first.get("number")}
    except Exception:
        pr = None
    _pr_cache[key] = (pr, now)
    return pr


def query_snapshot(cwd: str, with_extras: bool = True) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    zordon_bin = os.environ.get("ZORDON_BIN", "zordon")
    try:
        res = subprocess.run(
            [zordon_bin, "status", "--format=json"],
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=10,
        )
        parsed = parse_agent_status(res.stdout)
        if parsed is not None:
            snap = parsed
        else:
            ran = subprocess.run([zordon_bin, "status"], cwd=cwd, capture_output=True, text=True, timeout=10)
            snap = parse_text_status(ran.returncode, ran.stdout, ran.stderr, cwd)
    except Exception as exc:
        snap = {"kind": "error", "message": f"cannot run zordon: {exc}", "workspace": None}

    checkouts: list[dict[str, Any]] = []
    ws = snap.get("workspace")
    if isinstance(ws, dict) and snap.get("kind") not in ("loading", "inactive"):
        if with_extras and ws.get("stateDir"):
            ws["sizeKB"] = disk_usage_kb(ws["stateDir"])
        services = snap.get("services") if snap.get("kind") in ("running", "stopped") else []
        alphasfile = str(ws.get("alphasfile", ""))
        root = (alphasfile.rsplit("/", 1)[0] or "/") if ws.get("name") == "main" else str(ws.get("stateDir", cwd))
        checkouts = group_checkouts(services or [], root)
        if with_extras:
            for c in checkouts:
                c["pr"] = pull_request(c["path"], c.get("branch"))

    return snap, checkouts


def fit_hunks(hunks: str) -> str | None:
    if len(hunks) <= CODE_LIMIT:
        return hunks
    cut = hunks.rfind("\n@@", 0, CODE_LIMIT)
    return hunks[:cut] if cut > 0 else None


def parse_git_diff(out: str) -> list[dict[str, Any]]:
    files: list[dict[str, Any]] = []
    current_path: str | None = None
    current_lines: list[str] = []

    def flush() -> None:
        nonlocal current_path, current_lines
        if current_path is None:
            return
        start = next((i for i, l in enumerate(current_lines) if l.startswith("@@")), -1)
        body = [] if start < 0 else current_lines[start:]
        added = sum(1 for l in body if l.startswith("+"))
        removed = sum(1 for l in body if l.startswith("-"))
        is_binary = any(l.startswith("Binary files") for l in current_lines)
        hunks = fit_hunks("\n".join(body).rstrip()) if body else None
        note = (
            "binary file"
            if is_binary
            else "mode or rename only"
            if not body
            else "diff too large to show"
            if hunks is None
            else None
        )
        files.append({"path": current_path, "added": added, "removed": removed, "hunks": hunks, "note": note})

    for line in out.splitlines():
        header = FILE_HEADER_RE.match(line)
        if header:
            flush()
            current_path = header.group(1) or "?"
            current_lines = []
        elif current_path is not None:
            current_lines.append(line)
    flush()
    return files


def discover_diff_apps(cwd: str) -> list[dict[str, Any]]:
    src = Path(cwd) / "src"
    if not src.is_dir():
        return []
    apps: list[dict[str, Any]] = []
    for child in sorted(src.iterdir(), key=lambda p: p.name):
        if not child.is_dir() or not (child / ".git").exists():
            continue
        try:
            res = subprocess.run(
                ["git", "-C", str(child), "status", "--porcelain"],
                capture_output=True,
                text=True,
                timeout=10,
            )
            changes = len([l for l in res.stdout.splitlines() if l.strip()]) if res.returncode == 0 else 0
        except Exception:
            changes = 0
        apps.append({"name": child.name, "changes": changes})
    return apps


def query_diff(cwd: str, asked_app: str = "") -> dict[str, Any]:
    apps = discover_diff_apps(cwd)
    if not apps:
        return {"apps": [], "app": None, "files": [], "more": 0}
    asked = asked_app.strip()
    if not asked:
        picked = next((a["name"] for a in apps if a["changes"] > 0), apps[0]["name"])
    else:
        picked = asked if any(a["name"] == asked for a in apps) else apps[0]["name"]

    app_dir = Path(cwd) / "src" / picked
    tracked: list[dict[str, Any]] = []
    untracked: list[dict[str, Any]] = []
    try:
        diff_res = subprocess.run(
            ["git", "-C", str(app_dir), "diff", "HEAD", "--no-color", "--no-ext-diff"],
            capture_output=True,
            text=True,
            timeout=15,
        )
        if diff_res.returncode == 0:
            tracked = parse_git_diff(diff_res.stdout)
        ls_res = subprocess.run(
            ["git", "-C", str(app_dir), "ls-files", "--others", "--exclude-standard"],
            capture_output=True,
            text=True,
            timeout=15,
        )
        if ls_res.returncode == 0:
            untracked = [
                {"path": p, "added": 0, "removed": 0, "hunks": None, "note": "untracked"}
                for p in ls_res.stdout.splitlines()
                if p.strip()
            ]
    except Exception:
        pass

    all_files = tracked + untracked
    return {
        "apps": apps,
        "app": picked,
        "files": all_files[:MAX_FILES],
        "more": max(0, len(all_files) - MAX_FILES),
    }


def describe_args(input_obj: Any) -> str:
    if not isinstance(input_obj, dict):
        return ""
    args = input_obj.get("args")
    if isinstance(args, list):
        return " ".join(str(x) for x in args)
    filtered = {k: v for k, v in input_obj.items() if k not in ("tool", "tool_use_id", "consent")}
    if not filtered:
        return ""
    return json.dumps(filtered)


def extract_mcp_call(payload: dict[str, Any]) -> tuple[str, str, str] | None:
    """Return (call_id, command, args_str) if payload represents a zordon MCP tool call."""
    tool_call = payload.get("toolCall")
    if isinstance(tool_call, dict):
        name = str(tool_call.get("name", ""))
        args = tool_call.get("args") if isinstance(tool_call.get("args"), dict) else {}
        step_idx = payload.get("stepIdx", "")
        if name == "call_mcp_tool":
            server = str(args.get("ServerName", ""))
            tool_name = str(args.get("ToolName", ""))
            if server in ("zordon", "zordon_zordon", "plugin_zordon_zordon") and tool_name:
                return (f"step-{step_idx}-{tool_name}", tool_name, describe_args(args.get("Arguments") or {}))
        m = MCP_TOOL_RE.match(name)
        if m:
            cmd = m.group(1)
            return (f"step-{step_idx}-{cmd}", cmd, describe_args(args))

    tool_name = str(payload.get("tool_name") or payload.get("tool") or "")
    m = MCP_TOOL_RE.match(tool_name)
    if m:
        cmd = m.group(1)
        tool_input = payload.get("tool_input") if isinstance(payload.get("tool_input"), dict) else payload
        call_id = str(payload.get("tool_use_id") or f"{tool_name}-{int(time.time() * 1000)}")
        return (call_id, cmd, describe_args(tool_input))

    return None


def is_antigravity_payload(payload: dict[str, Any]) -> bool:
    return any(k in payload for k in ("conversationId", "toolCall", "workspacePaths"))


def extract_cwd(payload: dict[str, Any]) -> str:
    if isinstance(payload.get("cwd"), str) and payload["cwd"]:
        return payload["cwd"]
    ws_paths = payload.get("workspacePaths")
    if isinstance(ws_paths, list) and ws_paths and isinstance(ws_paths[0], str):
        return ws_paths[0]
    return ""


def parse_semver(raw: str) -> SemverTuple | None:
    m = SEMVER_RE.match(raw.strip())
    if not m:
        return None
    major, minor, patch = int(m.group(1)), int(m.group(2)), int(m.group(3))
    pre_raw = m.group(4)
    if not pre_raw:
        return (major, minor, patch, None)
    segs: tuple[int | str, ...] = tuple(int(s) if s.isdigit() else s for s in pre_raw.split("."))
    return (major, minor, patch, segs)


def compare_semver(a: SemverTuple, b: SemverTuple) -> int:
    if a[:3] != b[:3]:
        return -1 if a[:3] < b[:3] else 1
    ap, bp = a[3], b[3]
    if ap == bp:
        return 0
    if ap is None:
        return 1
    if bp is None:
        return -1
    for ai, bi in zip(ap, bp):
        if ai == bi:
            continue
        if isinstance(ai, int) and isinstance(bi, int):
            return -1 if ai < bi else 1
        if isinstance(ai, int):
            return -1
        if isinstance(bi, int):
            return 1
        return -1 if str(ai) < str(bi) else 1
    if len(ap) == len(bp):
        return 0
    return -1 if len(ap) < len(bp) else 1


def parse_zordon_version(stdout: str) -> str | None:
    for line in stdout.splitlines():
        stripped = line.strip()
        if not stripped:
            continue
        m = ZORDON_VER_RE.match(stripped)
        if not m:
            return None
        token = m.group(1)
        if token == "(devel)":
            return "(devel)"
        return token.lstrip("v") if parse_semver(token) is not None else None
    return None


def parse_latest_release(raw: str) -> dict[str, str] | None:
    try:
        obj = json.loads(raw)
    except Exception:
        return None
    if not isinstance(obj, dict) or obj.get("draft") is True or obj.get("prerelease") is True:
        return None
    tag_name = obj.get("tag_name")
    if not isinstance(tag_name, str):
        return None
    sv = parse_semver(tag_name)
    if sv is None or sv[3] is not None:
        return None
    version = f"{sv[0]}.{sv[1]}.{sv[2]}"
    html_url = obj.get("html_url")
    if isinstance(html_url, str) and html_url.startswith("https://github.com/piotrkowalczuk/zordon/releases/"):
        return {"version": version, "url": html_url}
    return {"version": version, "url": f"{RELEASE_TAG_BASE}{version}"}


def plugin_root_path() -> Path:
    env_root = os.environ.get("PLUGIN_ROOT", "").strip()
    if env_root:
        return Path(env_root).resolve()
    return Path(__file__).resolve().parents[2]


def read_plugin_version(plugin_root: Path | None = None) -> str:
    root = plugin_root or plugin_root_path()
    for rel in (Path(".antigravity-plugin/plugin.json"), Path("plugin.json"), Path(".claude-plugin/plugin.json")):
        candidate = root / rel
        if not candidate.is_file():
            continue
        try:
            data = json.loads(candidate.read_text(encoding="utf-8"))
            ver = data.get("version")
            if isinstance(ver, str) and ver.strip():
                return ver.strip().lstrip("v")
        except Exception:
            continue
    return "0.0.0"


def is_brew_binary(bin_path: str | None) -> bool:
    if not bin_path:
        return False
    candidates = [bin_path]
    try:
        resolved = os.path.realpath(bin_path)
        if resolved != bin_path:
            candidates.append(resolved)
    except Exception:
        pass
    combined = " ".join(candidates).lower()
    return any(k in combined for k in ("homebrew", "cellar", "caskroom", "linuxbrew"))


def binary_upgrade_plan(bin_path: str | None) -> tuple[str, str, str]:
    target_dir = str(Path(bin_path).parent) if bin_path else ""
    if is_brew_binary(bin_path):
        return ("brew", target_dir, BREW_UPGRADE_CMD)
    shown_dir = target_dir or "~/.local/bin"
    return ("download", target_dir, f"download release → {shown_dir}")


def detect_platform(sys_platform: str | None = None, machine: str | None = None) -> tuple[str, str] | None:
    sp = (sys_platform or sys.platform).lower()
    mc = (machine or platform.machine()).lower()
    goos = "darwin" if sp.startswith("darwin") else ("linux" if sp.startswith("linux") else None)
    goarch = "arm64" if mc in ("arm64", "aarch64") else ("amd64" if mc in ("x86_64", "amd64") else None)
    if goos is None or goarch is None:
        return None
    return (goos, goarch)


def parse_checksum(checksums_text: str, archive_name: str) -> str | None:
    for line in checksums_text.splitlines():
        parts = line.strip().split()
        if len(parts) < 2:
            continue
        digest = parts[0].lower()
        fname = parts[1].lstrip("*")
        if fname == archive_name and len(digest) == 64 and all(c in "0123456789abcdef" for c in digest):
            return digest
    return None


def perform_binary_upgrade(
    latest: str,
    bin_path: str | None = None,
    download_base_url: str | None = None,
    plat: tuple[str, str] | None = None,
) -> dict[str, Any]:
    if not re.match(r"^\d+\.\d+\.\d+$", latest):
        return {"ok": False, "error": f"invalid target version: {latest}"}

    if bin_path is None:
        _, bin_path = query_zordon_version()

    if is_brew_binary(bin_path):
        brew_bin = os.environ.get("ZORDON_BREW_BIN", "").strip() or "brew"
        try:
            res = subprocess.run(
                [brew_bin, "upgrade", "piotrkowalczuk/tap/zordon"],
                capture_output=True,
                text=True,
                timeout=180,
            )
        except Exception as exc:
            return {"ok": False, "error": f"brew failed: {exc}"}
        if res.returncode != 0:
            err = (res.stderr.strip() or res.stdout.strip() or "brew upgrade failed").splitlines()[-1]
            return {"ok": False, "error": err}
        _version_cache.clear()
        new_ver, _ = query_zordon_version()
        return {
            "ok": True,
            "method": "brew",
            "version": new_ver or latest,
            "message": f"Upgraded zordon via Homebrew to v{latest}",
        }

    resolved_plat = plat or detect_platform()
    if resolved_plat is None:
        return {"ok": False, "error": f"unsupported platform: {sys.platform}/{platform.machine()}"}
    goos, goarch = resolved_plat

    target_dir: Path | None = None
    if bin_path:
        parent = Path(bin_path).parent
        if parent.is_dir() and os.access(parent, os.W_OK):
            target_dir = parent
    if target_dir is None:
        target_dir = Path(os.environ.get("ZORDON_INSTALL_DIR", "").strip() or Path.home() / ".local" / "bin")
        try:
            target_dir.mkdir(parents=True, exist_ok=True)
        except Exception as exc:
            return {"ok": False, "error": f"cannot create {target_dir}: {exc}"}

    base = (download_base_url or os.environ.get("ZORDON_RELEASE_DOWNLOAD_BASE", "").strip() or RELEASE_DOWNLOAD_BASE).rstrip("/")
    if not _allowed_release_api_url(base):
        return {"ok": False, "error": "disallowed download base URL"}

    archive_name = f"zordon_{latest}_{goos}_{goarch}.tar.gz"
    checksums_url = f"{base}/v{latest}/checksums.txt"
    archive_url = f"{base}/v{latest}/{archive_name}"

    try:
        req_cs = Request(checksums_url, headers={"User-Agent": "zordon-plugin"})
        with urlopen(req_cs, timeout=15) as resp:
            checksums_txt = resp.read(262_144).decode("utf-8", errors="replace")
    except Exception as exc:
        return {"ok": False, "error": f"failed to fetch checksums.txt: {exc}"}

    expected_sha = parse_checksum(checksums_txt, archive_name)
    if expected_sha is None:
        return {"ok": False, "error": f"checksum not found for {archive_name}"}

    stage_dir = Path(tempfile.mkdtemp(prefix=".zordon-upgrade-", dir=str(target_dir)))
    try:
        archive_path = stage_dir / archive_name
        hasher = hashlib.sha256()
        try:
            req_ar = Request(archive_url, headers={"User-Agent": "zordon-plugin"})
            with urlopen(req_ar, timeout=60) as resp, archive_path.open("wb") as out_f:
                remaining = 200 * 1024 * 1024
                while remaining > 0:
                    chunk = resp.read(min(65_536, remaining))
                    if not chunk:
                        break
                    hasher.update(chunk)
                    out_f.write(chunk)
                    remaining -= len(chunk)
        except Exception as exc:
            return {"ok": False, "error": f"failed to download {archive_name}: {exc}"}

        actual_sha = hasher.hexdigest().lower()
        if not hmac.compare_digest(actual_sha, expected_sha):
            return {"ok": False, "error": f"SHA-256 mismatch for {archive_name}"}

        try:
            with tarfile.open(archive_path, "r:gz") as tf:
                found: set[str] = set()
                for member in tf.getmembers():
                    clean_name = member.name.lstrip("./")
                    if clean_name not in REQUIRED_BINARIES or not member.isfile():
                        continue
                    extracted = tf.extractfile(member)
                    if extracted is None:
                        continue
                    dest_bin = stage_dir / clean_name
                    with extracted, dest_bin.open("wb") as bin_out:
                        shutil.copyfileobj(extracted, bin_out)
                    os.chmod(dest_bin, 0o755)
                    found.add(clean_name)
        except Exception as exc:
            return {"ok": False, "error": f"failed to unpack {archive_name}: {exc}"}

        missing = [b for b in REQUIRED_BINARIES if b not in found]
        if missing:
            return {"ok": False, "error": f"archive missing binaries: {', '.join(missing)}"}

        if goos == "darwin":
            try:
                subprocess.run(
                    ["xattr", "-dr", "com.apple.quarantine", *[str(stage_dir / b) for b in REQUIRED_BINARIES]],
                    capture_output=True,
                    timeout=5,
                )
            except Exception:
                pass

        for b in REQUIRED_BINARIES:
            (stage_dir / b).replace(target_dir / b)
    finally:
        shutil.rmtree(stage_dir, ignore_errors=True)

    _version_cache.clear()
    new_ver, _ = query_zordon_version()
    return {
        "ok": True,
        "method": "download",
        "targetDir": str(target_dir),
        "version": new_ver or latest,
        "message": f"Installed zordon v{latest} into {target_dir}",
    }


def compute_update(
    binary_version: str | None,
    plugin_version: str,
    latest_release: dict[str, str] | None,
    bin_path: str | None = None,
    plugin_upgrade_cmd: str = AGY_PLUGIN_UPGRADE_CMD,
) -> dict[str, Any] | None:
    plugin_sv = parse_semver(plugin_version)
    if plugin_sv is None:
        return None
    bin_sv = parse_semver(binary_version) if (binary_version and binary_version != "(devel)") else None
    rel_sv = parse_semver(latest_release["version"]) if latest_release else None

    is_binary_outdated = bin_sv is not None and rel_sv is not None and compare_semver(bin_sv, rel_sv) < 0

    plugin_target_sv = rel_sv
    if bin_sv is not None and bin_sv[3] is None:
        if plugin_target_sv is None or compare_semver(bin_sv, plugin_target_sv) > 0:
            plugin_target_sv = bin_sv
    is_plugin_outdated = plugin_target_sv is not None and compare_semver(plugin_sv, plugin_target_sv) < 0

    if not is_binary_outdated and not is_plugin_outdated:
        return None

    target_sv = (
        rel_sv
        if (rel_sv is not None and (plugin_target_sv is None or compare_semver(rel_sv, plugin_target_sv) >= 0))
        else plugin_target_sv
    )
    if target_sv is None:
        return None

    latest = f"{target_sv[0]}.{target_sv[1]}.{target_sv[2]}"
    release_url = (
        latest_release["url"]
        if (latest_release and latest_release.get("version") == latest and latest_release.get("url"))
        else f"{RELEASE_TAG_BASE}{latest}"
    )
    method, target_dir, bin_label = binary_upgrade_plan(bin_path)
    return {
        "latest": latest,
        "releaseUrl": release_url,
        "binaryVersion": binary_version,
        "pluginVersion": plugin_version.lstrip("v"),
        "isBinaryOutdated": is_binary_outdated,
        "isPluginOutdated": is_plugin_outdated,
        "binaryMethod": method,
        "binaryTargetDir": target_dir,
        "binaryUpgradeCmd": bin_label,
        "pluginUpgradeCmd": plugin_upgrade_cmd,
    }


def query_zordon_version() -> tuple[str | None, str | None]:
    zordon_bin = os.environ.get("ZORDON_BIN", "zordon")
    now = time.monotonic()
    cached = _version_cache.get(zordon_bin)
    if cached and (now - cached[2]) < VERSION_EVERY_S:
        return cached[0], cached[1]
    ver: str | None = None
    bin_path = shutil.which(zordon_bin) or (zordon_bin if "/" in zordon_bin else None)
    try:
        res = subprocess.run([zordon_bin, "--version"], capture_output=True, text=True, timeout=5)
        if res.returncode == 0:
            ver = parse_zordon_version(res.stdout)
    except Exception:
        ver = None
    _version_cache[zordon_bin] = (ver, bin_path, now)
    return ver, bin_path


def resolve_update_info(
    state: dict[str, Any],
    plugin_root: Path | None = None,
) -> tuple[dict[str, Any] | None, bool]:
    cache = state.get("updateCache") if isinstance(state.get("updateCache"), dict) else {}
    cached_rel: dict[str, str] | None = None
    if isinstance(cache.get("latestVersion"), str) and parse_semver(cache["latestVersion"]) is not None:
        ver_clean = cache["latestVersion"].lstrip("v")
        url_clean = cache.get("releaseUrl")
        if not (isinstance(url_clean, str) and url_clean.startswith("https://github.com/piotrkowalczuk/zordon/releases/")):
            url_clean = f"{RELEASE_TAG_BASE}{ver_clean}"
        cached_rel = {"version": ver_clean, "url": url_clean}

    mutated = False
    check_enabled = os.environ.get("ZORDON_UPDATE_CHECK", "1").strip().lower() not in ("0", "false", "no")
    checked_at = float(cache.get("checkedAt") or 0.0) if isinstance(cache.get("checkedAt"), (int, float)) else 0.0
    ttl = UPDATE_TTL_S if cached_rel is not None else UPDATE_FAIL_TTL_S
    now = time.time()

    if check_enabled and (now - checked_at) >= ttl:
        api_url = os.environ.get("ZORDON_RELEASE_API_URL", "").strip() or RELEASE_API_URL
        if _allowed_release_api_url(api_url):
            try:
                req = Request(
                    api_url,
                    headers={"Accept": "application/vnd.github+json", "User-Agent": "zordon-plugin"},
                )
                with urlopen(req, timeout=2.5) as resp:
                    raw = resp.read(65536).decode("utf-8", errors="replace")
                parsed = parse_latest_release(raw)
                if parsed is not None:
                    cached_rel = parsed
            except Exception:
                pass
        new_cache: dict[str, Any] = {"checkedAt": now}
        if cached_rel is not None:
            new_cache["latestVersion"] = cached_rel["version"]
            new_cache["releaseUrl"] = cached_rel["url"]
        state["updateCache"] = new_cache
        mutated = True

    bin_ver, bin_path = query_zordon_version()
    plugin_ver = read_plugin_version(plugin_root)
    upd = compute_update(bin_ver, plugin_ver, cached_rel, bin_path)
    return upd, mutated


def format_update_markdown(upd: dict[str, Any]) -> str:
    latest = upd["latest"]
    url = upd["releaseUrl"]
    bin_v = upd.get("binaryVersion")
    plug_v = upd.get("pluginVersion")
    plug_cmd = upd["pluginUpgradeCmd"]
    if upd.get("isBinaryOutdated") and upd.get("isPluginOutdated"):
        return (
            f"⬆️ **Zordon [v{latest}]({url}) is available** "
            f"(CLI `v{bin_v}`, plugin `{plug_v}`) — "
            f"click **Update** in [Zordon](sidecar://zordon/dashboard/) to upgrade the CLI, "
            f"and update the plugin with `{plug_cmd}` (or `/plugin`)."
        )
    if upd.get("isBinaryOutdated"):
        return (
            f"⬆️ **Zordon CLI [v{latest}]({url}) is available** "
            f"(installed `v{bin_v}`) — click **Update** in [Zordon](sidecar://zordon/dashboard/) to upgrade automatically."
        )
    return (
        f"⬆️ **Zordon plugin [{latest}]({url}) is available** "
        f"(installed `{plug_v}`) — update with `{plug_cmd}` (or `/plugin`)."
    )


def format_update_text(upd: dict[str, Any]) -> str:
    latest = upd["latest"]
    bin_v = upd.get("binaryVersion")
    plug_v = upd.get("pluginVersion")
    bin_cmd = upd["binaryUpgradeCmd"]
    plug_cmd = upd["pluginUpgradeCmd"]
    if upd.get("isBinaryOutdated") and upd.get("isPluginOutdated"):
        return f"↑ zordon v{latest} available (cli v{bin_v}, plugin {plug_v}) — {bin_cmd} · {plug_cmd}"
    if upd.get("isBinaryOutdated"):
        return f"↑ zordon v{latest} available (cli v{bin_v}) — {bin_cmd}"
    return f"↑ zordon plugin {latest} available (installed {plug_v}) — {plug_cmd}"


def format_session_message(snap: dict[str, Any]) -> str:
    kind = snap.get("kind")
    if kind in ("inactive", "loading", None):
        return ""
    ws = snap.get("workspace") or {}
    ws_name = ws.get("name") or "main"
    af = ws.get("alphasfile") or "Alphasfile"
    head = summary_line(snap) or f"zordon {kind}"
    lines = [f"{head} (workspace={ws_name}, alphasfile={af})"]
    if kind == "error":
        lines.append(f"  error: {snap.get('message', 'unknown error')}")
    else:
        for svc in sort_services(snap.get("services") or []):
            mark = service_mark(svc)
            tags = [t for t in (mark["note"], "shared" if svc.get("isShared") else None) if t]
            tag_str = f" [{', '.join(tags)}]" if tags else ""
            prints = svc.get("print") or []
            prt_str = f" — {prints[0]}" if prints else ""
            lines.append(f"  {mark['glyph']} {svc.get('name')}{tag_str}{prt_str}")
    lines.append(
        "Use the zordon MCP tools (start, status, stop, get, plan, workspace, provision__*) "
        "instead of running zordon/alpha in the shell. "
        "Open [Zordon](sidecar://zordon/dashboard/) to view the live stack and diff pane."
    )
    return "\n".join(lines)


def handle_hook(event: str, raw_stdin: str, state_path: Path | None = None) -> tuple[str, int]:
    try:
        payload = json.loads(raw_stdin) if raw_stdin.strip() else {}
        if not isinstance(payload, dict):
            payload = {}
    except Exception:
        payload = {}

    agy = is_antigravity_payload(payload)
    cwd = extract_cwd(payload)
    state = load_state(state_path)
    if cwd:
        record_workspace(state, cwd)

    if event == "session-start":
        target_cwd = cwd or state.get("cwd") or os.getcwd()
        snap, _ = query_snapshot(target_cwd, with_extras=False)
        msg = format_session_message(snap)
        upd: dict[str, Any] | None = None
        if snap.get("kind") not in ("inactive", "loading", None):
            upd, _ = resolve_update_info(state)
        save_state(state, state_path)

        dismissed = state.get("dismissedUpdate") or ""
        shown_upd = upd if (upd and upd.get("latest") != dismissed) else None

        if agy:
            steps: list[dict[str, Any]] = []
            if msg:
                steps.append({"systemMessage": {"systemMessage": msg}})
            if shown_upd:
                steps.append(
                    {
                        "systemMessage": {
                            "systemMessage": "",
                            "metadata": {"user_markdown": format_update_markdown(shown_upd)},
                        }
                    }
                )
            if not steps:
                return ("{}\n", 0)
            return (json.dumps({"injectSteps": steps}) + "\n", 0)

        parts = [p for p in (msg, format_update_text(shown_upd) if shown_upd else "") if p]
        return ("\n\n".join(parts) + "\n" if parts else "", 0)

    if event == "pre-tool":
        call_info = extract_mcp_call(payload)
        if call_info is not None:
            call_id, command, args_str = call_info
            now_ms = int(time.time() * 1000)
            entry = {
                "id": call_id,
                "command": command,
                "args": args_str,
                "startedAt": now_ms,
                "durationMs": None,
                "isError": False,
                "output": "",
            }
            calls = [c for c in state.get("calls", []) if isinstance(c, dict)]
            state["calls"] = [entry] + calls[: MAX_CALLS - 1]
        save_state(state, state_path)
        return ('{"decision":"allow"}\n' if agy else "", 0)

    if event == "post-tool":
        call_info = extract_mcp_call(payload)
        if call_info is not None:
            call_id, command, _ = call_info
            now_ms = int(time.time() * 1000)
            err_str = str(payload.get("error") or "")
            out_str = err_str or str(payload.get("output") or payload.get("result") or "")
            for c in state.get("calls", []):
                if not isinstance(c, dict):
                    continue
                if c.get("id") == call_id or (c.get("command") == command and c.get("durationMs") is None):
                    started = int(c.get("startedAt") or now_ms)
                    c["durationMs"] = max(0, now_ms - started)
                    c["isError"] = bool(err_str)
                    if out_str:
                        c["output"] = out_str[:MAX_OUTPUT]
                    break
        save_state(state, state_path)
        return ("{}\n" if agy else "", 0)

    return ("{}\n" if agy else "", 0)


DASHBOARD_HTML = """<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Zordon</title>
  <style>
    :root {
      --bg: var(--background, #181a20);
      --panel-bg: var(--card, #1e222b);
      --bar-bg: var(--sidebar, #2b3040);
      --sub-bg: var(--secondary, #232733);
      --fg: var(--foreground, #e5e9f0);
      --muted: var(--muted-foreground, #8b94a5);
      --bdr: var(--border, #343b4d);
      --green: #3fb950;
      --red: #f85149;
      --yellow: #d29922;
      --gray: #6e7681;
      --accent-fg: var(--primary, #58a6ff);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 10px;
      background: var(--bg);
      color: var(--fg);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 12px;
      line-height: 1.45;
    }
    a { color: var(--accent-fg); text-decoration: none; }
    a:hover { text-decoration: underline; }
    .bar {
      display: flex;
      justify-content: space-between;
      align-items: center;
      background: var(--bar-bg);
      padding: 6px 10px;
      border-radius: 6px;
      font-weight: 600;
    }
    .bar-right { display: flex; align-items: center; gap: 8px; font-weight: 400; }
    .tab-btn, .fold-btn, .row-btn, .app-btn {
      background: transparent;
      color: inherit;
      border: none;
      font: inherit;
      cursor: pointer;
      padding: 0 4px;
    }
    .tab-btn {
      padding: 2px 7px;
      border-radius: 4px;
      color: var(--muted);
    }
    .tab-btn.active {
      background: var(--sub-bg);
      color: var(--fg);
      font-weight: 600;
    }
    .app-btn {
      padding: 3px 8px;
      border-radius: 4px;
      border: 1px solid var(--bdr);
      margin-right: 4px;
      margin-bottom: 4px;
    }
    .app-btn.active {
      background: var(--accent-fg);
      color: #fff;
      border-color: var(--accent-fg);
    }
    .upd-btn {
      background: var(--yellow);
      color: #111;
      border: none;
      border-radius: 4px;
      font: inherit;
      font-weight: 600;
      cursor: pointer;
      padding: 2px 8px;
    }
    .upd-btn:disabled {
      opacity: 0.6;
      cursor: default;
    }
    .panel {
      margin-top: 10px;
      border: 1px solid var(--bdr);
      border-radius: 8px;
      overflow: hidden;
      background: var(--panel-bg);
    }
    .panel.border-green { border-color: var(--green); }
    .panel.border-red { border-color: var(--red); }
    .panel.border-yellow { border-color: var(--yellow); }
    .panel.border-gray { border-color: var(--gray); }
    .panel-head, .panel-foot {
      display: flex;
      justify-content: space-between;
      align-items: center;
      background: var(--sub-bg);
      padding: 5px 10px;
    }
    .panel-title { font-weight: 600; }
    .panel-body { padding: 8px 10px; }
    .muted { color: var(--muted); }
    .dot-green { color: var(--green); }
    .dot-red { color: var(--red); }
    .dot-yellow { color: var(--yellow); }
    .dot-gray { color: var(--gray); }
    .svc-row, .call-row, .co-row { margin-bottom: 6px; }
    .svc-row:last-child, .call-row:last-child, .co-row:last-child { margin-bottom: 0; }
    .svc-print, .svc-details, .call-details {
      padding-left: 16px;
      white-space: pre-wrap;
      word-break: break-word;
    }
    pre.code {
      margin: 4px 0 0 0;
      padding: 6px 8px;
      background: var(--bg);
      border: 1px solid var(--bdr);
      border-radius: 4px;
      overflow-x: auto;
      font-size: 12px;
    }
    .diff-add { color: var(--green); }
    .diff-del { color: var(--red); }
    select.ws-select {
      width: 100%;
      margin-top: 8px;
      padding: 4px 6px;
      background: var(--panel-bg);
      color: var(--fg);
      border: 1px solid var(--bdr);
      border-radius: 4px;
      font: inherit;
    }
  </style>
</head>
<body>
  <div id="root"></div>
  <script>
    const state = {
      tab: 'stack',
      collapsed: false,
      foldWorkspace: false,
      foldRuntime: false,
      foldLog: false,
      openService: null,
      openCall: null,
      dir: new URLSearchParams(location.search).get('dir') || '',
      data: null,
      diff: null,
      diffApp: '',
      updateStatus: null
    };

    function esc(s) {
      return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    function markOf(svc) {
      if (svc.isFailed) return { glyph: '●', cls: 'dot-red', note: (svc.state || '').includes('unhealthy') ? 'unhealthy' : 'failed' };
      if (svc.isUp) return { glyph: '●', cls: 'dot-green', note: null };
      if (svc.state === 'stopped') return { glyph: '○', cls: 'dot-gray', note: 'stopped' };
      return { glyph: '◐', cls: 'dot-yellow', note: (svc.state || '').includes('probing') ? 'probing' : 'starting' };
    }

    function fmtDuration(ms) {
      if (ms == null) return '…';
      return ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(1)}s`;
    }

    function fmtSize(kb) {
      if (kb == null) return '…';
      const units = ['KB', 'MB', 'GB', 'TB'];
      let v = kb, u = 0;
      while (v >= 1024 && u < units.length - 1) { v /= 1024; u++; }
      return `${v < 10 && u > 0 ? v.toFixed(1) : Math.round(v)} ${units[u]}`;
    }

    function borderOf(svcs) {
      if (svcs.some(x => x.isFailed)) return 'border-red';
      if (svcs.length > 0 && svcs.every(x => x.state === 'stopped')) return 'border-gray';
      return 'border-green';
    }

    async function fetchSnapshot() {
      const q = state.dir ? `?dir=${encodeURIComponent(state.dir)}` : '';
      const res = await fetch(`/api/snapshot${q}`);
      state.data = await res.json();
      if (state.tab === 'diff') {
        await fetchDiff(state.diffApp);
      }
      render();
    }

    async function fetchDiff(app) {
      state.diffApp = app || '';
      const params = new URLSearchParams();
      if (state.dir) params.set('dir', state.dir);
      if (state.diffApp) params.set('app', state.diffApp);
      const res = await fetch(`/api/diff?${params.toString()}`);
      state.diff = await res.json();
      state.diffApp = state.diff.app || '';
      render();
    }

    function renderPanel({ key, title, aside, border, isFolded, onToggle, body, footer }) {
      return `
        <div class="panel ${border}">
          <div class="panel-head">
            <span class="panel-title">${esc(title)}</span>
            <span class="bar-right">
              ${aside || ''}
              <button class="fold-btn" onclick="${onToggle}">${isFolded ? '+' : '−'}</button>
            </span>
          </div>
          ${isFolded ? '' : `<div class="panel-body">${body}</div>`}
          ${footer ? `<div class="panel-foot"><span>${footer.left}</span><span>${footer.right}</span></div>` : ''}
        </div>
      `;
    }

    function renderStack() {
      const d = state.data;
      if (!d || !d.snapshot || d.snapshot.kind === 'loading') {
        return `<div class="panel border-gray"><div class="panel-body muted">Reading the stack…</div></div>`;
      }
      const s = d.snapshot;
      if (s.kind === 'inactive') {
        return `
          <div class="panel border-gray">
            <div class="panel-body">
              <div><span class="dot-gray">○ </span><strong>Not a workspace</strong></div>
              <div class="muted">No Alphasfile anywhere in this tree (${esc(d.cwd)})</div>
              <div class="muted">Learn more at <a href="https://zordon.io/" target="_blank">zordon.io</a></div>
            </div>
          </div>
        `;
      }

      const ws = s.workspace;
      const checkouts = d.checkouts || [];
      let wsBody = '';
      if (s.kind === 'error') {
        wsBody = `<div class="dot-red"><strong>❌ zordon error</strong></div><pre class="code">${esc(s.message)}</pre>`;
      } else if (checkouts.length > 0) {
        wsBody = checkouts.map(c => `
          <div class="co-row">
            <div>
              <strong>${esc(c.label)}</strong>
              ${c.pr ? `<a href="${esc(c.pr.url)}" target="_blank">${esc(c.branch || '')}</a> <span class="muted">#${esc(c.pr.number)}</span>` : `<span class="muted">${esc(c.branch || 'detached')}</span>`}
            </div>
            ${(c.apps || []).map(a => `<div>&nbsp;&nbsp;${esc(a.name)}${a.dir ? ` <span class="muted">${esc(a.dir)}</span>` : ''}</div>`).join('')}
          </div>
        `).join('');
      } else {
        wsBody = `<div class="muted">no service builds from a git checkout</div>`;
      }

      const wsBorder = s.kind === 'error' ? 'border-red' : (checkouts.length > 0 ? 'border-green' : 'border-gray');
      const wsPanel = renderPanel({
        key: 'workspace',
        title: 'Workspace',
        aside: ws ? `<span class="muted">${esc(ws.name)}</span>` : '',
        border: wsBorder,
        isFolded: state.foldWorkspace,
        onToggle: 'toggleFold("foldWorkspace")',
        body: wsBody,
        footer: ws ? {
          left: `<span class="muted">${esc(fmtSize(ws.sizeKB))}</span>`,
          right: `<span class="muted" title="${esc(ws.alphasfile)}">Alphasfile</span>`
        } : null
      });

      const svcs = d.services || [];
      let runtimePanel = '';
      if (svcs.length > 0) {
        const rows = svcs.map(svc => {
          const m = markOf(svc);
          const tags = [m.note, svc.isShared ? 'shared' : null].filter(Boolean).join(' · ');
          const isOpen = state.openService === svc.name;
          const details = [
            ['state', svc.state],
            ['print', (svc.print || []).join('\\n') || null],
            ['checkout', svc.checkout],
            ['branch', svc.branch],
            ['source', svc.sourceDir]
          ].filter(x => x[1]);
          return `
            <div class="svc-row">
              <div>
                <span class="${m.cls}">${m.glyph} </span>
                <button class="row-btn" onclick="toggleService('${esc(svc.name)}')">${isOpen ? '▾' : '▸'} ${esc(svc.name)}</button>
                ${tags ? `<span class="muted"> ${esc(tags)}</span>` : ''}
              </div>
              ${isOpen ? `<div class="svc-details">${details.map(([k, v]) => `<div><span class="muted">${esc(k)} </span>${esc(v)}</div>`).join('')}</div>`
                       : (svc.print || []).map(l => `<div class="svc-print muted">${esc(l)}</div>`).join('')}
            </div>
          `;
        }).join('');
        runtimePanel = renderPanel({
          key: 'runtime',
          title: 'Runtime',
          border: borderOf(svcs),
          isFolded: state.foldRuntime,
          onToggle: 'toggleFold("foldRuntime")',
          body: rows
        });
      }

      const calls = d.calls || [];
      let logPanel = '';
      if (calls.length > 0 && ws) {
        const callRows = calls.map(c => {
          const isOpen = state.openCall === c.id;
          const glyph = c.durationMs == null ? '◐' : (c.isError ? '✕' : '✓');
          const cls = c.durationMs == null ? 'dot-yellow' : (c.isError ? 'dot-red' : 'dot-green');
          const label = `${c.command}${c.args ? ' ' + c.args : ''}`;
          return `
            <div class="call-row">
              <div>
                <span class="${cls}">${glyph} </span>
                <button class="row-btn" onclick="toggleCall('${esc(c.id)}')">${esc(label)}</button>
                <span class="muted"> ${esc(fmtDuration(c.durationMs))}</span>
              </div>
              ${isOpen ? `
                <div class="call-details">
                  <div class="muted">${esc(new Date(c.startedAt).toLocaleTimeString())} · zordon ${esc(label)}</div>
                  ${c.output ? `<pre class="code">${esc(c.output)}</pre>` : `<div class="muted">no output yet</div>`}
                </div>
              ` : ''}
            </div>
          `;
        }).join('');
        logPanel = renderPanel({
          key: 'log',
          title: `Logs · ${calls.length}`,
          border: 'border-gray',
          isFolded: state.foldLog,
          onToggle: 'toggleFold("foldLog")',
          body: callRows
        });
      }

      return wsPanel + runtimePanel + logPanel;
    }

    function renderDiff() {
      const df = state.diff;
      if (!df) return `<div class="panel border-gray"><div class="panel-body muted">Loading diff…</div></div>`;
      const apps = df.apps || [];
      if (apps.length === 0) {
        return `<div class="panel border-gray"><div class="panel-body muted">No git worktrees under src/.</div></div>`;
      }
      const picker = apps.map(a => `
        <button class="app-btn ${a.name === df.app ? 'active' : ''}" onclick="fetchDiff('${esc(a.name)}')">
          ${esc(a.name)} · ${a.changes}
        </button>
      `).join('');
      const files = df.files || [];
      const body = files.length === 0
        ? `<div class="muted">No changes in src/${esc(df.app)}.</div>`
        : files.map(f => {
            const colored = f.hunks ? f.hunks.split('\\n').map(line => {
              if (line.startsWith('+')) return `<span class="diff-add">${esc(line)}</span>`;
              if (line.startsWith('-')) return `<span class="diff-del">${esc(line)}</span>`;
              return esc(line);
            }).join('\\n') : '';
            return `
              <div class="svc-row">
                <div><strong>${esc(f.path)}</strong> <span class="diff-add">+${f.added}</span> <span class="diff-del">−${f.removed}</span></div>
                ${f.hunks ? `<pre class="code">${colored}</pre>` : ''}
                ${f.note ? `<div class="muted">${esc(f.note)}</div>` : ''}
              </div>
            `;
          }).join('');
      return `
        <div class="panel border-gray">
          <div class="panel-head"><span class="panel-title">diff · src/${esc(df.app)}</span></div>
          <div class="panel-body">
            ${apps.length > 1 ? `<div style="margin-bottom:8px">${picker}</div>` : ''}
            ${body}
            ${df.more > 0 ? `<div class="muted">… ${df.more} more file(s)</div>` : ''}
          </div>
        </div>
      `;
    }

    function renderUpdate(d) {
      const u = d && d.update;
      const st = state.updateStatus;
      if ((!u || d.dismissedUpdate === u.latest) && (!st || st.kind !== 'done')) return '';
      if ((!u || d.dismissedUpdate === u.latest) && st && st.kind === 'done') {
        return `
          <div class="panel border-green">
            <div class="panel-head">
              <span class="panel-title dot-green">✓ ${esc(st.message)}</span>
              <button class="fold-btn" title="Close" onclick="clearUpdateStatus()">×</button>
            </div>
          </div>
        `;
      }
      const rows = [];
      if (u.isBinaryOutdated) {
        rows.push(`<div><span class="muted">cli v${esc(u.binaryVersion)} → v${esc(u.latest)}: </span><code>${esc(u.binaryUpgradeCmd)}</code></div>`);
      }
      if (u.isPluginOutdated) {
        rows.push(`<div><span class="muted">plugin ${esc(u.pluginVersion)} → ${esc(u.latest)}: </span><code>${esc(u.pluginUpgradeCmd)}</code></div>`);
      }
      if (st && st.message) {
        const cls = st.kind === 'error' ? 'dot-red' : (st.kind === 'done' ? 'dot-green' : 'dot-yellow');
        rows.push(`<div class="${cls}">${esc(st.message)}</div>`);
      }
      const isRunning = st && st.kind === 'running';
      const updBtn = u.isBinaryOutdated
        ? `<button class="upd-btn" ${isRunning ? 'disabled' : ''} onclick="triggerUpdate()">${isRunning ? 'Updating…' : 'Update'}</button>`
        : '';
      return `
        <div class="panel border-yellow">
          <div class="panel-head">
            <span class="panel-title dot-yellow">Update · v${esc(u.latest)}</span>
            <span class="bar-right">
              ${updBtn}
              <a href="${esc(u.releaseUrl)}" target="_blank">release</a>
              <button class="fold-btn" title="Dismiss" onclick="dismissUpdate('${esc(u.latest)}')">×</button>
            </span>
          </div>
          <div class="panel-body">${rows.join('')}</div>
        </div>
      `;
    }

    function render() {
      const d = state.data;
      const sum = d && d.summary ? d.summary.replace(/^zordon\\s+/, '') : '';
      const workspaces = (d && d.workspaces) || [];
      const wsSelect = workspaces.length > 1 ? `
        <select class="ws-select" onchange="selectDir(this.value)">
          ${workspaces.map(w => `<option value="${esc(w)}" ${w === d.cwd ? 'selected' : ''}>${esc(w)}</option>`).join('')}
        </select>
      ` : '';
      document.getElementById('root').innerHTML = `
        <div class="bar">
          <span>Zordon${sum ? ` <span class="muted">· ${esc(sum)}</span>` : ''}</span>
          <span class="bar-right">
            <button class="tab-btn ${state.tab === 'stack' ? 'active' : ''}" onclick="switchTab('stack')">Stack</button>
            <button class="tab-btn ${state.tab === 'diff' ? 'active' : ''}" onclick="switchTab('diff')">Diff</button>
            <a href="https://zordon.io/" target="_blank">zordon.io</a>
            <button class="fold-btn" onclick="toggleFold('collapsed')">${state.collapsed ? '+' : '−'}</button>
          </span>
        </div>
        ${state.collapsed ? '' : renderUpdate(d) + wsSelect + (state.tab === 'stack' ? renderStack() : renderDiff())}
      `;
    }

    window.toggleFold = k => { state[k] = !state[k]; render(); };
    window.toggleService = n => { state.openService = state.openService === n ? null : n; render(); };
    window.toggleCall = id => { state.openCall = state.openCall === id ? null : id; render(); };
    window.dismissUpdate = async ver => {
      state.updateStatus = null;
      if (state.data) state.data.dismissedUpdate = ver;
      render();
      try {
        await fetch('/api/dismiss-update', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ version: ver })
        });
      } catch {}
    };
    window.clearUpdateStatus = () => { state.updateStatus = null; render(); };
    window.triggerUpdate = async () => {
      if (state.updateStatus && state.updateStatus.kind === 'running') return;
      state.updateStatus = { kind: 'running', message: 'Upgrading zordon CLI…' };
      render();
      try {
        const res = await fetch('/api/update', { method: 'POST' });
        const body = await res.json();
        if (body && body.ok) {
          state.updateStatus = { kind: 'done', message: body.message || 'Updated zordon CLI' };
          await fetchSnapshot();
        } else {
          state.updateStatus = { kind: 'error', message: (body && body.error) || 'Update failed' };
          render();
        }
      } catch (err) {
        state.updateStatus = { kind: 'error', message: String(err) };
        render();
      }
    };
    window.switchTab = t => {
      state.tab = t;
      if (t === 'diff' && !state.diff) fetchDiff('');
      else render();
    };
    window.selectDir = dir => { state.dir = dir; fetchSnapshot(); };

    fetchSnapshot();
    setInterval(fetchSnapshot, ${POLL_MS});
  </script>
</body>
</html>
"""


def resolve_target_cwd(query_dir: str = "", state_path: Path | None = None) -> tuple[str, dict[str, Any]]:
    state = load_state(state_path)
    if query_dir and Path(query_dir).is_dir():
        return query_dir, state
    if state.get("cwd") and Path(state["cwd"]).is_dir():
        return state["cwd"], state
    env_dir = os.environ.get("ZORDON_DIR", "").strip()
    if env_dir and Path(env_dir).is_dir():
        return env_dir, state
    return os.getcwd(), state


def build_snapshot_payload(query_dir: str = "", state_path: Path | None = None) -> dict[str, Any]:
    cwd, state = resolve_target_cwd(query_dir, state_path)
    snap, checkouts = query_snapshot(cwd, with_extras=True)
    upd, mutated = resolve_update_info(state)
    if mutated:
        save_state(state, state_path)
    services = sort_services(snap.get("services") or []) if snap.get("kind") in ("running", "stopped") else []
    return {
        "cwd": cwd,
        "workspaces": state.get("workspaces") or ([cwd] if cwd else []),
        "snapshot": snap,
        "services": services,
        "checkouts": checkouts,
        "calls": state.get("calls") or [],
        "summary": summary_line(snap),
        "update": upd,
        "dismissedUpdate": state.get("dismissedUpdate") or "",
    }


class DashboardHandler(BaseHTTPRequestHandler):
    def log_message(self, format: str, *args: Any) -> None:
        return

    def _send_json(self, status: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        req_dir = (qs.get("dir") or [""])[0]

        if parsed.path == "/healthz":
            self._send_json(200, {"ok": True})
            return
        if parsed.path == "/api/snapshot":
            self._send_json(200, build_snapshot_payload(req_dir))
            return
        if parsed.path == "/api/diff":
            cwd, _ = resolve_target_cwd(req_dir)
            app = (qs.get("app") or [""])[0]
            self._send_json(200, query_diff(cwd, app))
            return
        if parsed.path in ("/", "/index.html"):
            page = DASHBOARD_HTML.replace("${POLL_MS}", str(POLL_MS)).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(page)))
            self.end_headers()
            self.wfile.write(page)
            return

        self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        parsed = urlparse(self.path)
        if parsed.path == "/api/dismiss-update":
            length = min(int(self.headers.get("Content-Length") or "0"), 4096)
            raw = self.rfile.read(length).decode("utf-8", errors="replace") if length > 0 else "{}"
            try:
                body = json.loads(raw)
            except Exception:
                body = {}
            ver = body.get("version") if isinstance(body, dict) else ""
            state = load_state()
            state["dismissedUpdate"] = str(ver or "").strip()
            save_state(state)
            self._send_json(200, {"ok": True, "dismissedUpdate": state["dismissedUpdate"]})
            return

        if parsed.path == "/api/update":
            with _upgrade_lock:
                state = load_state()
                upd, mutated = resolve_update_info(state)
                if mutated:
                    save_state(state)
                if not upd or not upd.get("isBinaryOutdated"):
                    self._send_json(200, {"ok": False, "error": "zordon CLI is already up to date"})
                    return
                _, bin_path = query_zordon_version()
                result = perform_binary_upgrade(upd["latest"], bin_path=bin_path)
                self._send_json(200, result)
            return

        self._send_json(404, {"error": "not found"})


def serve_dashboard(host: str, port: int) -> None:
    srv = ThreadingHTTPServer((host, port), DashboardHandler)
    bound_host, bound_port = srv.server_address[:2]
    print(f"zordon sidecar dashboard listening on http://{bound_host}:{bound_port}/", file=sys.stderr, flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


def main() -> int:
    parser = argparse.ArgumentParser(description="Zordon Antigravity sidecar & hook handler")
    parser.add_argument("--hook", choices=["session-start", "pre-tool", "post-tool"], help="run as a lifecycle hook")
    parser.add_argument("--host", default="127.0.0.1", help="bind host for sidecar web UI")
    parser.add_argument("--port", type=int, default=None, help="bind port (defaults to ANTIGRAVITY_SIDECAR_WEB_PORT or PORT)")
    args = parser.parse_args()

    if args.hook:
        raw = sys.stdin.read() if not sys.stdin.isatty() else ""
        out, code = handle_hook(args.hook, raw)
        if out:
            sys.stdout.write(out)
            sys.stdout.flush()
        return code

    port = args.port
    if port is None:
        raw_port = os.environ.get("ANTIGRAVITY_SIDECAR_WEB_PORT") or os.environ.get("PORT") or "0"
        port = int(raw_port)
    serve_dashboard(args.host, port)
    return 0


def _allowed_release_api_url(url: str) -> bool:
    return url.startswith("https://") or url.startswith("http://127.0.0.1:") or url.startswith("http://localhost:")


if __name__ == "__main__":
    raise SystemExit(main())
