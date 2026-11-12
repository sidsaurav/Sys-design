// Capstone 3 — the read path of a photo-sharing app.
//
// Everything a read passes on its way to the data, in one program:
//
//	an edge       a CDN's cache and a load balancer; it checks the signature on image URLs
//	three apps    stateless instances of the service, each on its own port
//	a cache       shared by the instances (a stand-in for Redis)
//	object store  the image files (a stand-in for S3)
//	PostgreSQL    a real primary and two real replicas, one of them 300 ms behind
//
// The database servers are Chapter 3.6's. Start them first:
//
//	bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start
//	go run ./phase-3-scaling-out/code/capstone-photos demo     # ten requests, hop by hop
//	go run ./phase-3-scaling-out/code/capstone-photos load     # 1,200 users for 20 seconds, then the checks
//	bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop
//
// `load` takes two options. -without removes one safeguard, to show what it
// was for: read-rule, marker, lag-limit, purge or private-header. -drill
// breaks something part-way through: replica (one replica stops applying
// changes) or instance (one application instance dies).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The design's lifetimes and budgets, divided by ten so that a run of
// twenty seconds exercises them.
const speedup = 10

var (
	urlLifetime = 60 * time.Second / speedup // how long a signed image URL works
	profileTTL  = 45 * time.Second / speedup // how long a profile stays in the cache

	budgetFeed    = 60 * time.Second / speedup  // a new photo, in a follower's feed
	budgetLike    = 30 * time.Second / speedup  // a like, in the count that others see
	budgetDelete  = 60 * time.Second / speedup  // a deleted photo, still listed or still served
	budgetProfile = 120 * time.Second / speedup // a change to a profile, as others see it

	// These two are measured against real replication lag, so they are not divided.
	lagLimit  = time.Second             // a replica further behind than this is given no reads
	markerTTL = 1500 * time.Millisecond // how long a changed profile is read from the primary: longer than lagLimit

	without = map[string]bool{} // safeguards switched off by -without
)

const pageSize = 12

var ctx = context.Background()

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// ------------------------------------------------------------ the database: one primary, some replicas

type replica struct {
	name    string
	pool    *pgxpool.Pool
	applied atomic.Int64 // it has applied every change made on the primary before this moment (Unix ns)
	behind  atomic.Int64 // and so is at most this far behind (ns), as last measured
	in      atomic.Bool  // is it being given reads?
	reads   atomic.Int64
}

type database struct {
	primary              *pgxpool.Pool
	replicas             []*replica
	primaryReads, writes atomic.Int64
}

// looked is where a user's last read (or write) went, and when.
type looked struct {
	source string
	at     time.Time
}

// reader chooses where one user's read goes (Chapter 3.6). Each user has a
// replica of their own. One rule keeps them from ever seeing the past: a
// user may move to a different server only if it has caught up with the
// moment they last looked. A write counts as a look at the primary, so the
// same rule shows users their own changes.
func (d *database) reader(user int, c *cache) (*pgxpool.Pool, string) {
	last, _ := c.get(lookedKey(user)).(looked)
	mayUse := func(r *replica) bool {
		return without["read-rule"] || last.source == "" || last.source == r.name || r.applied.Load() >= last.at.UnixNano()
	}
	var usable []*replica
	for _, r := range d.replicas {
		if r.in.Load() {
			usable = append(usable, r)
		}
	}
	if len(usable) == 0 {
		d.primaryReads.Add(1)
		return d.primary, "primary (no replica is within the lag limit)"
	}
	own := usable[user%len(usable)]
	if !mayUse(own) {
		own = nil
		for _, r := range usable {
			if r.name == last.source { // stay where they were
				own = r
			}
		}
	}
	if own == nil {
		d.primaryReads.Add(1)
		return d.primary, "primary (this user's replica has not caught up with what they last saw)"
	}
	own.reads.Add(1)
	return own.pool, "replica " + own.name
}

// saw records where a user's read went, once it has been answered.
func (d *database) saw(user int, pool *pgxpool.Pool, c *cache) {
	source := "primary"
	for _, r := range d.replicas {
		if r.pool == pool {
			source = r.name
		}
	}
	c.set(lookedKey(user), looked{source, time.Now()}, 5*time.Second)
}

// watchLag writes the time on the primary ten times a second, and reads it
// back on each replica. A replica showing an old time is that far behind.
// (Comparing times is sound here because everything shares one clock. Across
// machines, compare positions in the log, as Chapter 3.6 did.)
func (d *database) watchLag() {
	for {
		d.primary.Exec(ctx, "UPDATE ps_heartbeat SET at = clock_timestamp()")
		for _, r := range d.replicas {
			var at time.Time
			if r.pool.QueryRow(ctx, "SELECT at FROM ps_heartbeat").Scan(&at) != nil {
				continue
			}
			r.applied.Store(at.UnixNano())
			r.behind.Store(int64(time.Since(at)))
			r.in.Store(time.Since(at) <= lagLimit || without["lag-limit"])
		}
		time.Sleep(100 * time.Millisecond)
	}
}

const schema = `
DROP TABLE IF EXISTS ps_likes, ps_photos, ps_follows, ps_users, ps_heartbeat;
CREATE TABLE ps_users   (id int PRIMARY KEY, private boolean NOT NULL);
CREATE TABLE ps_follows (follower_id int NOT NULL, followee_id int NOT NULL, PRIMARY KEY (follower_id, followee_id));
CREATE INDEX ps_followers ON ps_follows (followee_id);
CREATE TABLE ps_photos  (id bigserial PRIMARY KEY, author_id int NOT NULL, likes int NOT NULL DEFAULT 0,
                         created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX ps_photos_by_author ON ps_photos (author_id, id DESC);
CREATE TABLE ps_likes   (photo_id bigint NOT NULL REFERENCES ps_photos ON DELETE CASCADE, user_id int NOT NULL,
                         PRIMARY KEY (photo_id, user_id));
CREATE TABLE ps_heartbeat (at timestamptz NOT NULL);
INSERT INTO ps_heartbeat VALUES (clock_timestamp());`

