package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

const (
	maxRetries       = 3
	maxRestarts      = 2
	attemptTimeout   = 2 * time.Minute
	challengeTimeout = 30 * time.Second
	settleDelay      = 1500 * time.Millisecond
	clickDelay       = 800 * time.Millisecond
	pollInterval     = 200 * time.Millisecond
	pollAttempts     = 25
	linkDelay        = time.Second
	closeTimeout     = 2 * time.Second
	aliveTimeout     = 5 * time.Second
	outputLimit      = 8 << 10
	outputLineLimit  = 200
)

var (
	ErrRateLimited    = errors.New("rate limited")
	ErrNotFound       = errors.New("download link not found")
	ErrChallenge      = errors.New("blocked by cloudflare challenge")
	ErrBrowserStopped = errors.New("browser keeps stopping")

	windowOpenPattern = regexp.MustCompile(`window\.open\(["'](https://[^"']*fuckingfast\.co/dl/[^"']+)["']`)
	rateLimitKeywords = []string{"rate limited", "rate limit", "too many requests"}
)

type Resolver struct {
	logf     func(format string, args ...any)
	session  *session
	restarts int
	last     time.Time
	mu       sync.Mutex
	found    string
	returned map[string]bool
	once     sync.Once
}

type session struct {
	launcher *launcher.Launcher
	browser  *rod.Browser
	page     *rod.Page
	output   *output
}

type output struct {
	mu   sync.Mutex
	data []byte
}

func New(ctx context.Context, logf func(format string, args ...any)) (*Resolver, error) {
	r := &Resolver{logf: logf, returned: make(map[string]bool)}
	s, err := r.launch(ctx)
	if err != nil {
		return nil, err
	}
	r.session = s
	return r, nil
}

func (r *Resolver) launch(ctx context.Context) (*session, error) {
	out := &output{}
	l := launcher.New().
		Context(ctx).
		HeadlessNew(true).
		Leakless(runtime.GOOS != "windows").
		Delete("enable-automation").
		Set("disable-blink-features", "AutomationControlled").
		Set("enable-logging", "stderr").
		Set("log-level", "3").
		Logger(out)
	if bin, ok := launcher.LookPath(); ok {
		l = l.Bin(bin)
	}

	controlURL, err := l.Launch()
	if err != nil {
		_ = os.RemoveAll(l.Get(flags.UserDataDir))
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	s := &session{
		launcher: l,
		browser:  rod.New().ControlURL(controlURL).NoDefaultDevice(),
		output:   out,
	}
	if err := r.setup(s); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (r *Resolver) setup(s *session) error {
	if err := s.browser.Connect(); err != nil {
		return fmt.Errorf("connect browser: %w", err)
	}

	version, err := proto.BrowserGetVersion{}.Call(s.browser)
	if err != nil {
		return fmt.Errorf("browser version: %w", err)
	}

	err = proto.BrowserSetDownloadBehavior{
		Behavior:      proto.BrowserSetDownloadBehaviorBehaviorDeny,
		EventsEnabled: true,
	}.Call(s.browser)
	if err != nil {
		return fmt.Errorf("download behavior: %w", err)
	}

	s.page, err = stealth.Page(s.browser)
	if err != nil {
		return fmt.Errorf("open page: %w", err)
	}

	err = s.page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent:      strings.ReplaceAll(version.UserAgent, "HeadlessChrome", "Chrome"),
		AcceptLanguage: "en-US,en;q=0.9",
	})
	if err != nil {
		return fmt.Errorf("user agent: %w", err)
	}

	go s.page.EachEvent(r.onRequest, r.onResponse)()
	go s.browser.EachEvent(func(e *proto.TargetTargetCreated) { r.onTargetCreated(s, e) }, r.onDownload)()
	return nil
}

func (r *Resolver) Close() {
	r.once.Do(r.session.close)
}

func (r *Resolver) restart(ctx context.Context) error {
	r.session.close()
	s, err := r.launch(ctx)
	if err != nil {
		return err
	}
	r.session = s
	return nil
}

func (s *session) close() {
	if s.browser != nil {
		_ = s.browser.Close()
	}
	if s.waitExit() {
		return
	}
	s.launcher.Kill()
	if !s.waitExit() {
		_ = os.RemoveAll(s.launcher.Get(flags.UserDataDir))
	}
}

func (s *session) waitExit() bool {
	done := make(chan struct{})
	go func() {
		s.launcher.Cleanup()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(closeTimeout):
		return false
	}
}

func (s *session) alive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
	defer cancel()
	_, err := s.page.Context(ctx).Eval(`() => true`)
	return err == nil
}

