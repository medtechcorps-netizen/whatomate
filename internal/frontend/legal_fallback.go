package frontend

import (
	"bytes"
	"fmt"
	"html/template"
	pathpkg "path"
	"strings"
)

const (
	appFallbackStartMarker = "<!-- REREPLY_FALLBACK_START -->"
	appFallbackEndMarker   = "<!-- REREPLY_FALLBACK_END -->"
	titleStartMarker       = "<!-- REREPLY_TITLE_START -->"
	titleEndMarker         = "<!-- REREPLY_TITLE_END -->"
	descriptionStartMarker = "<!-- REREPLY_DESCRIPTION_START -->"
	descriptionEndMarker   = "<!-- REREPLY_DESCRIPTION_END -->"
)

type legalFallbackSection struct {
	ID         string
	Title      string
	Paragraphs []string
	Bullets    []string
}

type legalFallbackDocument struct {
	Key       string
	Route     string
	Eyebrow   string
	Title     string
	Effective string
	Summary   string
	Sections  []legalFallbackSection
}

type renderedLegalFallbackSection struct {
	legalFallbackSection
	Href string
}

type renderedLegalFallbackDocument struct {
	legalFallbackDocument
	Sections     []renderedLegalFallbackSection
	AboutHref    string
	PrivacyHref  string
	TermsHref    string
	DeletionHref string
}

