package main

// Wired into main.go's subcommand dispatch by adding (immediately after
// the existing `args[0] == "login"` branch):
//
//   if len(args) > 1 && args[0] == "venue" && args[1] == "resolve" {
//       return runResolveCmd(context.Background(), args[2:], stdin, logOut, clk)
//   }
//
// runResolveCmd is the entry point this file exports for that wiring.
// Everything else in this file is helper plumbing (flag parsing, output
// formatting, the optional no-cache wrapper). Per Law 4, the cmd/
// package stays wiring-only: business logic lives in internal/resolver.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
)

// resolveFormat selects the output rendering for the resolved Venue.
type resolveFormat string

const (
	resolveFormatTable resolveFormat = "table"
	resolveFormatJSON  resolveFormat = "json"
)

// resolveOpts is the parsed shape of `venue resolve` flags.
type resolveOpts struct {
	query   string
	format  resolveFormat
	noCache bool
}

// resolverBackend bundles the provider+cache+cleanup the subcommand
// needs. The production wiring (openResolveBackend below) opens the
// real Resy client and the SQLite store; tests substitute their own
// provider + in-memory cache via openResolveBackendFn.
type resolverBackend struct {
	provider providers.Provider
	cache    resolver.CacheStore
	cleanup  func() error
}

// openResolveBackendFn is the test seam. Production points it at
// openResolveBackend; resolve_test.go swaps in a fake-backed builder
// so tests run without touching the network or filesystem. The shape
// mirrors openSnipeBackend's: pattern parity keeps the cmd/ wiring
// readable and uniformly testable.
var openResolveBackendFn = openResolveBackend

// runResolveCmd is the `venue resolve` subcommand entry point. It is
// the only export this file makes; everything else is private.
//
// Surface (as wired by main.go's dispatch):
//
//	resy-snipe venue resolve <url|slug+city|name>
//	    [-format=table|json] [-no-cache]
//
// Behavior:
//
//   - Parse the positional query into the appropriate VenueQuery
//     variant (URL / slug+city / name).
//   - Open the resolver backend (provider + cache).
//   - Run resolver.Resolve.
//   - Print the resolved Venue in the chosen format, or — on
//     AmbiguousError — print the candidate list and return a non-nil
//     error so main exits non-zero.
//
// The supplied out is where ALL user-visible output lands (resolved
// venue render AND error messages). slog logs land on the same writer
// at INFO level so the subcommand stays consistent with the rest of
// the CLI's logging shape. Returning a non-nil error from the entry
// point is how main signals a non-zero exit code.
func runResolveCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	opts, err := parseResolveFlags(args, out)
	if err != nil {
		return err
	}

	logger := newCLILogger(out, slog.LevelInfo)

	query, err := buildVenueQuery(opts.query)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	backend, err := openResolveBackendFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("resolve bootstrap: %w", err)
	}
	defer func() {
		if backend.cleanup != nil {
			_ = backend.cleanup()
		}
	}()

	// -no-cache wraps the real cache in a read-bypass shim that always
	// reports a miss. Writes still go through so a fresh resolution
	// populates the cache for the next call.
	cache := backend.cache
	if opts.noCache {
		cache = &noCacheReadShim{inner: backend.cache}
	}

	r := resolver.New(backend.provider, cache, clk, resolver.WithLogger(logger))

	venue, resErr := r.Resolve(ctx, query)
	if resErr != nil {
		return renderResolveError(out, resErr)
	}
	return renderVenue(out, venue, opts.format)
}

