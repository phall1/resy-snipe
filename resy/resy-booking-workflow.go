package resy

import (
	// "os"
	"fmt"
	"log"
	"resy-snipe/config"
	"sync"
	"time"
)

type ResyBookingWorkflow struct {
	resyClient ResyClient
	resDetails config.ReservationDetails
}

func NewResyBookingWorkflow(resyClient ResyClient, resDetails config.ReservationDetails) *ResyBookingWorkflow {
	return &ResyBookingWorkflow{
		resyClient: resyClient,
		resDetails: resDetails,
	}
}

func (r *ResyBookingWorkflow) Run(millisToRetry time.Duration) (string, error) {
	return r.runnable(millisToRetry, time.Now().UnixNano()/int64(time.Millisecond))
}

func (r *ResyBookingWorkflow) runnable(millisToRetry time.Duration, dateTimeStart int64) (string, error) {
	log.Println("Taking the shot... ︻デ═一 *")

	resQueue, err := r.resyClient.findReservations(r.resDetails.Date, r.resDetails.PartySize, r.resDetails.VenueId, r.resDetails.ResTimeTypes, 2)
	if err != nil {
		return "", err
	}

	var resyToken string
	var resyTokenErr error

	var wg sync.WaitGroup
	wg.Add(resQueue.Len())
	for resQueue.Len() > 0 {
		front := resQueue.Front()
		configId := front.Value.(string)
		go func(str string) {
			defer wg.Done()
			resyToken, resyTokenErr = r.snipeConfigId(configId, err)
		}(configId)
		resQueue.Remove(front)
	}

	wg.Wait()

	return resyToken, resyTokenErr
}

func (r *ResyBookingWorkflow) snipeConfigId(configId string, err error) (string, error) {
	var resyToken string
	var resyTokenErr error
	if configId != "" {
		bookingDetails, err := r.resyClient.getReservationDetails(configId, r.resDetails.Date, r.resDetails.PartySize)
		if err != nil {
			fmt.Printf("getReservationDetails error: %s", err)
			return "", err
		}
		resyToken, resyTokenErr = r.resyClient.BookReservation(bookingDetails.PaymentMethodID, bookingDetails.BookToken)
	} else {
		resyToken = ""
		resyTokenErr = err.(error)
	}

	return resyToken, resyTokenErr
}
