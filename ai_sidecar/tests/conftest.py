"""
Test fixtures and configuration for EdgeLite AI Sidecar tests.
"""
import asyncio
import sys
from pathlib import Path

import pytest
import pytest_asyncio

# Ensure the ai_sidecar directory is on the path
sys.path.insert(0, str(Path(__file__).parent.parent))


@pytest.fixture
def tmp_models_dir(tmp_path):
    """Create a temporary models directory."""
    models_dir = tmp_path / "models"
    models_dir.mkdir(parents=True, exist_ok=True)
    return str(models_dir)


@pytest.fixture
def sidecar_server(tmp_models_dir):
    """Create an AISidecarServer instance (initialized)."""
    from server import AISidecarServer
    server = AISidecarServer(models_dir=tmp_models_dir)
    loop = asyncio.new_event_loop()
    try:
        loop.run_until_complete(server.initialize())
    finally:
        loop.close()
    return server


@pytest_asyncio.fixture
async def aiohttp_client_factory(tmp_models_dir):
    """Create an aiohttp test client with a running sidecar."""
    from aiohttp.test_utils import TestClient, TestServer
    from server import AISidecarServer, create_app

    sidecar = AISidecarServer(models_dir=tmp_models_dir)
    await sidecar.initialize()
    app = create_app(sidecar)
    server = TestServer(app)
    client = TestClient(server)
    await client.start_server()
    yield client
    await client.close()
