#!/usr/bin/env python3
"""Build the static project website from its source and actual example profiles."""
from __future__ import annotations

import html
import os
from html.parser import HTMLParser
from pathlib import Path
import re
import shutil
import subprocess
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / ".build/site"
PROFILES = ("wgc-minimal", "wgc-tailscale", "singbox-full")


class LocalReferences(HTMLParser):
    def __init__(self):
        super().__init__()
        self.references = []
        self.ids = set()

    def handle_starttag(self, tag, attrs):
        attributes = dict(attrs)
        if "id" in attributes:
            if attributes["id"] in self.ids:
                raise ValueError(f"Duplicate HTML id: {attributes['id']}")
            self.ids.add(attributes["id"])
        for name in ("src", "href"):
            value = attributes.get(name)
            if value and not urlsplit(value).scheme and not value.startswith("//"):
                self.references.append(value)


def source_profiles():
    """Pin the dependency, downloads and source links to one committed tree."""
    revision = subprocess.check_output(
        ["git", "-C", str(ROOT), "rev-parse", "--verify", "HEAD^{commit}"],
        encoding="utf-8",
    ).strip()
    if re.fullmatch(r"[0-9a-f]{40}", revision) is None:
        raise ValueError("Website source revision must be a full commit ID")
    profiles = {}
    for name in PROFILES:
        profiles[name] = subprocess.check_output(
            ["git", "-C", str(ROOT), "show", f"{revision}:example/core-profiles/{name}.yaml"]
        )
        profiles[name].decode("utf-8")  # Reject invalid text before publishing any output.
    return revision, profiles


def build():
    revision, profiles = source_profiles()
    if (ROOT / ".build").is_symlink() or OUTPUT.is_symlink():
        raise ValueError("Website build output must not be a symlink")
    if OUTPUT.exists():
        shutil.rmtree(OUTPUT)
    shutil.copytree(ROOT / "site", OUTPUT)
    profile_dir = OUTPUT / "profiles"
    profile_dir.mkdir()
    for name in PROFILES:
        (profile_dir / f"{name}.yaml").write_bytes(profiles[name])
    document = OUTPUT / "index.html"
    template = document.read_text(encoding="utf-8")
    marker = "{{WGC_PROFILE}}"
    if template.count(marker) != 1:
        raise ValueError("Expected exactly one default-profile marker")
    # Keep the initial page readable without JavaScript. Strip only introductory
    # comments for this view; profile downloads retain every original byte.
    profile = profiles["wgc-minimal"].decode("utf-8")
    profile = profile[profile.index("sing_box_flutter:"):]
    site_url = os.environ.get("SITE_URL", "https://idy.github.io/sing-box-flutter/").rstrip("/") + "/"
    parsed_url = urlsplit(site_url)
    if (parsed_url.scheme != "https" or not parsed_url.hostname or parsed_url.username or
            parsed_url.password or parsed_url.query or parsed_url.fragment):
        raise ValueError("SITE_URL must be a public HTTPS site URL without credentials, query or fragment")
    if "{{CORE_REVISION}}" not in template:
        raise ValueError("Missing dependency revision marker")
    document.write_text(
        template.replace(marker, html.escape(profile)).replace("{{CORE_REVISION}}", revision)
        .replace("{{SITE_URL}}", html.escape(site_url, quote=True)),
        encoding="utf-8",
    )
    (OUTPUT / ".nojekyll").write_text("", encoding="utf-8")
    # Byte output keeps robots.txt line endings stable on Windows as well.
    (OUTPUT / "robots.txt").write_bytes(b"User-agent: *\nAllow: /\n")
    parser = LocalReferences()
    parser.feed(document.read_text(encoding="utf-8"))
    for reference in parser.references:
        parsed = urlsplit(reference)
        if not parsed.path:
            if parsed.fragment and parsed.fragment not in parser.ids:
                raise ValueError(f"Missing anchor: {reference}")
            continue
        local = (OUTPUT / unquote(parsed.path)).resolve()
        if not local.is_relative_to(OUTPUT.resolve()) or not local.is_file():
            raise ValueError(f"Missing or external local file: {reference}")
    required = ("assets/manrope-latin.woff2", "assets/OFL-Manrope.txt")
    for relative in required:
        if not (OUTPUT / relative).is_file():
            raise ValueError(f"Missing site asset: {relative}")
    print(f"Website built: {OUTPUT.relative_to(ROOT)}")
    print("HTML anchors, local files and all three original example profiles verified.")


if __name__ == "__main__":
    build()
