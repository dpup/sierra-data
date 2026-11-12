#!/usr/bin/env python3
"""Deploy sierra-data main to the Grid account (ECS Fargate, native blue/green).

Python 3 standard library only, in two halves:

  dispatch      `make deploy [REF=<tag or commit>]` on a laptop. Checks that
                what is asked for is on origin's main and passed CI, runs the
                Deploy workflow (.github/workflows/deploy.yml) with
                `gh workflow run`, then follows it with `gh run watch`. Needs
                gh, never AWS credentials.
  release       run BY that workflow, with the deploy role's OIDC credentials.
                For each image (grid, relay): reuse an identical image already
                in ECR or build and push it, register a task-definition
                revision from the family's latest with only that container's
                image changed, UpdateService, and follow the ECS service
                deployment (blue/green bake, automatic rollback) to its end.

Also `resolve` (the workflow's check of what it is asked to deploy) and
`check-config` (refuses when a required repository variable is unset).

Terraform (sierra-infra, grid/compute) owns the services, the task
definitions' content and the deployment configuration; this only swaps
images. The legacy stack (SQLite on EFS, the coordinator Lambda,
`make ecs-deploy`) deploys only from a checkout of legacy/efs-sqlite: nothing
here can reach the old account. docs/deploy-handoff.md has the whole flow.
"""

import argparse
import datetime
import glob
import hashlib
import json
import os
from pathlib import Path
import platform as host
import re
import subprocess
import sys
import tempfile
import time
import uuid

REPO = Path(__file__).resolve().parents[1]
MAIN = "main"
WORKFLOW = "deploy.yml"
CI_WORKFLOW = "ci.yml"
# Where main and legacy/efs-sqlite parted: this commit and everything before
# it are the SQLite stack, which cannot run in the Grid account although it
# is reachable from main.
FLOOR_TAG = "legacy-efs-final"

# Repository (or `production` environment) variables. The workflow passes
# them as environment variables of the same name; check-config refuses to
# go on while any is unset or malformed. The relay's three are OPTIONAL as a
# group (the Grid service exists in the new account before the relay does):
# all unset skips the relay with a notice, some set is refused, all set
# deploys it.
SETTINGS = {
    "DEPLOY_ROLE_ARN": (r"arn:aws:iam::[0-9]{12}:role/[\w+=,.@/-]+",
                        "role the workflow assumes through GitHub OIDC (sierra-infra grid/deploy)"),
    "DEPLOY_AWS_REGION": (r"[a-z]{2}(-[a-z]+)+-[0-9]", "the Grid account's region, us-west-2"),
    "DEPLOY_ECS_CLUSTER": (r"[A-Za-z0-9_-]{1,255}", "ECS cluster holding both services"),
    "DEPLOY_GRID_SERVICE": (r"[A-Za-z0-9_-]{1,255}", "the Grid ECS service"),
    "DEPLOY_GRID_TASK_FAMILY": (r"[A-Za-z0-9_-]{1,255}", "the Grid task-definition family"),
    "DEPLOY_GRID_ECR_REPOSITORY": (r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*",
                                   "ECR repository name for the grid image"),
    "DEPLOY_RELAY_SERVICE": (r"[A-Za-z0-9_-]{1,255}", "the mesh relay ECS service"),
    "DEPLOY_RELAY_TASK_FAMILY": (r"[A-Za-z0-9_-]{1,255}", "the relay task-definition family"),
    "DEPLOY_RELAY_ECR_REPOSITORY": (r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*",
                                    "ECR repository name for the relay image"),
}

# What goes into each image (Dockerfile stages `grid` and `relay`). The
# content key hashes these binaries, built reproducibly, plus these files:
# an image whose key is already in ECR is reused, not rebuilt, and a service
# whose image did not change is not redeployed. test_deploy.py holds this to
# the Dockerfile's COPY lines.
TARGETS = {
    "grid": {"stage": "grid", "binaries": [("./cmd/server", "sierra-server"), ("./cmd/gridctl", "gridctl")],
             "files": ["Dockerfile", "prefab.yaml", "api/v1/*.swagger.json"]},
    "relay": {"stage": "relay", "binaries": [("./cmd/mesh-relay", "mesh-relay")],
              "files": ["Dockerfile", "prefab.yaml"]},
}

OPTIONAL_GROUPS = {"relay": ["DEPLOY_RELAY_SERVICE", "DEPLOY_RELAY_TASK_FAMILY", "DEPLOY_RELAY_ECR_REPOSITORY"]}

