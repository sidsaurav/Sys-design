// Capstone 5 — a chat system.
//
// Two programs in one.
//
// The default is a model of the whole design, small enough to check: thirty
// users, twenty conversations on four shards, each shard with a leader and a
// follower. For 2,000 seconds users send messages, drop off and come back.
// One push in ten is lost and one in twenty arrives twice. One confirmation
// in twenty is lost, so that the sender's device sends the message again.
// And each shard's leader is replaced twice, while the old leader lingers for
// 20 seconds and still receives some of the requests.
//
// Everything is inside one process, and time is a counter of seconds: no
// network, no database. What the model keeps is a record of what every
// sender was told, against which it checks what the servers hold and what
// every device ends up showing.
//
//	go run ./phase-5-distributed-data/code/capstone-chat
//	go run ./phase-5-distributed-data/code/capstone-chat -without cursor
//
// -without removes one safeguard: request-id, sequence, sync, fencing,
// cursor, dedup or lease.
//
// With -sql the program instead runs the one step that a model cannot vouch
// for — giving each message its position — on PostgreSQL. See sql.go.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"slices"
	"sort"
	"strings"
)

var without = map[string]bool{}

const (
	users         = 30
	conversations = 20
	shards        = 4
	seconds       = 2000
	heartbeat     = 5  // a connected device tells the presence service that it is there, this often
	lease         = 15 // and is shown as online for this long after the last time it did
	lingers       = 20 // how long a replaced leader goes on receiving requests
	lag           = 2  // -without sync: how far the follower is behind the leader
)

type message struct {
	id      string // chosen by the sender's device: the same on every attempt to send it
	intent  int    // which tap on "send" it came from. Only the checks use this
	conv    int
	sender  int
	replyTo string // the message that this one answers, if any
	stamp   int    // the time on the sender's device, whose clock may be wrong
	seq     int    // its position in the conversation, given by the shard's leader
}

// ------------------------------------------------------------ storage: a shard, its leader and its follower

// A replica is one copy of a shard's data.
type replica struct {
	log   map[int][]message // conversation → its messages, in order
	seqOf map[string]int    // message ID → the number that message was given
}

func newReplica() *replica { return &replica{log: map[int][]message{}, seqOf: map[string]int{}} }

func (r *replica) put(m message) {
	r.log[m.conv] = append(r.log[m.conv], m)
	r.seqOf[m.id] = m.seq
}

func (r *replica) clone() *replica {
	n := newReplica()
	for conv, list := range r.log {
		n.log[conv] = append([]message(nil), list...)
	}
	for id, seq := range r.seqOf {
		n.seqOf[id] = seq
	}
	return n
}

type pending struct {
	m   message
	due int
}

type shard struct {
	leader, follower *replica
	behind           []pending // -without sync: confirmed by the leader, and not yet on the follower
	old              *replica  // a replaced leader that has not found out
	oldUntil         int
}

var answeredAgain int // sends that were answered with a message already stored

// store asks a replica that believes it is the leader to store a message.
// It returns the message with its number, and whether it was confirmed.
func (s *shard) store(m message, viaOld bool, now int) (message, bool) {
	at := s.leader
	if viaOld {
		at = s.old
	}
	if seq, stored := at.seqOf[m.id]; stored {
		answeredAgain++
		m.seq = seq
		return m, true // the same answer as the first time, and nothing new is stored
	}
	m.seq = len(at.log[m.conv]) + 1
	switch {
	case viaOld:
		// A replaced leader. Its follower answers to the new leader now, and
		// refuses it; and a leader that must have its follower's agreement
		// before confirming can then confirm nothing. That refusal is the fence.
		// Without sync, the old leader never asks. Without fencing, it takes
		// the refusal for a failed follower and carries on alone.
		if !without["sync"] && !without["fencing"] {
			return m, false
		}
	case without["sync"]:
		s.behind = append(s.behind, pending{m, now + lag}) // the follower will have it shortly
	default:
		s.follower.put(m) // the follower has it before anybody is told
	}
	at.put(m)
	return m, true
}

