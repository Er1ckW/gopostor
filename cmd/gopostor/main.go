package main

import (
	"log"
	"os"

	"gopostor/internal/gateway"
)

func main() {
	if err := gateway.Run(); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}
