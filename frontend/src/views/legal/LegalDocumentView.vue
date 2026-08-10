<script setup lang="ts">
import { computed, onBeforeUnmount, watchEffect, type Component } from 'vue'
import { RouterLink } from 'vue-router'
import {
  ArrowLeft,
  CheckCircle2,
  ChevronRight,
  FileText,
  Mail,
  ShieldCheck,
  Trash2
} from 'lucide-vue-next'
import ReReplyLogo from '@/components/brand/ReReplyLogo.vue'

type DocumentKey = 'privacy' | 'terms' | 'data-deletion'

interface LegalSection {
  id: string
  title: string
  paragraphs?: string[]
  bullets?: string[]
  steps?: Array<{
    title: string
    description: string
  }>
  callout?: string
}

interface LegalDocument {
  eyebrow: string
  title: string
  summary: string
  updated: string
  readingTime: string
  icon: Component
  sections: LegalSection[]
}

const props = defineProps<{
  documentKey: DocumentKey
}>()

const privacySections: LegalSection[] = [
  {
    id: 'scope',
    title: '1. Scope and our role',
    paragraphs: [
      'This Privacy Policy explains how ReReply, provided by Medtech Healthcare Sdn Bhd (“ReReply”, “we”, “us” or “our”), handles personal data through the ReReply website, applications, APIs and related support services.',
      'ReReply is a multi-tenant customer communication and CRM platform. The business or organisation that subscribes to ReReply (“Customer Organisation”) normally decides why and how its contact, patient, lead and conversation data is used. For that data, the Customer Organisation is the data controller and ReReply acts as its service provider or data processor. We may act as controller for account administration, billing, platform security, service analytics and our own business communications.'
    ]
  },
  {
    id: 'data',
    title: '2. Personal data we handle',
    bullets: [
      'Account data, such as names, work email addresses, roles, authentication records and organisation membership.',
      'Customer and contact data, such as names, telephone numbers, profile details, tags, notes, lead stages, appointments and service preferences.',
      'Conversation data, including WhatsApp and email messages, attachments, templates, delivery events and agent notes.',
      'Business configuration, including WhatsApp Business Account identifiers, phone-number identifiers, connected Google mailbox metadata, webhook settings and other connected-channel metadata.',
      'Technical and security data, such as IP addresses, device and browser information, access logs, audit events and error diagnostics.',
      'Commercial and support data, including subscription records, invoices, service requests and correspondence with us.',
      'Information submitted to optional automation or AI-assisted features when a Customer Organisation enables them.'
    ],
    callout: 'Please do not send passwords, one-time verification codes or unnecessary sensitive information through ReReply support channels.'
  },
  {
    id: 'purposes',
    title: '3. How we use personal data',
    bullets: [
      'Provide, operate, secure and troubleshoot the ReReply service.',
      'Route messages, maintain conversation history and support CRM, booking, campaign and workflow functions.',
      'Authenticate users, enforce organisation permissions and preserve tenant isolation.',
      'Process authorised integrations and instructions from Customer Organisations.',
      'Monitor reliability, prevent abuse and maintain audit evidence.',
      'Provide customer support, administer subscriptions and communicate service changes.',
      'Comply with applicable law, lawful requests and contractual obligations.'
    ],
    paragraphs: [
      'Our legal basis depends on the context and may include performance of a contract, legitimate interests in operating and securing the service, compliance with law, or consent where required. Customer Organisations are responsible for establishing an appropriate lawful basis for the data they upload or collect through ReReply.'
    ]
  },
  {
    id: 'channels',
    title: '4. WhatsApp and connected services',
    paragraphs: [
      'When a Customer Organisation connects WhatsApp Business Platform or another channel, ReReply exchanges data with the relevant provider to deliver messages and related functions. Meta and WhatsApp process information under their own terms and privacy policies. A Customer Organisation must have the necessary permissions and provide any required notices before contacting people through a connected channel.',
      'ReReply does not control how Meta, WhatsApp or another independently operated service uses data outside ReReply. Disconnecting a channel from ReReply does not automatically delete information held by that provider.'
    ]
  },
  {
    id: 'google-search-console',
    title: '5. Google Search Console and Google API data',
    paragraphs: [
      'When an authorised Customer Organisation user connects Google Search Console, ReReply uses the Google account selected by that user to discover website properties the account can access. The user then chooses which verified properties ReReply may display. ReReply requests only the read-only Google Search Console permission and cannot edit a website, change Search Console settings or publish content.',
      'For selected properties, ReReply may access property identifiers and permission levels together with search-performance data such as clicks, impressions, click-through rate (CTR), average position, search queries and pages. ReReply uses this data only to provide search-visibility reporting inside the requesting Customer Organisation’s workspace.',
      'OAuth refresh tokens are encrypted at rest. Connected-property metadata and integration configuration are kept in protected, tenant-isolated systems, and data is encrypted in transit. Search-performance reports are requested from Google when an authorised user opens the report; ReReply does not use Google Search Console data for advertising, profiling unrelated to the requested feature or training general-purpose AI models.',
      'ReReply retains the encrypted OAuth credential and selected-property configuration while the integration remains connected or as needed to provide and secure the service. An authorised workspace user may disconnect Google Search Console in ReReply, which deletes ReReply’s stored OAuth credential and disables the selected properties. The user may also revoke ReReply through their Google Account permissions. Limited audit and backup records may remain for the periods described in the Retention and deletion section below.'
    ],
    bullets: [
      'We do not sell Google user data.',
      'We do not share Google user data with another Customer Organisation or with third parties except service providers acting for ReReply under appropriate confidentiality and security obligations, or where disclosure is legally required.',
      'Our use and transfer of information received from Google APIs complies with the Google API Services User Data Policy, including the Limited Use requirements.'
    ],
    callout: 'You control the connection: choose verified properties, disconnect in ReReply, or revoke access in your Google Account.'
  },
  {
    id: 'google-gmail',
    title: '6. Gmail and Google API data',
    paragraphs: [
      'With permission from the Customer Organisation and Google account holder, a trusted ReReply operator connects one specifically configured Gmail or Google Workspace mailbox for that relay deployment. ReReply requests Gmail read-only and send permissions. The read-only permission is used to identify new inbox messages and display the conversation in ReReply’s unified workspace. ReReply may process the Gmail message and thread identifiers, sender and recipient addresses, subject, date and plain-text message content. Sent, draft, spam and trash messages are excluded, message content is limited to 10,000 characters, and attachment files are not downloaded or exposed.',
      'The Gmail send permission is used only when an authorised ReReply user writes and approves a reply. ReReply validates the recipient against the existing Gmail thread and sends the text reply into that thread. ReReply does not use these permissions to modify or delete Gmail content, create bulk campaigns, download attachments or send a standalone message to an unrelated recipient.',
      'OAuth refresh tokens are encrypted at rest, access tokens are short-lived and are not persisted, and data is encrypted in transit. Production relay and CRM storage use managed PostgreSQL and Valkey services with infrastructure encryption at rest, TLS-protected connections and network access restricted to the ReReply application. Gmail message content is transferred through a dedicated relay into the requesting Customer Organisation’s tenant-isolated ReReply workspace. Operational logs omit message bodies, OAuth tokens and provider response bodies. A queued relay body is removed after successful delivery or dead-lettering; delivery and idempotency markers are retained for seven days by default. Conversation content retained in ReReply remains workspace conversation history until the Customer Organisation instructs ReReply to delete it or another lawful retention requirement applies, as described in the Retention and deletion section below.',
      'ReReply retains the encrypted Gmail OAuth credential only while the mailbox remains connected or as needed to provide and secure the service. An authorised organisation administrator may ask ReReply to disconnect the mailbox and delete the stored credential. The Google account holder may also revoke ReReply through Google Account permissions. Disconnecting stops future Gmail processing but does not automatically delete conversation records already retained under the Customer Organisation’s lawful instructions.'
    ],
    bullets: [
      'We use Gmail data only to provide and secure the user-visible email conversation and reply features requested by the Customer Organisation.',
      'We do not sell Gmail or other Google user data, use it for advertising or unrelated profiling, make credit decisions with it, or use it to train general-purpose AI models.',
      'Gmail message content is not sent to ReReply’s optional automatic AI reply features.',
      'We do not share Gmail data with another Customer Organisation or with third parties except service providers acting for ReReply under appropriate confidentiality and security obligations, or where disclosure is legally required.',
      'Our use and transfer of information received from Google APIs complies with the Google API Services User Data Policy, including the Limited Use requirements.'
    ],
    callout: 'Gmail access is limited to one explicitly authorised mailbox: read new inbox conversations, send approved replies, disconnect or revoke whenever needed.'
  },
  {
    id: 'ai',
    title: '7. AI-assisted features',
    paragraphs: [
      'Customer Organisations may enable AI-assisted drafting, classification, chatbot or workflow features. When enabled, relevant prompts, message excerpts or configured knowledge may be sent to the selected AI provider, including Qwen through Alibaba Cloud DashScope where configured, solely to perform the requested feature and subject to the applicable provider terms and deployment configuration.',
      'AI output may be incomplete or inaccurate and must be reviewed by an authorised person before it is relied upon for patient care, clinical decisions, legal advice, financial decisions or other high-impact uses.'
    ]
  },
  {
    id: 'health',
    title: '8. Health and sensitive information',
    paragraphs: [
      'Some Customer Organisations may use ReReply in healthcare, wellness or pharmacy settings. They determine whether sensitive or health-related information may be collected and are responsible for obtaining appropriate consent, limiting access and complying with professional and legal duties.',
      'ReReply is a communication and workflow platform. It is not an emergency service, diagnostic tool or substitute for advice from a qualified healthcare professional.'
    ]
  },
  {
    id: 'sharing',
    title: '9. When information is shared',
    bullets: [
      'With authorised users and service providers of the relevant Customer Organisation.',
      'With infrastructure, communications, storage, security, analytics and AI providers that support the service under appropriate obligations.',
      'With Google, Meta, WhatsApp and other connected-channel providers when a Customer Organisation enables an integration.',
      'With professional advisers, regulators or authorities where reasonably necessary or legally required.',
      'In connection with a legitimate corporate transaction, subject to appropriate confidentiality and data-protection safeguards.'
    ],
    paragraphs: [
      'Medtech Healthcare Sdn Bhd uses DigitalOcean as a cloud infrastructure service provider and data processor for ReReply hosting, storage, security, backups and service operation. The current ReReply staging deployment uses DigitalOcean\'s Singapore (SGP) region. DigitalOcean is not necessarily the only service provider, and this does not mean that every production system or connected provider processes data only in Singapore.',
      'We do not sell personal data. We do not share one Customer Organisation’s CRM or conversation data with another Customer Organisation unless the relevant organisation expressly authorises a supported transfer or shared workflow.'
    ]
  },
  {
    id: 'retention',
    title: '10. Retention and deletion',
    paragraphs: [
      'We retain information for as long as needed to provide the service, fulfil Customer Organisation instructions, protect the platform and meet legal or contractual obligations. Retention periods vary by data category, organisation configuration and applicable law.',
      'When data is deleted from active systems, limited copies may remain temporarily in protected backups until they age out under the applicable backup-rotation schedule. We may retain minimal records necessary for security, fraud prevention, legal compliance, billing disputes and proof that a privacy request was completed.'
    ]
  },
  {
    id: 'transfers',
    title: '11. International processing',
    paragraphs: [
      'ReReply and its service providers may process information in Malaysia or other countries where our infrastructure and connected providers operate. Where required, we use contractual, organisational or technical safeguards for cross-border transfers. Customer Organisations remain responsible for assessing transfers created by the integrations they choose to enable.'
    ]
  },
  {
    id: 'security',
    title: '12. Security',
    paragraphs: [
      'We use administrative, technical and organisational safeguards designed to protect information, including access controls, tenant-aware authorisation, encrypted network transport, audit logging, restricted production access and service monitoring. No internet service can guarantee absolute security.',
      'Users must protect their credentials, use strong authentication and promptly notify us of suspected unauthorised access.'
    ]
  },
  {
    id: 'rights',
    title: '13. Your choices and rights',
    paragraphs: [
      'Depending on applicable law, you may have rights to request access, correction, deletion, restriction, portability or objection, or to withdraw consent. If your information was collected by a Customer Organisation using ReReply, contact that organisation first because it controls the relevant record. We will support valid instructions from the organisation.',
      'You may also follow our Data Deletion Instructions. We may need to verify your identity and relationship to the relevant organisation before acting.'
    ]
  },
  {
    id: 'children',
    title: '14. Children',
    paragraphs: [
      'ReReply is intended for business users and is not directed to children. A Customer Organisation that handles a child’s data is responsible for obtaining any required parent or guardian authorisation and applying appropriate safeguards.'
    ]
  },
  {
    id: 'updates',
    title: '15. Updates and contact',
    paragraphs: [
      'We may update this policy when the service, law or our processing practices change. We will post the revised version here and update the effective date.',
      'For privacy questions, contact Medtech Healthcare Sdn Bhd at medtechcorps@gmail.com. For records controlled by a clinic, pharmacy, wellness provider or other Customer Organisation, you should also contact that organisation directly.'
    ]
  }
]

