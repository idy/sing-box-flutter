"""Website source pinning, UTF-8 builds and native-CI isolation."""
from __future__ import annotations

import ast
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import re
import subprocess
from types import SimpleNamespace
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


class DeploymentConcurrency(unittest.TestCase):
    def test_non_main_dispatch_cannot_cancel_production(self):
        workflow = (ROOT / ".github/workflows/pages.yml").read_text(encoding="utf-8")
        expression = re.search(r"group: project-website-\$\{\{ (.+) \}\}", workflow).group(1)
        tree = ast.parse(expression.replace("&&", " and ").replace("||", " or "), mode="eval")
        # Evaluate only the workflow's string/boolean comparisons, never calls
        # or other arbitrary expressions in a modified workflow file.
        allowed = (ast.Expression, ast.BoolOp, ast.And, ast.Or, ast.Compare, ast.Eq,
                   ast.NotEq, ast.Attribute, ast.Name, ast.Load, ast.Constant)
        for node in ast.walk(tree):
            self.assertIsInstance(node, allowed)
            if isinstance(node, ast.Name):
                self.assertEqual(node.id, "github")
            if isinstance(node, ast.Attribute):
                self.assertIn(node.attr, ("event_name", "ref"))
        compiled = compile(tree, "pages-concurrency", "eval")
        cases = [
            ("push", "refs/heads/main", "production"),
            ("workflow_dispatch", "refs/heads/main", "production"),
            ("workflow_dispatch", "refs/heads/feature", "refs/heads/feature"),
            ("pull_request", "refs/pull/6/merge", "refs/pull/6/merge"),
            ("pull_request", "refs/heads/main", "refs/heads/main"),
        ]
        for event, ref, expected in cases:
            with self.subTest(event=event, ref=ref):
                context = SimpleNamespace(event_name=event, ref=ref)
                self.assertEqual(eval(compiled, {"__builtins__": {}}, {"github": context}), expected)


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
