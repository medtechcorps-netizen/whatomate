package graphstub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Graph route names. They label journal entries and log lines and select
// fault-injection targets; none of them carries an ID.
const (
	routeOAuth           = "oauth_access_token"
	routeDebugToken      = "debug_token"
	routePhone           = "phone"
	routeWABA            = "waba"
	routeMedia           = "media"
	routeTemplate        = "template"
	routeMessages        = "messages"
	routeMediaUpload     = "media_upload"
	routeMediaDownload   = "media_download"
	routePhoneNumbers    = "phone_numbers"
	routeTemplates       = "message_templates"
	routeSubscribedApps  = "subscribed_apps"
	routeRegister        = "register"
	routeBusinessProfile = "business_profile"
)

// mediaDownloadSegment prefixes the same-origin download URLs the stub hands
// out. It cannot collide with a Graph ID, which is always numeric.
const mediaDownloadSegment = "_media"

const maxJSONBody = 256 << 10

var (
	versionPattern      = regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,3}$`)
	templateNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,512}$`)
	languagePattern     = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)
	pinPattern          = regexp.MustCompile(`^[0-9]{6}$`)
)

// graphSegments splits a Graph path and drops an optional /v{n}.{m} prefix.
// It returns nil for an empty path or an empty segment.
func graphSegments(path string) []string {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) > 0 && versionPattern.MatchString(segments[0]) {
		segments = segments[1:]
	}
	if len(segments) == 0 {
		return nil
	}
	for _, segment := range segments {
		if segment == "" {
			return nil
		}
	}
	return segments
}

func (s *Server) serveGraph(c *call) {
	c.segments = graphSegments(c.r.URL.Path)
	if c.segments == nil {
		unsupported(c)
		return
	}
	switch {
	case len(c.segments) == 2 && c.segments[0] == "oauth" && c.segments[1] == "access_token":
		c.entry.Route = routeOAuth
		if !s.applyFault(c) {
			s.oauthAccessToken(c)
		}
		return
	case len(c.segments) == 1 && c.segments[0] == "debug_token":
		c.entry.Route = routeDebugToken
		if !s.applyFault(c) {
			s.debugToken(c)
		}
		return
	}
	if !oneOf(bearer(c.r), s.cfg.AccessTokens...) {
		c.entry.Route = "auth"
		graphError(c, http.StatusUnauthorized, 190, 0, "OAuthException", "Invalid OAuth access token - Cannot parse access token", false)
		return
	}
	route, handler := s.resolve(c)
	c.entry.Route = route
	if handler == nil {
		unsupported(c)
		return
	}
	if !s.applyFault(c) {
		handler(c)
	}
}

// resolve maps an authenticated request to its route and handler. A nil
// handler means the object is unknown or does not support the method.
func (s *Server) resolve(c *call) (string, func(*call)) {
	method, segments := c.r.Method, c.segments
	if len(segments) == 2 && segments[0] == mediaDownloadSegment {
		return routeMediaDownload, methodHandler(method, map[string]func(*call){http.MethodGet: s.downloadMedia})
	}
	if len(segments) > 2 {
		return "unsupported", nil
	}
	kind := s.objectKind(segments[0])
	if len(segments) == 1 {
		switch kind {
		case routePhone:
			return kind, methodHandler(method, map[string]func(*call){http.MethodGet: s.getPhone, http.MethodPost: s.setWebhookOverride})
		case routeWABA:
			return kind, methodHandler(method, map[string]func(*call){http.MethodGet: s.getWABA})
		case routeMedia:
			return kind, methodHandler(method, map[string]func(*call){http.MethodGet: s.getMedia, http.MethodDelete: s.deleteMedia})
		case routeTemplate:
			return kind, methodHandler(method, map[string]func(*call){http.MethodGet: s.getTemplate, http.MethodPost: s.updateTemplate})
		}
		return "unsupported", nil
	}
	edges := map[string]struct {
		route   string
		owner   string
		methods map[string]func(*call)
	}{
		"messages":                  {routeMessages, routePhone, map[string]func(*call){http.MethodPost: s.sendMessage}},
		"media":                     {routeMediaUpload, routePhone, map[string]func(*call){http.MethodPost: s.uploadMedia}},
		"register":                  {routeRegister, routePhone, map[string]func(*call){http.MethodPost: s.register}},
		"whatsapp_business_profile": {routeBusinessProfile, routePhone, map[string]func(*call){http.MethodGet: s.getBusinessProfile, http.MethodPost: s.updateBusinessProfile}},
		"phone_numbers":             {routePhoneNumbers, routeWABA, map[string]func(*call){http.MethodGet: s.phoneNumbers}},
		"message_templates":         {routeTemplates, routeWABA, map[string]func(*call){http.MethodGet: s.listTemplates, http.MethodPost: s.createTemplate, http.MethodDelete: s.deleteTemplate}},
		"subscribed_apps":           {routeSubscribedApps, routeWABA, map[string]func(*call){http.MethodGet: s.subscribedApps, http.MethodPost: s.subscribedApps, http.MethodDelete: s.subscribedApps}},
	}
	edge, ok := edges[segments[1]]
	if !ok {
		return "unsupported", nil
	}
	if kind != edge.owner {
		return edge.route, nil
	}
	return edge.route, methodHandler(method, edge.methods)
}

