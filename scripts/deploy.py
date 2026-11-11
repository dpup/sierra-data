#!/usr/bin/env python3
"""Build and deploy Sierra through its AWS coordinator using AWS credentials.

This client is self-contained: Python 3 and AWS CLI v2 are its only client
dependencies. --build uses this repository's existing Make/Docker targets.
It never reads the control bearer or directly updates ECS services. The private
coordinator handles database ownership and resolves deployment progress.
"""

import argparse
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import uuid


def aws(args, *command):
    profile = ["--profile", args.profile] if args.profile else []
    result = subprocess.run(["aws", *profile, "--region", args.region,
                             *command, "--output", "json"], capture_output=True, text=True,
                            env={**os.environ, "AWS_RETRY_MODE": "standard", "AWS_MAX_ATTEMPTS": "8"})
    if result.returncode:
        raise RuntimeError(result.stderr.strip())
    return json.loads(result.stdout) if result.stdout.strip() else {}


def invoke(args, payload):
    with tempfile.TemporaryDirectory(prefix="sierra-deploy-") as directory:
        source, dest = Path(directory) / "request.json", Path(directory) / "response.json"
        source.write_text(json.dumps(payload))
        result = aws(args, "lambda", "invoke", "--function-name", args.function,
                     "--cli-binary-format", "raw-in-base64-out", "--payload", "fileb://" + str(source), str(dest))
        response = json.loads(dest.read_text())
        if result.get("FunctionError"):
            raise RuntimeError(response.get("errorMessage", "coordinator request failed"))
        return response


REPO = Path(__file__).resolve().parents[1]


def configuration(args):
    result = aws(args, "lambda", "get-function-configuration", "--function-name", args.function)
    config = json.loads(result["Environment"]["Variables"]["CONFIG"])
    if set(config["repositories"]) != {"grid", "relay"}:
        raise RuntimeError("coordinator must configure both Grid and relay repositories")
    return config


LEGACY_BRANCH = "legacy/efs-sqlite"


def dirty_files():
    """Tracked files that differ from HEAD. A --build deploy tags its images
    with the commit, so anything listed here would ship labelled as code it is
    not, and a later rollback to that tag would bring it back."""
    result = subprocess.run(["git", "status", "--porcelain", "--untracked-files=no"],
                            cwd=REPO, capture_output=True, text=True, check=True)
    return [line[3:] for line in result.stdout.splitlines() if line.strip()]


def git(*command):
    return subprocess.run(["git", *command], cwd=REPO, capture_output=True, text=True,
                          check=True).stdout.strip()


def branch_drift(branch):
    """Why HEAD must not be built for the legacy stack, or None.

    The SQLite-on-EFS stack is frozen on one branch; main is the Postgres
    stack and cannot run on this infrastructure, so a --build deploy ships
    only that branch. It also ships only a commit origin already has: the
    images are tagged with the commit, and a rollback, a cherry-pick to main
    or the next hotfix must be able to find exactly what is running."""
    current = git("rev-parse", "--abbrev-ref", "HEAD")
    if current != branch:
        where = "detached" if current == "HEAD" else "on " + current
        return ("HEAD is %s, but the legacy stack deploys only from %s\n"
                "(main is the Postgres stack; hotfix on %s, then git switch to it)"
                % (where, branch, branch))
    fetched = subprocess.run(["git", "fetch", "--quiet", "origin", branch], cwd=REPO,
                             capture_output=True, text=True)
    if fetched.returncode:
        return "cannot compare %s with origin: %s" % (branch, fetched.stderr.strip() or "fetch failed")
    head, remote = git("rev-parse", "HEAD"), git("rev-parse", "origin/" + branch)
    if head != remote:
        ahead = git("rev-list", "--count", "origin/%s..HEAD" % branch)
        behind = git("rev-list", "--count", "HEAD..origin/%s" % branch)
        return ("%s is %s commit(s) ahead of and %s behind origin/%s\n"
                "(push or pull until they match; what runs must be on origin)"
                % (branch, ahead, behind, branch))
    return None


