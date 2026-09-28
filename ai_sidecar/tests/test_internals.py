"""
Comprehensive unit tests for server internals: helpers, wrappers, stats, logging.

These tests target code paths not covered by the API integration tests,
including helper functions, OnnxModelWrapper internals, InferenceStatsCollector,
JsonFormatter, _setup_logging, and create_app route registration.
"""
import asyncio
import json
import logging
import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import MagicMock, patch

import numpy as np
import pytest
import pytest_asyncio

from server import (
    AISidecarServer,
    DEFAULT_ANOMALY_THRESHOLD,
    DEFAULT_EWMA_ALPHA,
    DEFAULT_HOST,
    DEFAULT_PORT,
    DEFAULT_SCHEDULED_INTERVAL,
    DEFAULT_WINDOW_SIZE,
    InferenceStatsCollector,
    JsonFormatter,
    MIN_SAMPLES_FOR_ANOMALY,
    OnnxModelWrapper,
    PRESET_MODELS,
    PROVIDER_CPU,
    PROVIDER_CUDA,
    PROVIDER_OPENVINO,
    STATUS_ACTIVE,
    STATUS_ERROR,
    STATUS_INACTIVE,
    STATUS_LOADING,
    STATUS_UNAVAILABLE,
    VALID_PROVIDERS,
    _error_response,
    _metric_inc,
    _metric_set,
    _success_response,
    _setup_logging,
    create_app,
)


# ═══════════════════════════════════════════════════════════════════════
#  Helper function tests
# ═══════════════════════════════════════════════════════════════════════


class TestErrorResponse:
    """Tests for _error_response helper."""

    def test_basic_error(self):
        resp = _error_response("ERR_TEST", "Test error")
        assert resp.status == 200
        data = json.loads(resp.body)
        assert data["success"] is False
        assert data["error_code"] == "ERR_TEST"
        assert data["error_message"] == "Test error"

    def test_error_with_status(self):
        resp = _error_response("ERR_TEST", "Bad request", status=400)
        assert resp.status == 400

    def test_error_with_extra_fields(self):
        resp = _error_response(
            "ERR_TEST", "Error", model_id="m1", status_text="error"
        )
        data = json.loads(resp.body)
        assert data["model_id"] == "m1"
        assert data["status_text"] == "error"


class TestSuccessResponse:
    """Tests for _success_response helper."""

    def test_basic_success(self):
        resp = _success_response()
        assert resp.status == 200
        data = json.loads(resp.body)
        assert data["success"] is True

    def test_success_with_fields(self):
        resp = _success_response(model_info={"id": "m1"}, new_version="v2")
        data = json.loads(resp.body)
        assert data["model_info"]["id"] == "m1"
        assert data["new_version"] == "v2"

    def test_success_with_success_false(self):
        resp = _success_response(success=False)
        data = json.loads(resp.body)
        assert data["success"] is False


class TestMetrics:
    """Tests for _metric_inc and _metric_set."""

    def test_metric_inc(self):
        _metric_inc("test_counter")
        _metric_inc("test_counter")
        # Should not raise
        assert True

    def test_metric_inc_with_value(self):
        _metric_inc("test_sum", 42)
        _metric_inc("test_sum", 8)
        # Metrics should accumulate without error
        assert True

    def test_metric_set(self):
        _metric_set("test_gauge", 100.0)
        _metric_set("test_gauge", 200.0)
        # Should not raise
        assert True


# ═══════════════════════════════════════════════════════════════════════
#  OnnxModelWrapper tests
# ═══════════════════════════════════════════════════════════════════════


