package resy

import (
    // "os"
    "fmt"
    "time"
    "errors"
    "strconv"
    "strings"    
    "encoding/json"
    "resy-snipe/config"
)

const retryIntervalMs = 10

type ReservationMap map[string]TableTypeMap
type TableTypeMap map[string]string

type ResyClient struct {
    resyApi ResyAPI
}

type BookingDetails struct {
	PaymentMethodID string
	BookToken       string
}

// Define a struct that matches the structure of the JSON data
type Response struct {
    Results struct {
        Venues []struct {
            Slots []struct {
                Config   struct {
                    Type  string `json:"type"`
                    Token string `json:"token"`
                } `json:"config"`
                Date struct {
                    Start string `json:"start"`
                } `json:"date"`
            } `json:"slots"`
        } `json:"venues"`
    } `json:"results"`
}

func NewResyClient(resyApi ResyAPI) *ResyClient {
    return &ResyClient{resyApi: resyApi}
}

func (rc *ResyClient) findReservations(date string, partySize int, venueId int, resTimeTypes []config.ReservationTimeType, millisToRetry int64) (string, error) {
    return rc.retryFindReservations(date, partySize, venueId, resTimeTypes, millisToRetry, time.Now().UnixNano()/int64(time.Millisecond))
}

func (rc *ResyClient) retryFindReservations(date string, partySize int, venueId int, resTimeTypes []config.ReservationTimeType, millisToRetry int64, start int64) (string, error) {
    if len(resTimeTypes) == 0 {
        return "", errors.New("noReservationTimeTypesMsg")
    }

    resp, err := rc.resyApi.GetReservations(date, partySize, venueId)
    if err != nil {
        return "", err
    }

    // Create variables to store collected reservation information
    var reservations = make(ReservationMap)
    // var extractedValues map[string]interface{}

    // Unmarshal the JSON into the structured Go type
    var extractedValues Response
    err = json.Unmarshal([]byte(resp), &extractedValues)
    if err != nil {
        return "", err
    }

    // Access the extracted values
    for _, venue := range extractedValues.Results.Venues {
        for _, slot := range venue.Slots {
            start := strings.Split(slot.Date.Start, " ")[1]
            tableType := slot.Config.Type
            configID := slot.Config.Token

            if _, ok := reservations[start]; !ok {
                reservations[start] = TableTypeMap{
                    tableType: configID,
                }
            } else {
                reservations[start][tableType] = configID
            }
        }
    }
    // Now we have the reservation map
    
    // Now find a matching reservation
    for _, r := range resTimeTypes {
        tableTypeMap, ok := reservations[r.ReservationTime]
        // fmt.Println(ok)
        if !ok {
            continue
        }

        var firstKey string
        for key, _ := range tableTypeMap {
            firstKey = key
            break
        }

        if r.TableType != nil {
            // fmt.Println(*r.TableType)
            return tableTypeMap[*r.TableType], nil
        } else {
            if ok {
                return tableTypeMap[firstKey], nil
            }
        }
    }

    fmt.Println("No Hits")

    if time.Now().UnixNano()/int64(time.Millisecond)-start >= millisToRetry {
        return "", errors.New(fmt.Sprintf("couldNotFindResMsgFmt %s %d", date, partySize))
    }

    time.Sleep(time.Duration(retryIntervalMs) * time.Millisecond)
    return rc.retryFindReservations(date, partySize, venueId, resTimeTypes, millisToRetry, start)
}

// Get details of the reservation
// configId: Unique identifier for the reservation
// date: Date of the reservation in YYYY-MM-DD format
// partySize: Size of the party reservation
// returns the paymentMethodId and the bookingToken of the reservation
func (rc *ResyClient) getReservationDetails(configId string, date string, partySize int) (*BookingDetails, error) {
    resp, err := rc.resyApi.GetReservationDetails(configId, date, partySize)
    if err != nil {
        return nil, err
    }

    var resDetails map[string]interface{}
    respBytes := []byte(resp)
    if err := json.Unmarshal(respBytes, &resDetails); err != nil {
        return nil, err
    }

    // Searching this JSON structure...
    // {"user": {"payment_methods": [{"id": 42, ...}]}}
    paymentMethod := resDetails["user"].(map[string]interface{})["payment_methods"].([]interface{})[0].(map[string]interface{})
    paymentMethodId := strconv.Itoa(int(paymentMethod["id"].(float64)))

    // Searching this JSON structure...
    // {"book_token": {"value": "BOOK_TOKEN", ...}}
    bookToken := resDetails["book_token"].(map[string]interface{})["value"].(string)[0:len(resDetails["book_token"].(map[string]interface{})["value"].(string))]

    return &BookingDetails{
        PaymentMethodID: paymentMethodId,
        BookToken:       bookToken,
    }, nil
}

// BookReservation books the reservation
// paymentMethodId: unique identifier of the payment id in case of a late cancellation fee
// bookToken: unique identifier of the reservation in question
// returns: unique identifier of the confirmed booking
func (rc *ResyClient) BookReservation(paymentMethodID string, bookToken string) (string, error) {
    resp, err := rc.resyApi.PostReservation(paymentMethodID, bookToken)
    if err != nil {
        return "", err
    }

    var resyToken string
    resyToken = resp
    fmt.Println(resyToken)
    // respBytes := []byte(resp)
    // if err := json.Unmarshal(respBytes, &resyToken); err != nil {
    //     fmt.Println("err")
    //     fmt.Println(err)
    //     return "", err
    // }
    fmt.Println("Headshot!")
    fmt.Println("(҂‾ ▵‾)︻デ═一 (× _ ×#")
    fmt.Println("Successfully sniped reservation")

    return resyToken, nil
}
