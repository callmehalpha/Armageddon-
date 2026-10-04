// P4: do lease + epoch fencing + CAS commits hold under crashes and
// partitions?
//
// A deterministic, seeded simulation of the protocol in
// docs/design/v0.1-architecture.md §3–§4 and §6.4: one authority with a server
// seat, and two devices. Each step the scheduler either lets a user act (edit,
// work local, forced takeover), injects a fault (crash, restart, partition,
// permanent device loss, server drift), or delivers a message from a network
// that drops, duplicates, delays and reorders. After every step a checker that
// shares no code with the protocol verifies:
//
//	I1  every accepted commit was made by the seat granted that epoch, and no
//	    epoch was granted twice;
//	I3  the current checkpoint contains every acknowledged checkpoint's work;
//	I4  every edit made on a seat that still exists is somewhere durable
//	    (server current, a quarantine, or that seat's own durable state).
//
// At the end of each run the faults stop and the system must converge
// (liveness). Mutants (-mutants) break one protocol rule each to show the
// checker is not vacuous.
//
// Work is modelled as a set of unique edit tokens: an edit adds a token, so
// losing work means losing a token.
package main

import (
	"flag"
	"fmt"
	"math/bits"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	runs    = flag.Int("runs", 100000, "randomized runs")
	steps   = flag.Int("steps", 300, "fault-injection steps per run")
	seed    = flag.Int64("seed", 1, "base seed")
	mutants = flag.Bool("mutants", true, "also run each mutant and report detection")
	mutRuns = flag.Int("mutant-runs", 2000, "runs per mutant")
	verbose = flag.Bool("v", false, "print first failure trace")
	onlyMut = flag.String("mutant", "", "run only this mutant")
)

// ---------------------------------------------------------------------------
// Token sets (≤ 512 edits per run).

type Set [8]uint64

func (s *Set) Add(t int)     { s[t/64] |= 1 << (t % 64) }
func (s Set) Has(t int) bool { return s[t/64]&(1<<(t%64)) != 0 }
func (s Set) Union(o Set) (r Set) {
	for i := range s {
		r[i] = s[i] | o[i]
	}
	return
}
func (s Set) SubsetOf(o Set) bool {
	for i := range s {
		if s[i]&^o[i] != 0 {
			return false
		}
	}
	return true
}
func (s Set) Count() (n int) {
	for _, w := range s {
		n += bits.OnesCount64(w)
	}
	return
}
func (s Set) Minus(o Set) (r Set) {
	for i := range s {
		r[i] = s[i] &^ o[i]
	}
	return
}

const maxTokens = 500

// ---------------------------------------------------------------------------
// Protocol model

type Seat string

const server Seat = "server"

type Checkpoint struct {
	ID      int
	Parent  int // 0 = none
	Content Set
	Epoch   int
	Author  Seat
	Seq     int // assigned at commit
}

type Quarantine struct {
	ID      int
	From    Seat
	Content Set
}

type Msg struct {
	To, From Seat
	Kind     string
	Epoch    int
	CP       *Checkpoint
	Q        *Quarantine
	ID, Seq  int
	Content  Set
	Reason   string
	at       int
}

type Mutant string

const (
	MutNone               Mutant = ""
	MutNoEpochCheck       Mutant = "authority-ignores-epoch"
	MutNoParentCAS        Mutant = "authority-skips-parent-CAS"
	MutNoDirtyQuarantine  Mutant = "follower-overwrites-dirty-tree"
	MutNoGrantReconcile   Mutant = "grant-keeps-stale-tree"
	MutForceNoEpochBump   Mutant = "force-takeover-keeps-epoch"
	MutVolatileOutbox     Mutant = "quarantine-outbox-not-durable"
	MutAckLostIsNew       Mutant = "retry-after-lost-ack-not-idempotent"
	MutNoLeaseInResync    Mutant = "resync-omits-lease-state"
	MutNoHeartbeat        Mutant = "writer-sends-no-heartbeat"
	MutNoEpochNoCAS       Mutant = "ignores-epoch-AND-skips-CAS"
	MutStaleRejectHonored Mutant = "stale-epoch-rejection-honored"
	MutNoWriterResync     Mutant = "writer-never-resyncs-base"
)

var allMutants = []Mutant{MutNoEpochCheck, MutNoParentCAS, MutNoDirtyQuarantine, MutNoGrantReconcile, MutForceNoEpochBump, MutVolatileOutbox, MutAckLostIsNew, MutNoLeaseInResync, MutNoHeartbeat, MutNoEpochNoCAS, MutStaleRejectHonored, MutNoWriterResync}