class TestOnnxModelWrapper:
    """Tests for OnnxModelWrapper model lifecycle."""

    def test_initialization(self):
        w = OnnxModelWrapper(
            model_id="test-model",
            model_name="Test",
            model_version="v1.0.0",
            model_type="anomaly",
            is_preset=True,
            model_path="/fake/path",
        )
        assert w.model_id == "test-model"
        assert w.model_name == "Test"
        assert w.model_version == "v1.0.0"
        assert w.status == STATUS_INACTIVE
        assert w.session is None
        assert w.inference_count == 0
        assert w.error_count == 0

    def test_to_dict(self):
        w = OnnxModelWrapper(
            model_id="m1",
            model_name="Model1",
            model_version="v1",
            model_type="type1",
            is_preset=False,
            model_path="/path",
        )
        d = w.to_dict()
        assert d["model_id"] == "m1"
        assert d["model_name"] == "Model1"
        assert d["model_version"] == "v1"
        assert d["status"] == STATUS_INACTIVE
        assert d["is_preset"] is False

    @pytest.mark.asyncio
    async def test_load_invalid_path(self):
        """Loading a non-existent model file should set status to error."""
        w = OnnxModelWrapper(
            model_id="m1",
            model_name="M1",
            model_version="v1",
            model_type="",
            is_preset=False,
            model_path="/nonexistent/path/model.onnx",
        )
        await w.load()
        assert w.status == STATUS_ERROR
        assert w.session is None

    @pytest.mark.asyncio
    async def test_unload(self):
        """Unload should clear session and set status."""
        w = OnnxModelWrapper(
            model_id="m1",
            model_name="M1",
            model_version="v1",
            model_type="",
            is_preset=False,
            model_path="/fake/path",
        )
        w.status = STATUS_ACTIVE
        w.session = MagicMock()
        await w.unload()
        assert w.session is None
        assert w.status == STATUS_INACTIVE

    @pytest.mark.asyncio
    async def test_unload_already_none(self):
        """Unload when session is already None should be safe."""
        w = OnnxModelWrapper(
            model_id="m1",
            model_name="M1",
            model_version="v1",
            model_type="",
            is_preset=False,
            model_path="/fake",
        )
        await w.unload()
        assert w.session is None

    def test_get_version_history_empty(self):
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        history = w.get_version_history()
        assert isinstance(history, list)

    def test_record_version_via_sidecar(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        initial = len(wrapper.get_version_history())
        sidecar._record_version(wrapper, "manual_note")
        assert len(wrapper.get_version_history()) == initial + 1


# ═══════════════════════════════════════════════════════════════════════
#  InferenceStatsCollector tests
# ═══════════════════════════════════════════════════════════════════════


class TestInferenceStatsCollector:
    """Tests for InferenceStatsCollector."""

    def test_record_inference_success(self):
        s = InferenceStatsCollector()
        s.record_inference("m1", 50, "success")
        snapshot = s.get_snapshot()
        assert snapshot["total_calls"] == 1
        assert snapshot["total_errors"] == 0

    def test_record_inference_error(self):
        s = InferenceStatsCollector()
        s.record_inference("m1", 50, "error")
        snapshot = s.get_snapshot()
        assert snapshot["total_calls"] == 1
        assert snapshot["total_errors"] == 1

    def test_model_stats(self):
        s = InferenceStatsCollector()
        s.record_inference("m1", 10, "success")
        s.record_inference("m1", 20, "success")
        s.record_inference("m1", 30, "error")
        stats = s.get_model_stats("m1")
        assert stats is not None
        assert stats["call_count"] == 3
        assert stats["error_count"] == 1

    def test_model_stats_not_found(self):
        s = InferenceStatsCollector()
        assert s.get_model_stats("nonexistent") is None

    def test_avg_latency(self):
        s = InferenceStatsCollector()
        s.record_inference("m1", 100, "success")
        s.record_inference("m1", 200, "success")
        snapshot = s.get_snapshot()
        assert snapshot["avg_latency_ms"] == 150.0


# ═══════════════════════════════════════════════════════════════════════
#  AISidecarServer internal methods tests
# ═══════════════════════════════════════════════════════════════════════


class TestAISidecarInternals:
    """Tests for AISidecarServer internal methods."""

    @pytest.mark.asyncio
    async def test_resolve_shape_dimensions(self):
        assert AISidecarServer._resolve_shape_dimensions([1, 10]) == [1, 10]
        assert AISidecarServer._resolve_shape_dimensions([-1, 10]) == [-1, 10]
        assert AISidecarServer._resolve_shape_dimensions(["batch", 5]) == [-1, 5]
        assert AISidecarServer._resolve_shape_dimensions(None) == [1]
        assert AISidecarServer._resolve_shape_dimensions([]) == [1]

    @pytest.mark.asyncio
    async def test_is_batch_mode(self):
        arr_multi = np.array([1.0, 2.0, 3.0], dtype=np.float32)
        arr_single = np.array([1.0], dtype=np.float32)
        assert AISidecarServer._is_batch_mode([1], arr_multi) is True
        assert AISidecarServer._is_batch_mode([1], arr_single) is False
        assert AISidecarServer._is_batch_mode([10], arr_multi) is False

    @pytest.mark.asyncio
    async def test_compute_resolve_shape(self):
        flat = np.array([1.0] * 10, dtype=np.float32)
        result = AISidecarServer._compute_resolve_shape([1, 10], flat)
        assert result == [1, 10]

    @pytest.mark.asyncio
    async def test_auto_increment_version(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        new_ver = sidecar._auto_increment_version(wrapper)
        assert new_ver != "v1.0.0"
        assert "v1" in new_ver or "v2" in new_ver

    @pytest.mark.asyncio
    async def test_record_version(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        initial = len(wrapper.get_version_history())
        sidecar._record_version(wrapper, "manual_note")
        assert len(wrapper.get_version_history()) == initial + 1

    @pytest.mark.asyncio
    async def test_find_version_in_history_not_found(self):
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = AISidecarServer._find_version_in_history(wrapper, "nonexistent")
        assert result is None

    @pytest.mark.asyncio
    async def test_validate_provider_available_cpu(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        assert sidecar._validate_provider_available(PROVIDER_CPU) == PROVIDER_CPU

    @pytest.mark.asyncio
    async def test_validate_provider_available_cuda_fallback(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        # CUDA is likely not available in test env, should fallback to CPU
        result = sidecar._validate_provider_available(PROVIDER_CUDA)
        assert result in (PROVIDER_CUDA, PROVIDER_CPU)

    @pytest.mark.asyncio
    async def test_validate_provider_available_invalid(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        assert sidecar._validate_provider_available("InvalidProvider") == PROVIDER_CPU

    @pytest.mark.asyncio
    async def test_try_generate_preset(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        preset = PRESET_MODELS[0]
        result = sidecar._try_generate_preset(preset)
        assert isinstance(result, bool)

    @pytest.mark.asyncio
    async def test_stop_scheduled_not_running(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        result = await sidecar._stop_scheduled("nonexistent")
        assert result is False

    @pytest.mark.asyncio
    async def test_handle_timeout_no_cache(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        resp = sidecar._handle_timeout(wrapper, "m1", 100)
        data = json.loads(resp.body)
        assert data["status"] == "error"
        assert data["error_code"] == "ERR_AI_INFERENCE_TIMEOUT"

    @pytest.mark.asyncio
    async def test_handle_timeout_with_cache(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        wrapper.last_result = {"output_0": [0.5]}
        resp = sidecar._handle_timeout(wrapper, "m1", 100)
        data = json.loads(resp.body)
        assert data["status"] == "degraded"
        assert data["error_code"] == "ERR_AI_INFERENCE_TIMEOUT"
        assert "output_data" in data


# ═══════════════════════════════════════════════════════════════════════
#  Logging tests
# ═══════════════════════════════════════════════════════════════════════


class TestLogging:
    """Tests for JsonFormatter and _setup_logging."""

    def test_json_formatter_basic(self):
        formatter = JsonFormatter()
        record = logging.LogRecord(
            name="test", level=logging.INFO, pathname="", lineno=0,
            msg="Test message", args=(), exc_info=None,
        )
        output = formatter.format(record)
        data = json.loads(output)
        assert data["level"] == "INFO"
        assert data["message"] == "Test message"
        assert "timestamp" in data

    def test_json_formatter_with_exception(self):
        formatter = JsonFormatter()
        try:
            raise ValueError("Test exception")
        except ValueError:
            import sys
            record = logging.LogRecord(
                name="test", level=logging.ERROR, pathname="", lineno=0,
                msg="Error occurred", args=(), exc_info=sys.exc_info(),
            )
        output = formatter.format(record)
        data = json.loads(output)
        assert "exception" in data
        assert "ValueError" in data["exception"]

    def test_setup_logging_text(self):
        _setup_logging("DEBUG", json_format=False)
        assert logging.getLogger().level == logging.DEBUG

    def test_setup_logging_json(self):
        _setup_logging("WARNING", json_format=True)
        assert logging.getLogger().level == logging.WARNING

    def test_setup_logging_invalid_level(self):
        _setup_logging("INVALID", json_format=False)
        assert logging.getLogger().level == logging.INFO


# ═══════════════════════════════════════════════════════════════════════
#  create_app route registration test
# ═══════════════════════════════════════════════════════════════════════


class TestCreateApp:
    """Test that create_app registers all routes."""

    @pytest.mark.asyncio
    async def test_all_routes_registered(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        app = create_app(sidecar)
        routes = [r.resource.canonical for r in app.router.routes()]
        # Verify key routes exist
        assert "/health" in routes
        assert "/health/live" in routes
        assert "/health/ready" in routes
        assert "/metrics" in routes
        assert "/models" in routes
        assert "/models/load" in routes
        assert "/models/unload" in routes
        assert "/models/reload" in routes
        assert "/models/enable" in routes
        assert "/models/disable" in routes
        assert "/models/remove" in routes
        assert "/models/rollback" in routes
        assert "/models/generate-presets" in routes
        assert "/infer" in routes
        assert "/scheduled/start" in routes
        assert "/scheduled/stop" in routes
        assert "/self-learning/sample" in routes
        assert "/self-learning/predict" in routes
        assert "/self-learning/stats" in routes
        assert "/self-learning/stats/all" in routes
        assert "/self-learning/reset" in routes
        assert "/self-learning/threshold" in routes
        assert "/stats" in routes
        assert "/execution-provider" in routes
        assert "/execution-providers" in routes


# ═══════════════════════════════════════════════════════════════════════
#  Constants tests
# ═══════════════════════════════════════════════════════════════════════


class TestConstants:
    """Verify constants are properly defined and have expected values."""

    def test_default_values(self):
        assert DEFAULT_HOST == "0.0.0.0"
        assert DEFAULT_PORT == 50052
        assert DEFAULT_WINDOW_SIZE == 100
        assert DEFAULT_EWMA_ALPHA == 0.3
        assert DEFAULT_ANOMALY_THRESHOLD == 3.0
        assert MIN_SAMPLES_FOR_ANOMALY == 10

    def test_provider_constants(self):
        assert PROVIDER_CPU == "CPU"
        assert PROVIDER_CUDA == "CUDA"
        assert PROVIDER_OPENVINO == "OpenVINO"
        assert PROVIDER_CPU in VALID_PROVIDERS

    def test_status_constants(self):
        assert STATUS_ACTIVE == "active"
        assert STATUS_UNAVAILABLE == "unavailable"
        assert STATUS_ERROR == "error"
        assert STATUS_LOADING == "loading"
        assert STATUS_INACTIVE == "inactive"

    def test_preset_models_defined(self):
        assert len(PRESET_MODELS) > 0
        for preset in PRESET_MODELS:
            assert "model_id" in preset
            assert "model_file" in preset
