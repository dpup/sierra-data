#!/usr/bin/env python3
"""Build and deploy Sierra through its AWS coordinator using AWS credentials.

This client is self-contained: Python 3 and AWS CLI v2 are its only client
dependencies. --build uses this repository's existing Make/Docker targets.
It never reads the control bearer or directly updates ECS services. The private
coordinator handles database ownership and resolves deployment progress.
"""

import argparse
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


def wait(args, operation_id):
    deadline = time.monotonic() + args.timeout
    previous = None
    while time.monotonic() < deadline:
        state = invoke(args, {"command": "status"})
        op = state.get("operation")
        if not op or (state.get("last") or {}).get("id") == operation_id:
            last = state.get("last") or {}
            if last.get("id") != operation_id:
                raise RuntimeError("operation no longer current; inspect durable coordinator status")
            print(json.dumps({"result": last["result"], "operationId": operation_id,
                              "owner": state.get("owner")}, indent=2))
            return 0 if last["result"] == "SUCCEEDED" else 2
        if op["id"] != operation_id:
            raise RuntimeError("another operation is current; refusing to follow it")
        phase = op["phase"]
        if phase != previous:
            print(json.dumps({"operationId": operation_id, "phase": phase,
                              "execution": op["execution"]}), flush=True)
            previous = phase
        if phase == "BLOCKED":
            raise RuntimeError("ownership lock retained: " + op.get("error", "unknown outcome"))
        time.sleep(3)
    raise RuntimeError("CLI wait timed out; workflow and ownership lock remain intact")


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
    parser.add_argument("--timeout", type=int, default=1800)
    args = parser.parse_args()
    if args.build and args.action != "deploy":
        parser.error("--build is supported only for deploy")
    if args.action == "deploy" and not args.tag:
        if not args.build:
            parser.error("deploy requires --tag for both images, or --build to use the current commit")
        args.tag = subprocess.run(["git", "rev-parse", "--short=12", "HEAD"], cwd=REPO,
                                  capture_output=True, text=True, check=True).stdout.strip()
    if args.tag:
        if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", args.tag):
            parser.error("--tag must be a valid Docker/ECR tag")
    if args.action == "status":
        print(json.dumps(invoke(args, {"command": "status"}), indent=2))
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
    result = invoke(args, payload)
    print(json.dumps(result, indent=2), flush=True)
    return wait(args, result["operationId"]) if args.wait else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, KeyError, ValueError, OSError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
