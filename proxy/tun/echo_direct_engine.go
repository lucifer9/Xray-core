package tun

import (
	"bytes"
	"context"
	stderrors "errors"
	"net/netip"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/errors"
)

var (
	errEchoProberClosed      = stderrors.New("Echo prober is closed")
	errEchoCarrierUnknown    = stderrors.New("Outbound carrier interface is unknown")
	errEchoOverloaded        = stderrors.New("Direct Echo probe capacity exceeded")
	errEchoRateLimited       = stderrors.New("Direct Echo probe rate exceeded")
	errEchoTransportReplaced = stderrors.New("Direct Echo transport replaced")
	errEchoTimeout           = stderrors.New("Direct Echo probe timed out")
)

const (
	directEchoTimeout          = 5 * time.Second
	directEchoMaxPending       = 1024
	directEchoMaxPendingFamily = 512
	directEchoRatePerSecond    = 256
	directEchoRateBurst        = 512
	unboundEchoGeneration      = ^uint64(0)
)

type echoWireRequest struct {
	Destination netip.Addr
	Identifier  uint16
	Sequence    uint16
	Payload     []byte
}

type echoWireReply struct {
	Source     netip.Addr
	Identifier uint16
	Sequence   uint16
	Payload    []byte
	TTL        uint8
}

type echoTransport interface {
	Send(echoWireRequest) error
	Close() error
}

type echoTransportFactory interface {
	Open(echoFamily, carrierInterface, func(echoWireReply)) (echoTransport, error)
}

type directEchoLimits struct {
	Timeout          time.Duration
	MaxPending       int
	MaxPendingFamily int
	RatePerSecond    float64
	RateBurst        float64
}

type directEchoPendingKey struct {
	family     echoFamily
	generation uint64
	identifier uint16
	sequence   uint16
}

type directEchoPending struct {
	request  echoRequest
	complete func(echoResult)
	timer    echoTimer
}

type echoTimer interface {
	Stop() bool
}

type echoClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) echoTimer
}

type systemEchoClock struct{}

func (systemEchoClock) Now() time.Time { return time.Now() }
func (systemEchoClock) AfterFunc(duration time.Duration, callback func()) echoTimer {
	return time.AfterFunc(duration, callback)
}

type directEchoFamilyState struct {
	carrier     *carrierInterface
	transport   echoTransport
	osTransport echoTransport
	generation  uint64
	nextID      uint16
	pending     int
	retry       echoTimer
}

type directEchoEngine struct {
	mu           sync.Mutex
	closed       bool
	factory      echoTransportFactory
	families     map[echoFamily]*directEchoFamilyState
	pending      map[directEchoPendingKey]*directEchoPending
	limits       directEchoLimits
	tokens       float64
	lastRefill   time.Time
	unsubscribe  func()
	policy       *outboundCarrierPolicy
	retryDelay   time.Duration
	clock        echoClock
	initializing bool
	desired      map[echoFamily]*carrierInterface
}

func newDirectEchoEngine(policy *outboundCarrierPolicy, factory echoTransportFactory) (*directEchoEngine, error) {
	return newDirectEchoEngineWithLimits(policy, factory, directEchoLimits{
		Timeout:          directEchoTimeout,
		MaxPending:       directEchoMaxPending,
		MaxPendingFamily: directEchoMaxPendingFamily,
		RatePerSecond:    directEchoRatePerSecond,
		RateBurst:        directEchoRateBurst,
	})
}

func newDirectEchoEngineWithLimits(policy *outboundCarrierPolicy, factory echoTransportFactory, limits directEchoLimits) (*directEchoEngine, error) {
	return newDirectEchoEngineWithClock(policy, factory, limits, systemEchoClock{})
}

