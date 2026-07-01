# Engineering Standards

Follow the engineering standard defined in @AGENTS.md. When a task touches a specific domain (backend, data, concurrency, frontend, mobile, cloud, systems, security, testing, architecture, paradigms, algorithms, or version-control craft), open and apply the matching file under `skills/engineering-standards/reference/<domain>.md`.

## 🛠️ Bot Operations (Running & Restarting)

AI models and coding agents have full permission and access to control, start, stop, and restart the backend bot (`bot.exe`) and Next.js frontend.

### 1. Kill Any Active Bot Processes
To ensure ports `8081` (backend) and `3000`/`3001` (frontend) are released, terminate any existing execution:
* **PowerShell**:
  ```powershell
  Stop-Process -Name "bot" -Force -ErrorAction SilentlyContinue
  Stop-Process -Name "main" -Force -ErrorAction SilentlyContinue
  ```
* **Command Prompt (CMD)**:
  ```cmd
  taskkill /F /IM bot.exe /T 2>nul
  taskkill /F /IM main.exe /T 2>nul
  ```

### 2. Run / Restart the Backend Bot
Run the compiled executable from the workspace root, or run directly from the source code:
* **Option A: Run Compiled Binary** (Recommended):
  ```powershell
  .\bot.exe
  ```
* **Option B: Run from Source**:
  ```powershell
  go run backend/cmd/bot/main.go
  ```

### 3. Run / Restart the Next.js Web Dashboard
* Run:
  ```powershell
  cd frontend
  npm run dev
  ```