const termsSections: LegalSection[] = [
  {
    id: 'agreement',
    title: '1. Agreement and eligibility',
    paragraphs: [
      'These Terms of Service (“Terms”) govern access to ReReply, a multi-tenant communication, CRM and automation platform provided by Medtech Healthcare Sdn Bhd. By creating an account, accepting an order or using ReReply, you agree to these Terms on behalf of yourself and, where applicable, your organisation.',
      'You must be legally able to enter into this agreement and authorised to bind the organisation you represent. If a separate signed order, reseller agreement or data-processing agreement conflicts with these Terms, the signed agreement controls for that conflict.'
    ]
  },
  {
    id: 'service',
    title: '2. The service',
    paragraphs: [
      'ReReply provides tools for customer communications, WhatsApp and other channel integrations, CRM records, workflows, campaigns, bookings, reporting and optional AI-assisted functions. Available features may depend on your subscription, organisation configuration, region and third-party provider eligibility.',
      'Each Customer Organisation has a logically separate workspace. Users may access only the organisations and functions for which they are authorised.'
    ]
  },
  {
    id: 'accounts',
    title: '3. Accounts and administrators',
    bullets: [
      'Provide accurate registration and organisation information.',
      'Keep credentials, access tokens and verification codes confidential.',
      'Assign the minimum access necessary to each user and promptly remove former users.',
      'Remain responsible for activity performed through your organisation’s accounts and integrations.',
      'Notify us promptly if you suspect an account or connected channel has been compromised.'
    ]
  },
  {
    id: 'customer-data',
    title: '4. Customer data and instructions',
    paragraphs: [
      'You retain ownership of data you or your users submit to ReReply (“Customer Data”). You grant us the limited rights needed to host, process, transmit, back up and otherwise handle Customer Data to provide, secure and support the service.',
      'You are responsible for the accuracy, legality and quality of Customer Data; providing required privacy notices; obtaining necessary consent; responding to data-subject requests; and ensuring your instructions comply with law and professional obligations.'
    ]
  },
  {
    id: 'messaging',
    title: '5. Messaging and channel rules',
    paragraphs: [
      'Your use of WhatsApp, Meta products and other connected services is also governed by their terms, policies, template rules, opt-in requirements, messaging limits and fees. You must not send spam, deceptive messages or content prohibited by a channel provider.',
      'Third-party providers may suspend a phone number, reject a display name or template, change an API or limit delivery. We do not control those decisions, but we may assist with reasonable troubleshooting.'
    ]
  },
  {
    id: 'healthcare',
    title: '6. Healthcare and high-impact use',
    paragraphs: [
      'ReReply is a general business communication and workflow platform. It is not a medical device, emergency service, clinical record system or substitute for professional judgement. Do not use ReReply as the only method for urgent or emergency communication.',
      'Healthcare, pharmacy and wellness organisations are responsible for clinical governance, professional confidentiality, patient consent, recordkeeping rules and determining whether ReReply is appropriate for a particular workflow.'
    ]
  },
  {
    id: 'ai',
    title: '7. AI-assisted features',
    paragraphs: [
      'AI features may generate drafts, classifications or automated responses. Output can be inaccurate, incomplete or unsuitable. You are responsible for configuring safeguards, supervising automated workflows and reviewing output before it is used for patient care or another high-impact decision.',
      'You must not use AI features to make an unlawful decision about a person or to submit data that you are not authorised to process.'
    ]
  },
  {
    id: 'acceptable-use',
    title: '8. Acceptable use',
    bullets: [
      'Do not violate law, third-party rights, privacy rights or communications regulations.',
      'Do not upload malware, probe security controls, bypass tenant isolation or interfere with the service.',
      'Do not use ReReply for harassment, fraud, discrimination, illegal goods, exploitation or harmful content.',
      'Do not reverse engineer or copy the service except where applicable law expressly permits it.',
      'Do not resell, sublicense or provide access outside an approved reseller or subscription arrangement.'
    ]
  },
  {
    id: 'fees',
    title: '9. Subscriptions, fees and taxes',
    paragraphs: [
      'Subscription terms, usage allowances, prices and billing dates are stated in the applicable order or plan. Fees from Meta, WhatsApp, telecommunications carriers, AI providers or other third parties may be charged separately or passed through as described in your order.',
      'Unless an order says otherwise, fees are non-refundable except where required by law. You are responsible for applicable taxes, excluding taxes based on our net income.'
    ]
  },
  {
    id: 'ownership',
    title: '10. Intellectual property',
    paragraphs: [
      'We and our licensors retain all rights in ReReply, including the software, design, documentation, trademarks and service improvements. These Terms grant you a limited, non-exclusive, non-transferable right to use the service during your subscription.',
      'If you provide feedback, you allow us to use it without restriction or payment, provided we do not identify you publicly without permission.'
    ]
  },
  {
    id: 'confidentiality',
    title: '11. Confidentiality and data protection',
    paragraphs: [
      'Each party must protect the other party’s non-public confidential information using reasonable care and use it only for the relationship. This obligation does not apply to information that is public without breach, independently developed, lawfully received from another source or required to be disclosed by law.',
      'Our handling of personal data is described in the ReReply Privacy Policy and any applicable data-processing agreement.'
    ]
  },
  {
    id: 'availability',
    title: '12. Availability and changes',
    paragraphs: [
      'We aim to operate a reliable service but do not promise uninterrupted or error-free availability. Maintenance, security events, internet conditions and third-party services may affect access. We may change features to improve security, comply with law or respond to provider changes.',
      'We will use reasonable efforts to give notice of a material reduction to paid core functionality where practicable.'
    ]
  },
  {
    id: 'termination',
    title: '13. Suspension and termination',
    paragraphs: [
      'We may suspend access when reasonably necessary to address non-payment, a security threat, unlawful use, provider restrictions or a material breach. Where practicable, we will give notice and an opportunity to remedy the issue.',
      'On termination, your right to use the service ends. Data export and deletion are handled according to the applicable order, Privacy Policy and legal retention requirements. Provisions that by their nature should survive termination will remain effective.'
    ]
  },
  {
    id: 'disclaimers',
    title: '14. Disclaimers',
    paragraphs: [
      'To the extent permitted by law, ReReply is provided on an “as is” and “as available” basis. We disclaim implied warranties of merchantability, fitness for a particular purpose and non-infringement. We do not warrant third-party services, message delivery, AI output or results from using the service.',
      'Nothing in these Terms excludes rights or warranties that cannot lawfully be excluded.'
    ]
  },
  {
    id: 'liability',
    title: '15. Limitation of liability',
    paragraphs: [
      'To the maximum extent permitted by law, neither party will be liable for indirect, incidental, special, exemplary or consequential loss, or loss of profits, revenue, goodwill or data, arising from these Terms. Our aggregate liability relating to the service will not exceed the fees paid or payable for the affected service during the twelve months before the event giving rise to the claim.',
      'These limitations do not apply where liability cannot be limited by law, or to fraud, wilful misconduct or breach of confidentiality to the extent a limitation is prohibited.'
    ]
  },
  {
    id: 'law',
    title: '16. Governing law',
    paragraphs: [
      'These Terms are governed by the laws of Malaysia, without regard to conflict-of-law principles. The courts of Malaysia will have jurisdiction, unless a signed agreement specifies a different dispute process or applicable law requires otherwise.'
    ]
  },
  {
    id: 'changes',
    title: '17. Changes and contact',
    paragraphs: [
      'We may update these Terms to reflect service, legal or operational changes. Material changes will apply prospectively and will be communicated through the service, email or this page as appropriate.',
      'Questions about these Terms may be sent to medtechcorps@gmail.com.'
    ]
  }
]

