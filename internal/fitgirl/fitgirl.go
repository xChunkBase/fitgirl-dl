package fitgirl

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

var linkPattern = regexp.MustCompile(`https://fuckingfast\.co/\w+#.*?(?:\.part\d+\.rar|\.rar|\.bin)`)

func Links(ctx context.Context, pageURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to retrieve the page: %s", resp.Status)
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse page: %w", err)
	}

	var links []string
	seen := make(map[string]bool)
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode || n.DataAtom != atom.A {
			continue
		}
		for _, attr := range n.Attr {
			if attr.Key == "href" && linkPattern.MatchString(attr.Val) && !seen[attr.Val] {
				seen[attr.Val] = true
				links = append(links, attr.Val)
			}
		}
	}
	return links, nil
}

func Slug(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "fitgirl"
	}
	slug := path.Base(strings.TrimRight(u.Path, "/"))
	if slug == "" || slug == "." || slug == "/" {
		return "fitgirl"
	}
	return slug
}

func FileName(link string) string {
	base, fragment, _ := strings.Cut(link, "#")
	if name, err := url.PathUnescape(fragment); err == nil {
		fragment = name
	}
	if name := clean(fragment); name != "" {
		return name
	}
	if u, err := url.Parse(base); err == nil {
		if name := clean(path.Base(u.Path)); name != "" {
			return name
		}
	}
	return "download.bin"
}

func clean(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}