// Authority state is durable (SQLite in the real design). The server seat's
// working tree is durable (disk).
type Authority struct {
	up          bool
	holder      Seat
	epoch       int
	handoff     bool
	handoffTo   Seat
	deadline    int
	current     *Checkpoint
	seq         int
	commits     map[int]*Checkpoint
	quarantines map[int]*Quarantine
	// server seat
	srvTree Set
	srvBase *Checkpoint // what srvTree was last synced to
}

type Device struct {
	id Seat
	// durable
	tree      Set
	base      *Checkpoint // last applied / acknowledged checkpoint (shadow copy)
	baseSeq   int
	epoch     int // >0 while this device believes it holds the lease
	pending   []*Checkpoint
	outbox    []*Quarantine
	releasing bool
	// volatile
	up          bool
	destroyed   bool
	partitioned bool
	acquiring   bool
	waitRelease bool
}

type Sim struct {
	rng     *rand.Rand
	mut     Mutant
	now     int
	a       *Authority
	devs    map[Seat]*Device
	order   []Seat
	net     []*Msg
	nextID  int
	nextTok int
	trace   []string
	// ghost state for the checker (never read by the protocol)
	origin      map[int]Seat
	madeBy      map[Seat]*Set
	ackedUnion  Set
	epochGrants map[int]Seat
	commitLog   []*Checkpoint
	lostByRPO   int
	stats       *Stats
}

type Stats struct {
	runs, steps, msgs, drops, dups, edits, commits, rejectedLeaseLost, rejectedParent int
	quarantines, forced, handoffs, handoffTimeouts, crashes, srvCrashes, destroyed    int
	partitions, drift, lostByRPO, strandedLocal, idempotentAcks                       int
}

func (s *Sim) log(f string, a ...any) {
	if *verbose {
		s.trace = append(s.trace, fmt.Sprintf("%4d ", s.now)+fmt.Sprintf(f, a...))
	}
}

func (s *Sim) id() int { s.nextID++; return s.nextID }

func (s *Sim) send(m *Msg) {
	m.at = s.now
	s.net = append(s.net, m)
}

func emptyCP() *Checkpoint { return &Checkpoint{ID: 0, Seq: 0} }

func NewSim(seed int64, mut Mutant, st *Stats) *Sim {
	s := &Sim{rng: rand.New(rand.NewSource(seed)), mut: mut, stats: st,
		origin: map[int]Seat{}, epochGrants: map[int]Seat{1: server}, devs: map[Seat]*Device{},
		madeBy: map[Seat]*Set{server: {}, "d1": {}, "d2": {}}}
	root := emptyCP()
	s.a = &Authority{up: true, holder: server, epoch: 1, current: root, commits: map[int]*Checkpoint{0: root},
		quarantines: map[int]*Quarantine{}, srvBase: root}
	for _, d := range []Seat{"d1", "d2"} {
		s.devs[d] = &Device{id: d, base: root, up: true}
		s.order = append(s.order, d)
	}
	return s
}

// ----- authority ---------------------------------------------------------

func (s *Sim) commit(cp *Checkpoint) {
	a := s.a
	a.seq++
	cp.Seq = a.seq
	a.commits[cp.ID] = cp
	a.current = cp
	s.stats.commits++
	// ghost
	s.commitLog = append(s.commitLog, cp)
	s.ackedUnion = s.ackedUnion.Union(cp.Content)
	if g, ok := s.epochGrants[cp.Epoch]; !ok || g != cp.Author {
		panic(violation("I1: commit by %s under epoch %d granted to %q", cp.Author, cp.Epoch, g))
	}
	s.log("COMMIT cp%d by %s e%d seq%d parent=%d tokens=%d", cp.ID, cp.Author, cp.Epoch, cp.Seq, cp.Parent, cp.Content.Count())
	// Server seat follows synchronously when it is not the holder.
	if a.holder != server {
		s.serverApply(cp)
	}
	for _, d := range s.order {
		if d != cp.Author {
			s.send(&Msg{To: d, From: server, Kind: "checkpoint", ID: cp.ID, Seq: cp.Seq, Content: cp.Content, CP: cp})
		}
	}
}

