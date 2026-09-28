"""
Unit tests for SelfLearningModel and SelfLearningManager.
"""
import math
import threading
import time

import pytest

from server import SelfLearningModel, SelfLearningManager


class TestSelfLearningModel:
    """Tests for the SelfLearningModel class."""

    def test_initialization(self):
        m = SelfLearningModel("device1", "point1", window_size=50)
        assert m.device_id == "device1"
        assert m.point_name == "point1"
        assert m.window_size == 50
        assert m.total_samples == 0
        assert m.anomaly_count == 0

    def test_initialization_default_window(self):
        m = SelfLearningModel("d", "p")
        assert m.window_size == 100

    def test_initialization_invalid_window(self):
        m = SelfLearningModel("d", "p", window_size=0)
        assert m.window_size == 100
        m2 = SelfLearningModel("d", "p", window_size=-5)
        assert m2.window_size == 100

    def test_add_sample_below_threshold(self):
        """Samples below 10 should not trigger anomaly detection."""
        m = SelfLearningModel("d", "p", window_size=100)
        for i in range(9):
            result = m.add_sample(float(i))
            assert result is False
        assert m.total_samples == 9

    def test_add_sample_normal_values(self):
        """Normal values should not trigger anomaly."""
        m = SelfLearningModel("d", "p", window_size=100)
        for i in range(20):
            result = m.add_sample(50.0)
            assert result is False

    def test_add_sample_anomaly_detection(self):
        """A sudden spike should trigger anomaly."""
        m = SelfLearningModel("d", "p", window_size=100)
        # Train with normal values
        for i in range(20):
            m.add_sample(50.0)
        # Add an anomalous value
        result = m.add_sample(500.0)
        assert result is True
        assert m.anomaly_count == 1

    def test_window_size_enforcement(self):
        """Values list should not exceed window_size."""
        m = SelfLearningModel("d", "p", window_size=15)
        for i in range(30):
            m.add_sample(float(i))
        stats = m.get_stats()
        assert stats["window_size"] <= 15

    def test_ewma_initialization_with_zero(self):
        """EWMA should correctly initialize even when first value is 0."""
        m = SelfLearningModel("d", "p", window_size=100)
        m.add_sample(0.0)
        m.add_sample(0.0)
        m.add_sample(10.0)
        # EWMA should not be stuck at 0 after initialization
        assert m.ewma > 0

    def test_predict(self):
        m = SelfLearningModel("d", "p", window_size=100)
        m.add_sample(42.0)
        assert m.predict() == 42.0

    def test_get_confidence_low_samples(self):
        m = SelfLearningModel("d", "p", window_size=100)
        m.add_sample(10.0)
        assert m.get_confidence() == 0

    def test_get_confidence_with_samples(self):
        m = SelfLearningModel("d", "p", window_size=100)
        for i in range(15):
            m.add_sample(50.0)
        conf = m.get_confidence()
        assert 0 <= conf <= 1.0

    def test_get_stats(self):
        m = SelfLearningModel("d", "p", window_size=100)
        for i in range(15):
            m.add_sample(float(i))
        stats = m.get_stats()
        assert stats["device_id"] == "d"
        assert stats["point_name"] == "p"
        assert stats["total_samples"] == 15
        assert "mean" in stats
        assert "std_dev" in stats
        assert "ewma" in stats
        assert "confidence" in stats

    def test_reset(self):
        m = SelfLearningModel("d", "p", window_size=100)
        for i in range(15):
            m.add_sample(float(i))
        m.reset()
        assert m.total_samples == 0
        assert m.anomaly_count == 0
        assert len(m.values) == 0

    def test_set_threshold(self):
        m = SelfLearningModel("d", "p", window_size=100)
        m.set_threshold(5.0)
        assert m.threshold == 5.0

    def test_thread_safety(self):
        """Concurrent add_sample calls should not crash."""
        m = SelfLearningModel("d", "p", window_size=200)
        errors = []

        def worker():
            try:
                for i in range(50):
                    m.add_sample(float(i))
            except Exception as e:
                errors.append(e)

        threads = [threading.Thread(target=worker) for _ in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert len(errors) == 0
        assert m.total_samples == 200


class TestSelfLearningManager:
    """Tests for the SelfLearningManager class."""

    def test_get_or_create(self):
        mgr = SelfLearningManager()
        m1 = mgr.get_or_create("d1", "p1")
        assert m1 is not None
        m2 = mgr.get_or_create("d1", "p1")
        assert m1 is m2

    def test_get_model_not_found(self):
        mgr = SelfLearningManager()
        assert mgr.get_model("nonexistent", "point") is None

    def test_get_all_stats_empty(self):
        mgr = SelfLearningManager()
        assert mgr.get_all_stats() == []

    def test_get_all_stats(self):
        mgr = SelfLearningManager()
        m1 = mgr.get_or_create("d1", "p1")
        m1.add_sample(10.0)
        m2 = mgr.get_or_create("d2", "p2")
        m2.add_sample(20.0)
        stats = mgr.get_all_stats()
        assert len(stats) == 2

    def test_thread_safety(self):
        mgr = SelfLearningManager()
        errors = []

        def worker(idx):
            try:
                for i in range(20):
                    m = mgr.get_or_create(f"d{idx}", f"p{i}")
                    m.add_sample(float(i))
            except Exception as e:
                errors.append(e)

        threads = [threading.Thread(target=worker, args=(i,)) for i in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert len(errors) == 0
