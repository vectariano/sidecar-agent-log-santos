package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/api/v1/logs/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var pretty map[string]interface{}
		if err := json.Unmarshal(body, &pretty); err == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			log.Printf("received JSON:\n%s", string(out))
		} else {
			log.Printf("received raw body:\n%s", string(body))
		}

		log.Printf("authorization header: %s", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	log.Println("mock server listening on :8080")
	log.Println("endpoint: POST /api/v1/logs/batch")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
