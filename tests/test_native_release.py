"""Exercise publication boundaries without creating tags, releases or uploads."""
import copy
import importlib.util
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("native_release_publisher", ROOT / ".github/scripts/publish_native_release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class FakeGitHub:
    head = "a" * 40

    def __init__(self, plan, existing=False, draft=True):
        self.plan = plan
        self.release = {"id": 1, "draft": draft} if existing else None
        self.assets = []
        self.writes = []
        self.bad_upload = False
        self.move_tag = False
        self.annotated = False
        self.next_id = 10

    def asset(self, name):
        self.next_id += 1
        return {"id": self.next_id, "name": name, "state": "uploaded",
                "size": self.plan["paths"][name].stat().st_size,
                "digest": "sha256:" + self.plan["digests"][name]}

    def find(self, tag):
        return self.release

    def api(self, path, method="GET", body=None, **kwargs):
        if method == "DELETE":
            self.writes.append(("delete", path))
            self.assets = [asset for asset in self.assets if asset["id"] != int(path.rsplit("/", 1)[1])]
            return None
        if method == "PATCH":
            self.writes.append(("publish", body))
            self.release.update(body)
            return self.release
        if path.startswith("git/ref/"):
            return {"object": {"type": "tag" if self.annotated else "commit", "sha": "b" * 40 if self.annotated or self.move_tag else self.head}}
        if path.startswith("git/tags/"):
            return {"object": {"type": "commit", "sha": self.head}}
        if path.endswith("/assets?per_page=100"):
            return copy.deepcopy(self.assets)
        raise AssertionError(path)

    def command(self, action, tag, *args):
        self.writes.append((action, tag))
        if action == "create":
            self.release = {"id": 1, "draft": True}
        elif action == "upload":
            asset = self.asset(Path(args[0]).name)
            if self.bad_upload:
                asset["digest"] = "sha256:" + "0" * 64
            self.assets.append(asset)
        elif action == "edit":
            notes = Path(args[args.index("--notes-file") + 1]).read_text()
            assert self.plan["digests"]["SHA256SUMS"] in notes
        else:
            raise AssertionError(action)


class NativeReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "tool").mkdir()
        for name in ("native-core.json", "tool/tailscale-go.json"):
            shutil.copyfile(ROOT / name, self.root / name)
        (self.root / "pubspec.yaml").write_text("name: test_release\nversion: 0.2.0\n")
        self.directory = self.root / "artifacts"
        self.directory.mkdir()
        pins = release.load_pins(self.root)
        self.names = [release.archive_name(pins, target) for target in ("android", "apple")]
        for name in self.names:
            (self.directory / name).write_bytes(("native bytes " + name).encode())
        (self.directory / "SHA256SUMS").write_text("".join(f"{release.sha256(self.directory / name)}  {name}\n" for name in self.names))
        self.plan = release.prepare(self.directory, "v0.2.0", self.root)

    def test_candidate_requires_matching_version_and_both_unchanged_archives(self):
        for kind in ("tag", "changed", "missing", "symlink", "manifest"):
            with self.subTest(kind=kind):
                if kind == "tag":
                    with self.assertRaises(release.BuildError):
                        release.prepare(self.directory, "v0.2.1", self.root)
                    continue
                name = self.names[0] if kind != "manifest" else "SHA256SUMS"
                path = self.directory / name
                original = path.read_bytes()
                try:
                    if kind == "changed":
                        path.write_bytes(b"changed")
                    elif kind == "manifest":
                        path.write_text("0" * 64 + "  unexpected.tar.gz\n")
                    else:
                        path.unlink()
                        if kind == "symlink":
                            path.symlink_to(self.directory / self.names[1])
                    with self.assertRaises(release.BuildError):
                        release.prepare(self.directory, "v0.2.0", self.root)
                finally:
                    if path.is_symlink():
                        path.unlink()
                    path.write_bytes(original)

    def test_prs_manual_runs_and_forks_cannot_enter_publisher(self):
        base = {"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/tags/v0.2.0", "GITHUB_REPOSITORY": release.RELEASE_REPO, "GITHUB_SHA": FakeGitHub.head}
        for change in ({"GITHUB_EVENT_NAME": "pull_request"}, {"GITHUB_EVENT_NAME": "workflow_dispatch"}, {"GITHUB_REF": "refs/heads/main"}, {"GITHUB_REPOSITORY": "other/fork"}, {"GITHUB_ACTIONS": "false"}):
            with self.subTest(change=change), mock.patch.object(release.subprocess, "check_output") as git:
                with self.assertRaises(release.BuildError):
                    release.approved_head("v0.2.0", self.root, dict(base, **change))
                git.assert_not_called()
        with mock.patch.object(release.subprocess, "check_output", side_effect=[FakeGitHub.head, FakeGitHub.head, FakeGitHub.head, ""]):
            self.assertEqual(release.approved_head("v0.2.0", self.root, base), FakeGitHub.head)
        with mock.patch.object(release.subprocess, "check_output", side_effect=[FakeGitHub.head, FakeGitHub.head, FakeGitHub.head, subprocess.CalledProcessError(1, "git")]):
            with self.assertRaises(subprocess.CalledProcessError):
                release.approved_head("v0.2.0", self.root, base)

    def test_new_release_is_public_only_after_verified_uploads(self):
        client = FakeGitHub(self.plan)
        client.annotated = True
        release.publish(self.plan, client.head, client)
        self.assertEqual(len(client.assets), 3)
        self.assertFalse(client.release["draft"])
        self.assertEqual(client.writes[-1][0], "publish")
        self.assertEqual(sum(action[0] == "upload" for action in client.writes), 3)

    def test_partial_draft_resumes_and_replaces_only_draft_mismatches(self):
        client = FakeGitHub(self.plan, existing=True)
        client.assets = [client.asset("SHA256SUMS"), client.asset(self.names[0])]
        client.assets[1]["digest"] = "sha256:" + "0" * 64
        release.publish(self.plan, client.head, client)
        self.assertEqual(sum(action[0] == "delete" for action in client.writes), 1)
        self.assertEqual(sum(action[0] == "upload" for action in client.writes), 2)
        self.assertFalse(client.release["draft"])

    def test_public_release_is_idempotent_and_cannot_be_overwritten(self):
        client = FakeGitHub(self.plan, existing=True, draft=False)
        client.assets = [client.asset(name) for name in self.plan["paths"]]
        release.publish(self.plan, client.head, client)
        self.assertEqual(client.writes, [])
        client.assets[0]["digest"] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(release.BuildError, "immutable"):
            release.publish(self.plan, client.head, client)
        self.assertEqual(client.writes, [])

    def test_bad_upload_and_moved_tag_never_publish(self):
        client = FakeGitHub(self.plan)
        client.bad_upload = True
        with self.assertRaisesRegex(release.BuildError, "checksum verification"):
            release.publish(self.plan, client.head, client)
        self.assertTrue(client.release["draft"])
        self.assertFalse(any(action[0] == "publish" for action in client.writes))
        client = FakeGitHub(self.plan)
        client.move_tag = True
        with self.assertRaisesRegex(release.BuildError, "Remote release tag changed"):
            release.publish(self.plan, client.head, client)
        self.assertEqual(client.writes, [])
        client = FakeGitHub(self.plan)
        original_command = client.command
        def move_after_upload(action, *args):
            original_command(action, *args)
            if action == "upload":
                client.move_tag = True
        client.command = move_after_upload
        with self.assertRaisesRegex(release.BuildError, "Remote release tag changed"):
            release.publish(self.plan, client.head, client)
        self.assertTrue(client.release["draft"])
        self.assertFalse(any(action[0] == "publish" for action in client.writes))

    def test_unexpected_draft_assets_are_not_exposed(self):
        client = FakeGitHub(self.plan, existing=True)
        client.assets = [{"name": "unexpected.txt"}]
        with self.assertRaisesRegex(release.BuildError, "unexpected assets"):
            release.publish(self.plan, client.head, client)
        self.assertEqual(client.writes, [])

    def test_build_metadata_hyphen_does_not_make_a_prerelease(self):
        (self.root / "pubspec.yaml").write_text("version: 0.2.0+build-prod\n")
        plan = release.prepare(self.directory, root=self.root)
        client = FakeGitHub(plan)
        release.publish(plan, client.head, client)
        self.assertFalse(client.release["prerelease"])
        (self.root / "pubspec.yaml").write_text("version: 0.2.1-rc.1\n")
        plan = release.prepare(self.directory, root=self.root)
        client = FakeGitHub(plan)
        release.publish(plan, client.head, client)
        self.assertTrue(client.release["prerelease"])
        self.assertEqual(client.release["make_latest"], "false")


if __name__ == "__main__":
    unittest.main()
