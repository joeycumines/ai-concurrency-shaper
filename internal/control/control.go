// Package control provides local, reviewable runtime changes. It deliberately
// exposes no HTTP endpoint and never changes breaker state or configuration files.
package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
)

const PreviewLifetime = 30 * time.Second
const MaxTTL = 24 * time.Hour
const HistoryLimit = 256

var (
	ErrDraining       = errors.New("provider is draining")
	ErrClosed         = errors.New("runtime controls are closed")
	ErrStale          = errors.New("runtime changed; review a new preview")
	ErrExpired        = errors.New("preview expired; review a new preview")
	ErrUnknownPreview = errors.New("unknown preview")
)

type State string

const (
	Running  State = "running"
	Paused   State = "paused"
	Draining State = "draining"
)

type Action string

const (
	Pause  Action = "pause"
	Resume Action = "resume"
	Drain  Action = "drain"
	Resize Action = "resize"
	Cancel Action = "cancel"
)

type Command struct {
	Action    Action        `json:"action"`
	Provider  string        `json:"provider"`
	Pool      string        `json:"pool,omitempty"`
	Limit     int           `json:"limit,omitempty"`
	TTL       time.Duration `json:"ttl_ns,omitempty"`
	RequestID string        `json:"request_id,omitempty"`
}

type Preview struct {
	Token        string    `json:"token"`
	Revision     uint64    `json:"revision"`
	ExpiresAt    time.Time `json:"expires_at"`
	Command      Command   `json:"command"`
	Effect       string    `json:"effect"`
	Consequences []string  `json:"consequences"`
}

type Result struct {
	Token    string    `json:"token,omitempty"`
	Revision uint64    `json:"revision"`
	At       time.Time `json:"at"`
	Command  Command   `json:"command"`
	Summary  string    `json:"summary"`
	Applied  bool      `json:"applied"`
}

type Audit struct {
	Result
	Kind string `json:"kind"` // applied, expired or superseded; failed applications do not mutate runtime.
}

type PoolState struct {
	ID        string    `json:"id"`
	Limit     int       `json:"limit"`
	Baseline  int       `json:"baseline"`
	Effective int       `json:"effective"`
	Active    int64     `json:"active"`
	Occupied  int64     `json:"occupied"`
	Cooldown  int64     `json:"cooldown"`
	Waiters   int64     `json:"waiters"`
	Debt      int64     `json:"debt"`
	Withheld  int64     `json:"withheld"`
	ExpiresAt time.Time `json:"expires_at"`
}
type ProviderState struct {
	Name      string      `json:"name"`
	State     State       `json:"state"`
	Baseline  State       `json:"baseline"`
	ExpiresAt time.Time   `json:"expires_at"`
	Pools     []PoolState `json:"pools"`
}
type Snapshot struct {
	Revision  uint64          `json:"revision"`
	Providers []ProviderState `json:"providers"`
	Audit     []Audit         `json:"audit"`
}

type pool struct {
	revision   uint64
	limiter    *queue.Limiter
	baseline   int
	generation uint64
	expires    time.Time
	timer      *time.Timer
}
type provider struct {
	state, baseline State
	changed         chan struct{}
	pools           map[string]*pool
	generation      uint64
	expires         time.Time
	timer           *time.Timer
}
type prepared struct {
	poolRevision uint64
	preview      Preview
	result       *Result
}

// Controller serializes previews, applications and expirations. A preview is an
// opaque random token bound to a frozen command and exact runtime revision.
// The caller never supplies the command again to Apply.
type Controller struct {
	mu         sync.Mutex
	providers  map[string]*provider
	revision   uint64
	prepared   map[string]*prepared
	order      []string
	audit      []Audit
	cancel     func(string, string) bool
	cancelByID func(string) bool
	onChange   func(Audit)
	closed     bool
}

func New() *Controller {
	return &Controller{providers: make(map[string]*provider), prepared: make(map[string]*prepared)}
}

