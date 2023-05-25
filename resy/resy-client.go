package resy

import (
    "os"
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

    fmt.Println("GetReservations")
    resp, err := rc.resyApi.GetReservations(date, partySize, venueId)
    if err != nil {
        return "", err
    }

    // fmt.Println("resp: ")
    // fmt.Println(resp)
    fmt.Printf("resp: %T\n", resp)

    // Convert the response into the reservations table?
    var reservations = make(ReservationMap)
    
    // reservations = buildReservationMap(resp)

    var extractedValues map[string]interface{}

    respBytes := []byte(resp)
    err = json.Unmarshal(respBytes, &extractedValues)
    fmt.Println("err")
    fmt.Println(err)
    if err != nil {
        return "", err
    }

    // fmt.Println("extractedValues")
    // fmt.Println(extractedValues)

    // Accessing values
    query := extractedValues["query"].(map[string]interface{})
    day := query["day"].(string)
    results := extractedValues["results"].(map[string]interface{})
    venues := results["venues"].([]interface{})
    
    // Print the extracted values
    fmt.Println("Day:", day)
    fmt.Println("Party Size:", partySize)

    fmt.Println("Venues:")
    for _, v := range venues {
        venue := v.(map[string]interface{})
        slots := venue["slots"].([]interface{})
        for _, s := range slots {
            slot := s.(map[string]interface{})
            config := slot["config"].(map[string]interface{})
            start_date := slot["date"].(map[string]interface{})
            start := strings.Split(start_date["start"].(string), " ")[1]
            table_type := config["type"].(string)
            config_id := config["token"].(string)

            // fmt.Println(" - start:", start)
            // fmt.Println(" - table_type:", table_type)
            // fmt.Println(" - token:", config_id)
            
            if _, ok := reservations[start]; !ok {
                reservations[start] = TableTypeMap {
                    table_type: config_id,
                }
            } else {
                reservations[start][table_type] = config_id
            }
        }
    }

    // fmt.Println("reservations")
    // fmt.Println(reservations)

    for _, r := range resTimeTypes {
        // fmt.Println("r.ReservationTime")
        // fmt.Println(r.ReservationTime)
        if r.TableType != nil {
            fmt.Println("*r.TableType")
            fmt.Println(*r.TableType)
        }
        fmt.Println("r.ReservationTime")
        fmt.Println(r.ReservationTime)
        
        tableTypeMap, ok := reservations[r.ReservationTime]
        // fmt.Println("ok")
        // fmt.Println(ok)
        if !ok {
            continue
        }

        fmt.Println("TableTypeMap")
        fmt.Println(tableTypeMap)

        

        // tableId, ok := tableTypeMap[*r.TableType]
        // if !ok {
        //     continue
        // }

        // Should be checking if the reservation is open here
        // If tabeID then we should be able to client.getResDetails? 
        // Get Details then client.bookReservation

        // Find ReservationTime(reservations, resTimeTypes)

        // configId, err := rc.resyApi.PostReservation(date, partySize, tableId, venueId)
        // if err == nil {
        //     return configId, nil
        // }
    }

    os.Exit(24)

    if time.Now().UnixNano()/int64(time.Millisecond)-start >= millisToRetry {
        return "", errors.New(fmt.Sprintf("couldNotFindResMsgFmt", date, partySize))
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
    bookToken := resDetails["book_token"].(map[string]interface{})["value"].(string)[1:len(resDetails["book_token"].(map[string]interface{})["value"].(string))-1]


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
    respBytes := []byte(resp)
    if err := json.Unmarshal(respBytes, &resyToken); err != nil {
        return "", err
    }

    fmt.Println("Headshot!")
    fmt.Println("(҂‾ ▵‾)︻デ═一 (× _ ×#")
    fmt.Println("Successfully sniped reservation")
    fmt.Println("Resy token is %s", resyToken)

    return resyToken, nil
}
