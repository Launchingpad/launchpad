// A stand-in for the app, for deploy/test-move-nginx.sh. It reads and counts
// every request body and answers JSON; the export path streams zeros.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
)

func main() {
	gib, _ := strconv.ParseInt(os.Getenv("EXPORT_GIB"), 10, 64)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/platform/move/export/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		buf := make([]byte, 256<<10)
		for n := int64(0); n < gib<<30; n += int64(len(buf)) {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int64{"bytes": n})
	})
	srv := &http.Server{Addr: ":8080", Handler: mux}
	_ = srv.ListenAndServe()
}
