#!/usr/bin/env python3
"""Rehearse the SQLite -> PostgreSQL import from an AWS Backup restore (migration gate 5).

One command runs the whole rehearsal and writes a Markdown report:

  restore   start an AWS Backup item-level restore of grid.db (latest recovery
            point, or --recovery-point-arn) to the same EFS file system, wait,
            and find the restored copy (aws-backup-restore_<ts>/...) with the
            old account's EFS admin task
  snapshot  run the old account's sierra-gridctl-snapshot task on the restored
            copy (never the live grid.db), uploading to the old-account bucket
  transfer  fallback only (--new-bucket): copy the snapshot down with the old
            profile and up into a Grid-account bucket with the new profile
  import    run the new account's sierra-gridctl task: import --from s3://...;
            fails unless the task exits 0 and prints RESULT: OK
  verify    the same task with verify; RESULT: OK required
  refuse    import again without --allow-nonempty; it must be refused as
            "not empty" (the plan's empty-check proof)
  apidiff   scripts/apidiff.py between production and --new-base-url (optional)
  cleanup   delete the restored directory (EFS admin task, or print how) and
            the snapshot objects

    python3 scripts/rehearse_import.py --old-profile OLD --new-profile NEW --dry-run
    make rehearse-import REHEARSE_ARGS="--old-profile OLD --new-profile NEW"

Re-running with the same --run-id resumes: steps that finished are skipped
(the restore job reuses its idempotency token, an uploaded snapshot is not
taken again). --from-step and --only run the named steps even if they are
done. --dry-run prints every AWS command and runs none.

The report and a state file go to the workspace's notes/local/ (ignored);
the script refuses any other directory under notes/ and the repository. Both
hold hashes, counts and log tails of the system of record: never commit
them. Runbook: docs/import.md ("Rehearsal from an AWS Backup restore").

Python 3 standard library and AWS CLI v2 only, like scripts/deploy.py.
"""

import argparse
import datetime
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

REPO = Path(__file__).resolve().parents[1]
STEPS = ("restore", "snapshot", "transfer", "import", "verify", "refuse", "apidiff", "cleanup")
LEGACY_URL = "https://data.sierragridteam.org"
NEW_ACCOUNT_ID = "083901560296"

# gridctl's refusal messages (internal/sqliteimport ErrNotEmpty, ErrWriterLive).
NOT_EMPTY = "the target database is not empty"
WRITER_LIVE = "another process holds the Grid writer lease"

RESTORE_DIR = re.compile(r"/aws-backup-restore_[A-Za-z0-9:._+-]+")
SNAPSHOT_KEY = re.compile(r"grid-[A-Za-z0-9._-]+\.db")
MANIFEST_SUFFIX = ".manifest.json"

POLL_TASK, POLL_RESTORE = 10, 30


class StepFailed(RuntimeError):
    pass


class Account:
    def __init__(self, label, profile, region):
        self.label, self.profile, self.region = label, profile, region

    def at(self, region):
        return Account(self.label, self.profile, region)


def utc_now():
    return datetime.datetime.now(datetime.timezone.utc)


def iso(ts=None):
    return (ts or utc_now()).strftime("%Y-%m-%dT%H:%M:%SZ")


def duration(seconds):
    if seconds is None:
        return "-"
    seconds = int(max(seconds, 0) + 0.5)
    hours, rest = divmod(seconds, 3600)
    minutes, secs = divmod(rest, 60)
    if hours:
        return "%dh%02dm" % (hours, minutes)
    if minutes:
        return "%dm%02ds" % (minutes, secs)
    return "%ds" % secs


def parse_time(value):
    """An AWS CLI timestamp (ISO 8601 by default, epoch seconds with some configs)."""
    if value is None:
        return None
    if isinstance(value, (int, float)):
        return datetime.datetime.fromtimestamp(value, datetime.timezone.utc)
    try:
        return datetime.datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None


def seconds_between(start, end):
    a, b = parse_time(start), parse_time(end)
    return (b - a).total_seconds() if a and b else None


# --- gridctl output ------------------------------------------------------------
#
# gridctl prints its report to stdout (cmd/gridctl printReport); the awslogs
# driver turns each line into one event. The JSON --report file stays inside
# the task, so the counts come from these lines.

TABLE_ROW = re.compile(r"^(?P<table>[a-z_]+)\s+(?P<source>\d+)\s+(?P<target>\d+)\s+(?P<copy>[\d.]+)\s+"
                       r"(?P<check>[\d.]+)\s+(?P<sha>[0-9a-f]*)(?P<differs>\s+DIFFERS)?\s*$")
TOTALS = re.compile(r"^copy ([\d.]+)s, verify ([\d.]+)s, total ([\d.]+)s")
CHECK = re.compile(r"^check (\S+)\s+(.*)$")
INFO = re.compile(r"^info  (\S+)\s+(\d+)$")
SKIPPED = re.compile(r"^skipped (\S+) \((\d+) rows\)$")


def parse_gridctl_report(lines):
    out = {"tables": [], "checks": {}, "info": {}, "skipped": {}, "problems": [], "result": None}
    in_problems = False
    for raw in lines:
        line = raw.rstrip()
        m = TABLE_ROW.match(line)
        if m:
            out["tables"].append({"table": m["table"], "source": int(m["source"]), "target": int(m["target"]),
                                  "copySeconds": float(m["copy"]), "checkSeconds": float(m["check"]),
                                  "sha256": m["sha"], "differs": bool(m["differs"])})
            continue
        if line.startswith("RESULT: "):
            out["result"] = line[len("RESULT: "):]
            in_problems = out["result"] != "OK"
            continue
        if in_problems and line.startswith("  - "):
            out["problems"].append(line[4:])
            continue
        for pattern, key in ((CHECK, "checks"), (INFO, "info"), (SKIPPED, "skipped")):
            m = pattern.match(line)
            if m:
                out[key][m[1]] = int(m[2]) if key != "checks" else m[2]
                break
        m = TOTALS.match(line)
        if m:
            out["copySeconds"], out["verifySeconds"], out["totalSeconds"] = map(float, m.groups())
    return out


# --- AWS -----------------------------------------------------------------------


