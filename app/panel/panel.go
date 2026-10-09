// Package panel serves a small web page for inspecting and switching
// balancers. It is mounted on the metrics HTTP server.
package panel

import (
	"context"
	_ "embed"
	"encoding/json"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/app/observatory/burst"
	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/errors"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/extension"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/routing"
)

//go:embed index.html
var indexHTML []byte

// Router is the part of app/router the panel relies on.
type Router interface {
	routing.Router
	routing.BalancerOverrider
	ListBalancers() ([]router.BalancerState, error)
	ListRuleTargets() []router.RuleTarget
	SetRuleBalancer(ruleTag, balancerTag string) error
}

// Checker is implemented by the burst observatory, which can probe on demand.
type Checker interface {
	Check(tags []string)
	LatestResults(tags []string) map[string]burst.LatestResult
}

type handler struct {
	router      Router
	observatory extension.Observatory // nil when no observatory is configured
	inbounds    inbound.Manager
}

// New returns the panel handler for the Xray instance in ctx. It fails when
// the instance does not use the built-in router.
func New(ctx context.Context) (http.Handler, error) {
	v := core.MustFromContext(ctx)
	r, ok := v.GetFeature(routing.RouterType()).(Router)
	if !ok {
		return nil, errors.New("panel requires the built-in router")
	}
	h := &handler{router: r, inbounds: v.GetFeature(inbound.ManagerType()).(inbound.Manager)}
	if o, ok := v.GetFeature(extension.ObservatoryType()).(extension.Observatory); ok {
		h.observatory = o
	}
	return h.routes(), nil
}

func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("POST /api/override", h.handleOverride)
	mux.HandleFunc("POST /api/rule", h.handleRule)
	mux.HandleFunc("POST /api/route", h.handleRoute)
	mux.HandleFunc("POST /api/check", h.handleCheck)
	return guard(mux)
}

// guard only admits loopback Host headers, which blocks DNS rebinding, and
// requires JSON bodies on POST, so cross-origin pages cannot trigger actions
// without a CORS preflight.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type ruleView struct {
	RuleTag  string `json:"ruleTag"`
	Outbound string `json:"outbound,omitempty"`
	Balancer string `json:"balancer,omitempty"`
}

type balancerView struct {
	Tag         string   `json:"tag"`
	Selectors   []string `json:"selectors"`
	Strategy    string   `json:"strategy"`
	FallbackTag string   `json:"fallbackTag,omitempty"`
	Candidates  []string `json:"candidates"`
	Override    string   `json:"override,omitempty"`
	Principle   []string `json:"principle"`
}

type sampleView struct {
	At     int64 `json:"at"` // unix milliseconds
	Ms     int64 `json:"ms"`
	Failed bool  `json:"failed"`
}

type nodeView struct {
	Alive     bool        `json:"alive"`
	Delay     int64       `json:"delay"`
	LastError string      `json:"lastError,omitempty"`
	PingAll   int64       `json:"pingAll"`
	PingFail  int64       `json:"pingFail"`
	PingAvgMs int64       `json:"pingAvgMs"`
	PingMinMs int64       `json:"pingMinMs"`
	PingMaxMs int64       `json:"pingMaxMs"`
	HasPing   bool        `json:"hasPing"`
	Latest    *sampleView `json:"latest,omitempty"`
}

type statusView struct {
	Time             int64                `json:"time"`
	Rules            []ruleView           `json:"rules"`
	Balancers        []balancerView       `json:"balancers"`
	Inbounds         []string             `json:"inbounds"`
	Observatory      map[string]*nodeView `json:"observatory,omitempty"`
	ObservatoryError string               `json:"observatoryError,omitempty"`
	CanCheck         bool                 `json:"canCheck"`
}

