// Chapter 4.1 — the same sign-up, with its slow work done inline and done later.
//
// Signing up saves the account, sends a welcome email through an outside
// provider, resizes an avatar and tells the analytics system. Only the first
// of those has to happen before the user is answered.
//
//	part 1  one sign-up: how long the user waits
//	part 2  the email provider is down for three seconds
//	part 3  200 people sign up in one second; the provider accepts 20 emails a second
//	part 4  what was given up: the process restarts with work still waiting
//
// Everything is inside this one process, and the queue is in memory. That
// is deliberate: part 4 shows what it costs, and Chapter 4.2 replaces it.
//
//	go run ./phase-4-async/code/ch01-background          # about 25 seconds
//	go run ./phase-4-async/code/ch01-background -part 3
package main

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ the slow work

const (
	saveTime   = 5 * time.Millisecond   // inserting the account
	emailTime  = 50 * time.Millisecond  // one call to the email provider, when it is well
	avatarTime = 400 * time.Millisecond // resizing a picture
	statsTime  = 100 * time.Millisecond // telling the analytics system
	patience   = time.Second            // how long we wait for the provider before giving up
)

var errBusy = errors.New("429 from the email provider: too many requests")
var errDown = errors.New("the email provider did not answer")

// provider is somebody else's email service. It accepts a limited number of
// emails a second, and now and then it is not there at all.
type provider struct {
	mu       sync.Mutex
	perSec   float64 // 0 means no limit
	allowed  float64 // how many it would accept right now
	lastSeen time.Time
	down     atomic.Bool
	sent     atomic.Int64
}

func (p *provider) send() error {
	if p.down.Load() {
		time.Sleep(patience)
		return errDown
	}
	if p.perSec > 0 {
		p.mu.Lock()
		now := time.Now()
		p.allowed = min(p.perSec, p.allowed+now.Sub(p.lastSeen).Seconds()*p.perSec)
		p.lastSeen = now
		if p.allowed < 1 {
			p.mu.Unlock()
			return errBusy
		}
		p.allowed--
		p.mu.Unlock()
	}
	time.Sleep(emailTime)
	p.sent.Add(1)
	return nil
}

// ------------------------------------------------------------ a queue, and workers

type job struct {
	run     func() error
	waiting time.Time // since when
}

// queue is a list of jobs in this process's memory.
type queue struct {
	mu       sync.Mutex
	jobs     []job
	deepest  int
	delays   []time.Duration // from being queued to being done
	finished sync.WaitGroup
}

func (q *queue) add(run func() error) {
	q.finished.Add(1)
	q.mu.Lock()
	q.jobs = append(q.jobs, job{run, time.Now()})
	q.deepest = max(q.deepest, len(q.jobs))
	q.mu.Unlock()
}

func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// work starts workers that take jobs one at a time. A job that fails goes
// back to the end of the queue, to be tried again a little later.
func (q *queue) work(workers int, stop <-chan struct{}) {
	for w := 0; w < workers; w++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				q.mu.Lock()
				if len(q.jobs) == 0 {
					q.mu.Unlock()
					time.Sleep(2 * time.Millisecond)
					continue
				}
				j := q.jobs[0]
				q.jobs = q.jobs[1:]
				q.mu.Unlock()

				if err := j.run(); err != nil {
					time.Sleep(200 * time.Millisecond)
					q.mu.Lock()
					q.jobs = append(q.jobs, j)
					q.mu.Unlock()
					continue
				}
				q.mu.Lock()
				q.delays = append(q.delays, time.Since(j.waiting))
				q.mu.Unlock()
				q.finished.Done()
			}
		}()
	}
}

func (q *queue) longestDelay() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	var longest time.Duration
	for _, d := range q.delays {
		longest = max(longest, d)
	}
	return longest
}

// ------------------------------------------------------------ signing up, two ways

func resizeAvatar() error  { time.Sleep(avatarTime); return nil }
func tellAnalytics() error { time.Sleep(statsTime); return nil }

// inline does everything before answering.
func inline(p *provider) error {
	time.Sleep(saveTime)
	if err := p.send(); err != nil {
		return err // the user is shown an error — although the account was saved
	}
	resizeAvatar()
	tellAnalytics()
	return nil
}

