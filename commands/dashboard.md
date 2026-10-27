---
name: zordon-cmd-dashboard
description: Show or hide the zordon dashboard pane
---

The zordon plugin's hooks draw the dashboard and answer this command themselves in Claude Code; in Antigravity, offer the `[Zordon](sidecar://zordon/dashboard/)` pill to open the sidecar pane in the auxiliary view.
Call the zordon `status` MCP tool and summarize the stack for me in a few lines: the workspace, which services are up, and anything failed or unhealthy with its reason.