// replace makes the follower the leader, as it is, and gives it a new follower.
func (s *shard) replace(now int) {
	s.old, s.oldUntil = s.leader, now+lingers
	s.leader, s.behind = s.follower, nil // whatever had not reached the follower is gone
	s.follower = s.leader.clone()        // copied at once, in this model. In life, in hours
}

// ------------------------------------------------------------ a user's device

type device struct {
	user   int
	online bool
	left   int               // when it dropped off
	tidy   bool              // whether it said goodbye
	backAt int               // when it will reconnect
	clock  int               // how wrong its clock is, in seconds
	beat   int               // when it last sent a heartbeat
	shown  map[int][]message // per conversation: what is on the screen, in the order it arrived
	have   map[int]int       // per conversation: every message up to this number is here
	ids    map[string]bool   // -without sequence: the messages that are here, by ID
	latest map[int]int       // -without sequence: per conversation, the latest time on any message here
}

var droppedRepeats, gapsFilled int

// receive handles one message that has reached the device, pushed or fetched.
func (d *device) receive(m message, s *shard) {
	c := m.conv
	if without["sequence"] { // no numbers: all the device can do is recognise a message it has
		if d.ids[m.id] {
			droppedRepeats++
			return
		}
		d.ids[m.id] = true
		d.shown[c] = append(d.shown[c], m)
		if t, any := d.latest[c]; !any || m.stamp > t {
			d.latest[c] = m.stamp
		}
		return
	}
	switch {
	case m.seq <= d.have[c]: // already here: a repeated push, or the push of a repeated send
		droppedRepeats++
		if without["dedup"] {
			d.shown[c] = append(d.shown[c], m)
		}
	case m.seq == d.have[c]+1: // the next one
		d.shown[c] = append(d.shown[c], m)
		d.have[c] = m.seq
	case without["cursor"]: // something is missing, and the device has no way to ask for it
		d.shown[c] = append(d.shown[c], m)
		d.have[c] = m.seq
	default: // something is missing: fetch everything after the last message that is here
		gapsFilled++
		d.catchUp(c, s)
	}
}

// catchUp fetches what the device lacks in one conversation, and says how much that was.
func (d *device) catchUp(c int, s *shard) (fetched int) {
	stored := s.leader.log[c]
	if without["sequence"] { // the only cursor there is: "later than the latest time I have"
		after, any := d.latest[c]
		for _, m := range stored {
			if !any || m.stamp > after {
				d.receive(m, s)
				fetched++
			}
		}
		return fetched
	}
	for d.have[c] < len(stored) {
		d.shown[c] = append(d.shown[c], stored[d.have[c]])
		d.have[c]++
		fetched++
	}
	return fetched
}

// screen is the conversation as the user sees it.
func (d *device) screen(c int) []message {
	if !without["sequence"] {
		return d.shown[c]
	}
	sorted := append([]message(nil), d.shown[c]...) // with no numbers, order by the senders' clocks
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].stamp < sorted[j].stamp })
	return sorted
}

// ------------------------------------------------------------ the run

