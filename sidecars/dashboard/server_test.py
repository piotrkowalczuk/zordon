#!/usr/bin/env python3
"""Unit and integration tests for sidecars/dashboard/server.py and hooks/zordon-hook.sh."""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from pathlib import Path
from urllib.request import urlopen

import server

RUNNING_TEXT = """# [3aec45fbd85ea76f] /x/Alphasfile (invocation, workspace=main)
  alpha pid=4242 started=2026-10-04T17:00:00Z
  services (3):
    - [go] api — running pid=10 [ready]
        http://localhost:8080
        checkout: /x (branch main)
    - [go] worker — running pid=11 [probing]
    - [pkg] postgres — failed
"""

FEDERATED_TEXT = """# [aaaa1111aaaa1111] /infra/Alphasfile
  alpha pid=900 started=2026-10-04T16:00:00Z
  services (1):
    - [pkg] postgres — running pid=901 [ready]
        postgres://localhost:5432

# [bbbb2222bbbb2222] /proj/Alphasfile (invocation, workspace=feature)
  alpha pid=1000 started=2026-10-04T17:00:00Z
  services (2):
    - [go] api — running pid=1001 [ready]
        checkout: /proj/workspaces/feature/src/api (branch zordon/feature/api)
    - [go] gateway — running pid=1002 [ready]
        checkout: /proj (branch main)
"""


