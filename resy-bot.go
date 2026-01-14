package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"resy-snipe/config"
	"resy-snipe/resy"
	"strconv"
	"strings"
	"time"
)

type cliOptions struct {
	interactive bool
	resDate     string
	partySize   int
	venueID     int
	resTimes    string
	tableTypes  string
	snipeDate   string
	snipeTime   string
}

type venueOption struct {
	Name string
	ID   int
}

var venueOptions = []venueOption{
	{Name: "Dead Rabbit", ID: config.DeadRabbit},
	{Name: "Rubirosa", ID: config.Rubirosa},
	{Name: "Red Pearl", ID: config.RedPearl},
	{Name: "Rafs", ID: config.Rafs},
	{Name: "Carbone", ID: config.Carbone},
	{Name: "Don Angie", ID: config.DonAngie},
	{Name: "San Sabino", ID: config.SanSabino},
	{Name: "Gertrudes", ID: config.Gertrudes},
	{Name: "Au Cheval", ID: config.AuCheval},
	{Name: "HOWOO", ID: config.HOWOO},
}

func main() {
	opts := parseFlags()

	resDate := config.ReservationDetailss.Date
	partySize := config.ReservationDetailss.PartySize
	venueID := config.ReservationDetailss.VenueId
	defaultResTimes, defaultTableTypes := resTimeDefaults(config.ReservationDetailss.ResTimeTypes)
	resTimesInput := strings.Join(defaultResTimes, ",")
	tableTypesInput := strings.Join(defaultTableTypes, ",")

	snipeDate := resDate
	snipeTimeInput := fmt.Sprintf("%02d:%02d", config.SnipeTimee.Hours, config.SnipeTimee.Minutes)

	if opts.resDate != "" {
		resDate = opts.resDate
	}
	if opts.partySize >= 0 {
		partySize = opts.partySize
	}
	if opts.venueID >= 0 {
		venueID = opts.venueID
	}
	if opts.resTimes != "" {
		resTimesInput = opts.resTimes
	}
	if opts.tableTypes != "" {
		tableTypesInput = opts.tableTypes
	}
	if opts.snipeDate != "" {
		snipeDate = opts.snipeDate
	} else {
		snipeDate = resDate
	}
	if opts.snipeTime != "" {
		snipeTimeInput = opts.snipeTime
	}

	if opts.interactive {
		reader := bufio.NewReader(os.Stdin)
		var err error

		resDate, err = promptDate(reader, "Reservation date (YYYY-MM-DD)", resDate)
		if err != nil {
			exitWithError(err)
		}
		partySize, err = promptInt(reader, "Party size", partySize)
		if err != nil {
			exitWithError(err)
		}
		venueID, err = promptVenue(reader, venueID)
		if err != nil {
			exitWithError(err)
		}

		defaultResTimesInput := resTimesInput
		resTimesInput, err = promptResTimes(reader, "Reservation times (comma-separated, HH:MM)", defaultResTimesInput)
		if err != nil {
			exitWithError(err)
		}
		tableTypesInput, err = promptTableTypes(reader, "Table types (comma-separated, optional, or 'none')", tableTypesInput)
		if err != nil {
			exitWithError(err)
		}

		if opts.snipeDate == "" {
			snipeDate = resDate
		}
		snipeDate, err = promptDate(reader, "Snipe date (YYYY-MM-DD)", snipeDate)
		if err != nil {
			exitWithError(err)
		}
		snipeTimeInput, err = promptClockTime(reader, "Snipe time (HH:MM)", snipeTimeInput)
		if err != nil {
			exitWithError(err)
		}
	}

	resTimes := parseCommaList(resTimesInput)
	tableTypes := parseTableTypesInput(tableTypesInput)
	resTimeTypes, err := buildResTimeTypes(resTimes, tableTypes)
	if err != nil {
		exitWithError(err)
	}

	scheduledTime, err := buildScheduledTime(snipeDate, snipeTimeInput)
	if err != nil {
		exitWithError(err)
	}

	duration := scheduledTime.Sub(time.Now())
	snipeTime := config.SnipeTime{Hours: scheduledTime.Hour(), Minutes: scheduledTime.Minute()}
	reservationDetails := config.ReservationDetails{
		Date:         resDate,
		PartySize:    partySize,
		VenueId:      venueID,
		ResTimeTypes: resTimeTypes,
	}

	resyApi := resy.NewResyAPI(config.ResyKeyss)
	resyClient := resy.NewResyClient(*resyApi)
	resyBookingWorkflow := resy.NewResyBookingWorkflow(*resyClient, reservationDetails)

	fmt.Printf("Sleeping for %v until %v:%v local time.\n", duration, snipeTime.Hours, snipeTime.Minutes)

	// Wait until the scheduled time
	time.Sleep(duration)

	resyBookingWorkflow.Run(1000)

	// Execute the program
	fmt.Println("Program executed at", scheduledTime)
}

