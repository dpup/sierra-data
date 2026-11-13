import contextlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import deploy

REPO = deploy.REPO
SHA = "0123456789abcdef0123456789abcdef01234567"
OLD_SHA = "fedcba9876543210fedcba9876543210fedcba98"
FLOOR = "3b084b21650d23ec6895623609d35d21bb8acb2c"
REGISTRY = "123456789012.dkr.ecr.us-west-2.amazonaws.com"
URI = REGISTRY + "/grid"
DIGEST = "sha256:" + "a" * 64
OLD_DIGEST = "sha256:" + "b" * 64
ENV = {"DEPLOY_ROLE_ARN": "arn:aws:iam::123456789012:role/sierra-data-deploy",
       "DEPLOY_AWS_REGION": "us-west-2", "DEPLOY_ECS_CLUSTER": "sierra",
       "DEPLOY_GRID_SERVICE": "grid", "DEPLOY_GRID_TASK_FAMILY": "sierra-grid",
       "DEPLOY_GRID_ECR_REPOSITORY": "grid", "DEPLOY_RELAY_SERVICE": "relay",
       "DEPLOY_RELAY_TASK_FAMILY": "sierra-relay", "DEPLOY_RELAY_ECR_REPOSITORY": "relay"}


def cfg():
    return deploy.Config(ENV)


def git_responses(**answers):
    def run(command, **_):
        key = " ".join(command[1:])
        if command[0] == "git" and command[1] == "fetch":
            return SimpleNamespace(returncode=0, stdout="", stderr="")
        if key in answers:
            value = answers[key]
            if value is None:
                return SimpleNamespace(returncode=1, stdout="", stderr="")
            return SimpleNamespace(returncode=0, stdout=value + "\n", stderr="")
        raise AssertionError("unexpected command: " + key)
    return run


class ConfigTests(unittest.TestCase):
    RELAY = ["DEPLOY_RELAY_SERVICE", "DEPLOY_RELAY_TASK_FAMILY", "DEPLOY_RELAY_ECR_REPOSITORY"]

    def test_every_unset_required_variable_is_named(self):
        problems = deploy.config_problems({})
        required = [n for n in deploy.SETTINGS if n not in self.RELAY]
        self.assertEqual(len(problems), len(required))
        for name in required:
            self.assertTrue(any(p.startswith(name + " is unset") for p in problems), name)

    def test_grid_variables_stay_required_without_the_relay(self):
        for name in ["DEPLOY_GRID_SERVICE", "DEPLOY_GRID_TASK_FAMILY", "DEPLOY_GRID_ECR_REPOSITORY"]:
            env = {k: v for k, v in ENV.items() if k != name and k not in self.RELAY}
            with self.subTest(name=name):
                self.assertEqual([p.split()[0] for p in deploy.config_problems(env)], [name])

    def test_relay_all_unset_is_skipped(self):
        env = {k: v for k, v in ENV.items() if k not in self.RELAY}
        self.assertEqual(deploy.config_problems(env), [])
        config = deploy.Config(env)
        self.assertEqual(list(config.targets), ["grid"])
        self.assertEqual(config.skipped, ["relay"])

    def test_relay_partly_set_names_the_missing_ones(self):
        env = {k: v for k, v in ENV.items() if k not in ("DEPLOY_RELAY_TASK_FAMILY", "DEPLOY_RELAY_ECR_REPOSITORY")}
        problems = deploy.config_problems(env)
        self.assertEqual([p.split()[0] for p in problems], ["DEPLOY_RELAY_TASK_FAMILY", "DEPLOY_RELAY_ECR_REPOSITORY"])
        self.assertIn("all or nothing", problems[0])
        self.assertIn("unset DEPLOY_RELAY_SERVICE", problems[0])
        with self.assertRaises(deploy.Refused):
            deploy.Config(env)

    def test_relay_all_set_deploys_both(self):
        config = deploy.Config(ENV)
        self.assertEqual(list(config.targets), ["grid", "relay"])
        self.assertEqual(config.skipped, [])

    def test_check_config_notices_a_skipped_relay(self):
        env = {**{k: v for k, v in ENV.items() if k not in self.RELAY}, **{k: "" for k in self.RELAY},
               "GITHUB_ACTIONS": "true"}
        with patch.dict(os.environ, env), contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(deploy.main(["check-config"]), 0)
        self.assertIn("::notice::relay: skipped, DEPLOY_RELAY_SERVICE, DEPLOY_RELAY_TASK_FAMILY, "
                      "DEPLOY_RELAY_ECR_REPOSITORY are all unset; deploying the Grid only", out.getvalue())

    def test_complete_configuration_passes(self):
        self.assertEqual(deploy.config_problems(ENV), [])
        self.assertEqual(cfg().targets["relay"], {"service": "relay", "family": "sierra-relay", "repository": "relay"})

    def test_malformed_values_are_refused(self):
        bad = {**ENV, "DEPLOY_ROLE_ARN": "sierra-data-deploy", "DEPLOY_GRID_ECR_REPOSITORY": "Grid;rm"}
        problems = deploy.config_problems(bad)
        self.assertEqual(len(problems), 2)
        with self.assertRaises(deploy.Refused):
            deploy.Config(bad)

    def test_check_config_command_exits_2_when_incomplete(self):
        with patch.dict(os.environ, {k: "" for k in ENV}), contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(deploy.main(["check-config"]), 2)
        self.assertIn("DEPLOY_ECS_CLUSTER is unset", err.getvalue())

    def test_release_refuses_before_any_aws_call_when_incomplete(self):
        with patch("deploy.aws") as aws:
            args = SimpleNamespace(sha=SHA, git_tag="", source=str(REPO), only=None, push_only=False,
                                   platform="linux/amd64", timeout=1)
            with self.assertRaises(deploy.Refused):
                deploy.release(args, environ={})
        aws.assert_not_called()

    def test_workflow_passes_exactly_the_settings(self):
        workflow = (REPO / ".github" / "workflows" / "deploy.yml").read_text()
        used = set(re.findall(r"vars\.(DEPLOY_[A-Z_]+)", workflow))
        self.assertEqual(used, set(deploy.SETTINGS))
        for name in deploy.SETTINGS:
            self.assertIn("%s: ${{ vars.%s }}" % (name, name), workflow, "passed through under its own name")


