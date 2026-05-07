package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"resy-snipe/internal/domain"
)

// Legacy venue IDs preserved from _legacy/config/resy-config.go.bak as a
// convenience list for the interactive prompt. Users can still enter any
// custom venue id at the prompt.
const (
	venueDeadRabbit = 38660
	venueRubirosa   = 466
	venueRedPearl   = 69820
	venueRafs       = 65679
	venueCarbone    = 6194
	venueDonAngie   = 1505
	venueSanSabino  = 78799
	venueGertrudes  = 71935
	venueAuCheval   = 5769
	venueHOWOO      = 86696
)

// venueOption is a built-in (Name, Resy venue id) pair shown by the
// interactive prompt.
type venueOption struct {
	Name string
	ID   int
}

// venueOptions is the convenience list of venues the interactive prompt
// shows. The list is a presentation-only nicety: any numeric Resy venue
// id is accepted at the prompt.
var venueOptions = []venueOption{
	{Name: "Dead Rabbit", ID: venueDeadRabbit},
	{Name: "Rubirosa", ID: venueRubirosa},
	{Name: "Red Pearl", ID: venueRedPearl},
	{Name: "Rafs", ID: venueRafs},
	{Name: "Carbone", ID: venueCarbone},
	{Name: "Don Angie", ID: venueDonAngie},
	{Name: "San Sabino", ID: venueSanSabino},
	{Name: "Gertrudes", ID: venueGertrudes},
	{Name: "Au Cheval", ID: venueAuCheval},
	{Name: "HOWOO", ID: venueHOWOO},
}

// release strategy enum values for the -release-strategy flag.
const (
	strategyExplicit   = "explicit"
	strategyDiscovered = "discovered"
	strategyContinuous = "continuous"
)

// cliOptions captures the post-parse, pre-validation form of the CLI
// flags. Interactive prompts also write into this struct, so the path
// from raw input → validated domain.Intent is single-rooted.
type cliOptions struct {
	interactive     bool
	resDate         string
	partySize       int
	venueID         int
	resTimes        string
	tableTypes      string
	snipeDate       string
	snipeTime       string
	releaseStrategy string
	retryWindow     time.Duration
	logLevel        string
	// user is the email address that identifies which persisted
	// session to load before running a snipe. When empty, the snipe
	// path skips the session-load step and the engine will (in a
	// future task) operate in unauthenticated discovery mode. The
	// canonical "no valid session" error message is surfaced when
	// this flag is set but no usable session is in the store.
	user string

	// snipeTimeProvided is true when the user explicitly supplied
	// -snipe-time. It influences the default release strategy: explicit
	// when a snipe time is provided, discovered otherwise.
	snipeTimeProvided bool
	// releaseStrategyProvided is true when the user explicitly supplied
	// -release-strategy; defaults are filled in only when this is false.
	releaseStrategyProvided bool
}

// defaultRetryWindow bounds the polling envelope for ContinuousRelease
// and the ProbeUntil-ProbeFrom span for DiscoveredRelease. 30m matches
// Resy's typical observed-release jitter.
const defaultRetryWindow = 30 * time.Minute

// parseFlags parses the legacy flag set plus the two new C1 flags into
// a cliOptions value. It accepts an explicit args slice and an io.Writer
// for usage/error output so it can be exercised in tests without
// touching real os.Args / os.Stderr.
func parseFlags(args []string, out io.Writer) (cliOptions, error) {
	opts := cliOptions{
		partySize:       -1,
		venueID:         -1,
		retryWindow:     defaultRetryWindow,
		releaseStrategy: "",
	}

	fs := flag.NewFlagSet("resy-snipe", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.BoolVar(&opts.interactive, "interactive", false, "Prompt for settings interactively.")
	fs.StringVar(&opts.resDate, "date", "", "Reservation date (YYYY-MM-DD).")
	fs.IntVar(&opts.partySize, "party-size", -1, "Party size.")
	fs.IntVar(&opts.venueID, "venue-id", -1, "Venue ID.")
	fs.StringVar(&opts.resTimes, "res-times", "", "Reservation times (comma-separated, HH:MM or HH:MM:SS).")
	fs.StringVar(&opts.tableTypes, "table-types", "", "Table types (comma-separated). Use 'none' for any.")
	fs.StringVar(&opts.snipeDate, "snipe-date", "", "Snipe date (YYYY-MM-DD). Defaults to reservation date.")
	fs.StringVar(&opts.snipeTime, "snipe-time", "", "Snipe time (HH:MM). Defaults to midnight.")
	fs.StringVar(&opts.releaseStrategy, "release-strategy", "",
		"Release strategy: explicit|discovered|continuous. Default: explicit if -snipe-time given, else discovered.")
	fs.DurationVar(&opts.retryWindow, "retry-window", defaultRetryWindow,
		"Bounds the engine's polling window for ContinuousRelease and the probe span for DiscoveredRelease.")
	fs.StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.StringVar(&opts.user, "user", "",
		"Email of the previously logged-in user; loads the persisted Resy session. "+
			"Run `resy-snipe login` first to populate.")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, fmt.Errorf("parsing flags: %w", err)
	}

	// Detect explicit user-supplied flags by walking fs.Visit (only set
	// flags appear there). This is what lets us choose a sensible default
	// for -release-strategy and tells toIntent whether the user pinned a
	// snipe time.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "snipe-time":
			opts.snipeTimeProvided = true
		case "release-strategy":
			opts.releaseStrategyProvided = true
		}
	})

	return opts, nil
}

