package tun

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestDirectEchoEngineRemapsAndMatchesConcurrentProbes(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	clock := newFakeEchoClock()
	engine, err := newDirectEchoEngineWithClock(policy, factory, testEchoLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	destination := netip.MustParseAddr("198.51.100.1")
	results := make(chan echoResult, 2)
	for _, payload := range [][]byte{{1, 2, 3}, {4, 5, 6}} {
		if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: destination, Identifier: 7, Sequence: 9, Payload: payload}, func(result echoResult) {
			results <- result
		}); err != nil {
			t.Fatal(err)
		}
	}
	transport := factory.boundLatest(echoIPv4)
	requests := transport.requests()
	if len(requests) != 2 || requests[0].Identifier == requests[1].Identifier {
		t.Fatalf("wire identifiers were not remapped: %v", requests)
	}
	for i := len(requests) - 1; i >= 0; i-- {
		request := requests[i]
		transport.reply(echoWireReply{Source: destination, Identifier: request.Identifier, Sequence: request.Sequence, Payload: request.Payload, TTL: 37})
	}
	for range 2 {
		result := <-results
		if result.Err != nil || result.Reply == nil || result.Reply.Identifier != 7 || result.Reply.Sequence != 9 || result.Reply.TTL != 37 {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
}

func TestDirectEchoEngineIgnoresUnmatchedRepliesAndTimesOut(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	limits := testEchoLimits()
	limits.Timeout = 5 * time.Second
	clock := newFakeEchoClock()
	engine, err := newDirectEchoEngineWithClock(policy, factory, limits, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	result := make(chan echoResult, 1)
	destination := netip.MustParseAddr("2001:db8::1")
	if err := engine.Probe(echoRequest{Family: echoIPv6, Destination: destination, Identifier: 1, Sequence: 2, Payload: []byte("payload")}, func(got echoResult) {
		result <- got
	}); err != nil {
		t.Fatal(err)
	}
	request := factory.boundLatest(echoIPv6).requests()[0]
	factory.boundLatest(echoIPv6).reply(echoWireReply{Source: netip.MustParseAddr("2001:db8::2"), Identifier: request.Identifier, Sequence: request.Sequence, Payload: request.Payload, TTL: 10})

	clock.Advance(5 * time.Second)
	got := <-result
	if !errors.Is(got.Err, errEchoTimeout) {
		t.Fatalf("result = %+v, want timeout", got)
	}
}

func TestDirectEchoEngineBoundsPendingWork(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	limits := testEchoLimits()
	limits.MaxPending = 1
	limits.MaxPendingFamily = 1
	engine, err := newDirectEchoEngineWithLimits(policy, factory, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	request := echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.1")}
	if err := engine.Probe(request, func(echoResult) {}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Probe(request, func(echoResult) {}); !errors.Is(err, errEchoOverloaded) {
		t.Fatalf("second Probe() error = %v, want overload", err)
	}
}

func TestDirectEchoEngineRateLimit(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	limits := testEchoLimits()
	limits.RateBurst = 1
	limits.RatePerSecond = 1
	clock := newFakeEchoClock()
	engine, err := newDirectEchoEngineWithClock(policy, factory, limits, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	destination := netip.MustParseAddr("198.51.100.1")
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: destination}, func(echoResult) {}); err != nil {
		t.Fatal(err)
	}
	request := factory.boundLatest(echoIPv4).requests()[0]
	factory.boundLatest(echoIPv4).reply(echoWireReply{Source: destination, Identifier: request.Identifier, Sequence: request.Sequence, Payload: request.Payload, TTL: 64})
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: destination}, func(echoResult) {}); !errors.Is(err, errEchoRateLimited) {
		t.Fatalf("second Probe() error = %v, want rate limit", err)
	}
	clock.Advance(time.Second)
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: destination}, func(echoResult) {}); err != nil {
		t.Fatalf("Probe() after deterministic refill error = %v", err)
	}
}

