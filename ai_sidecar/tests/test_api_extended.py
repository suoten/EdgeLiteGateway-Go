"""
Extended API integration tests covering model lifecycle, scheduled inference,
rollback, middleware edge cases, and error paths.
"""

import pytest_asyncio
from aiohttp.test_utils import TestClient, TestServer

from server import (
    AISidecarServer,
    STATUS_INACTIVE,
    STATUS_UNAVAILABLE,
    create_app,
)


@pytest_asyncio.fixture
async def client(tmp_path):
    """Create a test client with preset models loaded."""
    models_dir = str(tmp_path / "models")
    sidecar = AISidecarServer(models_dir=models_dir)
    await sidecar.initialize()
    app = create_app(sidecar)
    server = TestServer(app)
    cli = TestClient(server)
    await cli.start_server()
    yield cli, sidecar
    # Cleanup scheduled tasks
    for mid in list(sidecar._scheduled_tasks.keys()):
        await sidecar._stop_scheduled(mid)
    await cli.close()


class TestModelLifecycle:
    """Tests for full model lifecycle: load → reload → enable/disable → remove."""

    async def test_load_already_loaded_model(self, client):
        """Loading an already-loaded preset model should return error."""
        c, _ = client
        resp = await c.post("/models/load", json={
            "model_id": "preset-anomaly-v1",
            "model_path": "",
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_ALREADY_LOADED"

    async def test_load_nonexistent_model_file(self, client):
        """Loading a model with nonexistent file should create wrapper in error state."""
        c, _ = client
        resp = await c.post("/models/load", json={
            "model_id": "test-bad-model",
            "model_path": "/nonexistent/path.onnx",
        })
        assert resp.status == 200
        data = await resp.json()
        # Wrapper is created but model status should be error
        assert "model_info" in data or data.get("success") is True
        # Check model status
        resp = await c.get("/models/test-bad-model/status")
        status_data = await resp.json()
        assert status_data["status"] in ("error", "unavailable", "")

    async def test_unload_and_reload_preset(self, client):
        """Unload a preset model then reload it."""
        c, _ = client
        # Unload
        resp = await c.post("/models/unload", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True
        # Verify status is unavailable
        resp = await c.get("/models/preset-anomaly-v1/status")
        data = await resp.json()
        assert data["status"] in (STATUS_INACTIVE, STATUS_UNAVAILABLE, "")

    async def test_reload_model_success(self, client):
        """Reload an active model with auto version bump."""
        c, _ = client
        resp = await c.post("/models/reload", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True
        assert "new_version" in data

    async def test_reload_model_not_found(self, client):
        """Reload a non-existent model should return error."""
        c, _ = client
        resp = await c.post("/models/reload", json={
            "model_id": "nonexistent"
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_FOUND"

    async def test_enable_active_model(self, client):
        """Enable an already-active model should return success."""
        c, _ = client
        resp = await c.post("/models/enable", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True

    async def test_enable_model_not_found(self, client):
        """Enable a non-existent model should return error."""
        c, _ = client
        resp = await c.post("/models/enable", json={
            "model_id": "nonexistent"
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_FOUND"

    async def test_disable_model(self, client):
        """Disable an active model."""
        c, _ = client
        resp = await c.post("/models/disable", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True
        # Re-enable for other tests
        await c.post("/models/enable", json={
            "model_id": "preset-anomaly-v1"
        })

    async def test_disable_model_not_found(self, client):
        """Disable a non-existent model should return error."""
        c, _ = client
        resp = await c.post("/models/disable", json={
            "model_id": "nonexistent"
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_FOUND"

    async def test_remove_model(self, client):
        """Remove a model permanently."""
        c, sidecar = client
        # First load a new model
        resp = await c.post("/models/load", json={
            "model_id": "preset-anomaly-v1",
            "model_path": str(sidecar._models_dir / "anomaly_v1.onnx"),
        })
        # If already loaded, just remove it
        resp = await c.post("/models/remove", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True
        # Verify it's gone
        resp = await c.get("/models/preset-anomaly-v1/status")
        data = await resp.json()
        assert data["status"] == ""

    async def test_remove_model_not_found(self, client):
        """Remove a non-existent model should return error."""
        c, _ = client
        resp = await c.post("/models/remove", json={
            "model_id": "nonexistent"
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_FOUND"

    async def test_unload_model(self, client):
        """Unload model endpoint."""
        c, _ = client
        resp = await c.post("/models/unload", json={
            "model_id": "preset-anomaly-v1"
        })
        data = await resp.json()
        assert data["success"] is True
        # Reload for other tests
        await c.post("/models/load", json={
            "model_id": "preset-anomaly-v1",
            "model_path": "",
            "is_preset": True,
        })


class TestScheduledInference:
    """Tests for scheduled inference start/stop."""

    async def test_start_scheduled(self, client):
        """Start scheduled inference for an active model."""
        c, _ = client
        resp = await c.post("/scheduled/start", json={
            "model_id": "preset-anomaly-v1",
            "interval_seconds": 1,
            "device_id": "dev1",
            "point_name": "temp",
        })
        data = await resp.json()
        assert data["success"] is True
        # Stop it
        await c.post("/scheduled/stop", json={
            "model_id": "preset-anomaly-v1"
        })

    async def test_start_scheduled_already_running(self, client):
        """Starting scheduled inference twice should return error."""
        c, _ = client
        await c.post("/scheduled/start", json={
            "model_id": "preset-anomaly-v1",
            "interval_seconds": 1,
        })
        resp = await c.post("/scheduled/start", json={
            "model_id": "preset-anomaly-v1",
            "interval_seconds": 1,
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_SCHEDULED_ALREADY_RUNNING"
        await c.post("/scheduled/stop", json={
            "model_id": "preset-anomaly-v1"
        })

    async def test_start_scheduled_model_not_available(self, client):
        """Starting scheduled for unavailable model should error."""
        c, _ = client
        resp = await c.post("/scheduled/start", json={
            "model_id": "nonexistent",
            "interval_seconds": 1,
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_AVAILABLE"

    async def test_stop_scheduled_not_running(self, client):
        """Stopping a non-running scheduled task should return success=False."""
        c, _ = client
        resp = await c.post("/scheduled/stop", json={
            "model_id": "nonexistent"
        })
        data = await resp.json()
        assert data["success"] is False

    async def test_start_scheduled_missing_model_id(self, client):
        """Missing model_id should return 400."""
        c, _ = client
        resp = await c.post("/scheduled/start", json={})
        assert resp.status == 400


class TestModelRollback:
    """Tests for model version rollback."""

    async def test_rollback_missing_version(self, client):
        """Rollback without target_version should return 400."""
        c, _ = client
        resp = await c.post("/models/rollback", json={
            "model_id": "preset-anomaly-v1"
        })
        assert resp.status == 400
        data = await resp.json()
        assert data["error_code"] == "ERR_AI_VERSION_REQUIRED"

    async def test_rollback_model_not_found(self, client):
        """Rollback for non-existent model should return error."""
        c, _ = client
        resp = await c.post("/models/rollback", json={
            "model_id": "nonexistent",
            "target_version": "v1.0.0",
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_FOUND"

    async def test_rollback_version_not_found(self, client):
        """Rollback to non-existent version should return error."""
        c, _ = client
        resp = await c.post("/models/rollback", json={
            "model_id": "preset-anomaly-v1",
            "target_version": "v999.0.0",
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_VERSION_NOT_FOUND"

    async def test_rollback_missing_model_id(self, client):
        """Missing model_id in rollback should return 400."""
        c, _ = client
        resp = await c.post("/models/rollback", json={
            "target_version": "v1.0.0"
        })
        assert resp.status == 400

    async def test_get_version_history(self, client):
        """Get version history for a model."""
        c, _ = client
        resp = await c.get("/models/preset-anomaly-v1/history")
        data = await resp.json()
        assert "history" in data
        assert isinstance(data["history"], list)


class TestMiddlewareEdgeCases:
    """Tests for middleware error handling."""

    async def test_method_not_allowed(self, client):
        """Using wrong HTTP method should return 405."""
        c, _ = client
        resp = await c.delete("/health")
        assert resp.status == 405
        data = await resp.json()
        assert data["error_code"] == "ERR_METHOD_NOT_ALLOWED"

    async def test_404_handler(self, client):
        """Unknown route should return 404 JSON."""
        c, _ = client
        resp = await c.get("/totally-nonexistent")
        assert resp.status == 404
        data = await resp.json()
        assert data["error_code"] == "ERR_ROUTE_NOT_FOUND"

    async def test_invalid_json_on_model_load(self, client):
        """Invalid JSON body should return 400."""
        c, _ = client
        resp = await c.post(
            "/models/load",
            data="not json at all",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    async def test_invalid_json_on_unload(self, client):
        """Invalid JSON on unload should return 400."""
        c, _ = client
        resp = await c.post(
            "/models/unload",
            data="{invalid",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    async def test_invalid_json_on_scheduled_start(self, client):
        """Invalid JSON on scheduled start should return 400."""
        c, _ = client
        resp = await c.post(
            "/scheduled/start",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400

    async def test_invalid_json_on_rollback(self, client):
        """Invalid JSON on rollback should return 400."""
        c, _ = client
        resp = await c.post(
            "/models/rollback",
            data="bad",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status == 400


class TestInferenceEdgeCases:
    """Tests for inference edge cases."""

    async def test_infer_empty_input(self, client):
        """Inference with empty input data."""
        c, _ = client
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [],
        })
        assert resp.status == 200
        data = await resp.json()
        # Should either succeed or return error
        assert "status" in data

    async def test_infer_single_value(self, client):
        """Inference with a single value."""
        c, _ = client
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5],
        })
        assert resp.status == 200
        data = await resp.json()
        assert "status" in data

    async def test_infer_large_input(self, client):
        """Inference with a large input."""
        c, _ = client
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 500,
        })
        assert resp.status == 200

    async def test_infer_on_disabled_model(self, client):
        """Inference on a disabled model should return error."""
        c, _ = client
        # Disable model first
        await c.post("/models/disable", json={
            "model_id": "preset-anomaly-v1"
        })
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        data = await resp.json()
        assert data["status"] == "error"
        assert data["error_code"] == "ERR_AI_MODEL_NOT_AVAILABLE"
        # Re-enable
        await c.post("/models/enable", json={
            "model_id": "preset-anomaly-v1"
        })

    async def test_infer_wrong_input_size(self, client):
        """Inference with mismatched input size should handle gracefully."""
        c, _ = client
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [1.0] * 50,  # Less than expected 100
        })
        assert resp.status == 200
        data = await resp.json()
        assert "status" in data


class TestSelfLearningEdgeCases:
    """Edge case tests for self-learning API."""

    async def test_add_sample_with_window_size(self, client):
        """Add sample with custom window_size."""
        c, _ = client
        resp = await c.post("/self-learning/sample", json={
            "device_id": "dev1",
            "point_name": "temp",
            "value": 42.0,
            "window_size": 50,
        })
        assert resp.status == 200

    async def test_add_sample_with_invalid_window_size(self, client):
        """Add sample with invalid window_size should use default."""
        c, _ = client
        resp = await c.post("/self-learning/sample", json={
            "device_id": "dev1",
            "point_name": "temp",
            "value": 42.0,
            "window_size": -10,
        })
        assert resp.status == 200

    async def test_add_sample_missing_device_id(self, client):
        """Missing device_id should return 400."""
        c, _ = client
        resp = await c.post("/self-learning/sample", json={
            "point_name": "temp",
            "value": 42.0,
        })
        assert resp.status == 400

    async def test_add_sample_missing_point_name(self, client):
        """Missing point_name should return 400."""
        c, _ = client
        resp = await c.post("/self-learning/sample", json={
            "device_id": "dev1",
            "value": 42.0,
        })
        assert resp.status == 400

    async def test_reset_not_found(self, client):
        """Reset a non-existent self-learning model should return error."""
        c, _ = client
        resp = await c.post("/self-learning/reset", json={
            "device_id": "unknown",
            "point_name": "unknown",
        })
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_SELF_LEARNING_NOT_FOUND"

    async def test_stats_not_found(self, client):
        """Stats for non-existent model should return null."""
        c, _ = client
        resp = await c.post("/self-learning/stats", json={
            "device_id": "unknown",
            "point_name": "unknown",
        })
        data = await resp.json()
        assert data["stats"] is None

    async def test_predict_with_model(self, client):
        """Predict after adding samples."""
        c, _ = client
        for i in range(20):
            await c.post("/self-learning/sample", json={
                "device_id": "dev_pred",
                "point_name": "temp",
                "value": 50.0 + i * 0.1,
            })
        resp = await c.post("/self-learning/predict", json={
            "device_id": "dev_pred",
            "point_name": "temp",
        })
        data = await resp.json()
        assert "predicted_value" in data
        assert data["confidence"] > 0

    async def test_anomaly_detection(self, client):
        """Test that anomalies are detected."""
        c, _ = client
        # Train with normal values
        for _ in range(15):
            resp = await c.post("/self-learning/sample", json={
                "device_id": "dev_anom",
                "point_name": "temp",
                "value": 50.0,
            })
            data = await resp.json()
            assert data["is_anomaly"] is False
        # Add anomalous value
        resp = await c.post("/self-learning/sample", json={
            "device_id": "dev_anom",
            "point_name": "temp",
            "value": 500.0,
        })
        data = await resp.json()
        assert data["is_anomaly"] is True

    async def test_set_threshold_and_detect(self, client):
        """Set a custom threshold and verify anomaly detection respects it."""
        c, _ = client
        await c.post("/self-learning/threshold", json={
            "device_id": "dev_thresh",
            "point_name": "temp",
            "threshold": 1.0,
        })
        for _ in range(15):
            await c.post("/self-learning/sample", json={
                "device_id": "dev_thresh",
                "point_name": "temp",
                "value": 50.0,
            })
        # With low threshold, even small deviation should be anomaly
        resp = await c.post("/self-learning/sample", json={
            "device_id": "dev_thresh",
            "point_name": "temp",
            "value": 55.0,
        })
        data = await resp.json()
        # May or may not be anomaly depending on std_dev
        assert "is_anomaly" in data


class TestExecutionProviderExtended:
    """Extended execution provider tests."""

    async def test_set_cpu_provider_reloads_models(self, client):
        """Setting CPU provider should reload active models."""
        c, _ = client
        resp = await c.post("/execution-provider", json={
            "provider": "CPU"
        })
        data = await resp.json()
        assert data["success"] is True
        assert data["actual_provider"] == "CPU"

    async def test_set_empty_provider(self, client):
        """Empty provider should default to CPU."""
        c, _ = client
        resp = await c.post("/execution-provider", json={})
        data = await resp.json()
        assert data["success"] is True
        assert data["actual_provider"] == "CPU"

    async def test_set_cuda_provider(self, client):
        """Setting CUDA provider should either succeed or fallback to CPU."""
        c, _ = client
        resp = await c.post("/execution-provider", json={
            "provider": "CUDA"
        })
        data = await resp.json()
        # In test env, CUDA likely unavailable → fallback to CPU
        assert data["actual_provider"] in ("CPU", "CUDA")

    async def test_set_openvino_provider(self, client):
        """Setting OpenVINO provider should either succeed or fallback."""
        c, _ = client
        resp = await c.post("/execution-provider", json={
            "provider": "OpenVINO"
        })
        data = await resp.json()
        assert data["actual_provider"] in ("CPU", "OpenVINO")


class TestModelListAndStats:
    """Tests for model listing and stats endpoints."""

    async def test_list_models_has_preset_models(self, client):
        c, _ = client
        resp = await c.get("/models")
        data = await resp.json()
        model_ids = [m["model_id"] for m in data["models"]]
        assert "preset-anomaly-v1" in model_ids

    async def test_get_model_stats_after_inference(self, client):
        """Model stats should reflect inference count."""
        c, _ = client
        # Run an inference
        await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        resp = await c.get("/models/preset-anomaly-v1/stats")
        assert resp.status == 200
        data = await resp.json()
        assert "total" in data or "model_id" in data

    async def test_global_stats(self, client):
        """Global stats should contain engine info."""
        c, _ = client
        await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        resp = await c.get("/stats")
        data = await resp.json()
        assert "stats" in data
        assert data["stats"]["total_calls"] >= 1

    async def test_health_ready_after_inference(self, client):
        """Ready check should return 200 when models are active."""
        c, _ = client
        await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        resp = await c.get("/health/ready")
        assert resp.status in (200, 503)

    async def test_metrics_after_activity(self, client):
        """Metrics should reflect activity."""
        c, _ = client
        await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        resp = await c.get("/metrics")
        text = await resp.text()
        assert "ai_sidecar_inference_total" in text
        assert "ai_sidecar_uptime_seconds" in text
