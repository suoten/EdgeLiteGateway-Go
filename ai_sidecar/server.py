"""
EdgeLite AI Sidecar — Python HTTP JSON Server.

This module implements an async HTTP JSON API that wraps ONNX Runtime
and self-learning models.  The Go gateway calls this sidecar via HTTP.

Architecture::

    Go Gateway ─── HTTP JSON (50052) ───> Python AI Sidecar ───> ONNX Runtime

Key design principles
--------------------
* **Thread-safety** — all shared state is guarded by ``threading.RLock``
  (Python-side) or ``asyncio.Lock`` (async-side).
* **Observability** — Prometheus ``/metrics``, structured JSON logs,
  per-model inference statistics, and Kubernetes-ready health probes.
* **Resilience** — inference timeout with cached-result fallback,
  automatic provider fallback (CUDA→CPU), and graceful shutdown.
* **Consistency** — every error response carries ``error_code`` and
  ``error_message``; every success response carries ``success: True``.

:license: MIT
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import logging
import math
import os
import signal
import tempfile
import threading
import time
from collections.abc import Awaitable, Callable
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Protocol

import numpy as np
import onnxruntime as ort
from aiohttp import web

UTC = timezone.utc

# ═══════════════════════════════════════════════════════════════════════
#  Constants
# ═══════════════════════════════════════════════════════════════════════

SERVER_VERSION = "1.0.0"
DEFAULT_HOST = "0.0.0.0"
DEFAULT_PORT = 50052
DEFAULT_MODELS_DIR = "models"

# Self-learning defaults
DEFAULT_WINDOW_SIZE = 100
MIN_SAMPLES_FOR_ANOMALY = 10
DEFAULT_EWMA_ALPHA = 0.3
DEFAULT_ANOMALY_THRESHOLD = 3.0

# Statistics limits
MAX_RECENT_LATENCIES = 100
MAX_VERSION_HISTORY = 50

# Inference defaults
DEFAULT_INFERENCE_TIMEOUT = 30.0
DEFAULT_SCHEDULED_INTERVAL = 60

# ONNX model generation constants
ONNX_IR_VERSION = 7
ONNX_OPSET_VERSION = 13
ONNX_RANDOM_SEED = 42

# Execution providers
PROVIDER_CPU = "CPU"
PROVIDER_CUDA = "CUDA"
PROVIDER_OPENVINO = "OpenVINO"
VALID_PROVIDERS = (PROVIDER_CPU, PROVIDER_CUDA, PROVIDER_OPENVINO)

# Model status constants
STATUS_ACTIVE = "active"
STATUS_INACTIVE = "inactive"
STATUS_LOADING = "loading"
STATUS_ERROR = "error"
STATUS_UNAVAILABLE = "unavailable"

# Type aliases
JSONDict = dict[str, Any]
InferenceResult = list[Any] | dict[str, Any]

logger = logging.getLogger(__name__)

# ═══════════════════════════════════════════════════════════════════════
#  Prometheus-style metrics (zero-dependency)
# ═══════════════════════════════════════════════════════════════════════

_METRICS: JSONDict = {
    "inference_total": 0,
    "inference_errors": 0,
    "inference_latency_sum_ms": 0.0,
    "inference_latency_count": 0,
    "models_loaded": 0,
    "uptime_start": time.time(),
}
_METRICS_LOCK = threading.Lock()


def _metric_inc(key: str, value: float = 1) -> None:
    """Atomically increment a metric counter."""
    with _METRICS_LOCK:
        _METRICS[key] = _METRICS.get(key, 0) + value


def _metric_set(key: str, value: float) -> None:
    """Atomically set a metric gauge."""
    with _METRICS_LOCK:
        _METRICS[key] = value


def _render_prometheus() -> str:
    """Render metrics in Prometheus text exposition format.

    :returns: Prometheus-formatted text with HELP/TYPE headers and values.
    """
    with _METRICS_LOCK:
        snapshot = dict(_METRICS)
    uptime = time.time() - snapshot.get("uptime_start", time.time())
    count = snapshot["inference_latency_count"]
    avg_lat = (
        snapshot["inference_latency_sum_ms"] / count if count > 0 else 0.0
    )
    lines = [
        "# HELP ai_sidecar_inference_total Total inference calls",
        "# TYPE ai_sidecar_inference_total counter",
        f"ai_sidecar_inference_total {snapshot['inference_total']}",
        "# HELP ai_sidecar_inference_errors Total inference errors",
        "# TYPE ai_sidecar_inference_errors counter",
        f"ai_sidecar_inference_errors {snapshot['inference_errors']}",
        "# HELP ai_sidecar_avg_latency_ms Average inference latency (ms)",
        "# TYPE ai_sidecar_avg_latency_ms gauge",
        f"ai_sidecar_avg_latency_ms {avg_lat:.2f}",
        "# HELP ai_sidecar_models_loaded Number of loaded models",
        "# TYPE ai_sidecar_models_loaded gauge",
        f"ai_sidecar_models_loaded {snapshot['models_loaded']}",
        "# HELP ai_sidecar_uptime_seconds Server uptime in seconds",
        "# TYPE ai_sidecar_uptime_seconds gauge",
        f"ai_sidecar_uptime_seconds {uptime:.2f}",
    ]
    return "\n".join(lines) + "\n"


# ═══════════════════════════════════════════════════════════════════════
#  ONNX package availability
# ═══════════════════════════════════════════════════════════════════════

try:
    import onnx
    from onnx import TensorProto, helper, numpy_helper

    _HAS_ONNX_PKG = True
except ImportError:  # pragma: no cover
    _HAS_ONNX_PKG = False
    onnx = None  # type: ignore[assignment]

# ═══════════════════════════════════════════════════════════════════════
#  Error response helpers
# ═══════════════════════════════════════════════════════════════════════


def _error_response(
    error_code: str,
    error_message: str,
    status: int = 200,
    **extra: Any,
) -> web.Response:
    """Build a standardised error JSON response.

    :param error_code: Machine-readable error identifier.
    :param error_message: Human-readable description.
    :param status: HTTP status code (default 200 for business errors).
    :param extra: Additional fields to include in the response body.
    :returns: ``web.Response`` with JSON body.
    """
    body: JSONDict = {
        "success": False,
        "error_code": error_code,
        "error_message": error_message,
    }
    body.update(extra)
    return web.json_response(body, status=status)


def _success_response(**data: Any) -> web.Response:
    """Build a standardised success JSON response.

    :param data: Fields to include in the response body.
    :returns: ``web.Response`` with JSON body containing ``success: True``.
    """
    body: JSONDict = {"success": True}
    body.update(data)
    return web.json_response(body)


def _require_model_id(data: JSONDict) -> str | web.Response:
    """Extract and validate ``model_id`` from request data.

    :param data: Parsed JSON body.
    :returns: The model_id string, or an error Response if missing.
    """
    model_id: str = str(data.get("model_id", ""))
    if not model_id:
        return _error_response(
            "ERR_AI_MODEL_ID_REQUIRED",
            "model_id is required",
            status=400,
        )
    return model_id


# ═══════════════════════════════════════════════════════════════════════
#  Inference statistics
# ═══════════════════════════════════════════════════════════════════════


class InferenceStatsCollector:
    """Thread-safe collector for inference statistics.

    Tracks per-model and aggregate call counts, errors, and latency
    metrics for observability and Prometheus exposition.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._total_calls = 0
        self._total_errors = 0
        self._total_latency_ms = 0.0
        self._per_model_calls: dict[str, int] = {}
        self._per_model_errors: dict[str, int] = {}
        self._per_model_latency: dict[str, float] = {}
        self._per_model_max_latency: dict[str, int] = {}
        self._per_model_min_latency: dict[str, int] = {}
        self._recent_latencies: list[int] = []
        self._max_recent = MAX_RECENT_LATENCIES

    def record_inference(
        self, model_id: str, latency_ms: int, status: str
    ) -> None:
        """Record a single inference call.

        :param model_id: Identifier of the model used.
        :param latency_ms: Wall-clock latency in milliseconds.
        :param status: ``"success"`` or ``"error"``.
        """
        with self._lock:
            self._total_calls += 1
            self._total_latency_ms += latency_ms
            self._per_model_calls[model_id] = (
                self._per_model_calls.get(model_id, 0) + 1
            )
            self._per_model_latency[model_id] = (
                self._per_model_latency.get(model_id, 0.0) + latency_ms
            )
            cur_max = self._per_model_max_latency.get(model_id, 0)
            if latency_ms > cur_max:
                self._per_model_max_latency[model_id] = latency_ms
            cur_min = self._per_model_min_latency.get(model_id)
            if cur_min is None or latency_ms < cur_min:
                self._per_model_min_latency[model_id] = latency_ms
            self._recent_latencies.append(latency_ms)
            if len(self._recent_latencies) > self._max_recent:
                self._recent_latencies = self._recent_latencies[-self._max_recent:]
            if status == "error":
                self._total_errors += 1
                self._per_model_errors[model_id] = (
                    self._per_model_errors.get(model_id, 0) + 1
                )

    def get_snapshot(self) -> JSONDict:
        """Return a point-in-time snapshot of aggregate statistics.

        :returns: Dict with total_calls, total_errors, avg_latency_ms,
            model_distribution, recent_latencies, and loaded_models.
        """
        with self._lock:
            avg = (
                round(self._total_latency_ms / self._total_calls, 2)
                if self._total_calls > 0
                else 0.0
            )
            return {
                "total_calls": self._total_calls,
                "total_errors": self._total_errors,
                "avg_latency_ms": avg,
                "model_distribution": dict(self._per_model_calls),
                "recent_latencies": list(self._recent_latencies),
                "loaded_models": 0,  # filled by caller
            }

    def get_model_stats(self, model_id: str) -> JSONDict | None:
        """Return per-model statistics, or ``None`` if model is unknown.

        :param model_id: Identifier of the model to query.
        :returns: Stats dict or None.
        """
        with self._lock:
            calls = self._per_model_calls.get(model_id, 0)
            if calls == 0:
                return None
            return {
                "model_id": model_id,
                "call_count": calls,
                "error_count": self._per_model_errors.get(model_id, 0),
                "avg_latency_ms": round(
                    self._per_model_latency.get(model_id, 0.0) / calls, 2
                ),
                "max_latency_ms": self._per_model_max_latency.get(model_id, 0),
                "min_latency_ms": self._per_model_min_latency.get(model_id, 0),
            }


