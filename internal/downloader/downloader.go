package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fitgirl-dl/internal/status"
)

const (
	segmentSize  = 16 << 20
	bufferSize   = 1 << 20
	maxRetries   = 5
	idleTimeout  = 30 * time.Second
	tickInterval = 500 * time.Millisecond
	partSuffix   = ".download"
)

var errStalled = errors.New("connection stalled")

type Job struct {
	URL  string
	Name string
}

type segment struct {
	start int64
	end   int64
}

type Downloader struct {
	dir       string
	conns     int
	userAgent string
	client    *http.Client
	line      *status.Line
	slots     chan struct{}
	jobs      sync.WaitGroup
	stop      chan struct{}
	stopped   chan struct{}

	total      atomic.Int64
	downloaded atomic.Int64

	mu        sync.Mutex
	stage     string
	added     int
	completed int
	failed    int
}

func New(dir string, files, conns int, userAgent string, line *status.Line) *Downloader {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   files * conns,
		ReadBufferSize:        256 << 10,
		DisableCompression:    true,
		Protocols:             protocols,
	}

	d := &Downloader{
		dir:       dir,
		conns:     conns,
		userAgent: userAgent,
		client:    &http.Client{Transport: transport},
		line:      line,
		slots:     make(chan struct{}, files),
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	go d.render()
	return d
}

func (d *Downloader) Add(ctx context.Context, job Job) {
	d.mu.Lock()
	d.added++
	d.mu.Unlock()

	d.jobs.Go(func() {
		select {
		case d.slots <- struct{}{}:
		case <-ctx.Done():
			d.fail(ctx, job, ctx.Err())
			return
		}
		defer func() { <-d.slots }()

		if err := d.download(ctx, job); err != nil {
			d.fail(ctx, job, err)
			return
		}
		d.mu.Lock()
		d.completed++
		d.mu.Unlock()
	})
}

func (d *Downloader) Wait() error {
	d.jobs.Wait()
	close(d.stop)
	<-d.stopped

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed > 0 {
		return fmt.Errorf("%d of %d files failed to download", d.failed, d.added)
	}
	return nil
}

func (d *Downloader) SetStage(stage string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stage = stage
}

func (d *Downloader) Downloaded() int64 {
	return d.downloaded.Load()
}

func (d *Downloader) fail(ctx context.Context, job Job, err error) {
	d.mu.Lock()
	d.failed++
	d.mu.Unlock()
	if ctx.Err() == nil {
		d.line.Error("%s  %s", job.Name, d.line.Paint(err.Error(), status.Red))
	}
}

func (d *Downloader) download(ctx context.Context, job Job) error {
	size, ranged, err := d.probe(ctx, job.URL)
	if err != nil {
		return err
	}

	final := filepath.Join(d.dir, job.Name)
	if info, err := os.Stat(final); err == nil && size > 0 && info.Size() == size {
		d.line.Info("%s  %s", job.Name, d.line.Paint("already downloaded, skipped", status.Dim))
		return nil
	}
	if size > 0 {
		d.total.Add(size)
	}

	tmp := final + partSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	if ranged {
		err = d.segmented(ctx, f, job.URL, size)
	} else {
		err = d.single(ctx, f, job.URL)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}

	if size > 0 {
		d.line.Success("%s  %s", job.Name, d.line.Paint(status.Bytes(float64(size)), status.Dim))
	} else {
		d.line.Success("%s", job.Name)
	}
	return nil
}

func (d *Downloader) probe(ctx context.Context, rawURL string) (int64, bool, error) {
	var err error
	for attempt := range maxRetries + 1 {
		if attempt > 0 {
			if err := backoff(ctx, attempt); err != nil {
				return 0, false, err
			}
		}

		var size int64
		var ranged bool
		size, ranged, err = d.probeOnce(ctx, rawURL)
		if err == nil {
			return size, ranged, nil
		}
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
	}
	return 0, false, err
}

func (d *Downloader) probeOnce(ctx context.Context, rawURL string) (int64, bool, error) {
	resp, err := d.get(ctx, rawURL, "bytes=0-0")
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		size := totalSize(resp.Header.Get("Content-Range"))
		return size, size > 0, nil
	case http.StatusOK:
		return resp.ContentLength, false, nil
	default:
		return 0, false, fmt.Errorf("unexpected status: %s", resp.Status)
	}
}

func (d *Downloader) segmented(ctx context.Context, f *os.File, rawURL string, size int64) error {
	if err := f.Truncate(size); err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	segments := make(chan segment)
	go func() {
		defer close(segments)
		for start := int64(0); start < size; start += segmentSize {
			select {
			case segments <- segment{start: start, end: min(start+segmentSize, size) - 1}:
			case <-ctx.Done():
				return
			}
		}
	}()

	workers := min(int64(d.conns), (size+segmentSize-1)/segmentSize)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			buf := make([]byte, bufferSize)
			for seg := range segments {
				if err := d.fetchSegment(ctx, f, rawURL, seg, buf); err != nil {
					cancel(err)
					return
				}
			}
		})
	}
	wg.Wait()
	return context.Cause(ctx)
}