// The feed: the newest photos by the people this user follows. For each of
// them the index gives their newest few; the newest of all those are the page.
const feedSQL = `
SELECT p.id, p.author_id, p.likes
FROM ps_follows f
CROSS JOIN LATERAL (SELECT id, author_id, likes FROM ps_photos
                    WHERE author_id = f.followee_id ORDER BY id DESC LIMIT $2) p
WHERE f.follower_id = $1
ORDER BY p.id DESC LIMIT $2`

// A like: one statement, so that the count and the row cannot disagree (Chapter 2.4).
const likeSQL = `
WITH new AS (INSERT INTO ps_likes (photo_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING 1)
UPDATE ps_photos SET likes = likes + 1 WHERE id = $1 AND EXISTS (SELECT 1 FROM new)`

// ------------------------------------------------------------ the cache the instances share

type cache struct {
	mu           sync.Mutex
	items        map[string]item
	loading      map[string]chan struct{}
	deleted      map[string]int // how many times each key has been deleted
	hits, misses atomic.Int64
}

type item struct {
	value   any
	expires time.Time
}

func newCache() *cache {
	return &cache{items: map[string]item{}, loading: map[string]chan struct{}{}, deleted: map[string]int{}}
}

func (c *cache) get(key string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it, ok := c.items[key]; ok && time.Now().Before(it.expires) {
		return it.value
	}
	return nil
}

func (c *cache) has(key string) bool { return c.get(key) != nil }

func (c *cache) set(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = item{value, time.Now().Add(ttl)}
}

func (c *cache) del(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	c.deleted[key]++
}

// load returns what is cached under the key. If nothing is, it calls fetch —
// once, however many requests are waiting for the same key (Chapter 3.4) —
// and keeps the result. It does not keep it if the key was deleted while
// fetch was running: what fetch read may be from before the change (the
// lease of Chapter 3.3).
func (c *cache) load(key string, ttl time.Duration, fetch func() (any, error)) (any, bool, error) {
	for {
		c.mu.Lock()
		if it, ok := c.items[key]; ok && time.Now().Before(it.expires) {
			c.mu.Unlock()
			c.hits.Add(1)
			return it.value, true, nil
		}
		if busy, ok := c.loading[key]; ok {
			c.mu.Unlock()
			<-busy // someone is already fetching it: wait, then look again
			continue
		}
		done := make(chan struct{})
		c.loading[key] = done
		deletions := c.deleted[key]
		c.mu.Unlock()

		c.misses.Add(1)
		value, err := fetch()
		c.mu.Lock()
		if err == nil && c.deleted[key] == deletions {
			c.items[key] = item{value, time.Now().Add(ttl)}
		}
		delete(c.loading, key)
		close(done)
		c.mu.Unlock()
		return value, false, err
	}
}

func lookedKey(user int) string    { return fmt.Sprint("looked:", user) }            // where this user last read, and when
func profileKey(author int) string { return fmt.Sprint("profile:", author) }         // a cached profile
func markerKey(author int) string  { return fmt.Sprint("changed:profile:", author) } // that profile changed a moment ago

// ------------------------------------------------------------ object storage, and signed URLs

type objects struct {
	mu    sync.Mutex
	have  map[int64]bool
	reads atomic.Int64
}

func (o *objects) put(id int64) { o.mu.Lock(); o.have[id] = true; o.mu.Unlock() }
func (o *objects) del(id int64) { o.mu.Lock(); delete(o.have, id); o.mu.Unlock() }
func (o *objects) get(id int64) ([]byte, bool) {
	o.reads.Add(1)
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.have[id] {
		return nil, false
	}
	return []byte(strings.Repeat(fmt.Sprintf("photo %d. ", id), 300)), true // a few kilobytes of "image"
}

var signingKey = []byte("known to the application and to the edge")

