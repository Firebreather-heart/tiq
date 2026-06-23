package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "C:\\Users\\demil\\Documents\\tiq-polymarket\\backend\\trading_bot.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT timestamp, level, message FROM system_logs ORDER BY timestamp DESC LIMIT 20")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var timestamp, level, message string
		rows.Scan(&timestamp, &level, &message)
		fmt.Printf("[%s] %s: %s\n", timestamp, level, message)
	}
}