SHA_RE = re.compile(r"[0-9a-f]{7,40}")
FULL_SHA_RE = re.compile(r"[0-9a-f]{40}")
# A git tag that is also a valid ECR image tag.
TAG_RE = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}")
CONTENT_PREFIX = "content-"

ARCHITECTURES = {"X86_64": "amd64", "ARM64": "arm64"}
HOST_ARCHITECTURES = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

# Read-only fields of a described task definition that RegisterTaskDefinition
# refuses. Everything else is carried over unchanged (a denylist, so a field
# AWS adds later fails loudly here rather than being dropped silently).
READ_ONLY = {"taskDefinitionArn", "revision", "status", "requiresAttributes", "compatibilities",
             "registeredAt", "registeredBy", "deregisteredAt", "deleteRequestedAt"}

ACTIVE_DEPLOYMENT = ["PENDING", "IN_PROGRESS", "STOP_REQUESTED", "ROLLBACK_REQUESTED", "ROLLBACK_IN_PROGRESS"]
FAILED_DEPLOYMENT = {"STOPPED", "ROLLBACK_SUCCESSFUL", "ROLLBACK_FAILED"}


class Refused(Exception):
    """A guard said no: print the reason, exit 2."""


# --- git and gh (dispatch, resolve) -------------------------------------------


def git(*command, cwd=REPO):
    return subprocess.run(["git", *command], cwd=cwd, capture_output=True, text=True,
                          check=True).stdout.strip()


def dirty_files():
    """Tracked files that differ from HEAD. The workflow builds origin's
    commit, not this tree, so these would not ship; refusing keeps "what I
    tested is what deploys" true."""
    result = subprocess.run(["git", "status", "--porcelain", "--untracked-files=no"],
                            cwd=REPO, capture_output=True, text=True, check=True)
    return [line[3:] for line in result.stdout.splitlines() if line.strip()]


def branch_drift(branch=MAIN):
    """Why HEAD must not be deployed, or None.

    main deploys to the Grid account; the frozen SQLite stack deploys only
    from legacy/efs-sqlite with its own `make ecs-deploy`. HEAD must also be
    exactly origin's branch: images are tagged with the commit, and a
    rollback or a forward-port must be able to find what is running."""
    current = git("rev-parse", "--abbrev-ref", "HEAD")
    if current != branch:
        where = "detached" if current == "HEAD" else "on " + current
        return ("HEAD is %s, but this deploys only from %s\n"
                "(the legacy stack deploys from a legacy/efs-sqlite checkout with make ecs-deploy;"
                " to redeploy an older release use make deploy REF=<tag>)" % (where, branch))
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


def resolve(ref=None, fetch=True, cwd=REPO):
    """(commit, git tag or "") for a tag, a commit, or the tip of origin/main.

    Refuses anything not reachable from origin/main: only main deploys to
    the Grid account."""
    if fetch:
        fetched = subprocess.run(["git", "fetch", "--quiet", "--tags", "origin", MAIN], cwd=cwd,
                                 capture_output=True, text=True)
        if fetched.returncode:
            raise Refused("cannot fetch origin: " + (fetched.stderr.strip() or "fetch failed"))

    def commit(name):
        result = subprocess.run(["git", "rev-parse", "--verify", "--quiet", name + "^{commit}"], cwd=cwd,
                                capture_output=True, text=True)
        return result.stdout.strip() if result.returncode == 0 else None

    tag = ""
    if not ref:
        sha = commit("origin/" + MAIN)
    elif TAG_RE.fullmatch(ref) and commit("refs/tags/" + ref):
        sha, tag = commit("refs/tags/" + ref), ref
    elif SHA_RE.fullmatch(ref):
        sha = commit(ref)
    else:
        raise Refused("%r is not a tag or commit this checkout knows (tags must also be valid image tags)" % ref)
    if not sha:
        raise Refused("%r is not a commit this checkout knows" % (ref or "origin/" + MAIN))
    on_main = subprocess.run(["git", "merge-base", "--is-ancestor", sha, "origin/" + MAIN], cwd=cwd,
                             capture_output=True, text=True)
    if on_main.returncode:
        raise Refused("%s is not on origin/%s; only main deploys to the Grid account" % (ref or sha, MAIN))
    floor = commit("refs/tags/" + FLOOR_TAG)
    if not floor:
        raise Refused("tag %s is missing from this checkout (git fetch --tags origin); it marks the oldest"
                      " commit main may not deploy" % FLOOR_TAG)
    pre_split = subprocess.run(["git", "merge-base", "--is-ancestor", sha, floor], cwd=cwd,
                               capture_output=True, text=True)
    if pre_split.returncode == 0:
        raise Refused("%s is at or before %s: the SQLite stack, deployed only from legacy/efs-sqlite"
                      % (ref or sha, FLOOR_TAG))
    if not tag:
        tags = git("tag", "--points-at", sha, "--sort=-v:refname", cwd=cwd).split()
        tag = next((t for t in tags if TAG_RE.fullmatch(t) and not t.startswith(CONTENT_PREFIX)), "")
    if tag.startswith(CONTENT_PREFIX) or FULL_SHA_RE.fullmatch(tag):
        raise Refused("git tag %r would collide with the image tags the deploy uses" % tag)
    return sha, tag