// parseResolveFlags pulls the -format and -no-cache flags off args and
// returns the positional query as opts.query. ContinueOnError means a
// malformed flag surfaces through the returned error rather than
// flag.Exit-ing; usage is written to out so test harnesses can assert
// on it without scraping stderr.
func parseResolveFlags(args []string, out io.Writer) (resolveOpts, error) {
	fs := flag.NewFlagSet("venue resolve", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintf(out, "Usage: resy-snipe venue resolve <url|slug+city|name> [-format=table|json] [-no-cache]\n")
		fs.PrintDefaults()
	}

	format := fs.String("format", string(resolveFormatTable),
		"output format: table or json")
	noCache := fs.Bool("no-cache", false,
		"bypass the resolver cache for reads (writes still populate it)")

	if err := fs.Parse(args); err != nil {
		return resolveOpts{}, fmt.Errorf("parse flags: %w", err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return resolveOpts{}, errors.New("resolve: missing query argument")
	}
	// Join any trailing positional args with a space so the user can type
	// `venue resolve carbone nyc` without quoting. The query parser
	// downstream splits on the single delimiter it cares about.
	query := strings.TrimSpace(strings.Join(rest, " "))
	if query == "" {
		return resolveOpts{}, errors.New("resolve: empty query")
	}

	f := resolveFormat(strings.ToLower(strings.TrimSpace(*format)))
	switch f {
	case resolveFormatTable, resolveFormatJSON:
	default:
		return resolveOpts{}, fmt.Errorf("resolve: invalid -format %q (want table|json)", *format)
	}

	return resolveOpts{
		query:   query,
		format:  f,
		noCache: *noCache,
	}, nil
}

// buildVenueQuery picks the right VenueQuery variant for a raw user
// string:
//
//   - http/https prefix → ParseResyURL → VenueQueryURL
//   - contains '+' or ':' → split into VenueQuerySlug
//   - everything else → VenueQueryName (freeform)
//
// The slug+city heuristic explicitly does NOT trigger on a plain
// space-separated input ("astoria dc washington-dc") because freeform
// names also contain spaces; relying on '+' or ':' as a delimiter
// keeps the two domains separable without a magic-word guess.
func buildVenueQuery(raw string) (domain.VenueQuery, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("query is empty")
	}

	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		q, _, err := resolver.ParseResyURL(trimmed)
		if err != nil {
			return nil, err
		}
		return q, nil
	}

	// Slug+city pair, written either with '+' or ':' as the separator.
	// Two-tokens-on-either-side is the only valid shape; we trim each
	// side and reject empties so the resolver sees clean input.
	for _, sep := range []string{"+", ":"} {
		if !strings.Contains(trimmed, sep) {
			continue
		}
		parts := strings.SplitN(trimmed, sep, 2)
		slug := strings.ToLower(strings.TrimSpace(parts[0]))
		city := strings.ToLower(strings.TrimSpace(parts[1]))
		if slug == "" || city == "" {
			return nil, fmt.Errorf("slug+city: both sides of %q are required (got slug=%q city=%q)", sep, slug, city)
		}
		return domain.VenueQuerySlug{Slug: slug, City: city}, nil
	}

	// Default: freeform name. The resolver normalizes whitespace
	// internally so we forward the user's input verbatim.
	return domain.VenueQueryName{Name: trimmed}, nil
}

// renderVenue writes the resolved Venue in the requested format. The
// table layout is deliberately columnar (aligned label : value) so a
// human reading it in a terminal can scan it; the JSON form is the
// stable machine-readable surface (script-friendly, no surprises like
// embedded ANSI).
func renderVenue(out io.Writer, v domain.Venue, format resolveFormat) error {
	switch format {
	case resolveFormatJSON:
		tz := ""
		if v.TZ != nil {
			tz = v.TZ.String()
		}
		payload := map[string]string{
			"provider": string(v.Provider),
			"ref":      v.Ref,
			"name":     v.Name,
			"tz":       tz,
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return fmt.Errorf("render json: %w", err)
		}
		return nil
	case resolveFormatTable:
		tz := "<unset>"
		if v.TZ != nil {
			tz = v.TZ.String()
		}
		fprintf(out, "Resolved venue:\n")
		fprintf(out, "  Name:     %s\n", v.Name)
		fprintf(out, "  Provider: %s\n", v.Provider)
		fprintf(out, "  Ref:      %s\n", v.Ref)
		fprintf(out, "  TZ:       %s\n", tz)
		return nil
	default:
		return fmt.Errorf("render: unknown format %q", format)
	}
}

