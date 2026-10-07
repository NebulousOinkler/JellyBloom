package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
)

func main() {
	path := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*path)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ServerName":"` + cfg.ServerName + `","ProductName":"JellyBloom"}`))
	})

	log.Printf("JellyBloom listening on %s with %d upstreams (%s)", cfg.Listen, len(cfg.Upstreams), cfg.Merge)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
