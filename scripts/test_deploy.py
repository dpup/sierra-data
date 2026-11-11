import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import deploy


REGISTRY = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
CONFIG = {"repositories": {"grid": REGISTRY + "/grid", "relay": REGISTRY + "/relay"}}


def arguments(**values):
    return SimpleNamespace(**{"profile": None, "region": "us-east-1", "function": "coordinator",
                              "tag": "abc123", **values})


class DeployTests(unittest.TestCase):
    def test_ambient_credentials_use_the_normal_aws_chain(self):
        with patch.dict(os.environ, {"AWS_PROFILE": "ambient", "AWS_SESSION_TOKEN": "temporary"}), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=0, stdout="{}")) as run:
            deploy.aws(arguments(), "sts", "get-caller-identity")
        command = run.call_args.args[0]
        self.assertNotIn("--profile", command)
        self.assertEqual(run.call_args.kwargs["env"]["AWS_SESSION_TOKEN"], "temporary")

    def test_build_discovers_both_destinations_and_keeps_credentials(self):
        with patch.dict(os.environ, {"AWS_SESSION_TOKEN": "temporary"}), \
                patch("deploy.subprocess.run") as run:
            deploy.build_images(arguments(), CONFIG)
        self.assertEqual([c.args[0] for c in run.call_args_list], [
            ["make", "ecr-push-grid", "DOCKER_REGISTRY=" + REGISTRY, "DOCKER_IMAGE_NAME=grid", "DOCKER_TAG=abc123"],
            ["make", "ecr-push-relay", "DOCKER_REGISTRY=" + REGISTRY, "RELAY_IMAGE_NAME=relay", "DOCKER_TAG=abc123"],
        ])
        self.assertTrue(all(c.kwargs["cwd"] == deploy.REPO for c in run.call_args_list))
        self.assertEqual(run.call_args.kwargs["env"]["AWS_SESSION_TOKEN"], "temporary")

    def test_explicit_profile_applies_to_build_helpers_too(self):
        with patch.dict(os.environ, {"AWS_ACCESS_KEY_ID": "temporary", "AWS_SESSION_TOKEN": "temporary",
                                     "AWS_DEFAULT_PROFILE": "other"}), patch("deploy.subprocess.run") as run:
            deploy.build_images(arguments(profile="chosen"), CONFIG)
        env = run.call_args.kwargs["env"]
        self.assertEqual(env["AWS_PROFILE"], "chosen")
        self.assertEqual(env["AWS_DEFAULT_PROFILE"], "chosen")
        self.assertNotIn("AWS_ACCESS_KEY_ID", env)
        self.assertNotIn("AWS_SESSION_TOKEN", env)
        self.assertEqual(env["AWS_DEFAULT_REGION"], "us-east-1")

    def test_invalid_repository_is_refused_before_any_build(self):
        config = {"repositories": {"grid": CONFIG["repositories"]["grid"],
                                   "relay": REGISTRY + "/$(shell echo bad)"}}
        with patch("deploy.subprocess.run") as run, self.assertRaises(RuntimeError):
            deploy.build_images(arguments(), config)
        run.assert_not_called()

    def test_images_resolve_separate_immutable_digests(self):
        def aws(args, *command):
            repository = command[command.index("--repository-name") + 1]
            return {"imageDetails": [{"imageDigest": "sha256:" + ("a" if repository == "grid" else "b") * 64}]}

        with patch("deploy.aws", side_effect=aws):
            result = deploy.images(arguments(), CONFIG)
        self.assertEqual(result, {"grid": REGISTRY + "/grid@sha256:" + "a" * 64,
                                  "relay": REGISTRY + "/relay@sha256:" + "b" * 64})

    def test_deploy_builds_before_resolving_and_waits_for_coordinator(self):
        events = []
        images = {"grid": "grid@sha256:a", "relay": "relay@sha256:b"}
        with patch("sys.argv", ["deploy.py", "deploy", "--build", "--tag", "abc123", "--operation-id", "request", "--wait"]), \
                patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.configuration", return_value=CONFIG), \
                patch("deploy.build_images", side_effect=lambda *_: events.append("build")), \
                patch("deploy.images", side_effect=lambda *_: events.append("resolve") or images), \
                patch("deploy.invoke", return_value={"operationId": "request"}) as invoke, \
                patch("deploy.wait", return_value=0) as wait, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(), 0)
        self.assertEqual(events, ["build", "resolve"])
        self.assertEqual(invoke.call_args.args[1], {"command": "start", "action": "deploy",
                                                  "operationId": "request", "images": images})
        wait.assert_called_once()

    def test_failed_push_never_requests_a_deployment(self):
        with patch("sys.argv", ["deploy.py", "deploy", "--build", "--tag", "abc123"]), \
                patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.configuration", return_value=CONFIG), \
                patch("deploy.build_images", side_effect=subprocess.CalledProcessError(1, ["make"])), \
                patch("deploy.invoke") as invoke, patch("deploy.images") as resolve:
            with self.assertRaises(subprocess.CalledProcessError):
                deploy.main()
        invoke.assert_not_called()
        resolve.assert_not_called()

    def test_build_without_tag_uses_repository_commit(self):
        with patch("sys.argv", ["deploy.py", "deploy", "--build"]), patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(stdout="0123456789ab\n")) as git, \
                patch("deploy.configuration", return_value=CONFIG), patch("deploy.build_images") as build, \
                patch("deploy.images", return_value={}), patch("deploy.invoke", return_value={}), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(), 0)
        self.assertEqual(build.call_args.args[0].tag, "0123456789ab")
        self.assertEqual(git.call_args.kwargs["cwd"], deploy.REPO)

    def test_dirty_tree_never_builds_or_deploys(self):
        # Images are tagged with the commit; uncommitted changes would ship under it.
        with patch("sys.argv", ["deploy.py", "deploy", "--build", "--tag", "abc123"]), \
                patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=["api/v1/common.pb.go"]), \
                patch("deploy.configuration") as config, patch("deploy.build_images") as build, \
                patch("deploy.invoke") as invoke, contextlib.redirect_stderr(io.StringIO()) as err:
            with self.assertRaises(SystemExit) as exit:
                deploy.main()
        self.assertEqual(exit.exception.code, 2)
        self.assertIn("api/v1/common.pb.go", err.getvalue())
        self.assertIn("--allow-dirty", err.getvalue())
        config.assert_not_called()
        build.assert_not_called()
        invoke.assert_not_called()

    def test_allow_dirty_is_an_explicit_override(self):
        with patch("sys.argv", ["deploy.py", "deploy", "--build", "--tag", "abc123", "--allow-dirty"]), \
                patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=["Makefile"]) as dirty, \
                patch("deploy.configuration", return_value=CONFIG), patch("deploy.build_images") as build, \
                patch("deploy.images", return_value={}), patch("deploy.invoke", return_value={}), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(), 0)
        dirty.assert_not_called()
        build.assert_called_once()

    def test_dirty_files_lists_tracked_changes_only(self):
        porcelain = " M api/v1/common.pb.go\nM  Makefile\n"
        with patch("deploy.subprocess.run", return_value=SimpleNamespace(stdout=porcelain)) as git:
            self.assertEqual(deploy.dirty_files(), ["api/v1/common.pb.go", "Makefile"])
        self.assertIn("--untracked-files=no", git.call_args.args[0])
        self.assertEqual(git.call_args.kwargs["cwd"], deploy.REPO)

    def test_wrong_branch_never_builds_or_deploys(self):
        # main is the Postgres stack; only the frozen legacy branch runs on this infrastructure.
        with patch("sys.argv", ["deploy.py", "deploy", "--build"]), \
                patch("deploy.branch_drift", return_value="HEAD is on main, but the legacy stack deploys only from legacy/efs-sqlite"), \
                patch("deploy.dirty_files") as dirty, patch("deploy.configuration") as config, \
                patch("deploy.build_images") as build, patch("deploy.invoke") as invoke, \
                contextlib.redirect_stderr(io.StringIO()) as err:
            with self.assertRaises(SystemExit) as exit:
                deploy.main()
        self.assertEqual(exit.exception.code, 2)
        self.assertIn("legacy/efs-sqlite", err.getvalue())
        dirty.assert_not_called()
        config.assert_not_called()
        build.assert_not_called()
        invoke.assert_not_called()

    def test_branch_check_is_only_for_builds(self):
        # A --tag deploy names images that already exist; the checkout is irrelevant.
        with patch("sys.argv", ["deploy.py", "deploy", "--tag", "abc123"]), patch("deploy.branch_drift") as drift, \
                patch("deploy.configuration", return_value=CONFIG), patch("deploy.images", return_value={}), \
                patch("deploy.invoke", return_value={}), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(), 0)
        drift.assert_not_called()

    def git_responses(self, **answers):
        def run(command, **_):
            key = " ".join(command[1:])
            if key.startswith("fetch"):
                return SimpleNamespace(returncode=0, stdout="")
            return SimpleNamespace(stdout=answers[key] + "\n")
        return run

    def test_branch_drift_refuses_another_branch_before_touching_origin(self):
        with patch("deploy.subprocess.run", side_effect=self.git_responses(**{"rev-parse --abbrev-ref HEAD": "main"})) as git:
            drift = deploy.branch_drift("legacy/efs-sqlite")
        self.assertIn("on main", drift)
        self.assertIn("legacy/efs-sqlite", drift)
        self.assertEqual(len(git.call_args_list), 1)

    def test_branch_drift_names_a_detached_head(self):
        with patch("deploy.subprocess.run", side_effect=self.git_responses(**{"rev-parse --abbrev-ref HEAD": "HEAD"})):
            self.assertIn("detached", deploy.branch_drift("legacy/efs-sqlite"))

    def test_branch_drift_fetches_and_compares_with_origin(self):
        answers = {"rev-parse --abbrev-ref HEAD": "legacy/efs-sqlite", "rev-parse HEAD": "aaa",
                   "rev-parse origin/legacy/efs-sqlite": "bbb",
                   "rev-list --count origin/legacy/efs-sqlite..HEAD": "2", "rev-list --count HEAD..origin/legacy/efs-sqlite": "0"}
        with patch("deploy.subprocess.run", side_effect=self.git_responses(**answers)) as git:
            drift = deploy.branch_drift("legacy/efs-sqlite")
        self.assertIn("2 commit(s) ahead of and 0 behind origin/legacy/efs-sqlite", drift)
        self.assertIn(["git", "fetch", "--quiet", "origin", "legacy/efs-sqlite"], [c.args[0] for c in git.call_args_list])
        self.assertTrue(all(c.kwargs["cwd"] == deploy.REPO for c in git.call_args_list))

    def test_branch_drift_reports_a_missing_or_unreachable_origin(self):
        def run(command, **_):
            if command[1] == "fetch":
                return SimpleNamespace(returncode=128, stderr="fatal: couldn't find remote ref legacy/efs-sqlite")
            return SimpleNamespace(stdout="legacy/efs-sqlite\n")
        with patch("deploy.subprocess.run", side_effect=run):
            drift = deploy.branch_drift("legacy/efs-sqlite")
        self.assertIn("cannot compare legacy/efs-sqlite with origin", drift)
        self.assertIn("couldn't find remote ref", drift)

    def test_branch_drift_is_none_when_head_matches_origin(self):
        answers = {"rev-parse --abbrev-ref HEAD": "legacy/efs-sqlite", "rev-parse HEAD": "aaa",
                   "rev-parse origin/legacy/efs-sqlite": "aaa"}
        with patch("deploy.subprocess.run", side_effect=self.git_responses(**answers)):
            self.assertIsNone(deploy.branch_drift("legacy/efs-sqlite"))

    def test_completed_rollback_returns_nonzero_instead_of_ecs_stable(self):
        state = {"operation": None, "last": {"id": "request", "result": "ROLLED_BACK"}, "owner": {"arn": "owner"}}
        with patch("deploy.invoke", return_value=state), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.wait(arguments(timeout=1), "request"), 2)

    def test_blocked_or_superseded_workflow_is_refused(self):
        cases = [{"operation": {"id": "request", "phase": "BLOCKED", "execution": "flow", "error": "unknown owner"}},
                 {"operation": {"id": "different"}}]
        for state in cases:
            with self.subTest(state=state), patch("deploy.invoke", return_value=state), \
                    contextlib.redirect_stdout(io.StringIO()), self.assertRaises(RuntimeError):
                deploy.wait(arguments(timeout=1), "request")

    def test_unsafe_tag_cannot_reach_make(self):
        with patch("sys.argv", ["deploy.py", "deploy", "--build", "--tag", "$(shell bad)"]), \
                patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.configuration") as config, contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit):
            deploy.main()
        config.assert_not_called()

    def test_lambda_invoke_uses_temp_files_and_propagates_server_failure(self):
        def aws(args, *command):
            payload = json.loads(Path(command[command.index("--payload") + 1].removeprefix("fileb://")).read_text())
            self.assertEqual(payload, {"command": "status"})
            Path(command[-1]).write_text(json.dumps({"errorMessage": "lock retained"}))
            return {"FunctionError": "Unhandled"}

        with patch("deploy.aws", side_effect=aws), self.assertRaisesRegex(RuntimeError, "lock retained"):
            deploy.invoke(arguments(), {"command": "status"})


