package resy

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"resy-snipe/config"
	"strings"
)

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
	fmt.Println("-- Find --")
	return sendGetRequest(api.resyToken, "api.resy.com/4/find", queryParams)
}

// GetReservationDetails gets details of the reservation
func (api *ResyAPI) GetReservationDetails(configID string, date string, partySize int) (string, error) {
	queryParams := url.Values{
		"config_id":  {configID},
		"day":        {date},
		"party_size": {fmt.Sprintf("%d", partySize)},
	}
	fmt.Println("-- Details --")
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
func sendGetRequest(resyKeys config.ResyKeys, baseURL string, queryParams url.Values) (string, error) {
	url := fmt.Sprintf("https://%s?%s", baseURL, queryParams.Encode())

	fmt.Println("-- URL:", url)

	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", fmt.Sprintf(`ResyAPI api_key="%s"`, resyKeys.ApiKey))
	req.Header.Set("X-Resy-Universal-Auth", resyKeys.AuthToken)
	req.Header.Set("x-resy-auth-token", resyKeys.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://widgets.resy.com")
	req.Header.Set("Referer", "https://widgets.resy.com/")
	req.Header.Set("X-Origin", "https://widgets.resy.com")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("Error reading response body: ", err)
		}
		fmt.Println("failed to book reservation: ", resp.Status)
		fmt.Println("failed to book reservation: ", resp.Body)
		fmt.Println("failed to book reservation: ", respBody)
		return "", fmt.Errorf("failed to book reservation: %s", resp.Status)
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(bodyBytes), nil
}

func sendPostRequest(resyKeys config.ResyKeys, baseURL string, queryParams map[string]string) (string, error) {
	url := fmt.Sprintf("https://%s", baseURL)

	post := stringifyQueryParams(queryParams)

	req, err := http.NewRequest("POST", url, strings.NewReader(post))
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", fmt.Sprintf(`ResyAPI api_key="%s"`, resyKeys.ApiKey))
	req.Header.Set("X-Resy-Universal-Auth", resyKeys.AuthToken)
	req.Header.Set("x-resy-auth-token", resyKeys.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://widgets.resy.com")
	req.Header.Set("Referer", "https://widgets.resy.com/")
	req.Header.Set("X-Origin", "https://widgets.resy.com")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		fmt.Println("request failed:", url)
		fmt.Println("status:", resp.Status)
		fmt.Println("content-type:", resp.Header.Get("Content-Type"))
		fmt.Println("content-encoding:", resp.Header.Get("Content-Encoding"))
		fmt.Println("x-request-id:", resp.Header.Get("x-request-id"))
		fmt.Println("body-len:", len(b))
		fmt.Println("body:", string(b))
		return "", fmt.Errorf("request failed: %s", resp.Status)
	}

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