class TestSidecarAndHooks(unittest.TestCase):
    def test_parse_text_status_running_and_stopped(self) -> None:
        s = server.parse_text_status(0, RUNNING_TEXT, "")
        self.assertEqual(s["kind"], "running")
        self.assertEqual(
            [(x["name"], x["isUp"], x["isFailed"]) for x in s["services"]],
            [("api", True, False), ("worker", False, False), ("postgres", False, True)],
        )
        self.assertEqual(s["services"][0]["print"], ["http://localhost:8080"])
        self.assertEqual(server.summary_line(s), "zordon 1/3 up · 1 failed")

        stopped = server.parse_text_status(0, RUNNING_TEXT.replace("pid=4242", "pid=0"), "")
        self.assertEqual(stopped["kind"], "stopped")

    def test_parse_text_status_inactive_and_error(self) -> None:
        inactive = server.parse_text_status(1, "", "[ERROR] - invocation: no Alphasfile at or above /x")
        self.assertEqual(inactive["kind"], "inactive")

        err = server.parse_text_status(1, "", "[ERROR] - alpha reported error: port 5432 in use")
        self.assertEqual(err["kind"], "error")
        self.assertEqual(err["message"], "alpha reported error: port 5432 in use")

    def test_parse_agent_status_json(self) -> None:
        raw = json.dumps(
            {
                "workspace": "feature",
                "alphasfile": "/proj/Alphasfile",
                "state_dir": "/proj/workspaces/feature",
                "state": "running",
                "services": [
                    {
                        "name": "api",
                        "state": "ready",
                        "picked": True,
                        "print": "http://127.0.0.1:8080/",
                        "checkout_path": "/proj/workspaces/feature/src/api",
                        "branch": "zordon/feature/api",
                    },
                    {"name": "postgres", "state": "unhealthy", "health": "connection refused", "shared": True},
                ],
            }
        )
        s = server.parse_agent_status(raw)
        assert s is not None
        self.assertEqual(s["kind"], "running")
        self.assertEqual(s["workspace"]["name"], "feature")
        self.assertEqual(server.summary_line(s), "zordon 1/2 up · 1 failed")
        sorted_names = [x["name"] for x in server.sort_services(s["services"])]
        self.assertEqual(sorted_names, ["postgres", "api"])

    def test_group_checkouts_and_format_size(self) -> None:
        self.assertEqual(server.format_size(512), "512 KB")
        self.assertEqual(server.format_size(1_363_148), "1.3 GB")

        s = server.parse_text_status(0, FEDERATED_TEXT, "")
        groups = server.group_checkouts(s["services"], "/proj/workspaces/feature")
        self.assertEqual(
            [(g["label"], g["branch"], [a["name"] for a in g["apps"]]) for g in groups],
            [
                ("proj", "main", ["gateway"]),
                ("src/api", "zordon/feature/api", ["api"]),
            ],
        )

    def test_parse_git_diff(self) -> None:
        sample = (
            "diff --git a/main.go b/main.go\n"
            "index 111..222 100644\n"
            "--- a/main.go\n"
            "+++ b/main.go\n"
            "@@ -1,2 +1,3 @@\n"
            " package main\n"
            "-old\n"
            "+new1\n"
            "+new2\n"
        )
        files = server.parse_git_diff(sample)
        self.assertEqual(len(files), 1)
        self.assertEqual(files[0]["path"], "main.go")
        self.assertEqual(files[0]["added"], 2)
        self.assertEqual(files[0]["removed"], 1)
        self.assertIn("+new1", files[0]["hunks"])

    def test_hooks_and_http_dashboard(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            state_path = tmp_path / "state.json"
            ws_dir = tmp_path / "ws"
            app_dir = ws_dir / "src" / "api"
            app_dir.mkdir(parents=True)

            # Initialize a git repo under ws/src/api with a modified + untracked file
            subprocess.run(["git", "-C", str(app_dir), "init", "-q"], check=True)
            (app_dir / "hello.go").write_text("package main\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(app_dir), "add", "hello.go"], check=True)
            subprocess.run(
                [
                    "git",
                    "-C",
                    str(app_dir),
                    "-c",
                    "user.email=a@b",
                    "-c",
                    "user.name=z",
                    "-c",
                    "commit.gpgsign=false",
                    "commit",
                    "-q",
                    "-m",
                    "init",
                ],
                check=True,
            )
            (app_dir / "hello.go").write_text("package main\n// edited\n", encoding="utf-8")
            (app_dir / "untracked.txt").write_text("new\n", encoding="utf-8")

            # Fake zordon binary that answers `status --format=json`
            fake_zordon = tmp_path / "fake-zordon"
            fake_zordon.write_text(
                "#!/bin/sh\n"
                'cat <<EOF\n'
                '{"workspace":"feature","alphasfile":"/proj/Alphasfile","state_dir":"'
                + str(ws_dir)
                + '","state":"running","services":[{"name":"api","state":"ready","picked":true,"print":"http://127.0.0.1:8080/","checkout_path":"'
                + str(app_dir)
                + '","branch":"zordon/feature/api"}]}\n'
                "EOF\n",
                encoding="utf-8",
            )
            fake_zordon.chmod(0o755)

            old_bin = os.environ.get("ZORDON_BIN")
            old_state = os.environ.get("ZORDON_AGY_STATE")
            os.environ["ZORDON_BIN"] = str(fake_zordon)
            os.environ["ZORDON_AGY_STATE"] = str(state_path)
            try:
                # 1. Antigravity SessionStart hook
                out, code = server.handle_hook(
                    "session-start",
                    json.dumps({"conversationId": "c1", "workspacePaths": [str(ws_dir)]}),
                    state_path,
                )
                self.assertEqual(code, 0)
                parsed_out = json.loads(out)
                sys_msg = parsed_out["injectSteps"][0]["systemMessage"]["systemMessage"]
                self.assertIn("zordon 1/1 up (workspace=feature", sys_msg)
                self.assertIn("sidecar://zordon/dashboard/", sys_msg)

                # 2. Antigravity PreToolUse & PostToolUse hooks for call_mcp_tool
                pre_payload = {
                    "conversationId": "c1",
                    "workspacePaths": [str(ws_dir)],
                    "stepIdx": 7,
                    "toolCall": {
                        "name": "call_mcp_tool",
                        "args": {
                            "ServerName": "zordon_zordon",
                            "ToolName": "status",
                            "Arguments": {"args": ["--format=json"]},
                        },
                    },
                }
                pre_out, _ = server.handle_hook("pre-tool", json.dumps(pre_payload), state_path)
                self.assertEqual(json.loads(pre_out), {"decision": "allow"})

                post_payload = dict(pre_payload, output="workspace=feature state=running")
                post_out, _ = server.handle_hook("post-tool", json.dumps(post_payload), state_path)
                self.assertEqual(json.loads(post_out), {})

                # 3. Serve HTTP dashboard and verify endpoints
                httpd = ThreadingHTTPServer(("127.0.0.1", 0), server.DashboardHandler)
                port = httpd.server_address[1]
                t = threading.Thread(target=httpd.serve_forever, daemon=True)
                t.start()
                try:
                    with urlopen(f"http://127.0.0.1:{port}/healthz") as r:
                        self.assertEqual(json.loads(r.read().decode()), {"ok": True})
                    with urlopen(f"http://127.0.0.1:{port}/") as r:
                        html_text = r.read().decode()
                        self.assertIn("<title>Zordon</title>", html_text)
                    with urlopen(f"http://127.0.0.1:{port}/api/snapshot") as r:
                        snap_data = json.loads(r.read().decode())
                        self.assertEqual(snap_data["summary"], "zordon 1/1 up")
                        self.assertEqual(len(snap_data["calls"]), 1)
                        self.assertEqual(snap_data["calls"][0]["command"], "status")
                        self.assertEqual(snap_data["calls"][0]["args"], "--format=json")
                        self.assertIsNotNone(snap_data["calls"][0]["durationMs"])
                    with urlopen(f"http://127.0.0.1:{port}/api/diff") as r:
                        diff_data = json.loads(r.read().decode())
                        self.assertEqual(diff_data["app"], "api")
                        self.assertEqual([f["path"] for f in diff_data["files"]], ["hello.go", "untracked.txt"])
                finally:
                    httpd.shutdown()
                    httpd.server_close()
            finally:
                if old_bin is None:
                    os.environ.pop("ZORDON_BIN", None)
                else:
                    os.environ["ZORDON_BIN"] = old_bin
                if old_state is None:
                    os.environ.pop("ZORDON_AGY_STATE", None)
                else:
                    os.environ["ZORDON_AGY_STATE"] = old_state

    def test_plugin_manifests_and_shell_entrypoints(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]

        # 1. Validate .antigravity-plugin/plugin.json
        plugin_manifest = json.loads((repo_root / ".antigravity-plugin" / "plugin.json").read_text(encoding="utf-8"))
        for key in ("name", "displayName", "version", "description", "suggestedPrompts"):
            self.assertIn(key, plugin_manifest)
        self.assertEqual(plugin_manifest["name"], "zordon")
        self.assertTrue(plugin_manifest["suggestedPrompts"])

        # 2. Validate root mcp_config.json and sandbox mcp_config.json
        mcp_cfg = json.loads((repo_root / "mcp_config.json").read_text(encoding="utf-8"))
        self.assertEqual(mcp_cfg["mcpServers"]["zordon"]["command"], "zordon")
        self.assertEqual(mcp_cfg["mcpServers"]["zordon"]["args"], ["mcp"])

        sandbox_mcp = json.loads(
            (repo_root / "examples" / "mcp_http" / "sandbox" / "mcp_config.json").read_text(encoding="utf-8")
        )
        self.assertIn("url", sandbox_mcp["mcpServers"]["zordon"])

        # 3. Validate sidecars/dashboard/sidecar.json
        sidecar_dir = repo_root / "sidecars" / "dashboard"
        sidecar_cfg = json.loads((sidecar_dir / "sidecar.json").read_text(encoding="utf-8"))
        self.assertEqual(sidecar_cfg["command"], "python3")
        self.assertTrue(sidecar_cfg["has_web_ui"])
        self.assertEqual(
            sidecar_cfg["ui_config"]["views"][0]["entrypoint"],
            "SIDECAR_UI_ENTRYPOINT_AUX_PANE",
        )
        sidecar_bin = (sidecar_dir / sidecar_cfg["args"][0]).resolve()
        self.assertTrue(sidecar_bin.is_file(), f"missing sidecar entrypoint: {sidecar_bin}")
        self.assertTrue(os.access(sidecar_bin, os.X_OK), f"sidecar entrypoint not executable: {sidecar_bin}")

        # 4. Validate all Antigravity hooks.json files and referenced scripts
        hook_configs = [
            repo_root / "hooks.json",
            repo_root / ".agents" / "hooks.json",
            repo_root / "examples" / "mcp_http" / "sandbox" / ".agents" / "hooks.json",
        ]
        for cfg_path in hook_configs:
            cfg = json.loads(cfg_path.read_text(encoding="utf-8"))
            self.assertTrue(cfg, f"empty hooks config: {cfg_path}")
            for hook_group in cfg.values():
                for event_name, entries in hook_group.items():
                    if event_name == "enabled":
                        continue
                    for entry in entries:
                        handlers = entry.get("hooks", [entry])
                        for handler in handlers:
                            cmd_tokens = handler["command"].split()
                            script_path = (cfg_path.parent / cmd_tokens[0]).resolve()
                            self.assertTrue(script_path.is_file(), f"{cfg_path}: missing script {script_path}")
                            self.assertTrue(
                                os.access(script_path, os.X_OK), f"{cfg_path}: script not executable {script_path}"
                            )

        # 5. Execute hooks/zordon-hook.sh and sandbox zordon-preflight.sh as subprocesses
        with tempfile.TemporaryDirectory() as tmp:
            state_path = Path(tmp) / "state.json"
            env = dict(os.environ, ZORDON_AGY_STATE=str(state_path))
            hook_sh = repo_root / "hooks" / "zordon-hook.sh"
            res = subprocess.run(
                [str(hook_sh), "pre-tool"],
                input=json.dumps(
                    {
                        "conversationId": "c2",
                        "workspacePaths": [tmp],
                        "stepIdx": 1,
                        "toolCall": {
                            "name": "call_mcp_tool",
                            "args": {"ServerName": "zordon_zordon", "ToolName": "status", "Arguments": {}},
                        },
                    }
                ),
                text=True,
                capture_output=True,
                env=env,
                check=True,
            )
            self.assertEqual(json.loads(res.stdout), {"decision": "allow"})

            preflight_sh = repo_root / "examples" / "mcp_http" / "sandbox" / ".claude" / "hooks" / "zordon-preflight.sh"
            preflight_env = dict(env, ZORDON_MCP_URL="http://127.0.0.1:1/mcp")
            res_agy = subprocess.run(
                [str(preflight_sh)],
                input=json.dumps({"conversationId": "c2", "workspacePaths": [tmp]}),
                text=True,
                capture_output=True,
                env=preflight_env,
                check=True,
            )
            self.assertIn("injectSteps", json.loads(res_agy.stdout))

            res_claude = subprocess.run(
                [str(preflight_sh)],
                input=json.dumps({"session_id": "s1", "cwd": tmp}),
                text=True,
                capture_output=True,
                env=preflight_env,
                check=True,
            )
            self.assertIn("zordon MCP is NOT reachable", res_claude.stdout)


if __name__ == "__main__":
    unittest.main()