def build_images(args, config):
    destinations = []
    for name, target, variable in [("grid", "ecr-push-grid", "DOCKER_IMAGE_NAME"),
                                   ("relay", "ecr-push-relay", "RELAY_IMAGE_NAME")]:
        repository = config["repositories"][name]
        if not re.fullmatch(r"[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com(?:\.cn)?/[a-z0-9][a-z0-9._/-]*", repository):
            raise RuntimeError("unsupported ECR repository URL for " + name)
        registry, image = repository.split("/", 1)
        destinations.append(["make", target, "DOCKER_REGISTRY=" + registry,
                             variable + "=" + image, "DOCKER_TAG=" + args.tag])
    env = {**os.environ, "AWS_REGION": args.region, "AWS_DEFAULT_REGION": args.region}
    if args.profile:
        env["AWS_PROFILE"] = args.profile
        env["AWS_DEFAULT_PROFILE"] = args.profile
        # Build helpers run AWS CLI themselves. Match an explicit --profile
        # rather than letting inherited temporary credentials override it.
        for key in ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
                    "AWS_SECURITY_TOKEN", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
                    "AWS_ROLE_SESSION_NAME"]:
            env.pop(key, None)
    for command in destinations:
        subprocess.run(command, cwd=REPO, env=env, check=True)


def images(args, config):
    repositories = config["repositories"]
    out = {}
    for name, repository in repositories.items():
        image = aws(args, "ecr", "describe-images", "--repository-name", repository.split("/", 1)[1],
                    "--image-ids", "imageTag=" + args.tag)["imageDetails"][0]
        out[name] = repository + "@" + image["imageDigest"]
    return out


def wait(args, operation_id, first_phase=None, started=None):
    """Follow operation_id to completion, printing one line per phase change.

    Returns 0 on SUCCEEDED and 2 on any other result (a deployment that was
    rolled back is not a success). Raises when the ownership lock is BLOCKED,
    another operation becomes current, or the CLI's own wait times out (the
    server-side workflow carries on regardless)."""
    raw = getattr(args, "json", False)
    clock = time.monotonic
    started = clock() if started is None else started
    deadline = clock() + args.timeout
    previous, phase_since, last_note = first_phase, clock(), clock()
    while clock() < deadline:
        state = invoke(args, {"command": "status"})
        op = state.get("operation")
        if not op or (state.get("last") or {}).get("id") == operation_id:
            last = state.get("last") or {}
            if last.get("id") != operation_id:
                raise RuntimeError("operation no longer current; inspect durable coordinator status")
            result = last.get("result")
            if raw:
                print(json.dumps({"result": result, "operationId": operation_id,
                                  "owner": state.get("owner")}, indent=2))
            else:
                mark = "✅" if result == "SUCCEEDED" else "❌"
                print("%s %s in %s" % (mark, result, duration(clock() - started)))
                owner = state.get("owner")
                print("Owner     " + task_line(owner))
                for line in process_lines((owner or {}).get("processes"), "          "):
                    print(line)
                if result != "SUCCEEDED":
                    print("Inspect with `make ecs-status` before retrying.")
            return 0 if result == "SUCCEEDED" else 2
        if op.get("id") != operation_id:
            raise RuntimeError("another operation is current; refusing to follow it")
        phase = op.get("phase")
        if phase != previous:
            if raw:
                print(json.dumps({"operationId": operation_id, "phase": phase,
                                  "execution": op.get("execution")}), flush=True)
            else:
                progress(started, phase)
            previous, phase_since, last_note = phase, clock(), clock()
        elif not raw and clock() - last_note >= 60:
            progress(started, "%s (still waiting, %s)" % (phase, duration(clock() - phase_since)))
            last_note = clock()
        if phase == "BLOCKED":
            raise RuntimeError("ownership lock retained: " + op.get("error", "unknown outcome"))
        time.sleep(3)
    raise RuntimeError("CLI wait timed out; workflow and ownership lock remain intact")


def progress(started, text):
    print("  %s  +%-7s %s" % (time.strftime("%H:%M:%S"), duration(time.monotonic() - started), text),
          flush=True)


# --- status rendering ---------------------------------------------------------
#
# The coordinator's status document is the source of truth and its shape is
# its own; this only summarises it, and tolerates missing or unknown fields
# (anything it cannot place is simply not shown; --json has everything).


def short_arn(arn):
    """The readable part of an ARN: a task id, a family:revision, an execution name."""
    if not arn:
        return "-"
    tail = arn.rsplit(":", 1)[-1] if ":task-definition/" not in arn else arn.split(":task-definition/", 1)[1]
    return tail.rsplit("/", 1)[-1]