class Rehearsal:
    """The steps, over the AWS CLI. Every call goes through aws(); in dry-run
    it prints the command and returns the placeholder the caller supplies."""

    def __init__(self, args, state):
        self.args, self.state, self.dry = args, state, args.dry_run
        self.old = Account("old", args.old_profile, args.old_region)
        self.new = Account("new", args.new_profile, args.new_region)
        self.bucket_account = self.old.at(args.bucket_region)
        self.new_bucket_account = self.new.at(args.new_bucket_region or args.new_region)
        self._definitions, self._networks = {}, {}

    def say(self, text=""):
        print(text, flush=True)

    def aws(self, account, *command, dry=None, quiet=False, parse=True, check=True):
        cmd = ["aws"] + (["--profile", account.profile] if account.profile else []) + ["--region", account.region]
        cmd += list(command) + (["--output", "json"] if parse else [])
        if self.dry or not quiet:
            self.say("  $ " + shlex.join(cmd))
        if self.dry:
            return dry if dry is not None else ({} if parse else "")
        result = subprocess.run(cmd, capture_output=True, text=True,
                                env={**os.environ, "AWS_RETRY_MODE": "standard", "AWS_MAX_ATTEMPTS": "8",
                                     "AWS_PAGER": ""})
        if result.returncode:
            if not check:
                return None
            raise StepFailed("aws %s (%s account): %s" % (" ".join(command[:2]), account.label,
                                                          result.stderr.strip() or "exit %d" % result.returncode))
        if not parse:
            return result.stdout
        return json.loads(result.stdout) if result.stdout.strip() else {}

    def sleep(self, seconds):
        if not self.dry:
            time.sleep(seconds)

    # --- preflight ---

    def preflight(self):
        """Both identities, and the new one must be the Grid account: a wrong
        profile would otherwise import into whatever database it reaches."""
        for account, want in ((self.old, self.args.old_account_id), (self.new, self.args.new_account_id)):
            ident = self.aws(account, "sts", "get-caller-identity", dry={"Account": want or "<%s-account>" % account.label})
            got = ident.get("Account")
            if want and got != want and not self.dry:
                raise StepFailed("the %s profile is account %s, expected %s" % (account.label, got, want))
            self.state.setdefault("accounts", {})[account.label] = got
            if account is self.old:
                self.old_account_id = got

    # --- ECS one-off tasks ---

    def definition(self, account, family):
        key = (account.label, family)
        if key not in self._definitions:
            placeholder = {"taskDefinition": {"taskDefinitionArn": "<%s>" % family, "containerDefinitions": [
                {"name": "<container>", "logConfiguration": {"logDriver": "awslogs", "options": {
                    "awslogs-group": "<log-group>", "awslogs-stream-prefix": "<prefix>"}}}]}}
            self._definitions[key] = self.aws(account, "ecs", "describe-task-definition", "--task-definition", family,
                                              dry=placeholder)["taskDefinition"]
        return self._definitions[key]

    def network(self, account, cluster, service, subnets, groups, public_ip):
        if subnets:
            if not groups:
                raise StepFailed("--%s-subnets needs --%s-security-groups" % (account.label, account.label))
            vpc = {"subnets": subnets, "securityGroups": groups}
        else:
            if not service:
                raise StepFailed("no network for the %s account's tasks: pass --%s-subnets and "
                                 "--%s-security-groups, or --%s-service to copy" % ((account.label,) * 4))
            found = self.aws(account, "ecs", "describe-services", "--cluster", cluster, "--services", service,
                             dry={"services": [{"networkConfiguration": {"awsvpcConfiguration": {
                                 "subnets": ["<subnet>"], "securityGroups": ["<security-group>"],
                                 "assignPublicIp": "DISABLED"}}}]}).get("services") or []
            vpc = dict(((found[0] if found else {}).get("networkConfiguration") or {}).get("awsvpcConfiguration") or {})
            if not vpc.get("subnets"):
                raise StepFailed("service %s/%s has no awsvpc network to copy; pass --%s-subnets and "
                                 "--%s-security-groups" % (cluster, service, account.label, account.label))
            vpc.pop("assignPublicIp", None)
            vpc = {"subnets": vpc["subnets"], "securityGroups": vpc.get("securityGroups") or []}
        if public_ip is not None:
            vpc["assignPublicIp"] = "ENABLED" if public_ip else "DISABLED"
        return {"awsvpcConfiguration": vpc}

    def old_network(self):
        a = self.args
        if "old" not in self._networks:
            self._networks["old"] = self.network(self.old, a.old_cluster, a.old_service, a.old_subnets,
                                                 a.old_security_groups, a.old_public_ip)
        return self._networks["old"]

    def new_network(self):
        a = self.args
        if "new" not in self._networks:
            self._networks["new"] = self.network(self.new, a.new_cluster, a.new_service, a.new_subnets,
                                                 a.new_security_groups, a.new_public_ip)
        return self._networks["new"]

    def run_task(self, account, cluster, family, command, network, environment=None, log_group=None):
        """Run one task to completion. Returns its exit code, timings and log lines."""
        definition = self.definition(account, family)
        container = definition["containerDefinitions"][0]
        override = {"name": container["name"], "command": command}
        if environment:
            override["environment"] = [{"name": k, "value": v} for k, v in sorted(environment.items())]
        started = self.aws(account, "ecs", "run-task", "--cluster", cluster, "--task-definition", family,
                           "--launch-type", "FARGATE", "--count", "1",
                           "--started-by", "rehearsal-" + self.state["runId"],
                           "--network-configuration", json.dumps(network, separators=(",", ":")),
                           "--overrides", json.dumps({"containerOverrides": [override]}, separators=(",", ":")),
                           dry={"tasks": [{"taskArn": "arn:aws:ecs:%s:<account>:task/%s/<task-id>"
                                           % (account.region, cluster)}]})
        if started.get("failures") or not started.get("tasks"):
            raise StepFailed("run-task %s: %s" % (family, json.dumps(started.get("failures"))))
        arn = started["tasks"][0]["taskArn"]
        self.say("  task %s started" % arn.rsplit("/", 1)[-1])
        task = self.wait_task(account, cluster, arn)
        exit_code, reason = None, task.get("stoppedReason")
        for c in task.get("containers") or []:
            if c.get("name") == container["name"]:
                exit_code = c.get("exitCode")
                reason = reason or c.get("reason")
        lines = self.task_logs(account, container, arn, log_group)
        return {"taskArn": arn, "family": definition.get("taskDefinitionArn", family), "exitCode": exit_code,
                "stoppedReason": reason, "stopCode": task.get("stopCode"),
                "containerSeconds": seconds_between(task.get("startedAt"), task.get("stoppedAt")),
                "taskSeconds": seconds_between(task.get("createdAt"), task.get("stoppedAt")),
                "lines": lines}

    def wait_task(self, account, cluster, arn):
        deadline = time.monotonic() + self.args.task_timeout
        last = None
        while True:
            found = self.aws(account, "ecs", "describe-tasks", "--cluster", cluster, "--tasks", arn, quiet=True,
                             dry={"tasks": [{"lastStatus": "STOPPED", "containers": [{"name": "<container>",
                                                                                      "exitCode": 0}]}]})
            tasks = found.get("tasks") or []
            if not tasks:
                raise StepFailed("task %s disappeared: %s" % (arn, json.dumps(found.get("failures"))))
            status = tasks[0].get("lastStatus")
            if status != last:
                self.say("  task %s" % status)
                last = status
            if status == "STOPPED":
                return tasks[0]
            if time.monotonic() > deadline:
                raise StepFailed("task %s still %s after %s; it keeps running (stop it with aws ecs stop-task)"
                                 % (arn, status, duration(self.args.task_timeout)))
            self.sleep(POLL_TASK)

    def task_logs(self, account, container, arn, log_group=None):
        """The task's whole awslogs stream (prefix/container/task-id)."""
        options = (container.get("logConfiguration") or {}).get("options") or {}
        group = log_group or options.get("awslogs-group")
        prefix = options.get("awslogs-stream-prefix")
        if not group or not prefix:
            self.say("  (no awslogs group/prefix on the task definition; no logs)")
            return []
        stream = "%s/%s/%s" % (prefix, container["name"], arn.rsplit("/", 1)[-1])
        logs_account = account.at(options.get("awslogs-region") or account.region)
        for attempt in range(4):
            lines, token = [], None
            while True:
                command = ["logs", "get-log-events", "--log-group-name", group, "--log-stream-name", stream,
                           "--start-from-head"] + (["--next-token", token] if token else [])
                page = self.aws(logs_account, *command, quiet=token is not None, check=False,
                                dry={"events": [], "nextForwardToken": None})
                if page is None:
                    break
                lines += [e.get("message", "").rstrip("\n") for e in page.get("events") or []]
                nxt = page.get("nextForwardToken")
                if not nxt or nxt == token:
                    break
                token = nxt
            if lines or self.dry:
                return lines
            self.sleep(5)  # awslogs can trail the stop by a few seconds
        self.say("  (no log events in %s:%s)" % (group, stream))
        return []

    # --- steps ---

    def step_restore(self):
        a, st = self.args, self.state.setdefault("restore", {})
        fs = self.aws(self.old, "efs", "describe-file-systems", "--creation-token", a.efs_creation_token,
                      dry={"FileSystems": [{"FileSystemId": "<fs-id>",
                                            "FileSystemArn": "arn:aws:elasticfilesystem:%s:<account>:file-system/<fs-id>"
                                            % self.old.region}]}).get("FileSystems") or []
        if a.efs_id:
            fs = [f for f in fs if f.get("FileSystemId") == a.efs_id] or [{"FileSystemId": a.efs_id}]
        if len(fs) != 1:
            raise StepFailed("expected one EFS file system with creation token %s, found %d (pass --efs-id)"
                             % (a.efs_creation_token, len(fs)))
        st["fileSystemId"] = fs[0]["FileSystemId"]
        rp = a.recovery_point_arn
        if not rp:
            points = self.aws(self.old, "backup", "list-recovery-points-by-backup-vault",
                              "--backup-vault-name", a.backup_vault,
                              *(["--by-resource-arn", fs[0]["FileSystemArn"]] if fs[0].get("FileSystemArn") else []),
                              dry={"RecoveryPoints": [{"RecoveryPointArn": "<recovery-point-arn>",
                                                       "CreationDate": "<created>", "Status": "COMPLETED"}]})
            done = [p for p in points.get("RecoveryPoints") or [] if p.get("Status") == "COMPLETED"]
            if not done:
                raise StepFailed("no completed recovery point in vault %s" % a.backup_vault)
            latest = max(done, key=lambda p: parse_time(p.get("CreationDate")) or datetime.datetime.min.replace(
                tzinfo=datetime.timezone.utc))
            rp, st["recoveryPointCreated"] = latest["RecoveryPointArn"], latest.get("CreationDate")
        if st.get("jobId") and st.get("recoveryPointArn") == rp:
            job = st["jobId"]
            self.say("  resuming restore job %s" % job)
        else:
            st["recoveryPointArn"] = rp
            meta = self.aws(self.old, "backup", "get-recovery-point-restore-metadata",
                            "--backup-vault-name", a.backup_vault, "--recovery-point-arn", rp,
                            dry={"RestoreMetadata": {"Encrypted": "true"}}).get("RestoreMetadata") or {}
            # Item-level restore into the same file system: the copy lands in
            # aws-backup-restore_<ts>/ at the file system root.
            meta.pop("CreationToken", None)
            meta.update({"file-system-id": st["fileSystemId"], "newFileSystem": "false",
                         "ItemsToRestore": json.dumps([a.restore_item])})
            started = self.aws(self.old, "backup", "start-restore-job", "--recovery-point-arn", rp,
                               "--iam-role-arn", self.restore_role_arn(), "--resource-type", "EFS",
                               "--metadata", json.dumps(meta, separators=(",", ":")),
                               "--idempotency-token", "rehearsal-" + self.state["runId"],
                               dry={"RestoreJobId": "<restore-job-id>"})
            job = st["jobId"] = started["RestoreJobId"]
            st["requestedAt"] = iso()
        info = self.wait_restore(job)
        st.update({"status": info.get("Status"), "createdAt": info.get("CreationDate"),
                   "completedAt": info.get("CompletionDate"),
                   "seconds": seconds_between(info.get("CreationDate"), info.get("CompletionDate")),
                   "backupSizeBytes": info.get("BackupSizeInBytes")})
        if a.restored_path:
            st["path"] = a.restored_path
        else:
            st["path"] = self.discover_restored(st)
        return "job %s, %s" % (job, st.get("path"))

    def restore_role_arn(self):
        role = self.args.restore_role
        if role.startswith("arn:"):
            return role
        return "arn:aws:iam::%s:role/%s" % (getattr(self, "old_account_id", None) or "<old-account>", role)

    def wait_restore(self, job):
        deadline = time.monotonic() + self.args.restore_timeout
        last = None
        while True:
            info = self.aws(self.old, "backup", "describe-restore-job", "--restore-job-id", job, quiet=True,
                            dry={"Status": "COMPLETED"})
            status = info.get("Status")
            if status != last:
                self.say("  restore %s" % status)
                last = status
            if status == "COMPLETED":
                return info
            if status in ("ABORTED", "FAILED"):
                raise StepFailed("restore job %s %s: %s" % (job, status, info.get("StatusMessage", "")))
            if time.monotonic() > deadline:
                raise StepFailed("restore job %s still %s after %s" % (job, status, duration(self.args.restore_timeout)))
            self.sleep(POLL_RESTORE)

    def admin_task(self, script):
        a = self.args
        return self.run_task(self.old, a.old_cluster, a.efs_admin_family, [script], self.old_network(),
                             log_group=a.old_log_group)

    def discover_restored(self, st):
        """List the restored copies through the admin task (the file system
        root mounted at --efs-root-mount) and take the newest restore
        directory. Restore directories are named by their UTC time."""
        a = self.args
        if not a.efs_admin_family:
            raise StepFailed("cannot find the restored copy without the EFS admin task: look for "
                             "aws-backup-restore_*%s on %s and pass --restored-path" % (a.restore_item,
                                                                                    st["fileSystemId"]))
        mount = a.efs_root_mount.rstrip("/")
        name = a.restore_item.rsplit("/", 1)[-1]
        globs = " ".join(shlex.quote(mount) + "/aws-backup-restore_*" + shlex.quote(p)
                         for p in dict.fromkeys([a.restore_item, "/" + name]))
        script = ('for f in %s; do if [ -f "$f" ]; then echo "RESTORED $(stat -c \'%%s %%u:%%g %%a\' "$f") $f"; '
                  'fi; done' % globs)
        result = self.admin_task(script)
        st["discoverTask"] = summary(result)
        found = []
        for line in result["lines"]:
            parts = line.split()
            if len(parts) == 5 and parts[0] == "RESTORED" and parts[4].startswith(mount + "/"):
                rel = parts[4][len(mount):]
                if RESTORE_DIR.match(rel):
                    found.append({"path": rel, "size": int(parts[1]), "owner": parts[2], "mode": parts[3]})
        if self.dry:
            return "/aws-backup-restore_DRYRUN" + a.restore_item
        if result["exitCode"] != 0 or not found:
            raise StepFailed("no restored %s under %s (admin task exit %s); check the restore and pass "
                             "--restored-path" % (a.restore_item, mount, result["exitCode"]))
        found.sort(key=lambda f: RESTORE_DIR.match(f["path"]).group(0))
        st["found"] = found
        if len(found) > 1:
            st["warning"] = ("%d restore directories on the file system; using the newest. Remove the others "
                             "(cleanup removes only this run's)." % len(found))
            self.say("  " + st["warning"])
        st["restoredSize"], st["restoredOwner"] = found[-1]["size"], found[-1]["owner"]
        return found[-1]["path"]

    def snapshot_location(self):
        return "s3://%s/%s" % (self.args.bucket, self.state["snapshotKey"])

    def source_location(self):
        if (self.state.get("transfer") or {}).get("location"):
            return self.state["transfer"]["location"]
        return self.snapshot_location()

    def read_manifest(self, account, bucket, key):
        text = self.aws(account, "s3", "cp", "s3://%s/%s%s" % (bucket, key, MANIFEST_SUFFIX), "-",
                        parse=False, check=False, dry="")
        if not text:
            return None
        try:
            return json.loads(text)
        except ValueError:
            raise StepFailed("s3://%s/%s%s is not a manifest" % (bucket, key, MANIFEST_SUFFIX))

    def step_snapshot(self, explicit):
        a, st = self.args, self.state.setdefault("snapshot", {})
        path = a.restored_path or (self.state.get("restore") or {}).get("path")
        if not path:
            raise StepFailed("no restored path: run the restore step or pass --restored-path")
        if not RESTORE_DIR.match(path):
            raise StepFailed("refusing to snapshot %s: only a restored copy (/aws-backup-restore_*/...) is "
                             "rehearsed, never the live grid.db" % path)
        key = self.state["snapshotKey"]
        manifest = None if explicit else self.read_manifest(self.bucket_account, a.bucket, key)
        if manifest:
            self.say("  %s already uploaded; not taken again" % self.snapshot_location())
            st["reused"] = True
        else:
            db = a.efs_root_mount.rstrip("/") + path
            result = self.run_task(self.old, a.old_cluster, a.snapshot_family,
                                   ["snapshot", "--db", db, "--out", self.snapshot_location()], self.old_network(),
                                   # gridctl's S3 client uses the task's region; the bucket is in another.
                                   environment={"AWS_REGION": a.bucket_region, "AWS_DEFAULT_REGION": a.bucket_region},
                                   log_group=a.old_log_group)
            st["task"] = summary(result)
            if self.dry:
                return "would snapshot %s" % db
            if result["exitCode"] != 0:
                raise StepFailed("snapshot task exited %s (%s); log tail in the report"
                                 % (result["exitCode"], result["stoppedReason"]))
            manifest = self.read_manifest(self.bucket_account, a.bucket, key)
            if not manifest:
                raise StepFailed("the snapshot task succeeded but %s%s is missing" % (self.snapshot_location(),
                                                                                    MANIFEST_SUFFIX))
        st.update({"location": self.snapshot_location(), "size": manifest.get("size"),
                   "sha256": manifest.get("sha256"), "quickCheck": manifest.get("quickCheck"),
                   "schemaVersion": manifest.get("schemaVersion"), "createdAt": manifest.get("createdAt"),
                   "journalMode": manifest.get("sourceJournalMode"), "gridctl": manifest.get("gridctlVersion"),
                   "tables": manifest.get("tables") or {}})
        if manifest.get("quickCheck") not in (None, "ok"):
            raise StepFailed("snapshot quick_check: %s" % manifest.get("quickCheck"))
        return "%s, %s bytes" % (self.snapshot_location(), st["size"])

    def step_transfer(self):
        """The fallback when the Grid account cannot read the old account's
        bucket: download with the old profile, upload with the new, database
        first and manifest last (a manifest means a complete upload)."""
        a, key = self.args, self.state["snapshotKey"]
        scratch = tempfile.mkdtemp(prefix="sierra-rehearsal-", dir=a.scratch)
        try:
            for suffix in ("", MANIFEST_SUFFIX):
                local = os.path.join(scratch, key + suffix)
                self.aws(self.bucket_account, "s3", "cp", "s3://%s/%s%s" % (a.bucket, key, suffix), local,
                         "--only-show-errors", parse=False)
            for suffix in ("", MANIFEST_SUFFIX):
                local = os.path.join(scratch, key + suffix)
                self.aws(self.new_bucket_account, "s3", "cp", local, "s3://%s/%s%s" % (a.new_bucket, key, suffix),
                         "--checksum-algorithm", "SHA256", "--only-show-errors", parse=False)
        finally:
            shutil.rmtree(scratch, ignore_errors=True)
        location = "s3://%s/%s" % (a.new_bucket, key)
        self.state["transfer"] = {"location": location}
        return location

    def writer_check(self):
        """The import refuses while a server holds the writer lease; check the
        service is configured with the writer off first, so the operator gets
        the fix rather than a refusal."""
        a = self.args
        if a.skip_writer_check or not a.new_service:
            return "not checked"
        found = self.aws(self.new, "ecs", "describe-services", "--cluster", a.new_cluster, "--services", a.new_service,
                         dry={"services": []}).get("services") or []
        live = [s for s in found if s.get("status") == "ACTIVE"]
        if not live:
            return "service %s not found; not checked" % a.new_service
        definitions = {d.get("taskDefinition") for d in live[0].get("deployments") or []} | {live[0].get("taskDefinition")}
        for arn in sorted(d for d in definitions if d):
            td = self.aws(self.new, "ecs", "describe-task-definition", "--task-definition", arn)["taskDefinition"]
            values = [e.get("value", "") for c in td.get("containerDefinitions") or [] for e in c.get("environment") or []
                      if e.get("name") == "PF__GRID__WRITER__ENABLED"]
            if not values or any(v.strip().lower() != "false" for v in values):
                raise StepFailed("the Grid service %s (%s) does not set PF__GRID__WRITER__ENABLED=false; the import "
                                 "would be refused or race the writer. Deploy it with the writer disabled first "
                                 "(--skip-writer-check to override)" % (a.new_service, arn.rsplit("/", 1)[-1]))
        return "writer disabled on %s" % a.new_service

    def gridctl(self, *command):
        a = self.args
        return self.run_task(self.new, a.new_cluster, a.gridctl_family, list(command), self.new_network(),
                             log_group=a.new_log_group)

    def step_import(self):
        st = self.state.setdefault("import", {})
        st["writerCheck"] = self.writer_check()
        command = ["import", "--from", self.source_location(), "--report", "/tmp/report.json"]
        if self.args.allow_nonempty:
            command.append("--allow-nonempty")
        result = self.gridctl(*command)
        st.update({"task": summary(result), "report": parse_gridctl_report(result["lines"]),
                   "allowNonempty": self.args.allow_nonempty})
        return self.require_ok("import", result, st["report"])

    def step_verify(self):
        st = self.state.setdefault("verify", {})
        result = self.gridctl("verify", "--from", self.source_location(), "--report", "/tmp/report.json")
        st.update({"task": summary(result), "report": parse_gridctl_report(result["lines"])})
        return self.require_ok("verify", result, st["report"])

    def require_ok(self, what, result, report):
        if self.dry:
            return "would require exit 0 and RESULT: OK"
        if result["exitCode"] == 0 and report["result"] == "OK":
            return "RESULT: OK in %s" % duration(result["containerSeconds"])
        why = "exit %s" % result["exitCode"]
        if result["stoppedReason"] and result["exitCode"] is None:
            why += " (%s)" % result["stoppedReason"]
        why += ", RESULT: %s" % (report["result"] or "not printed")
        text = "\n".join(result["lines"])
        if what == "import" and WRITER_LIVE in text:
            why += "; a server holds the writer lease (run the Grid with PF__GRID__WRITER__ENABLED=false)"
        elif what == "import" and NOT_EMPTY in text:
            why += "; the database holds data (recreate it, or --allow-nonempty for a rehearsal re-run)"
        elif "AccessDenied" in text or "StatusCode: 403" in text:
            why += ("; S3 access denied: if the Grid account cannot read the old account's bucket, rerun with "
                    "--new-bucket (docs/import.md, fallback)")
        raise StepFailed("%s failed: %s" % (what, why))

    def step_refuse(self):
        """The import must refuse a database that already holds Grid data."""
        result = self.gridctl("import", "--from", self.source_location())
        text = "\n".join(result["lines"])
        st = self.state.setdefault("refuse", {})
        st["task"] = summary(result)
        if self.dry:
            return "would require a refusal naming '%s'" % NOT_EMPTY
        if result["exitCode"] == 0:
            raise StepFailed("a second import without --allow-nonempty succeeded; the empty check did not refuse")
        if NOT_EMPTY not in text:
            raise StepFailed("the second import failed (exit %s) but not with '%s'; read the log tail"
                             % (result["exitCode"], NOT_EMPTY))
        st["refused"] = True
        return "refused: %s" % NOT_EMPTY

    def step_apidiff(self):
        a = self.args
        command = [sys.executable, str(REPO / "scripts" / "apidiff.py"), "--legacy", a.legacy_url,
                   "--new", a.new_base_url, "--events", str(a.apidiff_events)]
        self.say("  $ " + shlex.join(command))
        st = self.state.setdefault("apidiff", {})
        st.update({"legacy": a.legacy_url, "new": a.new_base_url})
        if self.dry:
            return "would diff"
        result = subprocess.run(command, capture_output=True, text=True)
        lines = (result.stdout + result.stderr).splitlines()
        st.update({"exitCode": result.returncode, "differences": sum(l.startswith("== DIFF") for l in lines),
                   "lines": lines[:a.report_lines * 4]})
        if result.returncode == 2:
            raise StepFailed("apidiff: a server was unreachable")
        if result.returncode == 1:
            note = ("%d endpoint(s) differ; production has moved on since the recovery point, so explain each one"
                    % st["differences"])
            if a.apidiff_strict:
                raise StepFailed("apidiff: " + note)
            return note
        return "no differences"

    def step_cleanup(self):
        a, st = self.args, self.state.setdefault("cleanup", {})
        done = []
        path = (self.state.get("restore") or {}).get("path")
        if a.keep_restore or not path:
            st["restore"] = "kept" if path else "nothing restored"
        else:
            m = RESTORE_DIR.match(path)
            if not m:
                raise StepFailed("refusing to delete %s: not an aws-backup-restore_ directory" % path)
            target = a.efs_root_mount.rstrip("/") + m.group(0)
            if a.efs_admin_family:
                result = self.admin_task("rm -rf -- %s && echo REMOVED %s" % (shlex.quote(target), shlex.quote(target)))
                st["restoreTask"] = summary(result)
                if not self.dry and (result["exitCode"] != 0 or not any(l.startswith("REMOVED ") for l in result["lines"])):
                    raise StepFailed("removing %s failed (exit %s)" % (target, result["exitCode"]))
                st["restore"] = "removed %s" % m.group(0)
            else:
                st["restore"] = ("NOT REMOVED: no EFS admin task. Mount file system %s at its root as root "
                                 "(a one-off task) and run: rm -rf -- %s"
                                 % ((self.state.get("restore") or {}).get("fileSystemId", "<fs-id>"), shlex.quote(target)))
                self.say("  " + st["restore"])
            done.append(st["restore"])
        if a.keep_snapshot:
            st["snapshot"] = "kept"
        else:
            key = self.state["snapshotKey"]
            targets = [(self.bucket_account, a.bucket)]
            if self.state.get("transfer"):
                targets.append((self.new_bucket_account, a.new_bucket))
            for account, bucket in targets:
                for suffix in ("", MANIFEST_SUFFIX):
                    self.aws(account, "s3api", "delete-object", "--bucket", bucket, "--key", key + suffix)
            st["snapshot"] = "deleted %s" % ", ".join("s3://%s/%s" % (b, key) for _, b in targets)
        done.append(st["snapshot"])
        return "; ".join(done)


