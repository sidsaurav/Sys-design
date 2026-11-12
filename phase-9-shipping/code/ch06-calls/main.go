// Chapter 9.6 — how one service finds and calls another, and where the rules for that live.
//
//	part 1  discovery: four ways for a caller to find a service whose copies come and go
//	part 2  a proxy beside every caller, one rule for all of them, and what the proxy cannot know
//
//	go run ./phase-9-shipping/code/ch06-calls
//
// Everything is real HTTP between servers that this program starts on your own machine. The
// registry and the control plane are objects inside the program, where a real system would
// run them as services of their own. It takes about four seconds.
package main

import (
	"fmt"
	"net/http"
	"time"
)

// get makes one request, on a connection of its own, and says whether an answer came back.
// A reused connection would hide failures: Go's client quietly repeats a GET when one breaks.
var plain = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}

func get(url string) bool {
	resp, err := plain.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func main() {
	part1(150 * time.Millisecond)
	fmt.Println()
	part2()
}
