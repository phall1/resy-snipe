package resy

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"resy-snipe/config"
)

// ResyKeys holds the Resy API credentials
// type ResyKeys struct {
// 	ApiKey     string
// 	AuthToken string
// }

// ResyAPI stores the Resy API token
type ResyAPI struct {
	resyToken config.ResyKeys
}

// NewResyAPI returns a new instance of ResyAPI
func NewResyAPI(resyToken config.ResyKeys) *ResyAPI {
	return &ResyAPI{resyToken: resyToken}
}

// GetReservations finds available reservations
func (api *ResyAPI) GetReservations(date string, partySize int, venueID int) (string, error) {
	queryParams := url.Values{
		"lat":        {"0"},
		"long":       {"0"},
		"day":        {date},
		"party_size": {fmt.Sprintf("%d", partySize)},
		"venue_id":   {fmt.Sprintf("%d", venueID)},
	}

	return sendGetRequest(api.resyToken, "api.resy.com/4/find", queryParams)
}

// GetReservationDetails gets details of the reservation
func (api *ResyAPI) GetReservationDetails(configID string, date string, partySize int) (string, error) {
	queryParams := url.Values{
		"config_id":  {configID},
		"day":        {date},
		"party_size": {fmt.Sprintf("%d", partySize)},
	}

	return sendGetRequest(api.resyToken, "api.resy.com/3/details", queryParams)
}

// PostReservation books the reservation
func (api *ResyAPI) PostReservation(paymentMethodID string, bookToken string) (string, error) {
	queryParams := url.Values{
		"book_token":            {bookToken},
		"struct_payment_method": {fmt.Sprintf(`{"id":%s}`, paymentMethodID)},
	}

	params := make(map[string]string)
	for key, values := range queryParams {
    	if len(values) > 0 {
        	params[key] = values[0]
    	}
	}

	return sendPostRequest(api.resyToken, "api.resy.com/3/book", params)
}

// sendGetRequest sends a GET request
func sendGetRequest(resyToken config.ResyKeys, baseURL string, queryParams url.Values) (string, error) {
	url := fmt.Sprintf("https://%s?%s", baseURL, queryParams.Encode())

	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf(`ResyAPI api_key="%s"`, resyToken.ApiKey))
	req.Header.Set("x-resy-auth-token", resyToken.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(bodyBytes), nil
}

func sendPostRequest(resyKeys config.ResyKeys, baseURL string, queryParams map[string]string) (string, error) {
	url := fmt.Sprintf("https://%s", baseURL)

	post := stringifyQueryParams(queryParams)

	fmt.Printf("URL Request: %s\n", url)
	fmt.Printf("Post Params: %s\n", post)

	req, err := http.NewRequest("POST", url, strings.NewReader(post))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf(`ResyAPI api_key="%s"`, resyKeys.ApiKey))
	req.Header.Set("x-resy-auth-token", resyKeys.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("origin", "https://widgets.resy.com/")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func createHeaders(resyKeys config.ResyKeys) []string {
	headers := []string{
		fmt.Sprintf(`Authorization: ResyAPI api_key="%s"`, resyKeys.ApiKey),
		fmt.Sprintf("x-resy-auth-token: %s", resyKeys.AuthToken),
	}

	return headers
}

func stringifyQueryParams(queryParams map[string]string) string {
	values := url.Values{}

	for k, v := range queryParams {
		values.Add(k, v)
	}

	return values.Encode()
}