func methodHandler(method string, handlers map[string]func(*call)) func(*call) {
	return handlers[method]
}

func (s *Server) objectKind(id string) string {
	if !graphIDPattern.MatchString(id) {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state.phones[id] != nil:
		return routePhone
	case s.state.wabas[id] != nil:
		return routeWABA
	case s.state.media[id] != nil:
		return routeMedia
	}
	if _, item := s.state.template(id); item != nil {
		return routeTemplate
	}
	return ""
}

// requestedFields splits Graph's fields parameter at top level, so nested
// selections such as phone_numbers{id,name} count as one field.
func requestedFields(r *http.Request) []string {
	raw := r.URL.Query().Get("fields")
	var fields []string
	depth, start := 0, 0
	for index := 0; index <= len(raw); index++ {
		if index < len(raw) {
			switch raw[index] {
			case '{', '(':
				depth++
				continue
			case '}', ')':
				depth--
				continue
			case ',':
				if depth > 0 {
					continue
				}
			default:
				continue
			}
		}
		field := strings.TrimSpace(raw[start:index])
		if cut := strings.IndexAny(field, "{(."); cut >= 0 {
			field = field[:cut]
		}
		if field != "" {
			fields = append(fields, field)
		}
		start = index + 1
	}
	return fields
}

// selectFields returns id plus the requested (or default) fields of object.
func selectFields(r *http.Request, object map[string]any, defaults []string) map[string]any {
	fields := requestedFields(r)
	if len(fields) == 0 {
		fields = defaults
	}
	selected := map[string]any{}
	if id, ok := object["id"]; ok {
		selected["id"] = id
	}
	for _, field := range fields {
		if value, ok := object[field]; ok {
			selected[field] = value
		}
	}
	return selected
}

// paginate applies Graph cursor paging. It writes the error itself and
// returns ok=false when limit or after is malformed.
func paginate(c *call, items []map[string]any) (map[string]any, bool) {
	query := c.r.URL.Query()
	limit := 25
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			invalidParameter(c, "Param limit must be between 1 and 100")
			return nil, false
		}
		limit = value
	}
	offset := 0
	if raw := query.Get("after"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		value, convErr := strconv.Atoi(strings.TrimPrefix(string(decoded), "offset:"))
		if err != nil || convErr != nil || !strings.HasPrefix(string(decoded), "offset:") || value < 0 || value > len(items) {
			invalidParameter(c, "Param after is not a valid cursor")
			return nil, false
		}
		offset = value
	}
	end := min(offset+limit, len(items))
	page := append([]map[string]any{}, items[offset:end]...)
	result := map[string]any{"data": page}
	if end > offset {
		cursor := func(position int) string {
			return base64.RawURLEncoding.EncodeToString([]byte("offset:" + strconv.Itoa(position)))
		}
		paging := map[string]any{"cursors": map[string]string{"before": cursor(offset), "after": cursor(end)}}
		if end < len(items) {
			query.Set("after", cursor(end))
			next := url.URL{Scheme: "http", Host: c.r.Host, Path: c.r.URL.Path, RawQuery: query.Encode()}
			paging["next"] = next.String()
		}
		result["paging"] = paging
	}
	return result, true
}

func decodeJSON(c *call, target any) bool {
	body, ok := readBody(c, maxJSONBody)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, target); err != nil {
		invalidParameter(c, "The request body is not valid JSON")
		return false
	}
	return true
}

