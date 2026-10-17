package billing

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// PullWindowDays is how much history each pull re-reads.
	PullWindowDays = 31
	pullTimeout    = 90 * time.Second
	dailyHourUTC   = 3
)

// Clock makes the poller schedule testable.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NextRun is the next 03:00 UTC strictly after now.
func NextRun(now time.Time) time.Time {
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), dailyHourUTC, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// Pull triggers (closed vocabulary, logged on every pull).
const (
	TriggerStartup = "startup"
	TriggerDaily   = "daily"
	TriggerInstall = "install"
	TriggerRefresh = "refresh"
)

// override is the process-level key source set by the install/remove
// handlers. key != "" is the verified key held in memory until the mounted
// file is readable and matches; removed is the tombstone that blocks a
// lingering mounted file until the file is gone.
type override struct {
	key     string
	removed bool
}

// Service owns the pull logic and the poller loop.
type Service struct {
	Store   *Store
	Keys    KeyReader
	Vendors map[string]Vendor // anthropic, openai
	Bedrock *BedrockSource    // nil = never configured
	Clock   Clock

	locks map[string]*sync.Mutex

	mu   sync.Mutex
	ovr  map[string]override
	gen  map[string]int // bumped on every API install/remove; a pull that straddles one discards its result
	kick chan struct{}
}

// NewService wires a Service. clock nil = real time.
func NewService(store *Store, keys KeyReader, vendors map[string]Vendor, bedrock *BedrockSource, clock Clock) *Service {
	if clock == nil {
		clock = realClock{}
	}
	return &Service{
		Store: store, Keys: keys, Vendors: vendors, Bedrock: bedrock, Clock: clock,
		locks: map[string]*sync.Mutex{Anthropic: {}, OpenAI: {}, Bedrock: {}},
		ovr:   map[string]override{},
		gen:   map[string]int{},
		kick:  make(chan struct{}, 1),
	}
}

// Now exposes the service clock (handlers use it for verify samples).
func (s *Service) Now() time.Time { return s.Clock.Now() }

// Install records the just-verified key in memory; it is used for pulls
// until the mounted file catches up.
func (s *Service) Install(vendor, key string) { s.setOverride(vendor, override{key: key}) }

// Remove records the removal tombstone: the mounted file is ignored until it
// is gone.
func (s *Service) Remove(vendor string) { s.setOverride(vendor, override{removed: true}) }

func (s *Service) setOverride(vendor string, o override) {
	s.mu.Lock()
	s.ovr[vendor] = o
	s.gen[vendor]++
	s.mu.Unlock()
}

func (s *Service) generation(vendor string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen[vendor]
}

// resolveKey picks the key for a pull: override first, then the mounted file.
// fromFile says the key came from the file with no override in play.
func (s *Service) resolveKey(vendor string) (key string, ok, fromFile bool) {
	fk, fok := s.Keys.Read(vendor)
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.ovr[vendor]
	switch {
	case o.removed:
		if !fok {
			delete(s.ovr, vendor) // file is gone; nothing left to block
		}
		return "", false, false
	case o.key != "":
		if fok && fk == o.key {
			delete(s.ovr, vendor) // file caught up
			return fk, true, false
		}
		return o.key, true, false
	}
	return fk, fok, fok
}

// Kick requests a full pull of every configured vendor (non-blocking,
// coalescing).
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run pulls once at start, then daily at 03:00 UTC and on every Kick.
func (s *Service) Run(ctx context.Context) {
	s.PullAll(ctx, TriggerStartup)
	for {
		now := s.Clock.Now()
		trigger := TriggerDaily
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(NextRun(now).Sub(now)):
		case <-s.kick:
			trigger = TriggerRefresh
		}
		if ctx.Err() != nil {
			return
		}
		s.PullAll(ctx, trigger)
	}
}

// PullAll pulls every vendor, sequentially; one vendor's failure never blocks
// another.
func (s *Service) PullAll(ctx context.Context, trigger string) {
	for _, v := range []string{Anthropic, OpenAI, Bedrock} {
		if ctx.Err() != nil {
			return
		}
		s.PullVendor(ctx, v, trigger)
	}
}

func (s *Service) window() (time.Time, time.Time) {
	today := startOfDay(s.Clock.Now())
	return today.AddDate(0, 0, -(PullWindowDays - 1)), today.Add(24 * time.Hour)
}

