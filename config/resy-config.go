package config

import "os"

type ResyKeys struct {
	ApiKey    string
	AuthToken string
}

type ReservationTimeType struct {
	ReservationTime string
	TableType       *string
}

type ReservationDetails struct {
	Date         string
	PartySize    int
	VenueId      int
	ResTimeTypes []ReservationTimeType
}

type SnipeTime struct {
	Hours   int
	Minutes int
}

func NewReservationTimeType(reservationTime string, tableType *string) ReservationTimeType {
	return ReservationTimeType{ReservationTime: reservationTime, TableType: tableType}
}

var ResyKeyss = ResyKeys{ApiKey: os.Getenv("RESY_API_KEY"), AuthToken: os.Getenv("RESY_AUTH_TOKEN")}

var SnipeTimee = SnipeTime{Hours: 14, Minutes: 05}

var tableType = "Dining Room"

// var tableType = "Taproom Table"
// var tableType = "High Top"
// var tableType = "TAP EXT TABLE"
// var tableType = "Parlor"

var ResTimeTypes = []ReservationTimeType{
	NewReservationTimeType("18:30:00", nil),
}

// Venue IDs (Resy)
const (
	DeadRabbit int = 38660
	Rubirosa   int = 466
	RedPearl   int = 69820
	Rafs       int = 65679
	Carbone    int = 6194
	DonAngie   int = 1505
	SanSabino  int = 78799
	Gertrudes  int = 71935
	AuCheval   int = 5769
	HOWOO      int = 86696
)

var ReservationDetailss = ReservationDetails{
	Date:         "2026-01-06",
	PartySize:    2,
	VenueId:      HOWOO,
	ResTimeTypes: ResTimeTypes,
}
