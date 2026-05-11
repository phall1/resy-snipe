package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/clock"
)

// newTCPListener is a tiny test helper that grabs a free 127.0.0.1
// port for the smoke test. Uses net.ListenConfig.Listen so the
// listener is bound to ctx (noctx lint compliance).
func newTCPListener(addr string) (net.Listener, error) {
	lc := net.ListenConfig{}
	return lc.Listen(context.Background(), "tcp", addr)
}

// TestServeBootAndShutdown smoke-tests the full serve composition:
// boot the daemon in insecure mode, hit /healthz, then signal shutdown
// via context cancel. Verifies the listener cleanly drains.
func TestServeBootAndShutdown(t *testing.T) { //nolint:paralleltest // env-driven dev-mode tripwire
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	configPath := filepath.Join(tmp, "config.toml")

	// Pick a free port.
	ln, err := newTCPListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	bind := ln.Addr().String()
	_ = ln.Close()

	configBody := `
[daemon]
bind = "` + bind + `"
data_dir = "` + dataDir + `"
log_format = "json"
log_level = "info"
shutdown_drain_seconds = 2

[secrets]
mode = "insecure"

[http]
trusted_proxies = ["127.0.0.0/8"]
`
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// devModeTripwire (boot.go) refuses --insecure-no-encryption when
	// RESY_SNIPE_PROD=1 or /.dockerenv is present. We're in neither;
	// nothing to set.

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var (
		out    bytes.Buffer
		runErr error
		wg     sync.WaitGroup
	)
	wg.Go(func() {
		runErr = runServeCmd(ctx,
			[]string{"-config", configPath, "-insecure-no-encryption"},
			strings.NewReader(""),
			io.Discard,
			clock.Real{})
		_ = out
	})

	// Wait for the listener to come up (poll /healthz).
	deadline := time.Now().Add(8 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, "http://"+bind+"/healthz", stdhttp.NoBody)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := stdhttp.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == stdhttp.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		cancel()
		wg.Wait()
		t.Fatalf("daemon did not become ready: runErr=%v", runErr)
	}

	// Trigger graceful shutdown via context cancellation.
	cancel()
	wg.Wait()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("serve returned: %v", runErr)
	}
}