def gh(*command, capture=True):
    return subprocess.run(["gh", *command], cwd=REPO, capture_output=capture, text=True, check=True)


def ci_passed(sha):
    """Whether CI succeeded for this commit on a push to main."""
    out = gh("run", "list", "--workflow", CI_WORKFLOW, "--commit", sha, "--branch", MAIN,
             "--event", "push", "--status", "success", "--limit", "1", "--json", "databaseId").stdout
    return bool(json.loads(out or "[]"))


def find_run(request_id, timeout=60):
    """The id of the dispatched run whose title carries request_id."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        out = gh("run", "list", "--workflow", WORKFLOW, "--event", "workflow_dispatch", "--limit", "20",
                 "--json", "databaseId,displayTitle").stdout
        for run in json.loads(out or "[]"):
            if "[%s]" % request_id in (run.get("displayTitle") or ""):
                return str(run["databaseId"])
        time.sleep(3)
    raise RuntimeError("dispatched, but no Deploy run titled [%s] appeared; see gh run list --workflow %s"
                       % (request_id, WORKFLOW))


def variable_names():
    """Names of the repository and `production` environment variables, or
    None when gh cannot read them (that needs more than read access; the
    workflow checks them regardless)."""
    names = set()
    for scope in ([], ["--env", "production"]):
        try:
            out = gh("variable", "list", *scope, "--json", "name").stdout
        except (subprocess.CalledProcessError, OSError):
            return None
        names |= {v["name"] for v in json.loads(out or "[]")}
    return names


def check_variables():
    """The workflow's configuration check, by name only, before dispatching."""
    names = variable_names()
    if names is None:
        print("notice: cannot read the repository variables with gh; the workflow checks them", flush=True)
        return
    present = {n: "set" for n in names}
    problems = config_problems(present, check_format=False)
    if problems:
        raise Refused("deploy configuration incomplete; set these repository variables:\n  " + "\n  ".join(problems))
    for target in skipped_targets(present):
        notice(skip_notice(target), {})


def dispatch(args):
    if args.ref:
        # An existing release: the checkout's branch and tree do not matter,
        # only that the tag or commit is on origin/main.
        sha, tag = resolve(args.ref)
        what = args.ref
    else:
        drift = branch_drift(MAIN)
        if drift:
            raise Refused(drift)
        if not args.allow_dirty:
            changed = dirty_files()
            if changed:
                raise Refused("the working tree has uncommitted changes; the workflow deploys origin's commit,"
                              " not these:\n  " + "\n  ".join(changed)
                              + "\ncommit or discard them, or pass --allow-dirty (make deploy ALLOW_DIRTY=1)")
        sha, tag = git("rev-parse", "HEAD"), ""
        what = sha
    if not ci_passed(sha):
        raise Refused("CI has not passed for %s on a push to %s; deploy only what verify passed" % (sha[:12], MAIN))
    check_variables()
    request_id = uuid.uuid4().hex[:12]
    fields = ["-f", "ref=" + what, "-f", "request_id=" + request_id]
    if args.push_only:
        fields += ["-f", "push_only=true"]
    out = gh("workflow", "run", WORKFLOW, "--ref", MAIN, *fields).stdout
    match = re.search(r"/actions/runs/([0-9]+)", out or "")
    run_id = match.group(1) if match else find_run(request_id)
    print("Deploy %s (%s%s) dispatched: run %s" % (what, sha[:12], ", tag " + tag if tag else "", run_id), flush=True)
    print("The deploy job waits for a reviewer in the `production` environment; approve it on the run page.",
          flush=True)
    return subprocess.run(["gh", "run", "watch", run_id, "--exit-status", "--interval", "10"], cwd=REPO).returncode


