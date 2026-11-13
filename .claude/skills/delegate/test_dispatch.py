"""Exercise worker routing and failures without sending paid model requests."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


DISPATCH = Path(__file__).with_name("dispatch.sh").resolve()


class DispatchTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.repo = self.base / "repo"
        self.repo.mkdir()
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        (self.repo / ".git/info/exclude").write_text(".delegate/\n")
        (self.repo / ".delegate").mkdir()
        self.brief = self.repo / ".delegate/brief with $ and ' quotes.md"
        self.brief.write_text("A bounded task; literal $(do-not-run).\n")
        self.bin = self.base / "bin"
        self.bin.mkdir()
        self.capture = self.base / "capture.json"
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        SRAS_TEST_CAPTURE=str(self.capture))
        self.mock("codex", '''
import json, os, pathlib, sys
args = sys.argv[1:]
pathlib.Path(os.environ["SRAS_TEST_CAPTURE"]).write_text(json.dumps({"args": args, "brief": sys.stdin.read()}))
print('{"type":"thread.started","thread_id":"fake-session"}')
if os.environ.get("SRAS_TEST_FAIL"):
    print("simulated provider limit", file=sys.stderr)
    sys.exit(7)
if not os.environ.get("SRAS_TEST_EMPTY"):
    pathlib.Path(args[args.index("--output-last-message") + 1]).write_text("VERDICT: PASS\\n")
''')

    def mock(self, name, body):
        path = self.bin / name
        path.write_text("#!" + sys.executable + "\n" + body)
        path.chmod(0o755)

    def dispatch(self, role="reviewer", model="codex/gpt-6.1-sol", *extra):
        return subprocess.run(["sh", str(DISPATCH), role, model, str(self.brief), *extra],
                              cwd=self.repo, env=self.env, text=True, capture_output=True)

    def test_codex_models_and_role_sandboxes(self):
        for model in ("gpt-6-luna", "gpt-6.1-sol", "gpt-6-astra"):
            for role in ("scout", "reviewer", "implementer"):
                with self.subTest(model=model, role=role):
                    result = self.dispatch(role, "codex/" + model)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    call = json.loads(self.capture.read_text())
                    args = call["args"]
                    self.assertEqual(args[args.index("--model") + 1], model)
                    self.assertEqual(args[args.index("--sandbox") + 1],
                                     "workspace-write" if role == "implementer" else "read-only")
                    self.assertIn("model_reasoning_effort=" + ("medium" if model == "gpt-6-luna" else "high"), args)
                    self.assertEqual(args[:3], ["-a", "never", "exec"])
                    self.assertIn("agents.enabled=false", args)
                    self.assertEqual(call["brief"], self.brief.read_text())
                    self.assertIn("VERDICT: PASS", result.stdout)

    def test_dirty_checkout_blocks_writers_but_allows_readers(self):
        (self.repo / "user-work.go").write_text("unfinished work\n")
        result = self.dispatch("implementer")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("clean checkout", result.stderr)
        self.assertFalse(self.capture.exists())
        self.assertEqual(self.dispatch().returncode, 0)
        self.assertEqual((self.repo / "user-work.go").read_text(), "unfinished work\n")

    def test_git_failure_blocks_writer(self):
        self.mock("git", "import sys\nsys.exit(128)\n")
        result = self.dispatch("implementer")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot inspect", result.stderr)
        self.assertFalse(self.capture.exists())

    def test_explicit_dirty_opt_in_allows_task_repair(self):
        (self.repo / "task-work.go").write_text("work from the previous worker turn\n")
        self.env["DELEGATE_ALLOW_DIRTY"] = "1"
        result = self.dispatch("implementer")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.capture.exists())

    def test_provider_failure_is_not_success(self):
        self.env["SRAS_TEST_FAIL"] = "1"
        result = self.dispatch()
        self.assertEqual(result.returncode, 7)
        self.assertIn("simulated provider limit", result.stderr)
        self.assertNotIn("VERDICT: PASS", result.stdout)

    def test_empty_report_is_not_success(self):
        self.env["SRAS_TEST_EMPTY"] = "1"
        result = self.dispatch()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no usable Codex report", result.stderr)

    def test_invalid_inputs_do_not_launch_worker(self):
        for role, model, extra in (("unknown", "codex/gpt-6-luna", ()),
                                   ("scout", "unknown/model", ()),
                                   ("scout", "codex/gpt-6-luna", ("old-session",))):
            with self.subTest(role=role, model=model, extra=extra):
                self.assertNotEqual(self.dispatch(role, model, *extra).returncode, 0)
                self.assertFalse(self.capture.exists())
        self.brief.write_text("")
        self.assertNotEqual(self.dispatch().returncode, 0)
        self.assertFalse(self.capture.exists())

    def test_opencode_reuses_installed_harness_and_session(self):
        harness = Path.home() / ".agents/skills/delegate/dispatch.sh"
        if not harness.is_file() or not shutil.which("jq"):
            self.skipTest("OpenCode harness and jq are optional installed prerequisites")
        self.mock("opencode", '''
import json, os, pathlib, sys
pathlib.Path(os.environ["SRAS_TEST_CAPTURE"]).write_text(json.dumps({"args": sys.argv[1:]}))
print(json.dumps({"type":"text", "sessionID":"fake-session", "part":{"messageID":"reply", "text":"VERDICT: PASS"}}))
''')
        result = self.dispatch("reviewer", "deepseek-v4.1-flash", "existing-session")
        self.assertEqual(result.returncode, 0, result.stderr)
        args = json.loads(self.capture.read_text())["args"]
        self.assertEqual(args[args.index("-m") + 1], "opencode-go/deepseek-v4.1-flash")
        self.assertEqual(args[args.index("--agent") + 1], "delegate/reviewer")
        self.assertEqual(args[args.index("--session") + 1], "existing-session")
        self.assertIn("VERDICT: PASS", result.stdout)


if __name__ == "__main__":
    unittest.main()