func TestDirectEchoEngineSendFailureProducesNoReplyAndReleasesCapacity(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	engine, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	sendErr := errors.New("send failed")
	factory.boundLatest(echoIPv4).sendErr = sendErr
	result := make(chan echoResult, 1)
	request := echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.1")}
	if err := engine.Probe(request, func(value echoResult) { result <- value }); !errors.Is(err, sendErr) {
		t.Fatalf("Probe() error = %v", err)
	}
	if value := <-result; !errors.Is(value.Err, sendErr) || value.Reply != nil {
		t.Fatalf("send failure result = %+v", value)
	}
	factory.boundLatest(echoIPv4).sendErr = nil
	if err := engine.Probe(request, func(echoResult) {}); err != nil {
		t.Fatalf("capacity was not released after send failure: %v", err)
	}
}

func TestDirectEchoEngineCarrierChangeCancelsOnlyAffectedFamily(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	engine, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	result4 := make(chan echoResult, 1)
	result6 := make(chan echoResult, 1)
	_ = engine.Probe(echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.1")}, func(result echoResult) { result4 <- result })
	_ = engine.Probe(echoRequest{Family: echoIPv6, Destination: netip.MustParseAddr("2001:db8::1")}, func(result echoResult) { result6 <- result })

	policy.update(carrierIPv4, nil)
	if result := <-result4; !errors.Is(result.Err, errEchoTransportReplaced) {
		t.Fatalf("IPv4 result = %+v", result)
	}
	select {
	case result := <-result6:
		t.Fatalf("IPv6 probe was cancelled: %+v", result)
	default:
	}
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.2")}, func(echoResult) {}); !errors.Is(err, errEchoCarrierUnknown) {
		t.Fatalf("probe during carrier loss error = %v", err)
	}

	policy.update(carrierIPv4, &net.Interface{Name: "replacement4", Index: 14, Flags: net.FlagUp})
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.2")}, func(echoResult) {}); err != nil {
		t.Fatalf("probe after carrier recovery error = %v", err)
	}
}

func TestDirectEchoEngineStartsWithoutCarrierAndRecovers(t *testing.T) {
	policy := &outboundCarrierPolicy{subscribers: make(map[uint64]func(carrierFamily, *carrierInterface))}
	policy.update(carrierIPv4, &net.Interface{Name: "carrier4", Index: 4, Flags: net.FlagUp})
	factory := newFakeEchoTransportFactory()
	engine, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	if err := engine.Probe(echoRequest{Family: echoIPv6, Destination: netip.MustParseAddr("2001:db8::1")}, func(echoResult) {}); !errors.Is(err, errEchoCarrierUnknown) {
		t.Fatalf("IPv6 probe without carrier error = %v, want %v", err, errEchoCarrierUnknown)
	}
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.1")}, func(echoResult) {}); err != nil {
		t.Fatalf("IPv4 probe error = %v", err)
	}

	policy.update(carrierIPv6, &net.Interface{Name: "carrier6", Index: 6, Flags: net.FlagUp})
	if err := engine.Probe(echoRequest{Family: echoIPv6, Destination: netip.MustParseAddr("2001:db8::1")}, func(echoResult) {}); err != nil {
		t.Fatalf("IPv6 probe after carrier recovery error = %v", err)
	}
}

