package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

type terminalSessionRouteStore interface {
	LoadActiveTerminalSessionRoutes(context.Context, int64) ([]registry.TerminalSessionRoute, error)
	UpsertConfirmedTerminalSessionRoute(context.Context, registry.TerminalSessionRoute) error
	DeleteTerminalSessionRoute(context.Context, string, string) (bool, error)
	DeleteTerminalSessionRoutes(context.Context, []registry.TerminalSessionRouteRef) error
	DeleteTerminalSessionRoutesByNode(context.Context, string) (int64, error)
	DeleteExpiredTerminalSessionRoutes(context.Context, int64) (int64, error)
}

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
	ReservationID          uint64
	ConfirmedReservationID uint64
	ProvisionalUses        uint64
}

type terminalSessionRecoveryState uint8

const (
	terminalSessionRecoveryReady terminalSessionRecoveryState = iota
	terminalSessionRecoveryUnavailable
	terminalSessionRecoveryReconciling
)

func (s *RegistryService) RestoreTerminalSessionRoutes(ctx context.Context, now time.Time) error {
	if s == nil || s.terminalRouteStore == nil {
		return errors.New("terminal session route store is required")
	}
	nowUnixMs := routeNowUnixMs(now)
	if _, err := s.terminalRouteStore.DeleteExpiredTerminalSessionRoutes(ctx, nowUnixMs); err != nil {
		return fmt.Errorf("delete expired terminal session routes: %w", err)
	}
	persisted, err := s.terminalRouteStore.LoadActiveTerminalSessionRoutes(ctx, nowUnixMs)
	if err != nil {
		return fmt.Errorf("load terminal session routes: %w", err)
	}
	routes := make(map[string]terminalSessionRoute, len(persisted))
	index := make(map[string]map[string]struct{})
	for _, route := range persisted {
		sessionID := strings.TrimSpace(route.ScopedSessionID)
		nodeID := strings.TrimSpace(route.NodeID)
		if !isValidPersistedTerminalSessionID(sessionID) || nodeID == "" || route.LeaseExpiresUnixMs <= nowUnixMs {
			return errors.New("persisted terminal session route is invalid")
		}
		if _, duplicate := routes[sessionID]; duplicate {
			return errors.New("persisted terminal session route is duplicated")
		}
		routes[sessionID] = terminalSessionRoute{
			NodeID:             nodeID,
			LastUsedUnixMs:     route.LastUsedUnixMs,
			LeaseExpiresUnixMs: route.LeaseExpiresUnixMs,
			RecoveryState:      terminalSessionRecoveryUnavailable,
		}
		if index[nodeID] == nil {
			index[nodeID] = make(map[string]struct{})
		}
		index[nodeID][sessionID] = struct{}{}
	}

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	if len(s.terminalSessionToNode) != 0 || len(s.terminalNodeToSessionIDIndex) != 0 {
		return errors.New("terminal session routes are already initialized")
	}
	s.terminalSessionToNode = routes
	s.terminalNodeToSessionIDIndex = index
	return nil
}