func (r *Resolver) Resolve(ctx context.Context, link string) (string, error) {
	limited := 0
	delay := time.Until(r.last.Add(linkDelay))
	for {
		if err := sleep(ctx, delay); err != nil {
			return "", err
		}

		direct, err := r.resolve(ctx, link)
		r.last = time.Now()
		switch {
		case err == nil:
			r.restarts = 0
			r.mu.Lock()
			r.returned[direct] = true
			r.mu.Unlock()
			return direct, nil
		case ctx.Err() != nil:
			return "", ctx.Err()
		case !r.session.alive():
			detail := ""
			if line := r.session.output.fatal(); line != "" {
				detail = fmt.Sprintf(" (Chrome: %s)", line)
			}
			if r.restarts == maxRestarts {
				return "", fmt.Errorf("%w after %d restarts%s: %v", ErrBrowserStopped, maxRestarts, detail, err)
			}
			r.restarts++
			r.logf("Browser stopped unexpectedly%s, restarting it (%d/%d)", detail, r.restarts, maxRestarts)
			if err := r.restart(ctx); err != nil {
				return "", err
			}
			delay = 0
		case errors.Is(err, ErrRateLimited):
			if limited == maxRetries {
				return "", fmt.Errorf("%w: max retries reached", ErrRateLimited)
			}
			limited++
			r.logf("Rate limited on %s. Retrying (%d/%d)...", link, limited, maxRetries)
			delay = time.Duration(1<<limited)*time.Second + time.Duration(1000+rand.IntN(1000))*time.Millisecond
		default:
			return "", err
		}
	}
}

func (r *Resolver) resolve(ctx context.Context, link string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	page := r.session.page.Context(ctx)
	r.setFound("")

	target, _, _ := strings.Cut(link, "#")
	if err := page.Navigate(target); err != nil {
		return "", fmt.Errorf("navigate: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return "", fmt.Errorf("wait load: %w", err)
	}
	if err := r.waitChallenge(ctx, page); err != nil {
		return "", err
	}
	if err := sleep(ctx, settleDelay); err != nil {
		return "", err
	}
	if direct := r.result(); direct != "" {
		return direct, nil
	}

	text, err := evalString(page, `() => document.body ? document.body.textContent : ''`)
	if err != nil {
		return "", err
	}
	lower := strings.ToLower(text)
	for _, keyword := range rateLimitKeywords {
		if strings.Contains(lower, keyword) {
			return "", ErrRateLimited
		}
	}
	if m := windowOpenPattern.FindStringSubmatch(text); m != nil {
		return m[1], nil
	}

	has, button, err := page.HasR("a", "/download/i")
	if err != nil {
		return "", err
	}
	if has {
		click(button)
		if err := sleep(ctx, clickDelay); err != nil {
			return "", err
		}
		click(button)

		for range pollAttempts {
			if direct := r.result(); direct != "" {
				return direct, nil
			}
			if err := sleep(ctx, pollInterval); err != nil {
				return "", err
			}
		}
	}

	if direct := r.result(); direct != "" {
		return direct, nil
	}
	return "", ErrNotFound
}

func (r *Resolver) waitChallenge(ctx context.Context, page *rod.Page) error {
	deadline := time.Now().Add(challengeTimeout)
	for {
		title, err := evalString(page, `() => document.title`)
		if err != nil {
			return err
		}
		if !strings.Contains(strings.ToLower(title), "just a moment") {
			if err := page.WaitLoad(); err != nil {
				return fmt.Errorf("wait load: %w", err)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return ErrChallenge
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

func (r *Resolver) onRequest(e *proto.NetworkRequestWillBeSent) {
	r.record(e.Request.URL, "")
}

func (r *Resolver) onResponse(e *proto.NetworkResponseReceived) {
	r.record(e.Response.URL, "")
	for name, value := range e.Response.Headers {
		switch strings.ToLower(name) {
		case "hx-redirect", "location":
			r.record(value.Str(), e.Response.URL)
		}
	}
}

func (r *Resolver) onTargetCreated(s *session, e *proto.TargetTargetCreated) {
	info := e.TargetInfo
	if info.Type != proto.TargetTargetInfoTypePage || info.OpenerID != s.page.TargetID {
		return
	}
	r.record(info.URL, "")
	go func() {
		_, _ = proto.TargetCloseTarget{TargetID: info.TargetID}.Call(s.browser)
	}()
}

func (r *Resolver) onDownload(e *proto.BrowserDownloadWillBegin) {
	r.record(e.URL, "")
}

func (r *Resolver) record(raw, base string) {
	if raw == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	if base != "" {
		b, err := url.Parse(base)
		if err != nil {
			return
		}
		u = b.ResolveReference(u)
	}
	if !strings.HasSuffix(u.Hostname(), "fuckingfast.co") || !strings.HasPrefix(u.Path, "/dl/") {
		return
	}

	direct := u.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.found == "" && !r.returned[direct] {
		r.found = direct
	}
}

func (r *Resolver) setFound(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.found = value
}

func (r *Resolver) result() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.found
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data = append(o.data, p...)
	if len(o.data) > outputLimit {
		o.data = o.data[len(o.data)-outputLimit:]
	}
	return len(p), nil
}

func (o *output) fatal() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, raw := range slices.Backward(bytes.Split(o.data, []byte("\n"))) {
		line := strings.TrimSpace(string(raw))
		if strings.Contains(line, ":FATAL:") {
			if len(line) > outputLineLimit {
				line = line[:outputLineLimit] + "..."
			}
			return line
		}
	}
	return ""
}

func click(el *rod.Element) {
	err := el.Timeout(5*time.Second).Click(proto.InputMouseButtonLeft, 1)
	if err != nil {
		_, _ = el.Eval(`() => this.click()`)
	}
}

func evalString(page *rod.Page, js string) (string, error) {
	res, err := page.Eval(js)
	if err != nil {
		return "", err
	}
	return res.Value.Str(), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