func model() {
	rng := rand.New(rand.NewSource(8))
	var cluster [shards]*shard
	for i := range cluster {
		cluster[i] = &shard{leader: newReplica(), follower: newReplica()}
	}
	shardOf := func(conv int) *shard { return cluster[conv%shards] }

	members := make([][]int, conversations) // four members each
	in := make([][]int, users)              // user → the conversations they are in
	for c := range members {
		for len(members[c]) < 4 {
			if u := rng.Intn(users); !slices.Contains(members[c], u) {
				members[c] = append(members[c], u)
				in[u] = append(in[u], c)
			}
		}
	}
	devices := make([]*device, users)
	for u := range devices {
		devices[u] = &device{user: u, online: true, beat: -heartbeat,
			shown: map[int][]message{}, have: map[int]int{}, ids: map[string]bool{}, latest: map[int]int{}}
	}
	// Three of the thirty devices have clocks that are wrong, by this many seconds. The rest are right.
	devices[3].clock, devices[14].clock, devices[25].clock = -40, 25, 60

	presence := map[int]int{} // the presence service: user → shown as online until this second
	reconnect := func(d *device) (fetched int) {
		d.online = true
		if !without["cursor"] {
			for _, c := range in[d.user] {
				fetched += d.catchUp(c, shardOf(c))
			}
		}
		return fetched
	}

	confirmed := map[int]message{} // intent → the message as its sender was told it had been stored
	var attempts, refused, pushes, pushesLost, pushesTwice, dropOffs, fetchedOnReturn, heartbeats, longestGhost int
	for now := 1; now <= seconds; now++ {
		for i, s := range cluster {
			if now == 200*(i+1) || now == 200*(i+1)+1000 {
				s.replace(now)
			}
			if s.old != nil && now > s.oldUntil {
				s.old = nil
			}
			for len(s.behind) > 0 && s.behind[0].due <= now {
				s.follower.put(s.behind[0].m)
				s.behind = s.behind[1:]
			}
		}

		// Devices come and go, and the ones that are connected say so.
		for _, d := range devices {
			switch {
			case d.online && rng.Intn(100) == 0:
				dropOffs++
				d.online, d.left, d.backAt, d.tidy = false, now, now+20+rng.Intn(41), rng.Intn(2) == 0
				if without["lease"] && d.tidy {
					delete(presence, d.user) // "I am leaving" — if the device got the chance to say it
				}
			case !d.online && now >= d.backAt:
				fetchedOnReturn += reconnect(d)
				d.beat = now - heartbeat
			}
			if d.online && now >= d.beat+heartbeat {
				d.beat, presence[d.user] = now, now+lease
				heartbeats++
			}
			if until, known := presence[d.user]; !d.online && known && (without["lease"] || until > now) {
				longestGhost = max(longestGhost, now-d.left)
			}
		}

		// Two messages a second.
		for n := 0; n < 2; n++ {
			conv := rng.Intn(conversations)
			sender := devices[members[conv][rng.Intn(4)]]
			if !sender.online {
				continue
			}
			intent := len(confirmed) + 1
			m := message{id: fmt.Sprint("m", intent), intent: intent, conv: conv, sender: sender.user, stamp: now + sender.clock}
			if seen := sender.shown[conv]; len(seen) > 0 && rng.Intn(3) == 0 {
				m.replyTo = seen[len(seen)-1].id // an answer to the last message that arrived
			}
			s := shardOf(conv)
			for try := 0; ; try++ {
				attempts++
				if without["request-id"] {
					m.id = fmt.Sprint("m", intent, "/", try) // nothing connects a second attempt to the first
				}
				// While a replaced leader lingers, the first attempt of one send in three reaches it.
				stored, ok := s.store(m, s.old != nil && try == 0 && rng.Intn(3) == 0, now)
				if !ok {
					refused++
					continue // try again; the next attempt finds the new leader
				}
				for _, u := range members[conv] { // push it to the other members who are connected
					if d := devices[u]; d != sender && d.online {
						pushes++
						if rng.Intn(10) == 0 {
							pushesLost++
							continue
						}
						d.receive(stored, s)
						if rng.Intn(20) == 0 {
							pushesTwice++
							d.receive(stored, s)
						}
					}
				}
				if rng.Intn(20) == 0 {
					continue // the confirmation never reaches the sender, whose device sends again
				}
				confirmed[intent] = stored
				sender.receive(stored, s)
				break
			}
		}
	}
	for _, d := range devices { // at the end everybody is connected and has caught up
		reconnect(d)
	}

	// ---------------------------------------------------------------- the checks
	copies := map[int]int{}       // intent → how many messages the servers hold for it
	onServer := map[string]bool{} // the IDs of the messages the servers hold
	stored := 0
	for c := 0; c < conversations; c++ {
		for _, m := range shardOf(c).leader.log[c] {
			stored++
			copies[m.intent]++
			onServer[m.id] = true
		}
	}
	var storedTwice, lost, clashes, differ int
	for _, n := range copies {
		if n > 1 {
			storedTwice++
		}
	}
	type position struct{ conv, seq int }
	given := map[position]int{}
	for intent, m := range confirmed {
		if copies[intent] == 0 {
			lost++
		}
		if first, taken := given[position{m.conv, m.seq}]; taken && first != intent {
			clashes++
		}
		given[position{m.conv, m.seq}] = intent
	}
	early, missing, twice := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for c := 0; c < conversations; c++ {
		var first string
		same := true
		for i, u := range members[c] {
			at, ids := map[string]int{}, ""
			for n, m := range devices[u].screen(c) {
				if _, seen := at[m.id]; seen {
					twice[m.id] = true
				}
				at[m.id] = n
				ids += m.id + " "
			}
			for _, m := range devices[u].screen(c) {
				if n, there := at[m.replyTo]; there && n > at[m.id] {
					early[m.id] = true
				}
			}
			for _, m := range confirmed {
				if _, there := at[m.id]; m.conv == c && onServer[m.id] && !there {
					missing[m.id] = true
				}
			}
			if i == 0 {
				first = ids
			}
			same = same && ids == first
		}
		if !same {
			differ++
		}
	}

	fmt.Printf("%d seconds: %d users, %d conversations of four, %d shards. Each shard's leader was replaced twice.\n", seconds, users, conversations, shards)
	fmt.Printf("  messages sent %d, in %d attempts; the servers hold %d\n", len(confirmed), attempts, stored)
	fmt.Printf("  pushes %d, of which %d were lost and %d arrived twice\n", pushes, pushesLost, pushesTwice)
	fmt.Printf("  devices dropped off %d times; presence heartbeats %d\n\n", dropOffs, heartbeats)
	fmt.Println("WHAT THE SAFEGUARDS DID")
	for _, c := range []struct {
		name string
		n    int
	}{
		{"repeated sends answered with the message already stored", answeredAgain},
		{"sends refused because they had reached a replaced leader", refused},
		{"repeated messages that a device dropped", droppedRepeats},
		{"gaps that a device noticed, and filled by fetching", gapsFilled},
		{"messages fetched by devices when they reconnected", fetchedOnReturn},
	} {
		fmt.Printf("  %-66s %6d\n", c.name, c.n)
	}
	fmt.Println("\nCHECKS")
	for _, c := range []struct {
		name string
		n    int
	}{
		{"messages stored twice, because the send was repeated", storedTwice},
		{"messages confirmed to their sender that the servers do not hold", lost},
		{"positions given to two different messages", clashes},
		{"answers shown above the message they answer", len(early)},
		{"messages that some member's device never received", len(missing)},
		{"messages shown twice on some device", len(twice)},
		{"conversations that do not look the same on every member's device", differ},
	} {
		fmt.Printf("  %-66s %6d\n", c.name, c.n)
	}
	fmt.Printf("  %-66s %6d s\n", "longest that a user was shown as online after dropping off", longestGhost)
}

func main() {
	off := flag.String("without", "", "remove one safeguard: request-id, sequence, sync, fencing, cursor, dedup, lease; or, with -sql, counter")
	sql := flag.Bool("sql", false, "run the send path on PostgreSQL, instead of the model")
	flag.Parse()
	if *off != "" {
		valid := []string{"request-id", "sequence", "sync", "fencing", "cursor", "dedup", "lease"}
		if *sql {
			valid = []string{"counter"}
		}
		if !slices.Contains(valid, *off) {
			log.Fatalf("-without %s: here, choose one of: %s", *off, strings.Join(valid, ", "))
		}
		without[*off] = true
		fmt.Printf("WITHOUT %s\n", *off)
	}
	if *sql {
		sendPath()
		return
	}
	model()
}