func parseFlags() cliOptions {
	var opts cliOptions
	flag.BoolVar(&opts.interactive, "interactive", false, "Prompt for settings interactively.")
	flag.StringVar(&opts.resDate, "date", "", "Reservation date (YYYY-MM-DD).")
	flag.IntVar(&opts.partySize, "party-size", -1, "Party size.")
	flag.IntVar(&opts.venueID, "venue-id", -1, "Venue ID.")
	flag.StringVar(&opts.resTimes, "res-times", "", "Reservation times (comma-separated, HH:MM or HH:MM:SS).")
	flag.StringVar(&opts.tableTypes, "table-types", "", "Table types (comma-separated). Use 'none' for any.")
	flag.StringVar(&opts.snipeDate, "snipe-date", "", "Snipe date (YYYY-MM-DD). Defaults to reservation date.")
	flag.StringVar(&opts.snipeTime, "snipe-time", "", "Snipe time (HH:MM). Defaults to midnight.")
	flag.Parse()
	return opts
}

func resTimeDefaults(resTimeTypes []config.ReservationTimeType) ([]string, []string) {
	var times []string
	var tableTypes []string
	timeSeen := make(map[string]bool)
	tableSeen := make(map[string]bool)
	for _, r := range resTimeTypes {
		if !timeSeen[r.ReservationTime] {
			timeSeen[r.ReservationTime] = true
			times = append(times, r.ReservationTime)
		}
		if r.TableType != nil && !tableSeen[*r.TableType] {
			tableSeen[*r.TableType] = true
			tableTypes = append(tableTypes, *r.TableType)
		}
	}
	return times, tableTypes
}

func promptDate(reader *bufio.Reader, label string, defaultVal string) (string, error) {
	for {
		raw, err := promptRaw(reader, label, defaultVal)
		if err != nil {
			return "", err
		}
		if raw == "" {
			raw = defaultVal
		}
		if _, err := time.ParseInLocation("2006-01-02", raw, time.Local); err != nil {
			fmt.Println("Invalid date. Use YYYY-MM-DD.")
			continue
		}
		return raw, nil
	}
}

func promptClockTime(reader *bufio.Reader, label string, defaultVal string) (string, error) {
	for {
		raw, err := promptRaw(reader, label, defaultVal)
		if err != nil {
			return "", err
		}
		if raw == "" {
			raw = defaultVal
		}
		if _, err := time.Parse("15:04", raw); err != nil {
			fmt.Println("Invalid time. Use HH:MM (24h).")
			continue
		}
		return raw, nil
	}
}

func promptInt(reader *bufio.Reader, label string, defaultVal int) (int, error) {
	for {
		raw, err := promptRaw(reader, label, strconv.Itoa(defaultVal))
		if err != nil {
			return 0, err
		}
		if raw == "" {
			return defaultVal, nil
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Println("Please enter a number.")
			continue
		}
		return parsed, nil
	}
}

