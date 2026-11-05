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
                patch("deploy.configuration", return_value=CONFIG), \
                patch("deploy.build_images", side_effect=subprocess.CalledProcessError(1, ["make"])), \
                patch("deploy.invoke") as invoke, patch("deploy.images") as resolve:
            with self.assertRaises(subprocess.CalledProcessError):
                deploy.main()
        invoke.assert_not_called()
        resolve.assert_not_called()

    def test_build_without_tag_uses_repository_commit(self):
        with patch("sys.argv", ["deploy.py", "deploy", "--build"]), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(stdout="0123456789ab\n")) as git, \
                patch("deploy.configuration", return_value=CONFIG), patch("deploy.build_images") as build, \
                patch("deploy.images", return_value={}), patch("deploy.invoke", return_value={}), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(), 0)
        self.assertEqual(build.call_args.args[0].tag, "0123456789ab")
        self.assertEqual(git.call_args.kwargs["cwd"], deploy.REPO)

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


if __name__ == "__main__":
    unittest.main()
