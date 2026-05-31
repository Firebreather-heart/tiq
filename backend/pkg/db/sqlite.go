package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn *sql.DB
}

type Account struct {
	ID          string    `json:"id"`
	Environment string    `json:"environment"` // "demo" or "real"
	Balance     float64   `json:"balance"`
	Currency    string    `json:"currency"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Position struct {
	ID          string     `json:"id"`
	Instrument  string     `json:"instrument"`
	Units       float64    `json:"units"` // positive = long, negative = short
	OpenPrice   float64    `json:"open_price"`
	OpenTime    time.Time  `json:"open_time"`
	StopLoss    float64    `json:"stop_loss"`
	TakeProfit  float64    `json:"take_profit"`
	Status      string     `json:"status"` // "OPEN", "CLOSED"
	ClosePrice  *float64   `json:"close_price,omitempty"`
	CloseTime   *time.Time `json:"close_time,omitempty"`
	RealizedPnL *float64   `json:"realized_pnl,omitempty"`
}

type Transaction struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"` // "BUY", "SELL", "CLOSE"
	Instrument  string    `json:"instrument"`
	Price       float64   `json:"price"`
	Units       float64   `json:"units"`
	RealizedPnL float64   `json:"realized_pnl"`
	Timestamp   time.Time `json:"timestamp"`
}

type AlloraInference struct {
	TopicID       int       `json:"topic_id"`
	BlockHeight   int64     `json:"block_height"`
	CombinedValue string    `json:"combined_value"`
	ParsedValue   float64   `json:"parsed_value"`
	Timestamp     time.Time `json:"timestamp"`
}