func promptVenue(reader *bufio.Reader, defaultVal int) (int, error) {
	fmt.Println("Venues (or enter custom venue ID):")
	for i, venue := range venueOptions {
		fmt.Printf("  %d) %s (%d)\n", i+1, venue.Name, venue.ID)
	}
	defaultName := venueNameByID(defaultVal)
	if defaultName == "" {
		defaultName = "Custom"
	}
	fmt.Printf("Default venue: %s (%d)\n", defaultName, defaultVal)
	for {
		raw, err := promptRaw(reader, "Venue (list number, venue id, or name)", strconv.Itoa(defaultVal))
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
		if venueID, ok := venueIDByName(raw); ok {
			return venueID, nil
		}
		fmt.Println("Invalid venue. Enter a list number, venue id, or name.")
	}
}

func promptResTimes(reader *bufio.Reader, label string, defaultVal string) (string, error) {
	for {
		raw, err := promptRaw(reader, label, defaultVal)
		if err != nil {
			return "", err
		}
		if raw == "" {
			raw = defaultVal
		}
		times := parseCommaList(raw)
		if len(times) == 0 {
			fmt.Println("Provide at least one reservation time.")
			continue
		}
		valid := true
		for _, t := range times {
			if _, err := normalizeResTime(t); err != nil {
				fmt.Printf("Invalid time %q. Use HH:MM or HH:MM:SS.\n", t)
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

func promptTableTypes(reader *bufio.Reader, label string, defaultVal string) (string, error) {
	raw, err := promptRaw(reader, label, defaultVal)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultVal
	}
	return raw, nil
}

func promptRaw(reader *bufio.Reader, label string, defaultVal string) (string, error) {
	fmt.Printf("%s [%s]: ", label, defaultVal)
	raw, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
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
	name = strings.TrimSpace(strings.ToLower(name))
	for _, venue := range venueOptions {
		if strings.ToLower(venue.Name) == name {
			return venue.ID, true
		}
	}
	return 0, false
}

func parseCommaList(input string) []string {
	parts := strings.Split(input, ",")
	var values []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		values = append(values, part)
	}
	return values
}

func parseTableTypesInput(input string) []string {
	values := parseCommaList(input)
	if len(values) == 1 && (strings.EqualFold(values[0], "none") || strings.EqualFold(values[0], "nil")) {
		return nil
	}
	var cleaned []string
	for _, value := range values {
		if strings.EqualFold(value, "none") || strings.EqualFold(value, "nil") {
			continue
		}
		cleaned = append(cleaned, value)
	}
	return cleaned
}

func normalizeResTime(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("empty reservation time")
	}
	if parsed, err := time.Parse("15:04:05", input); err == nil {
		return parsed.Format("15:04:05"), nil
	}
	parsed, err := time.Parse("15:04", input)
	if err != nil {
		return "", err
	}
	return parsed.Format("15:04:05"), nil
}

func buildResTimeTypes(times []string, tableTypes []string) ([]config.ReservationTimeType, error) {
	if len(times) == 0 {
		return nil, fmt.Errorf("at least one reservation time is required")
	}
	var resTimeTypes []config.ReservationTimeType
	for _, timeValue := range times {
		normalized, err := normalizeResTime(timeValue)
		if err != nil {
			return nil, fmt.Errorf("invalid reservation time %q", timeValue)
		}
		if len(tableTypes) == 0 {
			resTimeTypes = append(resTimeTypes, config.NewReservationTimeType(normalized, nil))
			continue
		}
		for _, tableType := range tableTypes {
			tableType = strings.TrimSpace(tableType)
			if tableType == "" {
				continue
			}
			table := tableType
			resTimeTypes = append(resTimeTypes, config.NewReservationTimeType(normalized, &table))
		}
		resTimeTypes = append(resTimeTypes, config.NewReservationTimeType(normalized, nil))
	}
	return resTimeTypes, nil
}

func buildScheduledTime(dateInput string, timeInput string) (time.Time, error) {
	datePart, err := time.ParseInLocation("2006-01-02", dateInput, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid snipe date %q", dateInput)
	}
	timePart, err := time.Parse("15:04", timeInput)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid snipe time %q", timeInput)
	}
	return time.Date(datePart.Year(), datePart.Month(), datePart.Day(), timePart.Hour(), timePart.Minute(), 0, 0, time.Local), nil
}

func exitWithError(err error) {
	fmt.Println("Error:", err)
	os.Exit(1)
}
