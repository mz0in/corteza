package gatekeep

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cortezaproject/corteza/server/pkg/id"
	"go.uber.org/zap"
)

type (
	service struct {
		mux sync.RWMutex

		store        store
		queueManager *queueManager

		events eventManager

		logger *zap.Logger
	}

	EventListener func(evt Event)
	Event         struct {
		Kind ebEvent
		Lock Lock
	}

	eventManager interface {
		Subscribe(listener EventListener) int
		Unsubscribe(int)
		Publish(event Event)
	}

	Constraint struct {
		id uint64

		Resource  string
		Operation Operation
		UserID    uint64
		Overwrite bool
		Await     time.Duration
		ExpiresIn time.Duration

		queuedAt time.Time
	}

	queue struct {
		queue []Constraint
	}

	queueManager struct {
		mux    sync.Mutex
		queues map[string]*queue
	}

	store interface {
		GetValue(ctx context.Context, key string) ([]byte, error)
		SetValue(ctx context.Context, key string, v []byte) error
		DeleteValue(ctx context.Context, key string) error
	}

	Lock struct {
		ID        uint64    `json:"id,string"`
		UserID    uint64    `json:"userID,string"`
		CreatedAt time.Time `json:"createdAt"`
		Resource  string    `json:"resource"`
		Operation Operation `json:"operation"`

		State LockState `json:"state"`

		LockDuration time.Duration `json:"lockDuration"`
		AcquiredAt   time.Time     `json:"acquiredAt"`
	}

	ebEvent int

	LockState string
	Operation string
)

const (
	OpRead  Operation = "read"
	OpWrite Operation = "write"
)

const (
	lockStateNil    LockState = ""
	lockStateLocked LockState = "locked"
	lockStateFailed LockState = "failed"
	lockStateQueued LockState = "queued"
)

const (
	EbEventLockResolved ebEvent = iota
	EbEventLockReleased
)

var (
	gSvc *service

	// wrapper around id.Next() that will aid service testing
	nextID = func() uint64 {
		return id.Next()
	}
)

// New creates a DAL service with the primary connection
//
// It needs an established and working connection to the primary store
func New(log *zap.Logger, s store) (*service, error) {
	svc := &service{
		mux:    sync.RWMutex{},
		logger: log,
		store:  s,

		queueManager: &queueManager{
			mux:    sync.Mutex{},
			queues: make(map[string]*queue),
		},

		events: &inMemBus{},
	}
	return svc, nil
}

func Initialized() bool {
	return gSvc != nil
}

// Service returns the global initialized DAL service
//
// Function will panic if DAL service is not set (via SetGlobal)
func Service() *service {
	if gSvc == nil {
		panic("gatekeep global service not initialized: call gatekeep.SetGlobal first")
	}

	return gSvc
}

func SetGlobal(svc *service, err error) {
	if err != nil {
		panic(err)
	}

	gSvc = svc
}

// Lock attempts to acquire a lock conforming to the given constraints
//
// If a lock can't be acquired the request will either be queued or fail
// (if the .Await field is not set)
//
// The function doesn't block/wait for the lock to be acquired; that needs
// to be done by the caller
func (svc *service) Lock(ctx context.Context, c Constraint) (l Lock, err error) {
	svc.mux.Lock()
	defer svc.mux.Unlock()

	// Probe existing resource locks so we can figure out what we can do
	ll, err := svc.probeResource(ctx, c.Resource)
	if err != nil {
		return
	}

	// Check if we already have this lock so we can potentially extend the lock
	for _, l := range ll {
		if l.matchesConstraints(c) {
			// @todo extending?
			// @todo queued
			return l, nil
		}
	}

	// If we're wanting to acquire a read lock, we can only of there are none
	// or all existing locks are also read locks
	allRead := c.Operation == OpRead
	for _, t := range ll {
		allRead = allRead && t.Operation == OpRead
	}

	// If there are locks and we're not willing to wait, we're done
	if (len(ll) > 0 && !allRead) && c.Await == 0 {
		l = Lock{
			State: lockStateFailed,
		}
		return
	}

	// If there are no locks or all are read locks, we can acquire the lock
	if len(ll) == 0 || allRead {
		l, err = svc.acquireLock(ctx, c)
		if err != nil {
			l.State = lockStateFailed
			return
		}

		l.State = lockStateLocked
		return
	}

	// Queue the lock
	ref, err := svc.queueManager.queueLock(ctx, c)
	l = Lock{
		ID:        ref,
		UserID:    c.UserID,
		Resource:  c.Resource,
		Operation: c.Operation,
		State:     lockStateQueued,
	}

	if err != nil {
		l.State = lockStateFailed
		return
	}

	l.State = lockStateQueued
	return
}