func TestDirectEchoEngineRetriesCarrierReplacementFailure(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	clock := newFakeEchoClock()
	engine, err := newDirectEchoEngineWithClock(policy, factory, testEchoLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.retryDelay = 10 * time.Millisecond
	factory.setFailFamily(echoIPv4)
	policy.update(carrierIPv4, &net.Interface{Name: "replacement4", Index: 14, Flags: net.FlagUp})
	clock.Advance(10 * time.Millisecond)
	factory.setFailFamily(0)
	clock.Advance(10 * time.Millisecond)
	if transport := factory.boundLatest(echoIPv4); transport.carrier.Name != "replacement4" {
		t.Fatalf("replacement carrier = %+v", transport.carrier)
	}
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("198.51.100.1")}, func(echoResult) {}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectEchoEngineDoesNotReviveStaleCarrierRetry(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	clock := newFakeEchoClock()
	engine, err := newDirectEchoEngineWithClock(policy, factory, testEchoLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.retryDelay = 15 * time.Millisecond
	factory.setFailFamily(echoIPv4)
	policy.update(carrierIPv4, &net.Interface{Name: "stale4", Index: 14, Flags: net.FlagUp})
	policy.update(carrierIPv4, &net.Interface{Name: "current4", Index: 15, Flags: net.FlagUp})
	factory.setFailFamily(0)
	clock.Advance(15 * time.Millisecond)
	if carrier := factory.boundLatest(echoIPv4).carrier; carrier.Name != "current4" {
		t.Fatalf("current carrier was not installed: %+v", carrier)
	}
	clock.Advance(time.Second)
	if carrier := factory.boundLatest(echoIPv4).carrier; carrier.Name != "current4" {
		t.Fatalf("stale retry replaced current carrier: %+v", carrier)
	}
}

func TestDirectEchoEngineDoesNotOverrideExcludedRoutePrefix(t *testing.T) {
	policy := testCarrierPolicy()
	policy.excluded = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}
	factory := newFakeEchoTransportFactory()
	engine, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("100.64.1.1")}, func(echoResult) {}); err != nil {
		t.Fatalf("excluded probe error = %v", err)
	}
	transport := factory.unbound(echoIPv4)
	if len(transport.requests()) != 1 || transport.carrier.Name != "" || transport.carrier.Index != 0 {
		t.Fatalf("excluded probe did not use OS-selected transport: carrier=%+v requests=%v", transport.carrier, transport.requests())
	}
}

func TestDirectEchoEngineDoesNotBindLoopbackProbe(t *testing.T) {
	for _, test := range []struct {
		name        string
		family      echoFamily
		destination netip.Addr
	}{
		{name: "IPv4", family: echoIPv4, destination: netip.MustParseAddr("127.0.0.1")},
		{name: "IPv6", family: echoIPv6, destination: netip.MustParseAddr("::1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			factory := newFakeEchoTransportFactory()
			engine, err := newDirectEchoEngineWithLimits(testCarrierPolicy(), factory, testEchoLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()

			if err := engine.Probe(echoRequest{Family: test.family, Destination: test.destination}, func(echoResult) {}); err != nil {
				t.Fatalf("loopback probe error = %v", err)
			}
			if requests := factory.unbound(test.family).requests(); len(requests) != 1 || requests[0].Destination != test.destination {
				t.Fatalf("loopback probe did not use OS-selected transport: requests=%v", requests)
			}
			if requests := factory.boundLatest(test.family).requests(); len(requests) != 0 {
				t.Fatalf("loopback probe used carrier-bound transport: requests=%v", requests)
			}
		})
	}
}

func TestDirectEchoEngineExcludedProbeStillFailsClosedOnCarrierLoss(t *testing.T) {
	policy := testCarrierPolicy()
	policy.excluded = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}
	engine, err := newDirectEchoEngineWithLimits(policy, newFakeEchoTransportFactory(), testEchoLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	result := make(chan echoResult, 1)
	request := echoRequest{Family: echoIPv4, Destination: netip.MustParseAddr("100.64.1.1")}
	if err := engine.Probe(request, func(value echoResult) { result <- value }); err != nil {
		t.Fatal(err)
	}
	policy.update(carrierIPv4, nil)
	if value := <-result; !errors.Is(value.Err, errEchoTransportReplaced) {
		t.Fatalf("carrier-loss result = %+v", value)
	}
	if err := engine.Probe(request, func(echoResult) {}); !errors.Is(err, errEchoCarrierUnknown) {
		t.Fatalf("excluded probe during carrier loss error = %v", err)
	}
}

func TestDirectEchoEngineShutdownCompletesPendingWithoutLateReply(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	engine, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan echoResult, 2)
	destination := netip.MustParseAddr("198.51.100.1")
	if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: destination, Payload: []byte("pending")}, func(result echoResult) { results <- result }); err != nil {
		t.Fatal(err)
	}
	request := factory.boundLatest(echoIPv4).requests()[0]
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if result := <-results; !errors.Is(result.Err, errEchoProberClosed) {
		t.Fatalf("shutdown result = %+v", result)
	}
	factory.boundLatest(echoIPv4).reply(echoWireReply{Source: destination, Identifier: request.Identifier, Sequence: request.Sequence, Payload: request.Payload, TTL: 64})
	select {
	case result := <-results:
		t.Fatalf("late reply completed again: %+v", result)
	default:
	}
}

