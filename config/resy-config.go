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

var ResyKeyss = ResyKeys{ApiKey: os.Getenv("RESY_API_KEY"), AuthToken: os.Getenv("RESY_API_KEY")}
var SnipeTimee = SnipeTime{Hours: 0, Minutes: 0}
// var tableType = "Dining Room"
// var tableType = "Taproom Table"
var ResTimeTypes = []ReservationTimeType{
    NewReservationTimeType("19:00:00", nil),
    NewReservationTimeType("19:15:00", nil),
    NewReservationTimeType("18:45:00", nil),
    NewReservationTimeType("18:30:00", nil),
    NewReservationTimeType("18:15:00", nil),
    NewReservationTimeType("18:00:00", nil),
    NewReservationTimeType("19:45:00", nil),
    // NewReservationTimeType("12:15:00", &tableType),
    // NewReservationTimeType("16:00:00", nil),
    // NewReservationTimeType("12:30:00", nil),
    // NewReservationTimeType("19:15:00", nil),
    // NewReservationTimeType("19:30:00", nil),
    // NewReservationTimeType("18:30:00", nil),
    // NewReservationTimeType("18:30:00", &tableType),
}

// DeadRabbit: 38660
// Rubirosa: 466
// Red Pearl: 69820
var ReservationDetailss = ReservationDetails{
    Date:         "2023-06-30",
    PartySize:    2,
    VenueId:      466,
    ResTimeTypes: ResTimeTypes,
}