// legalFallbackDocuments is deliberately a concise, non-JavaScript summary of
// the full documents rendered by LegalDocumentView.vue. It gives provider
// crawlers and users without JavaScript the material terms, privacy disclosures,
// deletion process, effective date, and contact without duplicating every line
// of the richer interactive page.
var legalFallbackDocuments = map[string]legalFallbackDocument{
	"/privacy": {
		Key:       "privacy",
		Route:     "/privacy",
		Eyebrow:   "ReReply trust centre",
		Title:     "Privacy Policy",
		Effective: "Effective 3 August 2026",
		Summary:   "How ReReply protects and processes account, CRM and conversation data across separate customer organisations.",
		Sections: []legalFallbackSection{
			{
				ID:    "scope",
				Title: "Scope and our role",
				Paragraphs: []string{
					"This Privacy Policy explains how ReReply, provided by Medtech Healthcare Sdn Bhd, handles personal data through the ReReply website, applications, APIs and support services.",
					"A clinic, pharmacy, wellness provider or other business using ReReply (a Customer Organisation) normally decides why and how its contact, patient, lead and conversation data is used. The Customer Organisation is the data controller and ReReply acts as its service provider or data processor. ReReply may act as controller for account administration, billing, platform security, service analytics and its own business communications.",
				},
			},
			{
				ID:    "data-and-purposes",
				Title: "Personal data we handle and why",
				Paragraphs: []string{
					"ReReply uses this information to provide, secure and troubleshoot the service; route authorised messages; maintain conversation and CRM history; enforce organisation permissions and tenant isolation; administer subscriptions and support; prevent abuse; and comply with lawful obligations.",
				},
				Bullets: []string{
					"Account identity, work contact details, roles, authentication records and organisation membership.",
					"Customer and CRM details such as names, telephone numbers, profile information, tags, notes, lead stages, appointments and service preferences.",
					"WhatsApp, Facebook Messenger, Instagram and email conversations, attachments, templates, delivery events and authorised agent notes.",
					"Connected-channel identifiers and settings, including Meta sender and message identifiers, Facebook Page and Instagram account identifiers, timestamps, technical and security logs, subscription and support records, and information submitted to optional automation or AI-assisted features.",
				},
			},
			{
				ID:    "connected-services",
				Title: "Connected services and AI",
				Paragraphs: []string{
					"When a Customer Organisation connects WhatsApp, Meta, Gmail, Google Search Console or another service, ReReply exchanges only the information needed for the authorised feature. Providers also process information under their own terms and privacy policies. Disconnecting ReReply does not automatically erase information independently held by a provider.",
					"Search Console access is read-only. Gmail access is limited to reading new inbox conversations and sending a reply approved by an authorised user; it is not used to modify or delete Gmail content. Optional AI features may send relevant prompts or excerpts to the configured provider solely to perform the requested feature. ReReply does not sell personal data or use Google user data for advertising or to train general-purpose AI models.",
				},
			},
			{
				ID:    "sharing-and-security",
				Title: "Sharing and security",
				Paragraphs: []string{
					"Medtech Healthcare Sdn Bhd uses DigitalOcean as a cloud infrastructure service provider and data processor for ReReply hosting, storage, security, backups and service operation. The current ReReply staging deployment uses DigitalOcean's Singapore (SGP) region. DigitalOcean is not necessarily the only service provider, and this does not mean that every production system or connected provider processes data only in Singapore.",
					"Information may be shared with authorised users and service providers of the relevant Customer Organisation, infrastructure and communications providers supporting ReReply, connected-channel providers when enabled, and advisers or authorities where reasonably necessary or legally required. One Customer Organisation's CRM or conversation data is not shared with another without an authorised supported workflow.",
					"Safeguards include tenant-aware access controls, encrypted network transport, protected credentials, audit logging, restricted production access and service monitoring. No internet service can guarantee absolute security, and users must protect their credentials and report suspected unauthorised access.",
				},
			},
			{
				ID:    "retention-and-rights",
				Title: "Retention, deletion and your rights",
				Paragraphs: []string{
					"Information is retained only as needed to provide and secure the service, follow lawful Customer Organisation instructions, and meet legal or contractual obligations. Data removed from active systems may remain temporarily in protected backups until normal rotation completes; limited security, billing, dispute and compliance records may be retained where necessary.",
					"Depending on applicable law, you may request access, correction, deletion, restriction, portability or objection, or withdraw consent. Contact the Customer Organisation first when it controls the record. You may also follow ReReply's Data Deletion Instructions. Identity and authority may need to be verified before action is taken.",
					"Privacy questions may be sent to Medtech Healthcare Sdn Bhd at medtechcorps@gmail.com.",
				},
			},
		},
	},
	"/terms": {
		Key:       "terms",
		Route:     "/terms",
		Eyebrow:   "ReReply service agreement",
		Title:     "Terms of Service",
		Effective: "Effective 28 July 2026",
		Summary:   "The rules that keep ReReply secure, responsible and dependable for every organisation using the platform.",
		Sections: []legalFallbackSection{
			{
				ID:    "agreement",
				Title: "Agreement and the service",
				Paragraphs: []string{
					"These Terms govern access to ReReply, a multi-tenant communication, CRM and automation platform provided by Medtech Healthcare Sdn Bhd. By creating an account, accepting an order or using ReReply, you accept these Terms for yourself and, where applicable, your organisation.",
					"Available communications, CRM, workflow, campaign, booking, reporting and optional AI-assisted features depend on the subscription, organisation configuration, region and third-party provider eligibility. A signed order, reseller agreement or data-processing agreement controls where it expressly conflicts with these Terms.",
				},
			},
			{
				ID:    "accounts-and-data",
				Title: "Accounts, administrators and customer data",
				Paragraphs: []string{
					"Customers retain ownership of data they or their authorised users submit. They grant ReReply the limited rights needed to host, transmit, back up and otherwise process that data to provide and secure the service and follow lawful instructions.",
				},
				Bullets: []string{
					"Provide accurate account and organisation information and keep credentials, tokens and verification codes confidential.",
					"Give each user only the access they need, remove former users promptly, and remain responsible for activity through the organisation's accounts and integrations.",
					"Provide required privacy notices, obtain necessary consent, respond to data-subject requests, and ensure customer data and instructions comply with law and professional duties.",
				},
			},
			{
				ID:    "messaging",
				Title: "Messaging and channel rules",
				Paragraphs: []string{
					"Use of WhatsApp, Meta products, Gmail and other connected services is also governed by their terms, policies, opt-in requirements, template rules, messaging limits and fees. Customers must have permission to contact recipients and must not send spam, deceptive messages or prohibited content.",
					"ReReply may restrict or suspend a feature where continued use creates a security, legal, abuse or provider-policy risk. Third-party availability, approval, pricing and delivery decisions remain outside ReReply's control.",
				},
			},
			{
				ID:    "acceptable-use",
				Title: "Acceptable use and AI",
				Bullets: []string{
					"Do not violate law, third-party rights, privacy rights or communications regulations.",
					"Do not probe, disrupt or bypass security or tenant boundaries; introduce malicious code; scrape the service; or use another organisation's data without authority.",
					"Review AI-assisted output before relying on it. ReReply is not an emergency, diagnostic, legal or financial decision service, and AI output may be incomplete or inaccurate.",
				},
			},
			{
				ID:    "commercial-and-ending",
				Title: "Fees, availability and ending service",
				Paragraphs: []string{
					"Subscription terms, allowances, prices and billing dates are stated in the applicable plan or order. Provider, telecommunications and AI charges may be separate. Unless an order states otherwise, fees are non-refundable except where required by law.",
					"Either party may end service as allowed by the applicable order or for a material uncured breach. ReReply may suspend access for non-payment, security threats, abuse or legal necessity. After termination, authorised export or deletion requests remain subject to the agreement, retention requirements and technical backup cycles.",
				},
			},
			{
				ID:    "liability-and-law",
				Title: "Disclaimers, liability and governing law",
				Paragraphs: []string{
					"The service is provided with reasonable care but without a guarantee of uninterrupted provider access, message delivery, AI output or business results. To the maximum extent permitted by law, neither party is liable for indirect or consequential loss, and ReReply's aggregate liability is limited as stated in the full Terms and any signed order.",
					"These Terms are governed by the laws of Malaysia, subject to any different binding provision in a signed agreement or applicable law. Questions may be sent to medtechcorps@gmail.com.",
				},
			},
		},
	},
	"/data-deletion": {
		Key:       "data-deletion",
		Route:     "/data-deletion",
		Eyebrow:   "ReReply privacy request",
		Title:     "Data Deletion Instructions",
		Effective: "Effective 3 August 2026",
		Summary:   "A clear process for requesting deletion of eligible ReReply account, CRM and conversation data.",
		Sections: []legalFallbackSection{
			{
				ID:    "before",
				Title: "Before you submit a request",
				Paragraphs: []string{
					"ReReply stores information for separate clinics, pharmacies, wellness providers and other Customer Organisations. If your request concerns a conversation, appointment or CRM record, contact the organisation you interacted with first because it normally controls and can identify that record.",
					"You may also request deletion directly from Medtech Healthcare Sdn Bhd, the provider of ReReply. ReReply will route a verified request to the appropriate Customer Organisation or act on its lawful instructions. Never send a password, WhatsApp verification code, two-step verification PIN or full medical history.",
				},
			},
			{
				ID:    "request",
				Title: "How to request deletion",
				Paragraphs: []string{
					"Email medtechcorps@gmail.com with the subject 'ReReply Data Deletion Request'. ReReply aims to acknowledge requests within seven calendar days and will confirm completion, explain lawful retention, or identify the Customer Organisation that must decide the request.",
				},
				Bullets: []string{
					"Include your name, the email address or telephone number used with the service, and the clinic, pharmacy, wellness provider or other organisation involved.",
					"State whether you want an account, conversation, contact record, attachment or all eligible ReReply-held data deleted, without adding unnecessary clinical details.",
					"Complete limited identity, authority and relationship checks if requested. ReReply will not ask for your password or SMS verification code.",
				},
			},
			{
				ID:    "scope",
				Title: "What deletion covers",
				Bullets: []string{
					"Eligible ReReply user profile and authentication data.",
					"Eligible contact, CRM, appointment, note, attachment and conversation records controlled by the relevant Customer Organisation.",
					"Optional automation or AI inputs stored in ReReply-controlled systems, where applicable.",
					"Connected-channel credentials controlled by ReReply when an authorised organisation closes an integration, including an encrypted Google OAuth credential for a disconnected Gmail or Search Console connection.",
				},
			},
			{
				ID:    "retained",
				Title: "Information that may be retained",
				Paragraphs: []string{
					"ReReply or the relevant Customer Organisation may retain limited information needed for legal or professional recordkeeping, fraud and security investigations, financial records, dispute resolution, opt-out enforcement or proof that a deletion request was completed.",
					"Data removed from active systems may remain temporarily in encrypted or access-restricted backups until normal rotation completes. Backup data is not returned to active use except for disaster recovery and remains subject to the original deletion controls.",
				},
			},
			{
				ID:    "third-parties",
				Title: "Google, WhatsApp, Meta and other third parties",
				Paragraphs: []string{
					"A ReReply deletion request removes eligible data controlled by ReReply or the relevant Customer Organisation. It does not delete a Google, Gmail, WhatsApp or Facebook account or information independently held by those providers. Use each provider's privacy controls for information it controls.",
					"For questions or an unresolved request, reply to the confirmation email, contact the relevant Customer Organisation, or email medtechcorps@gmail.com.",
				},
			},
		},
	},
}