func sign(id int64, expires int64) string {
	mac := hmac.New(sha256.New, signingKey)
	fmt.Fprintf(mac, "%d|%d", id, expires)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// imageURL is what the application hands out: good for a short time only.
func imageURL(id int64) string {
	expires := time.Now().Add(urlLifetime).UnixMilli()
	return fmt.Sprintf("/img/%d?expires=%d&sig=%s", id, expires, sign(id, expires))
}

// ------------------------------------------------------------ the service: three identical instances

type photo struct {
	ID     int64  `json:"id"`
	Author int    `json:"author"`
	Likes  int    `json:"likes"`
	URL    string `json:"url"`
}

type feedPage struct {
	User   int     `json:"user"`
	Photos []photo `json:"photos"`
}

type profile struct {
	Author    int     `json:"author"`
	Private   bool    `json:"private"`
	Followers int     `json:"followers"`
	Count     int     `json:"count"`
	Photos    []photo `json:"photos"`
}

// Everything an instance uses is shared. Nothing lives in the instance (Chapter 3.1).
type service struct {
	db      *database
	cache   *cache
	objects *objects
	edge    *edge
}

type instance struct {
	*service
	name string
	addr string
	srv  *http.Server
}

func startInstance(s *service, name string) *instance {
	in := &instance{service: s, name: name}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /feed", in.feed)
	mux.HandleFunc("GET /profile", in.profile)
	mux.HandleFunc("GET /img/{id}", in.image)
	mux.HandleFunc("POST /upload", in.upload)
	mux.HandleFunc("POST /like", in.like)
	mux.HandleFunc("POST /delete", in.remove)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	in.addr, in.srv = ln.Addr().String(), &http.Server{Handler: mux}
	go in.srv.Serve(ln)
	return in
}

// who says which user is asking. A real service reads a session cookie here.
func who(r *http.Request) int {
	user, _ := strconv.Atoi(r.Header.Get("X-User"))
	return user
}

func withURLs(photos []photo) []photo {
	out := make([]photo, len(photos))
	for i, p := range photos {
		p.URL = imageURL(p.ID)
		out[i] = p
	}
	return out
}

func (in *instance) reply(w http.ResponseWriter, path string, body any) {
	w.Header().Set("X-Path", in.name+" > "+path)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}

// The feed is different for every user and is asked for once, so it is not
// cached anywhere. It is read from a replica.
func (in *instance) feed(w http.ResponseWriter, r *http.Request) {
	user := who(r)
	pool, from := in.db.reader(user, in.cache)
	rows, err := pool.Query(r.Context(), feedSQL, user, pageSize)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	photos, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (p photo, err error) {
		return p, row.Scan(&p.ID, &p.Author, &p.Likes)
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	in.db.saw(user, pool, in.cache)
	w.Header().Set("Cache-Control", "private, no-store")
	if without["private-header"] {
		w.Header().Set("Cache-Control", "public, max-age=2") // the mistake of Chapter 3.5
	}
	in.reply(w, from, feedPage{User: user, Photos: withURLs(photos)})
}

func loadProfile(c context.Context, pool *pgxpool.Pool, author int) (p profile, err error) {
	p.Author = author
	err = pool.QueryRow(c, `SELECT private,
	                               (SELECT count(*) FROM ps_follows WHERE followee_id = $1),
	                               (SELECT count(*) FROM ps_photos WHERE author_id = $1)
	                        FROM ps_users WHERE id = $1`, author).Scan(&p.Private, &p.Followers, &p.Count)
	if err != nil {
		return p, err
	}
	rows, err := pool.Query(c, "SELECT id FROM ps_photos WHERE author_id = $1 ORDER BY id DESC LIMIT $2", author, pageSize)
	if err != nil {
		return p, err
	}
	p.Photos, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ph photo, err error) {
		ph.Author = author
		return ph, row.Scan(&ph.ID)
	})
	return p, err
}

// A profile is the same for everyone who may see it, and popular ones are
// asked for constantly: it is cached, and the entry is deleted when its
// owner changes anything.
func (in *instance) profile(w http.ResponseWriter, r *http.Request) {
	viewer := who(r)
	author, _ := strconv.Atoi(r.URL.Query().Get("of"))
	var path string
	cached, hit, err := in.cache.load(profileKey(author), profileTTL, func() (any, error) {
		// If this profile has just changed, a replica may not have heard yet,
		// and what it says would sit in the cache for everyone: ask the primary.
		if in.cache.has(markerKey(author)) {
			in.db.primaryReads.Add(1)
			path = "cache miss > primary (this profile changed a moment ago)"
			return loadProfile(r.Context(), in.db.primary, author)
		}
		pool, from := in.db.reader(viewer, in.cache)
		path = "cache miss > " + from
		p, err := loadProfile(r.Context(), pool, author)
		in.db.saw(viewer, pool, in.cache)
		return p, err
	})
	if hit {
		path = "cache hit"
	}
	if err != nil {
		http.Error(w, "no such user", 404)
		return
	}
	p := cached.(profile)
	if p.Private && viewer != author {
		pool, _ := in.db.reader(viewer, in.cache)
		var follows bool
		pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM ps_follows WHERE follower_id = $1 AND followee_id = $2)",
			viewer, author).Scan(&follows)
		if !follows {
			w.Header().Set("X-Path", in.name+" > "+path+" > refused: a private account")
			http.Error(w, "this account is private", 403)
			return
		}
	}
	p.Photos = withURLs(p.Photos)
	w.Header().Set("Cache-Control", "private, no-store")
	in.reply(w, path, p)
}

// image is the origin for image files. Only the edge calls it.
func (in *instance) image(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	w.Header().Set("X-Path", in.name+" > object storage")
	body, ok := in.objects.get(id)
	if !ok {
		http.Error(w, "no such photo", 404)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // the file behind an ID never changes
	w.Write(body)
}

// changing is called before a user changes their profile, and changed after:
// a marker first, then the write, then the cached copy is deleted.
func (in *instance) changing(author int) {
	if !without["marker"] {
		in.cache.set(markerKey(author), true, markerTTL)
	}
}

func (in *instance) changed(author int) {
	in.cache.del(profileKey(author))
	in.wrote(author)
}

// wrote notes that a user has just written to the primary.
func (in *instance) wrote(user int) {
	in.db.writes.Add(1)
	in.db.saw(user, in.db.primary, in.cache)
}

func (in *instance) upload(w http.ResponseWriter, r *http.Request) {
	user := who(r)
	in.changing(user)
	var id int64
	if err := in.db.primary.QueryRow(r.Context(), "INSERT INTO ps_photos (author_id) VALUES ($1) RETURNING id", user).Scan(&id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	in.objects.put(id)
	in.changed(user)
	fmt.Fprint(w, id)
}

func (in *instance) like(w http.ResponseWriter, r *http.Request) {
	user := who(r)
	id, _ := strconv.ParseInt(r.URL.Query().Get("photo"), 10, 64)
	tag, err := in.db.primary.Exec(r.Context(), likeSQL, id, user)
	if err != nil {
		http.Error(w, "no such photo", 404) // deleted in the meantime
		return
	}
	in.wrote(user)
	fmt.Fprint(w, tag.RowsAffected()) // 1 if this was a new like, 0 if the user had liked it already
}

func (in *instance) remove(w http.ResponseWriter, r *http.Request) {
	user := who(r)
	id, _ := strconv.ParseInt(r.URL.Query().Get("photo"), 10, 64)
	in.changing(user)
	if _, err := in.db.primary.Exec(r.Context(), "DELETE FROM ps_photos WHERE id = $1 AND author_id = $2", id, user); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	in.objects.del(id)
	in.changed(user)
	if !without["purge"] {
		in.edge.purge(fmt.Sprintf("/img/%d", id))
	}
}

// ------------------------------------------------------------ the edge: a CDN's cache and a load balancer

type stored struct {
	status  int
	path    string
	body    []byte
	expires time.Time
}

type edge struct {
	addr      string
	instances []*instance
	down      []atomic.Int64 // per instance: not to be tried before this time (Unix ms)
	turn      atomic.Int64
	mu        sync.Mutex
	kept      map[string]stored
}

var backend = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}, Timeout: 5 * time.Second}

