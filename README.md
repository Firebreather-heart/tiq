# TIQ AI — Autonomous Forex & Crypto Trading Platform

TIQ AI is a premium, high-reliability autonomous trading agent powered by **Allora Network AI inferences** and integrated with **Oanda REST API** brokerage services. It features an interactive, real-time glassmorphic React dashboard for strategy visualization, telemetry tracking, and manual order execution.

---

## Key Features
* **Allora Network AI Integration**: Utilizes decentralised v2 consumer predictions for currency and crypto assets.
* **Dual Execution Modes**:
  * **Local Simulator**: Risk-free simulation sandbox with simulated real-time price volatility and automated Stop Loss / Take Profit triggers (perfect for weekend runs and testing).
  * **Oanda Broker Mode**: Connects directly to Oanda Practice (`demo`) or Live (`real`) accounts.
* **Premium Glassmorphic Dashboard**: Real-time interactive charting (Fast/Slow EMA overlays), live floating P&L tracking, and transaction logging.

---

## Prerequisites (Easy Setup)

To run TIQ AI on any machine (especially Windows), you only need two free tools:

1. **Go (Golang)**: [Download & Install Go](https://go.dev/dl/) (Required to run the trading core).
2. **Node.js**: [Download & Install Node.js](https://nodejs.org/) (Required to run the visual dashboard).

---

## How to Run (One-Click Launch)

### 💻 Windows Users
1. Double-click the `start.bat` file in the root folder.
2. The script will verify your installations, create a configuration file, start the Go server, and launch the dashboard.
3. Open your browser and navigate to: **[http://localhost:3000](http://localhost:3000)**

### 🍎 macOS & 🐧 Linux Users
1. Open your terminal in this directory.
2. Run the launcher script:
   ```bash
   ./start.sh
   ```
3. Open your browser and navigate to: **[http://localhost:3000](http://localhost:3000)**

---

## Configuration (`.env`)

You can configure your keys and asset targets by opening the `.env` file in a text editor (created automatically on first launch):

```env
# Target asset symbol (e.g., BTC_USD, EUR_USD, GBP_USD)
INSTRUMENT=BTC_USD

# Mode of execution: simulator, demo, or real
BOT_MODE=simulator

# Allora Network credentials
ALLORA_CHAIN_ID=ethereum-11155111
ALLORA_TOPIC_ID=14
ALLORA_KEY=your_allora_api_key_here

# Oanda credentials (required for demo/real modes)
OANDA_KEY=your_oanda_api_key_here
OANDA_ACCOUNT_ID=your_oanda_account_id_here
```

---

## Premium Dashboard Guide

* **Market Charting**: Renders M5 (5-minute) candlesticks with dynamic overlay of Fast and Slow EMAs.
* **Account KPIs**: Tracks live balance, real-time fluctuating equity, and active position size.
* **Open Position**: View open trades, current entry price, active Stop Loss/Take Profit thresholds, and dynamic floating P&L.
* **Manual Trading Widget**: Quickly enter Market Buy/Sell orders with optional custom SL/TP levels.
* **Allora Network Inferences**: Lists the latest cached machine-learning inferences received from the network.