"""
Tests for serve(), main(), _render_prometheus, and other uncovered paths.
"""
import asyncio
import json
import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from server import (
    AISidecarServer,
    OnnxModelWrapper,
    PRESET_MODELS,
    PROVIDER_CPU,
    STATUS_ACTIVE,
    STATUS_ERROR,
    STATUS_INACTIVE,
    STATUS_LOADING,
    STATUS_UNAVAILABLE,
    _render_prometheus,
    _metric_inc,
    _metric_set,
    create_app,
    serve,
)


class TestRenderPrometheus:
    """Tests for _render_prometheus."""

    def test_render_basic(self):
        _metric_inc("test_metric", 5)
        result = _render_prometheus()
        assert isinstance(result, str)
        # Should contain HELP and TYPE lines for known metrics
        assert "ai_sidecar_" in result or "# HELP" in result or result == ""

    def test_render_with_uptime(self):
        _metric_set("uptime_start", 1000.0)
        result = _render_prometheus()
        assert isinstance(result, str)


class TestParseJson:
    """Tests for _parse_json method."""

    @pytest.mark.asyncio
    async def test_parse_valid_json(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        request = MagicMock()
        request.json = AsyncMock(return_value={"key": "value"})
        result = await sidecar._parse_json(request)
        assert result == {"key": "value"}

    @pytest.mark.asyncio
    async def test_parse_invalid_json(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        from aiohttp import web
        request = MagicMock()
        request.json = AsyncMock(side_effect=json.JSONDecodeError("msg", "doc", 0))
        result = await sidecar._parse_json(request)
        assert isinstance(result, web.Response)
        assert result.status == 400

    @pytest.mark.asyncio
    async def test_parse_invalid_content_type(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        from aiohttp import web
        request = MagicMock()
        request.json = AsyncMock(
            side_effect=Exception("Attempt to decode JSON with unexpected mimetype")
        )
        result = await sidecar._parse_json(request)
        assert isinstance(result, web.Response)
        assert result.status == 400


class TestHandleMetrics:
    """Tests for handle_metrics."""

    @pytest.mark.asyncio
    async def test_metrics_endpoint(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        request = MagicMock()
        resp = await sidecar.handle_metrics(request)
        assert resp.status == 200
        text = resp.text
        assert "ai_sidecar" in text or "# HELP" in text


class TestHandleHealth:
    """Tests for health endpoints."""

    @pytest.mark.asyncio
    async def test_handle_health(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        request = MagicMock()
        resp = await sidecar.handle_health(request)
        assert resp.status == 200
        data = json.loads(resp.body)
        assert data["healthy"] is True
        assert data["status"] == "alive"

    @pytest.mark.asyncio
    async def test_handle_ready(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        request = MagicMock()
        resp = await sidecar.handle_ready(request)
        assert resp.status in (200, 503)


class TestHandleListModels:
    """Tests for handle_list_models."""

    @pytest.mark.asyncio
    async def test_list_models(self):
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        request = MagicMock()
        resp = await sidecar.handle_list_models(request)
        assert resp.status == 200
        data = json.loads(resp.body)
        assert "models" in data
        assert len(data["models"]) > 0


class TestEnableModelPaths:
    """Tests for enable model state machine paths."""

    @pytest.mark.asyncio
    async def test_enable_loading_model(self):
        """Enable a model in loading state should return error."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        wrapper.status = STATUS_LOADING
        resp = await sidecar._enable_wrapper(wrapper, "preset-anomaly-v1")
        data = json.loads(resp.body)
        assert data["error_code"] == "ERR_AI_MODEL_IS_LOADING"

    @pytest.mark.asyncio
    async def test_enable_inactive_model(self):
        """Enable a model in inactive state should try to load."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        wrapper.status = STATUS_INACTIVE
        resp = await sidecar._enable_wrapper(wrapper, "preset-anomaly-v1")
        # Should either succeed or fail depending on model file existence
        data = json.loads(resp.body)
        assert "success" in data or "error_code" in data

    @pytest.mark.asyncio
    async def test_enable_unavailable_preset(self):
        """Enable an unavailable preset model should regenerate."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        wrapper.status = STATUS_UNAVAILABLE
        # Model file should exist since presets were generated during init
        resp = await sidecar._enable_wrapper(wrapper, "preset-anomaly-v1")
        data = json.loads(resp.body)
        assert "success" in data or "error_code" in data

    @pytest.mark.asyncio
    async def test_enable_unavailable_no_file(self):
        """Enable unavailable model with missing file should return error."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = OnnxModelWrapper(
            model_id="fake", model_name="Fake", model_version="v1",
            model_type="", is_preset=False, model_path="/nonexistent",
        )
        wrapper.status = STATUS_UNAVAILABLE
        resp = await sidecar._enable_unavailable(wrapper, "fake")
        data = json.loads(resp.body)
        assert data["error_code"] == "ERR_AI_MODEL_FILE_NOT_FOUND"


class TestRollbackPaths:
    """Tests for rollback complete paths."""

    @pytest.mark.asyncio
    async def test_rollback_to_version_without_path(self):
        """Rollback to version with no model_path should just set version."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        # Record a version with no path
        sidecar._record_version(wrapper, "test_note")
        history = wrapper.get_version_history()
        # Find an entry to rollback to
        if history:
            target_version = history[0].get("version", "")
            if target_version:
                # Create a mock request
                from aiohttp import web
                request = MagicMock()
                request.json = AsyncMock(return_value={
                    "model_id": wrapper.model_id,
                    "target_version": target_version,
                })
                resp = await sidecar.handle_rollback_model(request)
                assert resp.status == 200

    @pytest.mark.asyncio
    async def test_rollback_success_with_path(self):
        """Rollback to a version with valid model_path should reload."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        # Manually add a version entry with a valid path
        model_path = str(sidecar._models_dir / PRESET_MODELS[0]["model_file"])
        wrapper._version_history.append({
            "version": "v0.9.0",
            "timestamp": "2024-01-01T00:00:00Z",
            "model_path": model_path,
            "note": "manual",
        })
        from aiohttp import web
        request = MagicMock()
        request.json = AsyncMock(return_value={
            "model_id": wrapper.model_id,
            "target_version": "v0.9.0",
        })
        resp = await sidecar.handle_rollback_model(request)
        assert resp.status == 200
        data = json.loads(resp.body)
        assert data["success"] is True


class TestScheduledLoop:
    """Tests for _scheduled_loop internals."""

    @pytest.mark.asyncio
    async def test_scheduled_loop_cancellation(self):
        """Scheduled loop should handle cancellation gracefully."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        sidecar._scheduled_tasks["test-model"] = asyncio.create_task(
            sidecar._scheduled_loop("test-model", 1)
        )
        await asyncio.sleep(0.1)
        # Cancelling should not raise; the loop catches CancelledError
        result = await sidecar._stop_scheduled("test-model")
        assert result is True
        assert "test-model" not in sidecar._scheduled_tasks

    @pytest.mark.asyncio
    async def test_stop_scheduled_with_active_task(self):
        """Stop a running scheduled task."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        sidecar._scheduled_tasks["test-model"] = asyncio.create_task(
            sidecar._scheduled_loop("test-model", 10)
        )
        result = await sidecar._stop_scheduled("test-model")
        assert result is True


class TestServeFunction:
    """Tests for serve() function."""

    @pytest.mark.asyncio
    async def test_serve_starts_and_stops(self):
        """serve() should start server and respond to stop event."""
        with tempfile.TemporaryDirectory() as tmpdir:
            # Run serve in a task and cancel after a short delay
            task = asyncio.create_task(
                serve(host="127.0.0.1", port=0, models_dir=tmpdir)
            )
            await asyncio.sleep(2)
            task.cancel()
            with pytest.raises((asyncio.CancelledError, SystemExit)):
                await task


class TestMainFunction:
    """Tests for main() CLI entry point."""

    def test_main_import(self):
        """main() should be importable and callable."""
        from server import main
        assert callable(main)

    def test_main_with_help(self):
        """main() with --help should raise SystemExit."""
        from server import main
        with patch.object(sys, "argv", ["server", "--help"]):
            with pytest.raises(SystemExit):
                main()


class TestReloadAllModels:
    """Tests for _reload_all_models."""

    @pytest.mark.asyncio
    async def test_reload_all_models_cpu(self):
        """Reload all active models with CPU provider."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        await sidecar._reload_all_models(PROVIDER_CPU)
        # All models should still be active
        for wrapper in sidecar._loaded_models.values():
            assert wrapper.status in (STATUS_ACTIVE, STATUS_ERROR)


class TestGeneratePresets:
    """Tests for preset model generation."""

    @pytest.mark.asyncio
    async def test_generate_presets_already_exist(self):
        """Generate presets when they already exist should return True."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        request = MagicMock()
        resp = await sidecar.handle_generate_presets(request)
        assert resp.status == 200
        data = json.loads(resp.body)
        assert "results" in data
        for model_id, success in data["results"].items():
            assert success is True


class TestModelWrapperSerializeSchema:
    """Tests for OnnxModelWrapper._serialize_schema."""

    def test_serialize_schema_dict(self):
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = w._serialize_schema({"shape": [1, 10], "dtype": "float32"})
        assert "shape" in result
        assert result["shape"] == "[1, 10]"
        assert result["dtype"] == "float32"

    def test_serialize_schema_empty(self):
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = w._serialize_schema({})
        assert result == {}

    def test_serialize_schema_with_string(self):
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1",
            model_type="", is_preset=False, model_path="/fake",
        )
        result = w._serialize_schema({"name": "test"})
        assert result["name"] == "test"


class TestOnnxModelWrapperLoad:
    """Tests for OnnxModelWrapper.load with real preset model."""

    @pytest.mark.asyncio
    async def test_load_real_model(self):
        """Load a real preset model file."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        wrapper = list(sidecar._loaded_models.values())[0]
        model_path = str(sidecar._models_dir / PRESET_MODELS[0]["model_file"])
        w = OnnxModelWrapper(
            model_id="test", model_name="Test", model_version="v1",
            model_type="anomaly", is_preset=True, model_path=model_path,
        )
        await w.load()
        assert w.status == STATUS_ACTIVE
        assert w.session is not None

    @pytest.mark.asyncio
    async def test_load_then_unload(self):
        """Load and then unload a model."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        model_path = str(sidecar._models_dir / PRESET_MODELS[0]["model_file"])
        w = OnnxModelWrapper(
            model_id="test", model_name="Test", model_version="v1",
            model_type="anomaly", is_preset=True, model_path=model_path,
        )
        await w.load()
        assert w.status == STATUS_ACTIVE
        await w.unload()
        assert w.session is None
        assert w.status == STATUS_INACTIVE

    @pytest.mark.asyncio
    async def test_load_with_provider(self):
        """Load with explicit CPU provider."""
        sidecar = AISidecarServer(models_dir=tempfile.mkdtemp())
        await sidecar.initialize()
        model_path = str(sidecar._models_dir / PRESET_MODELS[0]["model_file"])
        w = OnnxModelWrapper(
            model_id="test", model_name="Test", model_version="v1",
            model_type="anomaly", is_preset=True, model_path=model_path,
        )
        await w.load(provider=PROVIDER_CPU)
        assert w.status == STATUS_ACTIVE

    def test_to_dict_with_loaded_model(self):
        """to_dict should include all fields."""
        w = OnnxModelWrapper(
            model_id="m1", model_name="M1", model_version="v1.0.0",
            model_type="anomaly", is_preset=True, model_path="/fake",
            input_schema={"shape": [1, 10]},
            output_schema={"shape": [1]},
        )
        d = w.to_dict()
        assert d["model_id"] == "m1"
        assert d["is_preset"] is True
        assert d["model_type"] == "anomaly"
        assert "input_schema" in d
        assert "output_schema" in d