# ═══════════════════════════════════════════════════════════════════════
#  Self-Learning
# ═══════════════════════════════════════════════════════════════════════


class SelfLearningModel:
    """Online anomaly-detection model using EWMA and Z-score.

    Maintains a sliding window of recent values and computes:

    * **EWMA** — exponentially-weighted moving average for prediction.
    * **Z-score** — deviation from the mean relative to std-dev for
      anomaly detection.

    :param device_id: Device identifier this model is attached to.
    :param point_name: Data-point name (e.g. ``"temperature"``).
    :param window_size: Sliding-window size (defaults to 100).
    """

    def __init__(
        self,
        device_id: str,
        point_name: str,
        window_size: int = DEFAULT_WINDOW_SIZE,
    ) -> None:
        self.device_id = device_id
        self.point_name = point_name
        self.window_size = (
            window_size if window_size > 0 else DEFAULT_WINDOW_SIZE
        )
        self.values: list[float] = []
        self._ewma = 0.0
        self._ewma_initialized = False
        self.ewma_alpha = DEFAULT_EWMA_ALPHA
        self.threshold = DEFAULT_ANOMALY_THRESHOLD
        self.std_dev = 0.0
        self.last_anomaly: float = 0.0
        self.anomaly_count = 0
        self.total_samples = 0
        self._lock = threading.RLock()

    @property
    def ewma(self) -> float:
        """Current EWMA value (thread-safe read)."""
        with self._lock:
            return self._ewma

    def add_sample(self, value: float) -> bool:
        """Add a new sample and return whether it is an anomaly.

        :param value: The observed numeric value.
        :returns: ``True`` if the value is flagged as anomalous.
        """
        with self._lock:
            self.total_samples += 1
            self.values.append(value)
            if len(self.values) > self.window_size:
                self.values = self.values[1:]
            if len(self.values) < MIN_SAMPLES_FOR_ANOMALY:
                self._update_ewma(value)
                return False
            mean, std = self._calculate_stats()
            self._update_ewma(value)
            is_anomaly = False
            if std > 0:
                z_score = abs(value - mean) / std
                if z_score > self.threshold:
                    is_anomaly = True
                    self.anomaly_count += 1
                    self.last_anomaly = time.time()
            return is_anomaly

    def _update_ewma(self, value: float) -> None:
        """Update the EWMA with a new value (assumes lock is held)."""
        if not self._ewma_initialized:
            self._ewma = value
            self._ewma_initialized = True
        else:
            self._ewma = (
                self.ewma_alpha * value + (1 - self.ewma_alpha) * self._ewma
            )

    def _calculate_stats(self) -> tuple[float, float]:
        """Compute mean and std-dev of the current window.

        :returns: Tuple of (mean, std_dev).
        """
        n = len(self.values)
        if n == 0:
            return 0.0, 0.0
        mean = sum(self.values) / n
        variance = sum((v - mean) ** 2 for v in self.values) / n
        std = math.sqrt(variance)
        self.std_dev = std
        return mean, std

    def predict(self) -> float:
        """Return the predicted next value (current EWMA)."""
        with self._lock:
            return self._ewma

    def get_confidence(self) -> float:
        """Return a confidence score in ``[0, 1]``.

        Lower coefficient of variation (CV) yields higher confidence.
        Returns 0 if insufficient data or zero mean.
        """
        with self._lock:
            if (
                len(self.values) < MIN_SAMPLES_FOR_ANOMALY
                or self.std_dev == 0
            ):
                return 0.0
            mean, _ = self._calculate_stats()
            if mean == 0:
                return 0.0
            cv = self.std_dev / abs(mean)
            return min(1.0 / (1.0 + cv), 1.0)

    def get_stats(self) -> JSONDict:
        """Return a JSON-serializable snapshot of model state."""
        with self._lock:
            mean, std = self._calculate_stats()
            last_anomaly_str = ""
            if self.last_anomaly > 0:
                last_anomaly_str = datetime.fromtimestamp(
                    self.last_anomaly, tz=UTC
                ).isoformat()
            return {
                "device_id": self.device_id,
                "point_name": self.point_name,
                "total_samples": self.total_samples,
                "window_size": len(self.values),
                "max_window": self.window_size,
                "mean": mean,
                "std_dev": std,
                "ewma": self._ewma,
                "anomaly_count": self.anomaly_count,
                "last_anomaly": last_anomaly_str,
                "confidence": self.get_confidence(),
                # The threshold is the one knob the operator sets by hand, so a
                # registry that cannot report it leaves "设置阈值" unverifiable.
                "threshold": self.threshold,
            }

    def reset(self) -> None:
        """Reset all learned state to initial values."""
        with self._lock:
            self.values = []
            self._ewma = 0.0
            self._ewma_initialized = False
            self.std_dev = 0.0
            self.anomaly_count = 0
            self.total_samples = 0
            self.last_anomaly = 0.0

    def set_threshold(self, threshold: float) -> None:
        """Set the Z-score threshold for anomaly detection.

        :param threshold: New threshold (typically 2.0–5.0).
        """
        with self._lock:
            self.threshold = threshold


class SelfLearningManager:
    """Registry for :class:`SelfLearningModel` instances keyed by device:point.

    Provides thread-safe creation, lookup, and bulk statistics retrieval.
    """

    def __init__(self) -> None:
        self._models: dict[str, SelfLearningModel] = {}
        self._lock = threading.RLock()

    @staticmethod
    def _key(device_id: str, point_name: str) -> str:
        """Build the registry key from device and point identifiers."""
        return f"{device_id}:{point_name}"

    def get_or_create(
        self,
        device_id: str,
        point_name: str,
        window_size: int = DEFAULT_WINDOW_SIZE,
    ) -> SelfLearningModel:
        """Get an existing model or create a new one atomically.

        :param device_id: Device identifier.
        :param point_name: Data-point name.
        :param window_size: Window size for new models.
        :returns: The (existing or newly created) model.
        """
        key = self._key(device_id, point_name)
        with self._lock:
            if key not in self._models:
                self._models[key] = SelfLearningModel(
                    device_id, point_name, window_size
                )
            return self._models[key]

    def get_model(
        self, device_id: str, point_name: str
    ) -> SelfLearningModel | None:
        """Look up an existing model, or ``None`` if not found."""
        with self._lock:
            return self._models.get(self._key(device_id, point_name))

    def get_all_stats(self) -> list[JSONDict]:
        """Return statistics for all registered models."""
        with self._lock:
            models = list(self._models.values())
        return [m.get_stats() for m in models]


# ═══════════════════════════════════════════════════════════════════════
#  ONNX model wrapper
# ═══════════════════════════════════════════════════════════════════════


class OnnxSession(Protocol):
    """Protocol for ONNX Runtime inference sessions (duck typing)."""

    def run(
        self,
        output_names: list[str] | None,
        input_feed: dict[str, np.ndarray[Any, Any]],
    ) -> list[np.ndarray[Any, Any]]:
        """Run inference."""
        ...

    def get_inputs(self) -> list[Any]:
        """Return input metadata."""
        ...