// Unlock releases the lock or unqueues the lock if it's queued
//
// The function won't error out if the lock doesn't exist
// @todo should it?
func (svc *service) Unlock(ctx context.Context, c Constraint) (err error) {
	svc.mux.Lock()
	defer svc.mux.Unlock()
	// releasing a lock may result in other locks being acquirable
	defer svc.doQueued(ctx, c)

	lock, exists, err := svc.check(ctx, c)
	if err != nil {
		return
	}

	if lock.ID == 0 {
		return
	}

	if exists == lockStateLocked {
		err = svc.releaseLock(ctx, c, lock.ID)
	} else if exists == lockStateQueued {
		err = svc.releaseQueued(ctx, c, lock.ID)
	}
	if err != nil {
		return
	}

	svc.Publish(Event{
		Kind: EbEventLockReleased,
		Lock: lock,
	})

	return
}

// ProbeLock returns the current state of the lock
func (svc *service) ProbeLock(ctx context.Context, c Constraint, ref uint64) (state LockState, err error) {
	svc.mux.Lock()
	defer svc.mux.Unlock()

	tt, err := svc.probeResource(ctx, c.Resource)
	if err != nil {
		return
	}

	for _, t := range tt {
		if t.ID == ref {
			return t.State, nil
		}
	}

	return
}

func (svc *service) ProbeResource(ctx context.Context, r string) (tt []Lock, err error) {
	svc.mux.RLock()
	defer svc.mux.RUnlock()

	return svc.probeResource(ctx, r)
}

func (svc *service) Subscribe(listener EventListener) int {
	if svc.events == nil {
		panic("events not initialized")
	}

	return svc.events.Subscribe(listener)
}

func (svc *service) Unsubscribe(id int) {
	if svc.events == nil {
		panic("events not initialized")
	}

	svc.events.Unsubscribe(id)
}

func (svc *service) Publish(event Event) {
	if svc.events == nil {
		panic("events not initialized")
	}

	svc.events.Publish(event)
}

// probeResource returns all of the locks on the given resource
//
// The function returns both already acquired and queued locks
func (svc *service) probeResource(ctx context.Context, r string) (tt []Lock, err error) {
	bits := strings.Split(r, "/")
	schema := bits[0]
	path := bits[1:]

	seen := make(map[string]struct{}, len(path))

	var bb []byte
	var auxOut []Lock

	// Check for all keys that are either as specific or less for the same resource.
	// So if we're probing for a specific module, check all the wildcards corresponding to it.

	for i := len(path); i >= 0; i-- {
		if i <= len(path)-1 {
			path[i] = "*"
		}

		r := fmt.Sprintf("%s/%s", schema, strings.Join(path, "/"))
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}

		// Get the currently stored locks
		bb, err = svc.store.GetValue(ctx, r)
		if err != nil && err.Error() == "not found" {
			err = nil
			continue
		}
		if err != nil {
			return
		}

		err = json.Unmarshal(bb, &auxOut)
		tt = append(tt, auxOut...)
		if err != nil {
			return
		}

		// Get queued locks
		aux := svc.queueManager.queues[r]
		if aux == nil {
			continue
		}

		for _, c := range aux.queue {
			tt = append(tt, Lock{
				ID:        c.id,
				UserID:    c.UserID,
				Resource:  c.Resource,
				Operation: c.Operation,
				State:     lockStateQueued,
			})
		}
	}

	// @todo
	return
}