class BranchGuardTests(unittest.TestCase):
    def test_refuses_another_branch_before_touching_origin(self):
        with patch("deploy.subprocess.run", side_effect=git_responses(**{"rev-parse --abbrev-ref HEAD": "legacy/efs-sqlite"})) as git:
            drift = deploy.branch_drift()
        self.assertIn("on legacy/efs-sqlite", drift)
        self.assertIn("deploys only from main", drift)
        self.assertIn("make deploy REF=", drift, "points at the way to redeploy an older release")
        self.assertEqual(len(git.call_args_list), 1)

    def test_names_a_detached_head(self):
        with patch("deploy.subprocess.run", side_effect=git_responses(**{"rev-parse --abbrev-ref HEAD": "HEAD"})):
            self.assertIn("detached", deploy.branch_drift())

    def test_fetches_and_compares_with_origin(self):
        answers = {"rev-parse --abbrev-ref HEAD": "main", "rev-parse HEAD": "aaa", "rev-parse origin/main": "bbb",
                   "rev-list --count origin/main..HEAD": "2", "rev-list --count HEAD..origin/main": "0"}
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)) as git:
            drift = deploy.branch_drift()
        self.assertIn("2 commit(s) ahead of and 0 behind origin/main", drift)
        self.assertIn(["git", "fetch", "--quiet", "origin", "main"], [c.args[0] for c in git.call_args_list])
        self.assertTrue(all(c.kwargs["cwd"] == REPO for c in git.call_args_list))

    def test_reports_an_unreachable_origin(self):
        def run(command, **_):
            if command[1] == "fetch":
                return SimpleNamespace(returncode=128, stderr="fatal: couldn't find remote ref main")
            return SimpleNamespace(stdout="main\n")
        with patch("deploy.subprocess.run", side_effect=run):
            self.assertIn("cannot compare main with origin", deploy.branch_drift())

    def test_none_when_head_matches_origin(self):
        answers = {"rev-parse --abbrev-ref HEAD": "main", "rev-parse HEAD": "aaa", "rev-parse origin/main": "aaa"}
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)):
            self.assertIsNone(deploy.branch_drift())

    def test_dirty_files_lists_tracked_changes_only(self):
        porcelain = " M api/v1/common.pb.go\nM  Makefile\n"
        with patch("deploy.subprocess.run", return_value=SimpleNamespace(stdout=porcelain)) as git:
            self.assertEqual(deploy.dirty_files(), ["api/v1/common.pb.go", "Makefile"])
        self.assertIn("--untracked-files=no", git.call_args.args[0])


