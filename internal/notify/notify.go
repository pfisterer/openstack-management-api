// Package notify sends events to the notification service (pdns-bwcloud/notifications), which decides who hears of them how. The contract is POST $NOTIFY_URL/v1/events with a bearer token, answered 202. Sending never holds up or fails the request that caused it: events go out from a queue in the background and are tried a few times; without a URL nothing is sent at all.
package notify

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/httpx"
	"go.uber.org/zap"
)

// Source names this service in every event.
const Source = "openstack-management-api"

// Event classes: what the recipient is expected to do.
const (
	ClassAction     = "action"
	ClassDisruption = "disruption"
	ClassInfo       = "info"
)

// Event types sent by this service.
const (
	TypeAllocationExpired   = "allocation.expired"
	TypeAllocationShortened = "allocation.shortened"
	TypeApprovalWaiting     = "approval.waiting"
	TypeApprovalDecided     = "approval.decided"
	TypeProjectEnding       = "project.ending"
)

// Recipient is one person ({"user": e-mail}) or a group of the role provider ({"group": name}).
type Recipient struct {
	User  string `json:"user,omitempty"`
	Group string `json:"group,omitempty"`
}

// Object is what an event is about, with a link into the portal.
type Object struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Event is one notification as the service expects it. Key identifies the matter: the same key again updates it, Resolve closes it.
type Event struct {
	Source     string         `json:"source"`
	Type       string         `json:"type"`
	Key        string         `json:"key"`
	Class      string         `json:"class"`
	Resolve    bool           `json:"resolve"`
	Recipients []Recipient    `json:"recipients"`
	// Actor is the person whose action caused the event (a mail address); empty for what the service does by itself.
	Actor      string         `json:"actor,omitempty"`
	Due        string         `json:"due,omitempty"`
	Object     Object         `json:"object"`
	Data       map[string]any `json:"data"`
}

// Notifier is what the tree service sends through.
type Notifier interface {
	// Enabled reports whether events reach anybody: only then may the service act on the assumption that people were told (cutting an expired allocation after its period).
	Enabled() bool
	// Notify queues an event; it never blocks and never fails.
	Notify(Event)
}

// Nop is the notifier without a notification service.
type Nop struct{}

func (Nop) Enabled() bool { return false }
func (Nop) Notify(Event)  {}

// Client sends events to the notification service.
type Client struct {
	url, token string
	http       *http.Client
	queue      chan Event
	log        *zap.SugaredLogger
	attempts   int
	backoff    time.Duration
	wg         sync.WaitGroup
}

// queueSize bounds what waits to be sent; beyond it events are dropped with a warning rather than piling up while the service is down.
const queueSize = 512

// New returns a client sending to url (the service's base URL) with token, or Nop when url is empty. The sender runs until ctx ends.
func New(ctx context.Context, url, token string, log *zap.SugaredLogger) Notifier {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return Nop{}
	}
	c := &Client{url: url, token: token, http: httpx.RetryingClient(10 * time.Second), queue: make(chan Event, queueSize), log: log, attempts: 5, backoff: 2 * time.Second}
	c.wg.Add(1)
	go c.run(ctx)
	return c
}

func (c *Client) Enabled() bool { return true }

func (c *Client) Notify(ev Event) {
	if ev.Source == "" {
		ev.Source = Source
	}
	if ev.Data == nil {
		ev.Data = map[string]any{}
	}
	if ev.Recipients == nil {
		ev.Recipients = []Recipient{}
	}
	select {
	case c.queue <- ev:
	default:
		c.log.Warnw("notification dropped: queue full", "type", ev.Type, "key", ev.Key)
	}
}

// Wait blocks until the sender has stopped (after its context ended), for tests.
func (c *Client) Wait() { c.wg.Wait() }

func (c *Client) run(ctx context.Context) {
	defer c.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.queue:
			c.send(ctx, ev)
		}
	}
}

// send posts one event, trying again after an error or an answer outside 2xx (but not after one the service refuses as wrong, 4xx except 408/429), with a growing pause.
func (c *Client) send(ctx context.Context, ev Event) {
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		status, err := httpx.Do(ctx, c.http, httpx.Request{
			Method:  http.MethodPost,
			URL:     c.url + "/v1/events",
			Header:  http.Header{"Authorization": {"Bearer " + c.token}},
			Body:    ev,
			EmptyOK: true,
		}, nil)
		if err == nil {
			c.log.Debugw("notification sent", "type", ev.Type, "key", ev.Key, "status", status)
			return
		}
		final := attempt >= c.attempts || (status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests)
		if final {
			c.log.Warnw("notification not delivered", "type", ev.Type, "key", ev.Key, "status", status, "attempts", attempt, "error", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait *= 2
	}
}