def summary(result):
    """What the report keeps of a task run: no full logs, the tail only."""
    keep = dict(result)
    keep["logTail"] = result["lines"][-REPORT_TAIL:]
    keep["logLines"] = len(result["lines"])
    del keep["lines"]
    return keep


REPORT_TAIL = 60


# --- the report ------------------------------------------------------------------


def render_report(state):
    steps = state.get("steps") or {}
    failed = [n for n in STEPS if (steps.get(n) or {}).get("status") == "failed"]
    if failed:
        outcome = "**FAILED** at `%s`: %s" % (failed[0], steps[failed[0]].get("error", ""))
    elif all((steps.get(n) or {}).get("status") in ("ok", "skipped") for n in STEPS):
        outcome = "**PASSED**" if (steps.get("verify") or {}).get("status") == "ok" else "complete, not verified"
    else:
        outcome = "incomplete (resume with `--run-id %s`)" % state.get("runId")
    out = ["# Import rehearsal %s" % state.get("runId"), "",
           "Written by `scripts/rehearse_import.py` (sierra-data) at %s. It holds hashes, row counts and log "
           "tails of the production store: keep it in `notes/local/` only." % state.get("updatedAt", "-"), "",
           "Outcome: " + outcome, ""]
    accounts = state.get("accounts") or {}
    if accounts:
        out += ["Accounts: old %s, new %s." % (accounts.get("old", "-"), accounts.get("new", "-")), ""]
    out += ["| step | status | started (UTC) | took | detail |", "| --- | --- | --- | --- | --- |"]
    for name in STEPS:
        s = steps.get(name) or {}
        detail = s.get("error") if s.get("status") == "failed" else s.get("detail", "")
        out.append("| %s | %s | %s | %s | %s |" % (name, s.get("status", "not run"), s.get("startedAt", "-"),
                                                   duration(s.get("seconds")), cell(detail)))
    restore = state.get("restore") or {}
    if restore:
        out += ["", "## Restore", "",
                "- recovery point: `%s` (created %s)" % (restore.get("recoveryPointArn", "-"),
                                                         restore.get("recoveryPointCreated", "-")),
                "- restore job: `%s`, %s, %s (AWS Backup's own time)" % (
                    restore.get("jobId", "-"), restore.get("status", "-"), duration(restore.get("seconds"))),
                "- restored copy: `%s:%s`%s" % (restore.get("fileSystemId", "-"), restore.get("path", "-"),
                                                ", %s bytes, owner %s" % (restore["restoredSize"], restore.get("restoredOwner"))
                                                if restore.get("restoredSize") is not None else "")]
        if restore.get("warning"):
            out.append("- warning: " + restore["warning"])
    snap = state.get("snapshot") or {}
    if snap.get("location"):
        out += ["", "## Snapshot", "",
                "- `%s`%s" % (snap["location"], " (reused from an earlier run)" if snap.get("reused") else ""),
                "- %s bytes, sha256 `%s`, schema v%s, quick_check %s, source journal mode %s, gridctl %s" % (
                    snap.get("size"), snap.get("sha256"), snap.get("schemaVersion"), snap.get("quickCheck"),
                    snap.get("journalMode"), snap.get("gridctl")),
                "- snapshot task: %s in the container" % duration(((snap.get("task") or {}).get("containerSeconds")))]
        if (state.get("transfer") or {}).get("location"):
            out.append("- copied to `%s` (fallback)" % state["transfer"]["location"])
    for name in ("import", "verify"):
        section = state.get(name) or {}
        report = section.get("report")
        if not report:
            continue
        task = section.get("task") or {}
        out += ["", "## " + name.capitalize(), ""]
        if name == "import":
            out.append("- writer check: %s; `--allow-nonempty` %s" % (section.get("writerCheck", "-"),
                                                                    "yes" if section.get("allowNonempty") else "no"))
        out.append("- RESULT: %s; task exit %s; container %s, task %s" % (
            report.get("result") or "not printed", task.get("exitCode"), duration(task.get("containerSeconds")),
            duration(task.get("taskSeconds"))))
        if report.get("totalSeconds") is not None:
            out.append("- gridctl: copy %.1fs, verify %.1fs, total %.1fs" % (
                report.get("copySeconds", 0), report["verifySeconds"], report["totalSeconds"]))
        if report.get("tables"):
            out += ["", "| table | source | target | copy s | check s | sha256 (target, prefix) |",
                    "| --- | ---: | ---: | ---: | ---: | --- |"]
            for t in report["tables"]:
                out.append("| %s | %d | %d | %.2f | %.2f | `%s`%s |" % (
                    t["table"], t["source"], t["target"], t["copySeconds"], t["checkSeconds"], t["sha256"],
                    " **DIFFERS**" if t["differs"] else ""))
        for label, key in (("checks", "checks"), ("informational", "info"), ("not imported", "skipped")):
            if report.get(key):
                out += ["", "%s: %s" % (label.capitalize(), ", ".join("%s %s" % kv for kv in sorted(report[key].items())))]
        if report.get("problems"):
            out += ["", "Problems:"] + ["- " + p for p in report["problems"]]
    refuse = state.get("refuse") or {}
    if refuse:
        out += ["", "## Re-import refusal", "",
                "- %s" % ("refused as expected (`%s`)" % NOT_EMPTY if refuse.get("refused") else "not confirmed")]
    diff = state.get("apidiff") or {}
    if diff:
        out += ["", "## API diff", "",
                "- `%s` vs `%s`: exit %s, %s endpoint(s) differ" % (diff.get("legacy"), diff.get("new"),
                                                                    diff.get("exitCode", "-"), diff.get("differences", "-")),
                "- the recovery point predates production's current state, so differences are expected; each is "
                "a finding to explain"]
        if diff.get("lines"):
            out += ["", "```"] + diff["lines"] + ["```"]
    cleanup = state.get("cleanup") or {}
    if cleanup:
        out += ["", "## Cleanup", ""] + ["- %s: %s" % (k, cleanup[k]) for k in ("restore", "snapshot") if k in cleanup]
    tails = [(label, task) for label, task in (
        ("restore discovery", restore.get("discoverTask")), ("snapshot", snap.get("task")),
        ("import", (state.get("import") or {}).get("task")), ("verify", (state.get("verify") or {}).get("task")),
        ("re-import", refuse.get("task")), ("cleanup", cleanup.get("restoreTask"))) if task]
    if tails:
        out += ["", "## Task logs (tails)"]
        for label, task in tails:
            out += ["", "### %s: task `%s`, exit %s%s" % (
                label, str(task.get("taskArn", "-")).rsplit("/", 1)[-1], task.get("exitCode"),
                ", %s" % task["stoppedReason"] if task.get("stoppedReason") else ""), "",
                "```"] + (task.get("logTail") or ["(no log events)"]) + ["```"]
    return "\n".join(out) + "\n"