class ResolveTests(unittest.TestCase):
    def answers(self, **extra):
        base = {"rev-parse --verify --quiet origin/main^{commit}": SHA,
                "merge-base --is-ancestor %s origin/main" % SHA: "",
                "rev-parse --verify --quiet refs/tags/legacy-efs-final^{commit}": FLOOR,
                "merge-base --is-ancestor %s %s" % (SHA, FLOOR): None,
                "tag --points-at %s --sort=-v:refname" % SHA: ""}
        base.update(extra)
        return base

    def test_tip_of_main_with_its_tag(self):
        answers = self.answers(**{"tag --points-at %s --sort=-v:refname" % SHA: "v1.3.0\nv1.2.9"})
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)) as git:
            self.assertEqual(deploy.resolve(), (SHA, "v1.3.0"))
        self.assertIn(["git", "fetch", "--quiet", "--tags", "origin", "main"], [c.args[0] for c in git.call_args_list])

    def test_a_tag_resolves_to_its_commit(self):
        answers = self.answers(**{"rev-parse --verify --quiet refs/tags/v1.2.0^{commit}": SHA})
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)):
            self.assertEqual(deploy.resolve("v1.2.0", fetch=False), (SHA, "v1.2.0"))

    def test_a_commit_not_on_main_is_refused(self):
        answers = self.answers(**{"rev-parse --verify --quiet refs/tags/%s^{commit}" % OLD_SHA: None,
                                  "rev-parse --verify --quiet %s^{commit}" % OLD_SHA: OLD_SHA,
                                  "merge-base --is-ancestor %s origin/main" % OLD_SHA: None})
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)), \
                self.assertRaisesRegex(deploy.Refused, "not on origin/main"):
            deploy.resolve(OLD_SHA, fetch=False)

    def test_the_sqlite_stack_is_refused_although_main_reaches_it(self):
        for target in [FLOOR, OLD_SHA]:
            answers = self.answers(**{"rev-parse --verify --quiet refs/tags/%s^{commit}" % target: None,
                                      "rev-parse --verify --quiet %s^{commit}" % target: target,
                                      "merge-base --is-ancestor %s origin/main" % target: "",
                                      "merge-base --is-ancestor %s %s" % (target, FLOOR): ""})
            with self.subTest(target=target), patch("deploy.subprocess.run", side_effect=git_responses(**answers)), \
                    self.assertRaisesRegex(deploy.Refused, "legacy-efs-final"):
                deploy.resolve(target, fetch=False)

    def test_a_missing_floor_tag_is_refused(self):
        answers = self.answers(**{"rev-parse --verify --quiet refs/tags/legacy-efs-final^{commit}": None})
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)), \
                self.assertRaisesRegex(deploy.Refused, "missing"):
            deploy.resolve(fetch=False)

    def test_unknown_or_unsafe_refs_are_refused(self):
        for ref in ["$(shell bad)", "feature/x", "legacy/efs-sqlite"]:
            with self.subTest(ref=ref), patch("deploy.subprocess.run", side_effect=git_responses(**self.answers())), \
                    self.assertRaises(deploy.Refused):
                deploy.resolve(ref, fetch=False)

    def test_a_tag_shaped_like_an_image_key_is_refused(self):
        answers = self.answers(**{"rev-parse --verify --quiet refs/tags/content-abc^{commit}": SHA})
        with patch("deploy.subprocess.run", side_effect=git_responses(**answers)), \
                self.assertRaisesRegex(deploy.Refused, "collide"):
            deploy.resolve("content-abc", fetch=False)

    def test_resolve_command_prints_workflow_outputs(self):
        with patch("deploy.resolve", return_value=(SHA, "v1")), patch("deploy.ci_passed", return_value=True), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(deploy.main(["resolve", "--ref", "v1", "--no-fetch", "--require-ci"]), 0)
        self.assertEqual(out.getvalue(), "sha=%s\ngit_tag=v1\n" % SHA)

    def test_resolve_command_refuses_without_ci(self):
        with patch("deploy.resolve", return_value=(SHA, "")), patch("deploy.ci_passed", return_value=False), \
                contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(deploy.main(["resolve", "--require-ci"]), 2)
        self.assertEqual(out.getvalue(), "")


