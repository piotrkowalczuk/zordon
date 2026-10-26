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
import html
import json
import os
import re
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse

POLL_MS = 5000
SIZE_EVERY_S = 60.0
PR_EVERY_S = 60.0
MAX_CALLS = 30
MAX_OUTPUT = 4000
CODE_LIMIT = 10_000
MAX_FILES = 40

HEADER_LINE = re.compile(r"^# \[[0-9a-f]+\] (.+?) \(invocation, workspace=([^)]+)\)$")
LEVEL_LINE = re.compile(r"^# \[[0-9a-f]+\] ")
ALPHA_LINE = re.compile(r"^\s+alpha pid=(\d+)")
SERVICE_LINE = re.compile(r"^\s+- \[([^\]]*)\] (.+?) — (.+)$")
DETAIL_LINE = re.compile(r"^ {8}(.*)$")
CHECKOUT_LINE = re.compile(r"^checkout: (.+?) \((?:branch (\S+)[^)]*|detached @ [^)]+)\)$")
ERROR_LINE = re.compile(r"\[ERROR\]\s*-?\s*(.*)$")
MCP_TOOL_RE = re.compile(r"^mcp__?(?:plugin_zordon_)?(?:zordon_)?zordon__?(.+)$")
FILE_HEADER_RE = re.compile(r"^diff --git a/.+? b/(.+)$")

_size_cache: dict[str, tuple[int | None, float]] = {}
_pr_cache: dict[str, tuple[dict[str, Any] | None, float]] = {}


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
            }
    except Exception:
        pass
    return {"cwd": "", "workspaces": [], "calls": []}


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
        save_state(state, state_path)
        target_cwd = cwd or state.get("cwd") or os.getcwd()
        snap, _ = query_snapshot(target_cwd, with_extras=False)
        msg = format_session_message(snap)
        if not msg:
            return ("{}\n" if agy else "", 0)
        if agy:
            return (json.dumps({"injectSteps": [{"systemMessage": {"systemMessage": msg}}]}) + "\n", 0)
        return (msg + "\n", 0)

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
    .panel {
      margin-top: 10px;
      border: 1px solid var(--bdr);
      border-radius: 8px;
      overflow: hidden;
      background: var(--panel-bg);
    }
    .panel.border-green { border-color: var(--green); }
    .panel.border-red { border-color: var(--red); }
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
      diffApp: ''
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
        ${state.collapsed ? '' : wsSelect + (state.tab === 'stack' ? renderStack() : renderDiff())}
      `;
    }

    window.toggleFold = k => { state[k] = !state[k]; render(); };
    window.toggleService = n => { state.openService = state.openService === n ? null : n; render(); };
    window.toggleCall = id => { state.openCall = state.openCall === id ? null : id; render(); };
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
    services = sort_services(snap.get("services") or []) if snap.get("kind") in ("running", "stopped") else []
    return {
        "cwd": cwd,
        "workspaces": state.get("workspaces") or ([cwd] if cwd else []),
        "snapshot": snap,
        "services": services,
        "checkouts": checkouts,
        "calls": state.get("calls") or [],
        "summary": summary_line(snap),
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


if __name__ == "__main__":
    raise SystemExit(main())
