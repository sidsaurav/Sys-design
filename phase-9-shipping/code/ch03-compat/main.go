// Chapter 9.3 — two versions of the code, and one store of data, during a rolling deploy.
//
// An invoice is stored as a JSON document. Its amount is to change from "amount", a number
// of pounds, to "amount_pence", a whole number of pence. The ten copies of the service are
// replaced one at a time, and at every step requests arrive: some create an invoice, some
// read one, some edit a note. Each request is served by whichever copy it happens to reach.
//
//	part 1  the rename, made in one deploy, and made in three steps
//	part 2  a new value for an old field, with writers first, and with readers first
//
//	go run ./phase-9-shipping/code/ch03-compat
//
// The random numbers come from a fixed seed, so every run prints the same thing.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
)

// A version is one build of the service: how it creates a row, how it reads the amount
// from one, and how it edits a note. An edit reads the whole row and stores it again, as
// most code that uses an object mapper does.
type version struct {
	name  string
	write func(pence int, note string) []byte
	read  func(row []byte) (pence int, err error)
	touch func(row []byte, note string) []byte
}

var errMissing = errors.New("no amount")

// The old build knows one field.
type rowOld struct {
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
}

// The new build, written in one go, knows only the other.
type rowNew struct {
	AmountPence int    `json:"amount_pence"`
	Note        string `json:"note"`
}