func startEdge(instances []*instance) *edge {
	e := &edge{instances: instances, down: make([]atomic.Int64, len(instances)), kept: map[string]stored{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	e.addr = ln.Addr().String()
	go http.Serve(ln, e)
	return e
}

func (e *edge) purge(key string) {
	e.mu.Lock()
	delete(e.kept, key)
	e.mu.Unlock()
}

func (e *edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.RequestURI()
	if strings.HasPrefix(r.URL.Path, "/img/") {
		// An image URL is checked here, at the edge, on every request. The
		// file is then kept under its path alone, so that everyone who is
		// allowed to see it shares one copy.
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/img/"), 10, 64)
		expires, _ := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
		if time.Now().UnixMilli() > expires || !hmac.Equal([]byte(sign(id, expires)), []byte(r.URL.Query().Get("sig"))) {
			w.Header().Set("X-Path", "edge: refused, the URL has expired or was not signed by us")
			http.Error(w, "forbidden", 403)
			return
		}
		key = r.URL.Path
	}
	if r.Method == "GET" {
		e.mu.Lock()
		hit, ok := e.kept[key]
		e.mu.Unlock()
		if ok && time.Now().Before(hit.expires) {
			w.Header().Set("X-Path", "edge HIT")
			w.WriteHeader(hit.status)
			w.Write(hit.body)
			return
		}
	}

	// Not in the edge's cache: pass it to an instance. If that instance does
	// not answer at all, try the next — but only for requests that are safe to repeat.
	repeatable := r.Method == "GET" || r.URL.Path == "/like" || r.URL.Path == "/delete"
	for attempt := 0; attempt < len(e.instances); attempt++ {
		i := int(e.turn.Add(1)) % len(e.instances)
		if time.Now().UnixMilli() < e.down[i].Load() {
			continue
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, "http://"+e.instances[i].addr+r.URL.RequestURI(), nil)
		req.Header.Set("X-User", r.Header.Get("X-User"))
		resp, err := backend.Do(req)
		if err != nil {
			e.down[i].Store(time.Now().Add(2 * time.Second).UnixMilli()) // leave it alone for a while
			if repeatable {
				continue
			}
			break
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		path := "edge MISS > " + resp.Header.Get("X-Path")
		if !strings.HasPrefix(r.URL.Path, "/img/") {
			path = "edge (not for keeping) > " + resp.Header.Get("X-Path")
		}
		control := resp.Header.Get("Cache-Control")
		if r.Method == "GET" && resp.StatusCode == 200 && strings.Contains(control, "public") {
			life := time.Hour
			if _, after, ok := strings.Cut(control, "max-age="); ok {
				seconds, _ := strconv.Atoi(strings.SplitN(after, ",", 2)[0])
				life = time.Duration(seconds) * time.Second
			}
			e.mu.Lock()
			e.kept[key] = stored{resp.StatusCode, path, body, time.Now().Add(life)}
			e.mu.Unlock()
		}
		w.Header().Set("X-Path", path)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}
	http.Error(w, "no instance answered", 502)
}

// ------------------------------------------------------------ setting up

type system struct {
	*service
	instances []*instance
	truth     *truth
}

func connect(env, fallback string) *pgxpool.Pool {
	url := os.Getenv(env)
	if url == "" {
		url = fallback
	}
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the servers first:  bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start", url, err)
	}
	return pool
}

func setUp(users, following, photosEach int) *system {
	db := &database{primary: connect("PRIMARY_URL", "postgres://course@127.0.0.1:54350/postgres?pool_max_conns=24")}
	db.replicas = []*replica{
		{name: "A", pool: connect("FAST_REPLICA_URL", "postgres://course@127.0.0.1:54351/postgres?pool_max_conns=24")},
		{name: "B", pool: connect("SLOW_REPLICA_URL", "postgres://course@127.0.0.1:54352/postgres?pool_max_conns=24")},
	}
	for _, r := range db.replicas {
		r.pool.Exec(ctx, "SELECT pg_wal_replay_resume()") // in case an earlier run was interrupted
	}
	_, err := db.primary.Exec(ctx, schema)
	must(err)

	// Users, who follows whom, and some photos to begin with.
	rng := rand.New(rand.NewSource(1))
	t := newTruth()
	var userRows, followRows, photoRows [][]any
	for u := 1; u <= users; u++ {
		t.private[u] = u%10 == 0
		userRows = append(userRows, []any{u, t.private[u]})
		for len(t.following[u]) < following {
			if v := 1 + rng.Intn(users); v != u && !t.follows(u, v) {
				t.following[u] = append(t.following[u], v)
				t.followers[v] = append(t.followers[v], u)
				followRows = append(followRows, []any{u, v})
			}
		}
	}
	for i := 0; i < users*photosEach; i++ {
		photoRows = append(photoRows, []any{1 + rng.Intn(users)})
	}
	copyIn := func(table string, columns []string, rows [][]any) {
		_, err := db.primary.CopyFrom(ctx, pgx.Identifier{table}, columns, pgx.CopyFromRows(rows))
		must(err)
	}
	copyIn("ps_users", []string{"id", "private"}, userRows)
	copyIn("ps_follows", []string{"follower_id", "followee_id"}, followRows)
	copyIn("ps_photos", []string{"author_id"}, photoRows)
	_, err = db.primary.Exec(ctx, "ANALYZE ps_users, ps_follows, ps_photos")
	must(err)

	s := &service{db: db, cache: newCache(), objects: &objects{have: map[int64]bool{}}}
	rows, err := db.primary.Query(ctx, "SELECT id, author_id FROM ps_photos ORDER BY id")
	must(err)
	for rows.Next() {
		var id int64
		var author int
		must(rows.Scan(&id, &author))
		s.objects.put(id)
		t.byAuthor[author] = append(t.byAuthor[author], id)
		t.born[id] = time.Now()
	}

	// Wait until both replicas have all of that, then start watching their lag.
	var position string
	must(db.primary.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&position))
	for _, r := range db.replicas {
		for ok := false; !ok; time.Sleep(20 * time.Millisecond) {
			must(r.pool.QueryRow(ctx, "SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn", position).Scan(&ok))
		}
	}
	go db.watchLag()

	sys := &system{service: s, truth: t}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		sys.instances = append(sys.instances, startInstance(s, name))
	}
	s.edge = startEdge(sys.instances)
	time.Sleep(600 * time.Millisecond) // let the lag be measured a few times
	return sys
}