// later saves the account, notes the rest as work to be done, and answers.
func later(p *provider, emails, others *queue) error {
	time.Sleep(saveTime)
	emails.add(p.send)
	others.add(resizeAvatar)
	others.add(tellAnalytics)
	return nil
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// crowd runs sign-ups at a steady rate and reports how they went.
func crowd(count int, every time.Duration, signup func() error) (failed int, median, slowest time.Duration) {
	var mu sync.Mutex
	var times []time.Duration
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			begin := time.Now()
			err := signup()
			mu.Lock()
			times = append(times, time.Since(begin))
			if err != nil {
				failed++
			}
			mu.Unlock()
		}()
		time.Sleep(every)
	}
	wg.Wait()
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return failed, times[len(times)/2], times[len(times)-1]
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — one person signs up.")
	fmt.Println()
	fmt.Printf("  %-34s %18s %26s\n", "", "the user waits", "everything is finished after")

	p := &provider{}
	begin := time.Now()
	inline(p)
	fmt.Printf("  %-34s %18s %26s\n", "all of it before answering", ms(time.Since(begin)), ms(time.Since(begin)))

	emails, others, stop := &queue{}, &queue{}, make(chan struct{})
	emails.work(1, stop)
	others.work(2, stop)
	begin = time.Now()
	later(p, emails, others)
	waited := time.Since(begin)
	emails.finished.Wait()
	others.finished.Wait()
	fmt.Printf("  %-34s %18s %26s\n", "save, queue the rest, answer", ms(waited), ms(time.Since(begin)))
	close(stop)
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — 50 people sign up over five seconds. For three of those seconds the email provider is down.")
	fmt.Println()
	fmt.Printf("  %-34s %16s %16s %16s %22s\n", "", "sign-ups failed", "median wait", "longest wait", "welcome emails sent")

	for _, deferred := range []bool{false, true} {
		p := &provider{}
		emails, others, stop := &queue{}, &queue{}, make(chan struct{})
		emails.work(4, stop)
		others.work(8, stop)
		go func() {
			time.Sleep(time.Second)
			p.down.Store(true)
			time.Sleep(3 * time.Second)
			p.down.Store(false)
		}()
		signup, name := func() error { return inline(p) }, "all of it before answering"
		if deferred {
			signup, name = func() error { return later(p, emails, others) }, "save, queue the rest, answer"
		}
		failed, median, slowest := crowd(50, 100*time.Millisecond, signup)
		emails.finished.Wait()
		note := ""
		if deferred {
			note = fmt.Sprintf("   (the last, %s late)", ms(emails.longestDelay()))
		}
		fmt.Printf("  %-34s %10d of 50 %16s %16s %14d of 50%s\n", name, failed, ms(median), ms(slowest), p.sent.Load(), note)
		close(stop)
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — 200 people sign up within one second. The email provider accepts 20 emails a second.")
	fmt.Println()
	fmt.Printf("  %-34s %16s %16s %22s\n", "", "sign-ups failed", "longest wait", "welcome emails sent")

	p := &provider{perSec: 20, allowed: 20, lastSeen: time.Now()}
	failed, _, slowest := crowd(200, 5*time.Millisecond, func() error { return inline(p) })
	fmt.Printf("  %-34s %9d of 200 %16s %15d of 200\n", "all of it before answering", failed, ms(slowest), p.sent.Load())

	// One worker, taking 50 ms an email, sends exactly 20 a second: the
	// number of workers is what keeps us inside the provider's limit.
	p = &provider{perSec: 20, allowed: 20, lastSeen: time.Now()}
	emails, others, stop := &queue{}, &queue{}, make(chan struct{})
	emails.work(1, stop)
	others.work(40, stop)
	var depths []int
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		for range 11 {
			time.Sleep(time.Second)
			depths = append(depths, emails.depth())
		}
	}()
	failed, _, slowest = crowd(200, 5*time.Millisecond, func() error { return later(p, emails, others) })
	emails.finished.Wait()
	fmt.Printf("  %-34s %9d of 200 %16s %15d of 200   (the last, %s late)\n", "save, queue the rest, answer", failed, ms(slowest), p.sent.Load(), ms(emails.longestDelay()))
	<-watching
	fmt.Println()
	fmt.Print("  emails waiting in the queue, second by second: ")
	for _, d := range depths {
		fmt.Printf(" %d", d)
	}
	fmt.Println()
	close(stop)
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — the same 200 sign-ups, all answered \"welcome!\". Three seconds later the process is restarted")
	fmt.Println("         for a deploy. The queue was in its memory.")
	fmt.Println()
	p := &provider{perSec: 20, allowed: 20, lastSeen: time.Now()}
	emails, others, stop := &queue{}, &queue{}, make(chan struct{})
	emails.work(1, stop)
	others.work(40, stop)
	failed, _, _ := crowd(200, 5*time.Millisecond, func() error { return later(p, emails, others) })
	time.Sleep(2 * time.Second)
	close(stop) // the process stops; whatever was waiting is gone
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("  sign-ups that were told they had succeeded     %4d\n", 200-failed)
	fmt.Printf("  welcome emails sent before the restart         %4d\n", p.sent.Load())
	fmt.Printf("  welcome emails that will now never be sent     %4d\n", 200-p.sent.Load())
	fmt.Printf("  record that they were owed                     none\n")
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
