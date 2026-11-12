import contextlib
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import rehearse_import as ri

FS_ARN = "arn:aws:elasticfilesystem:us-east-1:111111111111:file-system/fs-0123"
RP_OLD = "arn:aws:backup:us-east-1:111111111111:recovery-point:old"
RP_NEW = "arn:aws:backup:us-east-1:111111111111:recovery-point:new"
RESTORED = "/aws-backup-restore_2026-11-12T10-00-00-000Z/info-ersn/grid.db"

MANIFEST = {"format": "gridctl-snapshot/1", "createdAt": "2026-11-12T10:05:00Z", "size": 180000000,
            "sha256": "ab" * 32, "schemaVersion": 14, "quickCheck": "ok", "sourceJournalMode": "delete",
            "gridctlVersion": "abc123", "tables": {"events": 6000, "event_revisions": 29810}}

IMPORT_OK = [
    "source s3://sierra-grid-migration/grid-rehearsal-r1.db (180000000 bytes, sha256 abab)",
    "table                    source     target   copy s  check s  sha256 (target)",
    "events                     6000       6000     0.40     0.30  0123456789abcdef",
    "event_revisions           29810      29810     1.10     0.70  fedcba9876543210",
    "skipped event_geo (6000 rows)",
    "check firstIngestedAt            ok",
    "info  eventsContentHashDiffersFromBlob     3",
    "copy 1.5s, verify 1.0s, total 2.9s",
    "RESULT: OK",
]
VERIFY_OK = [l.replace("0.40", "0.00").replace("1.10", "0.00") for l in IMPORT_OK if not l.startswith("copy")] + []
REFUSED = ["gridctl import: sqliteimport: the target database is not empty (run with --allow-nonempty)"]


