#!/usr/bin/env python3
"""Unit tests for the pure parts of netem-lab.

Only the parts whose failures are SILENT. Standing a lab up is a manual check
by construction — that is what the tool is for — but a netem argv that lost
its `limit`, or an nsenter argv that lost `--preserve-credentials`, fails in a
way that names something other than the cause.

Run directly::

    python3 scripts/netem-lab/test_netem_lab.py
"""

from __future__ import annotations

import importlib.util
import os
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path

_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE))

import shaping  # noqa: E402  (path set above)
import nsutil  # noqa: E402

# netem-lab.py has a hyphen, so it cannot be imported by name. Same importlib
# route scripts/test_dummy_harness.py already uses on dummy-harness.py.
_LAB = _HERE / "netem-lab.py"
_spec = importlib.util.spec_from_file_location("netem_lab", _LAB)
netem_lab = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(netem_lab)


class TestProfiles(unittest.TestCase):
    def test_known_profile_sets_its_delay(self):
        k = shaping.resolve_knobs("wan-us")
        self.assertEqual(k.delay_ms, 75)
        self.assertIsNone(k.rate)

    def test_bufferbloat_has_a_rate_and_a_shallow_queue(self):
        k = shaping.resolve_knobs("bufferbloat")
        self.assertEqual(k.rate, "20mbit")
        self.assertEqual(k.limit, 2000)

    def test_unknown_profile_names_the_known_ones(self):
        with self.assertRaises(ValueError) as cm:
            shaping.resolve_knobs("wan-mars")
        self.assertIn("wan-us", str(cm.exception))

    def test_explicit_knob_overrides_only_that_knob(self):
        k = shaping.resolve_knobs("bufferbloat", delay_ms=10)
        self.assertEqual(k.delay_ms, 10)
        self.assertEqual(k.rate, "20mbit", "unrelated knob was clobbered")
        self.assertEqual(k.limit, 2000, "unrelated knob was clobbered")

    def test_absent_override_does_not_clear_a_profile_value(self):
        # None means "not given on the command line". rate is also legally
        # None, so absent and explicitly-none must not collapse.
        k = shaping.resolve_knobs("bufferbloat", rate=None, mtu=None)
        self.assertEqual(k.rate, "20mbit")

    def test_no_profile_carries_a_middlebox_knob(self):
        # A profile that quietly enabled one would make a stall caused by a
        # broken middlebox read as a congestion-control result.
        for name, k in shaping.PROFILES.items():
            with self.subTest(profile=name):
                self.assertIsNone(k.mtu)
                self.assertFalse(k.pmtu_blackhole)
                self.assertIsNone(k.conntrack_udp_timeout)
                self.assertTrue(k.nat)


class TestNetemArgv(unittest.TestCase):
    def test_limit_is_always_emitted(self):
        for name in [None, *shaping.PROFILES]:
            with self.subTest(profile=name):
                argv = shaping.netem_argv(shaping.resolve_knobs(name))
                self.assertIn("limit", argv)

    def test_limit_is_emitted_even_with_every_knob_off(self):
        argv = shaping.netem_argv(shaping.Knobs())
        self.assertEqual(argv, ["netem", "limit", str(shaping.DEFAULT_LIMIT)])

    def test_delay_and_jitter_render_as_tc_expects(self):
        k = shaping.resolve_knobs(None, delay_ms=75, jitter_ms=10)
        self.assertEqual(
            shaping.netem_argv(k),
            ["netem", "delay", "75ms", "10ms", "distribution", "normal",
             "limit", str(shaping.DEFAULT_LIMIT)],
        )

    def test_integral_values_lose_the_trailing_zero(self):
        k = shaping.resolve_knobs(None, delay_ms=75.0, loss_pct=0.5)
        argv = shaping.netem_argv(k)
        self.assertIn("75ms", argv)
        self.assertIn("0.5%", argv)

    def test_reorder_without_delay_is_rejected_here(self):
        # tc rejects it too, but three layers away and in its own words.
        k = shaping.resolve_knobs(None, reorder_pct=5)
        with self.assertRaises(ValueError):
            shaping.netem_argv(k)


