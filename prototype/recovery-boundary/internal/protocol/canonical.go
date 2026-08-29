// Package protocol contains the local-only recovery-boundary protocol model.
//
// The package deliberately has no provider or network client. Production
// constructors fail closed in Gate A; the fault-injecting implementations in
// this package exist only behind explicit unexported test-oracle constructors.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxOperationBodyBytes = 4096
	MaxAuthEnvelopeBytes  = 4096

	OperationSchema = "recovery-boundary-operation/v1"
	AuthSchema      = "recovery-boundary-auth/v1"
)

const (
	RoleWriter   = "writer"
	RoleObserver = "observer"

	ActionMarkerCAS  = "marker-cas"
	ActionMarkerRead = "marker-read"

	MethodAuthorize      = "authorize"
	MethodBrokerReadback = "broker-readback"
	MethodTerminalProof  = "terminal-proof"
	MethodStatus         = "status"
)

var (
	canonicalIntegerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	positiveDecimalPattern  = regexp.MustCompile(`^[1-9][0-9]*$`)
	syntheticIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	semanticPattern         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	hexSHA256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gitCommitPattern        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hexNoncePattern         = regexp.MustCompile(`^[0-9a-f]{32,128}$`)
	canonicalJTIPattern     = regexp.MustCompile(`^(?:[0-9a-f]{32,128}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
)

type OperationBody struct {
	Schema                   string `json:"schema"`
	OperationID              string `json:"operation_id"`
	Generation               uint64 `json:"generation"`
	Role                     string `json:"role"`
	Phase                    string `json:"phase"`
	Action                   string `json:"action"`
	ControlSHA               string `json:"control_sha"`
	RuntimeSourceSHA         string `json:"runtime_source_sha"`
	WorkflowDefinitionSHA256 string `json:"workflow_definition_sha256"`
	ConfigSHA256             string `json:"config_sha256"`
	ImageSHA256              string `json:"image_sha256"`
	AppSpecSHA256            string `json:"app_spec_sha256"`
	RequestedAtUTC           string `json:"requested_at_utc"`
}

// AuthEnvelope is fresh per call. None of its fields participate in the
// immutable OperationBody digest.
type AuthEnvelope struct {
	Schema              string `json:"schema"`
	OperationBodySHA256 string `json:"operation_body_sha256"`
	Method              string `json:"method"`
	JTI                 string `json:"jti"`
	Challenge           string `json:"challenge"`
	IssuedAtUTC         string `json:"issued_at_utc"`
}

func (b OperationBody) Validate() error {
	if b.Schema != OperationSchema {
		return fmt.Errorf("unexpected operation schema %q", b.Schema)
	}
	if !syntheticIDPattern.MatchString(b.OperationID) {
		return errors.New("operation_id is not a canonical synthetic identifier")
	}
	if b.Generation == 0 {
		return errors.New("generation must be positive")
	}
	if b.Role != RoleWriter && b.Role != RoleObserver {
		return errors.New("role must be writer or observer")
	}
	if !semanticPattern.MatchString(b.Phase) {
		return errors.New("phase is not canonical")
	}
	switch b.Action {
	case ActionMarkerCAS, ActionMarkerRead:
	default:
		return errors.New("action must be marker-cas or marker-read")
	}
	if !gitCommitPattern.MatchString(b.ControlSHA) {
		return errors.New("control_sha must be an exact lowercase Git commit SHA")
	}
	if !gitCommitPattern.MatchString(b.RuntimeSourceSHA) {
		return errors.New("runtime_source_sha must be an exact lowercase Git commit SHA")
	}
	for name, value := range map[string]string{
		"workflow_definition_sha256": b.WorkflowDefinitionSHA256,
		"config_sha256":              b.ConfigSHA256,
		"image_sha256":               b.ImageSHA256,
		"app_spec_sha256":            b.AppSpecSHA256,
	} {
		if !hexSHA256Pattern.MatchString(value) {
			return fmt.Errorf("%s must be lowercase SHA-256", name)
		}
	}
	if _, err := parseCanonicalUTC(b.RequestedAtUTC); err != nil {
		return fmt.Errorf("requested_at_utc: %w", err)
	}
	return nil
}

func (a AuthEnvelope) Validate() error {
	if a.Schema != AuthSchema {
		return fmt.Errorf("unexpected auth schema %q", a.Schema)
	}
	if !hexSHA256Pattern.MatchString(a.OperationBodySHA256) {
		return errors.New("operation_body_sha256 must be lowercase SHA-256")
	}
	if !validAuthMethod(a.Method) {
		return errors.New("method is not an approved protocol endpoint")
	}
	if !canonicalJTIPattern.MatchString(a.JTI) {
		return errors.New("jti must be a canonical high-entropy hexadecimal value or lowercase UUID")
	}
	if !hexNoncePattern.MatchString(a.Challenge) {
		return errors.New("challenge must be a canonical high-entropy hexadecimal value")
	}
	if a.JTI == a.Challenge {
		return errors.New("jti and challenge must be independent")
	}
	if _, err := parseCanonicalUTC(a.IssuedAtUTC); err != nil {
		return fmt.Errorf("issued_at_utc: %w", err)
	}
	return nil
}

func validAuthMethod(method string) bool {
	switch method {
	case MethodAuthorize, MethodBrokerReadback, MethodTerminalProof, MethodStatus:
		return true
	default:
		return false
	}
}

func parseCanonicalUTC(value string) (time.Time, error) {
	if !strings.HasSuffix(value, "Z") {
		return time.Time{}, errors.New("timestamp must use UTC Z form")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New("timestamp must be RFC3339Nano")
	}
	if parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("timestamp is not in canonical UTC form")
	}
	return parsed, nil
}

func MarshalOperationBody(body OperationBody) ([]byte, error) {
	if err := body.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(body)
}

func DecodeOperationBody(raw []byte) (OperationBody, error) {
	var body OperationBody
	if err := decodeCanonical(raw, MaxOperationBodyBytes, &body); err != nil {
		return OperationBody{}, err
	}
	if err := body.Validate(); err != nil {
		return OperationBody{}, err
	}
	return body, nil
}

func MarshalAuthEnvelope(envelope AuthEnvelope) ([]byte, error) {
	if err := envelope.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func DecodeAuthEnvelope(raw []byte) (AuthEnvelope, error) {
	var envelope AuthEnvelope
	if err := decodeCanonical(raw, MaxAuthEnvelopeBytes, &envelope); err != nil {
		return AuthEnvelope{}, err
	}
	if err := envelope.Validate(); err != nil {
		return AuthEnvelope{}, err
	}
	return envelope, nil
}

func decodeCanonical(raw []byte, limit int, dst any) error {
	if len(raw) == 0 {
		return errors.New("canonical JSON body is empty")
	}
	if len(raw) > limit {
		return fmt.Errorf("canonical JSON body exceeds %d bytes", limit)
	}
	if !utf8.Valid(raw) {
		return errors.New("canonical JSON must be valid UTF-8")
	}
	if err := inspectJSON(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode canonical JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	canonical, err := json.Marshal(dst)
	if err != nil {
		return fmt.Errorf("encode canonical JSON: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("JSON encoding is not canonical")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are forbidden")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func inspectJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON token: %w", err)
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("invalid object key: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if err := validateCanonicalString(key); err != nil {
					return fmt.Errorf("object key: %w", err)
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := inspectJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("unterminated JSON object")
			}
		case '[':
			for decoder.More() {
				if err := inspectJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("unterminated JSON array")
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		if !canonicalIntegerPattern.MatchString(value.String()) {
			return fmt.Errorf("JSON number %q is not a canonical unsigned integer", value)
		}
	case string:
		if err := validateCanonicalString(value); err != nil {
			return err
		}
	case bool, nil:
		return nil
	default:
		return fmt.Errorf("unexpected JSON token type %T", token)
	}
	return nil
}

// Gate A intentionally restricts signed protocol strings to printable ASCII.
// That makes Unicode equivalence unambiguous without a normalization library:
// all alternate Unicode forms are rejected rather than silently normalized.
func validateCanonicalString(value string) error {
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return errors.New("protocol strings must use canonical printable ASCII")
		}
	}
	return nil
}

func OperationBodyDigest(canonicalBody []byte) ([32]byte, error) {
	if _, err := DecodeOperationBody(canonicalBody); err != nil {
		return [32]byte{}, err
	}
	return domainHash("operation-body", canonicalBody), nil
}

func AuthEnvelopeDigest(canonicalEnvelope []byte) ([32]byte, error) {
	if _, err := DecodeAuthEnvelope(canonicalEnvelope); err != nil {
		return [32]byte{}, err
	}
	return domainHash("auth-envelope", canonicalEnvelope), nil
}

func domainHash(domain string, payload []byte) [32]byte {
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(len(payload)))
	hasher := sha256.New()
	hasher.Write([]byte("rereply-recovery-boundary/hash/v1\x00"))
	hasher.Write([]byte(domain))
	hasher.Write([]byte{0})
	hasher.Write(prefix[:])
	hasher.Write(payload)
	var result [32]byte
	copy(result[:], hasher.Sum(nil))
	return result
}

func SHA256Hex(value [32]byte) string {
	return hex.EncodeToString(value[:])
}