// RegisterProvider accepts existing positive pools. IDs are stable local labels
// such as default, global and route:POST /v1/messages. Duplicate names fail.
func (c *Controller) RegisterProvider(name string, pools map[string]*queue.Limiter) error {
	if name == "" || len(name) > 256 {
		return errors.New("provider name must contain 1..256 bytes")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if _, ok := c.providers[name]; ok {
		return fmt.Errorf("provider %q is already registered", name)
	}
	p := &provider{state: Running, baseline: Running, changed: make(chan struct{}), pools: make(map[string]*pool, len(pools))}
	for id, l := range pools {
		if id == "" || len(id) > 512 || l == nil || l.Limit() < 1 {
			return fmt.Errorf("invalid pool %q", id)
		}
		p.pools[id] = &pool{limiter: l, baseline: l.Limit(), revision: l.Stats().Revision}
	}
	c.providers[name] = p
	c.revision++
	return nil
}

// SetCanceller connects exact logical-request cancellation. The function must
// be bounded and memory-only, and must not call the controller.
func (c *Controller) SetCanceller(cancel func(string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelByID = cancel
	if cancel != nil {
		c.cancel = func(_ string, id string) bool { return cancel(id) }
	} else {
		c.cancel = nil
	}
}

// SetProviderCanceller connects cancellation that verifies the reviewed
// provider identity rather than cancelling by request ID alone.
func (c *Controller) SetProviderCanceller(cancel func(string, string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = cancel
}

// SetOnChange is for safe logs and typed history markers. It runs after the
// state change, outside the controller lock; callbacks must not block.
func (c *Controller) SetOnChange(fn func(Audit)) { c.mu.Lock(); defer c.mu.Unlock(); c.onChange = fn }

// Gate runs once before provider admission, body reads and breaker Allow.
// A permit already returned by Gate remains valid through later pauses/drains.
// Pause waits on the original request context; drain rejects new arrivals.
func (c *Controller) Gate(ctx context.Context, name string) error {
	return c.GateObserved(ctx, name, nil)
}

// GateObserved brackets only an actual paused wait. Callbacks must be bounded,
// memory-only and may not call the controller. They execute without its lock.
func (c *Controller) GateObserved(ctx context.Context, name string, waiting func(bool)) error {
	waited := false
	defer func() {
		if waited && waiting != nil {
			waiting(false)
		}
	}()
	if c == nil {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return ErrClosed
		}
		p, ok := c.providers[name]
		if !ok {
			c.mu.Unlock()
			return fmt.Errorf("unknown provider %q", name)
		}
		state, changed := p.state, p.changed
		c.mu.Unlock()
		switch state {
		case Running:
			return nil
		case Draining:
			return ErrDraining
		case Paused:
			if !waited {
				waited = true
				if waiting != nil {
					waiting(true)
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			}
		}
	}
}

func (c *Controller) Preview(cmd Command) (Preview, error) {
	if c == nil {
		return Preview{}, ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Preview{}, ErrClosed
	}
	effect, consequences, err := c.describeLocked(cmd)
	if err != nil {
		return Preview{}, err
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Preview{}, err
	}
	now := time.Now()
	p := Preview{Token: hex.EncodeToString(token[:]), Revision: c.revision, ExpiresAt: now.Add(PreviewLifetime), Command: cmd, Effect: effect, Consequences: consequences}
	for len(c.order) >= HistoryLimit {
		delete(c.prepared, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, p.Token)
	ready := &prepared{preview: p}
	if cmd.Action == Resize {
		st := c.providers[cmd.Provider].pools[cmd.Pool].limiter.Stats()
		ready.poolRevision = st.Revision
		p.Effect = fmt.Sprintf("Resize %s / %s from %d to %d slots.", cmd.Provider, cmd.Pool, st.Limit, cmd.Limit)
		ready.preview = p
	}
	c.prepared[p.Token] = ready
	return clonePreview(p), nil
}

func clonePreview(p Preview) Preview { p.Consequences = slices.Clone(p.Consequences); return p }

func (c *Controller) describeLocked(cmd Command) (string, []string, error) {
	if cmd.TTL < 0 || cmd.TTL > MaxTTL {
		return "", nil, errors.New("TTL must be between 0 and 24h")
	}
	p, ok := c.providers[cmd.Provider]
	if !ok && cmd.Action != Cancel {
		return "", nil, fmt.Errorf("unknown provider %q", cmd.Provider)
	}
	if len(cmd.Provider) > 256 {
		return "", nil, errors.New("provider label exceeds 256 bytes")
	}
	consequences := []string{"Circuit breaker, existing leases and release holds remain in force."}
	var effect string
	switch cmd.Action {
	case Pause:
		effect = fmt.Sprintf("Pause new admissions for %s.", cmd.Provider)
		consequences = append(consequences, "New requests wait before reading their body; their deadlines and cancellation remain active.", "Requests already permitted, including queued requests, continue.")
	case Resume:
		effect = fmt.Sprintf("Resume new admissions for %s.", cmd.Provider)
		consequences = append(consequences, "Paused requests resume only if their original context is still live.")
	case Drain:
		effect = fmt.Sprintf("Drain %s: reject new admissions with local HTTP 503.", cmd.Provider)
		consequences = append(consequences, "Already permitted requests continue, including queued work. Local model discovery remains available.")
	case Resize:
		pl, ok := p.pools[cmd.Pool]
		if !ok {
			return "", nil, fmt.Errorf("unknown pool %q", cmd.Pool)
		}
		if cmd.Limit < 1 || cmd.Limit > 1_000_000 {
			return "", nil, errors.New("limit must be between 1 and 1000000")
		}
		effect = fmt.Sprintf("Resize %s / %s from %d to %d slots.", cmd.Provider, cmd.Pool, pl.limiter.Limit(), cmd.Limit)
		consequences = append(consequences, "Existing leases and cooldowns are retained. A smaller limit creates debt until occupied slots fall below it.", "A larger limit may immediately admit FIFO waiters. Default, route and global pools are separate.")
	case Cancel:
		if c.cancel == nil {
			return "", nil, errors.New("live request cancellation is unavailable")
		}
		if cmd.RequestID == "" || len(cmd.RequestID) > 128 {
			return "", nil, errors.New("an exact live request ID is required")
		}
		if cmd.TTL != 0 {
			return "", nil, errors.New("request cancellation cannot have a TTL")
		}
		effect = fmt.Sprintf("Cancel logical request %s.", cmd.RequestID)
		consequences = append(consequences, "Cancellation is cooperative and preserves failure/cancel holds; it does not prove that the remote model has stopped.")
	default:
		return "", nil, errors.New("unsupported runtime action")
	}
	if cmd.Action != Resize && (cmd.Pool != "" || cmd.Limit != 0) {
		return "", nil, errors.New("pool and limit apply only to resize")
	}
	if cmd.Action != Cancel && cmd.RequestID != "" {
		return "", nil, errors.New("request ID applies only to cancel")
	}
	if cmd.TTL > 0 {
		consequences = append(consequences, fmt.Sprintf("Expires after %s and restores the current non-expiring process baseline unless superseded.", cmd.TTL))
	} else if cmd.Action != Cancel {
		consequences = append(consequences, "This becomes the process baseline; restarting reloads CLI configuration.")
	}
	return effect, consequences, nil
}

// Apply compares the exact revision and consumes the frozen command. Repeating
// the same applied token returns the original result without applying it twice.
func (c *Controller) Apply(token string) (Result, error) {
	if c == nil {
		return Result{}, ErrClosed
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Result{}, ErrClosed
	}
	pr, ok := c.prepared[token]
	if !ok {
		c.mu.Unlock()
		return Result{}, ErrUnknownPreview
	}
	if pr.result != nil {
		result := *pr.result
		c.mu.Unlock()
		return result, nil
	}
	now := time.Now()
	if !now.Before(pr.preview.ExpiresAt) {
		c.mu.Unlock()
		return Result{}, ErrExpired
	}
	if pr.preview.Revision != c.revision {
		c.mu.Unlock()
		return Result{}, ErrStale
	}
	cmd := pr.preview.Command
	p := c.providers[cmd.Provider]
	switch cmd.Action {
	case Pause, Resume, Drain:
		p.generation++
		generation := p.generation
		if p.timer != nil {
			p.timer.Stop()
		}
		switch cmd.Action {
		case Pause:
			p.state = Paused
		case Resume:
			p.state = Running
		case Drain:
			p.state = Draining
		}
		p.expires = time.Time{}
		if cmd.TTL == 0 {
			p.baseline = p.state
		} else {
			p.expires = now.Add(cmd.TTL)
			p.timer = time.AfterFunc(cmd.TTL, func() { c.expireProvider(cmd.Provider, generation) })
		}
		close(p.changed)
		p.changed = make(chan struct{})
	case Resize:
		pl := p.pools[cmd.Pool]
		revision, resizeErr := pl.limiter.ResizeIfRevision(cmd.Limit, pr.poolRevision)
		if err := resizeErr; err != nil {
			c.mu.Unlock()
			if errors.Is(err, queue.ErrStaleRevision) {
				return Result{}, ErrStale
			}
			return Result{}, err
		}
		pl.revision = revision
		pl.generation++
		generation := pl.generation
		if pl.timer != nil {
			pl.timer.Stop()
		}
		pl.expires = time.Time{}
		if cmd.TTL == 0 {
			pl.baseline = cmd.Limit
		} else {
			pl.expires = now.Add(cmd.TTL)
			pl.timer = time.AfterFunc(cmd.TTL, func() { c.expirePool(cmd.Provider, cmd.Pool, generation) })
		}
	case Cancel:
		if c.cancel == nil || !c.cancel(cmd.Provider, cmd.RequestID) {
			c.mu.Unlock()
			return Result{}, errors.New("request is no longer live")
		}
	}
	c.revision++
	result := Result{Token: token, Revision: c.revision, At: now, Command: cmd, Summary: pr.preview.Effect, Applied: true}
	pr.result = &result
	audit := Audit{Result: result, Kind: "applied"}
	c.appendAuditLocked(audit)
	callback := c.onChange
	c.mu.Unlock()
	if callback != nil {
		callback(audit)
	}
	return result, nil
}

func (c *Controller) expireProvider(name string, generation uint64) {
	c.mu.Lock()
	p := c.providers[name]
	if c.closed || p == nil || p.generation != generation || p.expires.IsZero() {
		c.mu.Unlock()
		return
	}
	p.state = p.baseline
	p.expires = time.Time{}
	p.generation++
	close(p.changed)
	p.changed = make(chan struct{})
	c.revision++
	audit := Audit{Kind: "expired", Revision: c.revision, At: time.Now(), Command: Command{Provider: name}, Summary: fmt.Sprintf("%s restored to %s after expiry.", name, p.state), Applied: true}
	c.appendAuditLocked(audit)
	callback := c.onChange
	c.mu.Unlock()
	if callback != nil {
		callback(audit)
	}
}
func (c *Controller) expirePool(name, id string, generation uint64) {
	c.mu.Lock()
	p := c.providers[name]
	if c.closed || p == nil {
		c.mu.Unlock()
		return
	}
	pl := p.pools[id]
	if pl == nil || pl.generation != generation || pl.expires.IsZero() {
		c.mu.Unlock()
		return
	}
	_, resizeErr := pl.limiter.ResizeIfRevision(pl.baseline, pl.revision)
	pl.expires = time.Time{}
	pl.generation++
	c.revision++
	audit := Audit{Kind: "expired", Revision: c.revision, At: time.Now(), Command: Command{Action: Resize, Provider: name, Pool: id, Limit: pl.baseline}, Summary: fmt.Sprintf("%s / %s restored to %d slots after expiry.", name, id, pl.baseline), Applied: true}
	if resizeErr != nil {
		audit.Kind = "superseded"
		audit.Applied = false
		audit.Summary = fmt.Sprintf("%s / %s expiry skipped because its configured limit changed independently.", name, id)
	}
	c.appendAuditLocked(audit)
	callback := c.onChange
	c.mu.Unlock()
	if callback != nil {
		callback(audit)
	}
}
func (c *Controller) appendAuditLocked(a Audit) {
	if len(c.audit) == HistoryLimit {
		copy(c.audit, c.audit[1:])
		c.audit = c.audit[:HistoryLimit-1]
	}
	c.audit = append(c.audit, a)
}

func (c *Controller) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Snapshot{Revision: c.revision, Audit: slices.Clone(c.audit)}
	for name, p := range c.providers {
		ps := ProviderState{Name: name, State: p.state, Baseline: p.baseline, ExpiresAt: p.expires}
		for id, pl := range p.pools {
			st := pl.limiter.Stats()
			ps.Pools = append(ps.Pools, PoolState{ID: id, Limit: st.Limit, Baseline: pl.baseline, Effective: st.Effective, Active: st.Active, Occupied: st.Occupied, Cooldown: st.Cooldown, Waiters: st.Waiters, Debt: st.Debt, Withheld: st.Withheld, ExpiresAt: pl.expires})
		}
		slices.SortFunc(ps.Pools, func(a, b PoolState) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		s.Providers = append(s.Providers, ps)
	}
	slices.SortFunc(s.Providers, func(a, b ProviderState) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return s
}

func (c *Controller) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for _, p := range c.providers {
		if p.timer != nil {
			p.timer.Stop()
		}
		for _, pl := range p.pools {
			if pl.timer != nil {
				pl.timer.Stop()
			}
		}
		close(p.changed)
	}
}