func TestDirectEchoEngineInitializationIsAtomicAcrossFamilies(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	factory.failFamily = echoIPv6
	if _, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits()); err == nil {
		t.Fatal("expected IPv6 transport initialization failure")
	}
	ipv4 := factory.boundLatest(echoIPv4)
	ipv4.mu.Lock()
	closed := ipv4.closed
	ipv4.mu.Unlock()
	if !closed {
		t.Fatal("IPv4 transport remained open after IPv6 initialization failure")
	}
}

func TestDirectEchoEngineConcurrentIdenticalClientIdentity(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	limits := testEchoLimits()
	limits.MaxPending = 128
	limits.MaxPendingFamily = 128
	engine, err := newDirectEchoEngineWithLimits(policy, factory, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const probes = 64
	var submitted sync.WaitGroup
	var completed sync.WaitGroup
	submitted.Add(probes)
	completed.Add(probes)
	errs := make(chan error, probes)
	destination := netip.MustParseAddr("198.51.100.1")
	for i := range probes {
		go func(i int) {
			defer submitted.Done()
			request := echoRequest{
				Family: echoIPv4, Source: netip.AddrFrom4([4]byte{10, 0, byte(i), 1}), Destination: destination,
				Identifier: 7, Sequence: 9, Payload: []byte{byte(i)},
			}
			if err := engine.Probe(request, func(result echoResult) {
				defer completed.Done()
				if result.Err != nil || result.Reply == nil || result.Reply.Identifier != 7 || result.Reply.Sequence != 9 {
					errs <- errors.New("invalid concurrent probe result")
				}
			}); err != nil {
				errs <- err
				completed.Done()
			}
		}(i)
	}
	submitted.Wait()
	requests := factory.boundLatest(echoIPv4).requests()
	if len(requests) != probes {
		t.Fatalf("wire requests = %d, want %d", len(requests), probes)
	}
	identifiers := make(map[uint16]struct{}, probes)
	for _, request := range requests {
		identifiers[request.Identifier] = struct{}{}
		factory.boundLatest(echoIPv4).reply(echoWireReply{Source: destination, Identifier: request.Identifier, Sequence: request.Sequence, Payload: request.Payload, TTL: 64})
	}
	completed.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(identifiers) != probes {
		t.Fatalf("remapped identifiers = %d, want %d", len(identifiers), probes)
	}
}

func BenchmarkDirectEchoEngineRoundTrip(b *testing.B) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	engine, err := newDirectEchoEngineWithLimits(policy, factory, directEchoLimits{Timeout: time.Second, MaxPending: 16, MaxPendingFamily: 16, RatePerSecond: 1e9, RateBurst: 1e9})
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close()
	destination := netip.MustParseAddr("198.51.100.1")
	transport := factory.boundLatest(echoIPv4)
	b.ResetTimer()
	for b.Loop() {
		if err := engine.Probe(echoRequest{Family: echoIPv4, Destination: destination, Identifier: 7, Sequence: 9, Payload: []byte("benchmark")}, func(echoResult) {}); err != nil {
			b.Fatal(err)
		}
		request := transport.takeLastRequest()
		transport.reply(echoWireReply{Source: destination, Identifier: request.Identifier, Sequence: request.Sequence, Payload: request.Payload, TTL: 64})
	}
}

func TestDirectEchoEngineReconcilesCarrierChangeDuringInitialization(t *testing.T) {
	policy := testCarrierPolicy()
	factory := newFakeEchoTransportFactory()
	factory.blockFamily = echoIPv4
	factory.openEntered = make(chan struct{})
	factory.openRelease = make(chan struct{})
	result := make(chan *directEchoEngine, 1)
	errs := make(chan error, 1)
	go func() {
		engine, err := newDirectEchoEngineWithLimits(policy, factory, testEchoLimits())
		result <- engine
		errs <- err
	}()
	<-factory.openEntered
	policy.update(carrierIPv4, &net.Interface{Name: "replacement4", Index: 14, Flags: net.FlagUp})
	close(factory.openRelease)
	engine := <-result
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if carrier := factory.boundLatest(echoIPv4).carrier; carrier.Name != "replacement4" {
		t.Fatalf("initialized transport carrier = %+v, want replacement4", carrier)
	}
}

