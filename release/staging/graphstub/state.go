package graphstub

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"time"
)

// Memory bounds. The stub is long-lived on staging, so every collection is
// capped and evicts its oldest entries.
const (
	maxMediaBytes       = 16 << 20
	maxStoredMediaBytes = 128 << 20
	maxStoredMessages   = 10_000
	maxTemplatesPerWABA = 500
	maxOAuthCodes       = 256
	oauthCodeLifetime   = 10 * time.Minute
)

type phone struct {
	account     Account
	registered  bool
	overrideURL string
	profile     map[string]any
}

type waba struct {
	id         string
	phones     []string
	subscribed bool
	templates  []*template
}

type template struct {
	ID              string
	Name            string
	Language        string
	Category        string
	Status          string
	ParameterFormat string
	Components      json.RawMessage
}

type media struct {
	ID       string
	PhoneID  string
	MimeType string
	SHA256   string
	Data     []byte
}

type sentMessage struct {
	ID      string
	PhoneID string
	To      string
}

type oauthCode struct {
	token   string
	expires time.Time
}

// state is the stub's synthetic Meta world. Server.mu guards it.
type state struct {
	phones     map[string]*phone
	wabas      map[string]*waba
	media      map[string]*media
	mediaOrder []string
	mediaBytes int
	messages   map[string]sentMessage
	sentOrder  []string
	codes      map[string]oauthCode
}

func newState(accounts []Account) *state {
	s := &state{
		phones:   make(map[string]*phone),
		wabas:    make(map[string]*waba),
		media:    make(map[string]*media),
		messages: make(map[string]sentMessage),
		codes:    make(map[string]oauthCode),
	}
	for _, account := range accounts {
		// Config.Validate already normalized and deduplicated these.
		_ = s.upsertAccount(account)
	}
	return s
}

var errPhoneInOtherWABA = errors.New("phone_number_id already belongs to another business account")

func (s *state) upsertAccount(account Account) error {
	if existing, ok := s.phones[account.PhoneNumberID]; ok {
		if existing.account.BusinessAccountID != account.BusinessAccountID {
			return errPhoneInOtherWABA
		}
		existing.account = account
		return nil
	}
	if len(s.phones) >= maxAccounts {
		return errors.New("too many accounts")
	}
	if _, isPhone := s.phones[account.BusinessAccountID]; isPhone {
		return errors.New("business_account_id is already a phone_number_id")
	}
	if _, isWABA := s.wabas[account.PhoneNumberID]; isWABA {
		return errors.New("phone_number_id is already a business_account_id")
	}
	owner, ok := s.wabas[account.BusinessAccountID]
	if !ok {
		owner = &waba{id: account.BusinessAccountID}
		s.wabas[owner.id] = owner
	}
	owner.phones = append(owner.phones, account.PhoneNumberID)
	s.phones[account.PhoneNumberID] = &phone{
		account: account,
		profile: map[string]any{
			"messaging_product":   "whatsapp",
			"about":               "Synthetic staging business",
			"address":             "",
			"description":         "",
			"email":               "",
			"profile_picture_url": "",
			"vertical":            "OTHER",
			"websites":            []string{},
		},
	}
	return nil
}

func (s *state) template(id string) (*waba, *template) {
	for _, owner := range s.wabas {
		for _, item := range owner.templates {
			if item.ID == id {
				return owner, item
			}
		}
	}
	return nil, nil
}

func (s *state) storeMedia(item *media) {
	for s.mediaBytes+len(item.Data) > maxStoredMediaBytes && len(s.mediaOrder) > 0 {
		s.deleteMedia(s.mediaOrder[0])
	}
	s.media[item.ID] = item
	s.mediaOrder = append(s.mediaOrder, item.ID)
	s.mediaBytes += len(item.Data)
}

func (s *state) deleteMedia(id string) bool {
	item, ok := s.media[id]
	if !ok {
		return false
	}
	delete(s.media, id)
	s.mediaBytes -= len(item.Data)
	for index, candidate := range s.mediaOrder {
		if candidate == id {
			s.mediaOrder = append(s.mediaOrder[:index], s.mediaOrder[index+1:]...)
			break
		}
	}
	return true
}

func (s *state) recordMessage(message sentMessage) {
	if len(s.sentOrder) >= maxStoredMessages {
		delete(s.messages, s.sentOrder[0])
		s.sentOrder = s.sentOrder[1:]
	}
	s.messages[message.ID] = message
	s.sentOrder = append(s.sentOrder, message.ID)
}

func (s *state) addCode(code, token string, now time.Time) error {
	for existing, grant := range s.codes {
		if !grant.expires.After(now) {
			delete(s.codes, existing)
		}
	}
	if len(s.codes) >= maxOAuthCodes {
		return errors.New("too many pending codes")
	}
	s.codes[code] = oauthCode{token: token, expires: now.Add(oauthCodeLifetime)}
	return nil
}

// takeCode consumes a code: each one exchanges at most once.
func (s *state) takeCode(code string, now time.Time) (string, bool) {
	grant, ok := s.codes[code]
	delete(s.codes, code)
	if !ok || !grant.expires.After(now) {
		return "", false
	}
	return grant.token, true
}

// newGraphID returns a random 15-digit decimal ID, below 2^53 so JSON number
// decoders (the product reads message_template_id as int64) keep it exact.
func newGraphID() string {
	value, err := rand.Int(rand.Reader, big.NewInt(9e14))
	if err != nil {
		panic("graphstub: crypto/rand failed")
	}
	return value.Add(value, big.NewInt(1e14)).String()
}

// newMessageID returns a WAMID-shaped ID made of URL-safe characters only.
func newMessageID() string {
	return "wamid." + randomToken(30)
}

func randomToken(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		panic("graphstub: crypto/rand failed")
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}