class DispatchTests(unittest.TestCase):
    def gh(self, url="https://github.com/sierragridteam/sierra-data/actions/runs/42\n", variables=tuple(ENV)):
        calls = []

        def fake(*command, capture=True):
            calls.append(list(command))
            if command[:2] == ("workflow", "run"):
                return SimpleNamespace(stdout=url)
            if command[:2] == ("variable", "list"):
                # Repository scope holds them all; the environment scope none.
                names = [] if "--env" in command else list(variables)
                return SimpleNamespace(stdout=json.dumps([{"name": n} for n in names]))
            return SimpleNamespace(stdout="[]")
        return calls, fake

    @staticmethod
    def workflow_run(calls):
        return next(c for c in calls if c[:2] == ["workflow", "run"])

    def dispatch_with_variables(self, variables):
        calls, fake = self.gh(variables=variables)
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.git", return_value=SHA), patch("deploy.ci_passed", return_value=True), \
                patch("deploy.gh", side_effect=fake), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=0)), \
                contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            code = deploy.main(["dispatch"])
        return code, out.getvalue(), err.getvalue(), calls

    def test_dispatch_with_every_variable_deploys_both_silently(self):
        code, out, _, calls = self.dispatch_with_variables(ENV)
        self.assertEqual(code, 0)
        self.assertNotIn("skipped", out)
        self.workflow_run(calls)

    def test_dispatch_without_relay_variables_says_the_relay_is_skipped(self):
        code, out, _, calls = self.dispatch_with_variables([n for n in ENV if not n.startswith("DEPLOY_RELAY_")])
        self.assertEqual(code, 0)
        self.assertIn("notice: relay: skipped", out)
        self.workflow_run(calls)

    def test_dispatch_with_some_relay_variables_is_refused(self):
        code, _, err, calls = self.dispatch_with_variables([n for n in ENV if n != "DEPLOY_RELAY_TASK_FAMILY"])
        self.assertEqual(code, 2)
        self.assertIn("DEPLOY_RELAY_TASK_FAMILY is unset", err)
        self.assertFalse([c for c in calls if c[:2] == ["workflow", "run"]], "nothing dispatched")

    def test_dispatch_goes_on_when_gh_cannot_read_variables(self):
        def fake(*command, capture=True):
            if command[:2] == ("variable", "list"):
                raise subprocess.CalledProcessError(1, ["gh"])
            return SimpleNamespace(stdout="https://github.com/x/y/actions/runs/7\n")
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.git", return_value=SHA), patch("deploy.ci_passed", return_value=True), \
                patch("deploy.gh", side_effect=fake), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=0)), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(deploy.main(["dispatch"]), 0)
        self.assertIn("the workflow checks them", out.getvalue())

    def test_head_dispatches_main_and_watches_the_run(self):
        calls, fake = self.gh()
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.git", return_value=SHA), patch("deploy.ci_passed", return_value=True) as ci, \
                patch("deploy.gh", side_effect=fake), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=0)) as watch, \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(["dispatch"]), 0)
        ci.assert_called_once_with(SHA)
        run = self.workflow_run(calls)
        self.assertEqual(run[:5], ["workflow", "run", "deploy.yml", "--ref", "main"])
        self.assertIn("ref=" + SHA, run)
        self.assertTrue(any(f.startswith("request_id=") for f in run))
        self.assertEqual(watch.call_args.args[0], ["gh", "run", "watch", "42", "--exit-status", "--interval", "10"])

    def test_watch_failure_is_the_exit_code(self):
        calls, fake = self.gh()
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.git", return_value=SHA), patch("deploy.ci_passed", return_value=True), \
                patch("deploy.gh", side_effect=fake), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=1)), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(["dispatch"]), 1)

    def test_wrong_branch_never_dispatches(self):
        with patch("deploy.branch_drift", return_value="HEAD is on legacy/efs-sqlite, but this deploys only from main"), \
                patch("deploy.dirty_files") as dirty, patch("deploy.gh") as gh, \
                contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(deploy.main(["dispatch"]), 2)
        self.assertIn("deploys only from main", err.getvalue())
        dirty.assert_not_called()
        gh.assert_not_called()

    def test_dirty_tree_never_dispatches(self):
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=["Makefile"]), \
                patch("deploy.gh") as gh, contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(deploy.main(["dispatch"]), 2)
        self.assertIn("Makefile", err.getvalue())
        self.assertIn("ALLOW_DIRTY", err.getvalue())
        gh.assert_not_called()

    def test_allow_dirty_is_an_explicit_override(self):
        calls, fake = self.gh()
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files") as dirty, \
                patch("deploy.git", return_value=SHA), patch("deploy.ci_passed", return_value=True), \
                patch("deploy.gh", side_effect=fake), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=0)), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(["dispatch", "--allow-dirty"]), 0)
        dirty.assert_not_called()

    def test_red_ci_never_dispatches(self):
        with patch("deploy.branch_drift", return_value=None), patch("deploy.dirty_files", return_value=[]), \
                patch("deploy.git", return_value=SHA), patch("deploy.ci_passed", return_value=False), \
                patch("deploy.gh") as gh, contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(deploy.main(["dispatch"]), 2)
        self.assertIn("CI has not passed", err.getvalue())
        gh.assert_not_called()

    def test_a_tag_skips_the_checkout_guards_but_not_resolution(self):
        calls, fake = self.gh()
        with patch("deploy.branch_drift") as drift, patch("deploy.dirty_files") as dirty, \
                patch("deploy.resolve", return_value=(OLD_SHA, "v1.2.0")) as resolve, \
                patch("deploy.ci_passed", return_value=True) as ci, patch("deploy.gh", side_effect=fake), \
                patch("deploy.subprocess.run", return_value=SimpleNamespace(returncode=0)), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(["dispatch", "--ref", "v1.2.0"]), 0)
        drift.assert_not_called()
        dirty.assert_not_called()
        resolve.assert_called_once_with("v1.2.0")
        ci.assert_called_once_with(OLD_SHA)
        self.assertIn("ref=v1.2.0", self.workflow_run(calls))

    def test_run_is_found_by_request_id_when_gh_prints_no_url(self):
        listing = json.dumps([{"databaseId": 7, "displayTitle": "Deploy main [other]"},
                              {"databaseId": 9, "displayTitle": "Deploy main [abc123]"}])
        with patch("deploy.gh", return_value=SimpleNamespace(stdout=listing)):
            self.assertEqual(deploy.find_run("abc123", timeout=5), "9")

    def test_ci_check_asks_for_a_successful_push_run_on_main(self):
        with patch("deploy.gh", return_value=SimpleNamespace(stdout='[{"databaseId": 1}]')) as gh:
            self.assertTrue(deploy.ci_passed(SHA))
        command = gh.call_args.args
        for flag, value in [("--workflow", "ci.yml"), ("--commit", SHA), ("--branch", "main"),
                            ("--event", "push"), ("--status", "success")]:
            self.assertEqual(command[command.index(flag) + 1], value)


