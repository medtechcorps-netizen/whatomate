package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const organizationInvitationTTL = 24 * time.Hour

var errInvalidOrganizationInvitation = errors.New("invalid organization invitation")

type organizationInvitationClaims struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	RoleID         uuid.UUID `json:"role_id"`
	jwt.RegisteredClaims
}

type createOrganizationInvitationRequest struct {
	RoleID *uuid.UUID `json:"role_id"`
}

func organizationInvitationKey(jti string) string {
	return fmt.Sprintf("organization-invitation:%s", jti)
}

// CreateOrganizationInvitation creates a short-lived, single-use registration
// token for the current organization.
func (a *App) CreateOrganizationInvitation(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceOrganizations, models.ActionAssign)
	if err != nil {
		return nil
	}

	var req createOrganizationInvitationRequest
	if len(r.RequestCtx.PostBody()) > 0 {
		if err := a.decodeRequest(r, &req); err != nil {
			return nil
		}
	}

	var role models.CustomRole
	if req.RoleID != nil {
		if err := a.DB.Where("id = ? AND organization_id = ?", *req.RoleID, orgID).First(&role).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid role", nil, "")
		}
	} else {
		if err := a.DB.Where("organization_id = ? AND is_default = ?", orgID, true).First(&role).Error; err != nil {
			if err := a.DB.Where("organization_id = ? AND name = ? AND is_system = ?", orgID, "agent", true).First(&role).Error; err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Organization has no default role", nil, "")
			}
		}
	}

	now := time.Now()
	expiresAt := now.Add(organizationInvitationTTL)
	jti := uuid.NewString()
	claims := organizationInvitationClaims{
		OrganizationID: orgID,
		RoleID:         role.ID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "whatomate",
			Subject:   "organization-invitation",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(a.Config.JWT.Secret))
	if err != nil {
		a.Log.Error("Failed to sign organization invitation", "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create invitation", nil, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Redis.Set(ctx, organizationInvitationKey(jti), orgID.String(), organizationInvitationTTL).Err(); err != nil {
		a.Log.Error("Failed to store organization invitation", "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create invitation", nil, "")
	}

	return r.SendEnvelope(map[string]any{
		"token":      signed,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

// consumeOrganizationInvitation validates and atomically consumes an
// invitation. A failed registration after this point requires a fresh invite;
// this intentionally prevents replay and concurrent use.
func (a *App) consumeOrganizationInvitation(tokenString string) (*organizationInvitationClaims, error) {
	if tokenString == "" || a.Redis == nil {
		return nil, errInvalidOrganizationInvitation
	}

	token, err := jwt.ParseWithClaims(
		tokenString,
		&organizationInvitationClaims{},
		func(token *jwt.Token) (any, error) {
			return []byte(a.Config.JWT.Secret), nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("whatomate"),
	)
	if err != nil || !token.Valid {
		return nil, errInvalidOrganizationInvitation
	}

	claims, ok := token.Claims.(*organizationInvitationClaims)
	if !ok ||
		claims.Subject != "organization-invitation" ||
		claims.ID == "" ||
		claims.OrganizationID == uuid.Nil ||
		claims.RoleID == uuid.Nil {
		return nil, errInvalidOrganizationInvitation
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	orgID, err := a.Redis.GetDel(ctx, organizationInvitationKey(claims.ID)).Result()
	if err != nil || orgID != claims.OrganizationID.String() {
		return nil, errInvalidOrganizationInvitation
	}

	return claims, nil
}
