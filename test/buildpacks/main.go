package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "cnb-offline-fixture") })
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}
