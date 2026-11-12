package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand"
)

// What the second document assumes about exports, and Chapter 9.8's prices and bill.
const (
	perDay      = 2000.0   // exports asked for in a day
	storedToday = 330000.0 // GB of export files kept today, after eight months of never deleting
	monthsKept  = 8.0
	outToday    = 60000.0 // GB of exports downloaded in a month
	workers     = 6.0     // running on average, where 45 run all day today
	costToday   = 18665.0 // what exports cost a month now

	copyHour = 0.10 // pounds for one copy of a service for an hour
	gbStored = 0.02 // for a gigabyte kept for the month
	gbOut    = 0.08 // for a gigabyte sent out to the internet
	hours    = 730

	// What exports use of shared things in that bill: a quarter of the main database and of its
	// backups, a fifth of the logs, metrics and traces, and the cluster's own copies in proportion.
	shared = 0.25*(6*hours*1.50+150000*gbStored) + 0.20*(579+1000+3500) + 1460*workers/(30+16+10+14+workers)
)

// sample writes invoice lines as CSV, as an export would, and returns its size and its size compressed.
func sample(lines int) (raw, zipped int) {
	what := []string{"Consulting", "Design review", "Support, monthly", "Hosting", "Training day", "Licence, annual", "Travel", "Hardware"}
	state := []string{"paid", "paid", "paid", "sent", "overdue", "draft"}
	r := rand.New(rand.NewSource(99))
	var csv bytes.Buffer
	csv.WriteString("invoice,customer,issued,description,quantity,unit_pence,tax_rate,currency,status\n")
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&csv, "inv-%07d,c-%05d,2026-%02d-%02d,\"%s, %s\",%d,%d,%d,GBP,%s\n", 1000000+i/6, r.Intn(4000), 1+r.Intn(6), 1+r.Intn(28),
			what[r.Intn(len(what))], []string{"March", "April", "May", "June"}[r.Intn(4)], 1+r.Intn(20), 100*(5+r.Intn(2500)), []int{0, 5, 20, 20}[r.Intn(4)], state[r.Intn(len(state))])
	}
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	z.Write(csv.Bytes())
	z.Close()
	return csv.Len(), out.Len()
}

func estimate() {
	monthly := storedToday / monthsKept // GB of exports made in a month
	exports := perDay * 30
	raw, zipped := sample(50000)
	measured := float64(zipped) / float64(raw)

	fmt.Println("PART 2 — the arithmetic behind the second document's cost section.")
	fmt.Println()
	fmt.Println("  what it assumes")
	fmt.Printf("  %-62s %8s\n", "exports asked for in a day", commas(perDay))
	fmt.Printf("  %-62s %8.2f\n", "gigabytes in an export before compression, on average", monthly/exports)
	fmt.Printf("  %-62s %8.2f\n", "times that a file is downloaded, on average", outToday/monthly)
	fmt.Printf("  %-62s %8.0f\n", "workers running, on average, where 45 run all day today", workers)
	fmt.Printf("  %-62s %8d\n", "days that a file is kept, where today it is kept for ever", 30)
	fmt.Println()
	fmt.Printf("  measured here: 50,000 invoice lines as CSV are %s bytes, and %s compressed: %.0f%%\n", commas(float64(raw)), commas(float64(zipped)), 100*measured)
	fmt.Println()
	fmt.Printf("  %-28s %8s %10s %8s %14s %9s %9s\n", "if real exports shrink to", "files", "downloads", "workers", "its share of", "a month", "for each")
	fmt.Printf("  %-28s %8s %10s %8s %14s %9s %9s\n", "", "kept", "", "", "shared things", "", "export")
	for _, c := range []struct {
		what  string
		ratio float64
	}{{"their full size", 1}, {"half", 0.5}, {fmt.Sprintf("%.0f%%, as the sample did", 100*measured), measured}} {
		files, down, work := monthly*c.ratio*gbStored, outToday*c.ratio*gbOut, workers*hours*copyHour
		total := files + down + work + shared
		fmt.Printf("  %-28s %8s %10s %8s %14s %9s %9.3f\n", c.what, commas(files), commas(down), commas(work), commas(shared), commas(total), total/exports)
	}
	fmt.Printf("  (today: %s a month, and %.3f for each export)\n", commas(costToday), costToday/exports)
}