class FakeAWS:
    """subprocess.run stand-in for the AWS CLI (and apidiff): answers by
    command, records every call, and can be told how each task ends."""

    def __init__(self, task_logs=None, task_exit=None, manifest_present=False, writer="false", apidiff=0):
        self.calls, self.tasks = [], []
        self.task_logs = {"discover": ["RESTORED 180000000 10001:10001 644 /efs" + RESTORED],
                          "snapshot": ["snapshot ok"], "import": IMPORT_OK, "verify": VERIFY_OK,
                          "refuse": REFUSED, "cleanup": ["REMOVED /efs/aws-backup-restore_2026-11-12T10-00-00-000Z"],
                          **(task_logs or {})}
        self.task_exit = {"refuse": 1, **(task_exit or {})}
        self.manifest_present, self.writer, self.apidiff = manifest_present, writer, apidiff
        self.uploaded = False

    def kind(self, command):
        override = json.loads(command[command.index("--overrides") + 1])["containerOverrides"][0]["command"]
        if override[0] == "snapshot":
            return "snapshot"
        if override[0] == "verify":
            return "verify"
        if override[0] == "import":
            return "import" if "--report" in override else "refuse"
        return "cleanup" if override[0].startswith("rm ") else "discover"

    def __call__(self, cmd, **kwargs):
        self.calls.append(cmd)
        if cmd[0] != "aws":
            return SimpleNamespace(returncode=self.apidiff, stdout="== DIFF stats: /stats\n-a\n+b\n" if self.apidiff else "",
                                   stderr="")
        i = cmd.index("--region") + 2
        service, op = cmd[i], cmd[i + 1]
        ok = lambda body: SimpleNamespace(returncode=0, stdout=body if isinstance(body, str) else json.dumps(body), stderr="")
        if (service, op) == ("sts", "get-caller-identity"):
            account = ri.NEW_ACCOUNT_ID if "us-west-2" in cmd else "111111111111"
            return ok({"Account": account})
        if (service, op) == ("efs", "describe-file-systems"):
            return ok({"FileSystems": [{"FileSystemId": "fs-0123", "FileSystemArn": FS_ARN}]})
        if op == "list-recovery-points-by-backup-vault":
            return ok({"RecoveryPoints": [
                {"RecoveryPointArn": RP_OLD, "CreationDate": "2026-11-12T03:00:00-08:00", "Status": "COMPLETED"},
                {"RecoveryPointArn": RP_NEW, "CreationDate": "2026-11-12T06:00:00-08:00", "Status": "COMPLETED"},
                {"RecoveryPointArn": "partial", "CreationDate": "2026-11-12T09:00:00-08:00", "Status": "PARTIAL"}]})
        if op == "get-recovery-point-restore-metadata":
            return ok({"RestoreMetadata": {"Encrypted": "true", "CreationToken": "info-ersn-data",
                                           "PerformanceMode": "generalPurpose"}})
        if op == "start-restore-job":
            return ok({"RestoreJobId": "job-1"})
        if op == "describe-restore-job":
            return ok({"Status": "COMPLETED", "CreationDate": "2026-11-12T10:00:00Z",
                       "CompletionDate": "2026-11-12T10:03:30Z", "BackupSizeInBytes": 190000000})
        if (service, op) == ("ecs", "describe-services"):
            if "sierra-grid" in cmd:
                return ok({"services": [{"status": "ACTIVE", "taskDefinition": "arn:td/grid:3",
                                         "deployments": [{"taskDefinition": "arn:td/grid:3"}],
                                         "networkConfiguration": {"awsvpcConfiguration": {
                                             "subnets": ["subnet-new"], "securityGroups": ["sg-grid"]}}}]})
            return ok({"services": [{"status": "ACTIVE", "networkConfiguration": {"awsvpcConfiguration": {
                "subnets": ["subnet-old"], "securityGroups": ["sg-info-ersn"], "assignPublicIp": "ENABLED"}}}]})
        if op == "describe-task-definition":
            family = cmd[cmd.index("--task-definition") + 1]
            env = [{"name": "PF__GRID__WRITER__ENABLED", "value": self.writer}] if self.writer is not None else []
            return ok({"taskDefinition": {"taskDefinitionArn": "arn:td/" + family, "containerDefinitions": [{
                "name": "gridctl" if "grid:" not in family else "grid", "environment": env,
                "logConfiguration": {"options": {"awslogs-group": "lg-" + family, "awslogs-stream-prefix": "ecs"}}}]}})
        if op == "run-task":
            kind = self.kind(cmd)
            self.tasks.append(kind)
            return ok({"tasks": [{"taskArn": "arn:aws:ecs:r:a:task/c/%s-%d" % (kind, len(self.tasks))}]})
        if op == "describe-tasks":
            kind = self.tasks[-1]
            return ok({"tasks": [{"lastStatus": "STOPPED", "createdAt": "2026-11-12T10:00:00Z",
                                  "startedAt": "2026-11-12T10:00:30Z", "stoppedAt": "2026-11-12T10:01:00Z",
                                  "containers": [{"name": "gridctl", "exitCode": self.task_exit.get(kind, 0)}]}]})
        if op == "get-log-events":
            if "--next-token" in cmd:
                return ok({"events": [], "nextForwardToken": cmd[cmd.index("--next-token") + 1]})
            return ok({"events": [{"message": l} for l in self.task_logs[self.tasks[-1]]], "nextForwardToken": "t1"})
        if (service, op) == ("s3", "cp") and cmd[i + 3] == "-":
            if self.manifest_present or "snapshot" in self.tasks:
                return ok(json.dumps(MANIFEST))
            return SimpleNamespace(returncode=1, stdout="", stderr="An error occurred (404)")
        if service in ("s3", "s3api"):
            return ok("")
        raise AssertionError("unexpected command %s" % cmd)

    def ops(self):
        out = []
        for c in self.calls:
            if c[0] != "aws":
                out.append("apidiff")
                continue
            i = c.index("--region") + 2
            out.append(self.kind(c) if c[i + 1] == "run-task" else "%s %s" % (c[i], c[i + 1]))
        return out


