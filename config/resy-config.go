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
var SnipeTimee = SnipeTime{Hours: 8, Minutes: 5}
var tableType = "Dining Room"
var ResTimeTypes = []ReservationTimeType{
    NewReservationTimeType("11:00:00", nil),
    NewReservationTimeType("18:30:00", &tableType),
}
var ReservationDetailss = ReservationDetails{
    Date:         "2023-06-01",
    PartySize:    2,
    VenueId:      466,
    ResTimeTypes: ResTimeTypes,
}