# --- configuration -------------------------------------------------------------


def skipped_targets(environ):
    """The optional images whose variables are all unset."""
    return [target for target, names in OPTIONAL_GROUPS.items() if not any(environ.get(n) for n in names)]


def skip_notice(target):
    return ("%s: skipped, %s are all unset; deploying the Grid only"
            % (target, ", ".join(OPTIONAL_GROUPS[target])))


def config_problems(environ, check_format=True):
    """Every unset or malformed variable. An optional group that is entirely
    unset is no problem (it is skipped); a partly set one names what is
    missing."""
    skipped = {n for target in skipped_targets(environ) for n in OPTIONAL_GROUPS[target]}
    problems = []
    for name, (pattern, meaning) in SETTINGS.items():
        if name in skipped:
            continue
        value = environ.get(name, "")
        group = next((t for t, names in OPTIONAL_GROUPS.items() if name in names), None)
        if not value:
            if group:
                problems.append("%s is unset (%s); the %s variables are all or nothing: set it, or unset %s"
                                % (name, meaning, group,
                                   " and ".join(n for n in OPTIONAL_GROUPS[group] if environ.get(n))))
            else:
                problems.append("%s is unset (%s)" % (name, meaning))
        elif check_format and not re.fullmatch(pattern, value):
            problems.append("%s=%r is malformed (%s)" % (name, value, meaning))
    return problems


def notice(text, environ=os.environ):
    """A line in the log, and an annotation on the run when in Actions."""
    print(("::notice::" if environ.get("GITHUB_ACTIONS") == "true" else "notice: ") + text, flush=True)


class Config:
    def __init__(self, environ):
        problems = config_problems(environ)
        if problems:
            raise Refused("deploy configuration incomplete; set these repository variables:\n  "
                          + "\n  ".join(problems))
        self.region = environ["DEPLOY_AWS_REGION"]
        self.cluster = environ["DEPLOY_ECS_CLUSTER"]
        self.skipped = skipped_targets(environ)
        self.targets = {name: {"service": environ["DEPLOY_%s_SERVICE" % name.upper()],
                               "family": environ["DEPLOY_%s_TASK_FAMILY" % name.upper()],
                               "repository": environ["DEPLOY_%s_ECR_REPOSITORY" % name.upper()]}
                        for name in TARGETS if name not in self.skipped}


# --- AWS -----------------------------------------------------------------------


def aws(region, *command):
    result = subprocess.run(["aws", "--region", region, *command, "--output", "json"], capture_output=True,
                            text=True, env={**os.environ, "AWS_RETRY_MODE": "standard", "AWS_MAX_ATTEMPTS": "8"})
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or "aws %s failed" % " ".join(command[:2]))
    return json.loads(result.stdout) if result.stdout.strip() else {}


def ecr_image(cfg, repository, tag=None, digest=None):
    """{imageDigest, imageTags} for a tag or digest, or None."""
    image_id = "imageTag=" + tag if tag else "imageDigest=" + digest
    try:
        found = aws(cfg.region, "ecr", "describe-images", "--repository-name", repository, "--image-ids", image_id)
    except RuntimeError as error:
        if "ImageNotFoundException" in str(error):
            return None
        raise
    details = found.get("imageDetails") or []
    return details[0] if details else None


def add_tag(cfg, repository, digest, tag):
    """Point a new tag at an image already in ECR (no rebuild, no pull)."""
    existing = ecr_image(cfg, repository, tag=tag)
    if existing:
        if existing["imageDigest"] != digest:
            raise RuntimeError("%s:%s already names another image (%s); ECR tags are immutable"
                               % (repository, tag, existing["imageDigest"]))
        return False
    got = aws(cfg.region, "ecr", "batch-get-image", "--repository-name", repository,
              "--image-ids", "imageDigest=" + digest, "--accepted-media-types",
              "application/vnd.docker.distribution.manifest.v2+json",
              "application/vnd.docker.distribution.manifest.list.v2+json",
              "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json")
    image = got["images"][0]
    with tempfile.TemporaryDirectory(prefix="sierra-deploy-") as directory:
        manifest = Path(directory) / "manifest.json"
        manifest.write_text(image["imageManifest"])
        command = ["ecr", "put-image", "--repository-name", repository, "--image-tag", tag,
                   "--image-digest", digest, "--image-manifest", "file://" + str(manifest)]
        if image.get("imageManifestMediaType"):
            command += ["--image-manifest-media-type", image["imageManifestMediaType"]]
        aws(cfg.region, *command)
    return True


