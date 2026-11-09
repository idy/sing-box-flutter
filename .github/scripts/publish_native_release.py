"""Publish verified native artifacts from a reviewed version tag, never from PRs."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tool"))
from native_common import BuildError, RELEASE_REPO, archive_name, load_pins, parse_manifest, sha256


def release_tag(root: Path, tag: str | None = None) -> str:
    match = re.search(r"(?m)^version:\s*['\"]?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.+-]+)?)['\"]?\s*$", (root / "pubspec.yaml").read_text())
    if not match or (tag is not None and tag != "v" + match[1]):
        raise BuildError("Release tag must match pubspec.yaml version")
    return "v" + match[1]


def prepare(directory: Path, tag: str | None = None, root: Path = ROOT) -> dict:
    tag = release_tag(root, tag)
    pins = load_pins(root)
    names = {archive_name(pins, target) for target in ("android", "apple")}
    paths = {name: directory / name for name in names | {"SHA256SUMS"}}
    if directory.is_symlink() or any(path.is_symlink() or not path.is_file() for path in paths.values()):
        raise BuildError("Release assets must be regular files without symlinks")
    if paths["SHA256SUMS"].stat().st_size > 1024 * 1024:
        raise BuildError("Release checksum manifest is too large")
    expected = parse_manifest(paths["SHA256SUMS"].read_text())
    if set(expected) != names or any(sha256(paths[name]) != expected[name] for name in names):
        raise BuildError("Both native archives must match SHA256SUMS")
    expected["SHA256SUMS"] = sha256(paths["SHA256SUMS"])
    return {"tag": tag, "pins": pins, "paths": paths, "digests": expected}


def approved_head(tag: str, root: Path = ROOT, env: dict | None = None) -> str:
    env = os.environ if env is None else env
    if (env.get("GITHUB_ACTIONS") != "true" or env.get("GITHUB_EVENT_NAME") != "push"
            or env.get("GITHUB_REF") != "refs/tags/" + tag or env.get("GITHUB_REPOSITORY") != RELEASE_REPO):
        raise BuildError("Native publication is allowed only for this repository's version-tag pushes")
    def git(*args):
        return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()
    head = git("rev-parse", "HEAD")
    if git("rev-parse", env["GITHUB_SHA"] + "^{commit}") != head or git("rev-parse", "refs/tags/" + tag + "^{commit}") != head:
        raise BuildError("Checkout and release tag must identify the triggering commit")
    git("merge-base", "--is-ancestor", head, "refs/remotes/origin/" + env.get("DEFAULT_BRANCH", "main"))
    return head


class GitHub:
    def command(self, *args):
        subprocess.run(["gh", "release", *args, "--repo", RELEASE_REPO], check=True, timeout=900)

    def api(self, path, method="GET", body=None, missing=False):
        args = ["gh", "api", "repos/" + RELEASE_REPO + "/" + path, "--method", method]
        if body is not None:
            args += ["--input", "-"]
        result = subprocess.run(args, input=None if body is None else json.dumps(body), text=True, capture_output=True, timeout=60)
        if result.returncode:
            if missing and "HTTP 404" in result.stderr:
                return None
            raise BuildError(result.stderr.strip() or "GitHub API request failed")
        return json.loads(result.stdout) if result.stdout.strip() else None

    def find(self, tag):
        release = self.api("releases/tags/" + tag, missing=True)
        if release is None:
            release = next((entry for entry in self.api("releases?per_page=100") if entry["tag_name"] == tag), None)
        return release


def verify_remote_tag(client, tag: str, head: str) -> None:
    reference = client.api("git/ref/tags/" + tag)["object"]
    for _ in range(8):
        if reference["type"] != "tag":
            break
        reference = client.api("git/tags/" + reference["sha"])["object"]
    if reference["type"] != "commit" or reference["sha"] != head:
        raise BuildError("Remote release tag changed from the verified build commit")


def publish(plan: dict, head: str, client=None) -> None:
    client = GitHub() if client is None else client
    tag, paths, digests = plan["tag"], plan["paths"], plan["digests"]
    verify_remote_tag(client, tag, head)
    release = client.find(tag)
    with tempfile.TemporaryDirectory(prefix="native-release-") as temp:
        notes = Path(temp) / "notes.md"
        notes.write_text(f"Native Android and Apple artifacts for sing_box_flutter {tag[1:]}, built and tested in CI.\n\n"
                         f"sing-box: {plan['pins']['sing_box_version']}. Source and build profiles: "
                         f"https://github.com/{RELEASE_REPO}/tree/{head}\n\n"
                         f"SHA256SUMS SHA-256: `{digests['SHA256SUMS']}`\n\n"
                         "The archives retain upstream and dependency license notices. "
                         "Physical-device VPN forwarding still requires device testing.\n")
        if release is None:
            client.command("create", tag, "--draft", "--verify-tag", "--target", head,
                           "--title", "sing_box_flutter " + tag[1:], "--notes-file", str(notes))
            release = client.find(tag)
        assets = client.api(f"releases/{release['id']}/assets?per_page=100")
        if {asset["name"] for asset in assets} - digests.keys():
            raise BuildError("Release contains unexpected assets; inspect it before publication")
        for name in sorted(paths):
            current = next((asset for asset in assets if asset["name"] == name), None)
            matches = current and current["state"] == "uploaded" and current["size"] == paths[name].stat().st_size and current.get("digest") == "sha256:" + digests[name]
            if matches:
                continue
            if not release["draft"]:
                raise BuildError("Published release assets are immutable; refusing replacement")
            if current:
                client.api(f"releases/assets/{current['id']}", method="DELETE")
            client.command("upload", tag, str(paths[name]))
        final = client.api(f"releases/{release['id']}/assets?per_page=100")
        if (len(final) != len(paths) or {asset["name"] for asset in final} != paths.keys()
                or any(asset["state"] != "uploaded" or asset["size"] != paths[asset["name"]].stat().st_size
                       or asset.get("digest") != "sha256:" + digests[asset["name"]] for asset in final)):
            raise BuildError("Uploaded release assets failed size/checksum verification")
        if release["draft"]:
            verify_remote_tag(client, tag, head)
            client.command("edit", tag, "--notes-file", str(notes))
            prerelease = "-" in tag.split("+", 1)[0]
            client.api(f"releases/{release['id']}", method="PATCH",
                       body={"draft": False, "prerelease": prerelease, "make_latest": "false" if prerelease else "legacy"})
    print("Verified native release: https://github.com/" + RELEASE_REPO + "/releases/tag/" + tag)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, default=ROOT / ".build/release")
    parser.add_argument("--tag")
    parser.add_argument("--check-tag", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    try:
        tag = release_tag(ROOT, args.tag)
        if args.check_tag:
            print("Validated version tag:", tag)
            return 0
        plan = prepare(args.directory, tag)
        if args.dry_run:
            print("Verified native release inputs:", tag)
            print("SHA256SUMS SHA-256:", plan["digests"]["SHA256SUMS"])
            return 0
        publish(plan, approved_head(tag))
        return 0
    except (BuildError, OSError, ValueError, subprocess.SubprocessError) as error:
        print("error:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