const deletionSections: LegalSection[] = [
  {
    id: 'before',
    title: 'Before you submit a request',
    paragraphs: [
      'ReReply stores information on behalf of separate clinics, pharmacies, wellness providers and other Customer Organisations. If your request concerns a conversation, appointment or CRM record, contact the organisation you interacted with first. It is normally the data controller and can identify the relevant record.',
      'You may also send a request directly to Medtech Healthcare Sdn Bhd, the provider of ReReply. We will route a verified request to the appropriate Customer Organisation or act on its lawful instructions.'
    ],
    callout: 'Never send us your password, WhatsApp verification code, two-step verification PIN or full medical history.'
  },
  {
    id: 'request',
    title: 'How to request deletion',
    steps: [
      {
        title: 'Email the privacy desk',
        description: 'Send an email to medtechcorps@gmail.com with the subject “ReReply Data Deletion Request”.'
      },
      {
        title: 'Identify the relevant account',
        description: 'Include your name, the email address or telephone number used with the service, and the clinic, pharmacy, wellness provider or other organisation involved.'
      },
      {
        title: 'Describe the scope',
        description: 'Tell us whether you want an account, conversation, contact record, attachment or all eligible ReReply-held data deleted. Do not include unnecessary clinical details.'
      },
      {
        title: 'Complete verification',
        description: 'We may ask for limited information to confirm your identity, authority and relationship to the relevant organisation. We will not ask for your password or SMS verification code.'
      },
      {
        title: 'Receive confirmation',
        description: 'We aim to acknowledge requests within seven calendar days. We will confirm completion, explain any lawful retention, or tell you if the Customer Organisation must decide the request.'
      }
    ]
  },
  {
    id: 'account',
    title: 'Organisation and user accounts',
    paragraphs: [
      'An organisation administrator may request deletion or closure of a ReReply user account through its authorised support contact. Individual users can request deletion using the email process above. Closing a user account does not automatically delete records that the Customer Organisation must retain, such as audit events or business correspondence.'
    ]
  },
  {
    id: 'scope',
    title: 'What deletion covers',
    bullets: [
      'Eligible ReReply user profile and authentication data.',
      'Eligible contact, CRM, appointment, note and conversation records controlled by the relevant Customer Organisation.',
      'Attachments and media stored by ReReply, where they are not subject to a valid retention requirement.',
      'Optional AI or automation inputs stored in ReReply-controlled systems, where applicable.',
      'Connected-channel credentials under ReReply’s control when an authorised organisation closes the integration, including an encrypted Google OAuth credential for a disconnected Gmail or Search Console connection.'
    ]
  },
  {
    id: 'retained',
    title: 'Information that may be retained',
    paragraphs: [
      'We or the relevant Customer Organisation may retain limited information where necessary for legal compliance, patient or professional recordkeeping, fraud and security investigations, financial records, dispute resolution, opt-out enforcement or proof that a deletion request was completed.',
      'Data removed from active systems may remain temporarily in encrypted or access-restricted backups until those backups age out under normal rotation. Backup data is not returned to active use except for disaster recovery and remains subject to the original deletion controls.'
    ]
  },
  {
    id: 'third-parties',
    title: 'Google, WhatsApp, Meta and other third parties',
    paragraphs: [
      'A ReReply deletion request removes eligible data controlled by ReReply or the relevant Customer Organisation. It does not delete your Google, Gmail, WhatsApp or Facebook account, or information independently held by Google, Meta, WhatsApp, a telecommunications provider or another connected service.',
      'To delete information held by a third party, use that provider’s account and privacy controls. Disconnecting a ReReply integration stops future processing but may not erase records that the provider is independently required or permitted to retain.'
    ]
  },
  {
    id: 'appeal',
    title: 'Questions or unresolved requests',
    paragraphs: [
      'If you believe a request was not handled correctly, reply to the confirmation email and explain the issue. You may also contact the relevant Customer Organisation or the data-protection authority available to you under applicable law.',
      'Privacy contact: medtechcorps@gmail.com'
    ]
  }
]

