// Part 3: a log that can show whether it has been altered.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

type entry struct {
	When, Who, What string
	Before          string // the hash of the entry before this one
	Hash            string // the hash of everything above
}

func (e entry) hash() string {
	sum := sha256.Sum256([]byte(e.Before + "|" + e.When + "|" + e.Who + "|" + e.What))
	return hex.EncodeToString(sum[:])
}

// chain fills in the two hash fields of every entry from the given one onwards. Anyone who
// can write to the log can do this: there is no secret in it.
func chain(log []entry, from int) {
	for i := from; i < len(log); i++ {
		log[i].Before = ""
		if i > 0 {
			log[i].Before = log[i-1].Hash
		}
		log[i].Hash = log[i].hash()
	}
}

// check walks the chain. head, if given, is the latest hash as it was copied to a place that
// the writer of the log cannot change.
func check(log []entry, head string) string {
	for i, e := range log {
		before := ""
		if i > 0 {
			before = log[i-1].Hash
		}
		if e.Before != before || e.Hash != e.hash() {
			return fmt.Sprintf("noticed at entry %d", i+1)
		}
	}
	if head != "" && (len(log) == 0 || log[len(log)-1].Hash != head) {
		return "noticed: the end differs"
	}
	return "not noticed"
}

func part3() {
	original := []entry{
		{When: "09:02", Who: "ana (support)", What: "viewed invoice 20417 of tenant A, for ticket 311"},
		{When: "09:15", Who: "pipeline", What: "deployed invoices, build 412"},
		{When: "10:40", Who: "omar (owner, tenant A)", What: "made mia an owner of tenant A"},
		{When: "11:05", Who: "sam (support)", What: "exported every invoice of tenant T07"},
		{When: "11:06", Who: "sam (support)", What: "downloaded that export"},
		{When: "12:30", Who: "ana (support)", What: "viewed invoice 31002 of tenant B, for ticket 318"},
		{When: "13:10", Who: "pipeline", What: "deployed invoices, build 413"},
		{When: "14:00", Who: "omar (owner, tenant A)", What: "voided invoice 1001"},
	}
	chain(original, 0)
	head := original[len(original)-1].Hash // copied elsewhere as each entry is written

	changes := []struct {
		what string
		do   func(log []entry) []entry
	}{
		{"entry 4 is edited, and nothing else", func(log []entry) []entry {
			log[3].Who = "ana (support)"
			return log
		}},
		{"the same, with every later hash redone", func(log []entry) []entry {
			log[3].Who = "ana (support)"
			chain(log, 3)
			return log
		}},
		{"entries 4 and 5 removed, hashes redone", func(log []entry) []entry {
			log = append(log[:3], log[5:]...)
			chain(log, 3)
			return log
		}},
		{"the last three entries cut off", func(log []entry) []entry {
			return log[:5]
		}},
	}
	fmt.Println("PART 3 — an audit log of eight entries. Entry 4 says that sam exported a tenant's invoices. Somebody")
	fmt.Println("who can write to the log would like it not to.")
	fmt.Println()
	fmt.Printf("  %-39s %-12s %-23s %s\n", "what is done to the log", "rows in", "rows that each hold the", "…and the latest hash")
	fmt.Printf("  %-39s %-12s %-23s %s\n", "", "a table", "hash of the row before", "is also kept elsewhere")
	for _, c := range changes {
		log := c.do(append([]entry(nil), original...))
		fmt.Printf("  %-39s %-12s %-23s %s\n", c.what, "not noticed", check(log, ""), check(log, head))
	}
}
