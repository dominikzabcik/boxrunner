// Command stub-worker answers the calls that install, doctor and an idle
// agent make to a Worker, so scripts/e2e/run.sh needs no GitHub App. It
// serves the default config's classes and no repositories, so the agent
// opens no session. It logs each request as "METHOD PATH STATUS".
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "address to listen on")
	token := flag.String("token", "test", "the box token to accept")
	version := flag.String("version", "", "the release to name in the Boxrunner-Worker-Version header")
	flag.Parse()

	answers := map[string]string{
		"GET /v1/repos":      `{"repos":[],"classes":["boxrunner","boxrunner-light"],"paused":[]}`,
		"POST /v1/heartbeat": `{"paused":[]}`,
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		status, body := http.StatusNotFound, `{"error":"not_found","message":"the stub Worker does not serve this call"}`
		if r.Header.Get("Authorization") != "Bearer "+*token {
			status, body = http.StatusUnauthorized, `{"error":"unauthorized","message":"wrong box token"}`
		} else if answer, ok := answers[r.Method+" "+r.URL.Path]; ok {
			status, body = http.StatusOK, answer
		}
		w.Header().Set("Boxrunner-Worker-Version", *version)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
		fmt.Println(r.Method, r.URL.Path, status)
	}
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(handler)))
}
