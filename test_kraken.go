package main

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

func fetch(url string) {
	fmt.Println("Fetching:", url)
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "TIQ-AI-Agent/1.0")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Status: %d, Response: %s\n\n", resp.StatusCode, string(body))
}

func main() {
	fetch("https://api.bybit.com/v5/market/kline?category=linear&symbol=BTCUSDT&interval=5&limit=10")
	fetch("https://api.kucoin.com/api/v1/market/candles?type=5min&symbol=BTC-USDT")
	fetch("https://api.kraken.com/0/public/OHLC?pair=XBTUSD&interval=5")
	fetch("https://api.bybit.com/v5/market/tickers?category=linear&symbol=BTCUSDT")
}