def content_key(name, arch, source, run=subprocess.run):
    """A fingerprint of what goes into the image: its binaries built
    reproducibly (-trimpath, no VCS stamp, no build id, so a commit that
    changes nothing they link produces the same bytes) plus the files the
    Dockerfile copies in, and the Dockerfile itself (base image pins)."""
    spec = TARGETS[name]
    digest = hashlib.sha256(("%s linux/%s\n" % (name, arch)).encode())
    env = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch, "GOTOOLCHAIN": "local",
           "GOFLAGS": "-buildvcs=false"}
    with tempfile.TemporaryDirectory(prefix="sierra-key-") as directory:
        for package, binary in spec["binaries"]:
            out = Path(directory) / binary
            run(["go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", str(out), package],
                cwd=source, env=env, check=True)
            digest.update(("bin %s\n" % binary).encode())
            digest.update(hashlib.sha256(out.read_bytes()).digest())
    for pattern in spec["files"]:
        matches = sorted(glob.glob(str(Path(source) / pattern)))
        if not matches:
            raise RuntimeError("%s image: %s matches no file" % (name, pattern))
        for path in matches:
            digest.update(("file %s\n" % Path(path).relative_to(source)).encode())
            digest.update(hashlib.sha256(Path(path).read_bytes()).digest())
    return digest.hexdigest()[:32]


def docker_login(cfg, registry):
    password = subprocess.run(["aws", "--region", cfg.region, "ecr", "get-login-password"],
                              capture_output=True, text=True, check=True).stdout
    subprocess.run(["docker", "login", "--username", "AWS", "--password-stdin", registry],
                   input=password, capture_output=True, text=True, check=True)


def ensure_image(cfg, name, uri, key, sha, git_tag, arch, source, log=print):
    """The digest of this commit's image in ECR, built only when no image
    with the same content key (or this commit's tag) is there yet. Tags it
    with the commit, the git tag if any, and the content key."""
    repository = cfg.targets[name]["repository"]
    content_tag = CONTENT_PREFIX + key
    found = ecr_image(cfg, repository, tag=content_tag) or ecr_image(cfg, repository, tag=sha)
    if found:
        digest = found["imageDigest"]
        log("%s: reusing %s (content %s unchanged)" % (name, short_digest(digest), key[:12]))
    else:
        if HOST_ARCHITECTURES.get(host.machine().lower()) != arch:
            raise RuntimeError("%s runs on linux/%s but this runner is %s: the Dockerfile's runtime stages"
                               " need a native runner (see docs/deploy-handoff.md)" % (name, arch, host.machine()))
        tags = [sha] + ([git_tag] if git_tag else [])
        log("%s: building %s (linux/%s) as %s" % (name, TARGETS[name]["stage"], arch, ", ".join(tags)))
        build = ["docker", "build", "--platform", "linux/" + arch, "--target", TARGETS[name]["stage"],
                 "--build-arg", "GRIDCTL_VERSION=" + sha,
                 "--label", "org.opencontainers.image.revision=" + sha,
                 "--label", "org.opencontainers.image.source=https://github.com/sierragridteam/sierra-data",
                 "--label", "org.sierragridteam.content-key=" + key]
        for tag in tags + [content_tag]:
            build += ["-t", "%s:%s" % (uri, tag)]
        subprocess.run(build + ["."], cwd=source, check=True)
        # The content tag goes last: it marks a complete push, so a run that
        # died half way rebuilds (or reuses the commit tag) next time.
        for tag in tags + [content_tag]:
            subprocess.run(["docker", "push", "%s:%s" % (uri, tag)], cwd=source, check=True)
        digest = ecr_image(cfg, repository, tag=content_tag)["imageDigest"]
    for tag in [sha, git_tag, content_tag]:
        if tag and add_tag(cfg, repository, digest, tag):
            log("%s: tagged %s as %s" % (name, short_digest(digest), tag))
    return digest


