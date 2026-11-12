#!/usr/bin/env python3
"""Unit and integration tests for sidecars/dashboard/server.py and hooks/zordon-hook.sh."""

from __future__ import annotations

import hashlib
import io
import json
import os
import re
import subprocess
import tarfile
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.request import Request, urlopen

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
            old_upd = os.environ.get("ZORDON_UPDATE_CHECK")
            os.environ["ZORDON_BIN"] = str(fake_zordon)
            os.environ["ZORDON_AGY_STATE"] = str(state_path)
            os.environ["ZORDON_UPDATE_CHECK"] = "0"
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
                    dismiss_req = Request(
                        f"http://127.0.0.1:{port}/api/dismiss-update",
                        data=json.dumps({"version": "99.0.0"}).encode("utf-8"),
                        headers={"Content-Type": "application/json"},
                        method="POST",
                    )
                    with urlopen(dismiss_req) as r:
                        self.assertEqual(json.loads(r.read().decode()), {"ok": True, "dismissedUpdate": "99.0.0"})

                    # 4. Verify POST /api/update against a loopback release server (bad checksum rejected, valid tarball installed)
                    plat = server.detect_platform()
                    assert plat is not None
                    archive_name = f"zordon_99.0.0_{plat[0]}_{plat[1]}.tar.gz"
                    tar_bytes, good_sha = _build_release_tarball("99.0.0")
                    served = {"checksum": "0" * 64}

                    class ReleaseHandler(BaseHTTPRequestHandler):
                        def log_message(self, format: str, *args: object) -> None:
                            return

                        def do_GET(self) -> None:
                            if self.path == "/v99.0.0/checksums.txt":
                                payload = f"{served['checksum']}  {archive_name}\n".encode("utf-8")
                            elif self.path == f"/v99.0.0/{archive_name}":
                                payload = tar_bytes
                            else:
                                self.send_response(404)
                                self.end_headers()
                                return
                            self.send_response(200)
                            self.send_header("Content-Length", str(len(payload)))
                            self.end_headers()
                            self.wfile.write(payload)

                    rel_httpd = ThreadingHTTPServer(("127.0.0.1", 0), ReleaseHandler)
                    rel_port = rel_httpd.server_address[1]
                    rel_t = threading.Thread(target=rel_httpd.serve_forever, daemon=True)
                    rel_t.start()
                    old_dl_base = os.environ.get("ZORDON_RELEASE_DOWNLOAD_BASE")
                    try:
                        bin_dir = tmp_path / "install-bin"
                        bin_dir.mkdir()
                        target_zordon = bin_dir / "zordon"
                        target_zordon.write_text(
                            '#!/bin/sh\nif [ "$1" = "--version" ]; then echo "zordon v0.28.0"; exit 0; fi\n',
                            encoding="utf-8",
                        )
                        target_zordon.chmod(0o755)
                        os.environ["ZORDON_BIN"] = str(target_zordon)
                        os.environ["ZORDON_RELEASE_DOWNLOAD_BASE"] = f"http://127.0.0.1:{rel_port}"
                        server._version_cache.clear()

                        st = server.load_state(state_path)
                        st["updateCache"] = {
                            "checkedAt": time.time(),
                            "latestVersion": "99.0.0",
                            "releaseUrl": "https://github.com/piotrkowalczuk/zordon/releases/tag/v99.0.0",
                        }
                        st["dismissedUpdate"] = ""
                        server.save_state(st, state_path)

                        bad_req = Request(f"http://127.0.0.1:{port}/api/update", data=b"", method="POST")
                        with urlopen(bad_req) as r:
                            bad_res = json.loads(r.read().decode())
                        self.assertFalse(bad_res["ok"])
                        self.assertIn("SHA-256 mismatch", bad_res["error"])
                        self.assertIn("v0.28.0", target_zordon.read_text(encoding="utf-8"))

                        served["checksum"] = good_sha
                        good_req = Request(f"http://127.0.0.1:{port}/api/update", data=b"", method="POST")
                        with urlopen(good_req) as r:
                            good_res = json.loads(r.read().decode())
                        self.assertTrue(good_res["ok"])
                        self.assertEqual(good_res["method"], "download")
                        self.assertEqual(good_res["version"], "99.0.0")
                        for b in ("zordon", "alpha", "tommy"):
                            self.assertTrue((bin_dir / b).is_file())
                            self.assertTrue(os.access(bin_dir / b, os.X_OK))
                    finally:
                        rel_httpd.shutdown()
                        rel_httpd.server_close()
                        if old_dl_base is None:
                            os.environ.pop("ZORDON_RELEASE_DOWNLOAD_BASE", None)
                        else:
                            os.environ["ZORDON_RELEASE_DOWNLOAD_BASE"] = old_dl_base
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
                if old_upd is None:
                    os.environ.pop("ZORDON_UPDATE_CHECK", None)
                else:
                    os.environ["ZORDON_UPDATE_CHECK"] = old_upd

    def test_semver_and_compute_update(self) -> None:
        self.assertEqual(
            server.parse_zordon_version("zordon v0.29.0 (9b4437bd1c2e, 2026-07-23T20:00:00Z, go1.26.5)\n"),
            "0.29.0",
        )
        self.assertEqual(
            server.parse_zordon_version("zordon v0.29.1-0.20261112184434-cbc6427e35b4\n"),
            "0.29.1-0.20261112184434-cbc6427e35b4",
        )
        self.assertEqual(server.parse_zordon_version("zordon (devel) (7c8264, go1.26.5)\n"), "(devel)")
        self.assertIsNone(server.parse_zordon_version('{"state":"running"}'))

        sv_29 = server.parse_semver("0.29.0")
        sv_pseudo = server.parse_semver("v0.29.1-0.20261112184434-cbc6427e35b4")
        sv_291 = server.parse_semver("v0.29.1")
        assert sv_29 is not None and sv_pseudo is not None and sv_291 is not None
        self.assertLess(server.compare_semver(sv_29, sv_pseudo), 0)
        self.assertLess(server.compare_semver(sv_pseudo, sv_291), 0)
        self.assertEqual(server.compare_semver(sv_291, sv_291), 0)

        rel = server.parse_latest_release(
            json.dumps(
                {
                    "tag_name": "v0.30.0",
                    "html_url": "https://evil.example/phish",
                    "draft": False,
                    "prerelease": False,
                }
            )
        )
        self.assertEqual(
            rel,
            {"version": "0.30.0", "url": "https://github.com/piotrkowalczuk/zordon/releases/tag/v0.30.0"},
        )
        self.assertIsNone(server.parse_latest_release(json.dumps({"tag_name": "v0.30.0-rc.1", "prerelease": True})))

        self.assertIsNone(server.compute_update("0.30.0", "0.30.0", rel))

        both = server.compute_update("0.29.0", "0.29.0", rel, "/opt/homebrew/bin/zordon")
        assert both is not None
        self.assertTrue(both["isBinaryOutdated"])
        self.assertTrue(both["isPluginOutdated"])
        self.assertEqual(both["binaryMethod"], "brew")
        self.assertEqual(both["binaryUpgradeCmd"], "brew upgrade piotrkowalczuk/tap/zordon")
        self.assertIn("agy plugin install", server.format_update_markdown(both))

        cli_only = server.compute_update("0.29.0", "0.30.0", rel, "/Users/dev/go/bin/zordon")
        assert cli_only is not None
        self.assertTrue(cli_only["isBinaryOutdated"])
        self.assertFalse(cli_only["isPluginOutdated"])
        self.assertEqual(cli_only["binaryMethod"], "download")
        self.assertEqual(cli_only["binaryTargetDir"], "/Users/dev/go/bin")
        self.assertEqual(cli_only["binaryUpgradeCmd"], "download release → /Users/dev/go/bin")
        self.assertIn("sidecar://zordon/dashboard/", server.format_update_markdown(cli_only))

        offline_skew = server.compute_update("0.30.0", "0.29.0", None)
        assert offline_skew is not None
        self.assertFalse(offline_skew["isBinaryOutdated"])
        self.assertTrue(offline_skew["isPluginOutdated"])
        self.assertEqual(offline_skew["latest"], "0.30.0")

        self.assertIsNone(server.compute_update("(devel)", "0.30.0", rel))
        devel_plugin_old = server.compute_update("(devel)", "0.29.0", rel)
        assert devel_plugin_old is not None
        self.assertFalse(devel_plugin_old["isBinaryOutdated"])
        self.assertTrue(devel_plugin_old["isPluginOutdated"])

        self.assertEqual(server.detect_platform("darwin", "arm64"), ("darwin", "arm64"))
        self.assertEqual(server.detect_platform("linux", "aarch64"), ("linux", "arm64"))
        self.assertEqual(server.detect_platform("linux", "x86_64"), ("linux", "amd64"))
        self.assertIsNone(server.detect_platform("win32", "AMD64"))

        digest = "a" * 64
        self.assertEqual(
            server.parse_checksum(f"{digest}  zordon_0.30.0_darwin_arm64.tar.gz\n", "zordon_0.30.0_darwin_arm64.tar.gz"),
            digest,
        )
        self.assertIsNone(server.parse_checksum("short  zordon_0.30.0_darwin_arm64.tar.gz\n", "zordon_0.30.0_darwin_arm64.tar.gz"))
        self.assertFalse(server.perform_binary_upgrade("0.30.0", "/tmp/zordon", download_base_url="file:///etc")["ok"])

        with tempfile.TemporaryDirectory() as tmp:
            tmp_p = Path(tmp)
            cask_dir = tmp_p / "Caskroom" / "zordon" / "0.28.0"
            cask_dir.mkdir(parents=True)
            real_bin = cask_dir / "zordon"
            real_bin.write_text('#!/bin/sh\necho "zordon v0.28.0"\n', encoding="utf-8")
            real_bin.chmod(0o755)
            usr_bin = tmp_p / "bin"
            usr_bin.mkdir()
            link_bin = usr_bin / "zordon"
            link_bin.symlink_to(real_bin)

            brew_log = tmp_p / "brew.log"
            fake_brew = tmp_p / "fake-brew"
            fake_brew.write_text(
                f'#!/bin/sh\necho "$@" > "{brew_log}"\nprintf \'#!/bin/sh\\necho "zordon v0.30.0"\\n\' > "{real_bin}"\n',
                encoding="utf-8",
            )
            fake_brew.chmod(0o755)

            old_brew = os.environ.get("ZORDON_BREW_BIN")
            old_zbin = os.environ.get("ZORDON_BIN")
            os.environ["ZORDON_BREW_BIN"] = str(fake_brew)
            os.environ["ZORDON_BIN"] = str(link_bin)
            try:
                res_brew = server.perform_binary_upgrade("0.30.0", str(link_bin))
                self.assertTrue(res_brew["ok"])
                self.assertEqual(res_brew["method"], "brew")
                self.assertEqual(res_brew["version"], "0.30.0")
                self.assertEqual(brew_log.read_text(encoding="utf-8").strip(), "upgrade piotrkowalczuk/tap/zordon")
            finally:
                if old_brew is None:
                    os.environ.pop("ZORDON_BREW_BIN", None)
                else:
                    os.environ["ZORDON_BREW_BIN"] = old_brew
                if old_zbin is None:
                    os.environ.pop("ZORDON_BIN", None)
                else:
                    os.environ["ZORDON_BIN"] = old_zbin

    def test_session_start_update_prompt_and_dismiss(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            state_path = tmp_path / "state.json"
            ws_dir = tmp_path / "ws"
            ws_dir.mkdir()

            fake_active = tmp_path / "zordon-active"
            fake_active.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = "--version" ]; then echo "zordon v0.28.0 (abc, go1.27.0)"; exit 0; fi\n'
                f'echo \'{{"workspace":"main","alphasfile":"/proj/Alphasfile","state_dir":"{ws_dir}","state":"stopped","services":[]}}\'\n',
                encoding="utf-8",
            )
            fake_active.chmod(0o755)

            fake_inactive = tmp_path / "zordon-inactive"
            fake_inactive.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = "--version" ]; then echo "zordon v0.28.0 (abc, go1.27.0)"; exit 0; fi\n'
                'echo \'{"state":"no-alphasfile"}\'\n',
                encoding="utf-8",
            )
            fake_inactive.chmod(0o755)

            state_path.write_text(
                json.dumps(
                    {
                        "cwd": str(ws_dir),
                        "workspaces": [str(ws_dir)],
                        "calls": [],
                        "updateCache": {
                            "checkedAt": time.time(),
                            "latestVersion": "99.0.0",
                            "releaseUrl": "https://github.com/piotrkowalczuk/zordon/releases/tag/v99.0.0",
                        },
                        "dismissedUpdate": "",
                    }
                ),
                encoding="utf-8",
            )

            old_bin = os.environ.get("ZORDON_BIN")
            old_state = os.environ.get("ZORDON_AGY_STATE")
            old_upd = os.environ.get("ZORDON_UPDATE_CHECK")
            os.environ["ZORDON_AGY_STATE"] = str(state_path)
            os.environ["ZORDON_UPDATE_CHECK"] = "0"
            try:
                # 1. Active workspace prompts via user_markdown (with empty model systemMessage)
                os.environ["ZORDON_BIN"] = str(fake_active)
                out_agy, code = server.handle_hook(
                    "session-start",
                    json.dumps({"conversationId": "c1", "workspacePaths": [str(ws_dir)]}),
                    state_path,
                )
                self.assertEqual(code, 0)
                steps = json.loads(out_agy)["injectSteps"]
                self.assertEqual(len(steps), 2)
                self.assertEqual(steps[1]["systemMessage"]["systemMessage"], "")
                user_md = steps[1]["systemMessage"]["metadata"]["user_markdown"]
                self.assertIn("v99.0.0", user_md)
                self.assertIn("agy plugin install", user_md)

                # 2. Non-Antigravity plain text fallback includes update line
                out_claude, _ = server.handle_hook(
                    "session-start",
                    json.dumps({"session_id": "s1", "cwd": str(ws_dir)}),
                    state_path,
                )
                self.assertIn("↑ zordon v99.0.0 available", out_claude)

                # 3. Dismissing 99.0.0 silences SessionStart for 99.0.0
                st = server.load_state(state_path)
                st["dismissedUpdate"] = "99.0.0"
                server.save_state(st, state_path)
                out_dismissed, _ = server.handle_hook(
                    "session-start",
                    json.dumps({"conversationId": "c1", "workspacePaths": [str(ws_dir)]}),
                    state_path,
                )
                self.assertEqual(len(json.loads(out_dismissed)["injectSteps"]), 1)

                # 4. Inactive repo (no Alphasfile) stays silent on SessionStart,
                #    while dashboard snapshot still carries the update payload.
                st["dismissedUpdate"] = ""
                server.save_state(st, state_path)
                os.environ["ZORDON_BIN"] = str(fake_inactive)
                out_inactive, _ = server.handle_hook(
                    "session-start",
                    json.dumps({"conversationId": "c2", "workspacePaths": [str(ws_dir)]}),
                    state_path,
                )
                self.assertEqual(json.loads(out_inactive), {})
                snap_payload = server.build_snapshot_payload(str(ws_dir), state_path)
                self.assertEqual(snap_payload["snapshot"]["kind"], "inactive")
                self.assertEqual(snap_payload["update"]["latest"], "99.0.0")
            finally:
                if old_bin is None:
                    os.environ.pop("ZORDON_BIN", None)
                else:
                    os.environ["ZORDON_BIN"] = old_bin
                if old_state is None:
                    os.environ.pop("ZORDON_AGY_STATE", None)
                else:
                    os.environ["ZORDON_AGY_STATE"] = old_state
                if old_upd is None:
                    os.environ.pop("ZORDON_UPDATE_CHECK", None)
                else:
                    os.environ["ZORDON_UPDATE_CHECK"] = old_upd

    def test_plugin_manifests_and_shell_entrypoints(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]

        # 1. Validate .antigravity-plugin/plugin.json, .claude-plugin/plugin.json, and register.tsx PLUGIN_VERSION
        plugin_manifest = json.loads((repo_root / ".antigravity-plugin" / "plugin.json").read_text(encoding="utf-8"))
        claude_manifest = json.loads((repo_root / ".claude-plugin" / "plugin.json").read_text(encoding="utf-8"))
        register_tsx = (repo_root / "claude" / "hooks" / "register.tsx").read_text(encoding="utf-8")
        ver_match = re.search(r"export const PLUGIN_VERSION = '([^']+)'", register_tsx)
        self.assertIsNotNone(ver_match)
        assert ver_match is not None
        for key in ("name", "displayName", "version", "description", "suggestedPrompts"):
            self.assertIn(key, plugin_manifest)
        self.assertEqual(plugin_manifest["name"], "zordon")
        self.assertEqual(plugin_manifest["version"], claude_manifest["version"])
        self.assertEqual(ver_match.group(1), claude_manifest["version"])
        self.assertEqual(server.read_plugin_version(repo_root), plugin_manifest["version"])
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


def _build_release_tarball(version: str) -> tuple[bytes, str]:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name in ("zordon", "alpha", "tommy"):
            script = f'#!/bin/sh\nif [ "$1" = "--version" ]; then echo "{name} v{version}"; exit 0; fi\n'.encode("utf-8")
            info = tarfile.TarInfo(name=name)
            info.size = len(script)
            info.mode = 0o755
            tf.addfile(info, io.BytesIO(script))
    raw = buf.getvalue()
    return raw, hashlib.sha256(raw).hexdigest()


if __name__ == "__main__":
    unittest.main()
