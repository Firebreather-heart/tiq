@echo off
echo ===================================================
echo     TIQ AI - Autonomous Trading Platform Launcher
echo ===================================================
echo.

:: Check for Go
where go >nul 2>nul
if %errorlevel% neq 0 (
    echo [ERROR] Go (Golang) is not installed!
    echo Please download and install Go from: https://go.dev/dl/
    echo.
    pause
    exit /b 1
)

:: Check for Node.js
where node >nul 2>nul
if %errorlevel% neq 0 (
    echo [ERROR] Node.js is not installed!
    echo Please download and install Node.js from: https://nodejs.org/
    echo.
    pause
    exit /b 1
)

:: Create .env file if it doesn't exist
if not exist .env (
    echo [INFO] Creating default .env configuration file...
    echo INSTRUMENT=BTC_USD>.env
    echo ALLORA_CHAIN_ID=ethereum-11155111>>.env
    echo ALLORA_TOPIC_ID=14>>.env
    echo ALLORA_KEY=>>.env
    echo OANDA_KEY=>>.env
    echo OANDA_ACCOUNT_ID=>>.env
    echo BOT_MODE=simulator>>.env
    echo [WARNING] Default .env created. You can open it to configure your OANDA/Allora keys!
    echo.
)

:: Start Backend in a new window
echo [INFO] Installing backend dependencies and starting Go Server...
start "TIQ AI Backend Server" cmd /k "go run backend/cmd/bot/main.go"

:: Start Frontend in current window
echo [INFO] Installing frontend dependencies...
cd frontend
call npm install
echo.
echo [INFO] Starting Next.js Web Dashboard...
echo Open http://localhost:3000 in your browser to view the dashboard!
echo.
npm run dev

pause
