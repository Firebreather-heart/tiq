# TIQ AI — Autonomous Forex & Crypto Trading Platform

TIQ AI is a premium, high-reliability autonomous trading agent powered by **Allora Network AI inferences** and integrated with **Oanda REST API** brokerage services. It features a real-time interactive TradingView-style dashboard with candlestick charting, live P&L tracking, win-rate analytics, and manual order execution.

---

## Key Features

- **Allora Network AI**: Decentralised AI price inferences used as a weighted signal layer on top of technical indicators.
- **Smart Exit Logic**: Positions are closed early on signal reversal, EMA trend fade, or RSI overextension — not just at SL/TP.
- **Multi-Source Crypto Feeds**: Live candle data with automatic fallback — CoinGecko → Coinbase → Kraken → Binance.
- **Win Rate Card**: Live stats dashboard showing total trades, wins/losses, and cumulative PnL with an SVG ring indicator.
- **TradingView-Style Chart**: Full interactive candlestick chart with crosshair, O/H/L/C HUD, volume bars, and EMA overlays.
- **Dual Execution Modes**: Local simulator (no API key needed) or Oanda live/demo broker.

---

## Prerequisites

You only need two tools installed:

| Tool | Download |
|---|---|
| **Go (≥ 1.21)** | https://go.dev/dl/ |
| **Node.js (≥ 18)** | https://nodejs.org/ |

---

## Quick Start (One-Click)

### Windows
Double-click `start.bat` in the root folder — it handles everything automatically.

### macOS / Linux
```bash
chmod +x start.sh
./start.sh
```

Then open: **http://localhost:3000**

---

## Manual Setup (Step-by-Step)

If you prefer to run each part individually:

### 1. Clone the repo
```bash
git clone https://github.com/Firebreather-heart/tiq.git
cd tiq
```

### 2. Configure environment
```bash
cp env.example .env
# Open .env in a text editor and fill in your keys
```

### 3. Build & run the backend
```bash
# From the repo root
go build -o trading_bot ./backend/cmd/bot
./trading_bot
```
The API server will start on **http://localhost:8080**

### 4. Install & run the frontend
```bash
cd frontend
npm install
npm run dev
```
The dashboard will be available at **http://localhost:3000**

> **Note for Windows:** Replace `./trading_bot` with `trading_bot.exe`

---

## Environment Configuration (`.env`)

```env
# ── Asset & Mode ──────────────────────────────────────────
# Target instrument. Crypto: BTC_USD, ETH_USD | Forex: EUR_USD, GBP_USD
INSTRUMENT=BTC_USD

# Execution mode: simulator | demo | real
BOT_MODE=simulator

# ── Allora Network ─────────────────────────────────────────
ALLORA_CHAIN_ID=ethereum-11155111
ALLORA_TOPIC_ID=14
ALLORA_KEY=your_allora_api_key_here

# ── Oanda (required for demo/real modes only) ──────────────
OANDA_KEY=your_oanda_api_key_here
OANDA_ACCOUNT_ID=your_oanda_account_id_here

# ── CoinGecko (optional, improves crypto candle quality) ──
COIN_GECKO_KEY=your_coingecko_demo_key_here
```

Get a free CoinGecko demo key at: https://www.coingecko.com/en/api/pricing

---

## Strategy Logic

The bot runs a strategy tick every **1 minute**:

1. Fetches live OHLCV candles (CoinGecko → Coinbase → Kraken → Binance fallback chain)
2. Computes **Fast EMA**, **Slow EMA**, **RSI**, and **ATR**
3. Queries the **Allora AI network** for the latest inference
4. Generates a BUY / SELL / HOLD signal weighted by both technical + AI signals
5. Opens a position sized by **1% account risk / ATR-based stop distance**

### Early Exit Rules (fires before SL/TP)

| Trigger | Condition |
|---|---|
| Signal reversal | Opposite EMA+Allora signal appears |
| EMA trend fade | FastEMA crosses below SlowEMA on an open long (or above on a short) |
| RSI overextension | RSI > 75 on long, RSI < 25 on short |

---

## Dashboard Guide

| Widget | Description |
|---|---|
| **KPI Cards** | Balance, Equity, Active Position, Bot Environment, **Win Rate ring** |
| **Market Chart** | Interactive TradingView-style SVG. Hover for crosshair + O/H/L/C HUD |
| **Open Position** | Entry price, SL/TP, floating PnL, manual close button |
| **Manual Trading** | Market Buy/Sell with custom SL/TP |
| **Allora Inferences** | Live AI price predictions from the network |
| **Strategy Config** | Adjust EMA periods, risk %, ATR multiplier in real-time |
| **Live Console** | Scrolling real-time system log |