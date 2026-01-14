# resy-snipe

A Go CLI that checks Resy for availability at a target venue/date/party size, then attempts to book matching time + seating/table-type options at a scheduled "snipe" time. It supports both flag-driven usage and an interactive prompt mode.

Important note (ethics / ToS): This tool automates interactions with Resy endpoints and may violate Resy's terms, rate limits, or acceptable-use policies. Use responsibly, keep request volume low, and only for accounts/venues you're authorized to book.

## Features
- CLI flags for date, party size, venue ID, desired reservation times, seating/table types, and the snipe schedule.
- Interactive mode (`-interactive`) that prompts you through all inputs (with defaults).
- Time + table-type matching: provide one or more reservation times and (optionally) one or more seating/table types; the tool builds a list of candidate combinations.
- Reservation discovery loop with a short retry interval (10ms between attempts).
- Concurrent booking attempts: if multiple candidate config tokens are found, the workflow tries booking them concurrently.

## Requirements
- Go 1.20+
- A Resy account with valid tokens:
  - `RESY_API_KEY`
  - `RESY_AUTH_TOKEN`

## Setup
1) Export credentials:
```bash
export RESY_API_KEY="your_api_key"
export RESY_AUTH_TOKEN="your_auth_token"
```

2) Run from the repo root:
```bash
go run .
```

If env vars are missing/empty, requests will fail because the API client reads keys directly from env vars at startup.

## Default behavior
If you run with no flags:
- Reservation date defaults to 7 days from now in America/New_York.
- Party size defaults to 2.
- Venue defaults to Dead Rabbit.
- Reservation times default to whatever is in `ResTimeTypes` (currently `18:45:00`).
- Snipe time defaults to `00:00` (midnight).

## CLI usage
### Flags
- `-interactive` Prompt for settings interactively.
- `-date` Reservation date (YYYY-MM-DD).
- `-party-size` Party size.
- `-venue-id` Resy venue ID.
- `-res-times` One or more desired times, comma-separated. Times are normalized to `HH:MM:SS`.
- `-table-types` Optional seating/table types, comma-separated. Use `none` for any.
- `-snipe-date` Date to perform the snipe (defaults to reservation date).
- `-snipe-time` Time to perform the snipe (24h clock).

### Examples
Run at midnight tonight (local time), booking 7 days out (default date), Dead Rabbit, party of 2:
```bash
go run . -snipe-time 00:00
```

Book a specific venue and date at 09:00, try multiple times:
```bash
go run . \
  -date 2026-01-22 \
  -party-size 2 \
  -venue-id 466 \
  -res-times "18:30,18:45,19:00" \
  -table-types "Parlor,Bar" \
  -snipe-date 2026-01-15 \
  -snipe-time 09:00
```

Interactive prompt mode:
```bash
go run . -interactive
```

## Venue IDs
The CLI prints a venue menu in interactive mode based on constants in config (you can still enter a custom venue ID).

Current built-ins:
- Dead Rabbit (`38660`)
- Rubirosa (`466`)
- Red Pearl (`69820`)
- Rafs (`65679`)
- Carbone (`6194`)
- Don Angie (`1505`)
- San Sabino (`78799`)
- Gertrudes (`71935`)
- Au Cheval (`5769`)
- HOWOO (`86696`)

To add more, extend the constants in `config/resy-config.go` and the `venueOptions` list in `resy-bot.go`.

## Project layout
- `resy-bot.go`: main CLI entrypoint (flags, interactive prompts, schedule, run workflow).
- `config/resy-config.go`: environment-based auth keys, venue constants, default reservation details.
- `resy/resy-api.go`: low-level HTTP requests to Resy endpoints (`/find`, `/details`, `/book`) plus headers.
- `resy/resy-client.go`: parses find results into candidate booking config tokens, fetches booking details, books reservations.
- `resy/resy-booking-workflow.go`: orchestration: find -> details -> book.

## How it works
1) Scheduling
- The app computes a target `scheduledTime` from `-snipe-date` + `-snipe-time`, prints the sleep duration, and sleeps until that time.
- If the scheduled time is in the past, the computed duration will be negative and the sleep will effectively be skipped.

2) "Find" availability
- At runtime, the workflow calls Resy "find" (`/4/find`) with date, party size, and venue.
- The response is parsed into a structure mapping `start time -> table type -> config token`.
- The client compares available slots against your requested reservation time/type list, building a queue of matching config tokens.

3) "Details" and "Book"
- For each candidate config token:
  - Call `/3/details` to fetch `payment_method_id` and `book_token`.
  - Call `/3/book` with `book_token` and the payment method payload.

4) Concurrency model
- If multiple config tokens match, the workflow spins a goroutine per token and attempts booking concurrently.

## Configuration
Edit `config/resy-config.go` to change defaults:
- Default reservation date/time options: `ResTimeTypes`
- Default venue / party size / date: `ReservationDetailss`
- Default snipe time: `SnipeTimee`

## Retry tuning (important)
There are two key retry-related behaviors in the current code:
- The retry loop sleeps 10ms between attempts.
- The retry window passed to `findReservations` is currently hard-coded to `2` inside the booking workflow (not the `Run(millisToRetry ...)` parameter).

Because the retry stop condition is measured in milliseconds, a value of `2` means "retry for ~2ms total," which is effectively one quick attempt.

Recommended improvement: thread the intended retry duration through the workflow and pass a sensible window (for example, 30-120 seconds), and/or make it configurable via a CLI flag.

## Troubleshooting
### "No Hits"
The tool prints `No Hits` when it finds no matching slots for your requested time(s)/table type(s).

Common causes:
- Too narrow reservation times (try a wider range).
- Table types don't match the venue's returned types.
- Retry window is too short (see Retry tuning above).

### HTTP errors (401/403/429)
- 401/403 typically indicates invalid/expired tokens or missing env vars. Tokens are read from `RESY_API_KEY` and `RESY_AUTH_TOKEN`.
- 429 indicates rate limiting. Reduce request rate and widen your retry interval.

When non-OK responses occur, the code reads and prints response body details and returns an error.

### Negative "Sleeping for ..."
If your `-snipe-date`/`-snipe-time` is in the past, the computed duration will be negative. The tool will proceed immediately because `time.Sleep(duration)` effectively does not delay.

## Known limitations
- Data race on booking result: concurrent goroutines write `resyToken` / `resyTokenErr` without synchronization; "last writer wins." Consider guarding with a mutex or returning the first successful booking.
- Retry duration is not wired correctly: `Run(millisToRetry)` doesn't currently control the internal find retry window (hard-coded to 2).
- Assumes at least one payment method: `getReservationDetails` uses the first payment method in the array without checking length.

## Security tips
- Never commit your `RESY_API_KEY` / `RESY_AUTH_TOKEN`. The app is already designed to read them from environment variables.
- Consider using a local `.env` file + a loader (dotenv) for convenience (but keep it out of git).

## Development ideas
- Add a `-retry-ms` / `-retry-seconds` flag and plumb it correctly through `ResyBookingWorkflow` -> `ResyClient`.
- Add structured logging (request IDs, status codes, elapsed time).
- Add a "dry run" mode: find + details without booking.
- Add backoff and jitter to respect rate limits.
- Add deterministic selection (prefer earliest time, prefer specific table type, stop after first success).
