package status

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

type Style string

const (
	Bold   Style = "1"
	Dim    Style = "2"
	Red    Style = "31"
	Green  Style = "32"
	Yellow Style = "33"
	Cyan   Style = "36"
)

var enableVT = func(*os.File) bool { return true }

type Line struct {
	mu       sync.Mutex
	out      *os.File
	terminal bool
	color    bool
	text     string
}

func New(out *os.File) *Line {
	terminal := term.IsTerminal(int(out.Fd())) && os.Getenv("TERM") != "dumb" && enableVT(out)
	return &Line{
		out:      out,
		terminal: terminal,
		color:    terminal && os.Getenv("NO_COLOR") == "",
	}
}

func (l *Line) Paint(s string, styles ...Style) string {
	if !l.color || len(styles) == 0 || s == "" {
		return s
	}
	codes := make([]string, len(styles))
	for i, style := range styles {
		codes[i] = string(style)
	}
	return "\033[" + strings.Join(codes, ";") + "m" + s + "\033[0m"
}

func (l *Line) Bar(fraction float64, width int) string {
	filled := int(min(max(fraction, 0), 1) * float64(width))
	return l.Paint(strings.Repeat("━", filled), Cyan) + l.Paint(strings.Repeat("─", width-filled), Dim)
}

func (l *Line) Width() int {
	if width, _, err := term.GetSize(int(l.out.Fd())); err == nil && width > 0 {
		return width
	}
	return 80
}

func (l *Line) Set(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text = text
	if l.terminal {
		fmt.Fprint(l.out, "\r\033[K"+truncate(text, l.Width()-1))
	}
}

func (l *Line) Clear() {
	l.Set("")
}

func (l *Line) Info(format string, args ...any) {
	l.log(l.Paint("›", Cyan, Bold), format, args...)
}

func (l *Line) Success(format string, args ...any) {
	l.log(l.Paint("✓", Green, Bold), format, args...)
}

func (l *Line) Warn(format string, args ...any) {
	l.log(l.Paint("!", Yellow, Bold), format, args...)
}

func (l *Line) Error(format string, args ...any) {
	l.log(l.Paint("✗", Red, Bold), format, args...)
}

func (l *Line) log(symbol, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminal {
		fmt.Fprint(l.out, "\r\033[K")
	}
	fmt.Fprintf(l.out, "%s %s\n", symbol, fmt.Sprintf(format, args...))
	if l.terminal {
		fmt.Fprint(l.out, truncate(l.text, l.Width()-1))
	}
}

func VisibleLen(s string) int {
	n := 0
	escape := false
	for _, r := range s {
		switch {
		case escape:
			escape = r != 'm'
		case r == '\033':
			escape = true
		default:
			n++
		}
	}
	return n
}

func Bytes(n float64) string {
	return scale(n, 1024, "B", "KiB", "MiB", "GiB", "TiB")
}

func Bits(bytesPerSecond float64) string {
	return scale(bytesPerSecond*8, 1000, "bps", "Kbps", "Mbps", "Gbps", "Tbps")
}

func scale(n, base float64, units ...string) string {
	i := 0
	for n >= base && i < len(units)-1 {
		n /= base
		i++
	}
	if i == 0 || n >= 10 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

func truncate(s string, limit int) string {
	var b strings.Builder
	visible := 0
	escape := false
	for _, r := range s {
		switch {
		case escape:
			b.WriteRune(r)
			escape = r != 'm'
		case r == '\033':
			b.WriteRune(r)
			escape = true
		case visible < limit:
			b.WriteRune(r)
			visible++
		}
	}
	return b.String()
}