// check returns the lock reference along with it's state
func (svc *service) check(ctx context.Context, c Constraint) (lock Lock, state LockState, err error) {
	aux, err := svc.probeResource(ctx, c.Resource)
	if err != nil {
		return
	}

	for _, t := range aux {
		if !t.matchesConstraints(c) {
			continue
		}

		return t, t.State, nil
	}

	return lock, lockStateNil, nil
}

func (svc *service) cleanupStore(ctx context.Context) (err error) {
	svc.mux.Lock()
	defer svc.mux.Unlock()

	svc.logger.Debug("cleaning up stale locks")
	defer svc.logger.Debug("cleaned up stale locks")

	// @todo...

	return
}

func (svc *service) cleanupQueues(ctx context.Context) (err error) {
	svc.mux.Lock()
	defer svc.mux.Unlock()

	svc.logger.Debug("cleaning up stale queues")
	defer svc.logger.Debug("cleaned up stale queues")

	qm := svc.queueManager
	if qm == nil {
		return
	}

	qm.mux.Lock()
	defer qm.mux.Unlock()

	// Go backwards and spice out the ones that need to be removed.
	// Broadcast down the buss so we can kill off the watchers.
	now := time.Now()
	for _, qq := range qm.queues {
		for i := len(qq.queue) - 1; i >= 0; i-- {
			c := qq.queue[i]
			l := Lock{
				ID:        c.id,
				UserID:    c.UserID,
				CreatedAt: c.queuedAt,
				Resource:  c.Resource,
				Operation: c.Operation,

				State: lockStateFailed,
			}

			if !c.queuedAt.IsZero() && now.Before(c.queuedAt.Add(c.Await)) {
				continue
			}

			// Splice it out and publish the event
			qq.queue = append(qq.queue[:i], qq.queue[i+1:]...)
			svc.Publish(Event{
				Kind: EbEventLockResolved,
				Lock: l,
			})
		}
	}

	return
}

func (svc *service) Watch(ctx context.Context) {
	tcrGcQueued := time.NewTicker(time.Second * 5)

	// The store ticker is for a greater interval since it's a more hardcore operation
	// @todo potentially keep some in memory index of what's to expire?
	tcrGcStore := time.NewTicker(time.Minute * 5)

	svc.logger.Debug("watcher starting")

	var err error
	go func() {
		for {
			select {
			case <-tcrGcStore.C:
				svc.logger.Debug("tick gc store")

				err = svc.cleanupStore(ctx)
				if err != nil {
					// @todo logging
					svc.logger.Error("cleanup store error", zap.Error(err))
					err = nil
				}

			case <-tcrGcQueued.C:
				svc.logger.Debug("tick cleanup queue")

				err = svc.cleanupQueues(ctx)
				if err != nil {
					// @todo logging
					svc.logger.Error("cleanup error", zap.Error(err))
					err = nil
				}

			case <-ctx.Done():
				svc.logger.Debug("watcher stopping")
				tcrGcQueued.Stop()
				tcrGcStore.Stop()
				return
			}
		}
	}()
}

// @todo we could consider prioritizing some/all read locks over write locks
// so we can have a higher throughput
func (qm *queueManager) queueLock(ctx context.Context, c Constraint) (ref uint64, err error) {
	qm.mux.Lock()
	defer qm.mux.Unlock()

	key := c.Resource

	_, ok := qm.queues[key]
	if !ok {
		qm.queues[key] = &queue{
			queue: make([]Constraint, 0, 24),
		}
	}

	q := qm.queues[key]
	c.id = nextID()
	c.queuedAt = time.Now()
	q.queue = append(q.queue, c)
	qm.queues[key] = q

	return c.id, nil
}

