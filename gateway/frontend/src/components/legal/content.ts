// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Locale } from '../../i18n';

// Legal pages are OPERATOR TEMPLATES, not finalized legal text. Real-world
// identity fields are placeholders in [brackets]; every page carries the
// `banner` warning. The operator must fill these in and have them reviewed by a
// lawyer before publishing. No maintainer personal data appears here.

export type LegalPage = 'impressum' | 'nutzungsbedingungen' | 'datenschutz';

export interface LegalSection {
  heading: string;
  body: string[];
}

export interface LegalDoc {
  title: string;
  banner: string;
  sections: LegalSection[];
}

const REPO = 'https://github.com/JLor08/op-ai-gateway';

const BANNER_DE =
  'Vorlage (Template) — vom Betreiber mit echten Angaben zu füllen und vor ' +
  'Veröffentlichung juristisch zu prüfen. Dies ist keine Rechtsberatung.';
const BANNER_EN =
  'Template — the operator must complete this with real details and have it ' +
  'reviewed by a lawyer before publishing. This is not legal advice.';

const de: Record<LegalPage, LegalDoc> = {
  impressum: {
    title: 'Impressum',
    banner: BANNER_DE,
    sections: [
      {
        heading: 'Angaben gemäß § 5 DDG',
        body: ['[Name / Firma]', '[Ladungsfähige Anschrift]', '[Land]'],
      },
      {
        heading: 'Kontakt',
        body: ['E-Mail: [E-Mail-Adresse]', 'Telefon: [optional]'],
      },
      {
        heading: 'Vertretungsberechtigte(r)',
        body: ['[Name, bei juristischen Personen]'],
      },
      {
        heading: 'Registereintrag / USt-IdNr.',
        body: [
          '[Handelsregister und Registernummer, falls vorhanden]',
          '[USt-IdNr. gemäß § 27a UStG, falls vorhanden]',
        ],
      },
      {
        heading: 'Verantwortlich für den Inhalt nach § 18 Abs. 2 MStV',
        body: ['[Name und Anschrift der verantwortlichen Person]'],
      },
      {
        heading: 'Software',
        body: [
          'Diese Instanz betreibt OP AI Gateway, freie Software unter der GNU AGPL-3.0.',
          `Quelltext: ${REPO}`,
        ],
      },
    ],
  },
  nutzungsbedingungen: {
    title: 'Nutzungsbedingungen',
    banner: BANNER_DE,
    sections: [
      {
        heading: '1. Geltungsbereich',
        body: [
          'Diese Nutzungsbedingungen regeln die Nutzung dieser vom Betreiber ' +
            'betriebenen OP-AI-Gateway-Instanz (der „Dienst“).',
        ],
      },
      {
        heading: '2. Beschreibung des Dienstes',
        body: [
          'Der Dienst vermittelt KI-Anfragen an angebundene KI-Server und stellt ' +
            'ein Portal zur Verwaltung von Zugängen, Modellen und Nutzung bereit.',
        ],
      },
      {
        heading: '3. Software-Lizenz',
        body: [
          'Die zugrunde liegende Software steht unter der GNU AGPL-3.0. Diese ' +
            'Nutzungsbedingungen betreffen die Nutzung dieser betriebenen Instanz, ' +
            'nicht die Softwarelizenz selbst.',
          `Quelltext: ${REPO}`,
        ],
      },
      {
        heading: '4. Zulässige Nutzung',
        body: [
          'Keine rechtswidrigen, rechteverletzenden oder schädlichen Inhalte oder Anfragen.',
          '[Weitere betreiberspezifische Nutzungsregeln]',
        ],
      },
      {
        heading: '5. Verfügbarkeit',
        body: ['Der Dienst wird ohne Zusicherung einer bestimmten Verfügbarkeit bereitgestellt.'],
      },
      {
        heading: '6. Gewährleistung und Haftung',
        body: [
          'Die Software wird gemäß den §§ 15–16 der AGPL-3.0 „ohne jede ' +
            'Gewährleistung“ bereitgestellt.',
          '[Betreiberspezifische Haftungsregelung]',
        ],
      },
      {
        heading: '7. Änderungen',
        body: ['Der Betreiber kann diese Bedingungen anpassen. [Verfahren/Ankündigung]'],
      },
      {
        heading: '8. Anwendbares Recht und Gerichtsstand',
        body: ['[z. B. Recht der Bundesrepublik Deutschland; Gerichtsstand]'],
      },
    ],
  },
  datenschutz: {
    title: 'Datenschutzerklärung',
    banner: BANNER_DE,
    sections: [
      {
        heading: '1. Verantwortlicher',
        body: [
          '[Name / Firma, Anschrift und Kontakt des Betreibers]',
          '[Datenschutzbeauftragte(r), falls benannt]',
        ],
      },
      {
        heading: '2. Verarbeitete Daten',
        body: [
          'Konto- und Anmeldedaten sowie serverseitige Sitzungen (Login, Rollen).',
          'Von Nutzern erstellte API-Tokens (nur Metadaten und Hashes, keine Klartext-Secrets).',
          'Nutzungsereignisse pro Anfrage, Nutzer, Token und Sitzung (z. B. Modell, ' +
            'Zeitpunkt, Token-Zahlen, Status).',
          'Telemetrie der angebundenen KI-Server.',
          'Optionale Payload-Capture: Inhalte von Anfragen und Antworten NUR wenn ' +
            'aktiviert (verschlüsselt gespeichert oder flüchtig im RAM), Aufbewahrung ' +
            'gemäß der Einstellung capture_retention_days.',
        ],
      },
      {
        heading: '3. Zwecke und Rechtsgrundlagen',
        body: [
          'Bereitstellung und Betrieb des Dienstes, Abrechnung/Kontingente und Sicherheit.',
          'Rechtsgrundlagen: [Art. 6 Abs. 1 DSGVO — konkret benennen]',
        ],
      },
      {
        heading: '4. Speicherdauer',
        body: [
          '[Aufbewahrungsfristen]',
          'Payload-Capture: gemäß capture_retention_days; danach automatische Löschung.',
        ],
      },
      {
        heading: '5. Empfänger / Auftragsverarbeiter',
        body: [
          'Angebundene KI-Server verarbeiten die weitergeleiteten Anfragen.',
          '[Weitere Auftragsverarbeiter und Auftragsverarbeitungsverträge]',
        ],
      },
      {
        heading: '6. Betroffenenrechte',
        body: [
          'Auskunft, Berichtigung, Löschung, Einschränkung, Widerspruch und ' +
            'Datenübertragbarkeit sowie Beschwerderecht bei einer Aufsichtsbehörde.',
        ],
      },
      {
        heading: '7. Kontakt',
        body: ['[E-Mail-Adresse für Datenschutzanfragen]'],
      },
    ],
  },
};

