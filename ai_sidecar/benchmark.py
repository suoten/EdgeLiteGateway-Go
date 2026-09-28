#!/usr/bin/env python3
"""Benchmark script for EdgeLite AI Sidecar inference performance.

Measures latency, throughput, and success rate across multiple
concurrent requests and input sizes.

Usage:
    python benchmark.py [--host localhost] [--port 50052]
                        [--model preset-anomaly-v1]
                        [--concurrent 10] [--requests 100]
                        [--input-size 100]
"""
import argparse
import asyncio
import json
import statistics
import sys
import time

try:
    import aiohttp
except ImportError:
    print("ERROR: aiohttp not installed. Run: pip install aiohttp")
    sys.exit(1)


async def single_inference(
    session: aiohttp.ClientSession,
    url: str,
    model_id: str,
    input_size: int,
) -> dict:
    """Execute a single inference request and return timing data."""
    start = time.perf_counter()
    try:
        async with session.post(
            url,
            json={
                "model_id": model_id,
                "input_data": [0.5] * input_size,
            },
        ) as resp:
            data = await resp.json()
            elapsed = (time.perf_counter() - start) * 1000
            return {
                "success": data.get("status") == "success",
                "latency_ms": elapsed,
                "server_latency_ms": data.get("latency_ms", 0),
                "status": data.get("status", "unknown"),
            }
    except Exception as e:
        elapsed = (time.perf_counter() - start) * 1000
        return {
            "success": False,
            "latency_ms": elapsed,
            "server_latency_ms": 0,
            "status": "exception",
            "error": str(e),
        }


async def run_benchmark(
    host: str,
    port: int,
    model_id: str,
    concurrent: int,
    total_requests: int,
    input_size: int,
) -> None:
    """Run the benchmark and print results."""
    url = f"http://{host}:{port}/infer"
    print(f"EdgeLite AI Sidecar Benchmark")
    print(f"  URL:         {url}")
    print(f"  Model:       {model_id}")
    print(f"  Concurrent:  {concurrent}")
    print(f"  Requests:    {total_requests}")
    print(f"  Input size:  {input_size} floats")
    print(f"  {'─' * 60}")

    # Verify server is healthy
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=30)) as session:
        try:
            async with session.get(
                f"http://{host}:{port}/health/live"
            ) as resp:
                health = await resp.json()
                if not health.get("healthy"):
                    print("ERROR: Server is not healthy")
                    return
        except Exception as e:
            print(f"ERROR: Cannot connect to server: {e}")
            return

    # Warmup
    print("  Warming up...", end="", flush=True)
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=30)) as session:
        for _ in range(5):
            await single_inference(session, url, model_id, input_size)
    print(" done")

    # Run benchmark
    results: list[dict] = []
    semaphore = asyncio.Semaphore(concurrent)

    async def bounded_request(session):
        async with semaphore:
            return await single_inference(session, url, model_id, input_size)

    print(f"  Running {total_requests} requests...", end="", flush=True)
    start_time = time.perf_counter()
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=30)) as session:
        tasks = [bounded_request(session) for _ in range(total_requests)]
        results = await asyncio.gather(*tasks)
    total_time = time.perf_counter() - start_time
    print(" done")

    # Analyze results
    latencies = [r["latency_ms"] for r in results if r["success"]]
    server_latencies = [
        r["server_latency_ms"] for r in results if r["success"]
    ]
    successes = sum(1 for r in results if r["success"])
    failures = total_requests - successes

    print(f"  {'─' * 60}")
    print(f"  Results:")
    print(f"    Total requests:     {total_requests}")
    print(f"    Successful:         {successes}")
    print(f"    Failed:             {failures}")
    print(f"    Success rate:       {successes/total_requests*100:.1f}%")
    print(f"    Total time:         {total_time:.2f}s")
    print(f"    Throughput:         {total_requests/total_time:.1f} req/s")
    print()
    if latencies:
        print(f"    Client-side latency (ms):")
        print(f"      Min:    {min(latencies):.2f}")
        print(f"      Max:    {max(latencies):.2f}")
        print(f"      Mean:   {statistics.mean(latencies):.2f}")
        print(f"      Median: {statistics.median(latencies):.2f}")
        if len(latencies) > 1:
            print(f"      Stdev:  {statistics.stdev(latencies):.2f}")
        print(f"      P95:    {sorted(latencies)[int(len(latencies)*0.95)]:.2f}")
        print(f"      P99:    {sorted(latencies)[int(len(latencies)*0.99)]:.2f}")
    if server_latencies:
        print()
        print(f"    Server-side latency (ms):")
        print(f"      Min:    {min(server_latencies):.2f}")
        print(f"      Max:    {max(server_latencies):.2f}")
        print(f"      Mean:   {statistics.mean(server_latencies):.2f}")
        print(f"      Median: {statistics.median(server_latencies):.2f}")

    if failures > 0:
        print()
        print(f"    Error breakdown:")
        error_types: dict[str, int] = {}
        for r in results:
            if not r["success"]:
                status = r.get("status", "unknown")
                error_types[status] = error_types.get(status, 0) + 1
        for err_type, count in sorted(error_types.items()):
            print(f"      {err_type}: {count}")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="EdgeLite AI Sidecar benchmark"
    )
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=50052)
    parser.add_argument("--model", default="preset-anomaly-v1")
    parser.add_argument("--concurrent", type=int, default=10)
    parser.add_argument("--requests", type=int, default=100)
    parser.add_argument("--input-size", type=int, default=100)
    args = parser.parse_args()
    asyncio.run(run_benchmark(
        args.host, args.port, args.model,
        args.concurrent, args.requests, args.input_size,
    ))


if __name__ == "__main__":
    main()
