package main

import (
	"math"
	"math/rand"
	"sync/atomic"
	"time"
)

// A driver is a phone in a car. It reports where it is, and answers offers.
type driver struct {
	id     int
	offers chan int // rides offered to this phone
	onRide atomic.Bool
}

func (d *driver) run(c *city, seed int64, begin time.Time, stop <-chan struct{}) {
	r := rand.New(rand.NewSource(seed*1000 + int64(d.id)))
	x, y := r.Intn(grid), r.Intn(grid)
	silentAt := time.Duration(-1) // eight phones in a hundred go silent during the run, without a word
	if r.Intn(100) < 8 {
		silentAt = time.Duration(20+r.Intn(240)) * second
	}
	silent := func() bool { return silentAt >= 0 && time.Since(begin) > silentAt }
	c.index.report(d.id, x, y)
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if silent() {
				continue
			}
			if !d.onRide.Load() && r.Intn(2) == 0 { // a free driver drifts
				x, y = min(max(x+r.Intn(3)-1, 0), grid-1), min(max(y+r.Intn(3)-1, 0), grid-1)
			}
			c.index.report(d.id, x, y)
		case id := <-d.offers:
			if silent() {
				continue
			}
			go d.decide(c, id, r.Intn(100), time.Duration(1+r.Intn(3))*second, time.Duration(50+r.Intn(20))*second)
		}
	}
}

// decide is the person looking at the offer. The phone accepts for a driver who is free as
// far as the phone knows, and the phone knows only what the server has told it.
func (d *driver) decide(c *city, id, roll int, think, journey time.Duration) {
	switch {
	case roll < 8: // looks away, and never answers
		return
	case roll < 12: // says no
		time.Sleep(think)
		c.tell(id, reply{d.id, false})
		return
	case roll < 17: // says yes, after the offer has run out
		think += offerFor
	}
	time.Sleep(think)
	if !c.store.match(id, d.id) {
		return
	}
	c.tell(id, reply{d.id, true})
	d.onRide.Store(true)
	time.Sleep(journey) // to the rider, and then to where the rider is going
	d.onRide.Store(false)
	c.finish(id)
}

// tell passes a driver's answer to whoever is dispatching the ride, if anybody still is.
func (c *city) tell(id int, a reply) {
	if req, ok := c.asking.Load(id); ok {
		select {
		case req.(*request).result <- a:
		default:
		}
	}
}

// riders ask for rides: 1.6 a second in each city, and eight times that in the south for
// half a minute. Four requests in ten come from the four cells in the centre.
func (c *city) riders(seed int64, stadium bool) {
	r, begin := rand.New(rand.NewSource(100+seed)), time.Now()
	for s := 0; s < runFor; s++ {
		time.Sleep(time.Until(begin.Add(time.Duration(s) * second)))
		rate := 1.6
		if stadium && s >= 120 && s < 150 {
			rate *= 8
		}
		for n := poisson(r, rate); n > 0; n-- {
			x, y := r.Intn(grid), r.Intn(grid)
			if r.Intn(10) < 4 {
				x, y = 4+r.Intn(2), 4+r.Intn(2)
			}
			c.ask(r.Intn(1e6), x, y)
		}
	}
}

// poisson draws how many arrive in one second, when the average is mean.
func poisson(r *rand.Rand, mean float64) int {
	limit, product, n := math.Exp(-mean), r.Float64(), 0
	for product > limit {
		product *= r.Float64()
		n++
	}
	return n
}