def task_definition(cfg, ref):
    found = aws(cfg.region, "ecs", "describe-task-definition", "--task-definition", ref, "--include", "TAGS")
    return found["taskDefinition"], found.get("tags") or []


def family_of(arn):
    return arn.split(":task-definition/", 1)[-1].rsplit(":", 1)[0]


def container_index(definition, uri, name):
    """The container that runs this image: the one whose image is in our
    repository, else the one named after the image (grid, relay)."""
    containers = definition["containerDefinitions"]
    ours = [i for i, c in enumerate(containers) if re.match(re.escape(uri) + r"[:@]", c.get("image", ""))]
    if not ours:
        ours = [i for i, c in enumerate(containers) if c.get("name") == name]
    if len(ours) != 1:
        raise RuntimeError("%s: expected one container using %s (or named %s) in %s, found %d"
                           % (name, uri, name, definition.get("taskDefinitionArn"), len(ours)))
    return ours[0]


def with_image(definition, tags, index, image):
    """A RegisterTaskDefinition request: the described revision with only
    one container's image replaced."""
    request = {k: v for k, v in definition.items() if k not in READ_ONLY}
    request["containerDefinitions"] = [dict(c) for c in definition["containerDefinitions"]]
    request["containerDefinitions"][index]["image"] = image
    if tags:
        request["tags"] = tags
    return request


def register(cfg, request):
    with tempfile.TemporaryDirectory(prefix="sierra-deploy-") as directory:
        path = Path(directory) / "task-definition.json"
        path.write_text(json.dumps(request))
        return aws(cfg.region, "ecs", "register-task-definition",
                   "--cli-input-json", "file://" + str(path))["taskDefinition"]["taskDefinitionArn"]


def image_digest(cfg, repository, uri, image):
    """The digest an image reference in our repository points at, or None."""
    if image.startswith(uri + "@"):
        return image.split("@", 1)[1]
    if image.startswith(uri + ":"):
        found = ecr_image(cfg, repository, tag=image.split(":")[-1])
        return found["imageDigest"] if found else None
    return None


def commit_tag(cfg, repository, digest):
    found = ecr_image(cfg, repository, digest=digest) if digest else None
    return next((t for t in (found or {}).get("imageTags") or [] if FULL_SHA_RE.fullmatch(t)), None)


def short_digest(digest):
    return digest[:7 + 12] if digest and digest.startswith("sha256:") else (digest or "-")


def short_arn(arn):
    return (arn or "-").rsplit("/", 1)[-1]


def describe_service(cfg, service):
    found = aws(cfg.region, "ecs", "describe-services", "--cluster", cfg.cluster, "--services", service)
    services = found.get("services") or []
    if not services or services[0].get("status") != "ACTIVE":
        raise RuntimeError("service %s is not ACTIVE in cluster %s" % (service, cfg.cluster))
    return services[0]


def deployments(cfg, service, statuses=None, after=None):
    command = ["ecs", "list-service-deployments", "--cluster", cfg.cluster, "--service", service]
    if statuses:
        command += ["--status", *statuses]
    if after:
        command += ["--created-at", json.dumps({"after": after})]
    return aws(cfg.region, *command).get("serviceDeployments") or []


def describe_deployment(cfg, arn):
    found = aws(cfg.region, "ecs", "describe-service-deployments", "--service-deployment-arns", arn)
    return (found.get("serviceDeployments") or [{}])[0]