func testCarrierPolicy() *outboundCarrierPolicy {
	policy := &outboundCarrierPolicy{subscribers: make(map[uint64]func(carrierFamily, *carrierInterface))}
	policy.update(carrierIPv4, &net.Interface{Name: "carrier4", Index: 4, Flags: net.FlagUp})
	policy.update(carrierIPv6, &net.Interface{Name: "carrier6", Index: 6, Flags: net.FlagUp})
	return policy
}

func testEchoLimits() directEchoLimits {
	return directEchoLimits{Timeout: time.Second, MaxPending: 16, MaxPendingFamily: 8, RatePerSecond: 1000, RateBurst: 1000}
}

type fakeEchoTransportFactory struct {
	mu          sync.Mutex
	transports  map[echoFamily][]*fakeEchoTransport
	failFamily  echoFamily
	blockFamily echoFamily
	openEntered chan struct{}
	openRelease chan struct{}
}

func newFakeEchoTransportFactory() *fakeEchoTransportFactory {
	return &fakeEchoTransportFactory{transports: make(map[echoFamily][]*fakeEchoTransport)}
}

func (f *fakeEchoTransportFactory) Open(family echoFamily, carrier carrierInterface, deliver func(echoWireReply)) (echoTransport, error) {
	f.mu.Lock()
	if f.failFamily == family {
		f.mu.Unlock()
		return nil, errors.New("transport initialization failed")
	}
	block := f.blockFamily == family && carrier.Name != ""
	entered, release := f.openEntered, f.openRelease
	if block {
		f.blockFamily = 0
	}
	f.mu.Unlock()
	if block {
		close(entered)
		<-release
	}
	transport := &fakeEchoTransport{carrier: carrier, deliver: deliver}
	f.mu.Lock()
	f.transports[family] = append(f.transports[family], transport)
	f.mu.Unlock()
	return transport, nil
}

func (f *fakeEchoTransportFactory) setFailFamily(family echoFamily) {
	f.mu.Lock()
	f.failFamily = family
	f.mu.Unlock()
}

func (f *fakeEchoTransportFactory) boundLatest(family echoFamily) *fakeEchoTransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.transports[family]
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].carrier.Name != "" {
			return items[i]
		}
	}
	return nil
}

func (f *fakeEchoTransportFactory) unbound(family echoFamily) *fakeEchoTransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.transports[family] {
		if item.carrier.Name == "" {
			return item
		}
	}
	return nil
}

type fakeEchoTransport struct {
	mu      sync.Mutex
	carrier carrierInterface
	deliver func(echoWireReply)
	sent    []echoWireRequest
	closed  bool
	sendErr error
}

func (t *fakeEchoTransport) Send(request echoWireRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errEchoProberClosed
	}
	if t.sendErr != nil {
		return t.sendErr
	}
	t.sent = append(t.sent, request)
	return nil
}

func (t *fakeEchoTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

func (t *fakeEchoTransport) requests() []echoWireRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]echoWireRequest(nil), t.sent...)
}

func (t *fakeEchoTransport) takeLastRequest() echoWireRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	request := t.sent[len(t.sent)-1]
	t.sent = t.sent[:len(t.sent)-1]
	return request
}

func (t *fakeEchoTransport) reply(reply echoWireReply) {
	t.deliver(reply)
}

type fakeEchoClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeEchoTimer
}

type fakeEchoTimer struct {
	clock    *fakeEchoClock
	deadline time.Time
	callback func()
	active   bool
}

func newFakeEchoClock() *fakeEchoClock {
	return &fakeEchoClock{now: time.Unix(1, 0)}
}

func (c *fakeEchoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeEchoClock) AfterFunc(duration time.Duration, callback func()) echoTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeEchoTimer{clock: c, deadline: c.now.Add(duration), callback: callback, active: true}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *fakeEchoClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	var callbacks []func()
	for _, timer := range c.timers {
		if timer.active && !timer.deadline.After(c.now) {
			timer.active = false
			callbacks = append(callbacks, timer.callback)
		}
	}
	c.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}

func (t *fakeEchoTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}