const en: Record<LegalPage, LegalDoc> = {
  impressum: {
    title: 'Legal notice',
    banner: BANNER_EN,
    sections: [
      {
        heading: 'Provider (§ 5 DDG)',
        body: ['[Name / Company]', '[Full postal address]', '[Country]'],
      },
      { heading: 'Contact', body: ['Email: [email address]', 'Phone: [optional]'] },
      {
        heading: 'Authorised representative',
        body: ['[Name, for legal entities]'],
      },
      {
        heading: 'Register entry / VAT ID',
        body: ['[Commercial register and number, if any]', '[VAT ID, if any]'],
      },
      {
        heading: 'Responsible for content',
        body: ['[Name and address of the responsible person]'],
      },
      {
        heading: 'Software',
        body: [
          'This instance runs OP AI Gateway, free software under the GNU AGPL-3.0.',
          `Source code: ${REPO}`,
        ],
      },
    ],
  },
  nutzungsbedingungen: {
    title: 'Terms of use',
    banner: BANNER_EN,
    sections: [
      {
        heading: '1. Scope',
        body: [
          'These terms govern the use of this operator-run OP AI Gateway instance (the "Service").',
        ],
      },
      {
        heading: '2. Description of the Service',
        body: [
          'The Service routes AI requests to connected AI servers and provides a ' +
            'portal to manage access, models and usage.',
        ],
      },
      {
        heading: '3. Software license',
        body: [
          'The underlying software is licensed under the GNU AGPL-3.0. These terms ' +
            'concern the use of this operated instance, not the software license itself.',
          `Source code: ${REPO}`,
        ],
      },
      {
        heading: '4. Acceptable use',
        body: [
          'No unlawful, infringing or harmful content or requests.',
          '[Further operator-specific rules]',
        ],
      },
      {
        heading: '5. Availability',
        body: ['The Service is provided without any guarantee of availability.'],
      },
      {
        heading: '6. Warranty and liability',
        body: [
          'The software is provided "without any warranty" under sections 15–16 of the AGPL-3.0.',
          '[Operator-specific liability terms]',
        ],
      },
      {
        heading: '7. Changes',
        body: ['The operator may amend these terms. [Procedure/notice]'],
      },
      {
        heading: '8. Governing law and jurisdiction',
        body: ['[e.g. applicable law and place of jurisdiction]'],
      },
    ],
  },
  datenschutz: {
    title: 'Privacy policy',
    banner: BANNER_EN,
    sections: [
      {
        heading: '1. Controller',
        body: [
          '[Name / company, address and contact of the operator]',
          '[Data protection officer, if appointed]',
        ],
      },
      {
        heading: '2. Data processed',
        body: [
          'Account and login data and server-side sessions (login, roles).',
          'User-created API tokens (metadata and hashes only, no plaintext secrets).',
          'Usage events per request, user, token and session (e.g. model, time, token counts, status).',
          'Telemetry of the connected AI servers.',
          'Optional payload capture: request and response contents ONLY when enabled ' +
            '(stored encrypted or held volatile in RAM), retained per the ' +
            'capture_retention_days setting.',
        ],
      },
      {
        heading: '3. Purposes and legal bases',
        body: [
          'Provision and operation of the Service, billing/quotas and security.',
          'Legal bases: [Art. 6(1) GDPR — specify]',
        ],
      },
      {
        heading: '4. Retention',
        body: [
          '[Retention periods]',
          'Payload capture: per capture_retention_days, deleted automatically thereafter.',
        ],
      },
      {
        heading: '5. Recipients / processors',
        body: [
          'Connected AI servers process the forwarded requests.',
          '[Further processors and data processing agreements]',
        ],
      },
      {
        heading: '6. Data subject rights',
        body: [
          'Access, rectification, erasure, restriction, objection and data ' +
            'portability, plus the right to lodge a complaint with a supervisory authority.',
        ],
      },
      { heading: '7. Contact', body: ['[Email address for privacy requests]'] },
    ],
  },
};

export const legalContent: Record<Locale, Record<LegalPage, LegalDoc>> = { de, en };