// applyDefaults fills in any unset legacy flags with the legacy defaults
// from _legacy/config/resy-config.go.bak (party size 2, Dead Rabbit,
// 18:45:00, snipe date == reservation date, snipe time 00:00). The
// reservation-date default is computed from the supplied now (passed
// in for testability) rather than the wall clock so toIntent stays
// deterministic.
func (o *cliOptions) applyDefaults(now time.Time) {
	if o.partySize < 0 {
		o.partySize = 2
	}
	if o.venueID < 0 {
		o.venueID = venueDeadRabbit
	}
	if strings.TrimSpace(o.resDate) == "" {
		o.resDate = now.AddDate(0, 0, 7).Format("2006-01-02")
	}
	if strings.TrimSpace(o.resTimes) == "" {
		o.resTimes = "18:45:00"
	}
	if strings.TrimSpace(o.snipeDate) == "" {
		o.snipeDate = o.resDate
	}
	if strings.TrimSpace(o.snipeTime) == "" {
		o.snipeTime = "00:00"
	}
}

// runInteractive walks the prompt sequence, mutating opts in place. The
// reader is bufio.Reader so the legacy newline-delimited prompt UX
// carries forward unchanged. promptOut is where the prompts and
// validation messages are written.
func runInteractive(opts *cliOptions, in io.Reader, promptOut io.Writer) error {
	reader := bufio.NewReader(in)
	var err error

	if opts.resDate, err = promptDate(reader, promptOut, "Reservation date (YYYY-MM-DD)", opts.resDate); err != nil {
		return err
	}
	if opts.partySize, err = promptInt(reader, promptOut, "Party size", opts.partySize); err != nil {
		return err
	}
	if opts.venueID, err = promptVenue(reader, promptOut, opts.venueID); err != nil {
		return err
	}
	if opts.resTimes, err = promptResTimes(reader, promptOut, "Reservation times (comma-separated, HH:MM)", opts.resTimes); err != nil {
		return err
	}
	if opts.tableTypes, err = promptTableTypes(reader, promptOut, "Table types (comma-separated, optional, or 'none')", opts.tableTypes); err != nil {
		return err
	}
	if opts.snipeDate, err = promptDate(reader, promptOut, "Snipe date (YYYY-MM-DD)", opts.snipeDate); err != nil {
		return err
	}
	if opts.snipeTime, err = promptClockTime(reader, promptOut, "Snipe time (HH:MM)", opts.snipeTime); err != nil {
		return err
	}
	return nil
}

// fprintln/fprintf wrap fmt's variants with discarded return values.
// The prompt UX writes to a user-facing terminal where a transient
// write error is benign — losing a "please re-enter" message is far
// less disruptive than aborting the whole interactive flow.
func fprintln(out io.Writer, args ...any) {
	_, _ = fmt.Fprintln(out, args...)
}

