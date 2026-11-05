# Kimi K3 compatibility fixtures

These synthetic assets were created for ccLoad. They contain no downloaded or third-party media.

- `blue.png`: 256 × 256, solid RGB (0, 80, 255).
- `flower.png`: a synthetic pink flower with a yellow center, stem and leaves.
- `flower.mp4`: the flower image held for 3 seconds, H.264, yuv420p, 12 fps.
- `black.mp4`: solid black, 320 × 240, 3 seconds, H.264, yuv420p, 12 fps.

Regenerate with Python 3 + Pillow and FFmpeg on PATH:

```sh
python internal/app/kimi_fixtures/generate.py
```

The upload-speed case appends a valid ISO-BMFF `free` atom to `black.mp4`.
The default 15 MiB binary payload encodes to 20 MiB Base64; the JSON envelope
adds a small overhead. This tests body transmission, not video complexity or
long-video understanding. The server records actual JSON bytes and measures
`httptrace.GotConn` to successful `WroteRequest`, excluding connection setup,
queue waiting and inference. This is the client's completed-write measurement,
not proof of when a remote server consumed every byte. The default limit is
60 seconds. Per-case timeout includes queue waiting and inference (180 seconds).

The URL-image case uses the same blue fixture at
`/public/model-test/kimi/blue.png`; it must be reachable by the upstream vendor.
The URL can be replaced with another publicly accessible blue image.

The ccLoad suite reconstructs the first 12 names and known assertions from the
user-provided Kimi K3 compatibility report. That report names
`builtin-capability-cases 1.2.0`, commit `ab05b24ddb38`, but does not include
original request bodies or assets. Accordingly this implementation has its own
version (`ccload-kimi-k3/1.0.1`) and does not claim identical original prompts.
Dynamic tools use `system.tools`; the global coexistence case also supplies a
top-level stock-price tool. The default weather case does not force tool choice.
The system message carrying dynamic tools has empty `content`, as required by
Moonshot's [dynamic-tool tests](https://github.com/MoonshotAI/Kimi-Vendor-Verifier/blob/main/tests/k3_features/test_dynamic_tools.py).
Version 1.0.1 fixes the non-empty content previously sent by both dynamic cases;
weather-call assertions are unchanged.
All weather cases require `get_weather`, JSON arguments with `city: 北京`, and
`finish_reason: tool_calls`. Three thinking-off flags are sent independently.

The runner sends raw OpenAI Chat requests using the selected channel's first
OpenAI-capable URL, model mapping, API key, header rules, proxy and concurrency
limits. Body transformation rules and video-to-image conversion are deliberately
not applied because they would change the capability being measured. Each case
sends one non-streaming request; the admin browser receives progress over SSE.
No automatic retries or cross-URL fallback hide upstream incompatibilities.
Tests consume normal API usage; this diagnostic runner does not record billing
or alter channel cooldown state. Runs are in memory and stop on disconnection.
