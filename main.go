package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fitgirl-dl/internal/downloader"
	"fitgirl-dl/internal/fitgirl"
	"fitgirl-dl/internal/resolver"
	"fitgirl-dl/internal/status"
)

const (
	maxFiles     = 4
	connsPerFile = 8
)

const usage = `Usage: fitgirl-dl <url> [--download [path]]

Resolves the direct download links of a FitGirl repack page.

  <url>               FitGirl repack page, e.g. https://fitgirl-repacks.site/content-name
  --download [path]   download the files instead of printing the links
                      (default path: ~/Downloads/<content-name>)
`

var errHelp = errors.New("help requested")

type options struct {
	pageURL  string
	download bool
	dir      string
}

func main() {
	line := status.New(os.Stderr)

	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		line.Error("%v", err)
		fmt.Fprint(os.Stderr, "\n"+usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, line, opts)
	interrupted := ctx.Err() != nil
	stop()

	if interrupted {
		line.Warn("Interrupted")
		os.Exit(130)
	}
	if err != nil {
		line.Error("%v", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (options, error) {
	var opts options
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "-help" || arg == "--help":
			return opts, errHelp
		case arg == "-download" || arg == "--download":
			opts.download = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !isURL(args[i+1]) {
				i++
				opts.dir = args[i]
			}
		case strings.HasPrefix(arg, "-download=") || strings.HasPrefix(arg, "--download="):
			opts.download = true
			_, opts.dir, _ = strings.Cut(arg, "=")
			if opts.dir == "" {
				return opts, fmt.Errorf("%s requires a path", strings.TrimSuffix(arg, "="))
			}
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag: %s", arg)
		case opts.pageURL != "":
			return opts, fmt.Errorf("unexpected argument: %s", arg)
		default:
			opts.pageURL = arg
		}
	}

	if opts.pageURL == "" {
		return opts, errors.New("missing url")
	}
	if !isURL(opts.pageURL) {
		return opts, fmt.Errorf("invalid url: %s", opts.pageURL)
	}
	return opts, nil
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func run(ctx context.Context, line *status.Line, opts options) error {
	line.Info("Fetching %s", line.Paint(opts.pageURL, status.Cyan))
	links, err := fitgirl.Links(ctx, opts.pageURL)
	if err != nil {
		return err
	}
	if len(links) == 0 {
		return errors.New("no FuckingFast links found")
	}
	line.Info("Found %s", line.Paint(fmt.Sprintf("%d links", len(links)), status.Bold))

	if !opts.download {
		var directs []string
		err := resolveAll(ctx, line, links,
			func(current, total int) {
				line.Set(line.Paint("Resolving", status.Bold) + " " +
					line.Bar(float64(current-1)/float64(total), 30) + " " +
					fmt.Sprintf("%d/%d", current, total))
			},
			func(_, direct string) {
				directs = append(directs, direct)
			})
		line.Clear()
		for _, direct := range directs {
			fmt.Println(direct)
		}
		if err != nil {
			return err
		}
		line.Success("Resolved %d/%d links", len(directs), len(links))
		return nil
	}

	dir, err := downloadDir(opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	line.Info("Saving to %s", line.Paint(dir, status.Cyan))

	start := time.Now()
	dl := downloader.New(dir, maxFiles, connsPerFile, fitgirl.UserAgent, line)
	resolveErr := resolveAll(ctx, line, links,
		func(current, total int) {
			dl.SetStage(line.Paint("links ", status.Dim) + fmt.Sprintf("%d/%d", current, total))
		},
		func(name, direct string) {
			dl.Add(ctx, downloader.Job{URL: direct, Name: name})
		})
	dl.SetStage("")
	if err := errors.Join(resolveErr, dl.Wait()); err != nil {
		return err
	}

	elapsed := time.Since(start)
	bytes := float64(dl.Downloaded())
	summary := "already up to date"
	if bytes > 0 {
		summary = fmt.Sprintf("%s in %s, %s", status.Bytes(bytes), elapsed.Round(time.Second), status.Bits(bytes/elapsed.Seconds()))
	}
	line.Success("%d files saved to %s %s", len(links), line.Paint(dir, status.Cyan), line.Paint("("+summary+")", status.Dim))
	return nil
}

func resolveAll(ctx context.Context, line *status.Line, links []string, progress func(current, total int), handle func(name, direct string)) error {
	res, err := resolver.New(ctx, line.Warn)
	if err != nil {
		return err
	}
	defer res.Close()

	failed := 0
	for i, link := range links {
		name := fitgirl.FileName(link)
		progress(i+1, len(links))

		direct, err := res.Resolve(ctx, link)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, resolver.ErrBrowserStopped) {
			return err
		}
		if err != nil {
			line.Error("%s  %s", name, line.Paint("could not be resolved: "+err.Error(), status.Red))
			failed++
			continue
		}
		handle(name, direct)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d links could not be resolved", failed, len(links))
	}
	return nil
}

func downloadDir(opts options) (string, error) {
	if opts.dir != "" {
		return filepath.Abs(opts.dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Downloads", fitgirl.Slug(opts.pageURL)), nil
}