func newDirectEchoEngineWithClock(policy *outboundCarrierPolicy, factory echoTransportFactory, limits directEchoLimits, clock echoClock) (*directEchoEngine, error) {
	engine := &directEchoEngine{
		factory: factory,
		families: map[echoFamily]*directEchoFamilyState{
			echoIPv4: {},
			echoIPv6: {},
		},
		pending:      make(map[directEchoPendingKey]*directEchoPending),
		limits:       limits,
		tokens:       limits.RateBurst,
		lastRefill:   clock.Now(),
		policy:       policy,
		retryDelay:   time.Second,
		clock:        clock,
		initializing: true,
		desired:      make(map[echoFamily]*carrierInterface),
	}
	unsubscribe, snapshots := policy.subscribeWithSnapshot(engine.carrierChanged)
	engine.unsubscribe = unsubscribe
	for _, family := range []echoFamily{echoIPv4, echoIPv6} {
		carrierFamily := carrierIPv6
		if family == echoIPv4 {
			carrierFamily = carrierIPv4
		}
		// A family without an Outbound carrier (for example a single-stack
		// host) degrades instead of aborting startup: its probes fail with
		// errEchoCarrierUnknown until carrierChanged delivers a carrier.
		if carrier := snapshots[carrierFamily]; carrier != nil {
			if err := engine.replaceTransport(family, carrier); err != nil {
				_ = engine.Close()
				return nil, err
			}
		}
		osTransport, err := factory.Open(family, carrierInterface{}, func(reply echoWireReply) {
			engine.handleReply(family, unboundEchoGeneration, reply)
		})
		if err != nil {
			_ = engine.Close()
			return nil, err
		}
		engine.families[family].osTransport = osTransport
	}
	engine.finishInitialization()
	return engine, nil
}

func (e *directEchoEngine) carrierChanged(family carrierFamily, carrier *carrierInterface) {
	echoFamily := echoIPv6
	if family == carrierIPv4 {
		echoFamily = echoIPv4
	}
	e.mu.Lock()
	if e.initializing {
		e.desired[echoFamily] = cloneCarrierInterface(carrier)
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.replaceTransportWithRetry(echoFamily, carrier)
}

func (e *directEchoEngine) finishInitialization() {
	for {
		e.mu.Lock()
		if len(e.desired) == 0 {
			e.initializing = false
			e.mu.Unlock()
			return
		}
		desired := e.desired
		e.desired = make(map[echoFamily]*carrierInterface)
		e.mu.Unlock()
		for family, carrier := range desired {
			e.replaceTransportWithRetry(family, carrier)
		}
	}
}

func (e *directEchoEngine) replaceTransportWithRetry(family echoFamily, carrier *carrierInterface) {
	if err := e.replaceTransport(family, carrier); err != nil {
		errors.LogInfoInner(context.Background(), err, "[tun] failed to replace Direct Echo transport; retrying")
		e.scheduleTransportRetry(family, carrier)
	}
}

func (e *directEchoEngine) Probe(request echoRequest, complete func(echoResult)) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errEchoProberClosed
	}
	state := e.families[request.Family]
	if state == nil || state.carrier == nil {
		e.mu.Unlock()
		return errEchoCarrierUnknown
	}
	transport := state.transport
	generation := state.generation
	if e.policy != nil && e.policy.usesSystemPath(request.Destination) {
		transport = state.osTransport
		generation = unboundEchoGeneration
	}
	if transport == nil {
		e.mu.Unlock()
		return errEchoCarrierUnknown
	}
	if len(e.pending) >= e.limits.MaxPending || state.pending >= e.limits.MaxPendingFamily {
		e.mu.Unlock()
		return errEchoOverloaded
	}
	if !e.takeRateTokenLocked(e.clock.Now()) {
		e.mu.Unlock()
		return errEchoRateLimited
	}
	identifier, ok := e.allocateIdentifierLocked(request.Family, generation, state)
	if !ok {
		e.mu.Unlock()
		return errEchoOverloaded
	}
	key := directEchoPendingKey{family: request.Family, generation: generation, identifier: identifier, sequence: request.Sequence}
	pending := &directEchoPending{request: request, complete: complete}
	pending.request.Payload = append([]byte(nil), request.Payload...)
	e.pending[key] = pending
	state.pending++
	pending.timer = e.clock.AfterFunc(e.limits.Timeout, func() {
		e.finish(key, echoResult{Err: errEchoTimeout})
	})
	e.mu.Unlock()

	if err := transport.Send(echoWireRequest{
		Destination: request.Destination,
		Identifier:  identifier,
		Sequence:    request.Sequence,
		Payload:     append([]byte(nil), request.Payload...),
	}); err != nil {
		e.finish(key, echoResult{Err: err})
		return err
	}
	return nil
}