def find_deployment(cfg, service, definition_arn, after, timeout=90):
    """The service deployment that rolls out definition_arn."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for brief in deployments(cfg, service, after=after):
            revision = brief.get("targetServiceRevisionArn")
            if not revision:
                continue
            found = aws(cfg.region, "ecs", "describe-service-revisions", "--service-revision-arns", revision)
            if any(r.get("taskDefinition") == definition_arn for r in found.get("serviceRevisions") or []):
                return brief["serviceDeploymentArn"]
        time.sleep(5)
    raise RuntimeError("UpdateService accepted %s but no service deployment for it appeared" % definition_arn)


def follow(cfg, arn, timeout, log=print):
    """Follow a service deployment to its end; True only on SUCCESSFUL."""
    started = time.monotonic()
    deadline = started + timeout
    seen = None
    while time.monotonic() < deadline:
        deployment = describe_deployment(cfg, arn)
        status, stage = deployment.get("status"), deployment.get("lifecycleStage")
        if (status, stage) != seen:
            log("  +%-6s %s%s%s" % (duration(time.monotonic() - started), status or "-",
                                    " " + stage if stage else "",
                                    " (%s)" % deployment["statusReason"] if deployment.get("statusReason") else ""))
            seen = (status, stage)
        if status == "SUCCESSFUL":
            return True
        if status in FAILED_DEPLOYMENT:
            return False
        time.sleep(10)
    log("  still running after %s; ECS carries on without this workflow" % duration(timeout))
    return False


def duration(seconds):
    minutes, secs = divmod(int(max(seconds, 0)), 60)
    return "%dm%02ds" % (minutes, secs) if minutes else "%ds" % secs


def release_one(cfg, name, sha, git_tag, source, timeout, log=print, push_only_arch=None):
    """Deploy one image. Returns (ok, summary lines)."""
    target = cfg.targets[name]
    repository = target["repository"]
    uri = aws(cfg.region, "ecr", "describe-repositories", "--repository-names",
              repository)["repositories"][0]["repositoryUri"]
    if push_only_arch:
        docker_login(cfg, uri.split("/", 1)[0])
        key = content_key(name, push_only_arch, source)
        digest = ensure_image(cfg, name, uri, key, sha, git_tag, push_only_arch, source, log)
        return True, ["%s: pushed %s@%s (push only; ECS untouched)" % (name, uri, digest)]

    service = describe_service(cfg, target["service"])
    current_arn = service["taskDefinition"]
    if family_of(current_arn) != target["family"]:
        raise RuntimeError("%s runs %s, not a revision of %s; check DEPLOY_%s_TASK_FAMILY"
                           % (target["service"], current_arn, target["family"], name.upper()))
    busy = deployments(cfg, target["service"], statuses=ACTIVE_DEPLOYMENT)
    if busy:
        raise RuntimeError("%s already has a deployment %s (%s); let it finish or stop it first"
                           % (target["service"], busy[0].get("status"), busy[0].get("serviceDeploymentArn")))
    latest, tags = task_definition(cfg, target["family"])
    current = latest if latest["taskDefinitionArn"] == current_arn else task_definition(cfg, current_arn)[0]
    arch = ARCHITECTURES.get((latest.get("runtimePlatform") or {}).get("cpuArchitecture") or "X86_64")
    if not arch:
        raise RuntimeError("%s: unsupported cpuArchitecture %r" % (name, latest["runtimePlatform"]))

    docker_login(cfg, uri.split("/", 1)[0])
    key = content_key(name, arch, source)
    digest = ensure_image(cfg, name, uri, key, sha, git_tag, arch, source, log)
    image = "%s@%s" % (uri, digest)

    running = current["containerDefinitions"][container_index(current, uri, name)]["image"]
    running_digest = image_digest(cfg, repository, uri, running)
    if current_arn == latest["taskDefinitionArn"] and running_digest == digest:
        return True, ["%s: unchanged, %s keeps running %s" % (name, short_arn(current_arn), short_digest(digest))]

    index = container_index(latest, uri, name)
    if latest["containerDefinitions"][index]["image"] == image:
        new_arn = latest["taskDefinitionArn"]
        log("%s: %s already runs this image" % (name, short_arn(new_arn)))
    else:
        new_arn = register(cfg, with_image(latest, tags, index, image))
        log("%s: registered %s from %s (image only)" % (name, short_arn(new_arn), short_arn(latest["taskDefinitionArn"])))

    after = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=2)).isoformat()
    updated = aws(cfg.region, "ecs", "update-service", "--cluster", cfg.cluster, "--service", target["service"],
                  "--task-definition", new_arn)["service"]
    primary = next((d.get("id") for d in updated.get("deployments") or [] if d.get("status") == "PRIMARY"), None)
    arn = find_deployment(cfg, target["service"], new_arn, after)
    region = "--region " + cfg.region
    log("%s: deployment %s (%s)" % (name, arn, primary or "-"))
    log("%s: to roll back while it runs:\n    aws ecs stop-service-deployment %s --service-deployment-arn %s"
        " --stop-type ROLLBACK" % (name, region, arn))
    ok = follow(cfg, arn, timeout, log)
    previous_sha = commit_tag(cfg, repository, running_digest)
    back = ["aws ecs update-service %s --cluster %s --service %s --task-definition %s"
            % (region, cfg.cluster, target["service"], current_arn)]
    if previous_sha:
        back.append("make deploy REF=%s" % previous_sha)
    lines = ["%s: %s %s -> %s, deployment %s" % (name, "deployed" if ok else "NOT deployed",
                                                  short_arn(current_arn), short_arn(new_arn), arn)]
    if ok:
        lines.append("%s: roll back to what ran before with:\n    %s" % (name, "\n    ".join(back)))
    else:
        lines.append("%s: inspect: aws ecs describe-service-deployments %s --service-deployment-arns %s"
                     % (name, region, arn))
    return ok, lines


def release(args, environ=os.environ):
    cfg = Config(environ)
    if not FULL_SHA_RE.fullmatch(args.sha):
        raise Refused("--sha must be a full commit id")
    if args.git_tag and (not TAG_RE.fullmatch(args.git_tag) or args.git_tag.startswith(CONTENT_PREFIX)):
        raise Refused("--git-tag %r is not usable as an image tag" % args.git_tag)
    source = Path(args.source).resolve()
    if args.only and args.only not in cfg.targets:
        raise Refused("--only %s, but the %s variables are unset" % (args.only, args.only))
    names = [args.only] if args.only else list(cfg.targets)
    summary, failed = [], False
    for name in cfg.skipped:
        if not args.only:
            notice(skip_notice(name), environ)
            summary.append(skip_notice(name))
    for name in names:
        ok, lines = release_one(cfg, name, args.sha, args.git_tag, source, args.timeout,
                                push_only_arch=args.platform.split("/")[-1] if args.push_only else None)
        summary += lines
        if not ok:
            failed = True
            break  # never move on to the next service after a failed one
    print("\n".join(summary), flush=True)
    if environ.get("GITHUB_STEP_SUMMARY"):
        with open(environ["GITHUB_STEP_SUMMARY"], "a") as out:
            out.write("### Deploy %s%s\n\n```\n%s\n```\n" % (args.sha[:12], " (%s)" % args.git_tag if args.git_tag
                                                              else "", "\n".join(summary)))
    return 1 if failed else 0


# --- command line --------------------------------------------------------------


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="action", required=True)
    p = sub.add_parser("dispatch", help="run the Deploy workflow for HEAD (main, pushed, clean) or --ref")
    p.add_argument("--ref", help="a tag or commit on origin/main to (re)deploy instead of HEAD")
    p.add_argument("--allow-dirty", action="store_true", help="dispatch HEAD even with uncommitted changes")
    p.add_argument("--push-only", action="store_true", help="build and push images; leave ECS alone")
    p = sub.add_parser("resolve", help="print sha= and git_tag= for --ref (or origin/main), refusing what is not on main")
    p.add_argument("--ref")
    p.add_argument("--no-fetch", action="store_true", help="use the refs already fetched (the workflow's checkout)")
    p.add_argument("--require-ci", action="store_true", help="also refuse a commit CI has not passed on main")
    sub.add_parser("check-config", help="refuse when a DEPLOY_* repository variable is unset or malformed")
    p = sub.add_parser("release", help="(the Deploy workflow) build/push, register, update, wait")
    p.add_argument("--sha", required=True)
    p.add_argument("--git-tag", default="")
    p.add_argument("--source", default=str(REPO), help="checkout of --sha to build from")
    p.add_argument("--only", choices=sorted(TARGETS))
    p.add_argument("--push-only", action="store_true")
    p.add_argument("--platform", default="linux/amd64", help="with --push-only (no task definition to read)")
    p.add_argument("--timeout", type=int, default=3600, help="seconds to follow each service deployment")
    args = parser.parse_args(argv)
    try:
        if args.action == "dispatch":
            return dispatch(args)
        if args.action == "resolve":
            sha, tag = resolve(args.ref, fetch=not args.no_fetch)
            if args.require_ci and not ci_passed(sha):
                raise Refused("CI has not passed for %s on a push to %s" % (sha[:12], MAIN))
            print("sha=%s\ngit_tag=%s" % (sha, tag))
            return 0
        if args.action == "check-config":
            problems = config_problems(os.environ)
            if problems:
                raise Refused("deploy configuration incomplete; set these repository variables:\n  "
                              + "\n  ".join(problems))
            for target in skipped_targets(os.environ):
                notice(skip_notice(target))
            print("deploy configuration complete")
            return 0
        return release(args)
    except Refused as refusal:
        print("refused: " + str(refusal), file=sys.stderr)
        return 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, KeyError, ValueError, OSError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