class Base(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.report_dir = Path(self.dir.name) / "notes" / "local"

    def run_main(self, fake, *extra, run_id="20261112T100000Z"):
        argv = ["--old-profile", "old", "--new-profile", "new", "--report-dir", str(self.report_dir),
                "--run-id", run_id, *extra]
        out, err = io.StringIO(), io.StringIO()
        with patch("rehearse_import.subprocess.run", side_effect=fake) as run, \
                patch("rehearse_import.time.sleep"), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = ri.main(argv)
        self.out, self.err, self.run = out.getvalue(), err.getvalue(), run
        return code

    def state(self, run_id="20261112T100000Z"):
        return json.loads((self.report_dir / ("import-rehearsal-%s.json" % run_id)).read_text())

    def report(self, run_id="20261112T100000Z"):
        return (self.report_dir / ("2026-11-12-import-rehearsal-%s.md" % run_id)).read_text()


class FlowTests(Base):
    def test_full_run_orders_the_steps_and_passes(self):
        fake = FakeAWS()
        self.assertEqual(self.run_main(fake, "--new-base-url", "https://next.example"), 0, self.err)
        tasks = [o for o in fake.ops() if " " not in o]
        self.assertEqual(tasks, ["discover", "snapshot", "import", "verify", "refuse", "apidiff", "cleanup"])
        ops = fake.ops()
        self.assertLess(ops.index("backup start-restore-job"), ops.index("snapshot"))
        self.assertLess(ops.index("ecs describe-services"), ops.index("snapshot"))
        steps = self.state()["steps"]
        self.assertEqual([steps[s]["status"] for s in ri.STEPS],
                         ["ok", "ok", "skipped", "ok", "ok", "ok", "ok", "ok"])
        self.assertIn("**PASSED**", self.report())

    def test_restore_uses_latest_completed_point_and_same_file_system(self):
        fake = FakeAWS()
        self.run_main(fake, "--only", "restore")
        start = next(c for c in fake.calls if "start-restore-job" in c)
        self.assertEqual(start[start.index("--recovery-point-arn") + 1], RP_NEW)
        self.assertEqual(start[start.index("--iam-role-arn") + 1], "arn:aws:iam::111111111111:role/info-ersn-backup")
        self.assertEqual(start[start.index("--idempotency-token") + 1], "rehearsal-20261112T100000Z")
        meta = json.loads(start[start.index("--metadata") + 1])
        self.assertEqual(meta["file-system-id"], "fs-0123")
        self.assertEqual(meta["newFileSystem"], "false")
        self.assertEqual(json.loads(meta["ItemsToRestore"]), ["/info-ersn/grid.db"])
        self.assertNotIn("CreationToken", meta)
        self.assertEqual(self.state()["restore"]["path"], RESTORED)

    def test_snapshot_reads_the_restored_copy_into_the_old_bucket(self):
        fake = FakeAWS()
        self.run_main(fake, "--only", "restore,snapshot")
        run = next(c for c in fake.calls if "run-task" in c and fake.kind(c) == "snapshot")
        self.assertEqual(run[run.index("--cluster") + 1], "project-hub")
        override = json.loads(run[run.index("--overrides") + 1])["containerOverrides"][0]
        self.assertEqual(override["command"], ["snapshot", "--db", "/efs" + RESTORED, "--out",
                                               "s3://sierra-grid-migration/grid-rehearsal-20261112T100000Z.db"])
        self.assertIn({"name": "AWS_REGION", "value": "us-west-2"}, override["environment"])
        network = json.loads(run[run.index("--network-configuration") + 1])["awsvpcConfiguration"]
        self.assertEqual(network, {"subnets": ["subnet-old"], "securityGroups": ["sg-info-ersn"],
                                   "assignPublicIp": "ENABLED"})
        self.assertEqual(self.state()["snapshot"]["sha256"], "ab" * 32)

    def test_snapshot_refuses_a_path_that_is_not_a_restore(self):
        fake = FakeAWS()
        code = self.run_main(fake, "--only", "snapshot")
        self.assertEqual(code, 1)
        self.assertIn("no restored path", self.state()["steps"]["snapshot"]["error"])
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            ri.parse_args(["--dry-run", "--report-dir", str(self.report_dir), "--restored-path", "/info-ersn/grid.db"])

    def test_import_task_runs_in_the_new_account(self):
        fake = FakeAWS(manifest_present=True)
        self.assertEqual(self.run_main(fake, "--only", "import", "--allow-nonempty"), 0, self.err)
        run = next(c for c in fake.calls if "run-task" in c)
        self.assertEqual(run[run.index("--region") + 1], "us-west-2")
        self.assertEqual(run[run.index("--profile") + 1], "new")
        self.assertEqual(run[run.index("--task-definition") + 1], "sierra-gridctl")
        override = json.loads(run[run.index("--overrides") + 1])["containerOverrides"][0]
        self.assertEqual(override["command"], ["import", "--from", "s3://sierra-grid-migration/grid-rehearsal-"
                                               "20261112T100000Z.db", "--report", "/tmp/report.json", "--allow-nonempty"])
        network = json.loads(run[run.index("--network-configuration") + 1])["awsvpcConfiguration"]
        self.assertEqual(network["assignPublicIp"], "DISABLED")

    def test_fallback_transfer_imports_from_the_grid_bucket(self):
        fake = FakeAWS(manifest_present=True)
        self.assertEqual(self.run_main(fake, "--only", "transfer,import", "--new-bucket", "sierra-grid-copy"), 0, self.err)
        cps = [c for c in fake.calls if c[c.index("--region") + 2:c.index("--region") + 4] == ["s3", "cp"]]
        self.assertEqual([c[c.index("--profile") + 1] for c in cps], ["old", "old", "new", "new"])
        self.assertTrue(cps[3][-4].endswith(".manifest.json"), "the manifest goes up last")
        run = next(c for c in fake.calls if "run-task" in c)
        self.assertIn("s3://sierra-grid-copy/grid-rehearsal-20261112T100000Z.db", run[run.index("--overrides") + 1])


class GateTests(Base):
    def test_import_without_result_ok_fails_and_stops(self):
        fake = FakeAWS(task_logs={"import": IMPORT_OK[:-1]})
        self.assertEqual(self.run_main(fake, "--restored-path", RESTORED, "--from-step", "import"), 1)
        self.assertNotIn("verify", fake.ops())
        state = self.state()
        self.assertEqual(state["steps"]["import"]["status"], "failed")
        self.assertIn("RESULT: not printed", state["steps"]["import"]["error"])
        self.assertNotIn("verify", state["steps"])
        self.assertIn("**FAILED** at `import`", self.report())

    def test_nonzero_exit_fails_even_with_result_ok(self):
        fake = FakeAWS(task_exit={"verify": 1})
        self.assertEqual(self.run_main(fake, "--only", "verify"), 1)
        self.assertIn("verify failed: exit 1", self.state()["steps"]["verify"]["error"])

    def test_failed_import_names_the_problems(self):
        lines = IMPORT_OK[:-1] + ["RESULT: FAILED (1 problems)", "  - events: row stream SHA-256 differs"]
        fake = FakeAWS(task_logs={"import": lines}, task_exit={"import": 1})
        self.assertEqual(self.run_main(fake, "--only", "import"), 1)
        self.assertEqual(self.state()["import"]["report"]["problems"], ["events: row stream SHA-256 differs"])
        self.assertIn("- events: row stream SHA-256 differs", self.report())

    def test_enabled_writer_stops_the_import_before_any_task(self):
        fake = FakeAWS(writer="true")
        self.assertEqual(self.run_main(fake, "--only", "import"), 1)
        self.assertNotIn("import", fake.tasks)
        self.assertIn("PF__GRID__WRITER__ENABLED=false", self.state()["steps"]["import"]["error"])
        fake = FakeAWS(writer=None)
        self.assertEqual(self.run_main(fake, "--only", "import", run_id="r2"), 1, "unset means enabled")

    def test_refusal_must_be_the_not_empty_refusal(self):
        fake = FakeAWS(task_exit={"refuse": 0})
        self.assertEqual(self.run_main(fake, "--only", "refuse"), 1)
        self.assertIn("succeeded", self.state()["steps"]["refuse"]["error"])
        fake = FakeAWS(task_logs={"refuse": ["gridctl import: sqliteimport: another process holds the Grid writer lease"]})
        self.assertEqual(self.run_main(fake, "--only", "refuse", run_id="r2"), 1)

    def test_snapshot_failure_propagates(self):
        fake = FakeAWS(task_exit={"snapshot": 1}, task_logs={"snapshot": ["gridctl snapshot: quick_check failed"]})
        self.assertEqual(self.run_main(fake), 1)
        self.assertEqual(fake.tasks, ["discover", "snapshot"])
        self.assertIn("gridctl snapshot: quick_check failed", self.report())

    def test_aws_error_propagates(self):
        def broken(cmd, **kw):
            if "start-restore-job" in cmd:
                return SimpleNamespace(returncode=255, stdout="", stderr="AccessDeniedException: no")
            return FakeAWS()(cmd, **kw)
        self.assertEqual(self.run_main(broken), 1)
        self.assertIn("AccessDeniedException", self.state()["steps"]["restore"]["error"])

    def test_wrong_new_account_is_refused_before_anything(self):
        fake = FakeAWS()
        self.assertEqual(self.run_main(fake, "--new-account-id", "999999999999"), 1)
        self.assertEqual(fake.ops(), ["sts get-caller-identity", "sts get-caller-identity"])

    def test_apidiff_differences_are_findings_unless_strict(self):
        fake = FakeAWS(apidiff=1)
        self.assertEqual(self.run_main(fake, "--only", "apidiff", "--new-base-url", "https://n"), 0)
        self.assertEqual(self.state()["apidiff"]["differences"], 1)
        self.assertEqual(self.run_main(FakeAWS(apidiff=1), "--only", "apidiff", "--new-base-url", "https://n",
                                       "--apidiff-strict", run_id="r2"), 1)


class ResumeTests(Base):
    def test_rerun_skips_finished_steps(self):
        fake = FakeAWS()
        self.assertEqual(self.run_main(fake), 0, self.err)
        again = FakeAWS()
        self.assertEqual(self.run_main(again), 0)
        self.assertEqual(again.tasks, [])
        self.assertIn("restore: done in an earlier run", self.out)

    def test_from_step_repeats_finished_steps(self):
        self.run_main(FakeAWS())
        again = FakeAWS()
        self.assertEqual(self.run_main(again, "--from-step", "verify"), 0)
        self.assertEqual(again.tasks, ["verify", "refuse", "cleanup"])

    def test_uploaded_snapshot_is_not_taken_again(self):
        fake = FakeAWS(manifest_present=True)
        self.assertEqual(self.run_main(fake, "--restored-path", RESTORED, "--skip", "restore,cleanup"), 0, self.err)
        self.assertNotIn("snapshot", fake.tasks)
        self.assertTrue(self.state()["snapshot"]["reused"])

    def test_resumed_restore_waits_on_the_same_job(self):
        def interrupted(cmd, **kw):
            if "run-task" in cmd:
                return SimpleNamespace(returncode=255, stdout="", stderr="throttled")
            return FakeAWS()(cmd, **kw)
        self.assertEqual(self.run_main(interrupted), 1)
        fake = FakeAWS()
        self.assertEqual(self.run_main(fake, "--only", "restore"), 0)
        self.assertNotIn("backup start-restore-job", fake.ops())


class DryRunTests(Base):
    def test_dry_run_prints_every_command_and_runs_none(self):
        argv = ["--dry-run", "--report-dir", str(self.report_dir), "--new-base-url", "https://n"]
        with patch("rehearse_import.subprocess.run") as run, contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(ri.main(argv), 0)
        run.assert_not_called()
        self.assertFalse(self.report_dir.exists(), "a dry run writes nothing")
        text = out.getvalue()
        for needle in ("backup start-restore-job", "--task-definition sierra-gridctl-snapshot",
                       "--task-definition sierra-gridctl ", "logs get-log-events", "s3api delete-object", "apidiff.py"):
            self.assertIn(needle, text)

    def test_live_run_needs_both_profiles(self):
        with patch.dict("os.environ", {}, clear=False), contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit):
            ri.parse_args(["--report-dir", str(self.report_dir), "--old-profile", "old"])


