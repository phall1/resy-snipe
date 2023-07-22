package config

type ResyKeys struct {
    ApiKey    string
    AuthToken string
}

type ReservationTimeType struct {
    ReservationTime string
    TableType       *string
}

type ReservationDetails struct {
    Date          string
    PartySize     int
    VenueId       int
    ResTimeTypes  []ReservationTimeType
}

type SnipeTime struct {
    Hours   int
    Minutes int
}

func NewReservationTimeType(reservationTime string, tableType *string) ReservationTimeType {
    return ReservationTimeType{ReservationTime: reservationTime, TableType: tableType}
}

RESY_API_KEY
Define ENV vars for RESY Credentials
// var ResyKeyss = ResyKeys{ApiKey: os.Getenv("RESY_API_KEY"), AuthToken: os.Getenv("RESY_API_KEY")}
var ResyKeyss = ResyKeys{ApiKey: os.Getenv("RESY_API_KEY"), AuthToken: os.Getenv("RESY_API_KEY")}
var SnipeTimee = SnipeTime{Hours: 0, Minutes: 0}
// var tableType = "Dining Room"
// var tableType = "Taproom Table"
var ResTimeTypes = []ReservationTimeType{
    NewReservationTimeType("19:00:00", nil),
    NewReservationTimeType("18:45:00", nil),
    NewReservationTimeType("19:15:00", nil),
    NewReservationTimeType("18:30:00", nil),
    NewReservationTimeType("18:15:00", nil),
    NewReservationTimeType("18:00:00", nil),
    NewReservationTimeType("19:45:00", nil),
    NewReservationTimeType("17:45:00", nil),
}

// DeadRabbit: 38660
// Rubirosa: 466
// Red Pearl: 69820
// Raf's: 65679
var ReservationDetailss = ReservationDetails{
    Date:         "2023-07-22",
    PartySize:    4,
    VenueId:      466,
    ResTimeTypes: ResTimeTypes,
}