const documents: Record<DocumentKey, LegalDocument> = {
  privacy: {
    eyebrow: 'Trust centre',
    title: 'Privacy Policy',
    summary: 'How ReReply protects and processes account, CRM and conversation data across separate customer organisations.',
    updated: 'Effective 3 August 2026',
    readingTime: '15 minute read',
    icon: ShieldCheck,
    sections: privacySections
  },
  terms: {
    eyebrow: 'Service agreement',
    title: 'Terms of Service',
    summary: 'The rules that keep ReReply secure, responsible and dependable for every organisation using the platform.',
    updated: 'Effective 28 July 2026',
    readingTime: '11 minute read',
    icon: FileText,
    sections: termsSections
  },
  'data-deletion': {
    eyebrow: 'Privacy request',
    title: 'Data Deletion Instructions',
    summary: 'A clear process for requesting deletion of eligible ReReply account, CRM and conversation data.',
    updated: 'Effective 3 August 2026',
    readingTime: '5 minute read',
    icon: Trash2,
    sections: deletionSections
  }
}

const legalDocument = computed(() => documents[props.documentKey])
const pageTitle = computed(() => `${legalDocument.value.title} · ReReply`)

const originalTitle = document.title
let descriptionTag = document.querySelector<HTMLMetaElement>('meta[name="description"]')
const originalDescription = descriptionTag?.content ?? ''

