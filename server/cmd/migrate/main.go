// Command migrate runs the embedded migrations by hand.
//
//	go run ./cmd/migrate up      # apply every pending migration
//	go run ./cmd/migrate down    # roll back EVERYTHING (dev databases only)
//
// It reads DATABASE_URL. The server already migrates up on boot.
package main

import (
	"fmt"
	"os"

	"devops/internal/database"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: migrate up|down")
	}
	switch args[0] {
	case "up":
		v, err := database.Migrate(url)
		if err != nil {
			return err
		}
		fmt.Println("at version", v)
		return nil
	case "down":
		return database.MigrateDown(url)
	default:
		return fmt.Errorf("unknown command %q (want up or down)", args[0])
	}
}
