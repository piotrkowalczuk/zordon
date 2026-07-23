#!/usr/bin/env python3
"""Publish the repo's Claude Code skills into the built site as an Agent Skills
discovery index (RFC v0.2.0).

Each SKILL.md is copied under /.well-known/agent-skills/<name>/ and the index
digests the copy that is actually served, so a digest can never describe
different bytes than an agent fetches.
"""

import hashlib
import json
import shutil
import sys
from pathlib import Path

import yaml

SCHEMA = "https://schemas.agentskills.io/discovery/0.2.0/schema.json"
BASE = "/.well-known/agent-skills"


def frontmatter(path: Path) -> dict:
    text = path.read_text(encoding="utf-8")
    if not text.startswith("---\n"):
        raise SystemExit(f"{path}: missing YAML frontmatter")
    return yaml.safe_load(text.split("---\n", 2)[1])


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: agent-skills-index.py <site-dir>")

    repo = Path(__file__).resolve().parent.parent
    out = Path(sys.argv[1]).resolve() / BASE.lstrip("/")
    out.mkdir(parents=True, exist_ok=True)

    skills = []
    for source in sorted(repo.glob("skills/*/SKILL.md")):
        meta = frontmatter(source)
        name = meta["name"]
        served = out / name / "SKILL.md"
        served.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, served)
        skills.append(
            {
                "name": name,
                "type": "skill-md",
                "description": meta["description"],
                "url": f"{BASE}/{name}/SKILL.md",
                "digest": "sha256:" + hashlib.sha256(served.read_bytes()).hexdigest(),
            }
        )

    if not skills:
        raise SystemExit("no skills found under skills/*/SKILL.md")

    index = out / "index.json"
    index.write_text(
        json.dumps({"$schema": SCHEMA, "skills": skills}, indent=2) + "\n",
        encoding="utf-8",
    )
    print(f"agent-skills: indexed {len(skills)} skills -> {index}")


if __name__ == "__main__":
    main()
