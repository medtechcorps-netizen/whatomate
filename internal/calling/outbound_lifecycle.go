package calling

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// withActiveCallingAccount starts only after the potentially long WebRTC/ICE
// setup has completed. It reloads the current credential while holding a shared
// account-row lock across the Graph write, so a lifecycle disconnect committed
// first prevents the write and a disconnect arriving during it waits for the
// already-started provider operation to finish.
func (m *Manager) withActiveCallingAccount(
	ctx context.Context,
	organizationID, accountID uuid.UUID,
	accountName, expectedPhoneID string,
	write func(*whatsapp.Account) error,
) error {
	if m == nil || m.db == nil || organizationID == uuid.Nil || write == nil {
		return whatsappaccount.ErrOutboundInactive
	}
	accountName = strings.TrimSpace(accountName)
	expectedPhoneID = strings.TrimSpace(expectedPhoneID)
	if accountID == uuid.Nil && (accountName == "" || expectedPhoneID == "") {
		return whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}

	return database.WithTenantReadCommitted(
		m.db.WithContext(ctx),
		organizationID,
		func(tx *gorm.DB) error {
			query := tx.Clauses(clause.Locking{Strength: "SHARE"}).
				Where("organization_id = ?", organizationID)
			if accountID != uuid.Nil {
				query = query.Where("id = ?", accountID)
			} else {
				query = query.Where(
					"name = ? AND BTRIM(phone_id) = BTRIM(?)",
					accountName,
					expectedPhoneID,
				)
			}

			var account models.WhatsAppAccount
			if err := query.First(&account).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return whatsappaccount.ErrOutboundInactive
				}
				return fmt.Errorf("lock WhatsApp account for calling: %w", err)
			}
			if expectedPhoneID != "" && strings.TrimSpace(account.PhoneID) != expectedPhoneID {
				return whatsappaccount.ErrOutboundInactive
			}
			if err := whatsappaccount.RequireActiveForOutbound(&account); err != nil {
				return err
			}
			if account.AccessTokenExpiresAt != nil &&
				!account.AccessTokenExpiresAt.After(time.Now().UTC()) {
				return errors.New("WhatsApp account access token has expired")
			}

			account.DecryptSecrets(m.encryptionKey)
			if strings.TrimSpace(account.AccessToken) == "" ||
				appcrypto.IsEncrypted(account.AccessToken) {
				return errors.New("WhatsApp account credential is unavailable for calling")
			}
			if strings.TrimSpace(account.PhoneID) == "" ||
				strings.TrimSpace(account.APIVersion) == "" {
				return errors.New("WhatsApp account provider binding is unavailable for calling")
			}
			return write(account.ToWAAccount())
		},
	)
}

func (m *Manager) initiateActiveCall(
	ctx context.Context,
	organizationID uuid.UUID,
	accountName string,
	previous *whatsapp.Account,
	recipient whatsapp.Recipient,
	sdpOffer string,
) (string, error) {
	if m == nil || m.whatsapp == nil || previous == nil {
		return "", whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var callID string
	err := m.withActiveCallingAccount(
		ctx,
		organizationID,
		uuid.Nil,
		accountName,
		previous.PhoneID,
		func(current *whatsapp.Account) error {
			var err error
			callID, err = m.whatsapp.InitiateCall(ctx, current, recipient, sdpOffer)
			return err
		},
	)
	return callID, err
}

func (m *Manager) withActiveIncomingCallAccount(
	ctx context.Context,
	session *CallSession,
	account *models.WhatsAppAccount,
	write func(*whatsapp.Account) error,
) error {
	if session == nil || account == nil ||
		session.OrganizationID == uuid.Nil ||
		session.OrganizationID != account.OrganizationID ||
		strings.TrimSpace(session.AccountName) != strings.TrimSpace(account.Name) {
		return whatsappaccount.ErrOutboundInactive
	}
	return m.withActiveCallingAccount(
		ctx,
		session.OrganizationID,
		account.ID,
		account.Name,
		account.PhoneID,
		write,
	)
}

func (m *Manager) preAcceptActiveCall(
	ctx context.Context,
	session *CallSession,
	account *models.WhatsAppAccount,
	sdpAnswer string,
) error {
	if m == nil || m.whatsapp == nil || session == nil {
		return whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.withActiveIncomingCallAccount(
		ctx,
		session,
		account,
		func(current *whatsapp.Account) error {
			return m.whatsapp.PreAcceptCall(ctx, current, session.ID, sdpAnswer)
		},
	)
}

func (m *Manager) acceptActiveCall(
	ctx context.Context,
	session *CallSession,
	account *models.WhatsAppAccount,
	sdpAnswer string,
) error {
	if m == nil || m.whatsapp == nil || session == nil {
		return whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.withActiveIncomingCallAccount(
		ctx,
		session,
		account,
		func(current *whatsapp.Account) error {
			return m.whatsapp.AcceptCall(ctx, current, session.ID, sdpAnswer)
		},
	)
}

func (m *Manager) rejectActiveCall(
	ctx context.Context,
	session *CallSession,
	account *models.WhatsAppAccount,
) error {
	if m == nil || m.whatsapp == nil || session == nil {
		return whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.withActiveIncomingCallAccount(
		ctx,
		session,
		account,
		func(current *whatsapp.Account) error {
			return m.whatsapp.RejectCall(ctx, current, session.ID)
		},
	)
}

func (m *Manager) terminateActiveCall(
	ctx context.Context,
	session *CallSession,
	previous *whatsapp.Account,
) error {
	if m == nil || m.whatsapp == nil || session == nil || previous == nil ||
		session.OrganizationID == uuid.Nil {
		return whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.withActiveCallingAccount(
		ctx,
		session.OrganizationID,
		uuid.Nil,
		session.AccountName,
		previous.PhoneID,
		func(current *whatsapp.Account) error {
			return m.whatsapp.TerminateCall(ctx, current, session.ID)
		},
	)
}
