// Package pwbrowser implements replay.Browser by spawning a Node.js
// playwright bridge process and communicating with it over stdin/stdout JSON.
//
// This avoids the playwright-go CDN dependency entirely. The bridge script
// (cmd/replay/playwright_bridge.js) uses the locally installed playwright
// npm package, which already has Chromium downloaded.
package pwbrowser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"computer-use/domain"
)

// Options configures the real browser launch.
type Options struct {
	// ChromiumPath is the path to the Chromium executable passed to Playwright.
	// If empty, playwright will use its own installed browser.
	ChromiumPath string
	// Headless runs the browser without a visible window.
	Headless bool
	// EvidenceDir is where screenshots are saved.
	EvidenceDir string
	// BridgePath is the path to playwright_bridge.js.
	// Defaults to cmd/replay/playwright_bridge.js relative to the working directory.
	BridgePath string
}

// bridgeResp is a single JSON response from the bridge.
type bridgeResp struct {
	ID    int             `json:"id"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Browser is the production replay.Browser implementation backed by Node.js playwright.
type Browser struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	pending     map[int]chan bridgeResp
	nextID      atomic.Int32
	evidenceDir string
}

// Launch starts the Node.js playwright bridge and returns a Browser ready for use.
// The returned cleanup function must be called when the run is complete.
func Launch(opts Options) (*Browser, func(), error) {
	bridgePath := opts.BridgePath
	if bridgePath == "" {
		// Find relative to the executable location.
		_, filename, _, _ := runtime.Caller(0)
		bridgePath = filepath.Join(filepath.Dir(filename), "..", "..", "cmd", "replay", "playwright_bridge.js")
		// Try cwd-relative fallback.
		if _, err := os.Stat(bridgePath); err != nil {
			bridgePath = filepath.Join("cmd", "replay", "playwright_bridge.js")
		}
	}
	if _, err := os.Stat(bridgePath); err != nil {
		return nil, nil, fmt.Errorf("playwright bridge not found at %s: %w", bridgePath, err)
	}

	evidenceDir := opts.EvidenceDir
	if evidenceDir == "" {
		evidenceDir = "evidence"
	}
	_ = os.MkdirAll(evidenceDir, 0755)

	env := os.Environ()
	if opts.ChromiumPath != "" {
		env = append(env, "CHROMIUM_PATH="+opts.ChromiumPath)
	}
	if !opts.Headless {
		env = append(env, "HEADLESS=false")
	}

	cmd := exec.Command("node", bridgePath)
	cmd.Env = env
	cmd.Stderr = os.Stderr // bridge errors → our stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("could not start node bridge: %w", err)
	}

	b := &Browser{
		cmd:         cmd,
		stdin:       stdin,
		pending:     make(map[int]chan bridgeResp),
		evidenceDir: evidenceDir,
	}

	// Background reader goroutine: routes responses to waiting callers.
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var resp bridgeResp
			if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
				continue
			}
			b.mu.Lock()
			ch, ok := b.pending[resp.ID]
			if ok {
				delete(b.pending, resp.ID)
			}
			b.mu.Unlock()
			if ok {
				ch <- resp
			}
		}
	}()

	cleanup := func() {
		_, _ = b.call(context.Background(), "close", nil)
		_ = cmd.Wait()
	}
	return b, cleanup, nil
}

// call sends one command to the bridge and waits for its response.
func (b *Browser) call(_ context.Context, method string, args interface{}) (json.RawMessage, error) {
	id := int(b.nextID.Add(1))
	msg := map[string]interface{}{"id": id, "method": method, "args": args}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	ch := make(chan bridgeResp, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	if _, err := b.stdin.Write(data); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, fmt.Errorf("bridge write: %w", err)
	}

	resp := <-ch
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Value, nil
}

// resolveSelector converts a domain.LocatorStrategy to a CSS selector string.
func resolveSelector(s domain.LocatorStrategy) string {
	switch s.Kind {
	case domain.LocatorKindTestID:
		return fmt.Sprintf("[data-test='%s']", s.Value)
	case domain.LocatorKindCSS:
		return s.Value
	case domain.LocatorKindXPath:
		return "xpath=" + s.Value
	case domain.LocatorKindText:
		return "text=" + s.Value
	case domain.LocatorKindRole:
		if s.Name != "" {
			return fmt.Sprintf("role=%s[name='%s']", s.Value, s.Name)
		}
		return "role=" + s.Value
	case domain.LocatorKindLabel:
		return "label=" + s.Value
	default:
		return s.Value
	}
}

// primarySelector returns the primary selector string for a Locator.
// (Fallbacks are handled by the bridge's locator resolution on the JS side.)
func primarySelector(loc domain.Locator) string {
	return resolveSelector(loc.Primary)
}

// ── replay.Browser interface ──────────────────────────────────────────────────

func (b *Browser) Navigate(ctx context.Context, url string) error {
	log.Printf("[pwbrowser] Navigate → %s", url)
	_, err := b.call(ctx, "navigate", map[string]string{"url": url})
	return err
}

func (b *Browser) Click(ctx context.Context, loc domain.Locator) error {
	sel := primarySelector(loc)
	log.Printf("[pwbrowser] Click %s", sel)
	_, err := b.call(ctx, "click", map[string]string{"selector": sel})
	return err
}

func (b *Browser) Fill(ctx context.Context, loc domain.Locator, value string) error {
	sel := primarySelector(loc)
	log.Printf("[pwbrowser] Fill %s", sel)
	_, err := b.call(ctx, "fill", map[string]string{"selector": sel, "value": value})
	return err
}

func (b *Browser) Select(ctx context.Context, loc domain.Locator, value string) error {
	sel := primarySelector(loc)
	log.Printf("[pwbrowser] Select %s value=%q", sel, value)
	_, err := b.call(ctx, "select", map[string]string{"selector": sel, "value": value})
	return err
}

func (b *Browser) Check(ctx context.Context, loc domain.Locator, checked bool) error {
	sel := primarySelector(loc)
	log.Printf("[pwbrowser] Check %s checked=%v", sel, checked)
	_, err := b.call(ctx, "check", map[string]interface{}{"selector": sel, "checked": checked})
	return err
}

func (b *Browser) KeyPress(ctx context.Context, loc domain.Locator, key string) error {
	sel := primarySelector(loc)
	log.Printf("[pwbrowser] KeyPress %s key=%q", sel, key)
	_, err := b.call(ctx, "keypress", map[string]string{"selector": sel, "key": key})
	return err
}

func (b *Browser) TextVisible(ctx context.Context, text string) (bool, error) {
	raw, err := b.call(ctx, "textvisible", map[string]string{"text": text})
	if err != nil {
		return false, err
	}
	var v bool
	_ = json.Unmarshal(raw, &v)
	log.Printf("[pwbrowser] TextVisible %q → %v", text, v)
	return v, nil
}

func (b *Browser) URLContains(ctx context.Context, substring string) (bool, error) {
	raw, err := b.call(ctx, "urlcontains", map[string]string{"substring": substring})
	if err != nil {
		return false, err
	}
	var v bool
	_ = json.Unmarshal(raw, &v)
	log.Printf("[pwbrowser] URLContains %q → %v", substring, v)
	return v, nil
}

func (b *Browser) ElementExists(ctx context.Context, loc domain.Locator) (bool, error) {
	sel := primarySelector(loc)
	raw, err := b.call(ctx, "elementexists", map[string]string{"selector": sel})
	if err != nil {
		return false, err
	}
	var v bool
	_ = json.Unmarshal(raw, &v)
	log.Printf("[pwbrowser] ElementExists %s → %v", sel, v)
	return v, nil
}

func (b *Browser) GetText(ctx context.Context, loc domain.Locator) (string, error) {
	sel := primarySelector(loc)
	raw, err := b.call(ctx, "gettext", map[string]string{"selector": sel})
	if err != nil {
		return "", err
	}
	var v string
	_ = json.Unmarshal(raw, &v)
	log.Printf("[pwbrowser] GetText %s → %q", sel, v)
	return v, nil
}

func (b *Browser) Screenshot(ctx context.Context, name string) (string, error) {
	p := filepath.Join(b.evidenceDir, name+".png")
	_, err := b.call(ctx, "screenshot", map[string]string{"path": p})
	if err != nil {
		return "", err
	}
	log.Printf("[pwbrowser] Screenshot → %s", p)
	return p, nil
}
