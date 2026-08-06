package grpcserver

import (
	"strings"
	"time"
)

type routeReservationReleaseResult int

const (
	routeReservationNotOwned routeReservationReleaseResult = iota
	routeReservationStillInUse
	routeReservationRemoved
)

type terminalSessionRoute struct {
	NodeID         string
	LastUsedUnixMs int64
	// ReservationID is non-zero only while the first dispatch is provisional.
	// A successful result confirms the route by clearing it.
	ReservationID   uint64
	ProvisionalUses uint64
}

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

func (s *RegistryService) clearTerminalSessionRoutesByNode(nodeID string) {
	if s == nil {
		return
	}
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return
	}

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	index := s.terminalNodeToSessionIDIndex[normalizedNodeID]
	if index == nil {
		return
	}
	for sessionID := range index {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != normalizedNodeID {
			continue
		}
		delete(s.terminalSessionToNode, sessionID)
	}
	delete(s.terminalNodeToSessionIDIndex, normalizedNodeID)
}

func (s *RegistryService) pruneExpiredTerminalSessionRoutes(now time.Time) int {
	if s == nil {
		return 0
	}
	ttl := s.terminalRouteTTL
	if ttl <= 0 {
		return 0
	}
	nowUnixMs := routeNowUnixMs(now)
	expireBefore := nowUnixMs - ttl.Milliseconds()

	removed := 0
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	for sessionID, route := range s.terminalSessionToNode {
		if route.LastUsedUnixMs > expireBefore {
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
