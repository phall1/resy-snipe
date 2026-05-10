package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// PassphraseSource resolves to the bytes of the operator passphrase
// used to derive the secrets unwrap key. The boot path constructs
// exactly one of these from Config.Secrets.Mode (and Options
// overrides for tests / dev). The returned bytes are wiped by the
// caller after key derivation; sources are expected to zero any
// internal buffer they hold.
type PassphraseSource interface {
	Passphrase() ([]byte, error)
	// Describe returns a short operator-facing label that appears in
	// the boot banner ("passphrase (stdin)" / "keyfile /etc/...").
	Describe() string
}

// envPassphraseSource reads RESY_SNIPE_PASSPHRASE once and zeros the
// returned slice's source via os.Unsetenv. The env-var path is for
// systemd LoadCredential and Docker secrets, both of which deliver
// the value out-of-band and unset before child processes start.
type envPassphraseSource struct {
	getenv    func(string) string
	unsetenv  func(string) error
	cachedRaw []byte
}

// NewEnvPassphraseSource wires the env-based passphrase reader using
// os.Getenv / os.Unsetenv. Test code injects a fake by passing
// custom hooks; production code uses NewEnvPassphraseSourceDefault.
func NewEnvPassphraseSource(getenv func(string) string, unsetenv func(string) error) PassphraseSource {
	return &envPassphraseSource{getenv: getenv, unsetenv: unsetenv}
}

// NewEnvPassphraseSourceDefault is the production wiring.
func NewEnvPassphraseSourceDefault() PassphraseSource {
	return NewEnvPassphraseSource(os.Getenv, os.Unsetenv)
}

func (s *envPassphraseSource) Passphrase() ([]byte, error) {
	if s.cachedRaw != nil {
		out := make([]byte, len(s.cachedRaw))
		copy(out, s.cachedRaw)
		return out, nil
	}
	v := s.getenv("RESY_SNIPE_PASSPHRASE")
	if v == "" {
		return nil, errors.New("daemon: RESY_SNIPE_PASSPHRASE is unset")
	}
	s.cachedRaw = []byte(v)
	// Zero the env-var so it doesn't leak via /proc/<pid>/environ to
	// other processes on the same box.
	_ = s.unsetenv("RESY_SNIPE_PASSPHRASE")
	out := make([]byte, len(s.cachedRaw))
	copy(out, s.cachedRaw)
	return out, nil
}

func (s *envPassphraseSource) Describe() string {
	return "passphrase (env)"
}

// stdinPassphraseSource prompts the operator with echo disabled.
// The "no TTY → refuse" check belongs to the boot orchestrator,
// not the source — see boot.go for that policy. This source assumes
// stdin is a TTY.
type stdinPassphraseSource struct {
	prompt string
	in     *os.File
	out    io.Writer
}

// NewStdinPassphraseSource returns a PassphraseSource that prompts
// on the given TTY. prompt is the operator-facing line written to
// out before reading (e.g. "Passphrase: ").
func NewStdinPassphraseSource(in *os.File, out io.Writer, prompt string) PassphraseSource {
	return &stdinPassphraseSource{prompt: prompt, in: in, out: out}
}

func (s *stdinPassphraseSource) Passphrase() ([]byte, error) {
	if s.in == nil {
		s.in = os.Stdin
	}
	if s.out == nil {
		s.out = os.Stderr
	}
	if _, err := fmt.Fprint(s.out, s.prompt); err != nil {
		return nil, fmt.Errorf("daemon: write prompt: %w", err)
	}
	fd := int(s.in.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("daemon: stdin is not a terminal; pass --keyfile or set RESY_SNIPE_PASSPHRASE")
	}
	raw, err := term.ReadPassword(fd)
	if err != nil {
		return nil, fmt.Errorf("daemon: read passphrase: %w", err)
	}
	_, _ = fmt.Fprintln(s.out)
	if len(raw) == 0 {
		return nil, errors.New("daemon: empty passphrase rejected")
	}
	return raw, nil
}

func (s *stdinPassphraseSource) Describe() string {
	return "passphrase (stdin)"
}

// keyfileSource is a marker that boot.go uses to short-circuit
// passphrase derivation and call secrets.ReadKeyfile directly. It is
// still a PassphraseSource so Options can carry a uniform type, but
// Passphrase returns ErrUseKeyfile to signal the special-case path.
type keyfileSource struct {
	path string
}

// ErrUseKeyfile is returned by keyfileSource.Passphrase to signal
// the boot orchestrator that key derivation should happen via
// secrets.ReadKeyfile, not Argon2id.
var ErrUseKeyfile = errors.New("daemon: use keyfile source")

// NewKeyfileSource returns a PassphraseSource sentinel. The boot
// path checks errors.Is(err, ErrUseKeyfile) and reads the keyfile
// from KeyfilePath() instead.
func NewKeyfileSource(path string) PassphraseSource {
	return &keyfileSource{path: path}
}

func (k *keyfileSource) Passphrase() ([]byte, error) {
	return nil, fmt.Errorf("%w: %s", ErrUseKeyfile, k.path)
}

func (k *keyfileSource) Describe() string {
	return "keyfile " + k.path
}

// KeyfilePath returns the path the keyfileSource was constructed
// with. Boot uses this after seeing ErrUseKeyfile.
func (k *keyfileSource) KeyfilePath() string {
	return k.path
}

// ResolvePassphraseSource picks the active PassphraseSource based on
// cfg.Secrets.Mode and the caller-supplied options. An explicit
// Options.PassphraseSource always wins so tests can inject a deterministic
// source without touching env or TTY.
//
// The selection rules:
//
//	mode=passphrase + RESY_SNIPE_PASSPHRASE set ⟹ env source
//	mode=passphrase + RESY_SNIPE_PASSPHRASE unset ⟹ stdin source
//	mode=keyfile ⟹ keyfile source (requires cfg.Secrets.Keyfile)
//	mode=insecure ⟹ caller must handle separately (boot.go)
func ResolvePassphraseSource(cfg Config, opts Options) (PassphraseSource, error) {
	if opts.PassphraseSource != nil {
		return opts.PassphraseSource, nil
	}
	switch cfg.Secrets.Mode {
	case "passphrase":
		getenv := opts.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		if getenv("RESY_SNIPE_PASSPHRASE") != "" {
			return NewEnvPassphraseSource(getenv, os.Unsetenv), nil
		}
		return NewStdinPassphraseSource(os.Stdin, os.Stderr, "Passphrase: "), nil
	case "keyfile":
		if strings.TrimSpace(cfg.Secrets.Keyfile) == "" {
			return nil, errors.New("daemon: secrets.mode=keyfile but no keyfile path")
		}
		return NewKeyfileSource(filepath.Clean(cfg.Secrets.Keyfile)), nil
	case "insecure":
		return nil, errors.New("daemon: secrets.mode=insecure handled separately")
	}
	return nil, fmt.Errorf("daemon: unknown secrets.mode %q", cfg.Secrets.Mode)
}
