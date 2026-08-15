// Package stub provides a no-op Browser implementation for development and testing.
// It simulates a successful browser session without requiring Playwright or a real browser.
// Replace this with the real Playwright-backed browser for production use.
package stub

import (
	"context"
	"fmt"
	"log"

	"computer-use/domain"
)

// Browser is a no-op implementation of replay.Browser.
// Every call logs what it would do and returns success.
type Browser struct {
	// CurrentURL tracks the simulated current URL for URLContains assertions.
	CurrentURL string
	// VisibleTexts is a set of texts that TextVisible will report as visible.
	VisibleTexts map[string]bool
}

// New creates a stub browser starting at the given URL.
func New(startURL string) *Browser {
	return &Browser{
		CurrentURL:   startURL,
		VisibleTexts: make(map[string]bool),
	}
}

func (b *Browser) Navigate(_ context.Context, url string) error {
	log.Printf("[stub] Navigate → %s", url)
	b.CurrentURL = url
	return nil
}

func (b *Browser) Click(_ context.Context, loc domain.Locator) error {
	log.Printf("[stub] Click  locator=%s:%s", loc.Primary.Kind, loc.Primary.Value)
	return nil
}

func (b *Browser) Fill(_ context.Context, loc domain.Locator, value string) error {
	log.Printf("[stub] Fill   locator=%s:%s  value=%q", loc.Primary.Kind, loc.Primary.Value, value)
	return nil
}

func (b *Browser) Select(_ context.Context, loc domain.Locator, value string) error {
	log.Printf("[stub] Select locator=%s:%s  value=%q", loc.Primary.Kind, loc.Primary.Value, value)
	return nil
}

func (b *Browser) Check(_ context.Context, loc domain.Locator, checked bool) error {
	log.Printf("[stub] Check  locator=%s:%s  checked=%v", loc.Primary.Kind, loc.Primary.Value, checked)
	return nil
}

func (b *Browser) KeyPress(_ context.Context, loc domain.Locator, key string) error {
	log.Printf("[stub] KeyPress locator=%s:%s  key=%q", loc.Primary.Kind, loc.Primary.Value, key)
	return nil
}

func (b *Browser) TextVisible(_ context.Context, text string) (bool, error) {
	visible := b.VisibleTexts[text]
	log.Printf("[stub] TextVisible %q → %v", text, visible)
	return visible, nil
}

func (b *Browser) URLContains(_ context.Context, substring string) (bool, error) {
	contains := len(b.CurrentURL) > 0 && len(substring) > 0 &&
		containsStr(b.CurrentURL, substring)
	log.Printf("[stub] URLContains %q in %q → %v", substring, b.CurrentURL, contains)
	return contains, nil
}

func (b *Browser) ElementExists(_ context.Context, loc domain.Locator) (bool, error) {
	log.Printf("[stub] ElementExists locator=%s:%s → true", loc.Primary.Kind, loc.Primary.Value)
	return true, nil
}

func (b *Browser) GetText(_ context.Context, loc domain.Locator) (string, error) {
	log.Printf("[stub] GetText locator=%s:%s", loc.Primary.Kind, loc.Primary.Value)
	return fmt.Sprintf("stub-text[%s]", loc.Primary.Value), nil
}

func (b *Browser) Screenshot(_ context.Context, name string) (string, error) {
	path := fmt.Sprintf("/tmp/screenshots/%s.png", name)
	log.Printf("[stub] Screenshot → %s", path)
	return path, nil
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