func (e *directEchoEngine) handleReply(family echoFamily, generation uint64, reply echoWireReply) {
	key := directEchoPendingKey{family: family, generation: generation, identifier: reply.Identifier, sequence: reply.Sequence}
	e.mu.Lock()
	pending := e.pending[key]
	if pending == nil || pending.request.Destination != reply.Source || reply.TTL == 0 || !bytes.Equal(pending.request.Payload, reply.Payload) {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.finish(key, echoResult{Reply: &echoReply{
		Identifier: pending.request.Identifier,
		Sequence:   pending.request.Sequence,
		Payload:    append([]byte(nil), pending.request.Payload...),
		TTL:        reply.TTL,
	}})
}

func (e *directEchoEngine) finish(key directEchoPendingKey, result echoResult) {
	e.mu.Lock()
	pending := e.pending[key]
	if pending == nil {
		e.mu.Unlock()
		return
	}
	delete(e.pending, key)
	if state := e.families[key.family]; state != nil && state.pending > 0 {
		state.pending--
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	e.mu.Unlock()
	pending.complete(result)
}

func (e *directEchoEngine) replaceTransport(family echoFamily, carrier *carrierInterface) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errEchoProberClosed
	}
	state := e.families[family]
	if state.retry != nil {
		state.retry.Stop()
		state.retry = nil
	}
	state.generation++
	generation := state.generation
	old := state.transport
	state.transport = nil
	state.carrier = nil
	var cancelled []directEchoPendingKey
	for key := range e.pending {
		if key.family == family {
			cancelled = append(cancelled, key)
		}
	}
	e.mu.Unlock()

	for _, key := range cancelled {
		e.finish(key, echoResult{Err: errEchoTransportReplaced})
	}
	if old != nil {
		_ = old.Close()
	}
	if carrier == nil {
		return nil
	}

	transport, err := e.factory.Open(family, *carrier, func(reply echoWireReply) {
		e.handleReply(family, generation, reply)
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed || state.generation != generation {
		e.mu.Unlock()
		_ = transport.Close()
		return errEchoTransportReplaced
	}
	carrierCopy := *carrier
	state.carrier = &carrierCopy
	state.transport = transport
	e.mu.Unlock()
	return nil
}

func (e *directEchoEngine) scheduleTransportRetry(family echoFamily, carrier *carrierInterface) {
	if carrier == nil || !e.carrierIsCurrent(family, carrier) {
		return
	}
	carrierCopy := *carrier
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	state := e.families[family]
	state.retry = e.clock.AfterFunc(e.retryDelay, func() {
		if !e.carrierIsCurrent(family, &carrierCopy) {
			return
		}
		if err := e.replaceTransport(family, &carrierCopy); err != nil {
			errors.LogInfoInner(context.Background(), err, "[tun] failed to retry Direct Echo transport replacement")
			e.scheduleTransportRetry(family, &carrierCopy)
		}
	})
	e.mu.Unlock()
}

func (e *directEchoEngine) carrierIsCurrent(family echoFamily, carrier *carrierInterface) bool {
	policyFamily := carrierIPv6
	if family == echoIPv4 {
		policyFamily = carrierIPv4
	}
	return sameCarrierInterface(e.policy.carrier(policyFamily), carrier)
}

func (e *directEchoEngine) allocateIdentifierLocked(family echoFamily, generation uint64, state *directEchoFamilyState) (uint16, bool) {
	for range 1 << 16 {
		identifier := state.nextID
		state.nextID++
		used := false
		for key := range e.pending {
			if key.family == family && key.generation == generation && key.identifier == identifier {
				used = true
				break
			}
		}
		if !used {
			return identifier, true
		}
	}
	return 0, false
}

func (e *directEchoEngine) takeRateTokenLocked(now time.Time) bool {
	elapsed := now.Sub(e.lastRefill).Seconds()
	e.tokens += elapsed * e.limits.RatePerSecond
	if e.tokens > e.limits.RateBurst {
		e.tokens = e.limits.RateBurst
	}
	e.lastRefill = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	return true
}

func (e *directEchoEngine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	unsubscribe := e.unsubscribe
	e.unsubscribe = nil
	transports := make([]echoTransport, 0, len(e.families))
	for _, state := range e.families {
		if state.retry != nil {
			state.retry.Stop()
			state.retry = nil
		}
		if state.transport != nil {
			transports = append(transports, state.transport)
			state.transport = nil
		}
		if state.osTransport != nil {
			transports = append(transports, state.osTransport)
			state.osTransport = nil
		}
	}
	pending := make([]*directEchoPending, 0, len(e.pending))
	for _, item := range e.pending {
		if item.timer != nil {
			item.timer.Stop()
		}
		pending = append(pending, item)
	}
	e.pending = make(map[directEchoPendingKey]*directEchoPending)
	e.mu.Unlock()

	if unsubscribe != nil {
		unsubscribe()
	}
	for _, transport := range transports {
		_ = transport.Close()
	}
	for _, item := range pending {
		item.complete(echoResult{Err: errEchoProberClosed})
	}
	return nil
}
