#!/usr/bin/env python3
"""Gate costly Windows packaging on known changes; unknown diffs always build."""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import subprocess

DOCUMENTATION_FILES = {"README.md", "CHANGELOG.md", "CONTRIBUTING.md",
                       "CODE_OF_CONDUCT.md", "DESIGN.md", "PRODUCT.md", "example/README.md"}
DOCUMENTATION_PREFIXES = ("doc/",)


def needs_windows_build(event_name: str, event: dict, *, run=subprocess.run) -> bool:
    if event_name == "workflow_dispatch":
        return True
    try:
        if event_name == "pull_request":
            base = event["pull_request"]["base"]["sha"]
            head = event["pull_request"]["head"]["sha"]
        elif event_name == "push":
            base, head = event["before"], event["after"]
        else:
            return True
        for revision in (base, head):
            if (not isinstance(revision, str) or not re.fullmatch(r"[0-9a-f]{40}", revision) or
                    revision == "0" * 40):
                return True
            actual = run(["git", "rev-parse", "--verify", f"{revision}^{{commit}}"],
                         check=True, capture_output=True, text=True, encoding="utf-8").stdout.strip()
            if actual != revision:
                return True
        # PRs compare their merge base with the actual head, not a shallow merge
        # commit. Pushes compare the validated before/after pair, including deletions.
        revisions = [f"{base}...{head}"] if event_name == "pull_request" else [base, head]
        changed = run(["git", "diff", "--no-renames", "--name-only", "-z", *revisions, "--"],
                      check=True, capture_output=True, text=True, encoding="utf-8").stdout
        # Skip only known documentation. New or unclassified inputs can affect
        # checkout, generated assets or packaging and must be tested by default.
        return any(name not in DOCUMENTATION_FILES and not name.startswith(DOCUMENTATION_PREFIXES)
                   for name in changed.split("\0") if name)
    except (KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
        return True


def main() -> int:
    try:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
        build = needs_windows_build(os.environ.get("GITHUB_EVENT_NAME", ""), event)
    except (KeyError, TypeError, ValueError, OSError):
        build = True
    value = "true" if build else "false"
    with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as output:
        output.write(f"build={value}\n")
    print(f"Windows native build/package required: {value}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
