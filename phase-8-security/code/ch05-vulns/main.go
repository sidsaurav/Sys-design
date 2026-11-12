// Chapter 8.5 — backend vulnerabilities: input that is taken for instructions.
//
// Three features of the invoicing product, each written the way that goes wrong and the
// way that does not. Nothing is attacked except what this program starts.
//
//	part 1  "import an invoice from a URL": server-side request forgery, and four defences
//	part 2  "edit the note on an invoice": mass assignment
//	part 3  "search invoices": SQL injection, parameters, and what a limited role saves
//
//	go run ./phase-8-security/code/ch05-vulns
//
// Part 3 needs the database from Phase 2 (DATABASE_URL, as before). Without it, it is
// skipped. ssrf.go holds part 1, assign.go part 2 and inject.go part 3.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

func check(err error) {
	if err != nil {
		fmt.Println("stopped:", err)
		os.Exit(1)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	check(err)
	return hex.EncodeToString(b)
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func main() {
	part1()
	fmt.Println()
	part2()
	fmt.Println()
	part3()
}
