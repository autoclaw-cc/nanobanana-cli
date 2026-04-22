package nanobanana

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nanobanana-cli/browser"

	"golang.org/x/image/draw"
)

const geminiURL = "https://gemini.google.com/"

// Result is the JSON payload returned by `gen`.
type Result struct {
	Prompt     string `json:"prompt"`
	Full       string `json:"full"`
	Thumb      string `json:"thumb"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	ThumbWidth int    `json:"thumb_width"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

// Options drive a single image generation.
type Options struct {
	Prompt     string
	OutDir     string
	ThumbWidth int           // target thumbnail width in px (height preserves aspect)
	Timeout    time.Duration // max time to wait for image to appear
}

// Gen orchestrates: navigate → fill prompt → submit → wait for image →
// canvas-extract displayed PNG → save full + locally-generated thumbnail.
func Gen(c *browser.Client, opts Options) (*Result, error) {
	start := time.Now()
	if opts.Prompt == "" {
		return nil, fmt.Errorf("prompt is empty")
	}
	if opts.OutDir == "" {
		opts.OutDir = "."
	}
	if opts.ThumbWidth <= 0 {
		opts.ThumbWidth = 256
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 90 * time.Second
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir output dir: %w", err)
	}

	// 1. Open Gemini in a new tab.
	if err := c.Navigate(geminiURL, true); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	// 2. Wait for prompt textbox to appear (SPA may take a beat to hydrate).
	if err := waitTextbox(c, 15*time.Second); err != nil {
		return nil, err
	}

	// 3. Inject prompt via execCommand("insertText") to trigger input events
	//    (Gemini uses a Quill editor; plain value setting won't enable the send
	//    button, since Angular only reacts to real input events).
	if err := injectPrompt(c, opts.Prompt); err != nil {
		return nil, err
	}

	// 4. Click send button (class `send-button` is stable; aria-label is locale-dep).
	if err := clickSend(c); err != nil {
		return nil, err
	}

	// 5. Poll until <generated-image> has a fully loaded <img>, then canvas-
	//    extract base64 PNG. fetch()-ing the underlying URL won't work: Gemini
	//    revokes the blob and the signed `lh3.googleusercontent.com/gg-dl/*`
	//    URLs are single-use nonces.
	dataURL, w, h, err := waitAndExtractImage(c, opts.Timeout)
	if err != nil {
		return nil, err
	}

	// 6. Decode PNG, write full, resize to thumb, write thumb.
	pngBytes, err := decodeDataURL(dataURL)
	if err != nil {
		return nil, fmt.Errorf("decode dataURL: %w", err)
	}
	stem := time.Now().Format("20060102-150405")
	fullPath := filepath.Join(opts.OutDir, stem+"-full.png")
	thumbPath := filepath.Join(opts.OutDir, stem+"-thumb.png")

	if err := os.WriteFile(fullPath, pngBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write full: %w", err)
	}
	if err := writeThumbnail(pngBytes, thumbPath, opts.ThumbWidth); err != nil {
		return nil, fmt.Errorf("write thumb: %w", err)
	}

	absFull, _ := filepath.Abs(fullPath)
	absThumb, _ := filepath.Abs(thumbPath)
	return &Result{
		Prompt:     opts.Prompt,
		Full:       absFull,
		Thumb:      absThumb,
		Width:      w,
		Height:     h,
		ThumbWidth: opts.ThumbWidth,
		ElapsedMS:  time.Since(start).Milliseconds(),
	}, nil
}

// --- browser-side steps ---

func waitTextbox(c *browser.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const code = `(function(){
		const tb = document.querySelector('div[contenteditable="true"][role="textbox"]');
		return { ok: !!tb };
	})()`
	for time.Now().Before(deadline) {
		var out struct{ OK bool `json:"ok"` }
		if err := c.EvaluateValue(code, &out); err == nil && out.OK {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for Gemini prompt textbox")
}

func injectPrompt(c *browser.Client, prompt string) error {
	encoded, _ := json.Marshal(prompt)
	code := fmt.Sprintf(`(function(){
		const tb = document.querySelector('div[contenteditable="true"][role="textbox"]');
		if (!tb) return { ok: false, err: 'textbox_not_found' };
		tb.focus();
		document.execCommand('selectAll', false, null);
		document.execCommand('insertText', false, %s);
		return { ok: true, text: (tb.innerText||'').slice(0, 200) };
	})()`, string(encoded))
	var out struct {
		OK   bool   `json:"ok"`
		Err  string `json:"err"`
		Text string `json:"text"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("inject prompt: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("inject prompt failed: %s", out.Err)
	}
	return nil
}

func clickSend(c *browser.Client) error {
	const code = `(function(){
		const selectors = [
			'button.send-button',
			'button[aria-label="发送"]',
			'button[aria-label="Send"]'
		];
		for (const sel of selectors) {
			const b = document.querySelector(sel);
			if (b && !b.disabled) { b.click(); return { ok: true, sel }; }
		}
		return { ok: false, err: 'send_button_not_found' };
	})()`
	var out struct {
		OK  bool   `json:"ok"`
		Sel string `json:"sel"`
		Err string `json:"err"`
	}
	if err := c.EvaluateValue(code, &out); err != nil {
		return fmt.Errorf("click send: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("click send failed: %s", out.Err)
	}
	return nil
}

func waitAndExtractImage(c *browser.Client, timeout time.Duration) (string, int, int, error) {
	const pollCode = `(function(){
		const img = document.querySelector(
			'generated-image img, .generated-image img, single-image img'
		);
		if (!img) return { state: 'pending' };
		if (!img.complete || img.naturalWidth === 0) return { state: 'loading' };
		return { state: 'ready', w: img.naturalWidth, h: img.naturalHeight };
	})()`
	const extractCode = `(function(){
		const img = document.querySelector(
			'generated-image img, .generated-image img, single-image img'
		);
		if (!img) return { ok: false, err: 'no_image' };
		if (!img.complete || img.naturalWidth === 0) return { ok: false, err: 'not_loaded' };
		const c = document.createElement('canvas');
		c.width = img.naturalWidth;
		c.height = img.naturalHeight;
		try {
			c.getContext('2d').drawImage(img, 0, 0);
			return { ok: true, w: img.naturalWidth, h: img.naturalHeight, dataURL: c.toDataURL('image/png') };
		} catch (e) { return { ok: false, err: 'canvas: ' + String(e).slice(0, 200) }; }
	})()`

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var poll struct {
			State string `json:"state"`
			W     int    `json:"w"`
			H     int    `json:"h"`
		}
		if err := c.EvaluateValue(pollCode, &poll); err != nil {
			return "", 0, 0, fmt.Errorf("poll image: %w", err)
		}
		if poll.State == "ready" {
			var ext struct {
				OK      bool   `json:"ok"`
				Err     string `json:"err"`
				W       int    `json:"w"`
				H       int    `json:"h"`
				DataURL string `json:"dataURL"`
			}
			if err := c.EvaluateValue(extractCode, &ext); err != nil {
				return "", 0, 0, fmt.Errorf("extract image: %w", err)
			}
			if !ext.OK {
				return "", 0, 0, fmt.Errorf("extract image failed: %s", ext.Err)
			}
			return ext.DataURL, ext.W, ext.H, nil
		}
		time.Sleep(1 * time.Second)
	}
	return "", 0, 0, fmt.Errorf("timeout waiting for generated image (did Gemini route this prompt to image generation?)")
}

// --- local image handling ---

func decodeDataURL(dataURL string) ([]byte, error) {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(dataURL, prefix) {
		return nil, fmt.Errorf("unexpected dataURL prefix")
	}
	return base64.StdEncoding.DecodeString(dataURL[len(prefix):])
}

func writeThumbnail(pngBytes []byte, path string, width int) error {
	src, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return fmt.Errorf("decode png: %w", err)
	}
	sb := src.Bounds()
	if sb.Dx() == 0 {
		return fmt.Errorf("source image has zero width")
	}
	// Preserve aspect; round height (clamp to ≥1 to survive extreme aspect ratios).
	height := max(width*sb.Dy()/sb.Dx(), 1)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Over, nil)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, dst)
}