func (s *Sim) serverApply(cp *Checkpoint) {
	a := s.a
	if cp.Seq <= a.srvBase.Seq {
		return
	}
	if a.srvTree != a.srvBase.Content { // server drift → quarantine (I4)
		q := &Quarantine{ID: s.id(), From: server, Content: a.srvTree}
		a.quarantines[q.ID] = q
		s.stats.quarantines++
		s.log("server drift quarantined q%d", q.ID)
	}
	a.srvTree = cp.Content
	a.srvBase = cp
}

func (s *Sim) grant(to Seat) {
	a := s.a
	prev := a.holder
	a.epoch++
	a.holder = to
	a.handoff = false
	if g, ok := s.epochGrants[a.epoch]; ok && g != to {
		panic(violation("I1: epoch %d granted twice (%s, %s)", a.epoch, g, to))
	}
	s.epochGrants[a.epoch] = to
	s.log("GRANT e%d %s → %s", a.epoch, prev, to)
	if to == server {
		// The server seat is in-process: reconcile its tree like a device.
		if a.srvBase.ID != a.current.ID && a.srvTree != a.srvBase.Content {
			q := &Quarantine{ID: s.id(), From: server, Content: a.srvTree}
			a.quarantines[q.ID] = q
			s.stats.quarantines++
		}
		if a.srvBase.ID != a.current.ID {
			a.srvTree, a.srvBase = a.current.Content, a.current
		}
		return
	}
	s.send(&Msg{To: to, From: server, Kind: "granted", Epoch: a.epoch, ID: a.current.ID, Seq: a.current.Seq, Content: a.current.Content, CP: a.current})
}

func (s *Sim) serverCapture() {
	a := s.a
	if !a.up || a.holder != server || a.srvTree == a.current.Content {
		return
	}
	cp := &Checkpoint{ID: s.id(), Parent: a.current.ID, Content: a.srvTree, Epoch: a.epoch, Author: server}
	s.commit(cp)
	a.srvBase = cp
}