class FakeClock:
    """time.monotonic/time.sleep stand-ins: each sleep advances the clock."""

    def __init__(self, step=20):
        self.now, self.step = 0.0, step

    def monotonic(self):
        return self.now

    def sleep(self, _):
        self.now += self.step


OWNER = {"arn": "arn:aws:ecs:us-east-1:123456789012:task/example-cluster/0f1e2d3c4b5a69788796a5b4c3d2e1f0",
         "definition": "arn:aws:ecs:us-east-1:123456789012:task-definition/example-coordinated:3",
         "ip": "10.0.0.30", "slot": "example-b", "state": "RUNNING",
         "processes": {"grid": {"instanceId": "aaaa1111bbbb2222", "restartCount": None},
                       "relay": {"instanceId": "cccc3333dddd4444", "restartCount": 0}}}


def statuses(*phases, result="SUCCEEDED", op="op-1"):
    """Coordinator status responses: one per phase poll, then the finished state."""
    out = [{"operation": {"id": op, "phase": p, "execution": "arn:aws:states:x:execution:handoff:" + op}}
           for p in phases]
    out.append({"operation": None, "last": {"id": op, "result": result}, "owner": OWNER})
    return out


class ProgressOutputTests(unittest.TestCase):
    def run_deploy(self, responses, extra=()):
        clock = FakeClock()
        replies = iter([{"operationId": "op-1", "phase": "PUBLISH",
                         "execution": "arn:aws:states:x:execution:handoff:op-1"}] + responses)
        with patch("sys.argv", ["deploy.py", "deploy", "--tag", "abc123", "--wait", *extra]), \
                patch("deploy.configuration", return_value=CONFIG), patch("deploy.images", return_value={}), \
                patch("deploy.invoke", side_effect=lambda *_: next(replies)), \
                patch("deploy.time.monotonic", clock.monotonic), patch("deploy.time.sleep", clock.sleep), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            code = deploy.main()
        return code, out.getvalue()

    def test_successful_deploy_prints_one_line_per_phase_and_a_summary(self):
        code, out = self.run_deploy(statuses("PUBLISH", "PREPARE", "WAIT_STANDBY", "WAIT_STANDBY",
                                             "WAIT_STANDBY", "WAIT_STANDBY", "ROUTING"))
        self.assertEqual(code, 0)
        lines = out.splitlines()
        self.assertEqual(lines[0], "deploy op-1 started  (retry this same request with --operation-id op-1)")
        rows = [l for l in lines if __import__("re").match(r"  \d\d:\d\d:\d\d  \+", l)]
        phases = [l.split()[-1] for l in rows if "still" not in l]
        self.assertEqual(phases, ["PUBLISH", "PREPARE", "WAIT_STANDBY", "ROUTING"], "each phase once")
        self.assertEqual(sum("WAIT_STANDBY (still waiting, 1m00s)" in l for l in lines), 1,
                         "a phase that drags gets a note a minute in")
        self.assertIn("✅ SUCCEEDED in 2m20s", lines)
        self.assertIn("Owner     example-b  task 0f1e2d3c4b5a69788796a5b4c3d2e1f0  RUNNING  10.0.0.30  "
                      "example-coordinated:3", lines)
        self.assertIn("          relay  instance cccc3333dddd4444  restarts 0", lines)
        self.assertNotIn("{", out, "no JSON in the default output")
        self.assertNotIn("arn:aws", out)

    def test_rolled_back_deploy_is_marked_and_exits_nonzero(self):
        code, out = self.run_deploy(statuses("PREPARE", result="ROLLED_BACK"))
        self.assertEqual(code, 2)
        self.assertIn("❌ ROLLED_BACK in", out)
        self.assertIn("Inspect with `make ecs-status` before retrying.", out)

    def test_json_flag_keeps_the_raw_output(self):
        code, out = self.run_deploy(statuses("PREPARE"), extra=["--json"])
        self.assertEqual(code, 0)
        self.assertIn('"operationId": "op-1"', out)
        self.assertIn('{"operationId": "op-1", "phase": "PREPARE"', out)
        self.assertIn('"result": "SUCCEEDED"', out)
        self.assertNotIn("✅", out)

    def test_waiting_needs_an_operation_id(self):
        with patch("sys.argv", ["deploy.py", "deploy", "--tag", "abc123", "--wait"]), \
                patch("deploy.configuration", return_value=CONFIG), patch("deploy.images", return_value={}), \
                patch("deploy.invoke", return_value={}), self.assertRaises(RuntimeError):
            deploy.main()


