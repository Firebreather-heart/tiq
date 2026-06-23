package strategy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type PolymarketMarketInfo struct {
	Question       string    `json:"question"`   // e.g. "Will Bitcoin be above $69,120 at 13:50?"
	MarketID       string    `json:"market_id"`  // Unique contract address or ID (ConditionID)
	Strike         float64   `json:"strike"`     // Price to beat parsed
	Expiration     time.Time `json:"expiration"` // Settlement time
	StartTimestamp int64     `json:"start_timestamp"`
	YesPrice       float64   `json:"yes_price"`
	NoPrice        float64   `json:"no_price"`
	YesTokenID     string    `json:"yes_token_id"`
	NoTokenID      string    `json:"no_token_id"`
}

// FetchActivePolymarketStrike queries the real Polymarket Gamma API for the active BTC contract closest to spot
func FetchActivePolymarketStrike(currentPrice float64) (*PolymarketMarketInfo, error) {
	// Mathematically calculate the current and next 5-minute btc-updown market slugs
	now := time.Now().Unix()
	windowTS := (now / 300) * 300
	
	// Check the current active window first
	slug := fmt.Sprintf("btc-updown-5m-%d", windowTS)
	
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("https://gamma-api.polymarket.com/markets/slug/%s", slug)
	
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	type Market struct {
		ID            string `json:"id"`
		Question      string `json:"question"`
		ConditionID   string `json:"conditionId"`
		Slug          string `json:"slug"`
		EndDate       string `json:"endDate"`
		Active        bool   `json:"active"`
		Closed        bool   `json:"closed"`
		OutcomePrices string `json:"outcomePrices"`
		ClobTokenIds  string `json:"clobTokenIds"`
	}

	if resp.StatusCode == http.StatusOK {
		var m Market
		if err := json.NewDecoder(resp.Body).Decode(&m); err == nil {
			expTime, parseErr := time.Parse(time.RFC3339, m.EndDate)
			if parseErr != nil {
				expTime, parseErr = time.Parse("2006-01-02T15:04:05Z", m.EndDate)
			}
			
			// If it has more than 10 seconds remaining, trade this active window!
			if parseErr == nil && time.Until(expTime).Seconds() > 10.0 {
				var startTS int64
				parts := strings.Split(m.Slug, "-")
				if len(parts) > 0 {
					fmt.Sscanf(parts[len(parts)-1], "%d", &startTS)
				}

				yesPrice, noPrice := 0.50, 0.50
				var outcomePrices []string
				if err := json.Unmarshal([]byte(m.OutcomePrices), &outcomePrices); err == nil && len(outcomePrices) >= 2 {
					if yp, err := strconv.ParseFloat(outcomePrices[0], 64); err == nil {
						yesPrice = yp
					}
					if np, err := strconv.ParseFloat(outcomePrices[1], 64); err == nil {
						noPrice = np
					}
				}

				yesTokenID, noTokenID := "", ""
				var tokenIDs []string
				if err := json.Unmarshal([]byte(m.ClobTokenIds), &tokenIDs); err == nil && len(tokenIDs) >= 2 {
					yesTokenID = tokenIDs[0]
					noTokenID = tokenIDs[1]
				}

				return &PolymarketMarketInfo{
					Question:       m.Question,
					MarketID:       m.ConditionID,
					Strike:         currentPrice, // default fallback, will be overwritten in strategy.go
					Expiration:     expTime,
					StartTimestamp: startTS,
					YesPrice:       yesPrice,
					NoPrice:        noPrice,
					YesTokenID:     yesTokenID,
					NoTokenID:      noTokenID,
				}, nil
			}
		}
	}

	// If current is too close to expiry or not found, query the NEXT upcoming window!
	nextSlug := fmt.Sprintf("btc-updown-5m-%d", windowTS+300)
	nextURL := fmt.Sprintf("https://gamma-api.polymarket.com/markets/slug/%s", nextSlug)
	
	nextResp, err := client.Get(nextURL)
	if err != nil {
		return nil, err
	}
	defer nextResp.Body.Close()

	if nextResp.StatusCode == http.StatusOK {
		var m Market
		if err := json.NewDecoder(nextResp.Body).Decode(&m); err == nil {
			expTime, parseErr := time.Parse(time.RFC3339, m.EndDate)
			if parseErr != nil {
				expTime, parseErr = time.Parse("2006-01-02T15:04:05Z", m.EndDate)
			}
			
			if parseErr == nil {
				var startTS int64
				parts := strings.Split(m.Slug, "-")
				if len(parts) > 0 {
					fmt.Sscanf(parts[len(parts)-1], "%d", &startTS)
				}

				yesPrice, noPrice := 0.50, 0.50
				var outcomePrices []string
				if err := json.Unmarshal([]byte(m.OutcomePrices), &outcomePrices); err == nil && len(outcomePrices) >= 2 {
					if yp, err := strconv.ParseFloat(outcomePrices[0], 64); err == nil {
						yesPrice = yp
					}
					if np, err := strconv.ParseFloat(outcomePrices[1], 64); err == nil {
						noPrice = np
					}
				}

				yesTokenID, noTokenID := "", ""
				var tokenIDs []string
				if err := json.Unmarshal([]byte(m.ClobTokenIds), &tokenIDs); err == nil && len(tokenIDs) >= 2 {
					yesTokenID = tokenIDs[0]
					noTokenID = tokenIDs[1]
				}

				return &PolymarketMarketInfo{
					Question:       m.Question,
					MarketID:       m.ConditionID,
					Strike:         currentPrice, // default fallback
					Expiration:     expTime,
					StartTimestamp: startTS,
					YesPrice:       yesPrice,
					NoPrice:        noPrice,
					YesTokenID:     yesTokenID,
					NoTokenID:      noTokenID,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("no active or upcoming 5m btc-updown market found on Polymarket Gamma API")
}

// FetchPolymarketPricesByExpiry retrieves the outcome prices for a 5m contract by its expiry timestamp
func FetchPolymarketPricesByExpiry(expiryUnix int64) (float64, float64, error) {
	startTS := expiryUnix - 300
	slug := fmt.Sprintf("btc-updown-5m-%d", startTS)
	
	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("https://gamma-api.polymarket.com/markets/slug/%s", slug)
	
	resp, err := client.Get(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("status code %d for slug %s", resp.StatusCode, slug)
	}

	type Market struct {
		OutcomePrices string `json:"outcomePrices"`
	}

	var m Market
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return 0, 0, err
	}

	yesPrice, noPrice := 0.50, 0.50
	var outcomePrices []string
	if err := json.Unmarshal([]byte(m.OutcomePrices), &outcomePrices); err == nil && len(outcomePrices) >= 2 {
		if yp, err := strconv.ParseFloat(outcomePrices[0], 64); err == nil {
			yesPrice = yp
		}
		if np, err := strconv.ParseFloat(outcomePrices[1], 64); err == nil {
			noPrice = np
		}
		return yesPrice, noPrice, nil
	}

	return 0, 0, fmt.Errorf("failed to parse outcome prices")
}