class LegacyTargetTests(unittest.TestCase):
    def test_legacy_targets_are_gone(self):
        makefile = (REPO / "Makefile").read_text()
        for target in ["ecs-deploy", "ecs-status", "ecs-rollback"]:
            self.assertNotIn(target + ":", makefile, "the retired stack's targets stay out of main's Makefile")

    def test_no_coordinator_client_remains(self):
        source = Path(deploy.__file__).read_text()
        for legacy in ["lambda", "coordinator\"", "SIERRA_DEPLOY_FUNCTION", "sierra-data-coordinator"]:
            self.assertNotIn(legacy, source)


class ContentKeyTests(unittest.TestCase):
    def fake_go(self, payloads):
        def run(command, cwd, env, check):
            out = Path(command[command.index("-o") + 1])
            out.write_bytes(payloads.get((command[-1], env["GOARCH"]), b"binary " + command[-1].encode()))
            self.assertEqual(env["CGO_ENABLED"], "0")
            self.assertEqual(env["GOFLAGS"], "-buildvcs=false", "a VCS stamp would change the key every commit")
            self.assertIn("-trimpath", command)
            return SimpleNamespace(returncode=0)
        return run

    def test_key_is_stable_and_follows_binaries_and_architecture(self):
        same = deploy.content_key("relay", "amd64", REPO, run=self.fake_go({}))
        self.assertEqual(same, deploy.content_key("relay", "amd64", REPO, run=self.fake_go({})))
        self.assertNotEqual(same, deploy.content_key("relay", "arm64", REPO, run=self.fake_go({})))
        changed = deploy.content_key("relay", "amd64", REPO, run=self.fake_go({("./cmd/mesh-relay", "amd64"): b"new"}))
        self.assertNotEqual(same, changed)
        self.assertRegex(same, r"^[0-9a-f]{32}$")

    def test_grid_and_relay_keys_differ(self):
        self.assertNotEqual(deploy.content_key("grid", "amd64", REPO, run=self.fake_go({})),
                            deploy.content_key("relay", "amd64", REPO, run=self.fake_go({})))

    def test_targets_cover_what_the_dockerfile_copies(self):
        # Each final stage copies binaries from its builder and files from the
        # build context (/app/...). The key must hash all of them, or a change
        # to one would ship nothing.
        dockerfile = (REPO / "Dockerfile").read_text()
        stages = re.findall(r"^FROM \S+ AS (\S+)", dockerfile, re.M)
        self.assertIn("grid", stages)
        self.assertIn("relay", stages)
        builders = {"go-builder": "grid", "relay-builder": "relay"}
        for builder, source in re.findall(r"^COPY --from=(\S+) (\S+) ", dockerfile, re.M):
            target = deploy.TARGETS[builders[builder]]
            if source.startswith("/app/"):
                path = source[len("/app/"):]
                self.assertIn(path, target["files"], "%s copies %s" % (builders[builder], path))
            else:
                self.assertIn(source.lstrip("/"), [b for _, b in target["binaries"]])
        for target in deploy.TARGETS.values():
            self.assertIn("Dockerfile", target["files"])