class OnnxModelWrapper:
    """Wrapper around an ONNX model with lifecycle management.

    Encapsulates model metadata, ONNX Runtime session, load/unload,
    version tracking, and serialisation.

    :param model_id: Unique identifier for this model.
    :param model_name: Human-readable name.
    :param model_version: Semantic version string (e.g. ``"v1.0.0"``).
    :param model_type: Category (``"anomaly"``, ``"trend"``, etc.).
    :param is_preset: Whether this is a built-in preset model.
    :param model_path: Filesystem path to the ``.onnx`` file.
    :param input_schema: Dict describing input shape and dtype.
    :param output_schema: Dict describing output shape and dtype.
    """

    def __init__(
        self,
        model_id: str,
        model_name: str,
        model_version: str,
        model_type: str,
        is_preset: bool,
        model_path: str,
        input_schema: JSONDict | None = None,
        output_schema: JSONDict | None = None,
    ) -> None:
        self.model_id = model_id
        self.model_name = model_name
        self.model_version = model_version
        self.model_type = model_type
        self.is_preset = is_preset
        self.model_path = model_path
        self.input_schema = input_schema or {}
        self.output_schema = output_schema or {}
        self.status: str = STATUS_INACTIVE
        self.session: OnnxSession | None = None
        self.loaded_at: datetime | None = None
        self.last_result: InferenceResult | None = None
        self._version_history: list[JSONDict] = []
        self._last_mtime: float = 0.0
        self.inference_count = 0
        self.error_count = 0
        self.avg_latency_ms = 0.0

    async def load(self, provider: str = PROVIDER_CPU) -> None:
        """Load the ONNX model into an inference session.

        Sets status to ``"active"`` on success or ``"error"`` on failure.

        :param provider: Execution provider name (``"CPU"``, ``"CUDA"``,
            ``"OpenVINO"``).
        """
        try:
            self.status = STATUS_LOADING
            providers = self._build_providers(provider)
            self.session = await asyncio.to_thread(
                lambda: ort.InferenceSession(self.model_path, providers=providers)
            )
            self.status = STATUS_ACTIVE
            self.loaded_at = datetime.now(UTC)
            logger.info("Model loaded: %s", self.model_id)
        except Exception as e:
            self.status = STATUS_ERROR
            self.session = None
            logger.error("Model load failed: %s - %s", self.model_id, e)

    @staticmethod
    def _build_providers(provider: str) -> list[str]:
        """Build the ONNX Runtime providers list with fallback.

        :param provider: Primary execution provider.
        :returns: Ordered list of providers (primary first, CPU fallback).
        """
        if provider == PROVIDER_CUDA:
            return ["CUDAExecutionProvider", "CPUExecutionProvider"]
        if provider == PROVIDER_OPENVINO:
            return ["OpenVINOExecutionProvider", "CPUExecutionProvider"]
        return ["CPUExecutionProvider"]

    async def unload(self) -> None:
        """Unload the inference session and mark the model as inactive."""
        if self.session is not None:
            del self.session
        self.session = None
        self.status = STATUS_INACTIVE
        self.loaded_at = None

    def to_dict(self) -> JSONDict:
        """Serialise model metadata to a JSON-safe dict."""
        return {
            "model_id": self.model_id,
            "model_name": self.model_name,
            "model_version": self.model_version,
            "model_type": self.model_type,
            "is_preset": self.is_preset,
            "model_path": self.model_path,
            "status": self.status,
            "loaded_at": self.loaded_at.isoformat() if self.loaded_at else "",
            "input_schema": self._serialize_schema(self.input_schema),
            "output_schema": self._serialize_schema(self.output_schema),
            "inference_count": self.inference_count,
            "error_count": self.error_count,
            "avg_latency_ms": self.avg_latency_ms,
        }

    @staticmethod
    def _serialize_schema(schema: JSONDict) -> JSONDict:
        """Serialise schema values to JSON-safe strings.

        :param schema: Dict potentially containing lists/dicts as values.
        :returns: Dict with all values converted to strings.
        """
        result: JSONDict = {}
        for k, v in schema.items():
            if isinstance(v, (list, dict)):
                result[str(k)] = json.dumps(v)
            else:
                result[str(k)] = str(v)
        return result

    def get_version_history(self) -> list[JSONDict]:
        """Return a copy of the version history."""
        return list(self._version_history)


# ═══════════════════════════════════════════════════════════════════════
#  Preset model definitions
# ═══════════════════════════════════════════════════════════════════════

PRESET_MODELS: list[JSONDict] = [
    {
        "model_id": "preset-anomaly-v1",
        "model_name": "Anomaly Detection v1",
        "model_version": "v1.0.0",
        "model_type": "anomaly",
        "model_file": "elg-anomaly-v1.onnx",
        "input_schema": {"shape": [1, 100], "dtype": "float32"},
        "output_schema": {
            "shape": [1],
            "dtype": "float32",
            "description": "anomaly score 0-1",
        },
    },
    {
        "model_id": "preset-trend-v1",
        "model_name": "Trend Prediction v1",
        "model_version": "v1.0.0",
        "model_type": "trend",
        "model_file": "elg-trend-v1.onnx",
        "input_schema": {"shape": [1, 200], "dtype": "float32"},
        "output_schema": {
            "shape": [1, 10],
            "dtype": "float32",
            "description": "next 10 steps",
        },
    },
    {
        "model_id": "preset-threshold-v1",
        "model_name": "Dynamic Threshold v1",
        "model_version": "v1.0.0",
        "model_type": "threshold",
        "model_file": "elg-threshold-v1.onnx",
        "input_schema": {"shape": [1, 50], "dtype": "float32"},
        "output_schema": {
            "shape": [1],
            "dtype": "float32",
            "description": "optimal threshold",
        },
    },
]


def _generate_onnx_model(
    model_file: str,
    input_shape: list[int],
    output_shape: list[int],
) -> bytes | None:
    """Generate a small ONNX model bytes blob for a preset.

    Builds a simple feed-forward graph whose weights are chosen so
    that the model produces meaningful (non-zero) outputs for testing.

    :param model_file: Filename, used to determine the model type.
    :param input_shape: Shape of the input tensor.
    :param output_shape: Shape of the output tensor.
    :returns: Serialised ONNX model bytes, or ``None`` on failure.
    """
    if not _HAS_ONNX_PKG:
        return None
    in_dim = input_shape[-1]
    out_dim = output_shape[-1]
    X = helper.make_tensor_value_info("input", TensorProto.FLOAT, input_shape)
    Y = helper.make_tensor_value_info("output", TensorProto.FLOAT, output_shape)

    if "anomaly" in model_file:
        graph = _build_anomaly_graph(X, Y, in_dim, out_dim)
    elif "trend" in model_file:
        graph = _build_trend_graph(X, Y, in_dim, out_dim)
    elif "threshold" in model_file:
        graph = _build_threshold_graph(X, Y, in_dim, out_dim)
    else:
        graph = _build_default_graph(X, Y, in_dim, out_dim)

    model = helper.make_model(
        graph, opset_imports=[helper.make_opsetid("", ONNX_OPSET_VERSION)]
    )
    model.ir_version = ONNX_IR_VERSION
    model.model_version = 1
    model.doc_string = model_file
    try:
        if onnx is not None:
            onnx.checker.check_model(model)
    except Exception as e:
        logger.error("ONNX model validation failed for %s: %s", model_file, e)
        return None
    serialized: bytes | None = model.SerializeToString()
    return serialized


