# Native DeepInfer validation

The pinned upstream release provides offline inference for DeepSeek-V4.1-Flash
on NVIDIA SM100 or Huawei Ascend 950. The current Windows RTX 5060 tests use
llama.cpp and Qwen through the OpenAI-compatible backend. Native DeepInfer
inference still needs a supported Linux machine.

## Run the reference fixture on NVIDIA

Use a dedicated Linux host with eight SM100 GPUs, Python 3.12, GCC 13 or later,
CUDA 13, and a local copy of the upstream V4.1 Flash checkpoint. Build DeepInfer
in a fresh virtual environment following the pinned upstream instructions.

```bash
git clone https://github.com/deepseek-ai/DeepInfer /opt/DeepInfer
git -C /opt/DeepInfer checkout --detach a679bb86ce36e883a98a97733492b686e5b9a08e
# In the active Python 3.12 virtual environment:
(cd /opt/DeepInfer && DEEPINFER_ACCELERATOR=nvidia ./build.sh)

python adapters/deepinfer/preflight.py --source /opt/DeepInfer --checkpoint /models/DeepSeek-V4.1-Flash-1017
python adapters/deepinfer/preflight.py --source /opt/DeepInfer --checkpoint /models/DeepSeek-V4.1-Flash-1017 --execute
```

The first invocation checks the OS, Python version, SM100 devices, tracked
upstream source and local checkpoint presence. The second also starts the pinned
upstream offline example. The example reports text, token counts and decode PPL
per rank. A green preflight does not establish that the checkpoint fits in
device memory or that kernels execute successfully. The launcher covers
single-host NVIDIA. Use the upstream build and example instructions for Ascend.

## Native service boundary

The reference entry points are `examples/_config.py:create_llm` and
`deepinfer.entrypoints.llm.LLM.generate`. The example creates a parallel group,
submits prompts and `SamplingParams`, then reads the returned token IDs and
decodes them through the engine tokenizer. This is a batch call. The published
reference does not expose a verified per-request token callback or cancellation
contract.

After the offline fixture passes on the target, a native worker can hold one
dedicated parallel group for batch requests. Feed it bounded token IDs built
with the verified DeepSeek chat template and return the decoded final output
once. Give all ranks the same ordered request lifecycle and isolate the process
group from the Agent. If a deadline or cancellation requires terminating a rank,
drain and rebuild the full group before reporting ready. Keep it out of online
streaming routes until token callbacks, cancellation, memory admission and
failure recovery have been exercised on native hardware.

[Pinned upstream source and hardware requirements](https://github.com/deepseek-ai/DeepInfer/tree/a679bb86ce36e883a98a97733492b686e5b9a08e)