// ------------------------------------------------------------ what is true, and what users were shown

// truth is the test's own record of what has happened, kept apart from the
// system, so that every answer the system gives can be checked against it.
type truth struct {
	mu        sync.Mutex
	private   map[int]bool
	following map[int][]int
	followers map[int][]int
	byAuthor  map[int][]int64       // each user's photos that still exist, oldest first
	born      map[int64]time.Time   // when each photo's upload was confirmed
	gone      map[int64]time.Time   // when each deleted photo's deletion was confirmed
	liked     map[int64][]time.Time // when each like of a photo was confirmed
	newest    map[int]int64         // the newest photo each user has been shown in their feed

	worst                                                       [5]time.Duration // the stalest answer seen, per budget
	late                                                        int              // answers staler than their budget
	ownMissing, backwards, leaked, wrongUser, failed, expiredOK int
	answers                                                     map[string]map[string]int // where each kind of read was answered
	feedTimes                                                   []time.Duration
}

const (
	newPhoto = iota
	likeCount
	deletedListed
	deletedServed
	profileChange
)

var budgets = [5]time.Duration{budgetFeed, budgetLike, budgetDelete, budgetDelete, budgetProfile}

func newTruth() *truth {
	return &truth{private: map[int]bool{}, following: map[int][]int{}, followers: map[int][]int{},
		byAuthor: map[int][]int64{}, born: map[int64]time.Time{}, gone: map[int64]time.Time{},
		liked: map[int64][]time.Time{}, newest: map[int]int64{}, answers: map[string]map[string]int{}}
}

func (t *truth) follows(u, v int) bool {
	for _, f := range t.following[u] {
		if f == v {
			return true
		}
	}
	return false
}

// stale records that an answer was this far out of date.
func (t *truth) stale(kind int, by time.Duration) {
	t.worst[kind] = max(t.worst[kind], by)
	if by > budgets[kind] {
		t.late++
	}
}

func (t *truth) answered(kind, path string) {
	where := "other"
	for _, w := range []string{"edge HIT", "cache hit", "primary", "replica", "object storage", "refused"} {
		if strings.Contains(path, w) {
			where = w
			break
		}
	}
	if t.answers[kind] == nil {
		t.answers[kind] = map[string]int{}
	}
	t.answers[kind][where]++
}

// checkFeed compares a feed that a user was shown with what is true.
func (t *truth) checkFeed(user int, page feedPage, received time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if page.User != user {
		t.wrongUser++ // somebody else's feed
		return
	}
	var should []int64 // the newest photos of the people this user follows
	for _, author := range t.following[user] {
		own := t.byAuthor[author]
		should = append(should, own[max(0, len(own)-pageSize):]...)
	}
	sort.Slice(should, func(i, j int) bool { return should[i] > should[j] })
	should = should[:min(pageSize, len(should))]

	shown := map[int64]bool{}
	for _, p := range page.Photos {
		shown[p.ID] = true
		if at, deleted := t.gone[p.ID]; deleted {
			t.stale(deletedListed, received.Sub(at))
		}
		if likes := t.liked[p.ID]; p.Likes < len(likes) {
			t.stale(likeCount, received.Sub(likes[p.Likes])) // the oldest like that is not in the count
		}
	}
	// A photo is missing if it should have come before something that is shown.
	var oldestShown int64
	if len(page.Photos) == pageSize {
		oldestShown = page.Photos[pageSize-1].ID
	}
	for _, id := range should {
		if !shown[id] && id > oldestShown {
			t.stale(newPhoto, received.Sub(t.born[id]))
		}
	}
	// Has this user been shown something newer before, which is now missing?
	if before := t.newest[user]; len(page.Photos) > 0 && page.Photos[0].ID < before {
		if _, deleted := t.gone[before]; !deleted {
			t.backwards++
		}
	}
	if len(page.Photos) > 0 {
		t.newest[user] = max(t.newest[user], page.Photos[0].ID)
	}
}

