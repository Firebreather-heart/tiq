package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"tiq/backend/pkg/db"
	"tiq/backend/pkg/engine"
	"tiq/backend/pkg/strategy"
)

type Server struct {
	store  *db.DB
	runner *strategy.Runner
	engine engine.ExecutionEngine
	port   int
}

func NewServer(store *db.DB, runner *strategy.Runner, eng engine.ExecutionEngine, port int) *Server {
	return &Server{
		store:  store,
		runner: runner,
		engine: eng,
		port:   port,
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register API routes
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/config", s.handleConfigUpdate)
	mux.HandleFunc("GET /api/positions", s.handlePositions)
	mux.HandleFunc("GET /api/trades", s.handleTrades)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/inferences", s.handleInferences)
	mux.HandleFunc("GET /api/candles", s.handleCandles)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/price", s.handlePrice)
	mux.HandleFunc("POST /api/trade/manual", s.handleManualTrade)
	mux.HandleFunc("POST /api/trade/close", s.handleClosePosition)

	// Wrap mux with CORS middleware
	handler := s.enableCORS(mux)

	s.store.Log("INFO", fmt.Sprintf("API Server starting on port %d...", s.port))
	return http.ListenAndServe(fmt.Sprintf(":%d", s.port), handler)
}

func (s *Server) enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	bal, eq, err := s.engine.GetBalance()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get balance: %v", err))
		return
	}

	response := map[string]interface{}{
		"runner_config": s.runner.GetConfig(),
		"environment":   s.engine.GetEnvironment(),
		"balance":       bal,
		"equity":        eq,
		"timestamp":     time.Now(),
	}

	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	var req strategy.Config
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	s.runner.UpdateConfig(req)
	s.writeJSON(w, http.StatusOK, map[string]string{"message": "Configuration updated successfully"})
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	pos, err := s.engine.GetOpenPositions()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch open positions: %v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, pos)
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	trades, err := s.engine.GetTrades()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch historical trades: %v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, trades)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}

	logs, err := s.store.GetLogs(limit)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch logs: %v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, logs)
}

func (s *Server) handleInferences(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}

	inferences, err := s.store.GetInferences(limit)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch inferences: %v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, inferences)
}

func (s *Server) handleManualTrade(w http.ResponseWriter, r *http.Request) {
	type ManualTradeReq struct {
		Instrument string  `json:"instrument"`
		Units      float64 `json:"units"`
		Price      float64 `json:"price"`
		StopLoss   float64 `json:"stop_loss"`
		TakeProfit float64 `json:"take_profit"`
	}

	var req ManualTradeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.Instrument == "" || req.Units == 0 || req.Price == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "Missing required parameters (instrument, units, price)")
		return
	}

	posID, err := s.engine.OpenPosition(req.Instrument, req.Units, req.Price, req.StopLoss, req.TakeProfit)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to place manual trade: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":     "Manual trade executed successfully",
		"position_id": posID,
	})
}

func (s *Server) handleClosePosition(w http.ResponseWriter, r *http.Request) {
	type CloseReq struct {
		PositionID   string  `json:"position_id"`
		CurrentPrice float64 `json:"current_price"`
	}

	var req CloseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.PositionID == "" || req.CurrentPrice == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "Missing required parameters (position_id, current_price)")
		return
	}

	err := s.engine.ClosePosition(req.PositionID, req.CurrentPrice)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to close position: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"message": "Position closed successfully"})
}

// Helpers
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) writeJSONError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleCandles(w http.ResponseWriter, r *http.Request) {
	instrument := r.URL.Query().Get("instrument")
	if instrument == "" {
		instrument = s.runner.GetConfig().Instrument
	}

	countStr := r.URL.Query().Get("count")
	count := 100
	if countStr != "" {
		if c, err := strconv.Atoi(countStr); err == nil {
			count = c
		}
	}

	candles, err := s.runner.GetCandles(instrument, count)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch candles: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, candles)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetWinRate()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch stats: %v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, stats)
}

// handlePrice returns the latest live price the engine knows about for a given instrument.
// This is updated every strategy tick from the real market feed — much fresher than candle close.
func (s *Server) handlePrice(w http.ResponseWriter, r *http.Request) {
	instrument := r.URL.Query().Get("instrument")
	if instrument == "" {
		instrument = s.runner.GetConfig().Instrument
	}
	price, ok := s.engine.GetPrice(instrument)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no live price available for %s", instrument))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"instrument": instrument,
		"price":      price,
	})
}
