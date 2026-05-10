package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/BurntSushi/toml"
)

// ApplyTOML overlays a TOML document onto cfg. Unknown keys are an
// error, not a warning — per docs/v2/design/daemon.md §3.2:
//
//	Unknown keys are an error, not a warning. The daemon refuses to
//	start with `unknown config key foo.bar at line 12`.
//
// The implementation uses toml.Decoder.Decode and then asks the
// metadata for Undecoded keys. The line number that surfaces in the
// error is the start-of-table line for any nested key, which is what
// BurntSushi/toml exposes; on a single-line value it is exact.
func (c *Config) ApplyTOML(r io.Reader) error {
	meta, err := toml.NewDecoder(r).Decode(c)
	if err != nil {
		return fmt.Errorf("config: parse TOML: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		var v ValidationError
		for _, key := range undecoded {
			v.addf("unknown config key %q", key.String())
		}
		return &v
	}
	return nil
}

// ApplyTOMLFile is a convenience wrapper that opens path and feeds
// it to ApplyTOML. Returns nil if path does not exist — callers
// chain this between defaults and env so a missing file is a no-op,
// not a startup failure (the operator may not have a config file).
//
// Other I/O errors (permission denied, EISDIR, etc.) are surfaced
// rather than swallowed.
func (c *Config) ApplyTOMLFile(path string) error {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied config path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := c.ApplyTOML(f); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return nil
}
