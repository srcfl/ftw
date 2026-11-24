import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "easee_start_diagnostics", Path(__file__).with_name("easee-start-diagnostics.py")
)
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)


class EaseeStopDiagnosticsTest(unittest.TestCase):
    def test_pilot_sample_uses_source_time(self):
        now = collector.source_ms("2026-11-22T00:02:00Z")
        baseline = now - 120_000
        for stamp, status in (
            ("2026-11-22T00:00:30Z", "fresh"),
            ("2026-11-22T00:00:29Z", "stale"),
            ("2026-11-22T00:02:01Z", "future"),
            (None, "missing"),
        ):
            with self.subTest(status=status):
                row = {"recorded_at_ms": now, "easee": {
                    "pilot_a": 8, "pilot_observed_at": stamp,
                    "pilot_confirmed_at_ms": now,
                }}
                sample = collector.pilot_sample(row, baseline)
                self.assertEqual(sample["status"], status)
                self.assertEqual(sample["confirmed_a"], 8 if status == "fresh" else None)

    def test_paired_evidence_requires_fresh_power_and_pilot(self):
        now = collector.source_ms("2026-11-22T00:02:00Z")
        fresh = "2026-11-22T00:01:45Z"
        stale = "2026-11-22T00:01:44Z"
        row = {
            "recorded_at_ms": now,
            "allocation": {"start_observations": {
                "allocated_l1_a": {"value": 8, "source_at": fresh, "confirmed_at_ms": now},
            }},
            "pixii": {"meter_l1_a": {"value": 4, "updated_at": stale}},
        }
        for watts, power_at, pilot, pilot_at, state in (
            (0, fresh, 8, fresh, "positive_pilot_no_draw"),
            (0, fresh, 0, fresh, "zero_pilot_no_draw"),
            (0, fresh, 8, stale, "no_draw_pilot_unverified"),
            (0, stale, 8, fresh, "power_unverified"),
            (5400, fresh, 8, stale, "power_observed"),
        ):
            with self.subTest(state=state):
                row["easee"] = {
                    "w": watts, "control_power_available": True,
                    "control_power_observed_at": power_at,
                    "pilot_a": pilot, "pilot_observed_at": pilot_at,
                }
                evidence = collector.paired_evidence(row, now - 20_000, {"watts": 5520})
                self.assertEqual(evidence["state"], state)
                self.assertEqual(evidence["allocation_a"]["l1"]["status"], "fresh")
                self.assertEqual(evidence["allocation_a"]["l1"]["received_at_ms"], now)
                self.assertEqual(evidence["grid_a"]["l1"]["status"], "stale")
                self.assertEqual(evidence["grid_a"]["l2"]["status"], "missing")

    def test_new_zero_after_stable_charge_with_older_source_timestamp(self):
        start = collector.source_ms("2026-11-22T00:00:00Z")
        order = {"watts": 5520}
        prior = {
            "recorded_at_ms": start - 5000,
            "loadpoint": {"plugged_in": True, "commanded_w": 5520},
            "feedback": {"sent_w": 5520},
            "easee": {
                "connected": True, "charging": True, "w": 5455,
                "control_power_available": True,
                "control_power_observed_at": "2026-11-21T23:54:00Z",
            },
        }
        row = {
            "recorded_at_ms": start,
            "loadpoint": {"plugged_in": True},
            "easee": {
                "connected": True, "w": 0, "control_power_available": True,
                "control_power_observed_at": "2026-11-21T23:59:57Z",
                "pilot_a": 8, "pilot_observed_at": "2026-11-21T23:54:00Z",
                "pilot_confirmed_at_ms": start - 10_000,
                "op_mode": 4, "reason_no_current": 78,
            },
            "allocation": {"pilot_mode": "B"},
        }
        baseline = collector.recent_charging_baseline([prior], start)
        self.assertIsNotNone(baseline)
        collector.start_sample(row, baseline, None)
        row["pilot_sample"] = collector.pilot_sample(row, baseline)
        evidence = collector.no_draw_evidence(row, order, baseline)
        self.assertEqual(evidence["status"], "confirmed_no_draw")
        self.assertEqual((evidence["confirmed_w"], evidence["pilot_a"]), (0, 8))
        self.assertIsNone(collector.no_draw_evidence(row, {"watts": 0}, baseline))

        row["easee"]["control_power_observed_at"] = prior["easee"]["control_power_observed_at"]
        collector.start_sample(row, baseline, None)
        self.assertEqual(
            collector.no_draw_evidence(row, order, baseline)["status"], "power_unconfirmed"
        )
        row["easee"]["pilot_confirmed_at_ms"] = start - 91_000
        self.assertIsNone(collector.no_draw_evidence(row, order, baseline))


if __name__ == "__main__":
    unittest.main()