func fprintf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// promptRaw prints the label + default and returns the trimmed user
// input. Returning EOF is propagated unchanged so callers can detect
// end-of-stream cleanly in tests.
func promptRaw(reader *bufio.Reader, out io.Writer, label, defaultVal string) (string, error) {
	fprintf(out, "%s [%s]: ", label, defaultVal)
	raw, err := reader.ReadString('\n')
	if err != nil && (err != io.EOF || raw == "") {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

func promptDate(reader *bufio.Reader, out io.Writer, label, defaultVal string) (string, error) {
	for {
		raw, err := promptRaw(reader, out, label, defaultVal)
		if err != nil {
			return "", err
		}
		if raw == "" {
			raw = defaultVal
		}
		if _, err := time.ParseInLocation("2006-01-02", raw, time.Local); err != nil {
			fprintln(out, "Invalid date. Use YYYY-MM-DD.")
			continue
		}
		return raw, nil
	}
}

func promptClockTime(reader *bufio.Reader, out io.Writer, label, defaultVal string) (string, error) {
	for {
		raw, err := promptRaw(reader, out, label, defaultVal)
		if err != nil {
			return "", err
		}
		if raw == "" {
			raw = defaultVal
		}
		if _, err := time.Parse("15:04", raw); err != nil {
			fprintln(out, "Invalid time. Use HH:MM (24h).")
			continue
		}
		return raw, nil
	}
}

func promptInt(reader *bufio.Reader, out io.Writer, label string, defaultVal int) (int, error) {
	for {
		raw, err := promptRaw(reader, out, label, strconv.Itoa(defaultVal))
		if err != nil {
			return 0, err
		}
		if raw == "" {
			return defaultVal, nil
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			fprintln(out, "Please enter a number.")
			continue
		}
		return parsed, nil
	}
}

func promptVenue(reader *bufio.Reader, out io.Writer, defaultVal int) (int, error) {
	fprintln(out, "Venues (or enter custom venue ID):")
	for i, venue := range venueOptions {
		fprintf(out, "  %d) %s (%d)\n", i+1, venue.Name, venue.ID)
	}
	defaultName := venueNameByID(defaultVal)
	if defaultName == "" {
		defaultName = "Custom"
	}
	fprintf(out, "Default venue: %s (%d)\n", defaultName, defaultVal)
	for {
		raw, err := promptRaw(reader, out, "Venue (list number, venue id, or name)", strconv.Itoa(defaultVal))
		if err != nil {
			return 0, err
		}
		if raw == "" {
			return defaultVal, nil
		}
		if value, err := strconv.Atoi(raw); err == nil {
			if value >= 1 && value <= len(venueOptions) {
				return venueOptions[value-1].ID, nil
			}
			return value, nil
		}
		if id, ok := venueIDByName(raw); ok {
			return id, nil
		}
		fprintln(out, "Invalid venue. Enter a list number, venue id, or name.")
	}
}

func promptResTimes(reader *bufio.Reader, out io.Writer, label, defaultVal string) (string, error) {
	for {
		raw, err := promptRaw(reader, out, label, defaultVal)
		if err != nil {
			return "", err
		}
		if raw == "" {
			raw = defaultVal
		}
		times := parseCommaList(raw)
		if len(times) == 0 {
			fprintln(out, "Provide at least one reservation time.")
			continue
		}
		valid := true
		for _, t := range times {
			if _, err := domain.ParseWallTime(t); err != nil {
				fprintf(out, "Invalid time %q. Use HH:MM or HH:MM:SS.\n", t)
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		return raw, nil
	}
}

func promptTableTypes(reader *bufio.Reader, out io.Writer, label, defaultVal string) (string, error) {
	raw, err := promptRaw(reader, out, label, defaultVal)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultVal
	}
	return raw, nil
}

func venueNameByID(id int) string {
	for _, venue := range venueOptions {
		if venue.ID == id {
			return venue.Name
		}
	}
	return ""
}

func venueIDByName(name string) (int, bool) {
	name = strings.TrimSpace(name)
	for _, venue := range venueOptions {
		if strings.EqualFold(venue.Name, name) {
			return venue.ID, true
		}
	}
	return 0, false
}

// parseCommaList splits and trims; empty fragments are dropped.
func parseCommaList(input string) []string {
	parts := strings.Split(input, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		values = append(values, part)
	}
	return values
}

// parseTableTypesInput accepts the user's -table-types string and
// returns the normalized list. "none" / "nil" (in any case, alone or
// alongside other entries) is dropped per the legacy convention.
func parseTableTypesInput(input string) []string {
	values := parseCommaList(input)
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if strings.EqualFold(value, "none") || strings.EqualFold(value, "nil") {
			continue
		}
		cleaned = append(cleaned, value)
	}
	return cleaned
}

// toIntent assembles a validated domain.Intent from a finalized
// cliOptions. The current instant is taken as a parameter (rather
// than read from the wall clock) so tests can pin it and the package
// stays free of non-deterministic clock reads. The returned Intent's
// Release field is populated according to opts.releaseStrategy /
// opts.retryWindow.
//
// IMPORTANT: this fixes the legacy buildResTimeTypes bug where each
// requested time produced one SlotPreference per requested table type
// AND an extra empty-table-type SlotPreference. Now: N times × M
// table types yields exactly N×M SlotPreferences when M>0; when M==0
// it yields exactly N SlotPreferences with empty TableType.
func toIntent(opts cliOptions, now time.Time) (domain.Intent, error) {
	if opts.partySize < 1 {
		return domain.Intent{}, fmt.Errorf("party-size must be >= 1, got %d", opts.partySize)
	}
	if opts.venueID <= 0 {
		return domain.Intent{}, errors.New("venue-id is required")
	}
	if strings.TrimSpace(opts.resDate) == "" {
		return domain.Intent{}, errors.New("date is required")
	}
	date, err := domain.ParseDate(strings.TrimSpace(opts.resDate))
	if err != nil {
		return domain.Intent{}, err
	}

	rawTimes := parseCommaList(opts.resTimes)
	if len(rawTimes) == 0 {
		return domain.Intent{}, errors.New("res-times: at least one reservation time is required")
	}
	tableTypes := parseTableTypesInput(opts.tableTypes)

	slots := make([]domain.SlotPreference, 0, len(rawTimes)*max(1, len(tableTypes)))
	for _, t := range rawTimes {
		wt, err := domain.ParseWallTime(t)
		if err != nil {
			return domain.Intent{}, fmt.Errorf("res-times: %w", err)
		}
		if len(tableTypes) == 0 {
			slots = append(slots, domain.SlotPreference{Time: wt, TableType: ""})
			continue
		}
		for _, tt := range tableTypes {
			slots = append(slots, domain.SlotPreference{Time: wt, TableType: tt})
		}
	}

	release, err := buildRelease(opts, now)
	if err != nil {
		return domain.Intent{}, err
	}

	return domain.Intent{
		Venue: domain.VenueRef{
			Provider: "resy",
			Ref:      strconv.Itoa(opts.venueID),
		},
		Date:      date,
		PartySize: opts.partySize,
		SlotPrefs: slots,
		Release:   release,
	}, nil
}

// buildRelease maps cliOptions into a concrete ReleaseStrategy. The
// snipe time is interpreted in time.Local (matching the legacy CLI's
// behavior — engine code uses the venue's resolved TZ for its own
// calculations and is unaffected).
func buildRelease(opts cliOptions, now time.Time) (domain.ReleaseStrategy, error) {
	strategy := opts.releaseStrategy
	if !opts.releaseStrategyProvided || strategy == "" {
		if opts.snipeTimeProvided {
			strategy = strategyExplicit
		} else {
			strategy = strategyDiscovered
		}
	}

	at, err := buildScheduledTime(opts.snipeDate, opts.snipeTime)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(strategy) {
	case strategyExplicit:
		return domain.ExplicitRelease{At: at}, nil
	case strategyDiscovered:
		// The probe envelope is centered on the snipe time: start
		// retry-window before, end retry-window after. This gives the
		// engine slack on both sides to catch a release that runs early
		// or late.
		return domain.DiscoveredRelease{
			ProbeFrom:  at.Add(-opts.retryWindow),
			ProbeUntil: at.Add(opts.retryWindow),
		}, nil
	case strategyContinuous:
		// ContinuousRelease starts polling now and gives up after
		// retry-window. The "snipe time" is irrelevant for this
		// variant beyond providing a deadline anchor.
		return domain.ContinuousRelease{Until: now.Add(opts.retryWindow)}, nil
	default:
		return nil, fmt.Errorf("invalid -release-strategy %q (want explicit|discovered|continuous)", opts.releaseStrategy)
	}
}

// buildScheduledTime composes a time.Local wall-clock instant from
// the user's snipe date + snipe time. Format errors are reported with
// the offending field for clearer diagnostics.
func buildScheduledTime(dateInput, timeInput string) (time.Time, error) {
	datePart, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(dateInput), time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid snipe date %q", dateInput)
	}
	timePart, err := time.Parse("15:04", strings.TrimSpace(timeInput))
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid snipe time %q", timeInput)
	}
	return time.Date(
		datePart.Year(), datePart.Month(), datePart.Day(),
		timePart.Hour(), timePart.Minute(), 0, 0,
		time.Local,
	), nil
}
