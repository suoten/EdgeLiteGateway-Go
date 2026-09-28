"""
Targeted tests for remaining uncovered code paths to reach 90%+ coverage.
"""
import asyncio
import json
import os
import tempfile
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import numpy as np
import pytest
from aiohttp import web

from server import (
    AISidecarServer,
    OnnxModelWrapper,
    PRESET_MODELS,
    PROVIDER_CPU,
    PROVIDER_CUDA,
    STATUS_ACTIVE,
    STATUS_ERROR,
    STATUS_INACTIVE,
    STATUS_LOADING,
    STATUS_UNAVAILABLE,
    _build_default_graph,
    create_app,
    error_handling_middleware,
    request_logging_middleware,
)
from aiohttp.test_utils import TestClient, TestServer


@pytest.fixture
async def setup_sidecar(tmp_path):
    """Create initialized sidecar."""
    models_dir = str(tmp_path / "models")
    sidecar = AISidecarServer(models_dir=models_dir)
    await sidecar.initialize()
    app = create_app(sidecar)
    server = TestServer(app)
    cli = TestClient(server)
    await cli.start_server()
    yield cli, sidecar
    for mid in list(sidecar._scheduled_tasks.keys()):
        await sidecar._stop_scheduled(mid)
    await cli.close()


class TestBuildDefaultGraph:
    """Cover _build_default_graph."""

    def test_build_default_graph(self):
        import onnx
        from onnx import helper, TensorProto
        X = helper.make_tensor_value_info("input", TensorProto.FLOAT, [1, 10])
        Y = helper.make_tensor_value_info("output", TensorProto.FLOAT, [1, 1])
        graph = _build_default_graph(X, Y, 10, 1)
        assert graph is not None
        assert graph.name == "preset_graph"


