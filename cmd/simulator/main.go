// simulator is a transport-test fixture, not an inference engine.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18081", "LOCAL test fixture only")
	model := flag.String("model", "demo", "simulated model ID")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]string{"id": *model, "object": "model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&body) != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, word := range strings.Fields("[SIMULATED] edge swarm transport is working") {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(15 * time.Millisecond):
			}
			event, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": word + " "}}}})
			fmt.Fprintf(w, "data: %s\n\n", event)
			http.NewResponseController(w).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	log.Printf("SIMULATED MODEL fixture listening on %s; no GPU inference", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