def _build_anomaly_graph(X: Any, Y: Any, in_dim: int, out_dim: int) -> Any:
    """Build a 2-layer MLP with ReLU + Sigmoid for anomaly scoring."""
    rng = np.random.RandomState(ONNX_RANDOM_SEED)
    hidden = min(64, in_dim // 2)
    W1 = (rng.randn(in_dim, hidden) * 0.1).astype(np.float32)
    b1 = np.zeros(hidden, dtype=np.float32)
    W2 = (rng.randn(hidden, out_dim) * 0.5).astype(np.float32)
    b2 = np.full(out_dim, 0.5, dtype=np.float32)
    nodes = [
        helper.make_node("MatMul", ["input", "W1"], ["h1"]),
        helper.make_node("Add", ["h1", "b1"], ["h1_pre"]),
        helper.make_node("Relu", ["h1_pre"], ["h1_act"]),
        helper.make_node("MatMul", ["h1_act", "W2"], ["h2"]),
        helper.make_node("Add", ["h2", "b2"], ["h2_pre"]),
        helper.make_node("Sigmoid", ["h2_pre"], ["output"]),
    ]
    inits = [
        numpy_helper.from_array(W1, name="W1"),
        numpy_helper.from_array(b1, name="b1"),
        numpy_helper.from_array(W2, name="W2"),
        numpy_helper.from_array(b2, name="b2"),
    ]
    return helper.make_graph(
        nodes, "anomaly_graph", [X], [Y], initializer=inits
    )


def _build_trend_graph(X: Any, Y: Any, in_dim: int, out_dim: int) -> Any:
    """Build a linear regression graph for trend prediction."""
    W = np.zeros((in_dim, out_dim), dtype=np.float32)
    for i in range(out_dim):
        start_idx = in_dim - out_dim - i
        for j in range(min(3, out_dim)):
            if 0 <= start_idx + j < in_dim:
                W[start_idx + j, i] = 0.8 - j * 0.2
    b = np.zeros(out_dim, dtype=np.float32)
    nodes = [
        helper.make_node("MatMul", ["input", "W"], ["linear_out"]),
        helper.make_node("Add", ["linear_out", "b"], ["output"]),
    ]
    inits = [
        numpy_helper.from_array(W, name="W"),
        numpy_helper.from_array(b, name="b"),
    ]
    return helper.make_graph(
        nodes, "trend_graph", [X], [Y], initializer=inits
    )


def _build_threshold_graph(X: Any, Y: Any, in_dim: int, out_dim: int) -> Any:
    """Build a pass-through graph that outputs the last input value."""
    W = np.zeros((in_dim, out_dim), dtype=np.float32)
    W[-1, 0] = 1.0
    b = np.zeros(out_dim, dtype=np.float32)
    nodes = [
        helper.make_node("MatMul", ["input", "W"], ["matmul_out"]),
        helper.make_node("Add", ["matmul_out", "b"], ["output"]),
    ]
    inits = [
        numpy_helper.from_array(W, name="W"),
        numpy_helper.from_array(b, name="b"),
    ]
    return helper.make_graph(
        nodes, "threshold_graph", [X], [Y], initializer=inits
    )


def _build_default_graph(X: Any, Y: Any, in_dim: int, out_dim: int) -> Any:
    """Build a default linear graph for unknown model types."""
    rng = np.random.RandomState(ONNX_RANDOM_SEED)
    W = rng.randn(in_dim, out_dim).astype(np.float32) * 0.01
    b = rng.randn(out_dim).astype(np.float32) * 0.1 + 0.5
    nodes = [
        helper.make_node("MatMul", inputs=["input", "W"], outputs=["m"]),
        helper.make_node("Add", inputs=["m", "b"], outputs=["output"]),
    ]
    inits = [
        numpy_helper.from_array(W, name="W"),
        numpy_helper.from_array(b, name="b"),
    ]
    return helper.make_graph(
        nodes, "preset_graph", [X], [Y], initializer=inits
    )


# ═══════════════════════════════════════════════════════════════════════
#  HTTP API Server
# ═══════════════════════════════════════════════════════════════════════

# Type for aiohttp middleware handler
Handler = Callable[[web.Request], Awaitable[web.StreamResponse]]


class AISidecarServer:
    """Main AI sidecar server managing models, inference, and self-learning.

    :param models_dir: Directory for ONNX model files.
    :param config: Optional configuration overrides.
    """

    def __init__(
        self, models_dir: str, config: JSONDict | None = None
    ) -> None:
        self._models_dir = Path(models_dir)
        self._models_dir.mkdir(parents=True, exist_ok=True)
        self._loaded_models: dict[str, OnnxModelWrapper] = {}
        self._stats = InferenceStatsCollector()
        self._execution_provider = PROVIDER_CPU
        self._config = config or {}
        self._self_learner = SelfLearningManager()
        self._lock = asyncio.Lock()
        self._scheduled_tasks: dict[str, asyncio.Task[None]] = {}
        self._scheduled_configs: dict[str, JSONDict] = {}

    async def initialize(self) -> None:
        """Load preset models and prepare the server for traffic."""
        await self._load_preset_models()
        logger.info(
            "AI sidecar initialized, %d models loaded",
            len(self._loaded_models),
        )

    # ── Preset model loading ──────────────────────────────────────────

    async def _load_preset_models(self) -> None:
        """Load all preset models defined in :data:`PRESET_MODELS`."""
        for preset in PRESET_MODELS:
            await self._load_single_preset(preset)

    async def _load_single_preset(self, preset: JSONDict) -> None:
        """Load a single preset model, generating the ONNX file if needed."""
        model_path = str(self._models_dir / preset["model_file"])
        file_exists = Path(model_path).exists()
        if not file_exists and self._try_generate_preset(preset):
            file_exists = True
        wrapper = OnnxModelWrapper(
            model_id=preset["model_id"],
            model_name=preset["model_name"],
            model_version=preset["model_version"],
            model_type=preset["model_type"],
            is_preset=True,
            model_path=model_path,
            input_schema=preset["input_schema"],
            output_schema=preset["output_schema"],
        )
        if not file_exists:
            wrapper.status = STATUS_UNAVAILABLE
        else:
            await wrapper.load(provider=self._execution_provider)
        self._loaded_models[preset["model_id"]] = wrapper

    def _try_generate_preset(self, preset: JSONDict) -> bool:
        """Attempt to generate a preset ONNX model file.

        :param preset: Preset model definition dict.
        :returns: ``True`` if the file was successfully generated.
        """
        try:
            ishape = preset["input_schema"].get("shape", [1, 1])
            oshape = preset["output_schema"].get("shape", [1, 1])
            model_bytes = _generate_onnx_model(
                preset["model_file"], ishape, oshape
            )
            if model_bytes is None:
                return False
            target = self._models_dir / preset["model_file"]
            target.parent.mkdir(parents=True, exist_ok=True)
            fd, tmp = tempfile.mkstemp(
                dir=str(target.parent), suffix=".onnx.tmp"
            )
            try:
                with os.fdopen(fd, "wb") as f:
                    f.write(model_bytes)
                os.replace(tmp, target)
            except BaseException:
                with contextlib.suppress(OSError):
                    os.unlink(tmp)
                raise
            return True
        except Exception as e:
            logger.warning(
                "Preset gen failed %s: %s", preset["model_file"], e
            )
            return False

    def _auto_increment_version(self, w: OnnxModelWrapper) -> str:
        """Auto-increment the patch version of a model.

        :param w: Model wrapper whose version to increment.
        :returns: New version string (e.g. ``"v1.0.1"`` -> ``"v1.0.2"``).
        """
        try:
            if w.model_version.startswith("v"):
                parts = w.model_version[1:].split(".")
                if len(parts) == 3:
                    return f"v{parts[0]}.{parts[1]}.{int(parts[2]) + 1}"
        except (ValueError, IndexError):
            pass
        return w.model_version

    def _record_version(
        self, w: OnnxModelWrapper, version: str | None = None
    ) -> None:
        """Append a snapshot to the model's version history."""
        w._version_history.append({
            "version": version or w.model_version,
            "model_path": w.model_path,
            "timestamp": datetime.now(UTC).isoformat(),
            "status": w.status,
        })
        if len(w._version_history) > MAX_VERSION_HISTORY:
            w._version_history = w._version_history[-MAX_VERSION_HISTORY:]

    # ── JSON parsing ──────────────────────────────────────────────────

    async def _parse_json(
        self, request: web.Request
    ) -> web.Response | JSONDict:
        """Parse JSON body safely.

        :param request: The HTTP request.
        :returns: Parsed dict on success, or error Response on failure.
        """
        try:
            data = await request.json()
            if not isinstance(data, dict):
                return _error_response(
                    "ERR_INVALID_REQUEST",
                    "Request body must be a JSON object",
                    status=400,
                )
            return data
        except json.JSONDecodeError:
            return _error_response(
                "ERR_INVALID_JSON", "Invalid JSON body", status=400
            )
        except Exception as e:
            return _error_response(
                "ERR_INVALID_REQUEST", str(e), status=400
            )

    # ── Health & metrics handlers ─────────────────────────────────────

    async def handle_health(self, request: web.Request) -> web.Response:
        """Liveness probe — returns 200 if the process is alive."""
        async with self._lock:
            loaded = len(self._loaded_models)
        return web.json_response({
            "healthy": True,
            "status": "alive",
            "version": SERVER_VERSION,
            "execution_provider": self._execution_provider,
            "onnx_available": _HAS_ONNX_PKG,
            "numpy_available": True,
            "loaded_model_count": loaded,
            "uptime_seconds": round(
                time.time() - _METRICS.get("uptime_start", time.time()), 2
            ),
        })

    async def handle_ready(self, request: web.Request) -> web.Response:
        """Readiness probe — returns 200 only if at least one model is active."""
        async with self._lock:
            active_count = sum(
                1 for w in self._loaded_models.values()
                if w.status == STATUS_ACTIVE
            )
        ready = active_count > 0
        return web.json_response(
            {"ready": ready, "active_model_count": active_count},
            status=200 if ready else 503,
        )

    async def handle_metrics(self, request: web.Request) -> web.Response:
        """Prometheus metrics endpoint (text exposition format)."""
        async with self._lock:
            _metric_set("models_loaded", len(self._loaded_models))
        return web.Response(
            text=_render_prometheus(),
            content_type="text/plain",
            charset="utf-8",
        )

    # ── Model management handlers ─────────────────────────────────────

    async def handle_list_models(self, request: web.Request) -> web.Response:
        """List all registered models."""
        async with self._lock:
            models = [w.to_dict() for w in self._loaded_models.values()]
        return web.json_response({"models": models})

    async def handle_load_model(self, request: web.Request) -> web.Response:
        """Load a new model from a file path."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        async with self._lock:
            if model_id in self._loaded_models:
                return _error_response(
                    "ERR_AI_MODEL_ALREADY_LOADED",
                    "Model already loaded",
                )
            wrapper = OnnxModelWrapper(
                model_id=model_id,
                model_name=data.get("model_name", ""),
                model_version=data.get("model_version", "v1.0.0"),
                model_type=data.get("model_type", ""),
                is_preset=data.get("is_preset", False),
                model_path=data.get("model_path", ""),
                input_schema=data.get("input_schema", {}),
                output_schema=data.get("output_schema", {}),
            )
            await wrapper.load(provider=self._execution_provider)
            self._loaded_models[model_id] = wrapper
        return _success_response(model_info=wrapper.to_dict())

    async def handle_unload_model(self, request: web.Request) -> web.Response:
        """Unload a model from memory."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        async with self._lock:
            wrapper = self._loaded_models.pop(model_id, None)
        if wrapper:
            await wrapper.unload()
            return _success_response()
        return _error_response(
            "ERR_AI_MODEL_NOT_FOUND", "Model not found"
        )

    async def handle_reload_model(self, request: web.Request) -> web.Response:
        """Hot-reload a model (auto version bump)."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        model_path = data.get("model_path", "")
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
            if wrapper is None:
                return _error_response(
                    "ERR_AI_MODEL_NOT_FOUND", "Model not found"
                )
            self._record_version(wrapper)
            wrapper.status = STATUS_LOADING
            if model_path:
                wrapper.model_path = model_path
        try:
            await wrapper.unload()
            await wrapper.load(provider=self._execution_provider)
            new_version = self._auto_increment_version(wrapper)
            wrapper.model_version = new_version
            self._record_version(wrapper)
            return _success_response(new_version=new_version)
        except Exception as e:
            wrapper.status = STATUS_UNAVAILABLE
            return _error_response(
                "ERR_AI_MODEL_RELOAD_FAILED", str(e)
            )

    async def handle_enable_model(self, request: web.Request) -> web.Response:
        """Enable a model (load if needed)."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
        if not wrapper:
            return _error_response(
                "ERR_AI_MODEL_NOT_FOUND", "Model not found"
            )
        return await self._enable_wrapper(wrapper, model_id)

    async def _enable_wrapper(
        self, wrapper: OnnxModelWrapper, model_id: str
    ) -> web.Response:
        """Handle the state-machine for enabling a model wrapper.

        :param wrapper: The model wrapper to enable.
        :param model_id: Model identifier (for preset lookup).
        :returns: HTTP response indicating success or specific error.
        """
        if wrapper.status == STATUS_LOADING:
            return _error_response(
                "ERR_AI_MODEL_IS_LOADING", "Model is loading"
            )
        if wrapper.status == STATUS_ACTIVE:
            return _success_response()
        if wrapper.status == STATUS_UNAVAILABLE:
            return await self._enable_unavailable(wrapper, model_id)
        if wrapper.status in (STATUS_ERROR, STATUS_INACTIVE):
            return await self._enable_from_error(wrapper)
        return _success_response()

    async def _enable_unavailable(
        self, wrapper: OnnxModelWrapper, model_id: str
    ) -> web.Response:
        """Enable a model in 'unavailable' status by (re)generating and loading."""
        mp = Path(wrapper.model_path)
        if not mp.exists() and wrapper.is_preset:
            preset = next(
                (p for p in PRESET_MODELS if p["model_id"] == model_id),
                None,
            )
            if preset:
                self._try_generate_preset(preset)
        if not mp.exists():
            return _error_response(
                "ERR_AI_MODEL_FILE_NOT_FOUND", "Model file not found"
            )
        await wrapper.load()
        if wrapper.status != STATUS_ACTIVE:
            return _error_response(
                "ERR_AI_MODEL_CANNOT_LOAD", "Model cannot be loaded"
            )
        return _success_response()

    async def _enable_from_error(
        self, wrapper: OnnxModelWrapper
    ) -> web.Response:
        """Enable a model in 'error' or 'inactive' status."""
        await wrapper.load()
        if wrapper.status != STATUS_ACTIVE:
            if wrapper.status == STATUS_ERROR:
                return _error_response(
                    "ERR_AI_MODEL_PREVIOUS_ERROR",
                    "Model has previous error",
                )
            return _error_response(
                "ERR_AI_MODEL_ENABLE_FAILED", "Model enable failed"
            )
        return _success_response()

    async def handle_disable_model(self, request: web.Request) -> web.Response:
        """Disable a model (unload from memory)."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
        if wrapper:
            await wrapper.unload()
            return _success_response()
        return _error_response(
            "ERR_AI_MODEL_NOT_FOUND", "Model not found"
        )

    async def handle_remove_model(self, request: web.Request) -> web.Response:
        """Remove a model permanently (unload + deregister)."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        async with self._lock:
            wrapper = self._loaded_models.pop(model_id, None)
        if wrapper:
            await wrapper.unload()
            await self._stop_scheduled(model_id)
            return _success_response()
        return _error_response(
            "ERR_AI_MODEL_NOT_FOUND", "Model not found"
        )

    async def handle_get_model_status(
        self, request: web.Request
    ) -> web.Response:
        """Get the current status of a model."""
        model_id = request.match_info["model_id"]
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
        return web.json_response({
            "status": wrapper.status if wrapper else ""
        })

    # ── Inference handler ─────────────────────────────────────────────

    async def handle_infer(self, request: web.Request) -> web.Response:
        """Run inference on a loaded model.

        Accepts ``model_id`` and ``input_data`` (list of floats).
        Returns ``output_data``, ``latency_ms``, and ``status``.
        Falls back to cached result on timeout.
        """
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = data.get("model_id", "")
        if not model_id:
            return _error_response(
                "ERR_AI_MODEL_ID_REQUIRED",
                "model_id is required",
                status=400,
                model_id=model_id,
                status_text="error",
            )
        input_data = data.get("input_data", [])
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
        if wrapper is None or wrapper.status != STATUS_ACTIVE:
            return web.json_response({
                "model_id": model_id,
                "status": "error",
                "error_code": "ERR_AI_MODEL_NOT_AVAILABLE",
                "error_message": f"Model not available: {model_id}",
            })
        return await self._run_inference(wrapper, model_id, input_data)

    async def _run_inference(
        self,
        wrapper: OnnxModelWrapper,
        model_id: str,
        input_data: list[Any],
    ) -> web.Response:
        """Execute inference and handle timeout/error fallbacks.

        :param wrapper: The active model wrapper.
        :param model_id: Model identifier for stats.
        :param input_data: Flat list of float input values.
        :returns: HTTP response with inference results or error.
        """
        start = time.perf_counter()
        timeout = float(
            self._config.get("inference_timeout", DEFAULT_INFERENCE_TIMEOUT)
        )
        try:
            output_data, latency_ms = await self._execute_inference(
                wrapper, input_data, timeout
            )
            self._record_inference_success(wrapper, model_id, latency_ms)
            return web.json_response({
                "model_id": model_id,
                "output_data": output_data,
                "latency_ms": latency_ms,
                "status": "success",
            })
        except TimeoutError:
            latency_ms = int((time.perf_counter() - start) * 1000)
            self._record_inference_error(wrapper, model_id, latency_ms)
            logger.warning(
                "Inference timeout for model %s after %dms",
                model_id, latency_ms,
            )
            return self._handle_timeout(wrapper, model_id, latency_ms)
        except Exception as e:
            latency_ms = int((time.perf_counter() - start) * 1000)
            self._record_inference_error(wrapper, model_id, latency_ms)
            logger.error(
                "Inference error for model %s: %s",
                model_id, e, exc_info=True,
            )
            return web.json_response({
                "model_id": model_id,
                "latency_ms": latency_ms,
                "status": "error",
                "error_code": "ERR_AI_INFERENCE_FAILED",
                "error_message": str(e),
            })

    async def _execute_inference(
        self,
        wrapper: OnnxModelWrapper,
        input_data: list[Any],
        timeout: float,
    ) -> tuple[InferenceResult, int]:
        """Execute the raw ONNX inference call.

        :param wrapper: Active model wrapper with a loaded session.
        :param input_data: Flat list of float values.
        :param timeout: Maximum inference duration in seconds.
        :returns: Tuple of (output_data, latency_ms).
        :raises TimeoutError: If inference exceeds the timeout.
        """
        start = time.perf_counter()
        session = wrapper.session
        if session is None:
            raise RuntimeError("Session is None")
        arr = np.array(input_data, dtype=np.float32)
        input_name = session.get_inputs()[0].name
        onnx_input = session.get_inputs()[0]
        actual_shape = self._resolve_shape_dimensions(onnx_input.shape)
        flat = arr.flatten()

        if self._is_batch_mode(actual_shape, flat):
            output_data = await self._run_batch_inference(
                session, input_name, flat, timeout
            )
        else:
            output_data = await self._run_standard_inference(
                session, input_name, actual_shape, flat, timeout
            )
        latency_ms = int((time.perf_counter() - start) * 1000)
        wrapper.last_result = output_data
        return output_data, latency_ms

    @staticmethod
    def _resolve_shape_dimensions(shape: list[Any]) -> list[int]:
        """Resolve ONNX shape dimensions to integers.

        Dynamic dimensions (strings or None) become ``-1``.
        """
        result: list[int] = []
        for dim in shape or [1]:
            if isinstance(dim, str) or dim is None:
                result.append(-1)
            else:
                result.append(int(dim))
        return result

    @staticmethod
    def _is_batch_mode(actual_shape: list[int], flat: np.ndarray[Any, Any]) -> bool:
        """Determine whether to use batch inference mode.

        Batch mode is used when the model expects a single scalar
        but the input contains multiple values.
        """
        return (
            len(actual_shape) == 1
            and actual_shape[0] == 1
            and len(flat) > 1
        )

    async def _run_batch_inference(
        self,
        session: OnnxSession,
        input_name: str,
        flat: np.ndarray[Any, Any],
        timeout: float,
    ) -> InferenceResult:
        """Run inference in batch mode (one call per element)."""

        def _batch() -> list[list[np.ndarray[Any, Any]]]:
            return [
                session.run(
                    None, {input_name: np.array([v], dtype=np.float32)}
                )
                for v in flat
            ]

        batch_outputs = await asyncio.wait_for(
            asyncio.to_thread(_batch), timeout=timeout
        )
        output_data: dict[str, list[Any]] = {}
        for i in range(len(batch_outputs[0])):
            vals: list[Any] = []
            for out in batch_outputs:
                v: Any = out[i]
                if isinstance(v, np.ndarray):
                    v = v.flatten().tolist()
                    if len(v) == 1:
                        v = v[0]
                vals.append(v)
            output_data[f"output_{i}"] = vals
        return output_data

    async def _run_standard_inference(
        self,
        session: OnnxSession,
        input_name: str,
        actual_shape: list[int],
        flat: np.ndarray[Any, Any],
        timeout: float,
    ) -> InferenceResult:
        """Run inference in standard mode (reshaped input)."""
        expected_size = 1
        for dim in actual_shape:
            if dim > 0:
                expected_size *= dim
        if expected_size <= 0:
            expected_size = len(flat)
        if len(flat) != expected_size:
            if len(flat) < expected_size:
                flat = np.pad(flat, (0, expected_size - len(flat)))
            else:
                flat = np.asarray(flat[:expected_size], dtype=np.float32)
        resolve_shape = self._compute_resolve_shape(actual_shape, flat)
        arr = flat.reshape(resolve_shape)
        raw = await asyncio.wait_for(
            asyncio.to_thread(
                lambda: session.run(None, {input_name: arr})
            ),
            timeout=timeout,
        )
        output_data: dict[str, Any] = {}
        for i, out in enumerate(raw):
            if isinstance(out, np.ndarray):
                val = out.tolist()
                if (
                    isinstance(val, list)
                    and len(val) == 1
                    and isinstance(val[0], list)
                ):
                    val = val[0]
                output_data[f"output_{i}"] = val
            else:
                output_data[f"output_{i}"] = out
        return output_data

    @staticmethod
    def _compute_resolve_shape(
        actual_shape: list[int], flat: np.ndarray[Any, Any]
    ) -> list[int]:
        """Compute the final shape, resolving dynamic dimensions."""
        resolve_shape: list[int] = []
        for idx, dim in enumerate(actual_shape):
            if dim > 0:
                resolve_shape.append(dim)
            else:
                known = 1
                for j, d2 in enumerate(actual_shape):
                    if j != idx and d2 > 0:
                        known *= d2
                resolve_shape.append(
                    max(1, len(flat) // known) if known > 0 else 1
                )
        return resolve_shape

    def _record_inference_success(
        self, wrapper: OnnxModelWrapper, model_id: str, latency_ms: int
    ) -> None:
        """Record a successful inference in stats and metrics."""
        wrapper.inference_count += 1
        self._stats.record_inference(model_id, latency_ms, "success")
        _metric_inc("inference_total")
        _metric_inc("inference_latency_sum_ms", latency_ms)
        _metric_inc("inference_latency_count")

    def _record_inference_error(
        self, wrapper: OnnxModelWrapper, model_id: str, latency_ms: int
    ) -> None:
        """Record a failed inference in stats and metrics."""
        wrapper.error_count += 1
        self._stats.record_inference(model_id, latency_ms, "error")
        _metric_inc("inference_total")
        _metric_inc("inference_errors")
        _metric_inc("inference_latency_sum_ms", latency_ms)
        _metric_inc("inference_latency_count")

    def _handle_timeout(
        self,
        wrapper: OnnxModelWrapper,
        model_id: str,
        latency_ms: int,
    ) -> web.Response:
        """Build a timeout response, with cached result if available."""
        cached = wrapper.last_result
        if cached:
            logger.warning(
                "Returning cached result for model %s due to timeout",
                model_id,
            )
            return web.json_response({
                "model_id": model_id,
                "output_data": cached,
                "latency_ms": latency_ms,
                "status": "degraded",
                "error_code": "ERR_AI_INFERENCE_TIMEOUT",
                "error_message": "Inference timeout, returning cached result",
            })
        return web.json_response({
            "model_id": model_id,
            "latency_ms": latency_ms,
            "status": "error",
            "error_code": "ERR_AI_INFERENCE_TIMEOUT",
            "error_message": "Inference timeout",
        })

    # ── Scheduled inference handlers ──────────────────────────────────

    async def handle_start_scheduled(
        self, request: web.Request
    ) -> web.Response:
        """Start a scheduled inference loop for a model."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        async with self._lock:
            if model_id in self._scheduled_tasks:
                return _error_response(
                    "ERR_AI_SCHEDULED_ALREADY_RUNNING",
                    "Scheduled inference already running",
                )
            wrapper = self._loaded_models.get(model_id)
            if wrapper is None or wrapper.status != STATUS_ACTIVE:
                return _error_response(
                    "ERR_AI_MODEL_NOT_AVAILABLE", "Model not available"
                )
            interval = data.get("interval_seconds", DEFAULT_SCHEDULED_INTERVAL)
            self._scheduled_configs[model_id] = {
                "model_id": model_id,
                "device_id": data.get("device_id", ""),
                "point_name": data.get("point_name", ""),
                "interval_seconds": interval,
                "input_window_size": data.get("input_window_size", 100),
            }
            self._scheduled_tasks[model_id] = asyncio.create_task(
                self._scheduled_loop(model_id, interval)
            )
        return _success_response()

    async def _scheduled_loop(self, model_id: str, interval: int) -> None:
        """Background loop for scheduled inference.

        :param model_id: Model to run inference for.
        :param interval: Sleep interval in seconds.
        """
        logger.info(
            "Scheduled inference loop started for model %s (interval=%ds)",
            model_id, interval,
        )
        try:
            while model_id in self._scheduled_tasks:
                await asyncio.sleep(
                    interval if interval > 0 else DEFAULT_SCHEDULED_INTERVAL
                )
                # NOTE: Actual inference execution would go here.
                # The Go gateway triggers inference via /infer when needed.
        except asyncio.CancelledError:
            logger.info(
                "Scheduled inference loop cancelled for model %s", model_id
            )
        finally:
            self._scheduled_tasks.pop(model_id, None)
            self._scheduled_configs.pop(model_id, None)

    async def _stop_scheduled(self, model_id: str) -> bool:
        """Stop a scheduled inference task.

        :param model_id: Model whose scheduled task should be stopped.
        :returns: ``True`` if a task was stopped.
        """
        task = self._scheduled_tasks.pop(model_id, None)
        self._scheduled_configs.pop(model_id, None)
        if task and not task.done():
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task
            return True
        return False

    async def handle_stop_scheduled(
        self, request: web.Request
    ) -> web.Response:
        """Stop a scheduled inference loop."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        return _success_response(
            success=await self._stop_scheduled(model_id)
        )

    # ── Self-learning handlers ────────────────────────────────────────

    async def handle_add_sample(self, request: web.Request) -> web.Response:
        """Add a sample to the self-learning model."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        device_id = data.get("device_id", "")
        point_name = data.get("point_name", "")
        if not device_id or not point_name:
            return _error_response(
                "ERR_INVALID_REQUEST",
                "device_id and point_name are required",
                status=400,
            )
        value = data.get("value", 0.0)
        window_size = data.get("window_size", DEFAULT_WINDOW_SIZE)
        model = self._self_learner.get_or_create(
            device_id, point_name,
            window_size if window_size > 0 else DEFAULT_WINDOW_SIZE,
        )
        is_anomaly = model.add_sample(value)
        return web.json_response({
            "is_anomaly": is_anomaly,
            "confidence": model.get_confidence(),
        })

    async def handle_predict(self, request: web.Request) -> web.Response:
        """Predict the next value using EWMA."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        device_id = data.get("device_id", "")
        point_name = data.get("point_name", "")
        model = self._self_learner.get_model(device_id, point_name)
        if model is None:
            return web.json_response({
                "predicted_value": 0, "confidence": 0
            })
        return web.json_response({
            "predicted_value": model.predict(),
            "confidence": model.get_confidence(),
        })

    async def handle_self_learning_stats(
        self, request: web.Request
    ) -> web.Response:
        """Get statistics for a specific self-learning model."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        device_id = data.get("device_id", "")
        point_name = data.get("point_name", "")
        model = self._self_learner.get_model(device_id, point_name)
        if model is None:
            return web.json_response({"stats": None})
        return web.json_response({"stats": model.get_stats()})

    async def handle_all_self_learning_stats(
        self, request: web.Request
    ) -> web.Response:
        """Get statistics for all self-learning models."""
        return web.json_response({
            "stats": self._self_learner.get_all_stats()
        })

    async def handle_reset_self_learning(
        self, request: web.Request
    ) -> web.Response:
        """Reset a self-learning model."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        device_id = data.get("device_id", "")
        point_name = data.get("point_name", "")
        model = self._self_learner.get_model(device_id, point_name)
        if model:
            model.reset()
            return _success_response()
        return _error_response(
            "ERR_AI_SELF_LEARNING_NOT_FOUND",
            "Self-learning model not found",
        )

    async def handle_set_threshold(
        self, request: web.Request
    ) -> web.Response:
        """Set the anomaly threshold for a self-learning model."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        device_id = data.get("device_id", "")
        point_name = data.get("point_name", "")
        threshold = data.get("threshold", DEFAULT_ANOMALY_THRESHOLD)
        model = self._self_learner.get_or_create(
            device_id, point_name, DEFAULT_WINDOW_SIZE
        )
        model.set_threshold(threshold)
        return _success_response()

    # ── Statistics & provider handlers ────────────────────────────────

    async def handle_get_stats(self, request: web.Request) -> web.Response:
        """Get engine-wide statistics."""
        snapshot = self._stats.get_snapshot()
        async with self._lock:
            snapshot["loaded_models"] = len(self._loaded_models)
        return web.json_response({"stats": snapshot})

    async def handle_get_model_stats(
        self, request: web.Request
    ) -> web.Response:
        """Get per-model inference statistics."""
        model_id = request.match_info["model_id"]
        stats = self._stats.get_model_stats(model_id)
        return web.json_response(stats or {})

    async def handle_set_execution_provider(
        self, request: web.Request
    ) -> web.Response:
        """Switch the execution provider (reloads all active models)."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        provider = data.get("provider", PROVIDER_CPU)
        if provider not in VALID_PROVIDERS:
            return _error_response(
                "ERR_AI_INVALID_PROVIDER",
                f"Invalid provider: {provider}",
                status=400,
            )
        provider = self._validate_provider_available(provider)
        self._execution_provider = provider
        await self._reload_all_models(provider)
        return _success_response(actual_provider=provider)

    def _validate_provider_available(self, provider: str) -> str:
        """Check if the requested provider is available; fallback to CPU.

        :param provider: Requested provider name.
        :returns: Actual provider to use (may be CPU if requested unavailable).
        """
        if provider == PROVIDER_CPU:
            return PROVIDER_CPU
        provider_map = {
            PROVIDER_CUDA: "CUDAExecutionProvider",
            PROVIDER_OPENVINO: "OpenVINOExecutionProvider",
        }
        ort_name = provider_map.get(provider, "")
        if not ort_name:
            return PROVIDER_CPU
        try:
            available = ort.get_available_providers()
            if ort_name not in available:
                logger.warning(
                    "%s provider requested but not available, "
                    "falling back to CPU",
                    provider,
                )
                return PROVIDER_CPU
        except Exception:
            return PROVIDER_CPU
        return provider

    async def _reload_all_models(self, provider: str) -> None:
        """Reload all active models with a new provider.

        :param provider: New execution provider name.
        """
        async with self._lock:
            to_reload = [
                (mid, w)
                for mid, w in self._loaded_models.items()
                if w.status == STATUS_ACTIVE
            ]
            for _, w in to_reload:
                w.status = STATUS_LOADING
        for mid, w in to_reload:
            try:
                await w.unload()
                await w.load(provider=provider)
            except Exception as e:
                logger.error(
                    "Failed to reload model %s: %s",
                    mid, e, exc_info=True,
                )
                w.status = STATUS_ERROR

    async def handle_get_providers(
        self, request: web.Request
    ) -> web.Response:
        """List available execution providers."""
        providers = [PROVIDER_CPU]
        try:
            available = ort.get_available_providers()
            if "CUDAExecutionProvider" in available:
                providers.append(PROVIDER_CUDA)
            if "OpenVINOExecutionProvider" in available:
                providers.append(PROVIDER_OPENVINO)
        except Exception:
            pass
        return web.json_response({"providers": providers})

    async def handle_model_version_history(
        self, request: web.Request
    ) -> web.Response:
        """Get version history for a model."""
        model_id = request.match_info["model_id"]
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
        if not wrapper:
            return web.json_response({"history": []})
        return web.json_response({"history": wrapper.get_version_history()})

    async def handle_rollback_model(
        self, request: web.Request
    ) -> web.Response:
        """Rollback a model to a previous version."""
        data = await self._parse_json(request)
        if isinstance(data, web.Response):
            return data
        model_id = _require_model_id(data)
        if isinstance(model_id, web.Response):
            return model_id
        target_version = data.get("target_version", "")
        if not target_version:
            return _error_response(
                "ERR_AI_VERSION_REQUIRED",
                "target_version is required",
                status=400,
            )
        async with self._lock:
            wrapper = self._loaded_models.get(model_id)
            if not wrapper:
                return _error_response(
                    "ERR_AI_MODEL_NOT_FOUND", "Model not found"
                )
            target = self._find_version_in_history(
                wrapper, target_version
            )
            if not target:
                return _error_response(
                    "ERR_AI_VERSION_NOT_FOUND", "Version not found"
                )
            target_path = target.get("model_path", "")
            if not (target_path and Path(target_path).exists()):
                wrapper.model_version = target_version
                return _success_response()
            wrapper.status = STATUS_LOADING
        try:
            await wrapper.unload()
            wrapper.model_path = target_path
            wrapper.model_version = target_version
            await wrapper.load(provider=self._execution_provider)
            self._record_version(
                wrapper, f"rollback_to_{target_version}"
            )
            return _success_response()
        except Exception as e:
            wrapper.status = STATUS_UNAVAILABLE
            logger.error(
                "Rollback failed for model %s: %s",
                model_id, e, exc_info=True,
            )
            return _error_response(
                "ERR_AI_ROLLBACK_FAILED", str(e)
            )

    @staticmethod
    def _find_version_in_history(
        wrapper: OnnxModelWrapper, target_version: str
    ) -> JSONDict | None:
        """Find a version entry in the model's history.

        :param wrapper: Model wrapper to search.
        :param target_version: Version string to find.
        :returns: History entry dict, or ``None`` if not found.
        """
        for entry in wrapper.get_version_history():
            if entry.get("version") == target_version:
                return entry
        return None

    async def handle_generate_presets(
        self, request: web.Request
    ) -> web.Response:
        """Regenerate preset ONNX model files."""
        results: dict[str, bool] = {}
        for preset in PRESET_MODELS:
            model_path = str(self._models_dir / preset["model_file"])
            if Path(model_path).exists():
                results[preset["model_id"]] = True
                continue
            results[preset["model_id"]] = self._try_generate_preset(preset)
        return web.json_response({"results": results})


# ═══════════════════════════════════════════════════════════════════════
#  Middleware
# ═══════════════════════════════════════════════════════════════════════


@web.middleware
async def error_handling_middleware(
    request: web.Request, handler: Handler
) -> web.StreamResponse:
    """Global error-handling middleware for unhandled exceptions.

    Catches :class:`web.HTTPException` (404/405) and generic exceptions,
    returning standardised JSON error responses.
    """
    try:
        return await handler(request)
    except web.HTTPException as ex:
        if ex.status == 404:
            return _error_response(
                "ERR_ROUTE_NOT_FOUND",
                f"Route not found: {request.method} {request.path}",
                status=404,
            )
        if ex.status == 405:
            return _error_response(
                "ERR_METHOD_NOT_ALLOWED",
                f"Method not allowed: {request.method}",
                status=405,
            )
        raise
    except Exception as e:
        logger.error(
            "Unhandled error processing %s %s: %s",
            request.method, request.path, e, exc_info=True,
        )
        return _error_response(
            "ERR_INTERNAL_SERVER_ERROR",
            "Internal server error",
            status=500,
        )


@web.middleware
async def request_logging_middleware(
    request: web.Request, handler: Handler
) -> web.StreamResponse:
    """Log each request with method, path, and response time."""
    start = time.perf_counter()
    response = await handler(request)
    duration_ms = (time.perf_counter() - start) * 1000
    logger.debug(
        "%s %s -> %d (%.2fms)",
        request.method, request.path, response.status, duration_ms,
    )
    return response


# ═══════════════════════════════════════════════════════════════════════
#  Application factory
# ═══════════════════════════════════════════════════════════════════════


def create_app(sidecar: AISidecarServer) -> web.Application:
    """Create and configure the aiohttp application.

    Registers all routes and middleware.

    :param sidecar: The :class:`AISidecarServer` instance to bind.
    :returns: Configured :class:`web.Application`.
    """
    app = web.Application(
        middlewares=[error_handling_middleware, request_logging_middleware]
    )
    # Health & readiness
    app.router.add_get("/health", sidecar.handle_health)
    app.router.add_get("/health/live", sidecar.handle_health)
    app.router.add_get("/health/ready", sidecar.handle_ready)
    # Prometheus metrics
    app.router.add_get("/metrics", sidecar.handle_metrics)
    # Model management
    app.router.add_get("/models", sidecar.handle_list_models)
    app.router.add_post("/models/load", sidecar.handle_load_model)
    app.router.add_post("/models/unload", sidecar.handle_unload_model)
    app.router.add_post("/models/reload", sidecar.handle_reload_model)
    app.router.add_post("/models/enable", sidecar.handle_enable_model)
    app.router.add_post("/models/disable", sidecar.handle_disable_model)
    app.router.add_post("/models/remove", sidecar.handle_remove_model)
    app.router.add_get(
        "/models/{model_id}/status", sidecar.handle_get_model_status
    )
    app.router.add_get(
        "/models/{model_id}/stats", sidecar.handle_get_model_stats
    )
    app.router.add_get(
        "/models/{model_id}/history", sidecar.handle_model_version_history
    )
    app.router.add_post("/models/rollback", sidecar.handle_rollback_model)
    app.router.add_post(
        "/models/generate-presets", sidecar.handle_generate_presets
    )
    # Inference
    app.router.add_post("/infer", sidecar.handle_infer)
    # Scheduled inference
    app.router.add_post("/scheduled/start", sidecar.handle_start_scheduled)
    app.router.add_post("/scheduled/stop", sidecar.handle_stop_scheduled)
    # Self-learning
    app.router.add_post(
        "/self-learning/sample", sidecar.handle_add_sample
    )
    app.router.add_post(
        "/self-learning/predict", sidecar.handle_predict
    )
    app.router.add_post(
        "/self-learning/stats", sidecar.handle_self_learning_stats
    )
    app.router.add_get(
        "/self-learning/stats/all", sidecar.handle_all_self_learning_stats
    )
    app.router.add_post(
        "/self-learning/reset", sidecar.handle_reset_self_learning
    )
    app.router.add_post(
        "/self-learning/threshold", sidecar.handle_set_threshold
    )
    # Statistics
    app.router.add_get("/stats", sidecar.handle_get_stats)
    # Execution provider
    app.router.add_post(
        "/execution-provider", sidecar.handle_set_execution_provider
    )
    app.router.add_get(
        "/execution-providers", sidecar.handle_get_providers
    )
    return app


# ═══════════════════════════════════════════════════════════════════════
#  Logging setup
# ═══════════════════════════════════════════════════════════════════════


class JsonFormatter(logging.Formatter):
    """Structured JSON log formatter for containerised environments."""

    def format(self, record: logging.LogRecord) -> str:
        """Format a log record as a JSON string."""
        log_entry: JSONDict = {
            "timestamp": datetime.fromtimestamp(
                record.created, tz=UTC
            ).isoformat(),
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
        }
        if record.exc_info:
            log_entry["exception"] = self.formatException(record.exc_info)
        return json.dumps(log_entry)


def _setup_logging(level: str = "INFO", json_format: bool = False) -> None:
    """Configure structured logging with optional JSON format.

    :param level: Log level name (``"DEBUG"``, ``"INFO"``, etc.).
    :param json_format: If ``True``, emit JSON-formatted log lines.
    """
    log_level = getattr(logging, level.upper(), logging.INFO)
    if json_format:
        handler = logging.StreamHandler()
        handler.setFormatter(JsonFormatter())
        logging.root.handlers = [handler]
        logging.root.setLevel(log_level)
    else:
        logging.basicConfig(
            level=log_level,
            format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
            force=True,
        )


# ═══════════════════════════════════════════════════════════════════════
#  Main entry point
# ═══════════════════════════════════════════════════════════════════════


async def serve(
    host: str = DEFAULT_HOST,
    port: int = DEFAULT_PORT,
    models_dir: str = DEFAULT_MODELS_DIR,
) -> None:
    """Start the HTTP JSON AI sidecar server.

    :param host: Listen host.
    :param port: Listen port.
    :param models_dir: Directory for ONNX model files.
    """
    log_level = os.environ.get("AI_SIDECAR_LOG_LEVEL", "INFO")
    json_logs = os.environ.get("AI_SIDECAR_JSON_LOGS", "").lower() in (
        "1", "true", "yes",
    )
    _setup_logging(log_level, json_logs)
    logger.info(
        "Starting EdgeLite AI Sidecar (HTTP) on %s:%d", host, port
    )

    sidecar = AISidecarServer(models_dir=models_dir)
    await sidecar.initialize()

    app = create_app(sidecar)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, host, port)
    await site.start()
    logger.info("AI Sidecar listening on %s:%d", host, port)

    # Graceful shutdown
    try:
        stop_event = asyncio.Event()
        for sig in (signal.SIGINT, signal.SIGTERM):
            try:
                loop = asyncio.get_event_loop()
                loop.add_signal_handler(sig, stop_event.set)
            except NotImplementedError:
                pass  # Windows does not support add_signal_handler
        await stop_event.wait()
        logger.info("Shutdown signal received, cleaning up...")
    finally:
        for model_id in list(sidecar._scheduled_tasks.keys()):
            await sidecar._stop_scheduled(model_id)
        await runner.cleanup()
        logger.info("AI Sidecar shutdown complete")


