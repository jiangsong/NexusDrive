package control

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/trigger"
)

// TriggerControl is what the control plane needs from the trigger engine:
// the rules and agents it was built from, the delivery queue, and the two
// actions the console offers. *trigger.Engine implements it; a daemon that
// runs no engine (no rules, or not the owner of agent.db) leaves
// Collector.Trigger nil, /triggers then answers {"enabled":false} and every
// other trigger route 404.
//
// Retry goes through the engine and never straight to the DAO: the engine
// caches which (rule, path) pairs are pending, and a row reopened behind
// its back would be merged into instead of run.
type TriggerControl interface {
	Rules() []config.Trigger
	Agents() []config.Agent
	Deliveries(ctx context.Context, q agent.DeliveryQuery) ([]agent.Delivery, string, error)
	Delivery(ctx context.Context, id int64) (agent.Delivery, error)
	// Test queues rule on path, due now, and returns the delivery id.
	Test(ctx context.Context, rule, path string) (int64, error)
	// Retry reopens a dead delivery; agent.ErrDeliveryNotDead otherwise.
	Retry(ctx context.Context, id int64) error
	Counts(ctx context.Context) (pending, dead int, err error)
}

// TriggersResponse is GET /triggers: the rules as the console renders them,
// read-only, and the agents by name only.
type TriggersResponse struct {
	Enabled bool          `json:"enabled"`
	Rules   []TriggerView `json:"rules"`
	Agents  []AgentName   `json:"agents"`
}

// TriggerView is one rule. Durations are strings ("2s") so the page shows
// them as the file spelt them.
type TriggerView struct {
	Name     string            `json:"name"`
	Paths    []string          `json:"paths"`
	Events   []string          `json:"events"`
	Origins  []string          `json:"origins"`
	Debounce string            `json:"debounce"`
	OnRescan string            `json:"on_rescan"`
	Action   TriggerActionView `json:"action"`
}

// TriggerActionView holds the one action a rule has, with Type naming it.
type TriggerActionView struct {
	Type    string       `json:"type"`
	Exec    *ExecView    `json:"exec,omitempty"`
	Webhook *WebhookView `json:"webhook,omitempty"`
}

// ExecView is the argv as separate elements, never joined: the page renders
// each one on its own so nothing reads as a shell line.
type ExecView struct {
	Command []string `json:"command"`
	Cwd     string   `json:"cwd,omitempty"`
	Timeout string   `json:"timeout"`
}

// WebhookView is the webhook without its secret. SecretConfigured is all
// the page learns about the signing key; neither the keyring reference nor
// the key itself is ever serialized.
type WebhookView struct {
	URL                string `json:"url"`
	SecretConfigured   bool   `json:"secret_configured"`
	Timeout            string `json:"timeout"`
	IncludeDownloadURL bool   `json:"include_download_url"`
	Proxy              string `json:"proxy,omitempty"`
	Insecure           bool   `json:"insecure"`
}

// AgentName is an agent as the console sees it: the name, which is all a
// run needs; the command stays in the configuration file.
type AgentName struct {
	Name string `json:"name"`
}

// DeliveriesResponse is GET /triggers/deliveries. Rows carry no output;
// the detail route does.
type DeliveriesResponse struct {
	Deliveries []agent.Delivery `json:"deliveries"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// TriggerTestRequest is POST /triggers/test.
type TriggerTestRequest struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Confirm bool   `json:"confirm"`
}

// TriggerRetryRequest is POST /triggers/retry.
type TriggerRetryRequest struct {
	ID int64 `json:"id"`
}

// TriggerActionResponse is what test and retry answer: the delivery to
// follow.
type TriggerActionResponse struct {
	ID int64 `json:"id"`
}

// TriggerEvent is the "trigger" SSE frame: what a table row needs to
// redraw itself.
type TriggerEvent struct {
	ID       int64  `json:"id"`
	Rule     string `json:"rule"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
}

// TriggerStatus is the triggers line of /status.
type TriggerStatus struct {
	Enabled bool `json:"enabled"`
	Pending int  `json:"pending"`
	Dead    int  `json:"dead"`
}

const (
	defaultDeliveriesLimit = 50
	maxDeliveriesLimit     = 500
	maxTriggerRequest      = 16 << 10
)

// TriggerViewOf renders one rule for the console; the CLI uses it offline
// so `cloudfs triggers list` prints the same thing with or without a
// daemon.
func TriggerViewOf(r config.Trigger) TriggerView {
	v := TriggerView{
		Name: r.Name, Paths: nonNil(r.Paths), Events: nonNil(r.Events), Origins: nonNil(r.Origins),
		Debounce: r.Debounce.String(), OnRescan: r.OnRescan,
	}
	switch {
	case r.Action.Exec != nil:
		v.Action = TriggerActionView{Type: "exec", Exec: &ExecView{
			Command: nonNil(r.Action.Exec.Command), Cwd: r.Action.Exec.Cwd, Timeout: r.Action.Exec.Timeout.String(),
		}}
	case r.Action.Webhook != nil:
		w := r.Action.Webhook
		v.Action = TriggerActionView{Type: "webhook", Webhook: &WebhookView{
			URL: w.URL, SecretConfigured: w.Secret != "", Timeout: w.Timeout.String(),
			IncludeDownloadURL: w.IncludeDownloadURL, Proxy: w.Proxy, Insecure: w.Insecure,
		}}
	}
	return v
}