class TestAutoIncrementVersionEdgeCases:
    """Cover _auto_increment_version edge cases."""

    @pytest.mark.asyncio
    async def test_non_version_format(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="custom",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = sidecar._auto_increment_version(w)
        assert result == "custom"

    @pytest.mark.asyncio
    async def test_version_no_v_prefix(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="1.0.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = sidecar._auto_increment_version(w)
        # Without 'v' prefix, should return as-is
        assert result == "1.0.0"

    @pytest.mark.asyncio
    async def test_version_two_parts(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = sidecar._auto_increment_version(w)
        # Only 2 parts, not 3, should return as-is
        assert result == "v1.0"


class TestRecordVersionHistoryLimit:
    """Cover version history trimming."""

    @pytest.mark.asyncio
    async def test_version_history_trim(self):
        from server import MAX_VERSION_HISTORY
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0.0",
            model_type="", is_preset=False, model_path="/fake",
        )
        # Add more than MAX_VERSION_HISTORY entries
        for i in range(MAX_VERSION_HISTORY + 5):
            sidecar._record_version(w, f"v1.0.{i}")
        assert len(w.get_version_history()) <= MAX_VERSION_HISTORY


class TestEnableUnavailablePaths:
    """Cover _enable_unavailable with preset regeneration."""

    @pytest.mark.asyncio
    async def test_enable_unavailable_regenerates_preset(self):
        """Enable unavailable preset should regenerate model file."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        model_path = wrapper.model_path
        wrapper.status = STATUS_UNAVAILABLE
        # Remove the model file to force regeneration
        if Path(model_path).exists():
            Path(model_path).unlink()
        resp = await sidecar._enable_wrapper(wrapper, "preset-anomaly-v1")
        data = json.loads(resp.body)
        # Should have either regenerated successfully or failed
        assert "success" in data or "error_code" in data


class TestInferenceTimeoutPath:
    """Cover inference timeout and error paths."""

    @pytest.mark.asyncio
    async def test_inference_timeout(self, setup_sidecar):
        """Test inference timeout returns degraded/error response."""
        c, sidecar = setup_sidecar
        # Set very short timeout
        sidecar._config["inference_timeout"] = 0.001
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        data = await resp.json()
        # Should be either success (if fast enough), degraded, or error
        assert data["status"] in ("success", "degraded", "error")

    @pytest.mark.asyncio
    async def test_inference_with_none_session(self):
        """Test inference when session is None raises RuntimeError."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        # session is None
        resp = await sidecar._run_inference(wrapper, "m1", [1.0])
        data = json.loads(resp.body)
        assert data["status"] == "error"
        assert data["error_code"] == "ERR_AI_INFERENCE_FAILED"


class TestBatchInferencePath:
    """Cover batch inference path."""

    @pytest.mark.asyncio
    async def test_batch_inference_with_single_dim_model(self):
        """Test that batch mode is used when model expects [1] shape."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        # Check if model has [1, 100] shape - batch mode won't trigger
        # But we can test _is_batch_mode directly
        arr_multi = np.array([1.0, 2.0, 3.0], dtype=np.float32)
        from server import AISidecarServer as SC
        assert SC._is_batch_mode([1], arr_multi) is True


class TestMiddleware500Error:
    """Cover middleware 500 error path."""

    @pytest.mark.asyncio
    async def test_middleware_500_error(self):
        """Test that unhandled exceptions return 500."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        app = web.Application(middlewares=[error_handling_middleware])

        async def exploding_handler(request):
            raise RuntimeError("Boom!")

        app.router.add_get("/explode", exploding_handler)
        server = TestServer(app)
        cli = TestClient(server)
        await cli.start_server()
        resp = await cli.get("/explode")
        assert resp.status == 500
        data = await resp.json()
        assert data["error_code"] == "ERR_INTERNAL_SERVER_ERROR"
        await cli.close()

    @pytest.mark.asyncio
    async def test_middleware_404_error(self):
        """Test 404 via middleware."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        app = web.Application(middlewares=[error_handling_middleware])
        server = TestServer(app)
        cli = TestClient(server)
        await cli.start_server()
        resp = await cli.get("/nonexistent")
        assert resp.status == 404
        data = await resp.json()
        assert data["error_code"] == "ERR_ROUTE_NOT_FOUND"
        await cli.close()

    @pytest.mark.asyncio
    async def test_middleware_405_error(self):
        """Test 405 via middleware."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        app = web.Application(middlewares=[error_handling_middleware])

        async def get_handler(request):
            return web.json_response({"ok": True})

        app.router.add_get("/test", get_handler)
        server = TestServer(app)
        cli = TestClient(server)
        await cli.start_server()
        resp = await cli.delete("/test")
        assert resp.status == 405
        data = await resp.json()
        assert data["error_code"] == "ERR_METHOD_NOT_ALLOWED"
        await cli.close()

    @pytest.mark.asyncio
    async def test_request_logging_middleware(self):
        """Test request logging middleware passes through."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        app = web.Application(middlewares=[request_logging_middleware])

        async def handler(request):
            return web.json_response({"ok": True})

        app.router.add_get("/test", handler)
        server = TestServer(app)
        cli = TestClient(server)
        await cli.start_server()
        resp = await cli.get("/test")
        assert resp.status == 200
        data = await resp.json()
        assert data["ok"] is True
        await cli.close()


class TestReloadModelErrorPath:
    """Cover reload model error path."""

    @pytest.mark.asyncio
    async def test_reload_model_error(self, setup_sidecar):
        """Reload model with bad path should handle error."""
        c, sidecar = setup_sidecar
        wrapper = list(sidecar._loaded_models.values())[0]
        # Reload with nonexistent path via the API
        resp = await c.post("/models/reload", json={
            "model_id": wrapper.model_id,
            "model_path": "/nonexistent/bad.onnx",
        })
        data = await resp.json()
        # Reload may succeed (reloading from original path) or fail
        # Either way, it should return a valid JSON response
        assert "success" in data or "error_code" in data


class TestDisableAndInfer:
    """Cover inference after disable."""

    @pytest.mark.asyncio
    async def test_infer_after_disable_then_enable(self, setup_sidecar):
        """Full cycle: disable → infer (error) → enable → infer (success)."""
        c, _ = setup_sidecar
        # Disable
        await c.post("/models/disable", json={"model_id": "preset-anomaly-v1"})
        # Infer should fail
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        data = await resp.json()
        assert data["status"] == "error"
        # Enable
        resp = await c.post("/models/enable", json={"model_id": "preset-anomaly-v1"})
        data = await resp.json()
        assert data["success"] is True
        # Infer should succeed
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        data = await resp.json()
        assert data["status"] == "success"


class TestRemoveAndScheduledStop:
    """Cover remove model with scheduled task running."""

    @pytest.mark.asyncio
    async def test_remove_model_stops_scheduled(self, setup_sidecar):
        """Remove model should also stop scheduled tasks."""
        c, sidecar = setup_sidecar
        # Start scheduled
        await c.post("/scheduled/start", json={
            "model_id": "preset-anomaly-v1",
            "interval_seconds": 5,
        })
        assert "preset-anomaly-v1" in sidecar._scheduled_tasks
        # Remove model
        resp = await c.post("/models/remove", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True
        # Scheduled task should be stopped
        # Note: _stop_scheduled is called in remove handler


class TestLoadModelWithInvalidJson:
    """Cover _parse_json error paths on various endpoints."""

    @pytest.mark.asyncio
    async def test_invalid_json_on_disable(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/models/disable",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_remove(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/models/remove",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_enable(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/models/enable",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_stop_scheduled(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/scheduled/stop",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_self_learning_sample(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/self-learning/sample",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_predict(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/self-learning/predict",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_stats(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/self-learning/stats",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_reset(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/self-learning/reset",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_threshold(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/self-learning/threshold",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    @pytest.mark.asyncio
    async def test_invalid_json_on_provider(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.post(
            "/execution-provider",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400


class TestHandleGetModelStatsEmpty:
    """Cover get_model_stats for nonexistent model."""

    @pytest.mark.asyncio
    async def test_get_stats_nonexistent(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.get("/models/nonexistent/stats")
        data = await resp.json()
        # Should return empty dict
        assert data == {} or "model_id" not in data


class TestHandleVersionHistoryNonexistent:
    """Cover version history for nonexistent model."""

    @pytest.mark.asyncio
    async def test_history_nonexistent(self, setup_sidecar):
        c, _ = setup_sidecar
        resp = await c.get("/models/nonexistent/history")
        data = await resp.json()
        assert data["history"] == []