def main() -> None:
    """CLI entry point for the AI sidecar server."""
    parser = argparse.ArgumentParser(
        description="EdgeLite AI Sidecar (HTTP)"
    )
    parser.add_argument(
        "--host",
        default=os.environ.get("AI_SIDECAR_HOST", DEFAULT_HOST),
        help="Listen host (default: 0.0.0.0)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=int(os.environ.get("AI_SIDECAR_PORT", str(DEFAULT_PORT))),
        help="Listen port (default: 50052)",
    )
    parser.add_argument(
        "--models-dir",
        default=os.environ.get("AI_MODELS_DIR", DEFAULT_MODELS_DIR),
        help="ONNX models directory (default: models)",
    )
    parser.add_argument(
        "--log-level",
        default=os.environ.get("AI_SIDECAR_LOG_LEVEL", "INFO"),
        help="Log level (default: INFO)",
    )
    parser.add_argument(
        "--json-logs",
        action="store_true",
        default=os.environ.get("AI_SIDECAR_JSON_LOGS", "").lower()
        in ("1", "true", "yes"),
        help="Enable structured JSON logging",
    )
    args = parser.parse_args()
    _setup_logging(args.log_level, args.json_logs)
    asyncio.run(serve(args.host, args.port, args.models_dir))


if __name__ == "__main__":
    main()
