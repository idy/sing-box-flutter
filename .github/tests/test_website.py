"""Website source pinning, UTF-8 builds and native-CI isolation."""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("website_builder", ROOT / "tool/build_site.py")
site = importlib.util.module_from_spec(spec)
spec.loader.exec_module(site)


class WebsiteBuild(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.output = self.root / ".build/site"
        self.env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        self.env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        subprocess.run(["git", "init", "-q", str(self.root)], env=self.env, check=True)
        self.source = self.root / "site"
        (self.source / "assets").mkdir(parents=True)
        (self.source / "assets/manrope-latin.woff2").write_bytes(b"font fixture")
        (self.source / "assets/OFL-Manrope.txt").write_text("License fixture", encoding="utf-8")
        (self.source / "index.html").write_text(
            '<!doctype html><html><head><title>選択 → café 🛰</title>'
            '<link rel="canonical" href="{{SITE_URL}}"></head>'
            '<body data-core-revision="{{CORE_REVISION}}"><pre>{{WGC_PROFILE}}</pre>'
            '<code>ref: {{CORE_REVISION}}</code></body></html>', encoding="utf-8")
        self.profiles = self.root / "example/core-profiles"
        self.profiles.mkdir(parents=True)
        for name in site.PROFILES:
            (self.profiles / (name + ".yaml")).write_text(
                "# Source profile →\nsing_box_flutter:\n  core:\n    endpoints: [wgc]\n", encoding="utf-8")
        self.commit()
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(site, "ROOT", self.root).start()
        mock.patch.object(site, "OUTPUT", self.output).start()
        mock.patch.dict(os.environ, {"SITE_URL": "https://example.com/project/"}).start()

    def git(self, *args):
        return subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", "-C", str(self.root), *args], env=self.env)

    def commit(self):
        self.git("add", "example")
        self.git("-c", "user.name=Website Tests", "-c", "user.email=tests@example.invalid",
                 "-c", "commit.gpgsign=false", "commit", "-qm",
                 "Website fixture\n\nCo-Authored-By: Claude Code <noreply@anthropic.com>")

    def build(self):
        with contextlib.redirect_stdout(io.StringIO()):
            site.build()
        return (self.output / "index.html").read_bytes().decode("utf-8")

    def test_dependency_and_downloads_use_the_same_commit(self):
        revision = self.git("rev-parse", "HEAD").decode("utf-8").strip()
        document = self.build()
        self.assertIn('data-core-revision="' + revision + '"', document)
        self.assertIn("ref: " + revision, document)
        self.assertIn('href="https://example.com/project/"', document)
        for name in site.PROFILES:
            expected = self.git("show", f"{revision}:example/core-profiles/{name}.yaml")
            self.assertEqual((self.output / "profiles" / (name + ".yaml")).read_bytes(), expected)

    def test_custom_domain_canonical_does_not_keep_the_repository_path(self):
        with mock.patch.dict(os.environ, {"SITE_URL": "https://sing-box-flutter.idy.dev"}):
            document = self.build()
        self.assertIn('href="https://sing-box-flutter.idy.dev/"', document)
        self.assertNotIn('https://sing-box-flutter.idy.dev/sing-box-flutter/', document)

    def test_dirty_profile_does_not_mismatch_an_older_dependency(self):
        path = self.profiles / "wgc-minimal.yaml"
        path.write_text("sing_box_flutter:\n  core:\n    endpoints: [future-protocol]\n", encoding="utf-8")
        self.assertNotIn("future-protocol", self.build())
        self.assertNotIn(b"future-protocol", (self.output / "profiles/wgc-minimal.yaml").read_bytes())

    def test_committed_profile_change_updates_the_dependency_pin(self):
        previous = self.git("rev-parse", "HEAD")
        (self.profiles / "wgc-minimal.yaml").write_text(
            "sing_box_flutter:\n  core:\n    endpoints: [new-capability]\n", encoding="utf-8")
        self.commit()
        current = self.git("rev-parse", "HEAD")
        self.assertNotEqual(current, previous)
        document = self.build()
        self.assertIn("ref: " + current.decode("utf-8").strip(), document)
        self.assertIn("new-capability", document)
        self.assertIn(b"new-capability", (self.output / "profiles/wgc-minimal.yaml").read_bytes())

    def test_text_is_utf8_even_when_path_defaults_to_cp932(self):
        read, write = Path.read_text, Path.write_text

        def locale_read(path, encoding=None, errors=None):
            return read(path, encoding=encoding or "cp932", errors=errors)

        def locale_write(path, data, encoding=None, errors=None):
            return write(path, data, encoding=encoding or "cp932", errors=errors)

        with mock.patch.object(Path, "read_text", locale_read), mock.patch.object(Path, "write_text", locale_write):
            document = self.build()
        self.assertIn("選択 → café 🛰", document)
        self.assertEqual((self.output / "robots.txt").read_bytes(), b"User-agent: *\nAllow: /\n")


class DeploymentEligibility(unittest.TestCase):
    def test_case_sensitive_main_gate_and_deploy_only_concurrency(self):
        workflow = (ROOT / ".github/workflows/pages.yml").read_text(encoding="utf-8")
        self.assertNotIn("\nconcurrency:", workflow)
        deploy = workflow.split("  deploy:\n", 1)[1]
        self.assertIn("if: needs.build.outputs.production == 'true'", deploy)
        self.assertIn("      group: project-website-production\n", deploy)
        self.assertIn("      queue: max\n", deploy)
        self.assertIn("      cancel-in-progress: false\n", deploy)
        self.assertIn("if: steps.production.outputs.eligible == 'true'", workflow)
        body = workflow.split("python3 -I - <<'PY'\n", 1)[1].split("\n          PY", 1)[0]
        body = textwrap.dedent(body)
        cases = [
            ("push", "refs/heads/main", True),
            ("workflow_dispatch", "refs/heads/main", True),
            ("workflow_dispatch", "refs/heads/Main", False),
            ("workflow_dispatch", "refs/heads/MAIN", False),
            ("workflow_dispatch", "refs/heads/feature", False),
            ("workflow_dispatch", "refs/tags/main", False),
            ("pull_request", "refs/pull/6/merge", False),
            ("pull_request", "refs/heads/main", False),
            ("push", "refs/heads/Main", False),
            ("pull_request_target", "refs/heads/main", False),
        ]
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            for event, ref, expected in cases:
                with self.subTest(event=event, ref=ref):
                    output.write_text("", encoding="utf-8")
                    environment = {"WORKFLOW_EVENT": event, "WORKFLOW_REF": ref, "GITHUB_OUTPUT": str(output)}
                    with mock.patch.dict(os.environ, environment):
                        exec(compile(body, "pages-production-gate", "exec"), {})
                    self.assertEqual(output.read_text(encoding="utf-8"), f"eligible={str(expected).lower()}\n")

    def test_isolation_regression_runs_when_native_workflow_changes(self):
        workflow = (ROOT / ".github/workflows/pages.yml").read_text(encoding="utf-8")
        triggers = workflow.split("permissions:", 1)[0]
        push, pull_request = triggers.split("  pull_request:\n", 1)
        self.assertIn("    branches: [main]\n", push)
        self.assertNotIn("    paths:", push)
        self.assertIn("      - .github/workflows/native-core.yml\n", pull_request)


class DeploymentFreshness(unittest.TestCase):
    def setUp(self):
        workflow = (ROOT / ".github/workflows/pages.yml").read_text(encoding="utf-8")
        deploy = workflow.split("  deploy:\n", 1)[1]
        body = deploy.split("python3 -I - <<'PY'\n", 1)[1].split("\n          PY", 1)[0]
        self.body = textwrap.dedent(body)
        self.assertEqual(deploy.count("if: steps.revision.outputs.current == 'true'"), 2)
        self.assertLess(deploy.index("id: revision"), deploy.index("uses: actions/configure-pages"))
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.output = Path(self.directory.name) / "output"
        self.environment = {"PAGES_BUILT_SHA": "a" * 40, "GH_TOKEN": "test-token",
                            "GITHUB_API_URL": "https://api.github.test", "GITHUB_REPOSITORY": "idy/sing-box-flutter",
                            "GITHUB_OUTPUT": str(self.output)}

    def run_guard(self, built, current):
        self.output.write_text("", encoding="utf-8")
        response = io.BytesIO(json.dumps({"object": {"sha": current}}).encode("utf-8"))
        with mock.patch.dict(os.environ, dict(self.environment, PAGES_BUILT_SHA=built)), \
                mock.patch("urllib.request.urlopen", return_value=response) as lookup, \
                contextlib.redirect_stdout(io.StringIO()):
            exec(compile(self.body, "pages-revision-guard", "exec"), {})
        self.assertEqual(lookup.call_args.args[0].full_url,
                         "https://api.github.test/repos/idy/sing-box-flutter/git/ref/heads/main")
        return self.output.read_text(encoding="utf-8")

    def test_newer_build_then_late_older_build_cannot_roll_back(self):
        newest = "b" * 40
        self.assertEqual(self.run_guard(newest, newest), "current=true\n")
        self.assertEqual(self.run_guard("a" * 40, newest), "current=false\n")

    def test_invalid_revision_fails_closed(self):
        with self.assertRaises(SystemExit):
            self.run_guard("a" * 40, "invalid-api-result")
        self.assertEqual(self.output.read_text(encoding="utf-8"), "")

    def test_failed_lookup_does_not_authorize_deployment(self):
        self.output.write_text("", encoding="utf-8")
        with mock.patch.dict(os.environ, self.environment), \
                mock.patch("urllib.request.urlopen", side_effect=OSError("offline")):
            with self.assertRaises(OSError):
                exec(compile(self.body, "pages-revision-guard", "exec"), {})
        self.assertEqual(self.output.read_text(encoding="utf-8"), "")


class NativeCIIsolation(unittest.TestCase):
    def test_website_helpers_skip_native_but_core_inputs_do_not(self):
        workflow = (ROOT / ".github/workflows/native-core.yml").read_text(encoding="utf-8")
        body = workflow.split("python3 -I - <<'PY'\n", 1)[1].split("\n          PY", 1)[0]
        body = textwrap.dedent(body)
        cases = [
            (["tool/build_site.py"], False),
            (["tool/serve_site.mjs"], False),
            (["site/index.html", "tool/build_site.py"], False),
            ([".github/tests/test_website.py"], False),
            (["tool/build_site.py", "tool/build_core.py"], True),
            (["tool/native_common.py"], True),
            (["example/core-profiles/wgc-minimal.yaml"], True),
            (["native/mobile/config.go"], True),
            ([".github/workflows/native-core.yml"], True),
        ]
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            for files, expected in cases:
                with self.subTest(files=files):
                    output.write_text("", encoding="utf-8")
                    environment = {"EVENT_NAME": "pull_request", "BASE_SHA": "a" * 40,
                                   "HEAD_SHA": "b" * 40, "GITHUB_OUTPUT": str(output)}
                    changed = b"\0".join(name.encode("utf-8") for name in files)
                    with mock.patch.dict(os.environ, environment), mock.patch("subprocess.check_output", return_value=changed):
                        exec(compile(body, "native-ci-classifier", "exec"), {})
                    self.assertEqual(output.read_text(encoding="utf-8"), f"native={str(expected).lower()}\n")


if __name__ == "__main__":
    unittest.main()