func ptr(s string) *string { return &s }

func (s *Service) setState(ctx context.Context, vendor, state string, errKind *string, pulled bool) {
	if err := s.Store.SetPullState(ctx, vendor, state, errKind, pulled); err != nil {
		slog.Error("billing: status write failed", "vendor", vendor)
	}
}

// PullVendor pulls one vendor and logs exactly one closed-vocabulary line.
// It never panics the caller and never logs a key or vendor text. The poller
// writes only state/last_pulled_at/last_error_kind (Bedrock's configured flag
// excepted: it has no install handler).
func (s *Service) PullVendor(ctx context.Context, vendor, trigger string) {
	mu := s.locks[vendor]
	if mu == nil {
		return
	}
	if trigger == TriggerInstall {
		mu.Lock() // an install-time pull must not be dropped because a poll is running
	} else if !mu.TryLock() {
		return // a pull for this vendor is already running
	}
	defer mu.Unlock()

	start := time.Now()
	outcome, rows := "vendor_error", 0
	defer func() {
		if r := recover(); r != nil {
			outcome, rows = KindVendorError, 0
		}
		slog.Info("billing: pull", "vendor", vendor, "trigger", trigger, "outcome", outcome,
			"rows", rows, "duration_ms", time.Since(start).Milliseconds())
	}()
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	from, to := s.window()

	if vendor == Bedrock {
		outcome, rows = s.pullBedrock(ctx, from, to)
		return
	}
	client := s.Vendors[vendor]
	if client == nil {
		outcome = StateNotConfigure
		return
	}

	gen := s.generation(vendor)
	key, ok, fromFile := s.resolveKey(vendor)
	configured, err := s.Store.Configured(ctx, vendor)
	if err != nil {
		slog.Error("billing: status read failed", "vendor", vendor)
		return
	}
	if !ok {
		outcome = StateNotConfigure
		if configured {
			outcome = StatePendingMount
		}
		if s.generation(vendor) == gen {
			s.setState(ctx, vendor, outcome, nil, false)
		}
		return
	}
	if fromFile && !configured && s.generation(vendor) == gen {
		// key installed out of band (no handler ran): record it, never clear.
		if err := s.Store.SetFileKeyConfigured(ctx, vendor, Last4(key)); err != nil {
			slog.Error("billing: status write failed", "vendor", vendor)
		}
	}
	s.setState(ctx, vendor, StatePulling, nil, false)
	rep, err := client.Pull(ctx, key, from, to)
	if s.generation(vendor) != gen {
		outcome = "superseded" // key installed/removed mid-pull: that request owns the status now
		return
	}
	if err != nil {
		e := AsError(err)
		outcome = e.Kind
		s.setState(ctx, vendor, e.Kind, ptr(e.Kind), false)
		return
	}
	outcome, rows = s.finish(ctx, vendor, rep.Usage, rep.Costs, rep.CostsErr)
}

func (s *Service) finish(ctx context.Context, vendor string, usage []UsageRow, costs []CostRow, costsErr *Error) (string, int) {
	if err := s.Store.Upsert(ctx, vendor, usage, costs); err != nil {
		s.setState(ctx, vendor, KindVendorError, ptr(KindStoreFailed), false)
		return KindStoreFailed, 0
	}
	n := len(usage) + len(costs)
	if costsErr != nil {
		s.setState(ctx, vendor, costsErr.Kind, ptr(costsErr.Kind), true)
		return costsErr.Kind, n
	}
	s.setState(ctx, vendor, StateOK, nil, true)
	return StateOK, n
}

func (s *Service) pullBedrock(ctx context.Context, from, to time.Time) (string, int) {
	credCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	configured := s.Bedrock.Configured(credCtx)
	cancel()
	if err := s.Store.SetBedrockConfigured(ctx, configured); err != nil {
		slog.Error("billing: status write failed", "vendor", Bedrock)
	}
	if !configured {
		s.setState(ctx, Bedrock, StateNotConfigure, nil, false)
		return StateNotConfigure, 0
	}
	s.setState(ctx, Bedrock, StatePulling, nil, false)
	costs, e := s.Bedrock.Pull(ctx, from, to)
	if e != nil {
		s.setState(ctx, Bedrock, e.Kind, ptr(e.Kind), false)
		return e.Kind, 0
	}
	return s.finish(ctx, Bedrock, nil, costs, nil)
}