def short_image(image):
    """repo@sha256:abcdef... -> repo@sha256:abcdef123456"""
    if not image:
        return "-"
    name, _, digest = image.partition("@")
    name = name.rsplit("/", 1)[-1]
    if digest.startswith("sha256:"):
        digest = digest[:7 + 12]
    return name + ("@" + digest if digest else "")


def when(ts, now, tz):
    if not ts:
        return "-"
    at = datetime.datetime.fromtimestamp(ts, tz)
    return at.strftime("%Y-%m-%d %H:%M:%S %Z") + " (" + duration(now - ts) + " ago)"


def duration(seconds):
    seconds = int(max(seconds, 0))
    hours, rest = divmod(seconds, 3600)
    minutes, secs = divmod(rest, 60)
    if hours:
        return "%dh%02dm" % (hours, minutes)
    if minutes:
        return "%dm%02ds" % (minutes, secs)
    return "%ds" % secs


def task_line(task):
    if not task:
        return "none"
    parts = [task.get("slot") or "-", "task " + short_arn(task.get("arn")), task.get("state") or "-"]
    if task.get("ip"):
        parts.append(task["ip"])
    if task.get("definition"):
        parts.append(short_arn(task["definition"]))
    return "  ".join(parts)


def process_lines(processes, indent):
    lines = []
    for name in sorted(processes or {}):
        p = processes[name] or {}
        restarts = p.get("restartCount")
        lines.append("%s%-6s instance %s  restarts %s" % (
            indent, name, p.get("instanceId") or "-", "-" if restarts is None else restarts))
    return lines


def operation_lines(op, now, tz, current):
    lines = []
    head = [op.get("kind") or "operation", op.get("id") or "-"]
    if current:
        head.append("phase " + (op.get("phase") or "-"))
    else:
        head.append(op.get("result") or op.get("phase") or "-")
    if op.get("rollback"):
        head.append("(rollback)")
    if op.get("run") and op["run"] != 1:
        head.append("run %s" % op["run"])
    lines.append("  ".join(head))
    if op.get("slot"):
        lines.append("  slot      " + op["slot"])
    started, finished = op.get("startedAt"), op.get("finishedAt")
    if started:
        lines.append("  started   " + when(started, now, tz))
    if finished:
        lines.append("  finished  " + when(finished, now, tz) +
                     ("  took " + duration(finished - started) if started else ""))
    elif current and op.get("phaseAt"):
        lines.append("  in phase  " + duration(now - op["phaseAt"]))
    if op.get("execution") and short_arn(op["execution"]) != op.get("id"):
        lines.append("  execution " + short_arn(op["execution"]))
    for name, image in sorted((op.get("images") or {}).items()):
        lines.append("  image     %-6s %s" % (name, short_image(image)))
    old = op.get("old") or {}
    if old:
        tasks = old.get("tasks") or []
        stopped = set((op.get("releaseEvidence") or {}).get("stoppedTasks") or [])
        desc = short_arn(old.get("definition")) + (" (legacy)" if old.get("legacy") else "")
        lines.append("  replaced  %s, %d task(s)" % (desc, len(tasks)))
        for t in tasks:
            mark = "stopped" if t.get("arn") in stopped else t.get("state") or "-"
            lines.append("            task %s  %s" % (short_arn(t.get("arn")), mark))
    if op.get("error"):
        lines.append("  error     " + str(op["error"]))
    timeline = op.get("timeline") or []
    if timeline:
        lines.append("  timeline (phase, offset from start, time in phase)")
        end = finished or now
        for i, step in enumerate(timeline):
            nxt = timeline[i + 1]["at"] if i + 1 < len(timeline) else end
            offset = step["at"] - (started or timeline[0]["at"])
            lines.append("    %-18s +%-7s %s" % (step.get("phase") or "-", duration(offset), duration(nxt - step["at"])))
    return lines


