package allora

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type APIInferenceResponse struct {
	Status bool `json:"status"`
	Data   struct {
		InferenceData struct {
			TopicID                    string `json:"topic_id"`
			NetworkInferenceNormalized string `json:"network_inference_normalized"`
			Timestamp                  int64  `json:"timestamp"`
		} `json:"inference_data"`
	} `json:"data"`
}

type Inference struct {
	TopicID       int
	BlockHeight   int64
	CombinedValue string
	ParsedValue   float64
	Timestamp     time.Time
}

func NewClient(apiKey string, chainID string) *Client {
	if chainID == "" {
		chainID = "ethereum-11155111"
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: fmt.Sprintf("https://api.allora.network/v2/allora/consumer/%s", chainID),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetLatestInference queries the Allora API for the latest inference on the specified topic.
// It implements exponential backoff retry in case of 429 Too Many Requests.
func (c *Client) GetLatestInference(topicID int) (*Inference, error) {
	url := fmt.Sprintf("%s?allora_topic_id=%d", c.baseURL, topicID)

	var bodyBytes []byte
	maxRetries := 4
	baseDelay := 1 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Accept", "application/json")
		if c.apiKey != "" {
			req.Header.Set("x-api-key", c.apiKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt == maxRetries-1 {
				return nil, fmt.Errorf("http request failed after %d attempts: %w", maxRetries, err)
			}
			time.Sleep(baseDelay * time.Duration(math.Pow(2, float64(attempt))))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			// Rate limited, wait and retry
			if attempt == maxRetries-1 {
				return nil, fmt.Errorf("rate limited by Allora API: %d Too Many Requests", resp.StatusCode)
			}
			delay := baseDelay * time.Duration(math.Pow(2, float64(attempt)))
			time.Sleep(delay)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("received non-ok status code: %d", resp.StatusCode)
		}

		bodyBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}
		break
	}

	var apiResp APIInferenceResponse
	if err := json.Unmarshal(bodyBytes, &apiResp); err != nil {
		// Fallback parse just in case structure differs
		var fallback map[string]interface{}
		if fErr := json.Unmarshal(bodyBytes, &fallback); fErr == nil {
			return nil, fmt.Errorf("unsupported json schema, got: %s", string(bodyBytes))
		}
		return nil, fmt.Errorf("failed to unmarshal JSON response: %w (body: %s)", err, string(bodyBytes))
	}

	if !apiResp.Status {
		return nil, fmt.Errorf("allora api returned status false: %s", string(bodyBytes))
	}

	combinedVal := apiResp.Data.InferenceData.NetworkInferenceNormalized
	parsedVal, err := strconv.ParseFloat(combinedVal, 64)
	if err != nil {
		parsedVal = 0.0
	}

	tID, _ := strconv.Atoi(apiResp.Data.InferenceData.TopicID)
	if tID == 0 {
		tID = topicID
	}

	blockHeight := apiResp.Data.InferenceData.Timestamp
	if blockHeight == 0 {
		blockHeight = time.Now().Unix()
	}

	return &Inference{
		TopicID:       tID,
		BlockHeight:   blockHeight,
		CombinedValue: combinedVal,
		ParsedValue:   parsedVal,
		Timestamp:     time.Unix(apiResp.Data.InferenceData.Timestamp, 0),
	}, nil
}