class TestQdiscCommands(unittest.TestCase):
    def test_without_a_rate_netem_is_the_root(self):
        cmds = shaping.qdisc_commands("v0", shaping.resolve_knobs("wan-us"))
        self.assertEqual(len(cmds), 1)
        self.assertEqual(cmds[0][:6], ["tc", "qdisc", "replace", "dev", "v0", "root"])
        self.assertIn("netem", cmds[0])

    def test_with_a_rate_htb_is_the_root_and_netem_is_the_leaf(self):
        cmds = shaping.qdisc_commands("v0", shaping.resolve_knobs("thin"))
        self.assertEqual(len(cmds), 3)
        self.assertIn("htb", cmds[0])
        self.assertEqual(cmds[1][:3], ["tc", "class", "replace"])
        self.assertIn("2mbit", cmds[1])
        self.assertIn("netem", cmds[2])
        self.assertIn("parent", cmds[2])

    def test_every_command_uses_replace_so_shape_reuses_this_path(self):
        # `shape` on a live lab must be the same code path as `up`; `add`
        # would fail on the second call and `shape` would need its own.
        for k in (shaping.resolve_knobs("wan-us"), shaping.resolve_knobs("thin")):
            for cmd in shaping.qdisc_commands("v0", k):
                with self.subTest(cmd=cmd):
                    self.assertIn("replace", cmd)


class TestNsenterArgv(unittest.TestCase):
    def test_preserve_credentials_is_always_present(self):
        # Without it, entry fails with "setgroups: Operation not permitted",
        # which names neither the namespace nor the missing flag.
        argv = nsutil.nsenter_argv(111, 222)
        self.assertIn("--preserve-credentials", argv)

    def test_it_names_both_namespaces_by_pid(self):
        argv = nsutil.nsenter_argv(111, 222)
        self.assertEqual(argv[0], "nsenter")
        self.assertIn("--user=/proc/111/ns/user", argv)
        self.assertIn("--net=/proc/222/ns/net", argv)

    def test_the_user_namespace_pid_may_differ_from_the_net_one(self):
        argv = nsutil.nsenter_argv(111, 111)
        self.assertIn("--user=/proc/111/ns/user", argv)
        self.assertIn("--net=/proc/111/ns/net", argv)


class _FakeRun:
    """Stands in for subprocess.run so the preflight is tested without
    spawning anything."""

    def __init__(self, returncode=0, stderr=""):
        self.returncode = returncode
        self.stderr = stderr
        self.stdout = ""
        self.calls: list[list[str]] = []

    def __call__(self, argv, **kwargs):
        self.calls.append(list(argv))
        return self


class TestPreflight(unittest.TestCase):
    def test_non_linux_refuses_and_names_the_platform(self):
        with unittest.mock.patch("platform.system", return_value="Windows"):
            problems = nsutil.preflight_problems(run=_FakeRun())
        self.assertEqual(len(problems), 1)
        self.assertIn("Windows", problems[0])

    def test_non_linux_does_not_go_on_to_spawn_a_probe(self):
        fake = _FakeRun()
        with unittest.mock.patch("platform.system", return_value="Darwin"):
            nsutil.preflight_problems(run=fake)
        self.assertEqual(fake.calls, [], "probed anyway on a platform that cannot")

    def test_disabled_user_namespaces_are_reported_with_the_sysctl_name(self):
        with unittest.mock.patch("platform.system", return_value="Linux"), \
             unittest.mock.patch.object(nsutil, "_read_sysctl", return_value=0):
            problems = nsutil.preflight_problems(run=_FakeRun())
        self.assertTrue(any("user.max_user_namespaces" in p for p in problems))

    def test_a_failing_probe_suggests_modprobe(self):
        fake = _FakeRun(returncode=1, stderr="Unknown qdisc \"netem\"")
        with unittest.mock.patch("platform.system", return_value="Linux"), \
             unittest.mock.patch.object(nsutil, "_read_sysctl", return_value=10000), \
             unittest.mock.patch("shutil.which", return_value="/usr/bin/x"):
            problems = nsutil.preflight_problems(run=fake)
        self.assertEqual(len(problems), 1)
        self.assertIn("modprobe", problems[0])
        self.assertIn("netem", problems[0])

    def test_a_clean_host_reports_nothing(self):
        with unittest.mock.patch("platform.system", return_value="Linux"), \
             unittest.mock.patch.object(nsutil, "_read_sysctl", return_value=10000), \
             unittest.mock.patch("shutil.which", return_value="/usr/bin/x"):
            problems = nsutil.preflight_problems(run=_FakeRun())
        self.assertEqual(problems, [])

    def test_missing_tools_are_listed_before_the_probe_runs(self):
        fake = _FakeRun()
        with unittest.mock.patch("platform.system", return_value="Linux"), \
             unittest.mock.patch.object(nsutil, "_read_sysctl", return_value=10000), \
             unittest.mock.patch("shutil.which", return_value=None):
            problems = nsutil.preflight_problems(run=fake)
        self.assertTrue(any("missing from PATH" in p for p in problems))
        self.assertEqual(fake.calls, [], "probed with the tools it needs absent")