func (s *Sim) handle(m *Msg) {
	a := s.a
	if m.To == server {
		if !a.up {
			return
		}
		switch m.Kind {
		case "commit":
			cp := m.CP
			if c, ok := a.commits[cp.ID]; ok && s.mut != MutAckLostIsNew {
				s.stats.idempotentAcks++
				s.send(&Msg{To: m.From, From: server, Kind: "ack", ID: cp.ID, Seq: c.Seq})
				return
			}
			epochOK := a.holder == m.From && m.Epoch == a.epoch
			if s.mut == MutNoEpochCheck || s.mut == MutNoEpochNoCAS {
				epochOK = true
			}
			if !epochOK {
				s.stats.rejectedLeaseLost++
				s.send(&Msg{To: m.From, From: server, Kind: "reject", ID: cp.ID, Reason: "lease_lost", Epoch: m.Epoch})
				return
			}
			if cp.Parent != a.current.ID && s.mut != MutNoParentCAS && s.mut != MutNoEpochNoCAS {
				s.stats.rejectedParent++
				s.send(&Msg{To: m.From, From: server, Kind: "reject", ID: cp.ID, Reason: "parent_mismatch", Epoch: m.Epoch})
				return
			}
			c := *cp
			c.Epoch = m.Epoch
			if s.mut == MutNoEpochCheck || s.mut == MutNoEpochNoCAS {
				s.epochGrants[c.Epoch] = c.Author // let the mutant past the I1 tripwire; I3/I4 must catch it
			}
			s.commit(&c)
			s.send(&Msg{To: m.From, From: server, Kind: "ack", ID: c.ID, Seq: c.Seq})
		case "acquire":
			if a.holder == m.From {
				s.grantResend(m.From)
				return
			}
			if a.handoff {
				if a.handoffTo != m.From {
					s.send(&Msg{To: m.From, From: server, Kind: "denied"})
				}
				return
			}
			a.handoff, a.handoffTo, a.deadline = true, m.From, s.now+40
			s.stats.handoffs++
			if a.holder == server {
				s.serverCapture() // flush
				s.grant(m.From)
			} else {
				s.send(&Msg{To: a.holder, From: server, Kind: "flush_release", Epoch: a.epoch})
			}
		case "release":
			switch {
			case a.handoff && a.holder == m.From && m.Epoch == a.epoch:
				to := a.handoffTo
				s.send(&Msg{To: m.From, From: server, Kind: "release_ack", Epoch: m.Epoch})
				s.grant(to)
			case m.Epoch < a.epoch:
				s.send(&Msg{To: m.From, From: server, Kind: "release_ack", Epoch: m.Epoch})
			default:
				s.send(&Msg{To: m.From, From: server, Kind: "release_rejected", Epoch: m.Epoch})
			}
		case "force":
			s.stats.forced++
			if s.mut == MutForceNoEpochBump {
				a.holder, a.handoff = m.From, false
				s.epochGrants[a.epoch] = m.From
				s.send(&Msg{To: m.From, From: server, Kind: "granted", Epoch: a.epoch, ID: a.current.ID, Seq: a.current.Seq, Content: a.current.Content, CP: a.current})
				return
			}
			s.grant(m.From)
		case "quarantine":
			if _, ok := a.quarantines[m.Q.ID]; !ok {
				a.quarantines[m.Q.ID] = m.Q
				s.stats.quarantines++
			}
			s.send(&Msg{To: m.From, From: server, Kind: "q_ack", ID: m.Q.ID})
		case "fetch":
			// The resync reply carries the lease, so a device that missed
			// its "granted" message (lost, or it crashed) learns it holds
			// the lease.
			holder := ""
			if a.holder == m.From && !(s.mut == MutNoLeaseInResync) {
				holder = "you"
			}
			s.send(&Msg{To: m.From, From: server, Kind: "current", ID: a.current.ID, Seq: a.current.Seq, Content: a.current.Content, CP: a.current, Epoch: a.epoch, Reason: holder})
		}
		return
	}
	d := s.devs[m.To]
	if d == nil || !d.up || d.destroyed {
		return
	}
	switch m.Kind {
	case "ack":
		if len(d.pending) > 0 && d.pending[0].ID == m.ID {
			cp := d.pending[0]
			d.pending = d.pending[1:]
			if m.Seq > d.baseSeq {
				d.base, d.baseSeq = cp, m.Seq
			}
		}
		s.maybeRelease(d)
	case "reject":
		if len(d.pending) == 0 || d.pending[0].ID != m.ID {
			return
		}
		if m.Epoch < d.epoch && s.mut != MutStaleRejectHonored {
			return // rejection of an attempt under an older epoch: stale
		}
		s.loseLease(d, "rejected:"+m.Reason)
	case "checkpoint", "current":
		if m.Kind == "current" && m.Reason == "you" && m.Epoch > d.epoch {
			s.onGranted(d, m.Epoch, m.CP, m.Seq)
			return
		}
		if m.Kind == "current" && d.epoch > 0 && m.Reason != "you" && m.Epoch > d.epoch {
			// Heartbeat reply: the lease moved on while this device was cut off.
			s.loseLease(d, "heartbeat: lease moved")
		}
		if m.Kind == "current" && d.epoch > 0 && m.Reason == "you" && m.Epoch == d.epoch &&
			len(d.pending) == 0 && m.ID != d.base.ID && m.Seq > d.baseSeq && s.mut != MutNoWriterResync {
			// Still the holder, but current moved past our base: one of our
			// own commits landed after we stopped tracking it. Catch up.
			if d.tree != d.base.Content {
				s.quarantineLocal(d, "writer resync, local edits on stale base")
			}
			d.tree, d.base, d.baseSeq = m.CP.Content, m.CP, m.Seq
			return
		}
		if m.Seq <= d.baseSeq {
			return
		}
		if d.epoch > 0 && m.CP.Epoch > d.epoch {
			// Committed under a newer epoch: this device lost the lease.
			// (Older checkpoints predate its grant and are already its base.)
			s.loseLease(d, "superseded")
		}
		if d.epoch > 0 {
			return
		}
		s.followerApply(d, m.CP, m.Seq)
	case "granted":
		s.onGranted(d, m.Epoch, m.CP, m.Seq)
	case "denied":
		d.acquiring = false
	case "flush_release":
		if m.Epoch == d.epoch {
			d.releasing = true
			s.capture(d)
			s.maybeRelease(d)
		}
	case "release_ack":
		if m.Epoch == d.epoch {
			d.epoch, d.releasing, d.waitRelease = 0, false, false
			s.log("%s released", d.id)
		}
	case "release_rejected":
		if m.Epoch == d.epoch {
			d.releasing, d.waitRelease = false, false
		}
	case "q_ack":
		for i, q := range d.outbox {
			if q.ID == m.ID {
				d.outbox = append(d.outbox[:i], d.outbox[i+1:]...)
				break
			}
		}
	}
}