var legalFallbackHTMLTemplate = template.Must(template.New("legal-fallback").Parse(`
<main class="app-fallback app-fallback--legal" aria-labelledby="legal-fallback-title" data-rereply-raw-document="{{.Key}}">
  <article class="app-fallback__content">
    <p class="app-fallback__brand">{{.Eyebrow}}</p>
    <h1 id="legal-fallback-title">{{.Title}}</h1>
    <p class="app-fallback__meta">{{.Effective}}</p>
    <p class="app-fallback__purpose">{{.Summary}}</p>
    <nav aria-label="Document sections">
      {{range .Sections}}<a href="{{.Href}}">{{.Title}}</a>{{end}}
    </nav>
    <div class="app-fallback__legal-body">
      {{range .Sections}}
      <section id="{{.ID}}">
        <h2>{{.Title}}</h2>
        {{range .Paragraphs}}<p>{{.}}</p>{{end}}
        {{if .Bullets}}<ul>{{range .Bullets}}<li>{{.}}</li>{{end}}</ul>{{end}}
      </section>
      {{end}}
    </div>
    <nav class="app-fallback__legal-footer" aria-label="Legal and contact links">
      <a href="mailto:medtechcorps@gmail.com">medtechcorps@gmail.com</a>
      <a href="{{.AboutHref}}">About ReReply</a>
      <a href="{{.PrivacyHref}}">Privacy Policy</a>
      <a href="{{.TermsHref}}">Terms of Service</a>
      <a href="{{.DeletionHref}}">Data deletion</a>
    </nav>
  </article>
</main>`))

