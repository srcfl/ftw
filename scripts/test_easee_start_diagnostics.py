import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "easee_start_diagnostics", Path(__file__).with_name("easee-start-diagnostics.py")
)
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)


class EaseeStopDiagnosticsTest(unittest.TestCase):
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
