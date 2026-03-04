package logutil

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	colorReset  = "\x1b[0m"
	colorBold   = "\x1b[1m"
	colorFaint  = "\x1b[2m"
	colorRed    = "\x1b[31m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorBlue   = "\x1b[34m"
	colorCyan   = "\x1b[36m"
)

type blockValue struct {
	key   string
	value string
}

type TextHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
	color  bool
}

func NewTextHandler(w io.Writer, opts *slog.HandlerOptions, color bool) *TextHandler {
	var level slog.Leveler = slog.LevelInfo
	if opts != nil && opts.Level != nil {
		level = opts.Level
	}

	return &TextHandler{
		mu:    &sync.Mutex{},
		w:     w,
		level: level,
		color: color,
	}
}

func (h *TextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *TextHandler) Handle(_ context.Context, record slog.Record) error {
	inline := make([]string, 0, 8)
	blocks := make([]blockValue, 0, 4)

	if !record.Time.IsZero() {
		inline = append(inline, h.formatInline("time", record.Time.Format(time.RFC3339Nano), false))
	}
	inline = append(inline, h.formatLevel(record.Level))
	inline = append(inline, h.formatInline("msg", record.Message, true))

	allAttrs := make([]slog.Attr, 0, len(h.attrs)+record.NumAttrs()+1)
	allAttrs = append(allAttrs, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		allAttrs = append(allAttrs, attr)
		return true
	})

	for _, attr := range allAttrs {
		h.appendAttr(&inline, &blocks, nil, attr)
	}

	var b strings.Builder
	b.WriteString(strings.Join(inline, " "))
	b.WriteByte('\n')

	for _, block := range blocks {
		b.WriteString(h.colorize(colorCyan, block.key))
		b.WriteString(":\n")

		lines := strings.Split(block.value, "\n")
		for _, line := range lines {
			b.WriteString(h.colorize(colorFaint, "  "))
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *TextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := h.clone()
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return next
}

func (h *TextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	next := h.clone()
	next.groups = append(append([]string{}, h.groups...), name)
	return next
}

func (h *TextHandler) clone() *TextHandler {
	return &TextHandler{
		mu:     h.mu,
		w:      h.w,
		level:  h.level,
		attrs:  h.attrs,
		groups: h.groups,
		color:  h.color,
	}
}

func (h *TextHandler) appendAttr(inline *[]string, blocks *[]blockValue, extraGroups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}

	if attr.Value.Kind() == slog.KindGroup {
		groupName := attr.Key
		for _, child := range attr.Value.Group() {
			childGroups := extraGroups
			if groupName != "" {
				childGroups = append(append([]string{}, extraGroups...), groupName)
			}
			h.appendAttr(inline, blocks, childGroups, child)
		}
		return
	}

	keyParts := make([]string, 0, len(h.groups)+len(extraGroups)+1)
	keyParts = append(keyParts, h.groups...)
	keyParts = append(keyParts, extraGroups...)
	if attr.Key != "" {
		keyParts = append(keyParts, attr.Key)
	}
	key := strings.Join(keyParts, ".")
	if key == "" {
		return
	}

	value, multiline := formatValue(attr.Value)
	if multiline {
		*blocks = append(*blocks, blockValue{key: key, value: value})
		return
	}

	*inline = append(*inline, h.formatInline(key, value, true))
}

func (h *TextHandler) formatInline(key string, value string, quote bool) string {
	rendered := value
	if quote {
		rendered = quoteIfNeeded(value)
	}

	return h.colorize(colorCyan, key) + "=" + rendered
}

func (h *TextHandler) formatLevel(level slog.Level) string {
	text := level.String()
	switch {
	case level < slog.LevelInfo:
		text = h.colorize(colorBlue, text)
	case level < slog.LevelWarn:
		text = h.colorize(colorGreen, text)
	case level < slog.LevelError:
		text = h.colorize(colorYellow, text)
	default:
		text = h.colorize(colorRed, text)
	}

	return h.colorize(colorCyan, "level") + "=" + text
}

func (h *TextHandler) colorize(code string, text string) string {
	if !h.color || text == "" {
		return text
	}

	return code + text + colorReset
}

func formatValue(value slog.Value) (string, bool) {
	switch value.Kind() {
	case slog.KindString:
		text := value.String()
		return text, strings.Contains(text, "\n")
	case slog.KindBool:
		return strconv.FormatBool(value.Bool()), false
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10), false
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10), false
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64), false
	case slog.KindDuration:
		return value.Duration().String(), false
	case slog.KindTime:
		return value.Time().Format(time.RFC3339Nano), false
	case slog.KindAny:
		anyValue := value.Any()
		if err, ok := anyValue.(error); ok {
			text := err.Error()
			return text, strings.Contains(text, "\n")
		}
		text := fmt.Sprint(anyValue)
		return text, strings.Contains(text, "\n")
	default:
		text := value.String()
		return text, strings.Contains(text, "\n")
	}
}

func quoteIfNeeded(value string) string {
	if value == "" {
		return `""`
	}

	if strings.ContainsAny(value, " \t\n\r\"=") {
		return strconv.Quote(value)
	}

	return value
}