class ReportDirTests(unittest.TestCase):
    def test_only_notes_local_under_notes(self):
        self.assertIsNone(ri.check_report_dir("/workspace/notes/local"))
        self.assertIsNone(ri.check_report_dir("/workspace/notes/local/rehearsals"))
        self.assertIn("notes/local", ri.check_report_dir("/workspace/notes"))
        self.assertIn("notes/local", ri.check_report_dir("/workspace/notes/grid"))
        self.assertIn("repository", ri.check_report_dir(ri.REPO / "scripts"))


class ReportTests(unittest.TestCase):
    def test_parse_gridctl_report(self):
        r = ri.parse_gridctl_report(IMPORT_OK)
        self.assertEqual(r["result"], "OK")
        self.assertEqual([t["table"] for t in r["tables"]], ["events", "event_revisions"])
        self.assertEqual(r["tables"][1]["target"], 29810)
        self.assertEqual(r["checks"], {"firstIngestedAt": "ok"})
        self.assertEqual(r["info"], {"eventsContentHashDiffersFromBlob": 3})
        self.assertEqual(r["skipped"], {"event_geo": 6000})
        self.assertEqual(r["totalSeconds"], 2.9)
        differs = ri.parse_gridctl_report(["events 1 2 0.10 0.20 0123456789abcdef  DIFFERS"])
        self.assertTrue(differs["tables"][0]["differs"])

    def test_render_report(self):
        state = {"runId": "r1", "updatedAt": "2026-11-12T11:00:00Z", "accounts": {"old": "111", "new": "222"},
                 "steps": {s: {"status": "ok", "startedAt": "2026-11-12T10:00:00Z", "seconds": 65, "detail": "d|x"}
                           for s in ri.STEPS},
                 "restore": {"recoveryPointArn": RP_NEW, "jobId": "job-1", "status": "COMPLETED", "seconds": 210,
                             "fileSystemId": "fs-0123", "path": RESTORED, "restoredSize": 1, "restoredOwner": "10001:10001"},
                 "snapshot": {"location": "s3://b/grid-x.db", "size": 180000000, "sha256": "ab" * 32,
                              "schemaVersion": 14, "quickCheck": "ok", "task": {"containerSeconds": 12,
                                                                                "logTail": ["snap"], "exitCode": 0}},
                 "import": {"writerCheck": "writer disabled on sierra-grid", "report": ri.parse_gridctl_report(IMPORT_OK),
                            "task": {"exitCode": 0, "containerSeconds": 40, "taskSeconds": 75, "logTail": IMPORT_OK}},
                 "verify": {"report": ri.parse_gridctl_report(VERIFY_OK), "task": {"exitCode": 0, "logTail": []}},
                 "refuse": {"refused": True}}
        text = ri.render_report(state)
        self.assertIn("Outcome: **PASSED**", text)
        self.assertIn("| import | ok | 2026-11-12T10:00:00Z | 1m05s | d\\|x |", text)
        self.assertIn("| event_revisions | 29810 | 29810 | 1.10 | 0.70 | `fedcba9876543210` |", text)
        self.assertIn("- gridctl: copy 1.5s, verify 1.0s, total 2.9s", text)
        self.assertIn("- RESULT: OK; task exit 0; container 40s, task 1m15s", text)
        self.assertIn("## Verify", text)
        self.assertIn("refused as expected", text)
        self.assertIn("job-1`, COMPLETED, 3m30s", text)
        self.assertIn("notes/local/", text)

    def test_render_incomplete_and_empty(self):
        self.assertIn("incomplete", ri.render_report({"runId": "r", "steps": {"restore": {"status": "ok"}}}))
        self.assertIn("incomplete", ri.render_report({}))


if __name__ == "__main__":
    unittest.main()