func (svc *service) doQueued(ctx context.Context, c Constraint) (err error) {
	svc.queueManager.mux.Lock()
	defer svc.queueManager.mux.Unlock()

	q := svc.queueManager.queues[c.Resource]
	if q == nil {
		return
	}

	if len(q.queue) == 0 {
		delete(svc.queueManager.queues, c.Resource)
		return
	}

	doReads := q.queue[0].Operation == OpRead

	if !doReads {
		// Check if we can acquire a new one
		qc := q.queue[0]

		// Probe existing resource locks so we can figure out what we can do
		var tt []Lock
		tt, err = svc.probeResource(ctx, qc.Resource)
		if err != nil {
			return
		}

		// Check if we already have this lock so we can potentially extend the lock
		for _, t := range tt {
			if t.ID == qc.id {
				continue
			}

			// If there are any locks and we're trying a write lock; no bueno
			return
		}

		q.queue = q.queue[1:]

		// @todo
		_, err = svc.acquireLock(ctx, qc, qc.id)
		if err != nil {
			svc.logger.Error("queued failed to acquire lock", zap.Error(err))
		}

		return
	}

	var i int
	var qc Constraint
	for i, qc = range q.queue {
		if qc.Operation != OpRead {
			break
		}

		_, err = svc.acquireLock(ctx, qc, qc.id)
		if err != nil {
			svc.logger.Error("queued failed to acquire lock", zap.Error(err))
			err = nil
			continue
		}
	}

	q.queue = q.queue[i:]
	return
}

func (svc *service) acquireLock(ctx context.Context, c Constraint, ids ...uint64) (l Lock, err error) {
	tt := make([]Lock, 0)

	// Get current locks from the store
	// @todo we can probably pass the OG slice around
	baseB, err := svc.store.GetValue(ctx, c.Resource)
	if err != nil && err.Error() != "not found" {
		return
	}

	if len(baseB) > 0 {
		err = json.Unmarshal(baseB, &tt)
		if err != nil {
			return
		}
	}

	id := nextID()
	if len(ids) > 0 {
		id = ids[0]
	}

	l = Lock{
		ID:        id,
		UserID:    c.UserID,
		CreatedAt: time.Now(),
		Resource:  c.Resource,
		Operation: c.Operation,
		State:     lockStateLocked,

		AcquiredAt: time.Now(),
	}
	tt = append(tt, l)

	bb, err := json.Marshal(tt)
	if err != nil {
		return
	}

	err = svc.store.SetValue(ctx, c.Resource, bb)
	if err != nil {
		return
	}

	svc.Publish(Event{
		Kind: EbEventLockResolved,
		Lock: l,
	})

	return
}

// releaseLock removes the lock from the store
func (svc *service) releaseLock(ctx context.Context, c Constraint, ref uint64) (err error) {
	baseB, err := svc.store.GetValue(ctx, c.Resource)
	if err != nil && err.Error() != "not found" {
		return
	}

	tt := make([]Lock, 0)
	if len(baseB) > 0 {
		err = json.Unmarshal(baseB, &tt)
		if err != nil {
			return
		}
	}

	aux := make([]Lock, 0, len(tt))
	for _, t := range tt {
		if t.ID == ref {
			continue
		}
		aux = append(aux, t)
	}

	bb, err := json.Marshal(aux)
	if err != nil {
		return
	}

	return svc.store.SetValue(ctx, c.Resource, bb)
}

// releaseQueued removes the lock from the queue
func (svc *service) releaseQueued(ctx context.Context, c Constraint, ref uint64) (err error) {
	if svc.queueManager.queues == nil {
		return
	}

	svc.queueManager.mux.Lock()
	defer svc.queueManager.mux.Unlock()

	qq := svc.queueManager.queues[c.Resource]
	if qq == nil {
		return
	}

	aux := make([]Constraint, 0, 24)
	for _, q := range qq.queue {
		if q.id == ref {
			continue
		}
		aux = append(aux, q)
	}

	if len(aux) == 0 {
		delete(svc.queueManager.queues, c.Resource)
		return
	}

	svc.queueManager.queues[c.Resource].queue = aux
	return
}

func (t Lock) matchesConstraints(c Constraint) (ok bool) {
	// Can't do anything
	if t.UserID != c.UserID || t.Resource != c.Resource {
		return false
	}

	// If we're grabbing the same operation or a weaker one.
	// If we have a write lock, the read lock is a given.
	if t.Operation == OpWrite || t.Operation == c.Operation {
		return true
	}

	return false
}