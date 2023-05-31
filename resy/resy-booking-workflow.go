package resy

import (
    // "os"
    "fmt"
    "log"
    "time"
    "resy-snipe/config"
)

type ResyBookingWorkflow struct {
    resyClient  ResyClient
    resDetails  config.ReservationDetails
}

func NewResyBookingWorkflow(resyClient ResyClient, resDetails config.ReservationDetails) *ResyBookingWorkflow {
    return &ResyBookingWorkflow{
        resyClient: resyClient,
        resDetails: resDetails,
    }
}

func (r *ResyBookingWorkflow) Run(millisToRetry time.Duration) (string, error) {
    return r.runnable(millisToRetry, time.Now().UnixNano() / int64(time.Millisecond))
}

func (r *ResyBookingWorkflow) runnable(millisToRetry time.Duration, dateTimeStart int64) (string, error) {
    log.Println("Taking the shot... ︻デ═一 *")

    // configId, err := r.resyClient.findReservations(r.resDetails.Date, r.resDetails.PartySize, r.resDetails.VenueId, r.resDetails.ResTimeTypes, 2)
    configId, err := r.resyClient.findReservations(r.resDetails.Date, r.resDetails.PartySize, r.resDetails.VenueId, r.resDetails.ResTimeTypes, 2)
    if err != nil {
        return "", err
    }

    // Return a list of configs? 
    // return a list of configs that exist in the desired time list then use go routines to request them all? 
    // Use Go routines to analyze if they exist instead? What is the best way to implement concurrency 
    // Where do I trigger for it to try the second option? 

    var resyToken string
    var resyTokenErr error
    if configId != "" {
        bookingDetails, err := r.resyClient.getReservationDetails(configId, r.resDetails.Date, r.resDetails.PartySize)
        if err != nil {
            fmt.Println(err)
            return "", err
        }
        resyToken, resyTokenErr = r.resyClient.BookReservation(bookingDetails.PaymentMethodID, bookingDetails.BookToken)
    } else {
        resyToken = ""
        resyTokenErr = err.(error)
    }

    if resyTokenErr != nil && millisToRetry > time.Duration(time.Now().UnixNano()/int64(time.Millisecond) - dateTimeStart) * time.Millisecond {
        time.Sleep(millisToRetry * time.Millisecond)
        return r.runnable(millisToRetry, dateTimeStart)
    }

    return resyToken, resyTokenErr
}