type SystemLog struct {
	ID        int64     `json:"id"`
	Level     string    `json:"level"` // "INFO", "WARN", "ERROR"
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// InitDB opens SQLite connection and executes schema migrations
func InitDB(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Enable WAL mode for better concurrency
	if _, err := conn.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to enable WAL: %w", err)
	}

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, err
	}

	return db, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			environment TEXT NOT NULL,
			balance REAL NOT NULL,
			currency TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS positions (
			id TEXT PRIMARY KEY,
			instrument TEXT NOT NULL,
			units REAL NOT NULL,
			open_price REAL NOT NULL,
			open_time DATETIME NOT NULL,
			stop_loss REAL NOT NULL,
			take_profit REAL NOT NULL,
			status TEXT NOT NULL,
			close_price REAL,
			close_time DATETIME,
			realized_pnl REAL
		);`,
		`CREATE TABLE IF NOT EXISTS transactions (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			instrument TEXT NOT NULL,
			price REAL NOT NULL,
			units REAL NOT NULL,
			realized_pnl REAL NOT NULL,
			timestamp DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS allora_inferences (
			topic_id INTEGER NOT NULL,
			block_height INTEGER NOT NULL,
			combined_value TEXT NOT NULL,
			parsed_value REAL NOT NULL,
			timestamp DATETIME NOT NULL,
			PRIMARY KEY (topic_id, block_height)
		);`,
		`CREATE TABLE IF NOT EXISTS system_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			level TEXT NOT NULL,
			message TEXT NOT NULL,
			timestamp DATETIME NOT NULL
		);`,
	}

	for _, query := range queries {
		if _, err := db.conn.Exec(query); err != nil {
			return fmt.Errorf("failed migration query: %w", err)
		}
	}
	return nil
}

// Account operations
func (db *DB) SaveAccount(acc Account) error {
	query := `INSERT INTO accounts (id, environment, balance, currency, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			balance = excluded.balance,
			updated_at = excluded.updated_at;`
	_, err := db.conn.Exec(query, acc.ID, acc.Environment, acc.Balance, acc.Currency, acc.UpdatedAt)
	return err
}

func (db *DB) GetAccount(id string) (Account, error) {
	query := `SELECT id, environment, balance, currency, updated_at FROM accounts WHERE id = ?;`
	var acc Account
	err := db.conn.QueryRow(query, id).Scan(&acc.ID, &acc.Environment, &acc.Balance, &acc.Currency, &acc.UpdatedAt)
	if err == sql.ErrNoRows {
		return acc, fmt.Errorf("account not found")
	}
	return acc, err
}

// Position operations
func (db *DB) SavePosition(pos Position) error {
	query := `INSERT INTO positions (id, instrument, units, open_price, open_time, stop_loss, take_profit, status, close_price, close_time, realized_pnl)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			units = excluded.units,
			stop_loss = excluded.stop_loss,
			take_profit = excluded.take_profit,
			status = excluded.status,
			close_price = excluded.close_price,
			close_time = excluded.close_time,
			realized_pnl = excluded.realized_pnl;`
	_, err := db.conn.Exec(query, pos.ID, pos.Instrument, pos.Units, pos.OpenPrice, pos.OpenTime, pos.StopLoss, pos.TakeProfit, pos.Status, pos.ClosePrice, pos.CloseTime, pos.RealizedPnL)
	return err
}

func (db *DB) GetPosition(id string) (Position, error) {
	query := `SELECT id, instrument, units, open_price, open_time, stop_loss, take_profit, status, close_price, close_time, realized_pnl FROM positions WHERE id = ?;`
	var pos Position
	err := db.conn.QueryRow(query, id).Scan(&pos.ID, &pos.Instrument, &pos.Units, &pos.OpenPrice, &pos.OpenTime, &pos.StopLoss, &pos.TakeProfit, &pos.Status, &pos.ClosePrice, &pos.CloseTime, &pos.RealizedPnL)
	return pos, err
}

func (db *DB) GetOpenPositions() ([]Position, error) {
	query := `SELECT id, instrument, units, open_price, open_time, stop_loss, take_profit, status, close_price, close_time, realized_pnl FROM positions WHERE status = 'OPEN';`
	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Position
	for rows.Next() {
		var pos Position
		if err := rows.Scan(&pos.ID, &pos.Instrument, &pos.Units, &pos.OpenPrice, &pos.OpenTime, &pos.StopLoss, &pos.TakeProfit, &pos.Status, &pos.ClosePrice, &pos.CloseTime, &pos.RealizedPnL); err != nil {
			return nil, err
		}
		list = append(list, pos)
	}
	return list, nil
}

// Transaction operations
func (db *DB) SaveTransaction(tx Transaction) error {
	query := `INSERT INTO transactions (id, type, instrument, price, units, realized_pnl, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?);`
	_, err := db.conn.Exec(query, tx.ID, tx.Type, tx.Instrument, tx.Price, tx.Units, tx.RealizedPnL, tx.Timestamp)
	return err
}

func (db *DB) GetTransactions() ([]Transaction, error) {
	query := `SELECT id, type, instrument, price, units, realized_pnl, timestamp FROM transactions ORDER BY timestamp DESC;`
	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Transaction
	for rows.Next() {
		var tx Transaction
		if err := rows.Scan(&tx.ID, &tx.Type, &tx.Instrument, &tx.Price, &tx.Units, &tx.RealizedPnL, &tx.Timestamp); err != nil {
			return nil, err
		}
		list = append(list, tx)
	}
	return list, nil
}

// Allora Inference Cache
func (db *DB) SaveAlloraInference(inf AlloraInference) error {
	query := `INSERT INTO allora_inferences (topic_id, block_height, combined_value, parsed_value, timestamp)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(topic_id, block_height) DO NOTHING;`
	_, err := db.conn.Exec(query, inf.TopicID, inf.BlockHeight, inf.CombinedValue, inf.ParsedValue, inf.Timestamp)
	return err
}

func (db *DB) GetLatestInference(topicID int) (AlloraInference, error) {
	query := `SELECT topic_id, block_height, combined_value, parsed_value, timestamp FROM allora_inferences
		WHERE topic_id = ? ORDER BY timestamp DESC LIMIT 1;`
	var inf AlloraInference
	err := db.conn.QueryRow(query, topicID).Scan(&inf.TopicID, &inf.BlockHeight, &inf.CombinedValue, &inf.ParsedValue, &inf.Timestamp)
	return inf, err
}

func (db *DB) GetInferences(limit int) ([]AlloraInference, error) {
	query := `SELECT topic_id, block_height, combined_value, parsed_value, timestamp FROM allora_inferences ORDER BY timestamp DESC LIMIT ?;`
	rows, err := db.conn.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []AlloraInference
	for rows.Next() {
		var inf AlloraInference
		if err := rows.Scan(&inf.TopicID, &inf.BlockHeight, &inf.CombinedValue, &inf.ParsedValue, &inf.Timestamp); err != nil {
			return nil, err
		}
		list = append(list, inf)
	}
	return list, nil
}

// Logging
func (db *DB) Log(level, message string) {
	query := `INSERT INTO system_logs (level, message, timestamp) VALUES (?, ?, ?);`
	_, _ = db.conn.Exec(query, level, message, time.Now())
}

func (db *DB) GetLogs(limit int) ([]SystemLog, error) {
	query := `SELECT id, level, message, timestamp FROM system_logs ORDER BY id DESC LIMIT ?;`
	rows, err := db.conn.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []SystemLog
	for rows.Next() {
		var log SystemLog
		if err := rows.Scan(&log.ID, &log.Level, &log.Message, &log.Timestamp); err != nil {
			return nil, err
		}
		list = append(list, log)
	}
	return list, nil
}