func (h *handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	balancers, err := h.router.ListBalancers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	view := statusView{
		Time:      time.Now().Unix(),
		Rules:     []ruleView{},
		Balancers: []balancerView{},
		Inbounds:  []string{},
	}
	for _, t := range h.router.ListRuleTargets() {
		view.Rules = append(view.Rules, ruleView{RuleTag: t.RuleTag, Outbound: t.OutboundTag, Balancer: t.BalancerTag})
	}
	for _, b := range balancers {
		view.Balancers = append(view.Balancers, balancerView{
			Tag:         b.Tag,
			Selectors:   nonNil(b.Selectors),
			Strategy:    b.Strategy,
			FallbackTag: b.FallbackTag,
			Candidates:  nonNil(b.Candidates),
			Override:    b.Override,
			Principle:   nonNil(b.Principle),
		})
	}
	for _, ib := range h.inbounds.ListHandlers(r.Context()) {
		if tag := ib.Tag(); tag != "" {
			view.Inbounds = append(view.Inbounds, tag)
		}
	}

	if h.observatory == nil {
		view.ObservatoryError = "no observatory or burstObservatory is configured"
	} else if nodes, err := h.observe(r.Context()); err != nil {
		view.ObservatoryError = err.Error()
	} else {
		view.Observatory = nodes
		_, view.CanCheck = h.observatory.(Checker)
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) observe(ctx context.Context) (map[string]*nodeView, error) {
	msg, err := h.observatory.GetObservation(ctx)
	if err != nil {
		return nil, err
	}
	result, ok := msg.(*observatory.ObservationResult)
	if !ok {
		return nil, errors.New("unexpected observation type")
	}
	nodes := make(map[string]*nodeView, len(result.Status))
	tags := make([]string, 0, len(result.Status))
	for _, s := range result.Status {
		n := &nodeView{Alive: s.Alive, Delay: s.Delay, LastError: s.LastErrorReason}
		// Burst observatory reports health ping values as time.Duration (ns).
		if hp := s.HealthPing; hp != nil {
			n.HasPing = true
			n.PingAll, n.PingFail = hp.All, hp.Fail
			n.PingAvgMs = time.Duration(hp.Average).Milliseconds()
			n.PingMinMs = time.Duration(hp.Min).Milliseconds()
			n.PingMaxMs = time.Duration(hp.Max).Milliseconds()
		}
		nodes[s.OutboundTag] = n
		tags = append(tags, s.OutboundTag)
	}
	if c, ok := h.observatory.(Checker); ok {
		for tag, latest := range c.LatestResults(tags) {
			nodes[tag].Latest = toSample(latest)
		}
	}
	return nodes, nil
}

func toSample(r burst.LatestResult) *sampleView {
	return &sampleView{At: r.Time.UnixMilli(), Ms: r.RTT.Milliseconds(), Failed: r.Failed}
}

func (h *handler) handleOverride(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Balancer string `json:"balancer"`
		Target   string `json:"target"` // empty clears the override
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := h.router.SetOverrideTarget(req.Balancer, req.Target); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	errors.LogInfo(r.Context(), "panel: balancer ", req.Balancer, " override set to [", req.Target, "]")
	writeJSON(w, http.StatusOK, struct{}{})
}

func (h *handler) handleRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RuleTag  string `json:"ruleTag"`
		Balancer string `json:"balancer"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := h.router.SetRuleBalancer(req.RuleTag, req.Balancer); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	errors.LogInfo(r.Context(), "panel: rule ", req.RuleTag, " now uses balancer ", req.Balancer)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (h *handler) handleRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target  string `json:"target"` // host:port
		Network string `json:"network"`
		Inbound string `json:"inbound"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	host, portText, err := net.SplitHostPort(req.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid port ", portText))
		return
	}
	rc := &routercmd.RoutingContext{InboundTag: req.Inbound, TargetPort: uint32(port)}
	switch req.Network {
	case "tcp":
		rc.Network = xnet.Network_TCP
	case "udp":
		rc.Network = xnet.Network_UDP
	default:
		writeError(w, http.StatusBadRequest, errors.New("unknown network ", req.Network))
		return
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
		}
		rc.TargetIPs = [][]byte{ip}
	} else {
		rc.TargetDomain = host
	}

	route, err := h.router.PickRoute(routercmd.AsRoutingContext(rc))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Outbound string `json:"outbound"`
		RuleTag  string `json:"ruleTag"`
	}{route.GetOutboundTag(), route.GetRuleTag()})
}

// handleCheck runs one burst observatory probe per tag and waits for it.
// The samples feed the balancers' selection like scheduled probes do.
func (h *handler) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tags []string `json:"tags"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	checker, ok := h.observatory.(Checker)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("on-demand checks need burstObservatory"))
		return
	}
	if len(req.Tags) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no tags to check"))
		return
	}
	// Only probe observed outbounds: a probe for any other tag would add a
	// stray result that lingers until the next scheduled round.
	nodes, err := h.observe(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, tag := range req.Tags {
		if _, found := nodes[tag]; !found {
			writeError(w, http.StatusBadRequest, errors.New("outbound ", tag, " is not observed by burstObservatory"))
			return
		}
	}

	start := time.Now()
	checker.Check(req.Tags)
	results := map[string]*sampleView{}
	for tag, latest := range checker.LatestResults(req.Tags) {
		// Probes are not recorded while the local network is down.
		if !latest.Time.Before(start) {
			results[tag] = toSample(latest)
		}
	}
	writeJSON(w, http.StatusOK, results)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, struct {
		Error string `json:"error"`
	}{err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		errors.LogInfoInner(context.Background(), err, "panel: failed to write response")
	}
}