// checkProfile does the same for a profile. asked is when the request was sent.
func (t *truth) checkProfile(viewer, author, status int, p profile, asked, received time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.private[author] && viewer != author && !t.follows(viewer, author) {
		if status != 403 {
			t.leaked++
		}
		return
	}
	shown := map[int64]bool{}
	for _, ph := range p.Photos {
		shown[ph.ID] = true
		if at, deleted := t.gone[ph.ID]; deleted && viewer != author {
			t.stale(deletedListed, received.Sub(at))
		}
	}
	var oldestShown int64
	if len(p.Photos) == pageSize {
		oldestShown = p.Photos[pageSize-1].ID
	}
	own := t.byAuthor[author]
	for _, id := range own[max(0, len(own)-pageSize):] {
		if shown[id] || id < oldestShown {
			continue
		}
		if viewer == author && t.born[id].Before(asked) {
			t.ownMissing++ // the owner must see their own photo at once, with no allowance
		} else if viewer != author {
			t.stale(profileChange, received.Sub(t.born[id]))
		}
	}
}

// ------------------------------------------------------------ a user

type client struct {
	sys  *system
	user int
	rng  *rand.Rand
	feed []photo // the last feed this user was shown
	old  string  // an image URL from the first feed, kept until long after it has expired
	oldT time.Time
}

var browser = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 400}, Timeout: 5 * time.Second}

