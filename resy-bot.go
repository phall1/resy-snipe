package main

import (
    "fmt"
    "time"
	"resy-snipe/resy"
	"resy-snipe/config"
)

func main() {
    // Set the time when you want the program to execute
    scheduledTime := time.Date(2023, time.May, 25, config.SnipeTimee.Hours, config.SnipeTimee.Minutes, 0, 0, time.Local)

    // Calculate the duration until the scheduled time
    duration := scheduledTime.Sub(time.Now())

	resyKeys, snipeTime, reservationDetails := config.ResyKeyss, config.SnipeTimee, config.ReservationDetailss

	resyApi := resy.NewResyAPI(resyKeys)
	resyClient := resy.NewResyClient(*resyApi)
	resyBookingWorkflow := resy.NewResyBookingWorkflow(*resyClient, reservationDetails)

	fmt.Println("Workflow Configured")
	fmt.Println(snipeTime)
	fmt.Println("Sleeping for ", duration)

    // Wait until the scheduled time
    time.Sleep(duration)

	resyBookingWorkflow.Run(100000)

    // Execute the program
    fmt.Println("Program executed at", scheduledTime)
}