func isValidPersistedTerminalSessionID(sessionID string) bool {
	parts := strings.SplitN(strings.TrimSpace(sessionID), taskOwnerScopeSeparator, 3)
	return len(parts) == 3 && parts[0] == taskOwnerScopePrefix && strings.TrimSpace(parts[1]) != "" && strings.TrimSpace(parts[2]) != ""
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
	if !ok || route.NodeID != normalizedNodeID || (route.ReservationID != reservationID && route.ConfirmedReservationID != reservationID) {
		return false
	}
	route.LastUsedUnixMs = nowUnixMs
	if reservationID != 0 {
		route.ReservationID = 0
		route.ConfirmedReservationID = reservationID
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
	if route.LeaseExpiresUnixMs < leaseExpiresUnixMs {
		route.LeaseExpiresUnixMs = leaseExpiresUnixMs
	}
	route.LastUsedUnixMs = routeNowUnixMs(now)
	route.RecoveryState = terminalSessionRecoveryReady
	s.terminalSessionToNode[normalizedSessionID] = route
	return true
}

func (s *RegistryService) commitConfirmedTerminalSessionRoute(
	sessionID string,
	expectedNodeID string,
	reservationID uint64,
	leaseExpiresUnixMs int64,
	now time.Time,
) (bool, error) {
	if s == nil || leaseExpiresUnixMs <= 0 {
		return false, errors.New("terminal session result is missing a valid lease")
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	normalizedNodeID := strings.TrimSpace(expectedNodeID)
	if normalizedSessionID == "" || normalizedNodeID == "" {
		return false, errors.New("terminal session route identity is required")
	}
	nowUnixMs := routeNowUnixMs(now)

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok || route.NodeID != normalizedNodeID || (route.ReservationID != reservationID && route.ConfirmedReservationID != reservationID) {
		return false, nil
	}
	if route.LeaseExpiresUnixMs < leaseExpiresUnixMs {
		route.LeaseExpiresUnixMs = leaseExpiresUnixMs
	}
	if s.terminalRouteStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
		err := s.terminalRouteStore.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
			ScopedSessionID:    normalizedSessionID,
			NodeID:             normalizedNodeID,
			LeaseExpiresUnixMs: route.LeaseExpiresUnixMs,
			LastUsedUnixMs:     nowUnixMs,
			CreatedAtUnixMs:    nowUnixMs,
			UpdatedAtUnixMs:    nowUnixMs,
		})
		cancel()
		if err != nil {
			return false, fmt.Errorf("persist terminal session route: %w", err)
		}
	}
	route.LastUsedUnixMs = nowUnixMs
	route.RecoveryState = terminalSessionRecoveryReady
	route.ReservationID = 0
	if reservationID != 0 {
		route.ConfirmedReservationID = reservationID
	}
	route.ProvisionalUses = 0
	s.terminalSessionToNode[normalizedSessionID] = route
	return true, nil
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
		if s.terminalRouteStore != nil {
			ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
			_, err := s.terminalRouteStore.DeleteTerminalSessionRoute(ctx, normalizedSessionID, route.NodeID)
			cancel()
			if err != nil {
				slog.Error("failed to delete expired terminal session route", "node_id", route.NodeID, "error", err)
				return terminalSessionRoute{}, false
			}
		}
		s.deleteTerminalSessionRouteLocked(normalizedSessionID, route)
		return terminalSessionRoute{}, false
	}
	return route, true
}

func (s *RegistryService) beginTerminalSessionRecovery(nodeID string, now time.Time) []*registryv1.TerminalSessionRecoveryCandidate {
	candidates, _ := s.beginTerminalSessionRecoveryWithError(nodeID, now)
	return candidates
}

