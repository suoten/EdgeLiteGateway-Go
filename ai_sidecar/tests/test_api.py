"""
Integration tests for the AI Sidecar HTTP API.
"""
import asyncio
import json

import pytest
import pytest_asyncio

from server import AISidecarServer, create_app


@pytest_asyncio.fixture
async def client(tmp_path):
    """Create a test client with a temporary models directory."""
    from aiohttp.test_utils import TestClient, TestServer
    models_dir = str(tmp_path / "models")
    sidecar = AISidecarServer(models_dir=models_dir)
    await sidecar.initialize()
    app = create_app(sidecar)
    server = TestServer(app)
    cli = TestClient(server)
    await cli.start_server()
    yield cli, sidecar
    await cli.close()


class TestHealthEndpoints:
    """Tests for health, readiness, and metrics endpoints."""

    async def test_health(self, client):
        c, _ = client
        resp = await c.get("/health")
        assert resp.status == 200
        data = await resp.json()
        assert data["healthy"] is True
        assert data["status"] == "alive"
        assert "version" in data
        assert "uptime_seconds" in data

    async def test_health_live(self, client):
        c, _ = client
        resp = await c.get("/health/live")
        assert resp.status == 200
        data = await resp.json()
        assert data["healthy"] is True

    async def test_health_ready(self, client):
        c, _ = client
        resp = await c.get("/health/ready")
        # Preset models should be loaded and active
        assert resp.status in (200, 503)

    async def test_metrics(self, client):
        c, _ = client
        resp = await c.get("/metrics")
        assert resp.status == 200
        text = await resp.text()
        assert "ai_sidecar_inference_total" in text
        assert "ai_sidecar_models_loaded" in text
        assert "ai_sidecar_uptime_seconds" in text


class TestModelManagement:
    """Tests for model management endpoints."""

    async def test_list_models(self, client):
        c, _ = client
        resp = await c.get("/models")
        assert resp.status == 200
        data = await resp.json()
        assert "models" in data
        assert len(data["models"]) > 0

    async def test_get_model_status(self, client):
        c, _ = client
        resp = await c.get("/models/preset-anomaly-v1/status")
        assert resp.status == 200
        data = await resp.json()
        assert "status" in data

    async def test_get_model_status_not_found(self, client):
        c, _ = client
        resp = await c.get("/models/nonexistent/status")
        assert resp.status == 200
        data = await resp.json()
        assert data["status"] == ""

    async def test_load_model_missing_id(self, client):
        c, _ = client
        resp = await c.post("/models/load", json={})
        assert resp.status == 400
        data = await resp.json()
        assert data["error_code"] == "ERR_AI_MODEL_ID_REQUIRED"

    async def test_load_model_invalid_json(self, client):
        c, _ = client
        resp = await c.post("/models/load", data="not json", headers={"Content-Type": "application/json"})
        assert resp.status == 400
        data = await resp.json()
        assert data["error_code"] in ("ERR_INVALID_JSON", "ERR_INVALID_REQUEST")

    async def test_unload_model_not_found(self, client):
        c, _ = client
        resp = await c.post("/models/unload", json={"model_id": "nonexistent"})
        assert resp.status == 200
        data = await resp.json()
        assert data["success"] is False
        assert data["error_code"] == "ERR_AI_MODEL_NOT_FOUND"

    async def test_generate_presets(self, client):
        c, _ = client
        resp = await c.post("/models/generate-presets", json={})
        assert resp.status == 200
        data = await resp.json()
        assert "results" in data

    async def test_get_model_stats(self, client):
        c, _ = client
        resp = await c.get("/models/preset-anomaly-v1/stats")
        assert resp.status == 200

    async def test_get_model_history(self, client):
        c, _ = client
        resp = await c.get("/models/preset-anomaly-v1/history")
        assert resp.status == 200
        data = await resp.json()
        assert "history" in data


class TestInference:
    """Tests for inference endpoint."""

    async def test_infer_missing_model_id(self, client):
        c, _ = client
        resp = await c.post("/infer", json={"input_data": [1.0, 2.0]})
        assert resp.status == 400
        data = await resp.json()
        assert data["error_code"] == "ERR_AI_MODEL_ID_REQUIRED"

    async def test_infer_model_not_available(self, client):
        c, _ = client
        resp = await c.post("/infer", json={"model_id": "nonexistent", "input_data": [1.0]})
        assert resp.status == 200
        data = await resp.json()
        assert data["status"] == "error"
        assert data["error_code"] == "ERR_AI_MODEL_NOT_AVAILABLE"

    async def test_infer_success(self, client):
        c, _ = client
        resp = await c.post("/infer", json={
            "model_id": "preset-anomaly-v1",
            "input_data": [0.5] * 100,
        })
        assert resp.status == 200
        data = await resp.json()
        assert data["status"] == "success"
        assert "output_data" in data
        assert "latency_ms" in data

    async def test_infer_invalid_json(self, client):
        c, _ = client
        resp = await c.post("/infer", data="bad", headers={"Content-Type": "application/json"})
        assert resp.status == 400