func (s *Sim) onGranted(d *Device, epoch int, cur *Checkpoint, seq int) {
	if epoch <= d.epoch {
		return
	}
	d.acquiring = false
	{
		switch {
		case len(d.pending) > 0 && d.pending[0].Parent == cur.ID:
			// Unacked work that builds on current carries over (fast-forward).
			for _, p := range d.pending {
				p.Epoch = epoch
			}
		case len(d.pending) > 0:
			s.quarantineLocal(d, "grant: pending not on current")
			d.pending = nil
			d.tree, d.base, d.baseSeq = cur.Content, cur, seq
		case d.base.ID == cur.ID:
			// Local edits on top of current are a fast-forward: keep them.
		case d.tree == d.base.Content || s.mut == MutNoGrantReconcile:
			if s.mut == MutNoGrantReconcile {
				d.base, d.baseSeq = cur, seq // keeps the stale tree
			} else {
				d.tree, d.base, d.baseSeq = cur.Content, cur, seq
			}
		default:
			s.quarantineLocal(d, "grant: dirty and server moved")
			d.tree, d.base, d.baseSeq = cur.Content, cur, seq
		}
	}
	d.epoch = epoch
	d.releasing, d.waitRelease = false, false
	s.log("%s now writer e%d (tree=%d tokens)", d.id, d.epoch, d.tree.Count())
}

func (s *Sim) grantResend(to Seat) {
	a := s.a
	s.send(&Msg{To: to, From: server, Kind: "granted", Epoch: a.epoch, ID: a.current.ID, Seq: a.current.Seq, Content: a.current.Content, CP: a.current})
}

func (s *Sim) followerApply(d *Device, cp *Checkpoint, seq int) {
	if d.tree != d.base.Content && s.mut != MutNoDirtyQuarantine {
		s.quarantineLocal(d, "follower dirty")
	}
	d.tree, d.base, d.baseSeq = cp.Content, cp, seq
}

func (s *Sim) quarantineLocal(d *Device, why string) {
	content := d.tree
	for _, p := range d.pending {
		content = content.Union(p.Content)
	}
	q := &Quarantine{ID: s.id(), From: d.id, Content: content}
	d.outbox = append(d.outbox, q)
	s.log("%s quarantine q%d (%s, %d tokens)", d.id, q.ID, why, content.Count())
}

func (s *Sim) loseLease(d *Device, why string) {
	s.quarantineLocal(d, why)
	d.pending = nil
	d.epoch, d.releasing, d.waitRelease = 0, false, false
	// Resync from current; the tree stays as is until a checkpoint applies,
	// and the quarantine guarantees nothing is lost when it is overwritten.
	d.base = &Checkpoint{ID: -1, Content: d.tree} // treat local as clean against a pseudo-base
	d.baseSeq = -1                                // accept the next resync even if it is not newer
	s.send(&Msg{To: server, From: d.id, Kind: "fetch"})
}

func (s *Sim) capture(d *Device) {
	if d.epoch == 0 {
		return
	}
	tail := d.base
	if len(d.pending) > 0 {
		tail = d.pending[len(d.pending)-1]
	}
	if d.tree == tail.Content {
		return
	}
	cp := &Checkpoint{ID: s.id(), Parent: tail.ID, Content: d.tree, Epoch: d.epoch, Author: d.id}
	d.pending = append(d.pending, cp)
}

func (s *Sim) maybeRelease(d *Device) {
	if d.releasing && len(d.pending) == 0 && d.tree == d.base.Content {
		d.waitRelease = true
		s.send(&Msg{To: server, From: d.id, Kind: "release", Epoch: d.epoch})
	}
}

// retransmit is what an agent's retry loop does each tick.
func (s *Sim) retransmit(d *Device) {
	if !d.up || d.destroyed {
		return
	}
	if len(d.pending) > 0 {
		s.send(&Msg{To: server, From: d.id, Kind: "commit", Epoch: d.pending[0].Epoch, CP: d.pending[0]})
	}
	for _, q := range d.outbox {
		s.send(&Msg{To: server, From: d.id, Kind: "quarantine", Q: q})
	}
	if d.waitRelease {
		s.send(&Msg{To: server, From: d.id, Kind: "release", Epoch: d.epoch})
	}
	if d.acquiring {
		s.send(&Msg{To: server, From: d.id, Kind: "acquire"})
	}
	if d.epoch > 0 && s.mut != MutNoHeartbeat {
		s.send(&Msg{To: server, From: d.id, Kind: "fetch"}) // lease heartbeat
	}
}