// renderResolveError translates a resolver error into a user-visible
// message + the non-zero exit signal (a non-nil returned error). The
// branches:
//
//   - *AmbiguousError: print the picker list with slug+city hints (the
//     domain.Venue type doesn't carry slug+city directly, so we render
//     the canonical provider:ref + name and instruct the user to
//     narrow the query).
//   - ErrVenueNotFound: print a "not found" line.
//   - everything else: forward the error verbatim (already wrapped by
//     the resolver layer with enough context).
func renderResolveError(out io.Writer, err error) error {
	var amb *resolver.AmbiguousError
	if errors.As(err, &amb) {
		fprintf(out, "Multiple matches — please be more specific:\n")
		for i, cand := range amb.Candidates {
			tz := "<unset>"
			if cand.TZ != nil {
				tz = cand.TZ.String()
			}
			fprintf(out, "  %d) %s (%s:%s, tz=%s)\n",
				i+1, cand.Name, cand.Provider, cand.Ref, tz)
		}
		return fmt.Errorf("ambiguous query %q (%d candidates)", amb.Query, len(amb.Candidates))
	}
	if errors.Is(err, resolver.ErrVenueNotFound) {
		fprintf(out, "Venue not found.\n")
		return err
	}
	fprintf(out, "Resolve failed: %v\n", err)
	return err
}

// openResolveBackend is the production wiring: it reuses the same
// store/Resy-client bootstrap as the snipe path so a single sqlite
// file backs both flows. The cleanup closes the underlying *sql.DB
// exactly once on subcommand exit.
//
// Distinct from openSnipeBackend in two ways: we don't need the
// engine.Provider's SlotPreparer surface (resolver only calls
// ResolveVenue + SearchVenuesByName), and we don't wire a notifier.
// Both differences fall out of the shared backend naturally because
// the returned providerAdapter already satisfies providers.Provider.
func openResolveBackend(
	ctx context.Context,
	logger *slog.Logger,
	clk clock.Clock,
) (resolverBackend, error) {
	rclient, sqlStore, cleanup, err := openSnipeBackend(ctx, logger, clk)
	if err != nil {
		return resolverBackend{}, err
	}
	return resolverBackend{
		provider: &providerAdapter{Client: rclient},
		cache:    newResolverCacheAdapter(sqlStore),
		cleanup:  cleanup,
	}, nil
}

// noCacheReadShim wraps a resolver.CacheStore so every read returns
// ErrCacheMiss, forcing a provider round-trip. Writes still flow
// through to the underlying store so the freshly-fetched row is
// available for the next (non-bypassing) call. This is the standard
// "force refresh" shape: bypass the cache for THIS resolve, keep the
// cache warm for the next one.
type noCacheReadShim struct {
	inner resolver.CacheStore
}

// Compile-time check.
var _ resolver.CacheStore = (*noCacheReadShim)(nil)

func (s *noCacheReadShim) UpsertVenueCache(
	ctx context.Context, slug, city string, venue domain.Venue, cachedAt time.Time,
) error {
	return s.inner.UpsertVenueCache(ctx, slug, city, venue, cachedAt)
}

func (s *noCacheReadShim) GetVenueCache(
	_ context.Context, _, _ string,
) (resolver.CachedVenue, error) {
	return resolver.CachedVenue{}, resolver.ErrCacheMiss
}

func (s *noCacheReadShim) UpsertNameSearchCache(
	ctx context.Context, query string, hits []domain.Venue, cachedAt time.Time,
) error {
	return s.inner.UpsertNameSearchCache(ctx, query, hits, cachedAt)
}

func (s *noCacheReadShim) GetNameSearchCache(
	_ context.Context, _ string,
) (resolver.CachedNameSearch, error) {
	return resolver.CachedNameSearch{}, resolver.ErrCacheMiss
}