class TaskDefinitionTests(unittest.TestCase):
    DEFINITION = {
        "taskDefinitionArn": "arn:aws:ecs:us-west-2:123456789012:task-definition/sierra-grid:7",
        "family": "sierra-grid", "revision": 7, "status": "ACTIVE", "registeredAt": 1, "registeredBy": "x",
        "compatibilities": ["EC2", "FARGATE"], "requiresAttributes": [{"name": "x"}],
        "requiresCompatibilities": ["FARGATE"], "cpu": "512", "memory": "1024", "networkMode": "awsvpc",
        "taskRoleArn": "arn:aws:iam::123456789012:role/grid-task", "executionRoleArn": "arn:aws:iam::123456789012:role/grid-exec",
        "runtimePlatform": {"cpuArchitecture": "X86_64", "operatingSystemFamily": "LINUX"},
        "containerDefinitions": [
            {"name": "grid", "image": URI + "@" + OLD_DIGEST,
             "environment": [{"name": "PF__GRID__WRITER__ENABLED", "value": "false"}],
             "secrets": [{"name": "PF__GRID__DATABASE__URL", "valueFrom": "arn:aws:secretsmanager:..."}]},
            {"name": "log-router", "image": "public.ecr.aws/aws-observability/aws-for-fluent-bit:3"}]}

    def test_only_the_image_changes(self):
        request = deploy.with_image(self.DEFINITION, [{"key": "app", "value": "grid"}], 0, URI + "@" + DIGEST)
        for field in deploy.READ_ONLY:
            self.assertNotIn(field, request)
        self.assertEqual(request["containerDefinitions"][0]["image"], URI + "@" + DIGEST)
        self.assertEqual(self.DEFINITION["containerDefinitions"][0]["image"], URI + "@" + OLD_DIGEST, "input untouched")
        expected = {k: v for k, v in self.DEFINITION.items() if k not in deploy.READ_ONLY}
        expected["containerDefinitions"] = [dict(self.DEFINITION["containerDefinitions"][0], image=URI + "@" + DIGEST),
                                            self.DEFINITION["containerDefinitions"][1]]
        expected["tags"] = [{"key": "app", "value": "grid"}]
        self.assertEqual(request, expected)

    def test_untagged_definition_sends_no_tags(self):
        self.assertNotIn("tags", deploy.with_image(self.DEFINITION, [], 0, "x"))

    def test_container_is_found_by_repository_then_by_name(self):
        self.assertEqual(deploy.container_index(self.DEFINITION, URI, "grid"), 0)
        placeholder = dict(self.DEFINITION, containerDefinitions=[
            {"name": "sidecar", "image": "busybox"}, {"name": "grid", "image": "public.ecr.aws/nginx/nginx:bootstrap"}])
        self.assertEqual(deploy.container_index(placeholder, URI, "grid"), 1)
        ambiguous = dict(self.DEFINITION, containerDefinitions=[{"name": "a", "image": URI + ":x"},
                                                                {"name": "b", "image": URI + ":y"}])
        with self.assertRaises(RuntimeError):
            deploy.container_index(ambiguous, URI, "grid")
        with self.assertRaises(RuntimeError):
            deploy.container_index(dict(self.DEFINITION, containerDefinitions=[{"name": "x", "image": "y"}]), URI, "grid")

    def test_family_of_an_arn(self):
        self.assertEqual(deploy.family_of(self.DEFINITION["taskDefinitionArn"]), "sierra-grid")


class FakeAws:
    """A scripted AWS: ECR images by tag, ECS service and task definitions,
    and a service deployment that walks through the given statuses."""

    def __init__(self, test, images=None, service_td=7, latest_td=7, running_image=None, statuses=("SUCCESSFUL",),
                 busy=False):
        self.test, self.calls = test, []
        self.images = dict(images or {})  # tag -> digest
        self.service_td, self.latest_td, self.busy = service_td, latest_td, busy
        self.running_image = running_image or URI + "@" + OLD_DIGEST
        self.statuses = list(statuses)
        self.registered = None

    def td(self, revision, image):
        d = json.loads(json.dumps(TaskDefinitionTests.DEFINITION))
        d["taskDefinitionArn"] = d["taskDefinitionArn"].rsplit(":", 1)[0] + ":%d" % revision
        d["revision"] = revision
        d["containerDefinitions"][0]["image"] = image
        return d

    def __call__(self, region, *command):
        self.test.assertEqual(region, "us-west-2")
        self.calls.append(command[:2])
        op = command[:2]
        arg = lambda flag: command[command.index(flag) + 1]
        if op == ("ecr", "describe-repositories"):
            return {"repositories": [{"repositoryUri": REGISTRY + "/" + arg("--repository-names")}]}
        if op == ("ecr", "describe-images"):
            kind, value = arg("--image-ids").split("=", 1)
            if kind == "imageTag":
                if value not in self.images:
                    raise RuntimeError("An error occurred (ImageNotFoundException)")
                return {"imageDetails": [{"imageDigest": self.images[value]}]}
            tags = [t for t, d in self.images.items() if d == value]
            return {"imageDetails": [{"imageDigest": value, "imageTags": tags}]}
        if op == ("ecr", "batch-get-image"):
            return {"images": [{"imageManifest": "{}", "imageManifestMediaType": "application/vnd.oci.image.manifest.v1+json"}]}
        if op == ("ecr", "put-image"):
            self.images[arg("--image-tag")] = arg("--image-digest")
            return {}
        if op == ("ecs", "describe-services"):
            return {"services": [{"status": "ACTIVE", "taskDefinition": self.td(self.service_td, "")["taskDefinitionArn"]}]}
        if op == ("ecs", "list-service-deployments"):
            if "--status" in command:
                return {"serviceDeployments": [{"status": "IN_PROGRESS", "serviceDeploymentArn": "busy"}] if self.busy else []}
            return {"serviceDeployments": [{"serviceDeploymentArn": "arn:sd/1", "targetServiceRevisionArn": "arn:rev/1"}]}
        if op == ("ecs", "describe-task-definition"):
            ref = arg("--task-definition")
            revision = self.latest_td if ref == "sierra-grid" else int(ref.rsplit(":", 1)[1])
            image = self.running_image if revision == self.service_td else URI + ":placeholder"
            return {"taskDefinition": self.td(revision, image), "tags": []}
        if op == ("ecs", "register-task-definition"):
            self.registered = json.loads(Path(arg("--cli-input-json")[len("file://"):]).read_text())
            return {"taskDefinition": {"taskDefinitionArn": self.td(self.latest_td + 1, "")["taskDefinitionArn"]}}
        if op == ("ecs", "update-service"):
            self.updated_to = arg("--task-definition")
            return {"service": {"deployments": [{"id": "ecs-svc/123", "status": "PRIMARY"}]}}
        if op == ("ecs", "describe-service-revisions"):
            return {"serviceRevisions": [{"taskDefinition": self.updated_to}]}
        if op == ("ecs", "describe-service-deployments"):
            status = self.statuses.pop(0) if len(self.statuses) > 1 else self.statuses[0]
            return {"serviceDeployments": [{"status": status, "lifecycleStage": "BAKE_TIME"}]}
        raise AssertionError("unexpected aws call %s" % (command,))