// ----- environment --------------------------------------------------------

func (s *Sim) edit(seat Seat) {
	if s.nextTok >= maxTokens {
		return
	}
	t := s.nextTok
	s.nextTok++
	s.origin[t] = seat
	s.madeBy[seat].Add(t)
	s.stats.edits++
	if seat == server {
		s.a.srvTree.Add(t)
		return
	}
	d := s.devs[seat]
	d.tree.Add(t)
}

func (s *Sim) live(d *Device) bool { return d.up && !d.destroyed }

func (s *Sim) step(faults bool) {
	s.now++
	s.stats.steps++
	r := s.rng.Float64()
	d := s.devs[s.order[s.rng.Intn(len(s.order))]]
	switch {
	case r < 0.30: // deliver a message (random order = reordering)
		s.deliver(faults)
	case r < 0.45: // user edits on some seat
		if s.rng.Float64() < 0.35 {
			if s.a.up && (s.a.holder == server || (faults && s.rng.Float64() < 0.05)) {
				if s.a.holder != server {
					s.stats.drift++
				}
				s.edit(server)
			}
		} else if s.live(d) {
			s.edit(d.id)
		}
	case r < 0.55: // capture (debounce fired)
		if s.live(d) {
			s.capture(d)
		}
		if s.a.up {
			s.serverCapture()
		}
	case r < 0.63: // agent retry loop
		s.retransmit(d)
	case r < 0.66: // user: armageddon work local
		if s.live(d) && d.epoch == 0 {
			d.acquiring = true
			s.send(&Msg{To: server, From: d.id, Kind: "acquire"})
		}
	case r < 0.68: // user: armageddon work remote (server takes it back)
		if s.a.up && s.a.holder != server && !s.a.handoff {
			s.a.handoff, s.a.handoffTo, s.a.deadline = true, server, s.now+40
			s.stats.handoffs++
			s.send(&Msg{To: s.a.holder, From: server, Kind: "flush_release", Epoch: s.a.epoch})
		}
	case r < 0.70: // forced takeover (explicit, by a device or the server)
		if !faults {
			return
		}
		if s.rng.Float64() < 0.5 && s.live(d) {
			s.send(&Msg{To: server, From: d.id, Kind: "force"})
		} else if s.a.up {
			s.stats.forced++
			s.grant(server)
		}
	case r < 0.72: // handoff timeout
		if s.a.up && s.a.handoff && s.now > s.a.deadline {
			s.a.handoff = false
			s.stats.handoffTimeouts++
			if s.a.handoffTo != server {
				s.send(&Msg{To: s.a.handoffTo, From: server, Kind: "denied"})
			}
		}
	default:
		if faults {
			s.fault(d)
		}
	}
}

func (s *Sim) deliver(faults bool) {
	if len(s.net) == 0 {
		return
	}
	i := s.rng.Intn(len(s.net))
	m := s.net[i]
	s.net = append(s.net[:i], s.net[i+1:]...)
	s.stats.msgs++
	if faults {
		dev := m.To
		if dev == server {
			dev = m.From
		}
		if dd := s.devs[dev]; dd != nil && dd.partitioned {
			s.stats.drops++
			return
		}
		if s.rng.Float64() < 0.10 {
			s.stats.drops++
			return
		}
		if s.rng.Float64() < 0.05 {
			s.stats.dups++
			c := *m
			s.net = append(s.net, &c)
		}
	}
	s.handle(m)
}

func (s *Sim) fault(d *Device) {
	switch r := s.rng.Float64(); {
	case r < 0.30:
		if d.up && !d.destroyed {
			d.up = false
			s.stats.crashes++
			// volatile state is lost
			d.acquiring, d.waitRelease = false, false
			if s.mut == MutVolatileOutbox {
				d.outbox = nil
			}
			s.log("%s crash", d.id)
		}
	case r < 0.55:
		if !d.up && !d.destroyed {
			s.restart(d)
		}
	case r < 0.70:
		if s.a.up {
			s.a.up = false
			s.stats.srvCrashes++
			s.log("server crash")
		} else {
			s.a.up = true
		}
	case r < 0.85:
		d.partitioned = !d.partitioned
		s.stats.partitions++
	case r < 0.87:
		// Laptop dies permanently. At most one per run, never both devices.
		alive := 0
		for _, x := range s.devs {
			if !x.destroyed {
				alive++
			}
		}
		if alive == len(s.devs) && !d.destroyed {
			d.destroyed = true
			s.stats.destroyed++
			s.log("%s DESTROYED", d.id)
		}
	}
}

