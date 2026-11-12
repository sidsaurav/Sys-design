// Chapter 9.2 — one bad build, released four ways; and the judge of a canary.
//
// A model, like those of Chapters 7.2 and 7.3. A service takes 200 requests a second on ten
// copies, and one request in a thousand fails. A new build fails one request in fifty. It
// passed its tests.
//
//	part 1  the same bad build, released by a rolling deploy, by blue-green, and by a canary
//	part 2  the canary's judge: how small a fault it sees, and how often it is wrong
//	part 3  a feature flag at 5%, 25% and 50%: who is in, and do they stay in?
//
//	go run ./phase-9-shipping/code/ch02-release
//
// The random numbers come from a fixed seed, so every run prints the same thing.
package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"strings"
)

const (
	perSecond = 200   // requests a second
	normal    = 0.001 // the chance that a request fails on the current build
	copies    = 10
	budget    = 0.001 // an objective of 99.9%: one failure in a thousand is allowed
)

// failures draws how many of n requests fail, when each fails with probability p. It
// does not toss a coin for each request: it draws the gap to the next failure, which
// gives the same answers with far fewer random numbers.
func failures(r *rand.Rand, n int, p float64) int {
	f := 0
	for i := 0; ; f++ {
		i += int(math.Log(1-r.Float64())/math.Log(1-p)) + 1 // the next request that fails
		if i > n {
			return f
		}
	}
}

// A release says, for each second, what share of the traffic the new build receives.
type release struct {
	what string
	// share is called once a second. It returns the share of traffic on the new build, and
	// may look at what has happened so far to decide.
	share func(t int, seen *history) float64
}

// history is what the release's automation, and the pager, can see.
type history struct {
	fails, total       []int // each second: every request
	newFails, newTotal int   // since the release began: the new build's requests
	oldFails, oldTotal int   // …and the old build's
	paged, undone      int   // when the page fired, and when the release was undone: -1 if never
}

// burning is the page of Chapter 7.3: over the last five minutes, failures are running at
// more than 14.4 times the rate that the objective allows.
func (h *history) burning() bool {
	f, n := 0, 0
	for s := max(0, len(h.fails)-300); s < len(h.fails); s++ {
		f += h.fails[s]
		n += h.total[s]
	}
	return n > 0 && float64(f)/float64(n) > 14.4*budget
}

// worse is the canary's judge. The new build has failed at least a few requests, at more
// than some multiple of the old build's rate over the same seconds. If sigmas is not zero,
// the gap must also be that many standard errors wide: more than chance would explain.
func (h *history) worse(atLeast int, times, sigmas float64) bool {
	if h.newFails < atLeast || h.newTotal == 0 || h.oldTotal == 0 {
		return false
	}
	n1, n0 := float64(h.newTotal), float64(h.oldTotal)
	p1, p0 := float64(h.newFails)/n1, max(float64(h.oldFails), 1)/n0 // one failure at least, so that no rate is zero
	if p1 <= times*p0 {
		return false
	}
	pooled := float64(h.newFails+h.oldFails) / (n1 + n0)
	return sigmas == 0 || (p1-p0)/math.Sqrt(pooled*(1-pooled)*(1/n1+1/n0)) > sigmas
}

// run plays ten quiet minutes, and then a release of a build that fails with probability
// bad. It returns what was seen, and how many requests failed that would not have failed
// on the old build. Times are counted from the start of the release.
func run(r *rand.Rand, rel release, bad float64, seconds int) (*history, int) {
	h := &history{paged: -1, undone: -1}
	usual := 0.0 // the failures that the same requests would have had on the old build
	for t := -600; t < seconds; t++ {
		share := 0.0
		if t >= 0 {
			share = rel.share(t, h)
		}
		onNew := int(float64(perSecond)*share + 0.5)
		fNew := failures(r, onNew, bad)
		fOld := failures(r, perSecond-onNew, normal)
		h.fails = append(h.fails, fNew+fOld)
		h.total = append(h.total, perSecond)
		h.newFails, h.newTotal = h.newFails+fNew, h.newTotal+onNew
		h.oldFails, h.oldTotal = h.oldFails+fOld, h.oldTotal+perSecond-onNew
		usual += float64(onNew) * normal
		if h.paged < 0 && h.burning() {
			h.paged = t
		}
	}
	return h, h.newFails - int(usual+0.5)
}

const (
	replaceEvery = 24      // a rolling deploy replaces one copy in ten every 24 seconds: four minutes in all
	person       = 5 * 60  // from the page to the start of a rollback, when a person does it: an assumption
	canaryShare  = 0.05    // one copy's worth of traffic
	canaryFor    = 10 * 60 // how long a canary is watched before the release goes on
	judgeEvery   = 30      // how often the judge looks
)

// rolled is the share on the new build, t seconds into a rolling deploy.
func rolled(t int) float64 {
	return float64(min(copies, t/replaceEvery+1)) / copies
}

func releases() []release {
	undo := func(h *history, t int) {
		if h.undone < 0 {
			h.undone = t
		}
	}
	return []release{
		{"a rolling deploy; a person rolls it back", func(t int, h *history) float64 {
			if h.paged >= 0 && t >= h.paged+person { // the rollback is another rolling deploy
				back := t - (h.paged + person)
				if back >= copies*replaceEvery {
					undo(h, t)
					return 0
				}
				return 1 - rolled(back)
			}
			return rolled(t)
		}},
		{"blue-green; a person switches back", func(t int, h *history) float64 {
			if h.paged >= 0 && t >= h.paged+person {
				undo(h, t)
				return 0
			}
			return 1
		}},
		{"blue-green; the alert switches it back", func(t int, h *history) float64 {
			if h.paged >= 0 {
				undo(h, t)
				return 0
			}
			return 1
		}},
		{"a canary of 5%; a judge compares the two", func(t int, h *history) float64 {
			if h.undone >= 0 {
				return 0
			}
			if t > 0 && t%judgeEvery == 0 && h.worse(5, 3, 0) {
				undo(h, t)
				return 0
			}
			if t < canaryFor {
				return canaryShare
			}
			return rolled(t - canaryFor) // it passed: now the rolling deploy
		}},
	}
}