class TestState(unittest.TestCase):
    """The state file is the only record of what to kill. Its loss leaves a
    server, a runner and three namespace holders alive with nothing naming
    them."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self._env = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(self._env)))
        os.environ["TMPDIR"] = self.tmp.name

    def test_round_trip_keeps_every_pid(self):
        st = {
            "USERNS_PID": 11, "SRV_PID": 22, "CLI_PID": 33,
            "SERVER_PID": 44, "RUNNER_PID": 55,
            "CID": "udp:10.90.0.2:9000-*", "HARNESS_PSK": "p",
            "TMP": self.tmp.name, "REPO": self.tmp.name,
            "SUBNET_A": "10.90.0.0/24", "SUBNET_B": "10.91.0.0/24",
            "PROFILE": "wan-us",
        }
        netem_lab.write_state("t", st)
        self.assertEqual(netem_lab.read_state("t"), st)

    def test_reading_an_absent_instance_is_none_not_an_error(self):
        self.assertIsNone(netem_lab.read_state("nope"))

    def test_down_on_an_absent_instance_is_a_no_op(self):
        self.assertEqual(netem_lab.cmd_down("nope"), 0)

    def test_state_dir_ignores_TMP(self):
        # dummy-harness.py's `env` exports TMP set to its own instance dir, and
        # tempfile.gettempdir() consults TMP first. Sourcing that in a shell
        # once made every later call resolve its state dir inside the instance,
        # `down` report nothing to stop, and a server and runner leak.
        os.environ["TMP"] = "/definitely/not/here"
        self.assertNotIn("definitely", str(netem_lab.state_dir()))


class TestBenchSummary(unittest.TestCase):
    """The summary exists to stop a single number being read as evidence, so
    the resolution figure is the part that must not be wrong."""

    def test_one_run_resolves_nothing_and_says_so(self):
        out = netem_lab._bench_summary([9.8])
        self.assertIn("resolves nothing", out)

    def test_no_runs_does_not_divide_by_zero(self):
        self.assertIn("no successful runs", netem_lab._bench_summary([]))

    def test_resolution_shrinks_as_runs_are_added(self):
        # Same spread, more samples: the difference a run set can distinguish
        # gets smaller. If this ever inverts, the figure is lying in the
        # direction that matters.
        spread = [8.0, 12.0, 9.0, 11.0, 10.0, 10.5]
        few = netem_lab._bench_summary(spread[:3])
        many = netem_lab._bench_summary(spread * 4)
        pct = lambda s: float(s.split("larger than about ")[1].split("%")[0])
        self.assertLess(pct(many), pct(few))

    def test_resolution_grows_with_noise(self):
        tight = netem_lab._bench_summary([10.0, 10.1, 9.9, 10.0, 10.05])
        loose = netem_lab._bench_summary([6.8, 12.7, 10.3, 7.5, 11.5])
        pct = lambda s: float(s.split("larger than about ")[1].split("%")[0])
        self.assertGreater(pct(loose), pct(tight))

    def test_the_measured_session_numbers_are_not_resolvable_at_10_percent(self):
        # The six runs that ended the investigation. Whatever else changes,
        # this set must not claim it could have detected a 10% improvement.
        out = netem_lab._bench_summary([6.76, 10.26, 10.13, 11.46, 12.70, 7.52])
        pct = float(out.split("larger than about ")[1].split("%")[0])
        self.assertGreater(pct, 10.0)
        self.assertIn("spread=1.88x", out)


class TwoPathLeg(unittest.TestCase):
    """`up --paths 2` and the `path` / `failover` verbs.

    Only the pure parts: building the leg needs namespaces, and the measurement
    needs a running harness. The properties worth pinning here are the ones a
    typo would silently invert — which devices a leg names, and that leg 1 stays
    the devices the one-path lab has always used.
    """

    def test_leg_1_is_the_original_srv_devices(self):
        # A renamed leg-1 device would leave every existing shaping command
        # pointed at an interface that no longer exists.
        self.assertEqual(netem_lab.leg_devs(1), netem_lab._DEV["srv"])

    def test_leg_2_is_a_distinct_pair(self):
        srv2, rtr2 = netem_lab.leg_devs(2)
        srv1, rtr1 = netem_lab.leg_devs(1)
        self.assertNotIn(srv2, (srv1, rtr1))
        self.assertNotIn(rtr2, (srv1, rtr1))

    def test_every_device_name_fits_the_kernel_limit(self):
        for leg in (1, 2):
            for dev in netem_lab.leg_devs(leg):
                self.assertLessEqual(len(dev.encode()), 15, dev)

    def test_an_out_of_range_leg_is_refused(self):
        with self.assertRaises(SystemExit):
            netem_lab.leg_devs(3)

    def test_the_second_leg_addresses_differ_from_the_first_two(self):
        a = netem_lab._addrs("10.90.0.0/24")
        c = netem_lab._addrs("10.92.0.0/24")
        self.assertNotEqual(a[0], c[0])
        self.assertNotEqual(a[1], c[1])


class RunnerRowParse(unittest.TestCase):
    """The regex `failover` measures with. It reads a REAL `ls` runner row —
    the shape recorded here is what the server printed during the 63s
    measurement — so a row-format change breaks the measurement loudly instead
    of reporting "never moved"."""

    ROW = ("RUNNERS\n"
           "  Idle     host=h  os=linux  tasks=0/4  agent=python  "
           "roots=/tmp/r  id=8faf554ecb4d4d0e9a7b1d2e3f405162  "
           "cid=ws:10.90.0.1:51712-31023\n"
           "TASKS\n  (none)\n")

    def test_reads_the_id_and_the_conn_id(self):
        st = {"SERVER_PORT": 1, "USERNS_PID": 1, "SRV_PID": 1, "CLI_PID": 1}
        with unittest.mock.patch.object(
                netem_lab, "ns_run",
                return_value=unittest.mock.Mock(stdout=self.ROW, returncode=0)):
            with unittest.mock.patch.object(netem_lab.dh.daemon, "bin_path",
                                            return_value="harness-cli"):
                got = netem_lab._runner_row(st)
        self.assertEqual(got, ("8faf554ecb4d4d0e9a7b1d2e3f405162",
                               "ws:10.90.0.1:51712-31023"))

    def test_no_runner_row_is_none_not_a_crash(self):
        st = {"SERVER_PORT": 1, "USERNS_PID": 1, "SRV_PID": 1, "CLI_PID": 1}
        with unittest.mock.patch.object(
                netem_lab, "ns_run",
                return_value=unittest.mock.Mock(stdout="RUNNERS\nTASKS\n",
                                                returncode=0)):
            with unittest.mock.patch.object(netem_lab.dh.daemon, "bin_path",
                                            return_value="harness-cli"):
                self.assertIsNone(netem_lab._runner_row(st))

    def test_the_task_section_cannot_supply_the_match(self):
        # A task row also carries id=; splitting at TASKS is what keeps a
        # runnerless lab from reading a task as its runner.
        rows = ("RUNNERS\n  (none)\nTASKS\n  Running  "
                "id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  cid=ws:1.2.3.4:5-6\n")
        st = {"SERVER_PORT": 1, "USERNS_PID": 1, "SRV_PID": 1, "CLI_PID": 1}
        with unittest.mock.patch.object(
                netem_lab, "ns_run",
                return_value=unittest.mock.Mock(stdout=rows, returncode=0)):
            with unittest.mock.patch.object(netem_lab.dh.daemon, "bin_path",
                                            return_value="harness-cli"):
                self.assertIsNone(netem_lab._runner_row(st))


if __name__ == "__main__":
    unittest.main()
