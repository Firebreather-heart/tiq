package oanda

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type Client struct {
	token      string
	accountID  string
	baseURL    string
	httpClient *http.Client
}

type Candle struct {
	Time   time.Time `json:"time"`
	Volume int       `json:"volume"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
}

type AccountsResponse struct {
	Accounts []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	} `json:"accounts"`
}

type AccountSummaryResponse struct {
	Summary struct {
		ID        string `json:"id"`
		Balance   string `json:"balance"`
		Currency  string `json:"currency"`
		NAV       string `json:"NAV"`
		UnrealizedPL string `json:"unrealizedPL"`
	} `json:"account"`
}

type OandaCandlesResponse struct {
	Instrument  string `json:"instrument"`
	Granularity string `json:"granularity"`
	Candles     []struct {
		Complete bool      `json:"complete"`
		Volume   int       `json:"volume"`
		Time     time.Time `json:"time"`
		Mid      struct {
			O string `json:"o"`
			H string `json:"h"`
			L string `json:"l"`
			C string `json:"c"`
		} `json:"mid"`
	} `json:"candles"`
}

type OrderResponse struct {
	OrderFillTransaction struct {
		ID           string `json:"id"`
		AccountID    string `json:"accountId"`
		BatchID      string `json:"batchId"`
		Price        string `json:"price"`
		Units        string `json:"units"`
		Commission   string `json:"commission"`
		Financing    string `json:"financing"`
		RealizedPL   string `json:"realizedPL"`
		Instrument   string `json:"instrument"`
		Timestamp    time.Time `json:"time"`
	} `json:"orderFillTransaction"`
}

func NewClient(token, accountID string, live bool) *Client {
	baseURL := "https://api-fxpractice.oanda.com/v3"
	if live {
		baseURL = "https://api-fxtrade.oanda.com/v3"
	}
	return &Client{
		token:     token,
		accountID: accountID,
		baseURL:   baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) SetAccountID(accountID string) {
	c.accountID = accountID
}

func (c *Client) GetAccountID() string {
	return c.accountID
}

func (c *Client) doRequest(method, path string, body []byte) ([]byte, error) {
	url := c.baseURL + path
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http call failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("oanda request returned error status: %d (body: %s)", resp.StatusCode, string(respBytes))
	}

	return respBytes, nil
}

// FetchAccountID resolves the first account ID available under the token
func (c *Client) FetchAccountID() (string, error) {
	res, err := c.doRequest("GET", "/accounts", nil)
	if err != nil {
		return "", err
	}

	var accs AccountsResponse
	if err := json.Unmarshal(res, &accs); err != nil {
		return "", fmt.Errorf("failed to parse accounts json: %w", err)
	}

	if len(accs.Accounts) == 0 {
		return "", fmt.Errorf("no Oanda accounts found for this token")
	}

	return accs.Accounts[0].ID, nil
}

// GetAccountSummary returns account balance, nav, and currency
func (c *Client) GetAccountSummary() (float64, float64, string, error) {
	if c.accountID == "" {
		return 0, 0, "", fmt.Errorf("account ID is not set")
	}

	path := fmt.Sprintf("/accounts/%s/summary", c.accountID)
	res, err := c.doRequest("GET", path, nil)
	if err != nil {
		return 0, 0, "", err
	}

	var sum AccountSummaryResponse
	if err := json.Unmarshal(res, &sum); err != nil {
		return 0, 0, "", fmt.Errorf("failed to parse account summary: %w", err)
	}

	bal, _ := strconv.ParseFloat(sum.Summary.Balance, 64)
	nav, _ := strconv.ParseFloat(sum.Summary.NAV, 64)
	return bal, nav, sum.Summary.Currency, nil
}

// GetCandles returns historical/latest candles for an instrument
func (c *Client) GetCandles(instrument string, count int, granularity string) ([]Candle, error) {
	path := fmt.Sprintf("/instruments/%s/candles?count=%d&granularity=%s&price=M", instrument, count, granularity)
	res, err := c.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var oandaResp OandaCandlesResponse
	if err := json.Unmarshal(res, &oandaResp); err != nil {
		return nil, fmt.Errorf("failed to parse candles json: %w", err)
	}

	var candles []Candle
	for _, oCandle := range oandaResp.Candles {
		o, _ := strconv.ParseFloat(oCandle.Mid.O, 64)
		h, _ := strconv.ParseFloat(oCandle.Mid.H, 64)
		l, _ := strconv.ParseFloat(oCandle.Mid.L, 64)
		cl, _ := strconv.ParseFloat(oCandle.Mid.C, 64)

		candles = append(candles, Candle{
			Time:   oCandle.Time,
			Volume: oCandle.Volume,
			Open:   o,
			High:   h,
			Low:    l,
			Close:  cl,
		})
	}

	return candles, nil
}

// PlaceMarketOrder places a trade order with take profit and stop loss parameters
func (c *Client) PlaceMarketOrder(instrument string, units float64, stopLoss, takeProfit float64) (*OrderResponse, error) {
	if c.accountID == "" {
		return nil, fmt.Errorf("account ID is not set")
	}

	type OrderField struct {
		Type             string      `json:"type"`
		Instrument       string      `json:"instrument"`
		Units            string      `json:"units"`
		TimeInForce      string      `json:"timeInForce"`
		PositionFill     string      `json:"positionFill"`
		StopLossOnFill   interface{} `json:"stopLossOnFill,omitempty"`
		TakeProfitOnFill interface{} `json:"takeProfitOnFill,omitempty"`
	}

	type OrderPayload struct {
		Order OrderField `json:"order"`
	}

	type TP struct {
		Price string `json:"price"`
	}
	type SL struct {
		Price string `json:"price"`
	}

	unitsStr := strconv.FormatFloat(units, 'f', 2, 64)

	payload := OrderPayload{
		Order: OrderField{
			Type:         "MARKET",
			Instrument:   instrument,
			Units:        unitsStr,
			TimeInForce:  "FOK",
			PositionFill: "DEFAULT",
		},
	}

	if takeProfit > 0 {
		payload.Order.TakeProfitOnFill = TP{Price: strconv.FormatFloat(takeProfit, 'f', 5, 64)}
	}
	if stopLoss > 0 {
		payload.Order.StopLossOnFill = SL{Price: strconv.FormatFloat(stopLoss, 'f', 5, 64)}
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order body: %w", err)
	}

	path := fmt.Sprintf("/accounts/%s/orders", c.accountID)
	res, err := c.doRequest("POST", path, bodyBytes)
	if err != nil {
		return nil, err
	}

	var ordResp OrderResponse
	if err := json.Unmarshal(res, &ordResp); err != nil {
		return nil, fmt.Errorf("failed to parse order response: %w", err)
	}

	return &ordResp, nil
}

// ClosePosition closes out any open units for an instrument
func (c *Client) ClosePosition(instrument string) error {
	if c.accountID == "" {
		return fmt.Errorf("account ID is not set")
	}

	type ClosePayload struct {
		LongUnits  string `json:"longUnits,omitempty"`
		ShortUnits string `json:"shortUnits,omitempty"`
	}

	// We close both directions to be safe
	payload := ClosePayload{
		LongUnits:  "ALL",
		ShortUnits: "ALL",
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/accounts/%s/positions/%s/close", c.accountID, instrument)
	_, err = c.doRequest("PUT", path, bodyBytes)
	return err
}