class TestSelfLearningAPI:
    """Tests for self-learning endpoints."""

    async def test_add_sample(self, client):
        c, _ = client
        resp = await c.post("/self-learning/sample", json={
            "device_id": "dev1",
            "point_name": "temp",
            "value": 42.0,
        })
        assert resp.status == 200
        data = await resp.json()
        assert "is_anomaly" in data
        assert "confidence" in data

    async def test_add_sample_missing_fields(self, client):
        c, _ = client
        resp = await c.post("/self-learning/sample", json={"value": 1.0})
        assert resp.status == 400

    async def test_predict(self, client):
        c, _ = client
        # First add some samples
        for i in range(15):
            await c.post("/self-learning/sample", json={
                "device_id": "dev1",
                "point_name": "temp",
                "value": 50.0,
            })
        resp = await c.post("/self-learning/predict", json={
            "device_id": "dev1",
            "point_name": "temp",
        })
        assert resp.status == 200
        data = await resp.json()
        assert "predicted_value" in data
        assert "confidence" in data

    async def test_predict_not_found(self, client):
        c, _ = client
        resp = await c.post("/self-learning/predict", json={
            "device_id": "unknown",
            "point_name": "unknown",
        })
        assert resp.status == 200
        data = await resp.json()
        assert data["predicted_value"] == 0
        assert data["confidence"] == 0

    async def test_self_learning_stats(self, client):
        c, _ = client
        await c.post("/self-learning/sample", json={
            "device_id": "dev1",
            "point_name": "temp",
            "value": 42.0,
        })
        resp = await c.post("/self-learning/stats", json={
            "device_id": "dev1",
            "point_name": "temp",
        })
        assert resp.status == 200
        data = await resp.json()
        assert data["stats"] is not None

    async def test_all_self_learning_stats(self, client):
        c, _ = client
        resp = await c.get("/self-learning/stats/all")
        assert resp.status == 200

    async def test_reset_self_learning(self, client):
        c, _ = client
        await c.post("/self-learning/sample", json={
            "device_id": "dev1",
            "point_name": "temp",
            "value": 42.0,
        })
        resp = await c.post("/self-learning/reset", json={
            "device_id": "dev1",
            "point_name": "temp",
        })
        assert resp.status == 200
        data = await resp.json()
        assert data["success"] is True

    async def test_set_threshold(self, client):
        c, _ = client
        resp = await c.post("/self-learning/threshold", json={
            "device_id": "dev1",
            "point_name": "temp",
            "threshold": 5.0,
        })
        assert resp.status == 200
        data = await resp.json()
        assert data["success"] is True


class TestExecutionProvider:
    """Tests for execution provider endpoints."""

    async def test_get_providers(self, client):
        c, _ = client
        resp = await c.get("/execution-providers")
        assert resp.status == 200
        data = await resp.json()
        assert "providers" in data
        assert "CPU" in data["providers"]

    async def test_set_invalid_provider(self, client):
        c, _ = client
        resp = await c.post("/execution-provider", json={"provider": "InvalidGPU"})
        assert resp.status == 400
        data = await resp.json()
        assert data["error_code"] == "ERR_AI_INVALID_PROVIDER"

    async def test_set_cpu_provider(self, client):
        c, _ = client
        resp = await c.post("/execution-provider", json={"provider": "CPU"})
        assert resp.status == 200
        data = await resp.json()
        assert data["success"] is True
        assert data["actual_provider"] == "CPU"


class TestErrorHandling:
    """Tests for error handling middleware."""

    async def test_404_handler(self, client):
        c, _ = client
        resp = await c.get("/nonexistent-route")
        assert resp.status == 404
        data = await resp.json()
        assert data["error_code"] == "ERR_ROUTE_NOT_FOUND"

    async def test_stats_endpoint(self, client):
        c, _ = client
        resp = await c.get("/stats")
        assert resp.status == 200
        data = await resp.json()
        assert "stats" in data