func (s *Sim) restart(d *Device) {
	d.up = true
	d.waitRelease = d.releasing && len(d.pending) == 0
	s.send(&Msg{To: server, From: d.id, Kind: "fetch"})
}

// ----- checker (independent of protocol code) ------------------------------

type violationErr string

func violation(f string, a ...any) violationErr { return violationErr(fmt.Sprintf(f, a...)) }

func (s *Sim) durableEverywhere() (all Set, perSeat map[Seat]Set) {
	perSeat = map[Seat]Set{}
	all = s.a.current.Content
	for _, q := range s.a.quarantines {
		all = all.Union(q.Content)
	}
	all = all.Union(s.a.srvTree)
	for _, id := range s.order {
		d := s.devs[id]
		if d.destroyed {
			continue
		}
		var mine Set
		mine = d.tree.Union(d.base.Content)
		for _, p := range d.pending {
			mine = mine.Union(p.Content)
		}
		for _, q := range d.outbox {
			mine = mine.Union(q.Content)
		}
		perSeat[id] = mine
		all = all.Union(mine)
	}
	return
}

func (s *Sim) check() {
	// I3: current contains all acknowledged work.
	if !s.ackedUnion.SubsetOf(s.a.current.Content) {
		miss := s.ackedUnion.Minus(s.a.current.Content)
		panic(violation("I3: %d acknowledged tokens missing from current", miss.Count()))
	}
	// I4: every token from a seat that still exists is durable somewhere.
	all, _ := s.durableEverywhere()
	for seat, made := range s.madeBy {
		if seat != server && s.devs[seat].destroyed {
			continue
		}
		if miss := made.Minus(all); miss.Count() > 0 {
			panic(violation("I4: %d tokens made on %s lost (not in current, any quarantine, or any durable seat state)", miss.Count(), seat))
		}
	}
}

// converge stops faults and drives the system to quiescence.
func (s *Sim) converge() {
	s.a.up = true
	for _, d := range s.devs {
		d.partitioned = false
		if !d.up && !d.destroyed {
			s.restart(d)
		}
	}
	// Any handoff still pending times out or completes.
	for i := 0; i < 4000; i++ {
		s.now++
		for _, id := range s.order {
			d := s.devs[id]
			if s.live(d) {
				s.capture(d)
				s.retransmit(d)
			}
		}
		s.serverCapture()
		if s.a.handoff && s.now > s.a.deadline {
			s.a.handoff = false
			if s.a.handoffTo != server {
				s.send(&Msg{To: s.a.handoffTo, From: server, Kind: "denied"})
			}
		}
		// Drain: replies to retries and heartbeats generate no further
		// traffic in a quiescent system.
		for n := 0; len(s.net) > 0 && n < 10000; n++ {
			s.deliver(false)
			s.check()
		}
		s.check()
		if len(s.net) == 0 && s.stable() {
			return
		}
	}
	panic(violation("liveness: no convergence after faults stopped (%s)", s.describe()))
}

func (s *Sim) stable() bool {
	if s.a.handoff {
		return false
	}
	for _, id := range s.order {
		d := s.devs[id]
		if d.destroyed {
			continue
		}
		if len(d.pending) > 0 || len(d.outbox) > 0 || d.waitRelease || d.acquiring {
			return false
		}
		if d.epoch > 0 && d.tree != s.a.current.Content {
			return false
		}
		if s.a.holder == d.id && d.epoch != s.a.epoch {
			s.send(&Msg{To: server, From: d.id, Kind: "fetch"})
			return false
		}
		if d.epoch == 0 && d.base.ID != s.a.current.ID && d.tree == d.base.Content {
			// clean follower not yet caught up
			s.send(&Msg{To: server, From: d.id, Kind: "fetch"})
			return false
		}
	}
	return true
}

func (s *Sim) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "holder=%s e%d handoff=%v cur=cp%d", s.a.holder, s.a.epoch, s.a.handoff, s.a.current.ID)
	for _, id := range s.order {
		d := s.devs[id]
		fmt.Fprintf(&b, " | %s up=%v destroyed=%v e%d base=cp%d pending=%d outbox=%d rel=%v", d.id, d.up, d.destroyed, d.epoch, d.base.ID, len(d.pending), len(d.outbox), d.releasing)
	}
	return b.String()
}

