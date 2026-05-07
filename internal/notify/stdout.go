package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
)

// stdoutTimestampFormat is the wall-clock prefix every line shares.
// HH:MM:SS is precise enough for live tailing without making the line
// noisy; the date is implied by "right now". Using a format constant
// (rather than time.Kitchen) keeps the layout aligned with the C2
// spec example: '[14:32:01] discovered -> awaiting'.
const stdoutTimestampFormat = "15:04:05"

// StdoutNotifier renders Notifier events as terse single-line text to
// an io.Writer (typically os.Stdout). Concurrent calls from different
// goroutines are safe; a single mutex serializes writes so transitions
// from the engine and a result line emitted from a different goroutine
// don't interleave mid-line.
//
// The Clock dependency is injected so tests can pin timestamps and
// production code never reaches for time.Now() directly.
type StdoutNotifier struct {
	mu  sync.Mutex
	w   io.Writer
	clk clock.Clock
}

// NewStdoutNotifier constructs a StdoutNotifier that writes to w using
// clk for the line timestamp. Both arguments are required; nil panics
// fail-fast at boot rather than producing a silent malformed output
// stream at runtime.
func NewStdoutNotifier(w io.Writer, clk clock.Clock) *StdoutNotifier {
	if w == nil {
		panic("notify: nil writer")
	}
	if clk == nil {
		panic("notify: nil clock")
	}
	return &StdoutNotifier{w: w, clk: clk}
}

// Compile-time interface check.
var _ Notifier = (*StdoutNotifier)(nil)

// Transition formats a single transition line:
//
//	[15:04:05] from -> to (k=v k=v ...)
//
// Attrs are appended in the order supplied. An empty attrs slice
// produces a trailing-paren-free line (i.e. no "()" suffix).
func (n *StdoutNotifier) Transition(
	_ context.Context,
	_ domain.SnipeID,
	from, to domain.Status,
	attrs []slog.Attr,
) {
	ts := n.clk.Now().Format(stdoutTimestampFormat)
	body := fmt.Sprintf("[%s] %s -> %s", ts, from, to)
	if extra := formatAttrs(attrs); extra != "" {
		body = body + " (" + extra + ")"
	}
	n.writeLine(body)
}

// Result emits the terminal-outcome line. The format mirrors the
// transition format's single-line discipline so a tailing user can
// scan the stream uniformly:
//
//	[15:04:05] BOOKED: <message> (k=v ...)
//	[15:04:05] FAILED: <message> (k=v ...)
//
// We intentionally use uppercase BOOKED / FAILED so the success and
// failure cases stand out from the lowercase status names emitted by
// Transition.
func (n *StdoutNotifier) Result(
	_ context.Context,
	_ domain.SnipeID,
	success bool,
	message string,
	attrs []slog.Attr,
) {
	ts := n.clk.Now().Format(stdoutTimestampFormat)
	verdict := "FAILED"
	if success {
		verdict = "BOOKED"
	}
	body := fmt.Sprintf("[%s] %s: %s", ts, verdict, message)
	if extra := formatAttrs(attrs); extra != "" {
		body = body + " (" + extra + ")"
	}
	n.writeLine(body)
}

// Close is a no-op for the stdout notifier — there is no buffered
// state. The method exists so StdoutNotifier satisfies the Notifier
// interface; future buffered/async impls supply real flush semantics.
func (*StdoutNotifier) Close() error { return nil }

// writeLine acquires the mutex, writes the body + a single newline,
// and ignores any write error. The notifier is non-load-bearing —
// stdout being closed mid-snipe must not unwind the engine.
func (n *StdoutNotifier) writeLine(body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, _ = fmt.Fprintln(n.w, body)
}

// formatAttrs renders []slog.Attr as a space-separated "k=v" list. We
// purposely avoid a heavyweight slog handler here: the output stream
// is for humans, not log aggregators, and a stable single-line layout
// is the point.
//
// Group-valued attrs are flattened with a "group.key=value" prefix so
// nested values still produce a single line.
func formatAttrs(attrs []slog.Attr) string {
	if len(attrs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(attrs))
	for _, a := range attrs {
		parts = append(parts, renderAttr("", a)...)
	}
	return strings.Join(parts, " ")
}

func renderAttr(prefix string, a slog.Attr) []string {
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		members := v.Group()
		out := make([]string, 0, len(members))
		for _, m := range members {
			out = append(out, renderAttr(key, m)...)
		}
		return out
	}
	return []string{key + "=" + v.String()}
}