def format_status(state, now=None, tz=None):
    """A human summary of the coordinator's status document."""
    now = time.time() if now is None else now
    tz = tz or datetime.datetime.now().astimezone().tzinfo
    owner = state.get("owner")
    lines = ["Owner     " + task_line(owner)]
    if owner:
        lines += process_lines(owner.get("processes"), "          ")
    op = state.get("operation")
    lines.append("")
    if op:
        lines.append("Operation " + operation_lines(op, now, tz, True)[0])
        lines += operation_lines(op, now, tz, True)[1:]
    else:
        lines.append("Operation none in progress")
    last = state.get("last")
    if last:
        lines.append("")
        body = operation_lines(last, now, tz, False)
        lines.append("Last      " + body[0])
        lines += body[1:]
    previous = state.get("previous")
    lines.append("")
    if previous:
        desc = short_arn(previous.get("definition")) if isinstance(previous, dict) else str(previous)
        lines.append("Previous  " + desc)
        for name, image in sorted(((previous.get("images") or {}) if isinstance(previous, dict) else {}).items()):
            lines.append("  image     %-6s %s" % (name, short_image(image)))
    else:
        lines.append("Previous  none (nothing to roll back to)")
    if "legacyRetired" in state:
        lines.append("Legacy    " + ("retired" if state["legacyRetired"] else "still running"))
    if state.get("version") is not None:
        lines.append("Version   %s" % state["version"])
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["status", "bootstrap", "deploy", "rollback", "resume", "abort"])
    parser.add_argument("--profile", help="named AWS profile; otherwise use the normal credential chain, including AWS_PROFILE")
    parser.add_argument("--region", default=os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION") or "us-east-1")
    parser.add_argument("--function", default=os.environ.get("SIERRA_DEPLOY_FUNCTION", "sierra-data-coordinator"))
    parser.add_argument("--tag", help="resolve this tag in BOTH configured repositories; deploy uses their digests")
    parser.add_argument("--operation-id", default=None, help="reuse on retries of the same start request")
    parser.add_argument("--confirm-legacy-stop", help="bootstrap acknowledgement: exact legacy service name")
    parser.add_argument("--wait", action="store_true")
    parser.add_argument("--build", action="store_true", help="build and push both images from this repository before deploying")
    parser.add_argument("--allow-dirty", action="store_true",
                        help="with --build, deploy even though tracked files differ from the commit the images are tagged with")
    parser.add_argument("--branch", default=os.environ.get("SIERRA_DEPLOY_BRANCH", LEGACY_BRANCH),
                        help="with --build, the only branch that may be built, and HEAD must match it on origin (default %(default)s)")
    parser.add_argument("--timeout", type=int, default=1800)
    parser.add_argument("--json", action="store_true", help="print raw coordinator responses instead of a summary")
    args = parser.parse_args()
    if args.build and args.action != "deploy":
        parser.error("--build is supported only for deploy")
    if args.build:
        drift = branch_drift(args.branch)
        if drift:
            parser.error(drift)
    if args.build and not args.allow_dirty:
        changed = dirty_files()
        if changed:
            parser.error("the working tree has uncommitted changes, which would ship under this commit's tag:\n  "
                         + "\n  ".join(changed)
                         + "\ncommit or discard them, or pass --allow-dirty (make ecs-deploy ALLOW_DIRTY=1)")
    if args.action == "deploy" and not args.tag:
        if not args.build:
            parser.error("deploy requires --tag for both images, or --build to use the current commit")
        args.tag = subprocess.run(["git", "rev-parse", "--short=12", "HEAD"], cwd=REPO,
                                  capture_output=True, text=True, check=True).stdout.strip()
    if args.tag:
        if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", args.tag):
            parser.error("--tag must be a valid Docker/ECR tag")
    if args.action == "status":
        state = invoke(args, {"command": "status"})
        print(json.dumps(state, indent=2) if args.json else format_status(state))
        return 0
    if args.action in {"resume", "abort"}:
        payload = {"command": args.action}
    else:
        payload = {"command": "start", "action": args.action,
                   "operationId": args.operation_id or uuid.uuid4().hex}
        if args.confirm_legacy_stop:
            payload["confirmLegacyStop"] = args.confirm_legacy_stop
        if args.tag:
            config = configuration(args)
            if args.build:
                build_images(args, config)
            payload["images"] = images(args, config)
    started = time.monotonic()
    result = invoke(args, payload)
    operation_id = result.get("operationId")
    if args.wait and not operation_id:
        raise RuntimeError("the coordinator did not return an operation id to follow")
    if args.json:
        print(json.dumps(result, indent=2), flush=True)
    else:
        print("%s %s started  (retry this same request with --operation-id %s)"
              % (payload.get("action") or args.action, operation_id, operation_id), flush=True)
        if result.get("phase"):
            progress(started, result["phase"])
    if not args.wait:
        return 0
    return wait(args, operation_id, first_phase=result.get("phase"), started=started)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, KeyError, ValueError, OSError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