func clock(seconds int) string {
	if seconds < 0 {
		return "never"
	}
	return fmt.Sprintf("%d min %02d s", seconds/60, seconds%60)
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func part1() {
	fmt.Println("PART 1 — a build that fails 1 request in 50, where 1 in 1,000 is normal. It passed its tests.")
	fmt.Println()
	fmt.Printf("  %-42s %-14s %-12s %-13s %s\n", "how it is released", "the most users", "the page", "the release", "requests that")
	fmt.Printf("  %-42s %-14s %-12s %-13s %s\n", "", "on it", "fires at", "is undone at", "the build failed")
	for _, rel := range releases() {
		r := rand.New(rand.NewSource(92))
		most := 0.0
		wrapped := release{rel.what, func(t int, h *history) float64 {
			s := rel.share(t, h)
			most = max(most, s)
			return s
		}}
		h, extra := run(r, wrapped, 0.02, 40*60)
		fmt.Printf("  %-42s %-14s %-12s %-13s %s\n", rel.what, fmt.Sprintf("%.0f%%", 100*most), clock(h.paged), clock(h.undone), commas(extra))
	}
}

func part2() {
	fmt.Println("PART 2 — the judge. A canary takes a share of the traffic for ten minutes, and every 30 seconds a")
	fmt.Println("judge decides whether to stop it. 400 releases of each build: how many are stopped, and the usual time.")
	fmt.Println()
	judges := []struct {
		what   string
		share  float64
		times  float64
		sigmas float64
	}{
		{"5% of traffic; 3 times the old rate", 0.05, 3, 0},
		{"5% of traffic; 1.5 times", 0.05, 1.5, 0},
		{"5% of traffic; 1.5 times, beyond chance", 0.05, 1.5, 3},
		{"25% of traffic; 1.5 times, beyond chance", 0.25, 1.5, 3},
	}
	builds := []struct {
		what string
		bad  float64
	}{
		{"good: 1 in 1,000", 0.001},
		{"1 in 500", 0.002},
		{"1 in 200", 0.005},
		{"1 in 50", 0.02},
	}
	head := fmt.Sprintf("  %-42s", "the judge stops a canary that fails…")
	for _, b := range builds {
		head += fmt.Sprintf(" %-16s", b.what)
	}
	fmt.Println(strings.TrimRight(head, " "))
	for _, j := range judges {
		line := fmt.Sprintf("  %-42s", j.what)
		for _, b := range builds {
			r := rand.New(rand.NewSource(7))
			var when []int
			for trial := 0; trial < 400; trial++ {
				rel := release{"", func(t int, h *history) float64 {
					if h.undone >= 0 {
						return 0
					}
					if t > 0 && t%judgeEvery == 0 && h.worse(5, j.times, j.sigmas) {
						h.undone = t
						return 0
					}
					return j.share
				}}
				if h, _ := run(r, rel, b.bad, canaryFor+1); h.undone >= 0 {
					when = append(when, h.undone)
				}
			}
			cell := fmt.Sprintf("%3.0f%%", 100*float64(len(when))/400)
			if len(when) > 0 && b.bad > normal {
				sort.Ints(when)
				median := when[len(when)/2]
				cell += fmt.Sprintf("  in %d:%02d", median/60, median%60)
			}
			line += fmt.Sprintf(" %-16s", cell)
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
}

// in says whether a user is inside a flag's rollout: the flag's name and the user are
// hashed together, so that each flag has its own first five percent.
func in(flag, user string, percent int) bool {
	h := fnv.New32a()
	h.Write([]byte(flag + "/" + user))
	return int(h.Sum32()%100) < percent
}

func part3() {
	fmt.Println("PART 3 — a feature flag for 10,000 users, opened to 5%, then 25%, then 50%, then closed.")
	fmt.Println()
	fmt.Printf("  %-36s %7s %7s %7s %7s   %s\n", "who is in", "at 5%", "at 25%", "at 50%", "at 0%", "had it, and lost it as it grew")
	r := rand.New(rand.NewSource(3))
	type rule struct {
		what string
		in   func(user string, percent int) bool
	}
	rules := []rule{
		{"a coin is tossed on every request", func(_ string, percent int) bool { return r.Intn(100) < percent }},
		{"the user's identifier is hashed", func(user string, percent int) bool { return in("new-export", user, percent) }},
	}
	for _, rule := range rules {
		counts, lost := []int{}, 0
		was := map[string]bool{}
		for _, percent := range []int{5, 25, 50, 0} {
			n := 0
			for u := 0; u < 10000; u++ {
				user := fmt.Sprint("user-", u)
				now := rule.in(user, percent)
				if now {
					n++
				}
				if was[user] && !now && percent != 0 {
					lost++ // had the feature, and lost it as the rollout grew
				}
				was[user] = now
			}
			counts = append(counts, n)
		}
		fmt.Printf("  %-36s %7s %7s %7s %7s   %s\n", rule.what, commas(counts[0]), commas(counts[1]), commas(counts[2]), commas(counts[3]), commas(lost))
	}
}

func main() {
	part1()
	fmt.Println()
	part2()
	fmt.Println()
	part3()
}