// TriggersResponseOf builds the rules view from configuration values.
func TriggersResponseOf(enabled bool, rules []config.Trigger, agents []config.Agent) TriggersResponse {
	out := TriggersResponse{Enabled: enabled, Rules: make([]TriggerView, 0, len(rules)), Agents: make([]AgentName, 0, len(agents))}
	for _, r := range rules {
		out.Rules = append(out.Rules, TriggerViewOf(r))
	}
	for _, a := range agents {
		out.Agents = append(out.Agents, AgentName{Name: a.Name})
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// triggerEventOf is the SSE frame for a delivery row.
func triggerEventOf(d agent.Delivery) TriggerEvent {
	return TriggerEvent{ID: d.ID, Rule: d.Rule, State: d.State, Attempts: d.Attempts, Path: d.Path, Kind: d.Kind}
}

// triggerReady is the guard every /triggers route but the rules view starts
// with: the request is local, the method is right and an engine runs.
func (s *Server) triggerReady(w http.ResponseWriter, r *http.Request, method string) (TriggerControl, bool) {
	if !privateRequest(w, r) {
		return nil, false
	}
	if !allowMethod(w, r, method) {
		return nil, false
	}
	if s.collector.Trigger == nil {
		httpErrorT(w, r, http.StatusNotFound, "err.triggers_disabled")
		return nil, false
	}
	return s.collector.Trigger, true
}

// triggerError maps an engine or queue error onto a status code.
func triggerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, trigger.ErrUnknownRule):
		httpErrorT(w, r, http.StatusNotFound, "err.trigger_not_found")
	case errors.Is(err, agent.ErrDeliveryNotFound):
		httpErrorT(w, r, http.StatusNotFound, "err.delivery_not_found")
	case errors.Is(err, agent.ErrDeliveryNotDead):
		httpErrorT(w, r, http.StatusConflict, "err.delivery_not_dead")
	case errors.Is(err, agent.ErrInvalidCursor):
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
	default:
		httpErrorT(w, r, http.StatusInternalServerError, "err.trigger_failed", err)
	}
}

// GET /triggers
func (s *Server) triggers(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	tc := s.collector.Trigger
	if tc == nil {
		writeJSON(w, TriggersResponseOf(false, nil, nil))
		return
	}
	writeJSON(w, TriggersResponseOf(true, tc.Rules(), tc.Agents()))
}

// GET /triggers/deliveries?cursor=&rule=&state=&limit=
func (s *Server) triggerDeliveries(w http.ResponseWriter, r *http.Request) {
	tc, ok := s.triggerReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	params := r.URL.Query()
	q := agent.DeliveryQuery{Cursor: params.Get("cursor"), Rule: params.Get("rule"), State: params.Get("state")}
	if q.State != "" && !knownDeliveryState(q.State) {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	if q.Limit, ok = queryLimit(w, r, defaultDeliveriesLimit, maxDeliveriesLimit); !ok {
		return
	}
	rows, next, err := tc.Deliveries(r.Context(), q)
	if err != nil {
		triggerError(w, r, err)
		return
	}
	writeJSON(w, DeliveriesResponse{Deliveries: deliveryRows(rows), NextCursor: next})
}

// deliveryRows strips the output from a page: a table of a few hundred
// rows must not carry a few hundred 128 KiB outputs. Truncated is kept, so
// the row can hint that the detail is worth opening.
func deliveryRows(rows []agent.Delivery) []agent.Delivery {
	out := make([]agent.Delivery, 0, len(rows))
	for _, d := range rows {
		d.Output = ""
		out = append(out, d)
	}
	return out
}

func knownDeliveryState(state string) bool {
	for _, s := range agent.DeliveryStates {
		if s == state {
			return true
		}
	}
	return false
}

// GET /triggers/deliveries/<id>
func (s *Server) triggerDeliveryByPath(w http.ResponseWriter, r *http.Request) {
	tc, ok := s.triggerReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/triggers/deliveries/")
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	d, err := tc.Delivery(r.Context(), id)
	if err != nil {
		triggerError(w, r, err)
		return
	}
	writeJSON(w, d)
}

// POST /triggers/test {"name":"inbox","path":"/work/a.txt","confirm":true}
//
// The delivery runs the rule's action for real — the exec command or the
// signed webhook — which is why it is confirmed like a deletion.
func (s *Server) triggerTest(w http.ResponseWriter, r *http.Request) {
	tc, ok := s.triggerReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q TriggerTestRequest
	if !decodeMutationLimit(w, r, &q, maxTriggerRequest) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	if !ruleConfigured(tc, q.Name) {
		httpErrorT(w, r, http.StatusNotFound, "err.trigger_not_found")
		return
	}
	if !confirmed(w, r, q.Confirm, "confirm.trigger.test", q.Name, p) {
		return
	}
	id, err := tc.Test(r.Context(), q.Name, p)
	if err != nil {
		triggerError(w, r, err)
		return
	}
	writeJSON(w, TriggerActionResponse{ID: id})
}

// ruleConfigured answers before the confirmation prompt, so a typo in the
// name is a 404 and not a prompt to confirm something that does not exist.
func ruleConfigured(tc TriggerControl, name string) bool {
	if name == "" {
		return false
	}
	for _, r := range tc.Rules() {
		if r.Name == name {
			return true
		}
	}
	return false
}

// POST /triggers/retry {"id":12}
func (s *Server) triggerRetry(w http.ResponseWriter, r *http.Request) {
	tc, ok := s.triggerReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q TriggerRetryRequest
	if !decodeMutationLimit(w, r, &q, maxTriggerRequest) {
		return
	}
	if q.ID <= 0 {
		httpErrorT(w, r, http.StatusNotFound, "err.delivery_not_found")
		return
	}
	if err := tc.Retry(r.Context(), q.ID); err != nil {
		triggerError(w, r, err)
		return
	}
	writeJSON(w, TriggerActionResponse{ID: q.ID})
}
