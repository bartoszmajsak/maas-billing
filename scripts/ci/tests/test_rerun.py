"""Exercise the actual workflow shell with a fake gh; never contact GitHub."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".github/workflows/rerun.yml"
SCRIPT = textwrap.dedent(WORKFLOW.read_text().split("        run: |\n", 1)[1])
FAKE_GH = '''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
with open(os.environ["GH_CALLS"], "a") as log:
    log.write(json.dumps(args) + "\\n")
if args[:2] == ["pr", "view"]:
    print(os.environ.get("HEAD_SHA", "current-head"))
elif args[0] == "api":
    print(os.environ.get("RUNS", ""))
elif args[:2] == ["run", "view"]:
    print({"101": "MaaS API", "102": "MaaS Controller", "103": "Docs"}[args[2]])
elif args[:2] == ["run", "rerun"]:
    sys.exit(1 if args[2] == os.environ.get("FAIL_RUN") else 0)
else:
    raise SystemExit("Unexpected gh call: " + repr(args))
'''


class RerunTests(unittest.TestCase):
    def run_command(self, command, runs="101\tfailure\n102\tcancelled", **extra):
        with tempfile.TemporaryDirectory() as directory:
            gh = Path(directory) / "gh"
            gh.write_text(FAKE_GH)
            gh.chmod(0o755)
            calls_path = Path(directory) / "calls.jsonl"
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"],
                       GH_CALLS=str(calls_path), GH_REPO="example/maas",
                       PR_NUMBER="42", COMMENT_BODY=command, RUNS=runs, **extra)
            result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", SCRIPT],
                                    env=env, capture_output=True, text=True)
            calls = [json.loads(line) for line in calls_path.read_text().splitlines()] if calls_path.exists() else []
            return result, calls

    @staticmethod
    def reruns(calls):
        return [call for call in calls if call[:2] == ["run", "rerun"]]

    def test_failed_jobs_and_cancelled_prerequisites(self):
        result, calls = self.run_command("/rerun-failed")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.reruns(calls), [["run", "rerun", "101", "--failed"],
                                             ["run", "rerun", "102"]])
        api = next(call for call in calls if call[0] == "api")
        self.assertIn("head_sha=current-head&event=pull_request&status=completed", api[2])
        self.assertIn("--paginate", api)
        self.assertIn('.conclusion == "failure" or .conclusion == "cancelled"', api[-1])

    def test_all_jobs(self):
        result, calls = self.run_command("/rerun-all")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.reruns(calls), [["run", "rerun", "101"], ["run", "rerun", "102"]])

    def test_target_workflow(self):
        for name in ("MaaS Controller", '"MaaS Controller"'):
            with self.subTest(name=name):
                result, calls = self.run_command("/rerun-workflow " + name)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(self.reruns(calls), [["run", "rerun", "102"]])

    def test_command_with_surrounding_prose(self):
        result, calls = self.run_command("Please retry CI:\n/rerun-failed\nThanks")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.reruns(calls)), 2)

    def test_crlf_commands(self):
        cases = (
            ("/rerun-failed", [["run", "rerun", "101", "--failed"], ["run", "rerun", "102"]]),
            ("/rerun-all", [["run", "rerun", "101"], ["run", "rerun", "102"]]),
            ("/rerun-workflow MaaS Controller", [["run", "rerun", "102"]]),
        )
        for command, expected in cases:
            with self.subTest(command=command):
                result, calls = self.run_command("Please retry CI:\r\n" + command + "\r\nThanks")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(self.reruns(calls), expected)

    def test_invalid_commands_do_not_call_api(self):
        for command in ("Mention /rerun-all", "> /rerun-all", "/rerun-alligator",
                        "/rerun-failed-extra", "/rerun-workflow ", "/rerun-workflow \"\""):
            with self.subTest(command=command):
                result, calls = self.run_command(command)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])

    def test_no_eligible_runs(self):
        result, calls = self.run_command("/rerun-failed", runs="")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.reruns(calls), [])

    def test_missing_head(self):
        result, calls = self.run_command("/rerun-all", HEAD_SHA="")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 1)

    def test_unknown_workflow(self):
        result, calls = self.run_command("/rerun-workflow Unknown")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.reruns(calls), [])

    def test_rerun_failure_does_not_skip_other_runs(self):
        result, calls = self.run_command("/rerun-all", FAIL_RUN="101")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.reruns(calls)), 2)

    def test_comment_does_not_execute_shell(self):
        result, calls = self.run_command('/rerun-workflow $(echo INJECTED >&2)')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("INJECTED", result.stderr)
        self.assertEqual(self.reruns(calls), [])


if __name__ == "__main__":
    unittest.main()
