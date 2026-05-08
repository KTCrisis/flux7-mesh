package approval

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Notifier sends webhook notifications for approval events.
type Notifier struct {
	client    *http.Client
	notifyURL string // config: POST on new pending (for humans)
}

// NewNotifier creates a notifier. notifyURL can be empty to disable.
func NewNotifier(notifyURL string) *Notifier {
	return &Notifier{
		client:    &http.Client{Timeout: 10 * time.Second},
		notifyURL: notifyURL,
	}
}

// notifyPayload is the JSON body sent to webhooks.
type notifyPayload struct {
	Event      string         `json:"event"` // "pending", "approved", "denied", "timeout"
	ID         string         `json:"id"`
	AgentID    string         `json:"agent_id"`
	Tool       string         `json:"tool"`
	Params     map[string]any `json:"params"`
	PolicyRule string         `json:"policy_rule"`
	ResolvedBy string         `json:"resolved_by,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
}

// OnSubmit fires the notify webhook (new pending approval → human).
func (n *Notifier) OnSubmit(pa *PendingApproval) {
	if n == nil || n.notifyURL == "" {
		return
	}
	payload := notifyPayload{
		Event:      "pending",
		ID:         pa.ID,
		AgentID:    pa.AgentID,
		Tool:       pa.Tool,
		Params:     pa.Params,
		PolicyRule: pa.PolicyRule,
		Timestamp:  pa.CreatedAt,
	}
	go n.postUnchecked(n.notifyURL, payload)
}

// OnResolve fires the callback to the agent (if X-Callback-URL was set).
// The callback URL is agent-supplied so we validate against SSRF.
func (n *Notifier) OnResolve(pa *PendingApproval, res Resolution) {
	if n == nil || pa.CallbackURL == "" {
		return
	}
	if !isSafeCallbackURL(pa.CallbackURL) {
		slog.Warn("callback URL rejected (SSRF protection)", "url", pa.CallbackURL, "agent", pa.AgentID)
		return
	}
	payload := notifyPayload{
		Event:      string(res.Status),
		ID:         pa.ID,
		AgentID:    pa.AgentID,
		Tool:       pa.Tool,
		Params:     pa.Params,
		PolicyRule: pa.PolicyRule,
		ResolvedBy: res.ResolvedBy,
		Timestamp:  res.ResolvedAt,
	}
	go n.postUnchecked(pa.CallbackURL, payload)
}

func (n *Notifier) postUnchecked(rawURL string, payload notifyPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("notify marshal failed", "error", err)
		return
	}
	resp, err := n.client.Post(rawURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("notify failed", "url", rawURL, "event", payload.Event, "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn("notify got error status", "url", rawURL, "event", payload.Event, "status", resp.StatusCode)
	}
}

// isSafeCallbackURL rejects non-HTTP(S) schemes, loopback, link-local,
// and private (RFC1918) addresses to prevent SSRF.
func isSafeCallbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Could be a hostname — resolve it
		addrs, err := net.LookupHost(host)
		if err != nil || len(addrs) == 0 {
			return false
		}
		ip = net.ParseIP(addrs[0])
		if ip == nil {
			return false
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}
