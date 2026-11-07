"""Validate and optionally launch the pinned upstream NVIDIA offline fixture."""

import argparse
import os
from pathlib import Path
import subprocess
import sys


UPSTREAM_COMMIT = "a679bb86ce36e883a98a97733492b686e5b9a08e"


def validate_host(platform, python_version, compute_caps, ep):
    if platform != "linux":
        raise ValueError("the pinned native DeepInfer release requires Linux")
    if python_version != (3, 12):
        raise ValueError("the pinned native DeepInfer release requires Python 3.12")
    if len(compute_caps) < ep or any(cap != "10.0" for cap in compute_caps[:ep]):
        raise ValueError(f"the first {ep} NVIDIA devices must be SM100 (compute capability 10.0)")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path, help="pinned upstream DeepInfer checkout")
    parser.add_argument("--checkpoint", required=True, type=Path, help="local V4.1 Flash checkpoint directory")
    parser.add_argument("--ep", type=int, default=8, help="number of dedicated NVIDIA GPUs in the group")
    parser.add_argument("--execute", action="store_true", help="run the upstream reference fixture after validation")
    args = parser.parse_args()
    if args.ep < 1:
        parser.error("--ep must be positive")

    try:
        # Report an unsupported OS/Python version before invoking vendor tools.
        validate_host(sys.platform, sys.version_info[:2], [], 0)
        if any(os.environ.get(key) for key in ("CUDA_VISIBLE_DEVICES", "DEVICE_OFFSET", "RANK", "WORLD_SIZE")):
            raise ValueError("unset CUDA_VISIBLE_DEVICES, DEVICE_OFFSET, RANK and WORLD_SIZE for this single-host runner")
        gpu_output = subprocess.check_output(
            ["nvidia-smi", "--query-gpu=compute_cap", "--format=csv,noheader"],
            text=True, timeout=10,
        )
        validate_host(sys.platform, sys.version_info[:2], gpu_output.split(), args.ep)
        source = args.source.resolve(strict=True)
        actual_commit = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=source, text=True, timeout=10,
        ).strip()
        if actual_commit != UPSTREAM_COMMIT:
            raise ValueError(f"DeepInfer source must be pinned to {UPSTREAM_COMMIT}")
        modified = subprocess.check_output(
            ["git", "status", "--porcelain", "--untracked-files=no"], cwd=source, text=True, timeout=10,
        ).strip()
        if modified:
            raise ValueError("tracked files in the DeepInfer checkout have local changes")
        checkpoint = args.checkpoint.resolve(strict=True)
        if not (checkpoint / "config.json").is_file() or not any(checkpoint.glob("*.safetensors")):
            raise ValueError("checkpoint directory must contain config.json and safetensors weights")
        print("Host, pinned source and local checkpoint preflight passed.", flush=True)
        if args.execute:
            env = dict(os.environ, DEEPINFER_ACCELERATOR="nvidia", DEEPINFER_HF_MODEL_DIR=str(checkpoint))
            return subprocess.call(
                [sys.executable, "examples/offline_inference.py", "--model_name", str(checkpoint),
                 "--ep", str(args.ep), "--dp", str(args.ep), "--cp", "1"],
                cwd=source, env=env,
            )
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        print(f"DeepInfer preflight failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