watchEffect(() => {
  document.title = pageTitle.value

  if (!descriptionTag) {
    descriptionTag = document.createElement('meta')
    descriptionTag.name = 'description'
    document.head.appendChild(descriptionTag)
  }
  descriptionTag.content = legalDocument.value.summary
})

onBeforeUnmount(() => {
  document.title = originalTitle
  if (descriptionTag) {
    descriptionTag.content = originalDescription
  }
})
</script>

<template>
  <div class="legal-shell min-h-screen text-[#eeefe6]">
    <div class="legal-grid pointer-events-none fixed inset-0 opacity-70" aria-hidden="true" />
    <div class="legal-glow legal-glow-top pointer-events-none fixed -top-64 right-[-10rem] h-[42rem] w-[42rem] rounded-full" aria-hidden="true" />
    <div class="legal-glow legal-glow-bottom pointer-events-none fixed -bottom-72 left-[-14rem] h-[38rem] w-[38rem] rounded-full" aria-hidden="true" />

    <header class="sticky top-0 z-40 border-b border-white/[0.07] bg-[#0c0f0a]/85 backdrop-blur-2xl">
      <div class="mx-auto flex h-[72px] max-w-[1180px] items-center justify-between px-5 sm:px-8">
        <RouterLink to="/about" aria-label="ReReply home">
          <ReReplyLogo size="md" tone="light" tagline />
        </RouterLink>
        <RouterLink
          to="/about"
          class="group inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/[0.04] px-4 py-2 text-xs font-semibold uppercase tracking-[0.13em] text-[#dfe5b6] transition hover:border-[#cbd49a]/35 hover:bg-[#cbd49a]/[0.08]"
        >
          <ArrowLeft class="h-3.5 w-3.5 transition-transform group-hover:-translate-x-0.5" />
          About ReReply
        </RouterLink>
      </div>
    </header>

    <main class="relative z-10">
      <section class="mx-auto max-w-[1180px] px-5 pb-14 pt-16 sm:px-8 sm:pb-20 sm:pt-24">
        <div class="max-w-[830px]">
          <div class="mb-8 inline-flex items-center gap-2 rounded-full border border-[#cbd49a]/20 bg-[#cbd49a]/[0.06] px-3 py-1.5 text-[11px] font-bold uppercase tracking-[0.22em] text-[#cbd49a]">
            <component :is="legalDocument.icon" class="h-3.5 w-3.5" />
            {{ legalDocument.eyebrow }}
          </div>
          <h1 class="legal-title max-w-[780px] text-[clamp(3rem,8vw,6.7rem)] leading-[0.92] tracking-[-0.065em] text-[#f3f4eb]">
            {{ legalDocument.title }}
          </h1>
          <p class="mt-8 max-w-[720px] text-base leading-8 text-white/55 sm:text-lg">
            {{ legalDocument.summary }}
          </p>
          <div class="mt-8 flex flex-wrap items-center gap-x-6 gap-y-2 text-xs font-medium uppercase tracking-[0.14em] text-white/35">
            <span>{{ legalDocument.updated }}</span>
            <span class="h-1 w-1 rounded-full bg-[#cbd49a]/70" />
            <span>{{ legalDocument.readingTime }}</span>
          </div>
        </div>
      </section>

      <div class="border-y border-white/[0.07] bg-black/10">
        <div class="mx-auto grid max-w-[1180px] grid-cols-1 gap-12 px-5 py-12 sm:px-8 lg:grid-cols-[240px_minmax(0,760px)] lg:gap-20 lg:py-20">
          <aside class="lg:sticky lg:top-[104px] lg:h-fit">
            <p class="mb-4 text-[10px] font-bold uppercase tracking-[0.24em] text-[#cbd49a]/70">
              In this document
            </p>
            <nav aria-label="Document sections" class="grid gap-1 sm:grid-cols-2 lg:grid-cols-1">
              <a
                v-for="section in legalDocument.sections"
                :key="section.id"
                :href="`/${props.documentKey}#${section.id}`"
                class="group flex items-start gap-2.5 rounded-lg px-2 py-2 text-xs font-medium leading-5 text-white/38 transition hover:bg-white/[0.035] hover:text-white/75"
              >
                <ChevronRight class="mt-0.5 h-3.5 w-3.5 shrink-0 text-[#cbd49a]/35 transition group-hover:translate-x-0.5 group-hover:text-[#cbd49a]" />
                <span>{{ section.title }}</span>
              </a>
            </nav>
          </aside>

          <article class="min-w-0">
            <section
              v-for="section in legalDocument.sections"
              :id="section.id"
              :key="section.id"
              class="legal-section scroll-mt-28 border-b border-white/[0.07] py-10 first:pt-0 last:border-0 last:pb-0"
            >
              <h2 class="legal-heading text-[1.65rem] leading-tight tracking-[-0.035em] text-[#eff0e8] sm:text-[1.9rem]">
                {{ section.title }}
              </h2>

              <div v-if="section.paragraphs" class="mt-6 space-y-5">
                <p
                  v-for="paragraph in section.paragraphs"
                  :key="paragraph"
                  class="text-[15px] leading-7 text-white/58"
                >
                  {{ paragraph }}
                </p>
              </div>

              <ul v-if="section.bullets" class="mt-6 space-y-3">
                <li
                  v-for="bullet in section.bullets"
                  :key="bullet"
                  class="flex gap-3 text-[15px] leading-7 text-white/58"
                >
                  <CheckCircle2 class="mt-1.5 h-4 w-4 shrink-0 text-[#aeb77a]" />
                  <span>{{ bullet }}</span>
                </li>
              </ul>

              <ol v-if="section.steps" class="mt-8 space-y-4">
                <li
                  v-for="(step, index) in section.steps"
                  :key="step.title"
                  class="group grid grid-cols-[42px_1fr] gap-4 rounded-2xl border border-white/[0.075] bg-white/[0.025] p-4 transition hover:border-[#cbd49a]/20 hover:bg-[#cbd49a]/[0.035] sm:p-5"
                >
                  <span class="flex h-10 w-10 items-center justify-center rounded-xl border border-[#cbd49a]/20 bg-[#cbd49a]/[0.07] font-serif text-lg text-[#dfe5b6]">
                    {{ index + 1 }}
                  </span>
                  <span>
                    <strong class="block text-sm font-semibold text-[#f0f1e8]">{{ step.title }}</strong>
                    <span class="mt-1.5 block text-sm leading-6 text-white/50">{{ step.description }}</span>
                  </span>
                </li>
              </ol>

              <div
                v-if="section.callout"
                class="mt-7 rounded-2xl border border-[#cbd49a]/20 bg-[#cbd49a]/[0.055] px-5 py-4 text-sm leading-6 text-[#e2e6c7]/80"
              >
                {{ section.callout }}
              </div>
            </section>
          </article>
        </div>
      </div>

      <section class="mx-auto max-w-[1180px] px-5 py-16 sm:px-8 sm:py-20">
        <div class="relative overflow-hidden rounded-[28px] border border-[#cbd49a]/20 bg-[#cbd49a]/[0.055] p-7 sm:p-10">
          <div class="absolute -right-16 -top-20 h-56 w-56 rounded-full bg-[#cbd49a]/10 blur-3xl" aria-hidden="true" />
          <div class="relative flex flex-col gap-7 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <p class="text-[10px] font-bold uppercase tracking-[0.24em] text-[#cbd49a]/70">Need clarification?</p>
              <h2 class="legal-heading mt-3 text-3xl tracking-[-0.04em] text-[#f0f1e8]">Talk to the privacy desk.</h2>
              <p class="mt-3 max-w-xl text-sm leading-6 text-white/48">
                Include the organisation involved and enough information to locate your account. Never include passwords or verification codes.
              </p>
            </div>
            <a
              href="mailto:medtechcorps@gmail.com?subject=ReReply%20Privacy%20Enquiry"
              class="inline-flex shrink-0 items-center justify-center gap-2 rounded-full bg-[#d9dfa9] px-5 py-3 text-sm font-semibold text-[#262b18] transition hover:-translate-y-0.5 hover:bg-[#e8edc3]"
            >
              <Mail class="h-4 w-4" />
              medtechcorps@gmail.com
            </a>
          </div>
        </div>
      </section>
    </main>

    <footer class="relative z-10 border-t border-white/[0.07]">
      <div class="mx-auto flex max-w-[1180px] flex-col gap-6 px-5 py-9 text-xs text-white/35 sm:flex-row sm:items-center sm:justify-between sm:px-8">
        <div>
          <ReReplyLogo size="sm" tone="light" />
          <p class="mt-3">Conversations, aligned. Data, respected.</p>
        </div>
        <nav aria-label="Legal" class="flex flex-wrap gap-x-5 gap-y-2">
          <RouterLink to="/about" class="text-white/45 transition hover:text-[#cbd49a]">About</RouterLink>
          <RouterLink to="/privacy" class="text-white/45 transition hover:text-[#cbd49a]">Privacy</RouterLink>
          <RouterLink to="/terms" class="text-white/45 transition hover:text-[#cbd49a]">Terms</RouterLink>
          <RouterLink to="/data-deletion" class="text-white/45 transition hover:text-[#cbd49a]">Data deletion</RouterLink>
        </nav>
      </div>
    </footer>
  </div>
</template>

<style scoped>
.legal-shell {
  background:
    radial-gradient(circle at 18% 7%, rgba(92, 101, 55, 0.12), transparent 28rem),
    linear-gradient(180deg, #0b0e09 0%, #0d110b 42%, #090b08 100%);
}

.legal-grid {
  background-image:
    linear-gradient(rgba(203, 212, 154, 0.025) 1px, transparent 1px),
    linear-gradient(90deg, rgba(203, 212, 154, 0.025) 1px, transparent 1px);
  background-size: 56px 56px;
  mask-image: linear-gradient(to bottom, black 0%, transparent 75%);
}

.legal-glow {
  background: rgba(112, 122, 67, 0.17);
  filter: blur(110px);
}

.legal-glow-bottom {
  background: rgba(203, 212, 154, 0.07);
}

.legal-title,
.legal-heading {
  font-family: Georgia, 'Times New Roman', serif;
  font-weight: 500;
}

</style>
