# Engineering Standards

Follow the engineering standard defined in @AGENTS.md, and load the per-domain deep-dive under `skills/engineering-standards/reference/<domain>.md` when relevant.

> **Claude Code users:** for automatic, progressive-disclosure loading (the standard triggers itself on coding tasks and pulls in only the relevant domain file), install the bundled skill as a plugin instead of relying on this file — see the README. This `CLAUDE.md` pointer is the fallback for when the plugin isn't installed.

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