func (s *Server) phoneObject(item *phone) map[string]any {
	status := "PENDING"
	if item.registered {
		status = "CONNECTED"
	}
	configuration := map[string]any{"application": s.webhooks.target("")}
	if item.overrideURL != "" {
		configuration["phone_number"] = item.overrideURL
	}
	return map[string]any{
		"id":                       item.account.PhoneNumberID,
		"display_phone_number":     item.account.DisplayPhoneNumber,
		"verified_name":            item.account.VerifiedName,
		"code_verification_status": "VERIFIED",
		"account_mode":             "LIVE",
		"quality_rating":           "GREEN",
		"is_on_biz_app":            false,
		"platform_type":            "CLOUD_API",
		"name_status":              "APPROVED",
		"status":                   status,
		"messaging_limit_tier":     "TIER_1K",
		"whatsapp_business_manager_messaging_limit": "TIER_1K",
		"throughput":            map[string]string{"level": "STANDARD"},
		"webhook_configuration": configuration,
	}
}

var defaultPhoneFields = []string{"display_phone_number", "verified_name", "code_verification_status", "quality_rating", "platform_type", "throughput"}

func (s *Server) getPhone(c *call) {
	id := c.segments[0]
	c.entry.PhoneNumberID = id
	s.mu.Lock()
	item := s.state.phones[id]
	var object map[string]any
	if item != nil {
		object = s.phoneObject(item)
	}
	s.mu.Unlock()
	if object == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, selectFields(c.r, object, defaultPhoneFields))
}

