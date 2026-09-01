# Search targets

Working file, not published — MkDocs builds `docs/` only.
It exists so the next iteration reads measurements instead of re-guessing.

## Diagnosis (2026-09-01)

zordon.io drew **81 impressions and 1 click over 28 days** (2026-08-04 → 2026-08-31).

This is not a ranking problem.
Half the site already ranks top-10 and takes 2–3 impressions doing it:

| Page | Impressions | Avg. position |
|---|---|---|
| `/concepts/` | 3 | 3.3 |
| `/alphasfile/` | 2 | 5.0 |
| `/architecture/` | 3 | 5.3 |
| `/workspaces/` | 2 | 5.5 |
| `/reference/mcp/` | 3 | 6.0 — **the only click on the site** |
| `/services/go/` | 1 | 10.0 |
| `/services/pkg/` | 6 | 22.8 |
| `/getting-started/` | 6 | 28.7 |
| `/lifecycle/` | 8 | 29.3 |
| `/` | 8 | 33.1 |
| `/dynamic-config/` | 8 | 38.8 |
| `/how-to/install-a-cli-tool-with-mise/` | 20 | 44.4 |
| `/how-to/clean-up-provision-side-effects/` | 2 | 48.5 |

Two populations.
The top block wins brand+feature navigational queries (`zordon alphasfile`, `zordon mcp`) that only people who already know the tool type — zero volume by construction.
The bottom block is filler on page 3–5 of broad head terms.
Ranking #1 on everything currently surfaced would still be ~10 clicks/month, so the constraint is **which queries we target**, not where we place on them.

Two structural facts:

- **The brand is contested.** Power Rangers, plus `zordon.pl`, `zordon.co`, `zordontechnologies.com`, and an npm package named `zordon`. `github.com/piotrkowalczuk/zordon` currently outranks `zordon.io` for the name. Brand-term SEO is a dead end for now; not worth a rename.
- **The vocabulary is invented.** Nobody searches "Alphasfile" or "process supervisor for agentic workflows". The niche is new; the *problem* is not.

Technical SEO is not the bottleneck — sitemap, IndexNow-on-deploy, per-page descriptions, canonicals, `llms.txt` and the ARD catalog all ship already.

## Pocket A — parallel coding agents (first wave, shipped 2026-09-01)

Chosen because the demand exists today, the SERPs are blog-dominated rather than brand-dominated, and every article that ranks stops at "use git worktrees" — which isolates code and leaves the stack shared. That gap is `zordon workspace`.

| Target query (unvalidated) | Page |
|---|---|
| `git worktrees ai coding agents` | `/blog/git-worktrees-dont-isolate-your-stack/` |
| `run multiple claude code agents in parallel` | `/blog/git-worktrees-dont-isolate-your-stack/` |
| `claude code worktree database` | `/blog/git-worktrees-dont-isolate-your-stack/` |
| `git worktree separate .env port` | `/blog/git-worktrees-dont-isolate-your-stack/` |
| `claude code port already in use` | `/blog/parallel-agent-port-conflicts/` |
| `multiple agents same port` | `/blog/parallel-agent-port-conflicts/` |
| `parallel agents database conflict` | `/blog/parallel-agent-port-conflicts/` |
| `run 3 claude code sessions at once` | `/blog/parallel-agent-port-conflicts/` |
| `docker compose alternative local development` | `/compare/docker-compose/` |
| `docker compose without containers` | `/compare/docker-compose/` |
| `docker compose per git worktree` | `/compare/docker-compose/` |
| `devcontainer for ai agent` | `/compare/devcontainers/` |
| `container per coding agent` | `/compare/devcontainers/` |
| `sandbox for coding agents` | `/compare/devcontainers/` |
| conversion target for all of the above | `/how-to/run-parallel-agents-with-workspaces/` |

Every row is a **guess** until Phase 0 below fills in volume and the queries GSC actually reports.

## Phase 0 — ground truth (manual, needs Search Console access)

1. **GSC → Performance → Search results → last 3 months → QUERIES tab.** Export with impressions + average position. Then filter by page `= /reference/mcp/` to identify the single converting query.
   Expect most rows withheld: GSC anonymizes low-volume queries. Thin data is a finding, it confirms the diagnosis.
2. **Google Ads → Keyword Planner** (free with an Ads account, no spend): paste the table above, record volume ranges here.
3. **Vocabulary mining** — Reddit (`r/ClaudeAI`, `r/ChatGPTCoding`), HN search, and the H1s of whatever currently ranks for `run multiple claude code agents in parallel`. H1s should mirror the searcher's phrasing, not the product's.
4. Record the results in this file. Correct the page titles against them; that is cheap now and expensive later.

## Decision gate — 4 weeks after publish (≈ 2026-09-29)

Success at this stage is **named queries appearing at all**, at any position.

- Pull GSC Queries filtered to the six new URLs.
- Check Indexing → Pages, and run URL Inspection → Request indexing on each new URL. IndexNow already pings on deploy; Google is the one that matters.
- **If impressions appear on queries we can name** → second wave of 3 posts in the same pocket.
- **If nothing appears** → the pocket is wrong, not the execution. Reuse the same page template on:
  - **Pocket B** — no-containers / local dev: `run postgres locally without docker`, `testcontainers alternative`.
  - **Pocket C** — Procfile process managers: `overmind alternative`, `process-compose vs`, `foreman replacement`. Established category, existing vocabulary, comparison intent.

## Off-site (not yet done)

- GitHub repo description is `Defends development environment from Rita, and her endless waves of containers!` — zero keywords, on the page that currently outranks the site for the brand. Replace; the joke keeps its home in the README.
- Repo topics: add `claude-code`, `mcp`, `ai-agents`, `developer-tools`, `local-development`, `process-manager`.
- Distribution: Show HN on the pillar post, `r/ClaudeAI`, lobste.rs, PRs to `awesome-claude-code` / `awesome-mcp-servers`. Cross-post to dev.to with `canonical_url` back to zordon.io.