func (d *Downloader) fetchSegment(ctx context.Context, f *os.File, rawURL string, seg segment, buf []byte) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := backoff(ctx, attempt); err != nil {
				return err
			}
		}

		var n int64
		n, err = d.fetch(ctx, f, rawURL, seg.start, seg.end, buf)
		seg.start += n
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if n > 0 {
			attempt = 0
		}
	}
	return err
}

func (d *Downloader) single(ctx context.Context, f *os.File, rawURL string) error {
	buf := make([]byte, bufferSize)
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := backoff(ctx, attempt); err != nil {
				return err
			}
		}
		if err := f.Truncate(0); err != nil {
			return err
		}

		var n int64
		n, err = d.fetch(ctx, f, rawURL, 0, -1, buf)
		if err == nil {
			return nil
		}
		d.downloaded.Add(-n)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

func (d *Downloader) fetch(ctx context.Context, f *os.File, rawURL string, start, end int64, buf []byte) (int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	byteRange, want, limit := "", http.StatusOK, int64(-1)
	if end >= 0 {
		byteRange = fmt.Sprintf("bytes=%d-%d", start, end)
		want = http.StatusPartialContent
		limit = end - start + 1
	}

	resp, err := d.get(ctx, rawURL, byteRange)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != want {
		return 0, fmt.Errorf("unexpected status: %s", resp.Status)
	}
	if limit < 0 {
		limit = resp.ContentLength
	}

	timer := time.AfterFunc(idleTimeout, func() { cancel(errStalled) })
	defer timer.Stop()

	var written int64
	for limit < 0 || written < limit {
		chunk := buf
		if limit >= 0 {
			chunk = buf[:min(int64(len(buf)), limit-written)]
		}

		n, readErr := fill(resp.Body, chunk, timer)
		if n > 0 {
			if _, err := f.WriteAt(chunk[:n], start+written); err != nil {
				return written, err
			}
			written += int64(n)
			d.downloaded.Add(int64(n))
		}

		if errors.Is(readErr, io.EOF) {
			if limit >= 0 && written < limit {
				return written, io.ErrUnexpectedEOF
			}
			return written, nil
		}
		if readErr != nil {
			if errors.Is(context.Cause(ctx), errStalled) {
				return written, errStalled
			}
			return written, readErr
		}
	}
	return written, nil
}

func (d *Downloader) get(ctx context.Context, rawURL, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", d.userAgent)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	return d.client.Do(req)
}

func (d *Downloader) render() {
	defer close(d.stopped)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	var speed float64
	lastBytes, lastTime := d.downloaded.Load(), time.Now()
	for {
		select {
		case <-d.stop:
			d.line.Clear()
			return
		case now := <-ticker.C:
			bytes := d.downloaded.Load()
			instant := float64(bytes-lastBytes) / now.Sub(lastTime).Seconds()
			lastBytes, lastTime = bytes, now
			if speed == 0 {
				speed = instant
			} else {
				speed = 0.3*instant + 0.7*speed
			}

			d.line.Set(d.statusText(bytes, speed))
		}
	}
}

func (d *Downloader) statusText(bytes int64, speed float64) string {
	d.mu.Lock()
	stage, added, completed := d.stage, d.added, d.completed
	d.mu.Unlock()

	if added == 0 {
		return stage
	}

	l := d.line
	total := d.total.Load()
	fraction := 0.0
	if total > 0 {
		fraction = float64(bytes) / float64(total)
	}

	parts := []string{
		l.Paint(fmt.Sprintf("%3.0f%%", min(fraction, 1)*100), status.Bold),
		status.Bytes(float64(bytes)) + " / " + status.Bytes(float64(total)),
		l.Paint(status.Bits(speed), status.Green),
		l.Paint("ETA ", status.Dim) + eta(total-bytes, speed),
		l.Paint("files ", status.Dim) + fmt.Sprintf("%d/%d", completed, added),
	}
	if stage != "" {
		parts = append(parts, stage)
	}

	text := strings.Join(parts, "  ")
	if width := min(30, l.Width()-status.VisibleLen(text)-2); width >= 8 {
		text = l.Bar(fraction, width) + " " + text
	}
	return text
}

func fill(r io.Reader, buf []byte, timer *time.Timer) (int, error) {
	var total int
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		if n > 0 {
			total += n
			timer.Reset(idleTimeout)
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func totalSize(contentRange string) int64 {
	_, total, ok := strings.Cut(contentRange, "/")
	if !ok {
		return -1
	}
	size, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return -1
	}
	return size
}

func backoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(min(time.Duration(1<<attempt)*time.Second, 30*time.Second))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func eta(remaining int64, speed float64) string {
	if remaining <= 0 || speed <= 0 {
		return "--"
	}
	return time.Duration(float64(remaining) / speed * float64(time.Second)).Round(time.Second).String()
}