func (c *client) do(method, target string) (status int, path string, body []byte) {
	req, _ := http.NewRequest(method, "http://"+c.sys.edge.addr+target, nil)
	req.Header.Set("X-User", strconv.Itoa(c.user))
	resp, err := browser.Do(req)
	if err != nil {
		return 0, "", nil
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Path"), body
}

func (c *client) failed() {
	c.sys.truth.mu.Lock()
	c.sys.truth.failed++
	c.sys.truth.mu.Unlock()
}

func (c *client) loadFeed() (page feedPage, path string) {
	asked := time.Now()
	status, path, body := c.do("GET", "/feed")
	received := time.Now()
	if status != 200 || json.Unmarshal(body, &page) != nil {
		c.failed()
		return page, path
	}
	t := c.sys.truth
	t.checkFeed(c.user, page, received)
	t.mu.Lock()
	t.answered("feeds", path)
	t.feedTimes = append(t.feedTimes, received.Sub(asked))
	t.mu.Unlock()
	if page.User == c.user {
		c.feed = page.Photos
		if c.old == "" && len(page.Photos) > 0 {
			c.old, c.oldT = page.Photos[0].URL, received
		}
	}
	return page, path
}

func (c *client) loadProfile(author int) (p profile, path string) {
	asked := time.Now()
	status, path, body := c.do("GET", fmt.Sprint("/profile?of=", author))
	received := time.Now()
	if status != 200 && status != 403 {
		c.failed()
		return p, path
	}
	json.Unmarshal(body, &p)
	t := c.sys.truth
	t.checkProfile(c.user, author, status, p, asked, received)
	t.mu.Lock()
	t.answered("profiles", path)
	t.mu.Unlock()
	return p, path
}

func (c *client) loadImage(id int64, url string) (status int, path string) {
	status, path, _ = c.do("GET", url)
	received := time.Now()
	t := c.sys.truth
	t.mu.Lock()
	defer t.mu.Unlock()
	switch at, deleted := t.gone[id]; {
	case status == 200 && deleted:
		t.stale(deletedServed, received.Sub(at))
	case status == 200 || status == 403 || status == 404:
	default:
		t.failed++
	}
	t.answered("images", path)
	return status, path
}

func (c *client) upload() int64 {
	status, _, body := c.do("POST", "/upload")
	id, err := strconv.ParseInt(string(body), 10, 64)
	if status != 200 || err != nil {
		c.failed()
		return 0
	}
	t := c.sys.truth
	t.mu.Lock()
	t.born[id] = time.Now()
	t.byAuthor[c.user] = append(t.byAuthor[c.user], id)
	t.mu.Unlock()
	return id
}

func (c *client) like(id int64) (isNew bool) {
	status, _, body := c.do("POST", fmt.Sprint("/like?photo=", id))
	if status != 200 && status != 404 {
		c.failed()
	}
	if status != 200 || string(body) != "1" {
		return false
	}
	t := c.sys.truth
	t.mu.Lock()
	t.liked[id] = append(t.liked[id], time.Now())
	t.mu.Unlock()
	return true
}

func (c *client) remove(id int64) {
	if status, _, _ := c.do("POST", fmt.Sprint("/delete?photo=", id)); status != 200 {
		c.failed()
		return
	}
	t := c.sys.truth
	t.mu.Lock()
	t.gone[id] = time.Now()
	own := t.byAuthor[c.user]
	for i, other := range own {
		if other == id {
			t.byAuthor[c.user] = append(own[:i:i], own[i+1:]...)
		}
	}
	t.mu.Unlock()
}

// act is one thing a user does. Most of what users do is look.
func (c *client) act(others []*client) {
	t := c.sys.truth
	switch r := c.rng.Float64(); {
	case r < 0.60: // open the feed, and load some of its pictures
		page, _ := c.loadFeed()
		for _, p := range page.Photos[:min(4, len(page.Photos))] {
			c.loadImage(p.ID, p.URL)
		}
	case r < 0.80: // look at the profile of someone they follow
		t.mu.Lock()
		author := t.following[c.user][c.rng.Intn(len(t.following[c.user]))]
		t.mu.Unlock()
		c.loadProfile(author)
	case r < 0.84: // look at a stranger's profile; one in ten is private
		c.loadProfile(1 + c.rng.Intn(len(others)))
	case r < 0.90 && len(c.feed) > 0: // like a photo in the feed, and look again
		p := c.feed[c.rng.Intn(len(c.feed))]
		if c.like(p.ID) {
			page, _ := c.loadFeed()
			for _, q := range page.Photos {
				if q.ID == p.ID && q.Likes <= p.Likes { // the count they saw before, plus their own, at least
					t.mu.Lock()
					t.ownMissing++
					t.mu.Unlock()
				}
			}
		}
	case r < 0.94: // post a photo, and look at their own profile — now, and again a little later
		if c.upload() == 0 {
			return
		}
		c.loadProfile(c.user)
		// Followers are told, and some look straight away.
		t.mu.Lock()
		fans := t.followers[c.user]
		t.mu.Unlock()
		for i := 0; i < min(3, len(fans)); i++ {
			fan := others[fans[c.rng.Intn(len(fans))]-1]
			wait := time.Duration(c.rng.Intn(250)) * time.Millisecond
			go func() {
				time.Sleep(wait)
				(&client{sys: c.sys, user: fan.user}).loadProfile(c.user)
			}()
		}
		go func() {
			time.Sleep(markerTTL + 300*time.Millisecond)
			(&client{sys: c.sys, user: c.user}).loadProfile(c.user)
		}()
	case r < 0.955: // delete one of their own photos; someone who was looking at it asks for the picture again
		t.mu.Lock()
		own := t.byAuthor[c.user]
		t.mu.Unlock()
		if len(own) == 0 {
			return
		}
		id := own[c.rng.Intn(len(own))]
		url := imageURL(id) // a URL that was handed out just before the deletion
		c.loadImage(id, url)
		c.remove(id)
		for _, wait := range []time.Duration{0, urlLifetime / 2, urlLifetime * 9 / 10, urlLifetime + 200*time.Millisecond} {
			go func() {
				time.Sleep(wait)
				(&client{sys: c.sys, user: c.user}).loadImage(id, url)
			}()
		}
	default: // try an image URL from long ago
		if c.old != "" && time.Since(c.oldT) > urlLifetime+100*time.Millisecond {
			if status, _, _ := c.do("GET", c.old); status == 200 {
				t.mu.Lock()
				t.expiredOK++
				t.mu.Unlock()
			}
		}
	}
}

// ------------------------------------------------------------ load: many users, then the checks

func load(seconds int, drill string) {
	const users = 1200
	sys := setUp(users, 25, 12)
	t := sys.truth
	fmt.Printf("%d users for %d seconds. Lifetimes and budgets are a tenth of the design's.\n", users, seconds)
	if len(without) > 0 {
		for name := range without {
			fmt.Printf("WITHOUT %s\n", name)
		}
	}

	clients := make([]*client, users)
	for i := range clients {
		clients[i] = &client{sys: sys, user: i + 1, rng: rand.New(rand.NewSource(int64(i)))}
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(c.rng.Intn(1200)) * time.Millisecond)
			for time.Now().Before(deadline) {
				c.act(clients)
				time.Sleep(time.Duration(600+c.rng.Intn(1200)) * time.Millisecond) // a user does something about once a second
			}
		}()
	}

	// Half-way through, something breaks.
	time.Sleep(time.Duration(seconds) * time.Second * 4 / 10)
	switch drill {
	case "replica":
		fmt.Printf("DRILL: after %d seconds, replica A stops applying changes. It still answers queries.\n", seconds*4/10)
		_, err := sys.db.replicas[0].pool.Exec(ctx, "SELECT pg_wal_replay_pause()")
		must(err)
		defer sys.db.replicas[0].pool.Exec(ctx, "SELECT pg_wal_replay_resume()")
	case "instance":
		fmt.Printf("DRILL: after %d seconds, app-3 dies.\n", seconds*4/10)
		sys.instances[2].srv.Close()
	}
	wg.Wait()
	time.Sleep(markerTTL + urlLifetime) // let the follow-up checks finish

	t.mu.Lock()
	defer t.mu.Unlock()
	share := func(kind string, wheres ...string) string {
		total := 0
		for _, n := range t.answers[kind] {
			total += n
		}
		line := fmt.Sprintf("  %-9s %7d   ", kind, total)
		for _, w := range wheres {
			if n := t.answers[kind][w]; n > 0 {
				line += fmt.Sprintf("%s %.1f%%   ", w, float64(n)*100/float64(max(total, 1)))
			}
		}
		return line
	}
	sort.Slice(t.feedTimes, func(i, j int) bool { return t.feedTimes[i] < t.feedTimes[j] })
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	fmt.Println("\nWHERE READS WERE ANSWERED")
	fmt.Println(share("images", "edge HIT", "object storage", "refused"))
	fmt.Println(share("feeds", "replica", "primary", "edge HIT"))
	fmt.Println(share("profiles", "cache hit", "replica", "primary"))
	fmt.Printf("  a feed took %.1f ms (median), %.1f ms (99th percentile)\n",
		ms(t.feedTimes[len(t.feedTimes)/2]), ms(t.feedTimes[len(t.feedTimes)*99/100]))
	fmt.Printf("\nTHE DATABASE\n  primary     %6d writes, %6d reads\n", sys.db.writes.Load(), sys.db.primaryReads.Load())
	for _, r := range sys.db.replicas {
		state := "in use"
		if !r.in.Load() {
			state = "taken out of use"
		}
		behind := fmt.Sprintf("%d ms", time.Duration(r.behind.Load()).Milliseconds())
		if r.behind.Load() > int64(lagLimit) {
			behind = fmt.Sprintf("%.0f s", time.Duration(r.behind.Load()).Seconds())
		}
		fmt.Printf("  replica %s   %21d reads     at the end: at most %s behind, %s\n", r.name, r.reads.Load(), behind, state)
	}

	fmt.Printf("\nHOW STALE WERE THE ANSWERS?%29s %12s\n", "budget", "worst seen")
	for kind, name := range []string{"a new photo, missing from a follower's feed", "a like, missing from the count others see",
		"a deleted photo, still listed", "a deleted photo's picture, still served", "a new photo, missing from the profile others see"} {
		fmt.Printf("  %-50s %6.1f s %10.1f s\n", name, budgets[kind].Seconds(), t.worst[kind].Seconds())
	}

	fmt.Println("\nCHECKS")
	for _, c := range []struct {
		name string
		n    int
	}{
		{"answers staler than their budget", t.late},
		{"users who did not see their own photo or like", t.ownMissing},
		{"users shown an older feed than one they had seen", t.backwards},
		{"users shown somebody else's feed", t.wrongUser},
		{"private profiles shown to someone not allowed", t.leaked},
		{"image URLs that still worked after expiring", t.expiredOK},
		{"requests that failed", t.failed},
	} {
		fmt.Printf("  %-50s %8d\n", c.name, c.n)
	}
}

