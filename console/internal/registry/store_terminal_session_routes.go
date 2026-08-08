package registry

import (
	"context"
	"errors"
	"strings"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

var ErrTerminalSessionRouteNodeConflict = errors.New("terminal session route belongs to another worker")

type TerminalSessionRoute struct {
	ScopedSessionID    string
	NodeID             string
	LeaseExpiresUnixMs int64
	LastUsedUnixMs     int64
	CreatedAtUnixMs    int64
	UpdatedAtUnixMs    int64
}

type TerminalSessionRouteRef struct {
	ScopedSessionID string
	NodeID          string
}

func (s *Store) LoadActiveTerminalSessionRoutes(ctx context.Context, nowUnixMs int64) ([]TerminalSessionRoute, error) {
	if s == nil || s.queries == nil {
		return nil, ErrPersistenceDBRequired
	}
	rows, err := s.queries.ListActiveTerminalSessionRoutes(ctx, nowUnixMs)
	if err != nil {
		return nil, err
	}
	routes := make([]TerminalSessionRoute, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, terminalSessionRouteFromSQL(row))
	}
	return routes, nil
}

func (s *Store) UpsertConfirmedTerminalSessionRoute(ctx context.Context, route TerminalSessionRoute) error {
	if s == nil || s.queries == nil {
		return ErrPersistenceDBRequired
	}
	route.ScopedSessionID = strings.TrimSpace(route.ScopedSessionID)
	route.NodeID = strings.TrimSpace(route.NodeID)
	if route.ScopedSessionID == "" || route.NodeID == "" || route.LeaseExpiresUnixMs <= 0 {
		return errors.New("invalid terminal session route")
	}
	rows, err := s.queries.UpsertTerminalSessionRoute(ctx, sqlc.UpsertTerminalSessionRouteParams{
		ScopedSessionID:    route.ScopedSessionID,
		NodeID:             route.NodeID,
		LeaseExpiresUnixMs: route.LeaseExpiresUnixMs,
		LastUsedUnixMs:     route.LastUsedUnixMs,
		CreatedAtUnixMs:    route.CreatedAtUnixMs,
		UpdatedAtUnixMs:    route.UpdatedAtUnixMs,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrTerminalSessionRouteNodeConflict
	}
	return nil
}

func (s *Store) DeleteTerminalSessionRoute(ctx context.Context, scopedSessionID string, expectedNodeID string) (bool, error) {
	if s == nil || s.queries == nil {
		return false, ErrPersistenceDBRequired
	}
	rows, err := s.queries.DeleteTerminalSessionRouteBySessionAndNode(ctx, sqlc.DeleteTerminalSessionRouteBySessionAndNodeParams{
		ScopedSessionID: strings.TrimSpace(scopedSessionID),
		NodeID:          strings.TrimSpace(expectedNodeID),
	})
	return rows > 0, err
}

func (s *Store) DeleteTerminalSessionRoutes(ctx context.Context, routes []TerminalSessionRouteRef) error {
	if s == nil || s.db == nil {
		return ErrPersistenceDBRequired
	}
	return s.db.WithTx(ctx, func(q *sqlc.Queries) error {
		for _, route := range routes {
			if _, err := q.DeleteTerminalSessionRouteBySessionAndNode(ctx, sqlc.DeleteTerminalSessionRouteBySessionAndNodeParams{
				ScopedSessionID: strings.TrimSpace(route.ScopedSessionID),
				NodeID:          strings.TrimSpace(route.NodeID),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteTerminalSessionRoutesByNode(ctx context.Context, nodeID string) (int64, error) {
	if s == nil || s.queries == nil {
		return 0, ErrPersistenceDBRequired
	}
	return s.queries.DeleteTerminalSessionRoutesByNode(ctx, strings.TrimSpace(nodeID))
}

func (s *Store) DeleteExpiredTerminalSessionRoutes(ctx context.Context, nowUnixMs int64) (int64, error) {
	if s == nil || s.queries == nil {
		return 0, ErrPersistenceDBRequired
	}
	return s.queries.DeleteExpiredTerminalSessionRoutes(ctx, nowUnixMs)
}

func terminalSessionRouteFromSQL(row sqlc.TerminalSessionRoute) TerminalSessionRoute {
	return TerminalSessionRoute{
		ScopedSessionID:    row.ScopedSessionID,
		NodeID:             row.NodeID,
		LeaseExpiresUnixMs: row.LeaseExpiresUnixMs,
		LastUsedUnixMs:     row.LastUsedUnixMs,
		CreatedAtUnixMs:    row.CreatedAtUnixMs,
		UpdatedAtUnixMs:    row.UpdatedAtUnixMs,
	}
}
