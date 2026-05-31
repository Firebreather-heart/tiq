#!/bin/bash
echo "==================================================="
echo "    TIQ AI - Autonomous Trading Platform Launcher"
echo "==================================================="
echo

# Check for Go
if ! command -v go &> /dev/null; then
    echo "[ERROR] Go (Golang) is not installed!"
    echo "Please install Go: https://go.dev/dl/"
    exit 1
fi

# Check for Node.js
if ! command -v node &> /dev/null; then
    echo "[ERROR] Node.js is not installed!"
    echo "Please install Node.js: https://nodejs.org/"
    exit 1
fi

# Create .env if not exists
if [ ! -f .env ]; then
    echo "[INFO] Creating default .env configuration file..."
    echo "INSTRUMENT=BTC_USD" > .env
    echo "ALLORA_CHAIN_ID=ethereum-11155111" >> .env
    echo "ALLORA_TOPIC_ID=14" >> .env
    echo "ALLORA_KEY=" >> .env
    echo "OANDA_KEY=" >> .env
    echo "OANDA_ACCOUNT_ID=" >> .env
    echo "BOT_MODE=simulator" >> .env
    echo "[WARNING] Default .env created. Update it with your keys when ready!"
    echo
fi

# Function to kill background server on exit
cleanup() {
    echo
    echo "[INFO] Stopping background server..."
    kill $BACKEND_PID 2>/dev/null
    exit
}
trap cleanup SIGINT SIGTERM EXIT

# Start Go Backend
echo "[INFO] Starting Go backend server in background..."
go run backend/cmd/bot/main.go &
BACKEND_PID=$!

# Start Next.js Frontend
echo "[INFO] Installing frontend dependencies..."
cd frontend
npm install
echo
echo "[INFO] Starting Next.js Web Dashboard..."
echo "Open http://localhost:3000 in your browser!"
echo
npm run dev