// ------------------------------------------------------------ demo: ten requests, hop by hop

func demo() {
	sys := setUp(40, 8, 6)
	t := sys.truth
	ana := &client{sys: sys, user: 1}
	t.mu.Lock()
	ben := &client{sys: sys, user: t.followers[1][0]}
	cy := &client{sys: sys, user: t.followers[1][1]}
	var dee, eve int // dee is private; eve does not follow dee
	for u := 10; u <= 40 && eve == 0; u += 10 {
		for v := 2; v <= 40; v++ {
			if v != u && !t.follows(v, u) {
				dee, eve = u, v
				break
			}
		}
	}
	t.mu.Unlock()
	step := 0
	show := func(what, result, path string) {
		step++
		fmt.Printf("%2d. %s\n      %-34s %s\n", step, what, result, path)
	}
	timed := func(f func() (int, string)) (string, string) {
		begin := time.Now()
		status, path := f()
		return fmt.Sprintf("%d in %.1f ms", status, float64(time.Since(begin).Microseconds())/1000), path
	}

	page, path := ana.loadFeed()
	show("Ana opens her feed", fmt.Sprintf("%d photos", len(page.Photos)), path)
	first := page.Photos[0]
	result, path := timed(func() (int, string) { return ana.loadImage(first.ID, first.URL) })
	show("Her browser asks for the first picture", result, path)
	result, path = timed(func() (int, string) { return ben.loadImage(first.ID, imageURL(first.ID)) })
	show("Ben is shown the same photo, with a URL of his own", result, path)

	id := ana.upload()
	p, path := ana.loadProfile(1)
	show("Ana posts a photo, and looks at her own profile at once", fmt.Sprintf("her new photo is there: %v", len(p.Photos) > 0 && p.Photos[0].ID == id), path)
	time.Sleep(100 * time.Millisecond)
	p, path = ben.loadProfile(1)
	show("Ben, who follows her, looks 100 ms later", fmt.Sprintf("the new photo is there: %v", p.Photos[0].ID == id), path)
	time.Sleep(markerTTL)
	p, path = cy.loadProfile(1)
	show("Cy looks a second and a half after that", fmt.Sprintf("the new photo is there: %v", p.Photos[0].ID == id), path)

	before := first.Likes
	ana.like(first.ID)
	page, path = ana.loadFeed()
	after := before
	for _, q := range page.Photos {
		if q.ID == first.ID {
			after = q.Likes
		}
	}
	show("Ana likes the first photo, and her feed reloads", fmt.Sprintf("likes: %d, then %d", before, after), path)

	status, path, _ := (&client{sys: sys, user: eve}).do("GET", fmt.Sprint("/profile?of=", dee))
	show("Eve asks for the profile of Dee, whose account is private", fmt.Sprint(status), path)

	url := imageURL(id)
	ben.loadImage(id, url)
	ana.remove(id)
	result, path = timed(func() (int, string) { return ben.loadImage(id, url) })
	show("Ana deletes her new photo. Ben's browser asks for the picture again", result, path)

	time.Sleep(urlLifetime + 100*time.Millisecond)
	result, path = timed(func() (int, string) { return ana.loadImage(first.ID, first.URL) })
	show(fmt.Sprintf("%.0f seconds later, the URL from step 2 is used again", urlLifetime.Seconds()), result, path)
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: capstone-photos demo | load [-seconds 20] [-without safeguard] [-drill replica|instance]")
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	seconds := flags.Int("seconds", 20, "how long the load runs")
	off := flags.String("without", "", "remove one safeguard: read-rule, marker, lag-limit, purge, private-header")
	drill := flags.String("drill", "", "break something part-way through: replica, instance")
	flags.Parse(os.Args[2:])
	if *off != "" {
		without[*off] = true
	}
	switch os.Args[1] {
	case "demo":
		demo()
	case "load":
		load(*seconds, *drill)
	default:
		log.Fatal("usage: capstone-photos demo | load")
	}
}
