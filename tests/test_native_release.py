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
        self.release = {"id": 1, "draft": draft, "body": ""} if existing else None
        self.native_run = None
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
        return copy.deepcopy(self.release)

    def api(self, path, method="GET", body=None, **kwargs):
        if method == "DELETE":
            self.writes.append(("delete", path))
            self.assets = [asset for asset in self.assets if asset["id"] != int(path.rsplit("/", 1)[1])]
            return None
        if method == "PATCH":
            self.writes.append(("publish", body))
            self.release.update(body)
            return self.release
        if path == "actions/runs/42":
            return copy.deepcopy(self.native_run)
        if path == "actions/workflows/native-core.yml":
            return {"id": 7}
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
            self.release = {"id": 1, "draft": True, "body": Path(args[args.index("--notes-file") + 1]).read_text()}
        elif action == "upload":
            asset = self.asset(Path(args[0]).name)
            if self.bad_upload:
                asset["digest"] = "sha256:" + "0" * 64
            self.assets.append(asset)
        elif action == "edit":
            notes = Path(args[args.index("--notes-file") + 1]).read_text()
            assert self.plan["digests"]["SHA256SUMS"] in notes
            self.release["body"] = notes
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

    def context_fixture(self):
        def git(*args):
            return subprocess.check_output(["git", "-C", str(self.root), *args], stderr=subprocess.PIPE).decode().strip()
        git("init", "-q", "-b", "main")
        git("config", "commit.gpgSign", "false")
        git("config", "core.hooksPath", "/dev/null")
        git("config", "user.name", "Release test")
        git("config", "user.email", "release-test@example.invalid")
        git("add", *release.SOURCE_METADATA)
        git("commit", "-q", "-m", "trusted source metadata")
        head = git("rev-parse", "HEAD")
        git("update-ref", "refs/remotes/origin/main", head)
        git("tag", "v0.2.0", head)
        client = FakeGitHub(self.plan)
        client.head = head
        client.native_run = {"event": "push", "conclusion": "success", "workflow_id": 7, "head_sha": head,
                             "repository": {"full_name": release.RELEASE_REPO}, "head_repository": {"full_name": release.RELEASE_REPO}}
        env = {"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_run", "GITHUB_REPOSITORY": release.RELEASE_REPO, "TRUSTED_SHA": head, "DEFAULT_BRANCH": "main"}
        return git, client, env

    def test_publisher_and_installer_share_the_release_tag_length_limit(self):
        (self.root / "pubspec.yaml").write_text("version: 0.2.0+" + "x" * 128 + "\n")
        with self.assertRaises(release.BuildError):
            release.release_tag(self.root)

    def test_only_trusted_workflow_run_can_enter_publisher(self):
        _, client, base = self.context_fixture()
        for change in ({"GITHUB_EVENT_NAME": "push"}, {"GITHUB_EVENT_NAME": "pull_request"}, {"GITHUB_EVENT_NAME": "workflow_dispatch"}, {"GITHUB_REPOSITORY": "other/fork"}, {"GITHUB_ACTIONS": "false"}):
            with self.subTest(change=change), mock.patch.object(release.subprocess, "check_output") as git:
                with self.assertRaises(release.BuildError):
                    release.approved_run(42, self.root, dict(base, **change), client)
                git.assert_not_called()
        context = release.approved_run(42, self.root, base, client)
        self.assertEqual(context["head"], client.head)
        self.assertEqual(context["tag"], "v0.2.0")
        self.assertEqual(set(context["metadata"]), set(release.SOURCE_METADATA))
        # Even extra tagged helper bytes are never materialized or imported.
        context["metadata"]["tool/native_common.py"] = b'raise RuntimeError("tag code executed")\n'
        self.assertEqual(release.prepare_from_run(self.directory, context)["digests"], self.plan["digests"])

    def test_unmerged_tag_is_rejected_before_tagged_helpers_can_run(self):
        git, client, env = self.context_fixture()
        git("checkout", "-q", "-b", "unmerged-tag")
        marker = self.root / "tag-code-executed"
        payload = f'from pathlib import Path\nPath({str(marker)!r}).touch()\n'
        for relative in ("tool/native_common.py", ".github/scripts/publish_native_release.py"):
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(payload)
        git("add", "tool/native_common.py", ".github/scripts/publish_native_release.py")
        git("commit", "-q", "-m", "unmerged tagged helpers")
        client.head = client.native_run["head_sha"] = git("rev-parse", "HEAD")
        git("checkout", "-q", "main")
        with self.assertRaises(subprocess.CalledProcessError):
            release.approved_run(42, self.root, env, client)
        self.assertFalse(marker.exists())
        self.assertEqual(client.writes, [])

    def test_failed_pr_fork_and_wrong_workflow_runs_are_rejected(self):
        _, client, env = self.context_fixture()
        original = copy.deepcopy(client.native_run)
        for changes in ({"event": "pull_request"}, {"conclusion": "failure"}, {"workflow_id": 8}, {"head_repository": {"full_name": "other/fork"}}):
            client.native_run = dict(original, **changes)
            with self.subTest(changes=changes), mock.patch.object(release.subprocess, "check_output") as git:
                with self.assertRaises(release.BuildError):
                    release.approved_run(42, self.root, env, client)
                git.assert_not_called()

    def test_privileged_workflow_checks_out_default_branch_before_gate_and_download(self):
        workflow = (ROOT / ".github/workflows/publish-native.yml").read_text()
        build = (ROOT / ".github/workflows/native-core.yml").read_text()
        self.assertIn("workflow_run:", workflow)
        self.assertIn("ref: ${{ github.sha }}", workflow)
        self.assertNotIn("ref: ${{ github.event.workflow_run.head_sha }}", workflow)
        self.assertLess(workflow.index("--check-run"), workflow.index("uses: actions/download-artifact"))
        self.assertNotIn("contents: write", build)
        self.assertNotIn("GH_TOKEN:", build)

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

    def test_existing_draft_notes_survive_and_managed_section_is_idempotent(self):
        client = FakeGitHub(self.plan, existing=True)
        original = "## Upgrade instructions\nKeep the maintainer's changelog.  \n\n"
        client.release["body"] = original
        release.publish(self.plan, client.head, client)
        body = client.release["body"]
        self.assertTrue(body.startswith(original))
        self.assertEqual(body.count(release.NOTES_START), 1)
        self.assertIn(self.plan["digests"]["SHA256SUMS"], body)
        self.assertEqual(release.merge_notes(original, "new metadata"), release.merge_notes(release.merge_notes(original, "old metadata"), "new metadata"))
        self.assertTrue(release.merge_notes(body + "\nManual footer\n", "replacement").endswith("\nManual footer\n"))

    def test_notes_edited_during_upload_are_preserved(self):
        client = FakeGitHub(self.plan, existing=True)
        client.release["body"] = "Initial notes\n"
        original_command = client.command
        def edit_during_upload(action, *args):
            original_command(action, *args)
            if action == "upload":
                client.release["body"] = "Latest maintainer instructions\n"
        client.command = edit_during_upload
        release.publish(self.plan, client.head, client)
        self.assertTrue(client.release["body"].startswith("Latest maintainer instructions\n"))

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