// The build in between knows both. Pointers tell a field that is absent from one that is zero.
type rowBoth struct {
	Amount      *float64 `json:"amount,omitempty"`
	AmountPence *int     `json:"amount_pence,omitempty"`
	Note        string   `json:"note"`
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var old = version{
	name:  "the old build",
	write: func(pence int, note string) []byte { return encode(rowOld{float64(pence) / 100, note}) },
	read: func(row []byte) (int, error) {
		var r rowOld
		json.Unmarshal(row, &r)
		if r.Amount == 0 {
			return 0, errMissing
		}
		return int(r.Amount*100 + 0.5), nil
	},
	touch: func(row []byte, note string) []byte {
		var r rowOld
		json.Unmarshal(row, &r) // a field that this build does not know is dropped here…
		r.Note = note
		return encode(r) // …and so is not in what it stores
	},
}

var renamed = version{
	name:  "the new build: reads and writes amount_pence",
	write: func(pence int, note string) []byte { return encode(rowNew{pence, note}) },
	read: func(row []byte) (int, error) {
		var r rowNew
		json.Unmarshal(row, &r)
		if r.AmountPence == 0 {
			return 0, errMissing
		}
		return r.AmountPence, nil
	},
	touch: func(row []byte, note string) []byte {
		var r rowNew
		json.Unmarshal(row, &r)
		r.Note = note
		return encode(r)
	},
}

// both is the expand step: it reads either field, and writes the two together.
var both = version{
	name: "the expand build: reads either, writes both",
	write: func(pence int, note string) []byte {
		pounds := float64(pence) / 100
		return encode(rowBoth{&pounds, &pence, note})
	},
	read: func(row []byte) (int, error) {
		var r rowBoth
		json.Unmarshal(row, &r)
		switch {
		case r.AmountPence != nil:
			return *r.AmountPence, nil
		case r.Amount != nil:
			return int(*r.Amount*100 + 0.5), nil
		}
		return 0, errMissing
	},
	touch: func(row []byte, note string) []byte {
		var r rowBoth
		json.Unmarshal(row, &r)
		r.complete()
		r.Note = note
		return encode(r)
	},
}

// complete gives a row whichever of the two fields it lacks.
func (r *rowBoth) complete() {
	if r.AmountPence == nil && r.Amount != nil {
		pence := int(*r.Amount*100 + 0.5)
		r.AmountPence = &pence
	}
	if r.Amount == nil && r.AmountPence != nil {
		pounds := float64(*r.AmountPence) / 100
		r.Amount = &pounds
	}
}

// migrate is the second step: every row that lacks a field is given it. It returns how
// many rows it had to change.
func migrate(s *store) int {
	changed := 0
	for i, row := range s.rows {
		var r rowBoth
		json.Unmarshal(row, &r)
		r.complete()
		if filled := encode(r); string(filled) != string(row) {
			s.rows[i] = filled
			changed++
		}
	}
	return changed
}

// A store is the rows, and beside them the truth: what each invoice's amount really is.
type store struct {
	rows  [][]byte
	truth []int
}

type outcome struct {
	wrong int // reads that returned the wrong amount, or none
	lost  int // invoices whose amount is in neither field any more
	stuck int // invoices that the build which is running cannot read
}

// unreadable counts the rows from which a build cannot get the right amount.
func (s *store) unreadable(v version) int {
	n := 0
	for i, row := range s.rows {
		if got, err := v.read(row); err != nil || got != s.truth[i] {
			n++
		}
	}
	return n
}

// serve sends n requests to copies of two builds, where newCopies of the ten are new.
func (s *store) serve(r *rand.Rand, from, to version, newCopies, n int, o *outcome) {
	for i := 0; i < n; i++ {
		v := from
		if r.Intn(10) < newCopies {
			v = to
		}
		switch k := r.Intn(10); {
		case k < 4 || len(s.rows) == 0: // create
			pence := 100 + r.Intn(100000)
			s.rows = append(s.rows, v.write(pence, "new"))
			s.truth = append(s.truth, pence)
		case k < 9: // read
			id := r.Intn(len(s.rows))
			if got, err := v.read(s.rows[id]); err != nil || got != s.truth[id] {
				o.wrong++
			}
		default: // edit the note
			id := r.Intn(len(s.rows))
			s.rows[id] = v.touch(s.rows[id], "edited")
		}
	}
}

// rollout replaces the ten copies one at a time, with requests at every step. If it is
// told to turn back, it stops when half the copies are new, and puts the old build back.
func rollout(s *store, r *rand.Rand, from, to version, turnBack bool) outcome {
	var o outcome
	for newCopies := 1; newCopies <= 10; newCopies++ {
		s.serve(r, from, to, newCopies, 1000, &o)
		if turnBack && newCopies == 5 {
			o.stuck = s.unreadable(from)
			return o
		}
	}
	o.lost = s.unreadable(both) // the expand build reads either field: what it cannot read is gone
	return o
}

// seeded is a store with 2,000 invoices that the old build wrote before anything changed.
func seeded(r *rand.Rand) *store {
	s := &store{}
	var o outcome
	s.serve(r, old, old, 0, 5000, &o)
	return s
}

func part1() {
	fmt.Println("PART 1 — \"amount\", in pounds, becomes \"amount_pence\". Ten copies are replaced one at a time, with")
	fmt.Println("1,000 requests at each step: 4 in 10 create an invoice, 5 in 10 read one, 1 in 10 edits a note.")
	fmt.Println()
	fmt.Printf("  %-48s %14s %13s %21s\n", "the change", "reads with the", "amounts lost", "after turning back")
	fmt.Printf("  %-48s %14s %13s %21s\n", "", "wrong amount", "for good", "half-way: unreadable")
	show := func(what string, o, back outcome) {
		fmt.Printf("  %-48s %14s %13s %21s\n", what, commas(o.wrong), commas(o.lost), commas(back.stuck))
	}
	// In one deploy.
	r := rand.New(rand.NewSource(93))
	o := rollout(seeded(r), r, old, renamed, false)
	r = rand.New(rand.NewSource(93))
	back := rollout(seeded(r), r, old, renamed, true)
	show("in one deploy: old build to new build", o, back)

	// In three steps. Each deploy starts from the store that the step before left.
	r = rand.New(rand.NewSource(93))
	s := seeded(r)
	rb := rand.New(rand.NewSource(93))
	o = rollout(s, r, old, both, false)
	back = rollout(seeded(rb), rb, old, both, true)
	show("in three steps: 1  expand: old to expand build", o, back)

	filled := migrate(s)
	show2 := fmt.Sprintf("                2  migrate: fill %s old rows", commas(filled))
	fmt.Printf("  %-48s %14s %13s %21s\n", show2, "—", commas(s.unreadable(both)), "—")

	half := &store{append([][]byte(nil), s.rows...), append([]int(nil), s.truth...)}
	o = rollout(s, r, both, renamed, false)
	back = rollout(half, rand.New(rand.NewSource(94)), both, renamed, true)
	show("                3  contract: expand to new build", o, back)
}

// Part 2: a status. The strict reader was written when there were two.
func strict(status string) error {
	switch status {
	case "open", "paid":
		return nil
	}
	return fmt.Errorf("unknown status %q", status)
}

// The tolerant reader shows a status that it does not know as it is, and carries on.
func tolerant(string) error { return nil }

func part2() {
	fmt.Println("PART 2 — a third status, \"refunded\", is added to a field that had two. One new invoice in ten is")
	fmt.Println("refunded. The old build's reader has a case for each status that it knows, and fails on any other.")
	fmt.Println()
	fmt.Printf("  %-58s %s\n", "the order of the deploys", "reads that failed during the rollouts")
	type build struct {
		read    func(string) error
		refunds bool // does it write the new status?
	}
	oldBuild, tolerantBuild, newBuild := build{strict, false}, build{tolerant, false}, build{tolerant, true}
	run := func(steps [][2]build) int {
		r := rand.New(rand.NewSource(95))
		statuses, failed := []string{"open", "paid"}, 0
		for _, step := range steps {
			for newCopies := 1; newCopies <= 10; newCopies++ {
				for i := 0; i < 1000; i++ {
					b := step[0]
					if r.Intn(10) < newCopies {
						b = step[1]
					}
					if r.Intn(10) < 4 { // create
						status := "open"
						if b.refunds && r.Intn(10) == 0 {
							status = "refunded"
						}
						statuses = append(statuses, status)
					} else if b.read(statuses[r.Intn(len(statuses))]) != nil {
						failed++
					}
				}
			}
		}
		return failed
	}
	fmt.Printf("  %-58s %s\n", "one deploy: the build that writes the new status", commas(run([][2]build{{oldBuild, newBuild}})))
	fmt.Printf("  %-58s %s\n", "two deploys: a tolerant reader first, then the writer", commas(run([][2]build{{oldBuild, tolerantBuild}, {tolerantBuild, newBuild}})))
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
}
