package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

var ErrProxyRouteTerminalSessionNotFound = errors.New("proxy route terminal session not found")

type ProxyRoute struct {
	RouteKey        string
	OwnerID         string
	SessionID       string
	ScopedSessionID string
	WorkerID        string
	Port            int
	CreatedAtUnixMs int64
	ExpiresAtUnixMs int64
}

func (s *Store) InsertProxyRoute(ctx context.Context, route ProxyRoute) (bool, error) {
	if s == nil || s.queries == nil {
		return false, ErrPersistenceDBRequired
	}
	route = normalizeProxyRoute(route)
	if !validProxyRoute(route) {
		return false, errors.New("invalid proxy route")
	}
	rows, err := s.queries.InsertProxyRoute(ctx, sqlc.InsertProxyRouteParams{
		RouteKey:        route.RouteKey,
		OwnerID:         route.OwnerID,
		SessionID:       route.SessionID,
		ScopedSessionID: route.ScopedSessionID,
		WorkerID:        route.WorkerID,
		Port:            int64(route.Port),
		CreatedAtUnixMs: route.CreatedAtUnixMs,
		ExpiresAtUnixMs: route.ExpiresAtUnixMs,
	})
	if err != nil {
		return false, err
	}
	if rows == 0 {
		terminalRoute, lookupErr := s.queries.GetTerminalSessionRouteBySession(ctx, route.ScopedSessionID)
		switch {
		case errors.Is(lookupErr, sql.ErrNoRows):
			return false, ErrProxyRouteTerminalSessionNotFound
		case lookupErr != nil:
			return false, lookupErr
		case terminalRoute.NodeID != route.WorkerID || terminalRoute.LeaseExpiresUnixMs <= route.CreatedAtUnixMs:
			return false, ErrProxyRouteTerminalSessionNotFound
		}
	}
	return rows > 0, nil
}

func (s *Store) DeleteProxyRoutesByScopedSessionID(ctx context.Context, scopedSessionID string) (int64, error) {
	if s == nil || s.queries == nil {
		return 0, ErrPersistenceDBRequired
	}
	return s.queries.DeleteProxyRoutesByScopedSessionID(ctx, strings.TrimSpace(scopedSessionID))
}

func (s *Store) DeleteProxyRoute(ctx context.Context, routeKey string, ownerID string) (bool, error) {
	if s == nil || s.queries == nil {
		return false, ErrPersistenceDBRequired
	}
	rows, err := s.queries.DeleteProxyRouteByKeyAndOwner(ctx, sqlc.DeleteProxyRouteByKeyAndOwnerParams{
		RouteKey: strings.TrimSpace(routeKey),
		OwnerID:  strings.TrimSpace(ownerID),
	})
	return rows > 0, err
}

func (s *Store) DeleteExpiredProxyRoutes(ctx context.Context, nowUnixMs int64) (int64, error) {
	if s == nil || s.queries == nil {
		return 0, ErrPersistenceDBRequired
	}
	return s.queries.DeleteExpiredProxyRoutes(ctx, nowUnixMs)
}

func (s *Store) LoadActiveProxyRoutes(ctx context.Context, nowUnixMs int64) ([]ProxyRoute, error) {
	if s == nil || s.queries == nil {
		return nil, ErrPersistenceDBRequired
	}
	rows, err := s.queries.ListActiveProxyRoutes(ctx, nowUnixMs)
	if err != nil {
		return nil, err
	}
	routes := make([]ProxyRoute, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, ProxyRoute{
			RouteKey:        row.RouteKey,
			OwnerID:         row.OwnerID,
			SessionID:       row.SessionID,
			ScopedSessionID: row.ScopedSessionID,
			WorkerID:        row.WorkerID,
			Port:            int(row.Port),
			CreatedAtUnixMs: row.CreatedAtUnixMs,
			ExpiresAtUnixMs: row.ExpiresAtUnixMs,
		})
	}
	return routes, nil
}

func normalizeProxyRoute(route ProxyRoute) ProxyRoute {
	route.RouteKey = strings.TrimSpace(route.RouteKey)
	route.OwnerID = strings.TrimSpace(route.OwnerID)
	route.SessionID = strings.TrimSpace(route.SessionID)
	route.ScopedSessionID = strings.TrimSpace(route.ScopedSessionID)
	route.WorkerID = strings.TrimSpace(route.WorkerID)
	return route
}

func validProxyRoute(route ProxyRoute) bool {
	return route.RouteKey != "" &&
		route.OwnerID != "" &&
		route.SessionID != "" &&
		route.ScopedSessionID != "" &&
		route.WorkerID != "" &&
		route.Port >= 1 && route.Port <= 65535 &&
		route.ExpiresAtUnixMs > route.CreatedAtUnixMs
}