func (s *Sim) finalAccounting() {
	// Tokens lost only because their device was destroyed before they
	// reached the server (the accepted RPO), and tokens still only local.
	all, perSeat := s.durableEverywhere()
	srv := s.a.current.Content
	for _, q := range s.a.quarantines {
		srv = srv.Union(q.Content)
	}
	for t := 0; t < s.nextTok; t++ {
		if !all.Has(t) {
			s.stats.lostByRPO++
		} else if !srv.Has(t) {
			for _, m := range perSeat {
				if m.Has(t) {
					s.stats.strandedLocal++ // a dirty follower the user has not resolved
					break
				}
			}
		}
	}
}

// ----- driver -------------------------------------------------------------

func runOne(seed int64, mut Mutant, st *Stats) (err error, trace []string) {
	s := NewSim(seed, mut, st)
	defer func() {
		if r := recover(); r != nil {
			v, ok := r.(violationErr)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%s", string(v))
			trace = s.trace
		}
	}()
	for i := 0; i < *steps; i++ {
		s.step(true)
		s.check()
	}
	s.converge()
	s.finalAccounting()
	st.runs++
	return nil, nil
}

func main() {
	flag.Parse()
	start := time.Now()
	if *onlyMut == "" {
		st := &Stats{}
		var fails []string
		var firstTrace []string
		for i := 0; i < *runs; i++ {
			if err, tr := runOne(*seed+int64(i), MutNone, st); err != nil {
				fails = append(fails, fmt.Sprintf("seed %d: %v", *seed+int64(i), err))
				if firstTrace == nil {
					firstTrace = tr
				}
			}
		}
		fmt.Printf("P4 protocol model — correct protocol: %d runs × %d fault steps + convergence (%s)\n", *runs, *steps, time.Since(start).Round(time.Second))
		fmt.Printf("  steps=%d messages=%d dropped=%d duplicated=%d\n", st.steps, st.msgs, st.drops, st.dups)
		fmt.Printf("  edits=%d commits=%d rejected(lease_lost)=%d rejected(parent)=%d idempotent_re-acks=%d\n", st.edits, st.commits, st.rejectedLeaseLost, st.rejectedParent, st.idempotentAcks)
		fmt.Printf("  handoffs=%d handoff_timeouts=%d forced_takeovers=%d quarantines=%d server_drift_edits=%d\n", st.handoffs, st.handoffTimeouts, st.forced, st.quarantines, st.drift)
		fmt.Printf("  device_crashes=%d server_crashes=%d partitions=%d devices_destroyed=%d\n", st.crashes, st.srvCrashes, st.partitions, st.destroyed)
		fmt.Printf("  tokens lost only with a destroyed device (accepted RPO)=%d; tokens left local on unresolved dirty followers=%d\n", st.lostByRPO, st.strandedLocal)
		fmt.Printf("  VIOLATIONS=%d\n", len(fails))
		for i, f := range fails {
			if i < 10 {
				fmt.Println("   ", f)
			}
		}
		if *verbose && firstTrace != nil {
			fmt.Println(strings.Join(firstTrace[max(0, len(firstTrace)-60):], "\n"))
		}
		if len(fails) > 0 && !*mutants {
			os.Exit(1)
		}
	}
	if *mutants || *onlyMut != "" {
		fmt.Printf("\nMutants (each breaks one rule; the checker must catch it), %d runs each:\n", *mutRuns)
		list := allMutants
		if *onlyMut != "" {
			list = []Mutant{Mutant(*onlyMut)}
		}
		for _, m := range list {
			st := &Stats{}
			caught := 0
			kinds := map[string]int{}
			var tr []string
			for i := 0; i < *mutRuns; i++ {
				if err, t := runOne(*seed+int64(i), m, st); err != nil {
					caught++
					k := strings.SplitN(err.Error(), ":", 2)[0]
					kinds[k]++
					if tr == nil {
						tr = t
					}
				}
			}
			var ks []string
			for k, v := range kinds {
				ks = append(ks, fmt.Sprintf("%s×%d", k, v))
			}
			sort.Strings(ks)
			fmt.Printf("  %-38s caught in %5d/%d runs (%.1f%%)  %s\n", m, caught, *mutRuns, 100*float64(caught)/float64(*mutRuns), strings.Join(ks, " "))
			if *verbose && *onlyMut != "" && tr != nil {
				fmt.Println(strings.Join(tr[max(0, len(tr)-40):], "\n"))
			}
		}
	}
}
