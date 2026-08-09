package server

import (
	"context"
	"sync"
	"time"
)

// usageReporter periodically tells the control plane what has been used.
type usageReporter struct {
	srv  *Server
	hook *hookClient

	// closed holds tunnels that ended between two reports. Without it a
	// tunnel that opens and closes inside one interval would never be billed.
	mu     sync.Mutex
	closed []HookUsageTunnel
}

// recordClosed remembers a tunnel's final totals.
func (u *usageReporter) recordClosed(t *Tunnel) {
	if u == nil {
		return
	}
	rec := u.snapshot(t)
	rec.Closed = true
	rec.ClosedAt = time.Now().UTC()

	u.mu.Lock()
	defer u.mu.Unlock()
	// Bound the buffer: if the control plane has been down for a long time,
	// dropping the oldest records is better than growing without limit.
	const maxBuffered = 10000
	if len(u.closed) >= maxBuffered {
		u.srv.log.Warn("usage buffer full, dropping oldest records", "buffered", len(u.closed))
		u.closed = u.closed[len(u.closed)/2:]
	}
	u.closed = append(u.closed, rec)
}

// snapshot reads a tunnel's counters.
func (u *usageReporter) snapshot(t *Tunnel) HookUsageTunnel {
	s := t.Session()
	rec := HookUsageTunnel{
		TunnelID: t.ID,
		Proto:    t.Proto,
		Public:   t.Public(u.srv.cfg),
		OpenedAt: t.Created.UTC(),
		Conns:    t.Conns.Load(),
		BytesIn:  t.Traffic.In.Load(),
		BytesOut: t.Traffic.Out.Load(),
	}
	if s != nil {
		rec.TokenID, rec.AgentAddr, rec.Hostname = s.TokenID, s.RemoteAddr, s.Hostname
	}
	return rec
}

// run reports until ctx is cancelled, then sends one final report so the last
// interval is not lost on a clean shutdown.
func (u *usageReporter) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A fresh context: the parent is already cancelled, and the final
			// report is the one most worth delivering.
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			u.report(final)
			cancel()
			return
		case <-ticker.C:
			u.report(ctx)
		}
	}
}

// report sends one snapshot of every live tunnel plus any that have closed.
func (u *usageReporter) report(ctx context.Context) {
	u.mu.Lock()
	pending := u.closed
	u.closed = nil
	u.mu.Unlock()

	live := u.srv.reg.List()
	records := make([]HookUsageTunnel, 0, len(live)+len(pending))
	records = append(records, pending...)
	for _, t := range live {
		records = append(records, u.snapshot(t))
	}
	if len(records) == 0 {
		return
	}

	var resp HookUsageResponse
	err := u.hook.post(ctx, u.hook.usageURL, HookUsageRequest{
		ServerVersion: u.srv.version,
		BaseDomain:    u.srv.cfg.BaseDomain,
		ReportedAt:    time.Now().UTC(),
		Tunnels:       records,
	}, &resp)
	if err != nil {
		// Put the closed records back so they go out with the next report;
		// live tunnels re-report their totals anyway.
		u.mu.Lock()
		u.closed = append(pending, u.closed...)
		u.mu.Unlock()
		u.srv.log.Warn("usage hook failed", "url", u.hook.usageURL, "err", err, "requeued", len(pending))
		return
	}

	u.srv.log.Debug("usage reported", "tunnels", len(records), "disconnect", len(resp.Disconnect))
	for _, id := range resp.Disconnect {
		u.hook.forget(id)
		u.srv.disconnectToken(id, "control plane asked to disconnect")
	}
}
