package handlers

import (
	"context"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	ws "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const websocketAuthorizationTimeout = 5 * time.Second

// newUpgrader creates a WebSocket upgrader that validates origins against the
// configured allowed origins. If no origins are configured, all are allowed.
func newUpgrader(allowedOrigins map[string]bool) websocket.FastHTTPUpgrader {
	return websocket.FastHTTPUpgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
			origin := string(ctx.Request.Header.Peek("Origin"))
			return middleware.IsOriginAllowed(origin, allowedOrigins)
		},
	}
}

// wsUpgrader returns a WebSocket upgrader configured with the app's allowed origins.
func (a *App) wsUpgrader() websocket.FastHTTPUpgrader {
	allowedOrigins := middleware.ParseAllowedOrigins(a.Config.Server.AllowedOrigins)
	return newUpgrader(allowedOrigins)
}

// WebSocketHandler handles WebSocket connections.
// Authentication is performed via message-based auth after the upgrade:
// the client must send {"type":"auth","payload":{"token":"<jwt>"}} within 5 seconds.
func (a *App) WebSocketHandler(r *fastglue.Request) error {
	// Upgrade to WebSocket immediately (unauthenticated)
	up := a.wsUpgrader()
	err := up.Upgrade(r.RequestCtx, func(conn *websocket.Conn) {
		// Create unauthenticated client — auth happens via first message
		client := ws.NewUnauthenticatedClient(a.WSHub, conn, a.validateWSTokenFn())

		// Start pumps in goroutines
		// Client self-registers with hub after successful auth message
		go client.WritePump()
		client.ReadPump() // Blocking - runs until connection closes
	})

	if err != nil {
		a.Log.Error("WebSocket upgrade failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "WebSocket upgrade failed", nil, "")
	}

	return nil
}

// validateWSTokenFn returns a function that validates a JWT token
// and returns user ID and organization ID.
func (a *App) validateWSTokenFn() ws.AuthenticateFn {
	return func(tokenString string) (uuid.UUID, uuid.UUID, error) {
		root := a.rootApp()
		if root == nil || root.Config == nil || root.DB == nil {
			return uuid.Nil, uuid.Nil, jwt.ErrTokenInvalidClaims
		}
		token, err := jwt.ParseWithClaims(tokenString, &middleware.JWTClaims{}, func(token *jwt.Token) (any, error) {
			return []byte(root.Config.JWT.Secret), nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))

		if err != nil || !token.Valid {
			return uuid.Nil, uuid.Nil, jwt.ErrTokenInvalidClaims
		}

		claims, ok := token.Claims.(*middleware.JWTClaims)
		if !ok || claims.Subject != middleware.JWTSubjectWebSocket || claims.ID != "" ||
			claims.UserID == uuid.Nil || claims.OrganizationID == uuid.Nil {
			return uuid.Nil, uuid.Nil, jwt.ErrTokenInvalidClaims
		}

		ctx, cancel := context.WithTimeout(context.Background(), websocketAuthorizationTimeout)
		defer cancel()
		err = database.WithTenantReadCommitted(
			root.DB.WithContext(ctx),
			claims.OrganizationID,
			func(tx *gorm.DB) error {
				purpose, purposeErr := database.IsPlatformComplianceOrganization(tx, claims.OrganizationID)
				if purposeErr != nil || purpose {
					return jwt.ErrTokenInvalidClaims
				}
				var user models.User
				if userErr := tx.Where("id = ?", claims.UserID).First(&user).Error; userErr != nil || !user.IsActive {
					return jwt.ErrTokenInvalidClaims
				}
				scoped := root.scopedApp(tx, claims.OrganizationID)
				if membershipErr := scoped.applyOrganizationMembership(&user, claims.OrganizationID, false); membershipErr != nil ||
					user.OrganizationID != claims.OrganizationID {
					return jwt.ErrTokenInvalidClaims
				}
				return nil
			},
		)
		if err != nil {
			return uuid.Nil, uuid.Nil, jwt.ErrTokenInvalidClaims
		}
		return claims.UserID, claims.OrganizationID, nil
	}
}