func (s *RegistryService) beginTerminalSessionRecoveryWithError(nodeID string, now time.Time) ([]*registryv1.TerminalSessionRecoveryCandidate, error) {
	if s == nil {
		return nil, nil
	}
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return nil, nil
	}
	nowUnixMs := routeNowUnixMs(now)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	index := s.terminalNodeToSessionIDIndex[normalizedNodeID]
	candidates := make([]*registryv1.TerminalSessionRecoveryCandidate, 0, len(index))
	expired := make([]registry.TerminalSessionRouteRef, 0)
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
			if route.LeaseExpiresUnixMs > 0 {
				expired = append(expired, registry.TerminalSessionRouteRef{ScopedSessionID: sessionID, NodeID: route.NodeID})
			}
			continue
		}
		candidates = append(candidates, &registryv1.TerminalSessionRecoveryCandidate{
			SessionId:          sessionID,
			LeaseExpiresUnixMs: leaseExpiresUnixMs,
		})
	}
	if len(expired) > 0 && s.terminalRouteStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
		err := s.terminalRouteStore.DeleteTerminalSessionRoutes(ctx, expired)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("delete expired recovery candidates: %w", err)
		}
	}
	for _, ref := range expired {
		if route, ok := s.terminalSessionToNode[ref.ScopedSessionID]; ok && route.NodeID == ref.NodeID {
			s.deleteTerminalSessionRouteLocked(ref.ScopedSessionID, route)
		}
	}
	for _, candidate := range candidates {
		route, ok := s.terminalSessionToNode[candidate.GetSessionId()]
		if !ok || route.NodeID != normalizedNodeID {
			continue
		}
		route.LeaseExpiresUnixMs = candidate.GetLeaseExpiresUnixMs()
		route.RecoveryState = terminalSessionRecoveryReconciling
		s.terminalSessionToNode[candidate.GetSessionId()] = route
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].GetSessionId() < candidates[j].GetSessionId() })
	return candidates, nil
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
	deleteRefs := make([]registry.TerminalSessionRouteRef, 0)
	for sessionID, leaseExpiresUnixMs := range candidates {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != session.nodeID {
			continue
		}
		if results[sessionID] != registryv1.TerminalSessionRecoveryResult_RECOVERED || (leaseExpiresUnixMs > 0 && leaseExpiresUnixMs <= nowUnixMs) {
			if route.LeaseExpiresUnixMs > 0 {
				deleteRefs = append(deleteRefs, registry.TerminalSessionRouteRef{ScopedSessionID: sessionID, NodeID: route.NodeID})
			}
		}
	}
	if len(deleteRefs) > 0 && s.terminalRouteStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
		err := s.terminalRouteStore.DeleteTerminalSessionRoutes(ctx, deleteRefs)
		cancel()
		if err != nil {
			s.terminalRoutesMu.Unlock()
			return fmt.Errorf("persist terminal session recovery report: %w", err)
		}
	}
	for sessionID, leaseExpiresUnixMs := range candidates {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != session.nodeID {
			continue
		}
		if results[sessionID] != registryv1.TerminalSessionRecoveryResult_RECOVERED || (leaseExpiresUnixMs > 0 && leaseExpiresUnixMs <= nowUnixMs) {
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

func (s *RegistryService) clearTerminalSessionRoute(sessionID string, expectedNodeID string) error {
	if s == nil {
		return nil
	}
	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedSessionID == "" {
		return nil
	}
	normalizedExpectedNodeID := strings.TrimSpace(expectedNodeID)

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()

	route, ok := s.terminalSessionToNode[normalizedSessionID]
	if !ok {
		return nil
	}
	if normalizedExpectedNodeID != "" && route.NodeID != normalizedExpectedNodeID {
		return nil
	}
	if route.LeaseExpiresUnixMs > 0 && s.terminalRouteStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
		_, err := s.terminalRouteStore.DeleteTerminalSessionRoute(ctx, normalizedSessionID, route.NodeID)
		cancel()
		if err != nil {
			return fmt.Errorf("delete terminal session route: %w", err)
		}
	}

	s.deleteTerminalSessionRouteLocked(normalizedSessionID, route)
	return nil
}

func (s *RegistryService) deleteTerminalSessionRoutesByNode(nodeID string) (int, error) {
	if s == nil {
		return 0, nil
	}
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return 0, nil
	}
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	if s.terminalRouteStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
		_, err := s.terminalRouteStore.DeleteTerminalSessionRoutesByNode(ctx, normalizedNodeID)
		cancel()
		if err != nil {
			return 0, fmt.Errorf("delete terminal session routes for worker: %w", err)
		}
	}
	removed := 0
	for sessionID := range s.terminalNodeToSessionIDIndex[normalizedNodeID] {
		route, ok := s.terminalSessionToNode[sessionID]
		if !ok || route.NodeID != normalizedNodeID {
			continue
		}
		s.deleteTerminalSessionRouteLocked(sessionID, route)
		removed++
	}
	return removed, nil
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

	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	persistedDeleteSucceeded := true
	if s.terminalRouteStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
		_, err := s.terminalRouteStore.DeleteExpiredTerminalSessionRoutes(ctx, nowUnixMs)
		cancel()
		if err != nil {
			persistedDeleteSucceeded = false
			slog.Error("failed to prune persisted terminal session routes", "error", err)
		}
	}

	removed := 0
	for sessionID, route := range s.terminalSessionToNode {
		if route.LeaseExpiresUnixMs > 0 {
			if route.LeaseExpiresUnixMs > nowUnixMs || !persistedDeleteSucceeded {
				continue
			}
		} else if ttl <= 0 || route.LastUsedUnixMs > expireBefore {
			continue
		}
		s.deleteTerminalSessionRouteLocked(sessionID, route)
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
