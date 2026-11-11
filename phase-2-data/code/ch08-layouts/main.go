// Chapter 2.8 — the same data, stored two ways.
//
// Five million orders are held in memory twice: row by row (each order's
// fields side by side, the way a transactional database lays out a table)
// and column by column (each field in an array of its own, the way an
// analytical database does). Then the same two questions are put to both:
//
//  1. "Show me order N": every field of one order.
//  2. "What is the total amount in each region?": two fields of every
//     order.
//
// Nothing differs between the two copies except the arrangement in memory.
//
// Run:
//
//	go run ./phase-2-data/code/ch08-layouts
//
// It uses about 700 MB of memory. Use -n to change the number of orders.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"time"
)

// One order, row by row: eight fields of eight bytes, 64 bytes in all.
type order struct {
	id         int64
	customerID int64
	productID  int64
	quantity   int64
	amount     int64 // in cents
	day        int64 // days since the first order
	region     int64 // 0 to 7
	status     int64 // 0 to 4
}

// The same orders, column by column: one array per field.
type columns struct {
	id, customerID, productID, quantity, amount, day, region, status []int64
}

func build(n int) ([]order, columns) {
	rng := rand.New(rand.NewSource(1))
	rows := make([]order, n)
	cols := columns{
		id: make([]int64, n), customerID: make([]int64, n), productID: make([]int64, n), quantity: make([]int64, n),
		amount: make([]int64, n), day: make([]int64, n), region: make([]int64, n), status: make([]int64, n),
	}
	for i := range rows {
		o := order{
			id:         int64(i + 1),
			customerID: rng.Int63n(500_000),
			productID:  rng.Int63n(20_000),
			quantity:   1 + rng.Int63n(5),
			amount:     100 + rng.Int63n(50_000),
			day:        int64(i) * 3650 / int64(n), // orders arrive in date order, over ten years
			region:     rng.Int63n(8),
			status:     rng.Int63n(5),
		}
		rows[i] = o
		cols.id[i], cols.customerID[i], cols.productID[i], cols.quantity[i] = o.id, o.customerID, o.productID, o.quantity
		cols.amount[i], cols.day[i], cols.region[i], cols.status[i] = o.amount, o.day, o.region, o.status
	}
	return rows, cols
}

// ---------------------------------------------------- question 1

// Fetch every field of each requested order. The checksum is there so
// that the fields really are read, and to confirm both layouts agree.

func fetchRows(rows []order, wanted []int32) (checksum int64) {
	for _, i := range wanted {
		o := &rows[i]
		checksum += o.id + o.customerID + o.productID + o.quantity + o.amount + o.day + o.region + o.status
	}
	return checksum
}

func fetchColumns(c columns, wanted []int32) (checksum int64) {
	for _, i := range wanted {
		checksum += c.id[i] + c.customerID[i] + c.productID[i] + c.quantity[i] + c.amount[i] + c.day[i] + c.region[i] + c.status[i]
	}
	return checksum
}

// ---------------------------------------------------- question 2

// SELECT region, sum(amount) FROM orders GROUP BY region

type totals [8]int64

func totalRows(rows []order) (t totals) {
	for i := range rows {
		t[rows[i].region&7] += rows[i].amount
	}
	return t
}

func totalColumns(regions, amounts []int64) (t totals) {
	for i, r := range regions {
		t[r&7] += amounts[i]
	}
	return t
}

// A column holds one kind of value, so it can be stored in the smallest
// type that fits. A region number needs one byte, not eight.
func totalCompact(regions []uint8, amounts []int32) (t totals) {
	for i, r := range regions {
		t[r&7] += int64(amounts[i])
	}
	return t
}

// ---------------------------------------------------- run-length encoding

// A run says: this value, this many times in a row.
type run struct {
	value int64
	count int32
}

func runLengthEncode(values []int64) (runs []run) {
	for _, v := range values {
		if n := len(runs); n > 0 && runs[n-1].value == v {
			runs[n-1].count++
		} else {
			runs = append(runs, run{v, 1})
		}
	}
	return runs
}

// ---------------------------------------------------- measuring

// best runs f five times and returns the shortest time and the result.
func best[T any](f func() T) (time.Duration, T) {
	var shortest time.Duration
	var result T
	for i := 0; i < 5; i++ {
		start := time.Now()
		result = f()
		if took := time.Since(start); i == 0 || took < shortest {
			shortest = took
		}
	}
	return shortest, result
}

func mb(bytes int) string { return fmt.Sprintf("%d MB", bytes/1_000_000) }

func main() {
	n := flag.Int("n", 5_000_000, "number of orders")
	flag.Parse()

	rows, cols := build(*n)
	fmt.Printf("%d orders, 8 fields each. Row by row: %s. Column by column: 8 arrays of %s.\n\n",
		*n, mb(*n*64), mb(*n*8))

	// Question 1: two million orders, picked at random.
	rng := rand.New(rand.NewSource(2))
	wanted := make([]int32, 2_000_000)
	for i := range wanted {
		wanted[i] = int32(rng.Intn(*n))
	}
	rowTime, rowSum := best(func() int64 { return fetchRows(rows, wanted) })
	colTime, colSum := best(func() int64 { return fetchColumns(cols, wanted) })
	if rowSum != colSum {
		panic("the two layouts disagree")
	}
	perFetch := func(d time.Duration) string { return fmt.Sprintf("%d ns", d.Nanoseconds()/int64(len(wanted))) }
	fmt.Println("1. Show me order N (all eight fields; average over 2,000,000 orders picked at random)")
	fmt.Printf("     row by row         %8s per order\n", perFetch(rowTime))
	fmt.Printf("     column by column   %8s per order   (%.1f times slower)\n\n",
		perFetch(colTime), float64(colTime)/float64(rowTime))

	// Question 2: one total over every order.
	compactRegion := make([]uint8, *n)
	compactAmount := make([]int32, *n)
	for i := range compactRegion {
		compactRegion[i], compactAmount[i] = uint8(cols.region[i]), int32(cols.amount[i])
	}
	rowTime, rowTotals := best(func() totals { return totalRows(rows) })
	colTime, colTotals := best(func() totals { return totalColumns(cols.region, cols.amount) })
	compactTime, compactTotals := best(func() totals { return totalCompact(compactRegion, compactAmount) })
	if rowTotals != colTotals || rowTotals != compactTotals {
		panic("the layouts disagree")
	}
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
	fmt.Println("2. Total amount in each region (two fields of every order)")
	fmt.Printf("     row by row                      %8s   reads %s\n", ms(rowTime), mb(*n*64))
	fmt.Printf("     column by column                %8s   reads %s   (%.1f times faster)\n",
		ms(colTime), mb(*n*16), float64(rowTime)/float64(colTime))
	fmt.Printf("     columns in the smallest types   %8s   reads %s   (%.1f times faster)\n\n",
		ms(compactTime), mb(*n*5), float64(rowTime)/float64(compactTime))

	// Compression: a column of similar values shrinks.
	runs := runLengthEncode(cols.day)
	fmt.Println("3. The 'day' column: orders arrive in date order, so the same day repeats many times in a row")
	fmt.Printf("     as it is            %d values, %s\n", *n, mb(*n*8))
	fmt.Printf("     run-length encoded  %d runs, %d KB   (value, how many times)\n", len(runs), len(runs)*12/1000)
}
