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

import sys
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE))

import shaping  # noqa: E402  (path set above)


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


if __name__ == "__main__":
    unittest.main()
