package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// flatAttr is the on-disk shape of one slog.Attr. Storing kind plus a
// stringified value lets us reconstruct the same typed Attr later for
// the kinds the engine actually uses (string, int, bool, time,
// duration). Other kinds (e.g. KindGroup) are stored as their string
// rendering and read back as slog.String.
type flatAttr struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// EncodeAttrs marshals attrs to a JSON array of {key, kind, value}.
func EncodeAttrs(attrs []slog.Attr) ([]byte, error) {
	if len(attrs) == 0 {
		return nil, nil
	}
	out := make([]flatAttr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, flatAttr{
			Key:   a.Key,
			Kind:  a.Value.Kind().String(),
			Value: stringifyValue(a.Value),
		})
	}
	return json.Marshal(out)
}

// DecodeAttrs reverses EncodeAttrs. Unknown / non-roundtrippable kinds
// are rebuilt as slog.String attributes — the audit log is a human
// surface, and a slightly degraded type on read is preferable to a
// hard decode failure.
func DecodeAttrs(data []byte) ([]slog.Attr, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var raw []flatAttr
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("DecodeAttrs: %w", err)
	}
	out := make([]slog.Attr, 0, len(raw))
	for _, f := range raw {
		out = append(out, decodeOneAttr(f))
	}
	return out, nil
}

func stringifyValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return v.String()
	}
}

func decodeOneAttr(f flatAttr) slog.Attr {
	switch f.Kind {
	case slog.KindInt64.String():
		if n, err := strconv.ParseInt(f.Value, 10, 64); err == nil {
			return slog.Int64(f.Key, n)
		}
	case slog.KindUint64.String():
		if n, err := strconv.ParseUint(f.Value, 10, 64); err == nil {
			return slog.Uint64(f.Key, n)
		}
	case slog.KindFloat64.String():
		if x, err := strconv.ParseFloat(f.Value, 64); err == nil {
			return slog.Float64(f.Key, x)
		}
	case slog.KindBool.String():
		if b, err := strconv.ParseBool(f.Value); err == nil {
			return slog.Bool(f.Key, b)
		}
	case slog.KindDuration.String():
		if d, err := time.ParseDuration(f.Value); err == nil {
			return slog.Duration(f.Key, d)
		}
	case slog.KindTime.String():
		if t, err := time.Parse(time.RFC3339Nano, f.Value); err == nil {
			return slog.Time(f.Key, t)
		}
	}
	return slog.String(f.Key, f.Value)
}
