package grpcserver

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

type routeReservationReleaseResult int

const (
	routeReservationNotOwned routeReservationReleaseResult = iota
	routeReservationStillInUse
	routeReservationRemoved
)

type terminalSessionRoute struct {
	NodeID             string
	LastUsedUnixMs     int64
	LeaseExpiresUnixMs int64
	RecoveryState      terminalSessionRecoveryState
	// ReservationID is non-zero only while the first dispatch is provisional.
	// A successful result confirms the route by clearing it.
	ReservationID   uint64
	ProvisionalUses uint64
}

type terminalSessionRecoveryState uint8

const (
	terminalSessionRecoveryReady terminalSessionRecoveryState = iota
	terminalSessionRecoveryUnavailable
	terminalSessionRecoveryReconciling
)

func (s *RegistryService) bindTerminalSessionRoute(sessionID string, nodeID string, now time.Time) {
	if s == nil {
		return
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedSessionID == "" || normalizedNodeID == "" {
		return
	}

	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	existing, exists := s.terminalSessionToNode[normalizedSessionID]
	if exists && existing.NodeID != normalizedNodeID {
		previousIndex := s.terminalNodeToSessionIDIndex[existing.NodeID]
		if previousIndex != nil {
			delete(previousIndex, normalizedSessionID)
			if len(previousIndex) == 0 {
				delete(s.terminalNodeToSessionIDIndex, existing.NodeID)
			}
		}
	}

	s.terminalSessionToNode[normalizedSessionID] = terminalSessionRoute{
		NodeID:          normalizedNodeID,
		LastUsedUnixMs:  nowUnixMs,
		RecoveryState:   terminalSessionRecoveryReady,
		ReservationID:   0,
		ProvisionalUses: 0,
	}
	index := s.terminalNodeToSessionIDIndex[normalizedNodeID]
	if index == nil {
		index = make(map[string]struct{})
		s.terminalNodeToSessionIDIndex[normalizedNodeID] = index
	}
	index[normalizedSessionID] = struct{}{}
}

func (s *RegistryService) reserveTerminalSessionRoute(sessionID string, preferredNodeID string, now time.Time) (string, uint64) {
	if s == nil {
		return "", 0
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	normalizedNodeID := strings.TrimSpace(preferredNodeID)
	if normalizedSessionID == "" || normalizedNodeID == "" {
		return "", 0
	}

	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	existing, exists := s.terminalSessionToNode[normalizedSessionID]
	if exists {
		existing.LastUsedUnixMs = nowUnixMs
		reservationID := uint64(0)
		if existing.ReservationID != 0 {
			existing.ProvisionalUses++
			reservationID = existing.ReservationID
		}
		s.terminalSessionToNode[normalizedSessionID] = existing
		return existing.NodeID, reservationID
	}

	s.terminalRouteReservationSeq++
	if s.terminalRouteReservationSeq == 0 {
		s.terminalRouteReservationSeq++
	}
	reservationID := s.terminalRouteReservationSeq
	s.terminalSessionToNode[normalizedSessionID] = terminalSessionRoute{
		NodeID:          normalizedNodeID,
		LastUsedUnixMs:  nowUnixMs,
		RecoveryState:   terminalSessionRecoveryReady,
		ReservationID:   reservationID,
		ProvisionalUses: 1,
	}
	index := s.terminalNodeToSessionIDIndex[normalizedNodeID]
	if index == nil {
		index = make(map[string]struct{})
		s.terminalNodeToSessionIDIndex[normalizedNodeID] = index
	}
	index[normalizedSessionID] = struct{}{}
	return normalizedNodeID, reservationID
}

// claimTerminalSessionRoute refreshes a route and joins its provisional
// reservation when the first dispatch has not completed yet.
func (s *RegistryService) claimTerminalSessionRoute(sessionID string, now time.Time) (string, uint64, bool) {
	if s == nil {
		return "", 0, false
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedSessionID == "" {
		return "", 0, false
	}

	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok || strings.TrimSpace(route.NodeID) == "" {
		return "", 0, false
	}
	route.LastUsedUnixMs = nowUnixMs
	reservationID := uint64(0)
	if route.ReservationID != 0 {
		route.ProvisionalUses++
		reservationID = route.ReservationID
	}
	s.terminalSessionToNode[normalizedSessionID] = route
	return route.NodeID, reservationID, true
}

// confirmTerminalSessionRoute confirms a provisional route only when the
// dispatch still owns the current reservation. A zero reservation ID may only
// refresh an already-confirmed route on the same node.
func (s *RegistryService) confirmTerminalSessionRoute(
	sessionID string,
	expectedNodeID string,
	reservationID uint64,
	now time.Time,
) bool {
	if s == nil {
		return false
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	normalizedNodeID := strings.TrimSpace(expectedNodeID)
	if normalizedSessionID == "" || normalizedNodeID == "" {
		return false
	}

	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok || route.NodeID != normalizedNodeID || route.ReservationID != reservationID {
		return false
	}
	route.LastUsedUnixMs = nowUnixMs
	if reservationID != 0 {
		route.ReservationID = 0
		route.ProvisionalUses = 0
	}
	s.terminalSessionToNode[normalizedSessionID] = route
	return true
}

// touchTerminalSessionRoute returns the mapped node and refreshes LastUsedUnixMs.
func (s *RegistryService) touchTerminalSessionRoute(sessionID string, now time.Time) (string, bool) {
	if s == nil {
		return "", false
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedSessionID == "" {
		return "", false
	}

	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok || strings.TrimSpace(route.NodeID) == "" {
		return "", false
	}
	route.LastUsedUnixMs = nowUnixMs
	s.terminalSessionToNode[normalizedSessionID] = route
	return route.NodeID, true
}

func (s *RegistryService) updateTerminalSessionRouteLease(sessionID string, expectedNodeID string, leaseExpiresUnixMs int64, now time.Time) bool {
	if s == nil || leaseExpiresUnixMs <= 0 {
		return false
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	normalizedNodeID := strings.TrimSpace(expectedNodeID)
	if normalizedSessionID == "" || normalizedNodeID == "" {
		return false
	}

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok || route.NodeID != normalizedNodeID || route.ReservationID != 0 {
		return false
	}
	route.LeaseExpiresUnixMs = leaseExpiresUnixMs
	route.LastUsedUnixMs = routeNowUnixMs(now)
	route.RecoveryState = terminalSessionRecoveryReady
	s.terminalSessionToNode[normalizedSessionID] = route
	return true
}

func (s *RegistryService) terminalSessionRouteSnapshot(sessionID string, now time.Time) (terminalSessionRoute, bool) {
	if s == nil {
		return terminalSessionRoute{}, false
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedSessionID == "" {
		return terminalSessionRoute{}, false
	}
	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok {
		return terminalSessionRoute{}, false
	}
	if route.LeaseExpiresUnixMs > 0 && route.LeaseExpiresUnixMs <= nowUnixMs {
		s.deleteTerminalSessionRouteLocked(normalizedSessionID, route)
		return terminalSessionRoute{}, false
	}
	return route, true
}

func (s *RegistryService) beginTerminalSessionRecovery(nodeID string, now time.Time) []*registryv1.TerminalSessionRecoveryCandidate {
	if s == nil {
		return nil
	}
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return nil
	}
	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	index := s.terminalNodeToSessionIDIndex[normalizedNodeID]
	candidates := make([]*registryv1.TerminalSessionRecoveryCandidate, 0, len(index))
	for sessionID := range index {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != normalizedNodeID {
			continue
		}
		leaseExpiresUnixMs := route.LeaseExpiresUnixMs
		if leaseExpiresUnixMs <= 0 && s.terminalRouteTTL > 0 {
			leaseExpiresUnixMs = route.LastUsedUnixMs + s.terminalRouteTTL.Milliseconds()
		}
		if route.ReservationID != 0 || leaseExpiresUnixMs <= nowUnixMs {
			s.deleteTerminalSessionRouteLocked(sessionID, route)
			continue
		}
		route.LeaseExpiresUnixMs = leaseExpiresUnixMs
		route.RecoveryState = terminalSessionRecoveryReconciling
		s.terminalSessionToNode[sessionID] = route
		candidates = append(candidates, &registryv1.TerminalSessionRecoveryCandidate{
			SessionId:          sessionID,
			LeaseExpiresUnixMs: leaseExpiresUnixMs,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].GetSessionId() < candidates[j].GetSessionId() })
	return candidates
}

func (s *RegistryService) markTerminalSessionRoutesUnavailable(nodeID string) int {
	if s == nil {
		return 0
	}
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return 0
	}
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	unavailable := 0
	for sessionID := range s.terminalNodeToSessionIDIndex[normalizedNodeID] {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != normalizedNodeID {
			continue
		}
		if route.ReservationID != 0 {
			s.deleteTerminalSessionRouteLocked(sessionID, route)
			continue
		}
		route.RecoveryState = terminalSessionRecoveryUnavailable
		s.terminalSessionToNode[sessionID] = route
		unavailable++
	}
	return unavailable
}

func (s *RegistryService) applyTerminalSessionRecoveryReport(session *activeSession, report *registryv1.TerminalSessionRecoveryReport, now time.Time) error {
	if s == nil || session == nil || report == nil {
		return nil
	}
	candidates := session.recoveryCandidateSnapshot()
	results := make(map[string]registryv1.TerminalSessionRecoveryResult_Status, len(report.GetResults()))
	for _, result := range report.GetResults() {
		if result == nil {
			continue
		}
		sessionID := strings.TrimSpace(result.GetSessionId())
		if _, ok := candidates[sessionID]; !ok {
			return &terminalRecoveryValidationError{message: "recovery result is not a candidate"}
		}
		if _, duplicate := results[sessionID]; duplicate {
			return &terminalRecoveryValidationError{message: "duplicate recovery result"}
		}
		status := result.GetStatus()
		if status != registryv1.TerminalSessionRecoveryResult_RECOVERED && status != registryv1.TerminalSessionRecoveryResult_MISSING && status != registryv1.TerminalSessionRecoveryResult_INVALID {
			return &terminalRecoveryValidationError{message: "invalid recovery status"}
		}
		results[sessionID] = status
	}
	if len(results) != len(candidates) {
		return &terminalRecoveryValidationError{message: "recovery report must cover every candidate"}
	}

	nowUnixMs := routeNowUnixMs(now)
	recovered := 0
	failures := 0
	s.terminalRoutesMu.Lock()
	for sessionID := range candidates {
		if route, ok := s.terminalSessionToNode[sessionID]; ok && route.NodeID != session.nodeID {
			s.terminalRoutesMu.Unlock()
			return &terminalRecoveryValidationError{message: "recovery candidate no longer belongs to worker"}
		}
	}
	for sessionID, leaseExpiresUnixMs := range candidates {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != session.nodeID {
			continue
		}
		status := results[sessionID]
		if status != registryv1.TerminalSessionRecoveryResult_RECOVERED || (leaseExpiresUnixMs > 0 && leaseExpiresUnixMs <= nowUnixMs) {
			s.deleteTerminalSessionRouteLocked(sessionID, route)
			failures++
			continue
		}
		route.RecoveryState = terminalSessionRecoveryReady
		route.LastUsedUnixMs = nowUnixMs
		s.terminalSessionToNode[sessionID] = route
		recovered++
	}
	unavailable := s.countTerminalSessionRoutesByStateLocked(terminalSessionRecoveryUnavailable)
	s.terminalRoutesMu.Unlock()
	session.setRecoveryResults(results)
	slog.Info(
		"terminal session recovery metrics",
		"executor_kind", session.executorKind,
		"recovery_duration_ms", now.Sub(session.connectedAt).Milliseconds(),
		"recovered_session_count", recovered,
		"recovery_failures", failures,
		"unavailable_route_count", unavailable,
	)
	return nil
}

func (s *RegistryService) countTerminalSessionRoutesByStateLocked(state terminalSessionRecoveryState) int {
	count := 0
	for _, route := range s.terminalSessionToNode {
		if route.RecoveryState == state && route.ReservationID == 0 {
			count++
		}
	}
	return count
}

type terminalRecoveryValidationError struct{ message string }

func (e *terminalRecoveryValidationError) Error() string { return e.message }

// clearTerminalSessionRouteReservation releases one provisional dispatch. The
// route is removed only after every dispatch sharing the reservation failed.
func (s *RegistryService) clearTerminalSessionRouteReservation(sessionID string, expectedNodeID string, reservationID uint64) routeReservationReleaseResult {
	if s == nil || reservationID == 0 {
		return routeReservationNotOwned
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	normalizedNodeID := strings.TrimSpace(expectedNodeID)
	if normalizedSessionID == "" || normalizedNodeID == "" {
		return routeReservationNotOwned
	}

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok || route.NodeID != normalizedNodeID || route.ReservationID != reservationID {
		return routeReservationNotOwned
	}
	if route.ProvisionalUses > 1 {
		route.ProvisionalUses--
		s.terminalSessionToNode[normalizedSessionID] = route
		return routeReservationStillInUse
	}
	s.deleteTerminalSessionRouteLocked(normalizedSessionID, route)
	return routeReservationRemoved
}

func (s *RegistryService) clearTerminalSessionRoute(sessionID string, expectedNodeID string) {
	if s == nil {
		return
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedSessionID == "" {
		return
	}
	normalizedExpectedNodeID := strings.TrimSpace(expectedNodeID)

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok {
		return
	}
	if normalizedExpectedNodeID != "" && route.NodeID != normalizedExpectedNodeID {
		return
	}

	s.deleteTerminalSessionRouteLocked(normalizedSessionID, route)
}

func (s *RegistryService) deleteTerminalSessionRouteLocked(sessionID string, route terminalSessionRoute) {
	delete(s.terminalSessionToNode, sessionID)
	index := s.terminalNodeToSessionIDIndex[route.NodeID]
	if index == nil {
		return
	}
	delete(index, sessionID)
	if len(index) == 0 {
		delete(s.terminalNodeToSessionIDIndex, route.NodeID)
	}
}

func (s *RegistryService) pruneExpiredTerminalSessionRoutes(now time.Time) int {
	if s == nil {
		return 0
	}
	ttl := s.terminalRouteTTL
	nowUnixMs := routeNowUnixMs(now)
	expireBefore := nowUnixMs - ttl.Milliseconds()

	removed := 0
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	for sessionID, route := range s.terminalSessionToNode {
		if route.LeaseExpiresUnixMs > 0 {
			if route.LeaseExpiresUnixMs > nowUnixMs {
				continue
			}
		} else if ttl <= 0 || route.LastUsedUnixMs > expireBefore {
			continue
		}
		delete(s.terminalSessionToNode, sessionID)
		index := s.terminalNodeToSessionIDIndex[route.NodeID]
		if index != nil {
			delete(index, sessionID)
			if len(index) == 0 {
				delete(s.terminalNodeToSessionIDIndex, route.NodeID)
			}
		}
		removed++
	}
	return removed
}

func (s *RegistryService) maybePruneTerminalSessionRoutes(now time.Time) {
	if s == nil {
		return
	}
	nowUnixMs := routeNowUnixMs(now)
	minIntervalMs := terminalRoutePruneMinInterval.Milliseconds()

	for {
		last := s.lastTerminalRoutePruneUnixMs.Load()
		if last > 0 && nowUnixMs-last < minIntervalMs {
			return
		}
		if s.lastTerminalRoutePruneUnixMs.CompareAndSwap(last, nowUnixMs) {
			break
		}
	}
	s.pruneExpiredTerminalSessionRoutes(now)
}

func routeNowUnixMs(now time.Time) int64 {
	if now.IsZero() {
		return time.Now().UnixMilli()
	}
	return now.UnixMilli()
}