def cell(text):
    return str(text or "").replace("|", "\\|").replace("\n", " ")


# --- main ------------------------------------------------------------------------


def default_report_dir():
    """The workspace's notes/local/ (the agent workspace keeps rehearsal reports there)."""
    if os.environ.get("SIERRA_REHEARSAL_DIR"):
        return Path(os.environ["SIERRA_REHEARSAL_DIR"])
    for parent in REPO.parents:
        if (parent / "OPEN_THREADS.md").is_file() and (parent / "notes").is_dir():
            return parent / "notes" / "local"
    return None


def check_report_dir(path):
    """notes/local/ or anywhere outside notes/ and the repository: the report
    holds the system of record's hashes and must never be committed."""
    resolved = Path(path).resolve()
    if resolved == REPO or REPO in resolved.parents:
        return "the report directory %s is inside the repository" % resolved
    parts = resolved.parts
    for i, part in enumerate(parts):
        if part == "notes" and (i + 1 >= len(parts) or parts[i + 1] != "local"):
            return "the report directory %s is under notes/ but not notes/local/" % resolved
    return None


def csv(value):
    return [v for v in value.split(",") if v] if value else []


def parse_args(argv):
    env = os.environ.get
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    g = p.add_argument_group("run")
    g.add_argument("--run-id", help="resume this run (default: a new UTC timestamp, e.g. 20261112T101500Z)")
    sel = g.add_mutually_exclusive_group()
    sel.add_argument("--from-step", choices=STEPS, help="run this step and the ones after it, even if done")
    sel.add_argument("--only", help="comma-separated steps to run, even if done (e.g. verify,apidiff)")
    g.add_argument("--dry-run", action="store_true", help="print every AWS command; run none, write nothing")
    g.add_argument("--report-dir", type=Path, default=None,
                   help="where the report and state go (default $SIERRA_REHEARSAL_DIR, else the workspace's notes/local/)")
    g.add_argument("--report-lines", type=int, default=60, help="log lines kept per task in the report (default 60)")
    g.add_argument("--keep-restore", action="store_true", help="cleanup leaves the restored directory")
    g.add_argument("--keep-snapshot", action="store_true", help="cleanup leaves the snapshot objects")
    g.add_argument("--skip", default="", help="comma-separated steps to leave out (e.g. refuse,cleanup)")

    g = p.add_argument_group("old account (project-hub: EFS, AWS Backup, the snapshot task, the bucket)")
    g.add_argument("--old-profile", default=env("SIERRA_OLD_AWS_PROFILE"), help="AWS profile (default $SIERRA_OLD_AWS_PROFILE)")
    g.add_argument("--old-region", default="us-east-1", help="default %(default)s")
    g.add_argument("--old-account-id", default=env("SIERRA_OLD_AWS_ACCOUNT_ID", ""),
                   help="refuse a profile in another account (default $SIERRA_OLD_AWS_ACCOUNT_ID; unchecked if empty)")
    g.add_argument("--old-cluster", default="project-hub", help="default %(default)s")
    g.add_argument("--old-service", default="sierra-data",
                   help="service whose subnets and security groups the old tasks copy (the EFS security group "
                        "admits only its group; default %(default)s)")
    g.add_argument("--old-subnets", type=csv, default=[], help="comma-separated; overrides --old-service")
    g.add_argument("--old-security-groups", type=csv, default=[], help="comma-separated; with --old-subnets")
    g.add_argument("--old-public-ip", action=argparse.BooleanOptionalAction, default=True,
                   help="assign a public IP (the old tasks run in public subnets; default on)")
    g.add_argument("--old-log-group", default=None, help="default: the task definition's awslogs-group")
    g.add_argument("--snapshot-family", default="sierra-gridctl-snapshot", help="default %(default)s")
    g.add_argument("--efs-admin-family", default="sierra-efs-admin",
                   help="one-off task with entryPoint [/bin/sh, -c] and the file system root at --efs-root-mount, "
                        "used to find and delete the restored copy; '' to do both by hand (default %(default)s)")
    g.add_argument("--efs-root-mount", default="/efs",
                   help="where both old task definitions mount the EFS file system root (default %(default)s)")
    g.add_argument("--efs-creation-token", default="info-ersn-data", help="default %(default)s")
    g.add_argument("--efs-id", default=None, help="file system id (default: found by --efs-creation-token)")
    g.add_argument("--backup-vault", default="info-ersn-backups", help="default %(default)s")
    g.add_argument("--recovery-point-arn", default=None, help="default: the latest completed recovery point")
    g.add_argument("--restore-role", default="info-ersn-backup",
                   help="role name or ARN AWS Backup restores with (default %(default)s)")
    g.add_argument("--restore-item", default="/info-ersn/grid.db",
                   help="path on the file system (the access point root is /info-ersn; default %(default)s)")
    g.add_argument("--restored-path", default=None,
                   help="the restored copy relative to the file system root, e.g. "
                        "/aws-backup-restore_<ts>/info-ersn/grid.db (default: found by the admin task)")
    g.add_argument("--bucket", default=env("SIERRA_MIGRATION_BUCKET", "sierra-grid-migration"),
                   help="migration bucket in the old account (default $SIERRA_MIGRATION_BUCKET or %(default)s)")
    g.add_argument("--bucket-region", default="us-west-2", help="default %(default)s")
    g.add_argument("--snapshot-key", default=None, help="object key, grid-*.db (default grid-rehearsal-<run-id>.db)")

    g = p.add_argument_group("new account (Grid 083901560296: RDS, the gridctl task)")
    g.add_argument("--new-profile", default=env("SIERRA_NEW_AWS_PROFILE"), help="AWS profile (default $SIERRA_NEW_AWS_PROFILE)")
    g.add_argument("--new-region", default="us-west-2", help="default %(default)s")
    g.add_argument("--new-account-id", default=NEW_ACCOUNT_ID, help="refuse a profile in another account (default %(default)s)")
    g.add_argument("--new-cluster", default="sierra-grid", help="default %(default)s")
    g.add_argument("--new-service", default="sierra-grid",
                   help="the Grid service: its writer must be disabled; its network is copied when --new-subnets "
                        "is not given ('' to skip both; default %(default)s)")
    g.add_argument("--new-subnets", type=csv, default=csv(env("SIERRA_GRIDCTL_SUBNETS", "")),
                   help="private subnets for gridctl (default $SIERRA_GRIDCTL_SUBNETS)")
    g.add_argument("--new-security-groups", type=csv, default=csv(env("SIERRA_GRIDCTL_SECURITY_GROUPS", "")),
                   help="the gridctl security group RDS admits (default $SIERRA_GRIDCTL_SECURITY_GROUPS)")
    g.add_argument("--new-public-ip", action=argparse.BooleanOptionalAction, default=False,
                   help="assign a public IP (default off: private subnets)")
    g.add_argument("--new-log-group", default=None, help="default: the task definition's awslogs-group")
    g.add_argument("--gridctl-family", default="sierra-gridctl", help="default %(default)s")
    g.add_argument("--allow-nonempty", action="store_true",
                   help="import replaces existing data (a second rehearsal on the same database)")
    g.add_argument("--skip-writer-check", action="store_true",
                   help="import without checking that --new-service runs with PF__GRID__WRITER__ENABLED=false")
    g.add_argument("--new-bucket", default=None,
                   help="fallback: copy the snapshot into this Grid-account bucket and import from there")
    g.add_argument("--new-bucket-region", default=None, help="default --new-region")
    g.add_argument("--scratch", default=None, help="local directory for the fallback copy (default the system temp)")

    g = p.add_argument_group("API diff (optional)")
    g.add_argument("--new-base-url", default=None, help="a new-stack server on the imported database; enables apidiff")
    g.add_argument("--legacy-url", default=LEGACY_URL, help="default %(default)s")
    g.add_argument("--apidiff-events", type=int, default=25, help="sampled event ids (-1 all; default 25)")
    g.add_argument("--apidiff-strict", action="store_true", help="fail the run on any API difference")

    g = p.add_argument_group("timeouts (seconds)")
    g.add_argument("--task-timeout", type=int, default=3600)
    g.add_argument("--restore-timeout", type=int, default=7200)
    args = p.parse_args(argv)

    if args.only:
        args.only = csv(args.only)
        bad = [s for s in args.only if s not in STEPS]
        if bad:
            p.error("--only: unknown step(s) %s (steps: %s)" % (", ".join(bad), ", ".join(STEPS)))
    args.skip = csv(args.skip)
    if [s for s in args.skip if s not in STEPS]:
        p.error("--skip: steps are %s" % ", ".join(STEPS))
    if args.run_id and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,25}", args.run_id):
        p.error("--run-id: letters, digits, '.', '_' and '-', at most 26 characters")
    if args.snapshot_key and not SNAPSHOT_KEY.fullmatch(args.snapshot_key):
        p.error("--snapshot-key must look like grid-<name>.db (the bucket policy grants grid-*)")
    if args.restored_path and not RESTORE_DIR.match(args.restored_path):
        p.error("--restored-path must start with /aws-backup-restore_ (the restored copy, never the live file)")
    if not args.dry_run and not (args.old_profile and args.new_profile):
        p.error("two accounts: pass --old-profile and --new-profile (or set SIERRA_OLD_AWS_PROFILE and "
                "SIERRA_NEW_AWS_PROFILE)")
    report_dir = args.report_dir or default_report_dir()
    if report_dir is None:
        p.error("no workspace notes/local/ found above the repository: pass --report-dir")
    problem = check_report_dir(report_dir)
    if problem:
        p.error(problem)
    args.report_dir = Path(report_dir)
    return args


