"""Tests for plot_results.py on tiny fixtures (no matplotlib needed). Run: python3 bench/test_plot_results.py"""
import csv
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import plot_results as p  # noqa: E402

COLD_HEAD = ["mode", "run", "spawn_ms", "load_ms", "fc_load_ms", "connect_ms", "agent_ms", "attempts", "faults_agent", "faults_call",
             "ready_ms", "upload_ms", "import_ms", "invoke_ms", "first_response_ms"]
DENS_HEAD = ["scenario", "vms", "idle", "pss_bytes", "rss_bytes", "available_bytes", "pss_per_vm", "rss_per_vm", "host_per_vm",
             "probe_p50_ms", "probe_p99_ms", "probe_failed"]
K6_HEAD = ["metric_name", "timestamp", "metric_value", "check", "error", "error_code", "expected_response", "group", "method",
           "name", "proto", "scenario", "service", "status", "subproto", "tls_version", "url", "extra_tags", "metadata"]


def write_csv(path, head, rows):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(head)
        w.writerows(rows)


def cold_row(mode, ready, first):
    row = {k: "0" for k in COLD_HEAD}
    row.update(mode=mode, run="1", ready_ms=str(ready), first_response_ms=str(first))
    return [row[k] for k in COLD_HEAD]


class PercentileTest(unittest.TestCase):
    def test_nearest_rank_like_go(self):
        v = list(range(1, 101))
        self.assertEqual(p.pct(v, 50), 50)
        self.assertEqual(p.pct(v, 99), 99)
        self.assertEqual(p.pct([7], 99), 7)
        self.assertTrue(p.pct([], 50) != p.pct([], 50))  # nan


class LoadingTest(unittest.TestCase):
    def test_coldstart_and_density_loaders(self):
        with tempfile.TemporaryDirectory() as d:
            write_csv(os.path.join(d, "coldstart-python", "coldstart-1.csv"), COLD_HEAD,
                      [cold_row("restore", 40, 90), cold_row("restore", 60, 110), cold_row("function", 45, 58)])
            write_csv(os.path.join(d, "density-one-function-1.csv"), DENS_HEAD,
                      [["one-function", "20", "20", "1", "2", "3", "6291456", "20971520", "7340032", "1.5", "3.0", "0"]])
            data = p.load_coldstart(d)
            self.assertEqual(len(data["python"]["restore"]), 2)
            self.assertEqual(len(data["python"]["function"]), 1)
            self.assertIn("one-function", p.load_density(d))


class K6Test(unittest.TestCase):
    def rows(self):
        out = []
        # 6 slices of 10 s, 2*(i+1) calls per slice, latency (i+1) ms; one failed call in the last slice.
        for i in range(6):
            for j in range(2 * (i + 1)):
                out.append((i * 10 + j * 0.1, "http_req_duration", float(i + 1), "200"))
        out.append((59.5, "http_req_duration", 500.0, "502"))
        return out

    def test_levels(self):
        lv = p.k6_levels(self.rows())
        self.assertEqual([l["vus"] for l in lv], [1, 2, 4, 8, 16, 32])
        self.assertEqual(lv[0]["p50"], 1.0)
        self.assertEqual(lv[5]["p50"], 6.0)
        self.assertEqual(lv[5]["failed"], 1)
        self.assertGreater(lv[5]["calls_per_s"], lv[0]["calls_per_s"])

    def test_by_start_and_rate(self):
        rows = [(0.0, "beevm_call_ms_warm", 2.0, ""), (1.0, "beevm_call_ms_warm", 4.0, ""), (1.0, "beevm_call_ms_restore", 80.0, ""),
                (0.0, "http_req_duration", 2.0, "200"), (2.0, "http_req_duration", 3.0, "200")]
        by = p.k6_by_start(rows)
        self.assertEqual(sorted(by["warm"]), [2.0, 4.0])
        self.assertEqual(by["restore"], [80.0])
        self.assertAlmostEqual(p.k6_rate(rows), 1.0)

    def test_iso_timestamps(self):
        self.assertGreater(p.parse_ts("2026-11-17T10:00:00Z"), 1.7e9)


class ReportTest(unittest.TestCase):
    def test_report_says_over_50_honestly(self):
        data = {"binary": {"restore": [{"ready_ms": "45", "first_response_ms": "90"}, {"ready_ms": "70", "first_response_ms": "120"}]}}
        g = p.grading(data)
        self.assertIn("p50 under 50 ms", g)
        self.assertIn("p99 OVER 50 ms", g)

    def test_report_with_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            text = p.build_report(d, {}, {}, [], None, [])
        for part in ("No cold start data", "No k6 warm run", "No density data", "make test-isolation"):
            self.assertIn(part, text)

    def test_report_includes_isolation_without_its_title(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "isolation.md"), "w") as f:
                f.write("# Isolation results" + chr(10) + "| Attack | Pass |" + chr(10))
            text = p.build_report(d, {}, {}, [], None, [])
        self.assertIn("| Attack | Pass |", text)
        self.assertNotIn("# Isolation results", text)

    def test_density_table(self):
        rows = {"one-function": [{"vms": "100", "pss_per_vm": "6291456", "rss_per_vm": "20971520", "host_per_vm": "7340032",
                                  "probe_p50_ms": "1.5", "probe_p99_ms": "3.0"}]}
        t = p.density_table(rows)
        self.assertIn("| one-function | 100 | 6.0 | 20.0 | 7.0 | 1.5 | 3.0 |", t)


if __name__ == "__main__":
    unittest.main()