// setWebhookOverride stores a phone's alternate callback, as Meta does for
// POST /{phone} with webhook_configuration. Deliveries still go to
// STUB_CALLBACK_ORIGIN; only the override's path and query are used.
func (s *Server) setWebhookOverride(c *call) {
	id := c.segments[0]
	c.entry.PhoneNumberID = id
	var request struct {
		WebhookConfiguration *struct {
			OverrideCallbackURI *string `json:"override_callback_uri"`
			VerifyToken         string  `json:"verify_token"`
		} `json:"webhook_configuration"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.WebhookConfiguration == nil || request.WebhookConfiguration.OverrideCallbackURI == nil {
		invalidParameter(c, "The parameter webhook_configuration.override_callback_uri is required")
		return
	}
	callback := strings.TrimSpace(*request.WebhookConfiguration.OverrideCallbackURI)
	if callback != "" {
		parsed, err := url.Parse(callback)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || len(callback) > 1024 {
			invalidParameter(c, "override_callback_uri must be an https URL")
			return
		}
		if RefusedHost(parsed.Hostname()) {
			c.entry.Error = "refused_callback_host"
			invalidParameter(c, "The Graph stub refuses production callback hosts")
			return
		}
		if strings.TrimSpace(request.WebhookConfiguration.VerifyToken) == "" {
			invalidParameter(c, "The parameter webhook_configuration.verify_token is required")
			return
		}
	}
	s.mu.Lock()
	item := s.state.phones[id]
	if item != nil {
		item.overrideURL = callback
	}
	s.mu.Unlock()
	if item == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
}

func wabaObject(id string) map[string]any {
	return map[string]any{
		"id":                           id,
		"name":                         "Graph Stub Business Account",
		"currency":                     "USD",
		"timezone_id":                  "1",
		"message_template_namespace":   "stub_" + id,
		"account_review_status":        "APPROVED",
		"business_verification_status": "verified",
		"ownership_type":               "SELF",
		"whatsapp_business_manager_messaging_limit": "TIER_1K",
	}
}

func (s *Server) getWABA(c *call) {
	id := c.segments[0]
	c.entry.BusinessAccountID = id
	s.mu.Lock()
	known := s.state.wabas[id] != nil
	s.mu.Unlock()
	if !known {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, selectFields(c.r, wabaObject(id), []string{"name", "timezone_id", "message_template_namespace"}))
}

func (s *Server) phoneNumbers(c *call) {
	id := c.segments[0]
	c.entry.BusinessAccountID = id
	s.mu.Lock()
	var items []map[string]any
	if owner := s.state.wabas[id]; owner != nil {
		for _, phoneID := range owner.phones {
			items = append(items, selectFields(c.r, s.phoneObject(s.state.phones[phoneID]),
				[]string{"display_phone_number", "verified_name", "quality_rating"}))
		}
	}
	s.mu.Unlock()
	if page, ok := paginate(c, items); ok {
		writeJSON(c.w, http.StatusOK, page)
	}
}

func templateObject(item *template) map[string]any {
	object := map[string]any{
		"id":              item.ID,
		"name":            item.Name,
		"language":        item.Language,
		"category":        item.Category,
		"status":          item.Status,
		"components":      item.Components,
		"quality_score":   map[string]string{"score": "UNKNOWN"},
		"rejected_reason": "NONE",
	}
	if item.ParameterFormat != "" {
		object["parameter_format"] = item.ParameterFormat
	}
	return object
}

var defaultTemplateFields = []string{"name", "language", "category", "status", "components", "parameter_format"}

func (s *Server) listTemplates(c *call) {
	id := c.segments[0]
	c.entry.BusinessAccountID = id
	name := c.r.URL.Query().Get("name")
	s.mu.Lock()
	items := []map[string]any{}
	if owner := s.state.wabas[id]; owner != nil {
		for _, item := range owner.templates {
			if name == "" || item.Name == name {
				items = append(items, selectFields(c.r, templateObject(item), defaultTemplateFields))
			}
		}
	}
	s.mu.Unlock()
	if page, ok := paginate(c, items); ok {
		writeJSON(c.w, http.StatusOK, page)
	}
}

func validComponents(raw json.RawMessage) bool {
	var components []map[string]any
	return json.Unmarshal(raw, &components) == nil && len(components) > 0 && len(components) <= 10
}

func (s *Server) createTemplate(c *call) {
	id := c.segments[0]
	c.entry.BusinessAccountID = id
	var request struct {
		Name            string          `json:"name"`
		Language        string          `json:"language"`
		Category        string          `json:"category"`
		ParameterFormat string          `json:"parameter_format"`
		Components      json.RawMessage `json:"components"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	c.entry.TemplateName = request.Name
	category := strings.ToUpper(strings.TrimSpace(request.Category))
	parameterFormat := strings.ToUpper(strings.TrimSpace(request.ParameterFormat))
	switch {
	case !templateNamePattern.MatchString(request.Name):
		invalidParameter(c, "Param name must contain only lowercase letters, digits and underscores")
		return
	case !languagePattern.MatchString(request.Language):
		invalidParameter(c, "Param language is not a supported language code")
		return
	case category != "MARKETING" && category != "UTILITY" && category != "AUTHENTICATION":
		invalidParameter(c, "Param category must be MARKETING, UTILITY or AUTHENTICATION")
		return
	case parameterFormat != "" && parameterFormat != "NAMED" && parameterFormat != "POSITIONAL":
		invalidParameter(c, "Param parameter_format must be NAMED or POSITIONAL")
		return
	case !validComponents(request.Components):
		invalidParameter(c, "Param components must be a non-empty array")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.state.wabas[id]
	if owner == nil {
		unsupported(c)
		return
	}
	for _, existing := range owner.templates {
		if existing.Name == request.Name && existing.Language == request.Language {
			graphError(c, http.StatusBadRequest, 100, 2388024, "OAuthException",
				"(#100) Content in this language already exists for this message template", false)
			return
		}
	}
	if len(owner.templates) >= maxTemplatesPerWABA {
		graphError(c, http.StatusBadRequest, 100, 2388025, "OAuthException", "(#100) Message template limit reached", false)
		return
	}
	item := &template{
		ID:              newGraphID(),
		Name:            request.Name,
		Language:        request.Language,
		Category:        category,
		Status:          "PENDING",
		ParameterFormat: parameterFormat,
		Components:      append(json.RawMessage(nil), request.Components...),
	}
	owner.templates = append(owner.templates, item)
	writeJSON(c.w, http.StatusOK, map[string]string{"id": item.ID, "status": item.Status, "category": item.Category})
}

// deleteTemplate deletes every language of the named template, as Meta does
// for DELETE ?name=.
func (s *Server) deleteTemplate(c *call) {
	id := c.segments[0]
	c.entry.BusinessAccountID = id
	name := c.r.URL.Query().Get("name")
	c.entry.TemplateName = name
	if name == "" {
		invalidParameter(c, "The parameter name is required")
		return
	}
	s.mu.Lock()
	removed := false
	if owner := s.state.wabas[id]; owner != nil {
		kept := owner.templates[:0]
		for _, item := range owner.templates {
			if item.Name == name {
				removed = true
				continue
			}
			kept = append(kept, item)
		}
		owner.templates = kept
	}
	s.mu.Unlock()
	if !removed {
		graphError(c, http.StatusBadRequest, 100, 2593002, "OAuthException", "(#100) Message template not found", false)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) getTemplate(c *call) {
	s.mu.Lock()
	_, item := s.state.template(c.segments[0])
	var object map[string]any
	if item != nil {
		c.entry.TemplateName = item.Name
		object = templateObject(item)
	}
	s.mu.Unlock()
	if object == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, selectFields(c.r, object, defaultTemplateFields))
}

// updateTemplate edits components, the only part of a template Meta lets the
// product change (pkg/whatsapp/template.go). The status is left as it was.
func (s *Server) updateTemplate(c *call) {
	var request struct {
		Components json.RawMessage `json:"components"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if !validComponents(request.Components) {
		invalidParameter(c, "Param components must be a non-empty array")
		return
	}
	s.mu.Lock()
	_, item := s.state.template(c.segments[0])
	if item != nil {
		c.entry.TemplateName = item.Name
		item.Components = append(json.RawMessage(nil), request.Components...)
	}
	s.mu.Unlock()
	if item == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) subscribedApps(c *call) {
	id := c.segments[0]
	c.entry.BusinessAccountID = id
	s.mu.Lock()
	owner := s.state.wabas[id]
	subscribed := false
	if owner != nil {
		switch c.r.Method {
		case http.MethodPost:
			owner.subscribed = true
		case http.MethodDelete:
			owner.subscribed = false
		}
		subscribed = owner.subscribed
	}
	s.mu.Unlock()
	switch {
	case owner == nil:
		unsupported(c)
	case c.r.Method != http.MethodGet:
		writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
	case subscribed:
		writeJSON(c.w, http.StatusOK, map[string]any{"data": []any{map[string]any{
			"whatsapp_business_api_data": map[string]string{"id": s.cfg.AppID, "name": "Graph Stub App"},
		}}})
	default:
		writeJSON(c.w, http.StatusOK, map[string]any{"data": []any{}})
	}
}

func (s *Server) register(c *call) {
	id := c.segments[0]
	c.entry.PhoneNumberID = id
	var request struct {
		MessagingProduct string `json:"messaging_product"`
		PIN              string `json:"pin"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.MessagingProduct != "whatsapp" {
		invalidParameter(c, "The parameter messaging_product is required")
		return
	}
	if !pinPattern.MatchString(request.PIN) {
		invalidParameter(c, "Param pin must be a 6-digit number")
		return
	}
	s.mu.Lock()
	item := s.state.phones[id]
	if item != nil {
		item.registered = true
	}
	s.mu.Unlock()
	if item == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
}

var defaultProfileFields = []string{"about", "address", "description", "email", "profile_picture_url", "websites", "vertical", "messaging_product"}

func (s *Server) getBusinessProfile(c *call) {
	id := c.segments[0]
	c.entry.PhoneNumberID = id
	s.mu.Lock()
	var profile map[string]any
	if item := s.state.phones[id]; item != nil {
		profile = selectFields(c.r, item.profile, defaultProfileFields)
		profile["messaging_product"] = "whatsapp"
	}
	s.mu.Unlock()
	if profile == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"data": []any{profile}})
}

func (s *Server) updateBusinessProfile(c *call) {
	id := c.segments[0]
	c.entry.PhoneNumberID = id
	var request struct {
		MessagingProduct string    `json:"messaging_product"`
		About            *string   `json:"about"`
		Address          *string   `json:"address"`
		Description      *string   `json:"description"`
		Email            *string   `json:"email"`
		Vertical         *string   `json:"vertical"`
		Websites         *[]string `json:"websites"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.MessagingProduct != "whatsapp" {
		invalidParameter(c, "The parameter messaging_product is required")
		return
	}
	updates := map[string]any{}
	for _, field := range []struct {
		name  string
		value *string
		limit int
	}{
		{"about", request.About, 139},
		{"address", request.Address, 256},
		{"description", request.Description, 512},
		{"email", request.Email, 128},
		{"vertical", request.Vertical, 64},
	} {
		if field.value == nil {
			continue
		}
		if utf8.RuneCountInString(*field.value) > field.limit {
			invalidParameter(c, "Param "+field.name+" is too long")
			return
		}
		updates[field.name] = *field.value
	}
	if request.Websites != nil {
		if len(*request.Websites) > 2 {
			invalidParameter(c, "Param websites accepts at most 2 URLs")
			return
		}
		updates["websites"] = append([]string{}, *request.Websites...)
	}
	s.mu.Lock()
	item := s.state.phones[id]
	if item != nil {
		for field, value := range updates {
			item.profile[field] = value
		}
	}
	s.mu.Unlock()
	if item == nil {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
}
