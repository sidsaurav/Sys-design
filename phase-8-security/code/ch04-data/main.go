// Chapter 8.4 — protecting data: on the wire, on the disk, and the keys to both.
//
// Everything here is real and local: certificates that this program makes, HTTPS servers
// on this machine, AES-256-GCM from Go's standard library, and a small key service.
// Nothing is attacked except the servers that this program starts.
//
//	part 1  TLS: is that really the ledger? One client verifies and one does not
//	part 2  mutual TLS: who is calling the ledger? A certificate for every caller
//	part 3  at rest: a snapshot of the database gets out. What can its finder read?
//	part 4  at rest: someone who can write to a table, and has no key, moves ciphertext
//
//	go run ./phase-8-security/code/ch04-data
//
// tls.go holds parts 1 and 2. atrest.go holds parts 3 and 4.
package main

import (
	"crypto/rand"
	"fmt"
	"os"
)

func check(err error) {
	if err != nil {
		fmt.Println("stopped:", err)
		os.Exit(1)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	check(err)
	return b
}

func main() {
	ours := newAuthority("the company's own authority")
	theirs := newAuthority("an authority that somebody made this morning")
	part1(ours, theirs)
	fmt.Println()
	part2(ours, theirs)
	fmt.Println()
	part3()
	fmt.Println()
	part4()
}
