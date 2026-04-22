# nanobanana-cli

Generate images via Google Gemini's **Nano Banana** (Gemini 2.5 Flash Image) model from the command line and save a full-size PNG plus a locally-scaled thumbnail.

The CLI drives your **real Chrome session** through [`kimi-webbridge`](https://www.kimi.com/features/webbridge), so it reuses your existing Gemini login — no API key, no OAuth setup, no separate Gemini for Developers quota.

## What it does

```bash
nanobanana-cli gen "画一只可爱的柴犬，卡通风格，白色背景" -o ./out
```

```json
{
  "ok": true,
  "data": {
    "prompt": "画一只可爱的柴犬，卡通风格，白色背景",
    "full":  "/abs/path/out/20260422-135030-full.png",
    "thumb": "/abs/path/out/20260422-135030-thumb.png",
    "width":  1024,
    "height": 559,
    "thumb_width": 256,
    "elapsed_ms": 86482
  }
}
```

Each run saves two files into `-o <dir>`:

- `<timestamp>-full.png` — 1024×559 PNG, pixel-exact copy of the image Gemini rendered
- `<timestamp>-thumb.png` — PNG scaled to `--thumb-width` px (aspect preserved, default 256)

## Requirements

- **macOS** (Linux/Windows likely work but untested)
- **Kimi Desktop App** running — bundles the `kimi-webbridge` daemon on `http://127.0.0.1:10086`. Install: <https://www.kimi.com/features/webbridge>
- **Chrome** with the WebBridge extension installed and connected (status check: `curl http://127.0.0.1:10086/status` should report `extension_connected: true`)
- **Gemini logged in** in that Chrome — the CLI reuses your cookies via the real browser
- **Go 1.22+** to build

## Build

```bash
git clone https://github.com/autoclaw-cc/nanobanana-cli.git
cd nanobanana-cli
go build -o nanobanana-cli .
```

## Usage

```
nanobanana-cli gen <prompt> [flags]

Flags:
  -o, --out string        output directory (default ".")
      --thumb-width int   thumbnail width in px (default 256)
      --timeout int       max seconds to wait for image generation (default 90)
```

Output is **always JSON** on stdout. Non-zero exit code on error. Error shape:

```json
{ "ok": false, "error": { "code": "...", "message": "..." } }
```

Common error codes: `daemon_unreachable`, `daemon_not_running`, `extension_not_connected`, `invalid_args`, `gen_failed`.

## How it works

```
user prompt
    │
    ▼
POST :10086/command  ─────▶  Chrome extension  ─────▶  gemini.google.com
    navigate                                           (your real session)
    evaluate(inject prompt via execCommand)
    evaluate(click button.send-button)
    evaluate(poll until .generated-image img.loaded)
    evaluate(canvas.drawImage → toDataURL('image/png'))
    │                                                         │
    │ ◀────── base64 PNG (~200KB)  ◀───────────────────────────┘
    ▼
Go: decode PNG → write *-full.png → resize (Catmull-Rom) → write *-thumb.png
```

**Why canvas extraction instead of clicking "Download full-size"?** Gemini's download mechanism hands the browser a **single-use signed URL** (`lh3.googleusercontent.com/gg-dl/...`) via a `c8o8Fe` batchexecute RPC. The nonce is consumed by Chrome's download pipeline and returns HTTP 400 on any replay. Canvas extraction reads the already-rendered pixels out of the `<img>` directly — same bytes, no nonce race, no dependency on Chrome's download-prompt UX.

**Why generate the thumbnail locally instead of asking Gemini?** Gemini produces a single 1024×559 image; the chat UI just CSS-scales it for display. The "thumbnail" you see in the page is not a separate resource. Scaling locally (via `golang.org/x/image/draw.CatmullRom`) is deterministic, offline-capable, and the thumbnail width is user-controlled.

## Project layout

```
nanobanana-cli/
├── main.go                   entry point
├── browser/client.go         HTTP client for kimi-webbridge daemon
├── output/output.go          structured JSON output helper
├── nanobanana/gen.go         the one feature: prompt → full + thumb
└── cmd/root.go               cobra command registration
```

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `daemon_unreachable` | Kimi Desktop App not running. Open it. |
| `extension_not_connected` | Chrome WebBridge extension not installed/enabled. See <https://www.kimi.com/features/webbridge>. |
| `timeout waiting for generated image` | Gemini routed your prompt to text response, not image. Rephrase to be clearly an image-gen request (e.g., start with `画` / `generate an image of`). |
| `prompt is empty` | `gen ""` — pass a non-empty prompt. |

## License

MIT (see `LICENSE`).