class ReleaseTests(unittest.TestCase):
    def run_release(self, fake, key="k" * 32, build=None):
        log = []
        with patch("deploy.aws", side_effect=fake), patch("deploy.content_key", return_value=key), \
                patch("deploy.docker_login"), patch("deploy.time.sleep"), \
                patch("deploy.host.machine", return_value="x86_64"), \
                patch("deploy.subprocess.run", side_effect=build or self.fail_build):
            ok, lines = deploy.release_one(cfg(), "grid", SHA, "v1.4.0", REPO, timeout=600, log=log.append)
        return ok, lines, log

    def fail_build(self, *a, **k):
        raise AssertionError("must not build: %s" % (a,))

    def test_unchanged_image_and_revision_is_not_redeployed(self):
        fake = FakeAws(self, images={"content-" + "k" * 32: OLD_DIGEST, OLD_SHA: OLD_DIGEST})
        ok, lines, _ = self.run_release(fake)
        self.assertTrue(ok)
        self.assertIn("unchanged", lines[0])
        self.assertNotIn(("ecs", "register-task-definition"), fake.calls)
        self.assertNotIn(("ecs", "update-service"), fake.calls)
        self.assertEqual(fake.images[SHA], OLD_DIGEST, "the commit still gets its tag on the reused image")
        self.assertEqual(fake.images["v1.4.0"], OLD_DIGEST)

    def test_new_image_registers_from_latest_and_follows_to_success(self):
        built = []

        def build(command, **kwargs):
            built.append(command)
            return SimpleNamespace(returncode=0)

        fake = FakeAws(self, images={}, statuses=("IN_PROGRESS", "SUCCESSFUL"))
        original_put = fake.__call__

        # docker push is simulated by the first describe of the content tag after the pushes.
        def aws(region, *command):
            if command[:2] == ("ecr", "describe-images") and any(c[1] == "push" for c in built) \
                    and "imageTag=content-" + "k" * 32 in command:
                fake.images.setdefault("content-" + "k" * 32, DIGEST)
            return original_put(region, *command)

        ok, lines, log = self.run_release(aws, build=build)
        self.assertTrue(ok, lines)
        docker_build = built[0]
        self.assertEqual(docker_build[:6], ["docker", "build", "--platform", "linux/amd64", "--target", "grid"])
        pushes = [c[2] for c in built if c[1] == "push"]
        self.assertEqual(pushes, [URI + ":" + SHA, URI + ":v1.4.0", URI + ":content-" + "k" * 32],
                         "content tag last: it marks a complete push")
        registered = fake.registered
        self.assertEqual(registered["containerDefinitions"][0]["image"], URI + "@" + DIGEST)
        self.assertEqual(registered["containerDefinitions"][0]["environment"],
                         TaskDefinitionTests.DEFINITION["containerDefinitions"][0]["environment"])
        self.assertEqual(fake.updated_to.rsplit(":", 1)[1], "8")
        self.assertTrue(any("stop-service-deployment" in l and "arn:sd/1" in l for l in log))
        self.assertTrue(any("update-service" in l and "sierra-grid:7" in l for l in lines),
                        "the rollback names the revision that ran before")

    def test_terraform_revision_is_deployed_even_when_the_image_is_the_same(self):
        # Terraform registered :8 (say the writer was enabled); the service still runs :7.
        fake = FakeAws(self, images={"content-" + "k" * 32: OLD_DIGEST}, service_td=7, latest_td=8)
        ok, lines, _ = self.run_release(fake)
        self.assertTrue(ok)
        self.assertIn(("ecs", "register-task-definition"), fake.calls)
        self.assertEqual(fake.registered["containerDefinitions"][0]["image"], URI + "@" + OLD_DIGEST)
        self.assertEqual(fake.updated_to.rsplit(":", 1)[1], "9")

    def test_rolled_back_deployment_fails(self):
        fake = FakeAws(self, images={"content-" + "k" * 32: DIGEST}, statuses=("IN_PROGRESS", "ROLLBACK_SUCCESSFUL"))
        ok, lines, _ = self.run_release(fake)
        self.assertFalse(ok)
        self.assertIn("NOT deployed", lines[0])
        self.assertIn("describe-service-deployments", lines[1])

    def test_timeout_fails_but_names_the_running_deployment(self):
        fake = FakeAws(self, images={"content-" + "k" * 32: DIGEST}, statuses=("IN_PROGRESS",))
        clock = iter(range(0, 100000, 10))
        with patch("deploy.time.monotonic", side_effect=lambda: next(clock)):
            ok, lines, log = self.run_release(fake)
        self.assertFalse(ok)
        self.assertTrue(any("still running" in l for l in log))

    def test_deployment_in_progress_is_refused_before_building(self):
        fake = FakeAws(self, images={}, busy=True)
        with self.assertRaisesRegex(RuntimeError, "already has a deployment"):
            self.run_release(fake)
        self.assertNotIn(("ecs", "update-service"), fake.calls)

    def test_service_on_another_family_is_refused(self):
        fake = FakeAws(self)
        with patch("deploy.aws", side_effect=fake), patch.dict(ENV, {"DEPLOY_GRID_TASK_FAMILY": "other"}), \
                self.assertRaisesRegex(RuntimeError, "not a revision of other"):
            deploy.release_one(cfg(), "grid", SHA, "", REPO, timeout=1, log=lambda *_: None)

    def test_tag_conflict_is_an_error(self):
        fake = FakeAws(self, images={"v1.4.0": OLD_DIGEST})
        with patch("deploy.aws", side_effect=fake), self.assertRaisesRegex(RuntimeError, "already names another image"):
            deploy.add_tag(cfg(), "grid", DIGEST, "v1.4.0")

    def test_release_without_relay_deploys_the_grid_only(self):
        args = SimpleNamespace(sha=SHA, git_tag="", source=str(REPO), only=None, push_only=False,
                               platform="linux/amd64", timeout=1)
        env = {k: v for k, v in ENV.items() if not k.startswith("DEPLOY_RELAY_")}
        with patch("deploy.release_one", return_value=(True, ["grid: deployed"])) as one, \
                contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(deploy.release(args, environ=env), 0)
        self.assertEqual([c.args[1] for c in one.call_args_list], ["grid"])
        self.assertIn("notice: relay: skipped", out.getvalue())
        with patch("deploy.release_one") as one, self.assertRaisesRegex(deploy.Refused, "relay variables are unset"):
            deploy.release(SimpleNamespace(**{**vars(args), "only": "relay"}), environ=env)
        one.assert_not_called()

    def test_release_with_some_relay_variables_is_refused(self):
        args = SimpleNamespace(sha=SHA, git_tag="", source=str(REPO), only=None, push_only=False,
                               platform="linux/amd64", timeout=1)
        env = {k: v for k, v in ENV.items() if k != "DEPLOY_RELAY_SERVICE"}
        with patch("deploy.release_one") as one, self.assertRaisesRegex(deploy.Refused, "DEPLOY_RELAY_SERVICE"):
            deploy.release(args, environ=env)
        one.assert_not_called()

    def test_failed_service_stops_the_release(self):
        args = SimpleNamespace(sha=SHA, git_tag="", source=str(REPO), only=None, push_only=False,
                               platform="linux/amd64", timeout=1)
        with patch("deploy.release_one", return_value=(False, ["grid: NOT deployed"])) as one, \
                patch.dict(os.environ, {"GITHUB_STEP_SUMMARY": ""}), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.release(args, environ=ENV), 1)
        self.assertEqual([c.args[1] for c in one.call_args_list], ["grid"], "relay is not touched after grid failed")

    def test_release_refuses_a_short_sha_or_unusable_tag(self):
        for sha, tag in [("abc123", ""), (SHA, "content-x"), (SHA, "a/b")]:
            args = SimpleNamespace(sha=sha, git_tag=tag, source=str(REPO), only=None, push_only=False,
                                   platform="linux/amd64", timeout=1)
            with self.subTest(sha=sha, tag=tag), patch("deploy.release_one") as one, self.assertRaises(deploy.Refused):
                deploy.release(args, environ=ENV)
            one.assert_not_called()

    def test_push_only_never_touches_ecs(self):
        fake = FakeAws(self, images={"content-" + "k" * 32: DIGEST})
        with patch("deploy.aws", side_effect=fake), patch("deploy.content_key", return_value="k" * 32), \
                patch("deploy.docker_login"):
            ok, lines = deploy.release_one(cfg(), "grid", SHA, "", REPO, timeout=1, log=lambda *_: None,
                                           push_only_arch="amd64")
        self.assertTrue(ok)
        self.assertFalse([c for c in fake.calls if c[0] == "ecs"])

    def test_native_runner_is_required_to_build(self):
        fake = FakeAws(self, images={})
        with patch("deploy.aws", side_effect=fake), patch("deploy.host.machine", return_value="x86_64"), \
                self.assertRaisesRegex(RuntimeError, "native runner"):
            deploy.ensure_image(cfg(), "grid", URI, "k" * 32, SHA, "", "arm64", REPO, log=lambda *_: None)


if __name__ == "__main__":
    unittest.main()