def plan(args, state):
    """[(step, explicit)] in order; explicit steps run even if they are done."""
    if args.only:
        return [(s, True) for s in STEPS if s in args.only]
    start = STEPS.index(args.from_step) if args.from_step else len(STEPS)
    return [(s, i >= start) for i, s in enumerate(STEPS) if not args.from_step or i >= start]


def skip_reason(args, name):
    if name in args.skip:
        return "--skip"
    if name == "transfer" and not args.new_bucket:
        return "no --new-bucket (the Grid account reads the old bucket)"
    if name == "apidiff" and not args.new_base_url:
        return "no --new-base-url"
    if name == "cleanup" and args.keep_restore and args.keep_snapshot:
        return "--keep-restore and --keep-snapshot"
    return None


def main(argv=None):
    args = parse_args(argv)
    run_id = args.run_id or utc_now().strftime("%Y%m%dT%H%M%SZ")
    state_path = args.report_dir / ("import-rehearsal-%s.json" % run_id)
    # Workspace note naming: yyyy-mm-dd-slug.md, dated by the run.
    day = ("%s-%s-%s" % (run_id[:4], run_id[4:6], run_id[6:8]) if re.match(r"\d{8}T", run_id)
           else utc_now().strftime("%Y-%m-%d"))
    report_path = args.report_dir / ("%s-import-rehearsal-%s.md" % (day, run_id))
    global REPORT_TAIL
    REPORT_TAIL = args.report_lines
    state = json.loads(state_path.read_text()) if state_path.is_file() else {"runId": run_id, "steps": {}}
    state["snapshotKey"] = args.snapshot_key or state.get("snapshotKey") or "grid-rehearsal-%s.db" % run_id
    rehearsal = Rehearsal(args, state)

    def save():
        if args.dry_run:
            return
        state["updatedAt"] = iso()
        args.report_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        state_path.write_text(json.dumps(state, indent=2) + "\n")
        report_path.write_text(render_report(state))

    print("import rehearsal %s%s (state %s)" % (run_id, " DRY RUN" if args.dry_run else "", state_path), flush=True)
    failed = False
    try:
        rehearsal.preflight()
        for name, explicit in plan(args, state):
            previous = state["steps"].get(name) or {}
            if previous.get("status") == "ok" and not explicit:
                print("\n== %s: done in an earlier run (--from-step %s to repeat)" % (name, name), flush=True)
                continue
            reason = skip_reason(args, name)
            if reason:
                state["steps"][name] = {"status": "skipped", "detail": reason}
                print("\n== %s: skipped (%s)" % (name, reason), flush=True)
                continue
            print("\n== %s" % name, flush=True)
            started, clock = iso(), time.monotonic()
            record = state["steps"][name] = {"status": "running", "startedAt": started}
            save()
            try:
                step = getattr(rehearsal, "step_" + name)
                detail = step(explicit) if name == "snapshot" else step()
            except StepFailed as error:
                record.update({"status": "failed", "error": str(error), "seconds": time.monotonic() - clock})
                print("FAILED %s: %s" % (name, error), file=sys.stderr, flush=True)
                failed = True
                break
            record.update({"status": "ok", "detail": detail, "seconds": time.monotonic() - clock})
            print("ok %s: %s (%s)" % (name, detail, duration(record["seconds"])), flush=True)
            save()
    except StepFailed as error:  # preflight
        print("FAILED preflight: %s" % error, file=sys.stderr, flush=True)
        failed = True
    finally:
        save()
    if args.dry_run:
        print("\n(dry run: would write %s)" % report_path)
    else:
        print("\nreport: %s" % report_path)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
