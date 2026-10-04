package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"home-harness/internal/domain"
)

// Availability: whether a device takes new work right now, from the
// operator's rule for it (NodeMeta.Availability) and what it last
// reported about its use (RuntimeState.Use) — so a phone works only on
// its charger and a PC only when nobody is using it, the way BOINC and
// HTCondor borrow machines.
//
// It holds back only new workloads (placement, the queue, restarts, job
// attempts, chats): work already running finishes, and commands,
// updates and cancels are never held. An unavailable device is "not
// now", never "can't": its work waits in the queue. A device whose agent
// predates FeatureAvailability is always available, as before; one with
// it reports its use in REGISTER and every heartbeat, and takes nothing
// while no report is at hand, so nothing lands on an unplugged phone in
// the moment after it reconnects. Unknown signals (no battery, input that
// can't be seen) don't hold a device back.

// availableNow is the one availability check everything uses: placement,
// the chat API's fast failure, and the dashboard. When the device is
// available the second value may still carry a note for the operator.
func (s *Server) availableNow(rec *NodeRecord, now time.Time) (bool, string) {
	rule := domain.Availability{}
	if a := s.meta.get(rec.Node.Identity.NodeID).Availability; a != nil {
		rule = *a
	}
	use := rec.LastMetrics.Use
	return evaluateAvailability(rule, rec.hasAgentFeature(domain.FeatureAvailability), use == nil, use, now)
}

func evaluateAvailability(rule domain.Availability, reports, noReportYet bool, use *domain.DeviceUse, now time.Time) (bool, string) {
	if rule.Mode == domain.AvailablePaused {
		return false, "paused"
	}
	if rule.Hours != "" {
		if in, err := withinHours(rule.Hours, now); err == nil && !in {
			return false, "outside its hours (" + rule.Hours + ")"
		}
	}
	if !reports {
		return true, ""
	}
	if noReportYet {
		return false, "hasn't reported how it is used yet"
	}
	if use == nil {
		use = &domain.DeviceUse{}
	}
	switch rule.Mode {
	case domain.AvailableAlways:
		return true, ""
	case domain.AvailableWhenIdle:
		minutes := rule.IdleMinutes
		if minutes <= 0 {
			minutes = domain.DefaultIdleMinutes
		}
		if use.IdleSeconds == nil {
			return true, "can't see when it is in use, so treated as idle"
		}
		if idle := time.Duration(*use.IdleSeconds) * time.Second; idle < time.Duration(minutes)*time.Minute {
			return false, fmt.Sprintf("in use (idle %s, needs %d min)", shortDuration(idle), minutes)
		}
		return true, ""
	default: // auto, charging
		if use.OnBattery != nil && *use.OnBattery {
			if use.BatteryPercent != nil {
				return false, fmt.Sprintf("on battery (%d%%)", *use.BatteryPercent)
			}
			return false, "on battery"
		}
		return true, ""
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// parseClock reads "HH:MM" as minutes after midnight.
func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, fmt.Errorf("%q is not a time like 22:00", s)
	}
	return hh*60 + mm, nil
}

// withinHours reports whether now falls in window "HH:MM-HH:MM" (local
// time; a window may wrap past midnight, e.g. 22:00-07:00).
func withinHours(window string, now time.Time) (bool, error) {
	a, b, ok := strings.Cut(window, "-")
	if !ok {
		return false, fmt.Errorf("hours must look like 22:00-07:00")
	}
	start, err := parseClock(a)
	if err != nil {
		return false, err
	}
	end, err := parseClock(b)
	if err != nil {
		return false, err
	}
	if start == end {
		return false, fmt.Errorf("hours %q start and end at the same time", window)
	}
	t := now.Hour()*60 + now.Minute()
	if start < end {
		return t >= start && t < end, nil
	}
	return t >= start || t < end, nil
}

func validateAvailability(a domain.Availability) error {
	switch a.Mode {
	case "", domain.AvailableAuto, domain.AvailableAlways, domain.AvailableWhenCharging, domain.AvailablePaused:
		if a.IdleMinutes != 0 {
			return errors.New("idleMinutes goes with mode idle")
		}
	case domain.AvailableWhenIdle:
		if a.IdleMinutes < 0 || a.IdleMinutes > 24*60 {
			return errors.New("idleMinutes must be 1-1440 (0 = the default, 5)")
		}
	default:
		return fmt.Errorf("mode %q: want auto, always, idle, charging or paused", a.Mode)
	}
	if a.Hours != "" {
		if _, err := withinHours(a.Hours, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// SetNodeAvailability sets a known node's availability rule, keeping its
// alias and labels; the default rule clears it.
func (s *Server) SetNodeAvailability(id domain.NodeID, a domain.Availability) (old domain.NodeMeta, err error) {
	if a.Mode == "" {
		a.Mode = domain.AvailableAuto
	}
	if err := validateAvailability(a); err != nil {
		return domain.NodeMeta{}, err
	}
	if _, ok := s.Registry.Get(id); !ok {
		return domain.NodeMeta{}, ErrUnknownNode
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	old = s.meta.get(id)
	meta := old
	meta.Availability = &a
	if a.IsDefault() {
		meta.Availability = nil
	}
	if s.store != nil {
		if err := s.store.PutNodeMeta(id, meta); err != nil {
			return domain.NodeMeta{}, fmt.Errorf("manager: persist node availability: %w", err)
		}
	}
	s.meta.set(id, meta)
	return old, nil
}

// apiPutNodeAvailability is PUT /nodes/{id}/availability
// {"mode":"idle","idleMinutes":10,"hours":"22:00-07:00"}.
func (s *Server) apiPutNodeAvailability(w http.ResponseWriter, r *http.Request) {
	id := domain.NodeID(r.PathValue("id"))
	var a domain.Availability
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&a); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	old, err := s.SetNodeAvailability(id, a)
	switch {
	case errors.Is(err, ErrUnknownNode):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(domain.AuditSecurity, "node.availability-changed", id, actorFrom(r.Context()), map[string]any{"from": old.Availability, "to": s.meta.get(id).Availability})
	s.publish(domain.EventNodeUpdated, id, map[string]any{"reason": "availability rule changed"})
	s.kickDispatch() // the rule may let queued work start now
	writeJSON(w, http.StatusOK, s.availabilityView(id))
}

// availabilityView is a node's rule and whether it takes work now.
type availabilityView struct {
	Rule      domain.Availability `json:"rule"`
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
}

func (s *Server) availabilityView(id domain.NodeID) availabilityView {
	v := availabilityView{Rule: domain.Availability{Mode: domain.AvailableAuto}, Available: true}
	if a := s.meta.get(id).Availability; a != nil {
		v.Rule = *a
	}
	if rec, ok := s.Registry.Get(id); ok {
		v.Available, v.Reason = s.availableNow(rec, time.Now())
	}
	return v
}

// noteAvailability re-checks a node after a heartbeat: when it changes,
// the dashboard hears of it and, if it became available, queued work is
// dispatched now rather than at the next tick.
func (s *Server) noteAvailability(id domain.NodeID, before bool) {
	rec, ok := s.Registry.Get(id)
	if !ok {
		return
	}
	now, reason := s.availableNow(rec, time.Now())
	if now == before {
		return
	}
	s.publish(domain.EventNodeUpdated, id, map[string]any{"reason": "availability", "available": now, "why": reason})
	if now {
		s.kickDispatch()
	}
}
