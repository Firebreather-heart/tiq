package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"tiq/backend/pkg/api"
	"tiq/backend/pkg/db"
	"tiq/backend/pkg/engine"
	"tiq/backend/pkg/oanda"
	"tiq/backend/pkg/strategy"
)

func main() {
	log.Println("Starting Allora-Oanda Trading Bot...")

	// 1. Load env variables manually from .env (to stick to standard library)
	// Check backend/.env first (when running bot.exe from project root), then fall back to .env
	env := loadEnv("backend/.env")
	if len(env) == 0 {
		env = loadEnv(".env")
	}

	oandaKey := env["OANDA_KEY"]
	botMode := env["BOT_MODE"]           // "simulator", "demo" (Oanda Demo), "real" (Oanda Live)
	instrument := env["INSTRUMENT"]       // e.g. "EUR_USD"
	dbPath := env["DB_PATH"]

	// Set defaults
	if dbPath == "" {
		dbPath = "trading_bot.db"
	}
	if instrument == "" {
		instrument = "BTC_USD"
	}
	if botMode == "" {
		botMode = "simulator" // default to simulator
	}

	// 2. Initialize Database
	store, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("Critical: failed to initialize SQLite database: %v", err)
	}
	defer store.Close()
	store.Log("INFO", fmt.Sprintf("Trading Bot initialized. Mode: %s, DB: %s", botMode, dbPath))

	// 3. Initialize Clients
	var oClient *oanda.Client
	var execEngine engine.ExecutionEngine

	if oandaKey != "" {
		isLive := botMode == "real"
		oClient = oanda.NewClient(oandaKey, env["OANDA_ACCOUNT_ID"], isLive)
	}

	// 4. Resolve Execution Engine
	switch botMode {
	case "polymarket":
		store.Log("INFO", "Initializing Web3 Polymarket Execution Engine...")
		poly, err := engine.NewPolymarketEngine(store, "", "")
		if err != nil {
			log.Fatalf("Critical: failed to create Polymarket engine: %v", err)
		}
		execEngine = poly
		store.Log("INFO", "Web3 Polymarket Engine successfully loaded.")

	case "demo", "real":
		if oandaKey == "" {
			log.Fatalf("Critical: OANDA_KEY is required for broker modes (demo/real)")
		}

		// Auto-discover account ID if not provided
		if env["OANDA_ACCOUNT_ID"] == "" {
			store.Log("INFO", "OANDA_ACCOUNT_ID not provided. Discovering first available account...")
			accID, err := oClient.FetchAccountID()
			if err != nil {
				log.Fatalf("Critical: failed to discover Oanda account ID: %v", err)
			}
			oClient.SetAccountID(accID)
			store.Log("INFO", fmt.Sprintf("Discovered Oanda Account ID: %s", accID))
		}

		execEngine = engine.NewOandaBroker(oClient, store, oClient.GetAccountID())
		store.Log("INFO", fmt.Sprintf("Broker engine initialized for account %s", oClient.GetAccountID()))

	case "simulator":
		fallthrough
	default:
		// If Oanda Client is missing but we are in simulation, we can initialize Oanda client with dummy or try to fetch data.
		// For simulator to get live candles, we need a working OANDA_KEY.
		if oandaKey == "" {
			store.Log("WARN", "OANDA_KEY is empty. The simulator will require Oanda candles for tick data. Please provide it in .env.")
		}
		sim, err := engine.NewSimulator(store, 100000.0) // $100k start
		if err != nil {
			log.Fatalf("Critical: failed to create simulator: %v", err)
		}
		execEngine = sim
		store.Log("INFO", "Local Simulator engine initialized with $100,000.00 starting balance")
	}

	// 5. Initialize Strategy Runner
	stratConfig := strategy.Config{
		Instrument:      instrument,
		Granularity:     "M5", // 5 minute bars
		RiskPercent:     50.0, // Risk 50% of balance per trade ($5 on a $10 balance)
		AtrMultiplier:   2.0,  // SL is 2 * ATR
		TpMultiplier:    3.0,  // TP is 3 * ATR
		EmaFastPeriod:   10,
		EmaSlowPeriod:   25,
		RsiPeriod:       14,
		MinRsiFilter:    30,
		MaxRsiFilter:    70,
		TradingEnabled:  true,
		UseAllora:       false,
		DefaultPipValue: 0.0001,
	}

	runner := strategy.NewRunner(stratConfig, store, oClient, nil, execEngine)

	// Sync initial balance
	_, _, err = execEngine.GetBalance()
	if err != nil {
		store.Log("WARN", fmt.Sprintf("Failed to fetch initial balance: %v", err))
	}

	// 6. Start Ticker in Background
	go func() {
		store.Log("INFO", "Background strategy ticker started.")
		// Oanda standard granularity M5 runs every 5 minutes.
		// For immediate testing and fast demo simulation, we will run every 1 second.
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// Run immediate tick on start
		if err := runner.Tick(); err != nil {
			store.Log("ERROR", fmt.Sprintf("Tick execution failed: %v", err))
		}

		for range ticker.C {
			if err := runner.Tick(); err != nil {
				store.Log("ERROR", fmt.Sprintf("Tick execution failed: %v", err))
			}
		}
	}()

	// 6.5. Start Live Price Ticker in Background
	go func() {
		store.Log("INFO", "Background live price ticker started.")
		liveTicker := time.NewTicker(1 * time.Second)
		defer liveTicker.Stop()

		for range liveTicker.C {
			if err := runner.LiveTick(); err != nil {
				// Silently handle to prevent spamming logs
			}
		}
	}()

	// 6.6. Start log-retention pruner: keep the most recent 200k log rows so the SQLite DB
	// can't grow unbounded from high-frequency WS trade prints on a 24/7 run.
	go func() {
		const keepRows = 200000
		pruneTicker := time.NewTicker(15 * time.Minute)
		defer pruneTicker.Stop()

		// Trim any existing backlog once on startup, then on the interval.
		if n, err := store.PruneLogs(keepRows); err == nil && n > 0 {
			store.Log("INFO", fmt.Sprintf("Log retention: pruned %d old log rows (keeping last %d).", n, keepRows))
		}
		for range pruneTicker.C {
			if n, err := store.PruneLogs(keepRows); err == nil && n > 0 {
				store.Log("INFO", fmt.Sprintf("Log retention: pruned %d old log rows (keeping last %d).", n, keepRows))
			}
		}
	}()

	// 7. Start REST API Server
	serverPort := 8081
	apiServer := api.NewServer(store, runner, execEngine, serverPort)
	if err := apiServer.Start(); err != nil {
		log.Fatalf("Critical: API Server stopped: %v", err)
	}
}

// loadEnv reads environment variables from a given file path
func loadEnv(filepath string) map[string]string {
	envMap := make(map[string]string)
	file, err := os.Open(filepath)
	if err != nil {
		return envMap
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Strip any quotes if present
			val = strings.Trim(val, `"'`)
			envMap[key] = val
			os.Setenv(key, val) // Load into OS environment
		}
	}
	return envMap
}
