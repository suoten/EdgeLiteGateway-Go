"""
Unit tests for InferenceStatsCollector.
"""
import threading


from server import InferenceStatsCollector


class TestInferenceStatsCollector:
    """Tests for the InferenceStatsCollector class."""

    def test_initial_state(self):
        c = InferenceStatsCollector()
        snapshot = c.get_snapshot()
        assert snapshot["total_calls"] == 0
        assert snapshot["total_errors"] == 0
        assert snapshot["avg_latency_ms"] == 0.0

    def test_record_inference_success(self):
        c = InferenceStatsCollector()
        c.record_inference("model1", 100, "success")
        snapshot = c.get_snapshot()
        assert snapshot["total_calls"] == 1
        assert snapshot["total_errors"] == 0
        assert snapshot["avg_latency_ms"] == 100.0

    def test_record_inference_error(self):
        c = InferenceStatsCollector()
        c.record_inference("model1", 50, "error")
        snapshot = c.get_snapshot()
        assert snapshot["total_calls"] == 1
        assert snapshot["total_errors"] == 1

    def test_avg_latency_precision(self):
        """avg_latency_ms should use float division, not integer division."""
        c = InferenceStatsCollector()
        c.record_inference("m1", 10, "success")
        c.record_inference("m1", 15, "success")
        c.record_inference("m1", 20, "success")
        snapshot = c.get_snapshot()
        assert snapshot["avg_latency_ms"] == round(45 / 3, 2)

    def test_model_distribution(self):
        c = InferenceStatsCollector()
        c.record_inference("model_a", 10, "success")
        c.record_inference("model_b", 20, "success")
        c.record_inference("model_a", 30, "success")
        snapshot = c.get_snapshot()
        assert snapshot["model_distribution"]["model_a"] == 2
        assert snapshot["model_distribution"]["model_b"] == 1

    def test_recent_latencies(self):
        c = InferenceStatsCollector()
        for i in range(150):
            c.record_inference("m1", i, "success")
        snapshot = c.get_snapshot()
        assert len(snapshot["recent_latencies"]) <= 100

    def test_get_model_stats_not_found(self):
        c = InferenceStatsCollector()
        assert c.get_model_stats("nonexistent") is None

    def test_get_model_stats(self):
        c = InferenceStatsCollector()
        c.record_inference("m1", 10, "success")
        c.record_inference("m1", 30, "success")
        c.record_inference("m1", 5, "error")
        stats = c.get_model_stats("m1")
        assert stats is not None
        assert stats["call_count"] == 3
        assert stats["error_count"] == 1
        assert stats["max_latency_ms"] == 30
        assert stats["min_latency_ms"] == 5

    def test_thread_safety(self):
        c = InferenceStatsCollector()
        errors = []

        def worker():
            try:
                for i in range(100):
                    c.record_inference("m1", i, "success")
            except Exception as e:
                errors.append(e)

        threads = [threading.Thread(target=worker) for _ in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert len(errors) == 0
        snapshot = c.get_snapshot()
        assert snapshot["total_calls"] == 400