TESTDATA = Path(__file__).resolve().parent / "testdata"
PST = __import__("datetime").timezone(__import__("datetime").timedelta(hours=-8), "PST")


class StatusFormatTests(unittest.TestCase):
    def test_completed_bootstrap_reads_as_a_summary(self):
        state = json.loads((TESTDATA / "status-bootstrap.json").read_text())
        out = deploy.format_status(state, now=1793853300, tz=PST)
        lines = out.splitlines()
        self.assertEqual(lines[0], "Owner     example-a  task 0f1e2d3c4b5a69788796a5b4c3d2e1f0  RUNNING  "
                                   "10.0.0.20  example-coordinated:2")
        self.assertIn("          grid   instance aaaa1111bbbb2222  restarts -", lines)
        self.assertIn("          relay  instance cccc3333dddd4444  restarts 0", lines)
        self.assertIn("Operation none in progress", lines)
        self.assertIn("Last      bootstrap  sierra-bootstrap-20261105T042314Z  SUCCEEDED", lines)
        self.assertIn("  finished  2026-11-04 20:29:35 PST (5m25s ago)  took 3m40s", lines)
        self.assertIn("  replaced  example:57 (legacy), 1 task(s)", lines)
        self.assertIn("            task 1a2b3c4d5e6f708192a3b4c5d6e7f809  stopped", lines)
        self.assertIn("    WAIT_OLD_STOPPED   +1m21s   1m53s", lines)
        self.assertIn("    WAIT_CLEANED       +3m38s   2s", lines, "the last phase runs to finishedAt")
        self.assertIn("Previous  none (nothing to roll back to)", lines)
        self.assertNotIn("execution", out, "an execution named like the operation is not repeated")
        self.assertNotIn("arn:aws", out, "ARNs are shortened")

    def test_operation_in_progress_shows_its_phase_and_images(self):
        state = {"owner": None, "last": None, "previous": {
                     "definition": "arn:aws:ecs:us-east-1:123456789012:task-definition/example-coordinated:3",
                     "images": {"grid": REGISTRY + "/grid@sha256:" + "a" * 64}},
                 "operation": {"id": "deploy-1", "kind": "deploy", "phase": "WAIT_STANDBY", "phaseAt": 1000,
                               "startedAt": 990, "rollback": True, "error": "standby not live yet",
                               "execution": "arn:aws:states:us-east-1:123456789012:execution:handoff:exec-9",
                               "images": {"grid": REGISTRY + "/grid@sha256:" + "b" * 64,
                                          "relay": REGISTRY + "/relay@sha256:" + "c" * 64},
                               "timeline": [{"at": 990, "phase": "PREPARE"}, {"at": 1000, "phase": "WAIT_STANDBY"}]}}
        out = deploy.format_status(state, now=1045, tz=PST).splitlines()
        self.assertEqual(out[0], "Owner     none")
        self.assertIn("Operation deploy  deploy-1  phase WAIT_STANDBY  (rollback)", out)
        self.assertIn("  in phase  45s", out)
        self.assertIn("  execution exec-9", out)
        self.assertIn("  image     grid   grid@sha256:" + "b" * 12, out)
        self.assertIn("  image     relay  relay@sha256:" + "c" * 12, out)
        self.assertIn("  error     standby not live yet", out)
        self.assertIn("    WAIT_STANDBY       +10s     45s", out, "the current phase runs to now")
        self.assertIn("Previous  example-coordinated:3", out)

    def test_empty_document_does_not_crash(self):
        out = deploy.format_status({}, now=0, tz=PST)
        self.assertIn("Owner     none", out)
        self.assertIn("Operation none in progress", out)

    def test_status_json_flag_prints_the_raw_document(self):
        state = json.loads((TESTDATA / "status-bootstrap.json").read_text())
        for argv, raw in ((["deploy.py", "status", "--json"], True), (["deploy.py", "status"], False)):
            with patch("sys.argv", argv), patch("deploy.invoke", return_value=state), \
                    contextlib.redirect_stdout(io.StringIO()) as out:
                self.assertEqual(deploy.main(), 0)
            if raw:
                self.assertEqual(json.loads(out.getvalue()), state)
            else:
                self.assertTrue(out.getvalue().startswith("Owner     example-a"))


if __name__ == "__main__":
    unittest.main()