var pageTitleHTMLTemplate = template.Must(template.New("page-title").Parse(`<title>{{.}} · ReReply</title>`))

var pageDescriptionHTMLTemplate = template.Must(template.New("page-description").Parse(`<meta name="description" content="{{.}}">`))

func legalFallbackIndexVariants(indexHTML []byte, basePath string) (map[string][]byte, error) {
	variants := make(map[string][]byte, len(legalFallbackDocuments))
	for route, document := range legalFallbackDocuments {
		fallback, err := renderLegalFallback(document, basePath)
		if err != nil {
			return nil, err
		}

		variant, err := replaceMarkedHTML(indexHTML, appFallbackStartMarker, appFallbackEndMarker, fallback)
		if err != nil {
			return nil, err
		}

		var title bytes.Buffer
		if err := pageTitleHTMLTemplate.Execute(&title, document.Title); err != nil {
			return nil, fmt.Errorf("render legal fallback title for %s: %w", route, err)
		}
		variant, err = replaceMarkedHTML(variant, titleStartMarker, titleEndMarker, title.Bytes())
		if err != nil {
			return nil, err
		}

		var description bytes.Buffer
		if err := pageDescriptionHTMLTemplate.Execute(&description, document.Summary); err != nil {
			return nil, fmt.Errorf("render legal fallback description for %s: %w", route, err)
		}
		variant, err = replaceMarkedHTML(variant, descriptionStartMarker, descriptionEndMarker, description.Bytes())
		if err != nil {
			return nil, err
		}

		variants[route] = variant
	}
	return variants, nil
}

func renderLegalFallback(document legalFallbackDocument, basePath string) ([]byte, error) {
	rendered := renderedLegalFallbackDocument{
		legalFallbackDocument: document,
		Sections:              make([]renderedLegalFallbackSection, 0, len(document.Sections)),
		AboutHref:             publicRouteHref(basePath, "/about"),
		PrivacyHref:           publicRouteHref(basePath, "/privacy"),
		TermsHref:             publicRouteHref(basePath, "/terms"),
		DeletionHref:          publicRouteHref(basePath, "/data-deletion"),
	}
	sectionBase := publicRouteHref(basePath, document.Route)
	for _, section := range document.Sections {
		rendered.Sections = append(rendered.Sections, renderedLegalFallbackSection{
			legalFallbackSection: section,
			Href:                 sectionBase + "#" + section.ID,
		})
	}

	var output bytes.Buffer
	if err := legalFallbackHTMLTemplate.Execute(&output, rendered); err != nil {
		return nil, fmt.Errorf("render legal fallback for %s: %w", document.Route, err)
	}
	return bytes.TrimSpace(output.Bytes()), nil
}

func replaceMarkedHTML(source []byte, startMarker, endMarker string, replacement []byte) ([]byte, error) {
	start := bytes.Index(source, []byte(startMarker))
	if start < 0 {
		return nil, fmt.Errorf("frontend index is missing marker %s", startMarker)
	}
	contentStart := start + len(startMarker)
	relativeEnd := bytes.Index(source[contentStart:], []byte(endMarker))
	if relativeEnd < 0 {
		return nil, fmt.Errorf("frontend index is missing marker %s", endMarker)
	}
	end := contentStart + relativeEnd

	result := make([]byte, 0, len(source)-(end-contentStart)+len(replacement)+2)
	result = append(result, source[:contentStart]...)
	result = append(result, '\n')
	result = append(result, replacement...)
	result = append(result, '\n')
	result = append(result, source[end:]...)
	return result, nil
}

func publicRouteHref(basePath, route string) string {
	base := normalizedPublicBasePath(basePath)
	route = "/" + strings.Trim(route, "/")
	if route == "/" {
		return base + "/"
	}
	return base + route
}

func legalFallbackRoute(requestPath, basePath string) string {
	route := pathpkg.Clean("/" + strings.TrimPrefix(requestPath, "/"))
	base := normalizedPublicBasePath(basePath)
	if base != "" {
		switch {
		case route == base:
			route = "/"
		case strings.HasPrefix(route, base+"/"):
			route = strings.TrimPrefix(route, base)
		}
	}
	if route != "/" {
		route = strings.TrimSuffix(route, "/")
	}
	if _, ok := legalFallbackDocuments[route]; !ok {
		return ""
	}
	return route
}

func normalizedPublicBasePath(basePath string) string {
	trimmed := strings.Trim(strings.TrimSpace(basePath), "/")
	if trimmed == "" {
		return ""
	}
	return "/" + trimmed
}
