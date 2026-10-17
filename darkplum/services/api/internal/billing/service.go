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

// Service owns the pull logic and the poller loop.
type Service struct {
	Store   *Store
	Keys    KeyReader
	Vendors map[string]Vendor // anthropic, openai
	Bedrock *BedrockSource    // nil = never configured
	Clock   Clock

	locks map[string]*sync.Mutex

	mu      sync.Mutex
	removed map[string]bool // key removed via the API; the mounted file may linger until kubelet syncs
	gen     map[string]int  // bumped on every API install/remove; a pull that straddles one discards its result
	kick    chan struct{}
}

// NewService wires a Service. clock nil = real time.
func NewService(store *Store, keys KeyReader, vendors map[string]Vendor, bedrock *BedrockSource, clock Clock) *Service {
	if clock == nil {
		clock = realClock{}
	}
	return &Service{
		Store: store, Keys: keys, Vendors: vendors, Bedrock: bedrock, Clock: clock,
		locks:   map[string]*sync.Mutex{Anthropic: {}, OpenAI: {}, Bedrock: {}},
		removed: map[string]bool{},
		gen:     map[string]int{},
		kick:    make(chan struct{}, 1),
	}
}

// Now exposes the service clock (handlers use it for verify samples).
func (s *Service) Now() time.Time { return s.Clock.Now() }

// MarkRemoved / MarkInstalled record that a key was just removed/installed
// through the API, overriding a stale mounted file.
func (s *Service) MarkRemoved(vendor string)   { s.setRemoved(vendor, true) }
func (s *Service) MarkInstalled(vendor string) { s.setRemoved(vendor, false) }
func (s *Service) setRemoved(vendor string, v bool) {
	s.mu.Lock()
	s.removed[vendor] = v
	s.gen[vendor]++
	s.mu.Unlock()
}

func (s *Service) generation(vendor string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen[vendor]
}

func (s *Service) isRemoved(vendor string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removed[vendor]
}

func (s *Service) mountedKey(vendor string) (string, bool) {
	k, ok := s.Keys.Read(vendor)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		s.removed[vendor] = false // file is gone; nothing left to override
		return "", false
	}
	if s.removed[vendor] {
		return "", false
	}
	return k, true
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
	s.PullAll(ctx)
	for {
		now := s.Clock.Now()
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(NextRun(now).Sub(now)):
		case <-s.kick:
		}
		if ctx.Err() != nil {
			return
		}
		s.PullAll(ctx)
	}
}

// PullAll pulls every vendor, sequentially; one vendor's failure never blocks
// another.
func (s *Service) PullAll(ctx context.Context) {
	for _, v := range []string{Anthropic, OpenAI, Bedrock} {
		if ctx.Err() != nil {
			return
		}
		s.PullVendor(ctx, v, "")
	}
}

func (s *Service) window() (time.Time, time.Time) {
	today := startOfDay(s.Clock.Now())
	return today.AddDate(0, 0, -(PullWindowDays - 1)), today.Add(24 * time.Hour)
}

func ptr(s string) *string { return &s }

func (s *Service) setStatus(ctx context.Context, vendor string, configured bool, state string, errKind, last4 *string, pulled bool) {
	if err := s.Store.SetStatus(ctx, vendor, configured, state, errKind, last4, pulled); err != nil {
		slog.Error("billing: status write failed", "vendor", vendor)
	}
}

// PullVendor pulls one vendor. keyOverride (the just-verified key at install
// time, before kubelet propagates the Secret file) is used instead of the
// mounted file when non-empty. It never panics the caller and logs only
// closed kinds.
func (s *Service) PullVendor(ctx context.Context, vendor, keyOverride string) {
	mu := s.locks[vendor]
	if mu == nil {
		return
	}
	if keyOverride != "" {
		mu.Lock() // an install-time pull must not be dropped because a poll is running
	} else if !mu.TryLock() {
		return // a pull for this vendor is already running
	}
	defer mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("billing: pull panicked", "vendor", vendor)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	from, to := s.window()

	if vendor == Bedrock {
		s.pullBedrock(ctx, from, to)
		return
	}

	key := keyOverride
	if key != "" && s.isRemoved(vendor) {
		return // removed between install and this pull starting
	}
	gen := s.generation(vendor)
	if key == "" {
		var ok bool
		if key, ok = s.mountedKey(vendor); !ok {
			s.setStatus(ctx, vendor, false, StateNotConfigure, nil, nil, false)
			return
		}
	}
	last4 := ptr(Last4(key))
	client := s.Vendors[vendor]
	if client == nil {
		return
	}
	s.setStatus(ctx, vendor, true, StatePulling, nil, last4, false)
	rep, err := client.Pull(ctx, key, from, to)
	if s.generation(vendor) != gen {
		return // key installed/removed mid-pull: that request owns the status now
	}
	if err != nil {
		e := AsError(err)
		slog.Warn("billing: pull failed", "vendor", vendor, "kind", e.Kind, "vendor_http_status", e.HTTPStatus)
		s.setStatus(ctx, vendor, true, e.Kind, ptr(e.Kind), last4, false)
		return
	}
	s.finish(ctx, vendor, true, last4, rep.Usage, rep.Costs, rep.CostsErr)
}

func (s *Service) finish(ctx context.Context, vendor string, configured bool, last4 *string, usage []UsageRow, costs []CostRow, costsErr *Error) {
	if err := s.Store.Upsert(ctx, vendor, usage, costs); err != nil {
		slog.Error("billing: store failed", "vendor", vendor, "kind", KindStoreFailed)
		s.setStatus(ctx, vendor, configured, KindVendorError, ptr(KindStoreFailed), last4, false)
		return
	}
	if costsErr != nil {
		slog.Warn("billing: cost report unavailable", "vendor", vendor, "kind", costsErr.Kind, "vendor_http_status", costsErr.HTTPStatus)
		s.setStatus(ctx, vendor, configured, costsErr.Kind, ptr(costsErr.Kind), last4, true)
		return
	}
	s.setStatus(ctx, vendor, configured, StateOK, nil, last4, true)
}

func (s *Service) pullBedrock(ctx context.Context, from, to time.Time) {
	credCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	configured := s.Bedrock.Configured(credCtx)
	cancel()
	if !configured {
		s.setStatus(ctx, Bedrock, false, StateNotConfigure, nil, nil, false)
		return
	}
	s.setStatus(ctx, Bedrock, true, StatePulling, nil, nil, false)
	costs, e := s.Bedrock.Pull(ctx, from, to)
	if e != nil {
		slog.Warn("billing: pull failed", "vendor", Bedrock, "kind", e.Kind)
		s.setStatus(ctx, Bedrock, true, e.Kind, ptr(e.Kind), nil, false)
		return
	}
	s.finish(ctx, Bedrock, true, nil, nil, costs, nil)
}
