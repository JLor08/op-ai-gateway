# API-Token: Angebotene Override-Namen und Umleitung unbekannter Modelle

**Status:** entworfen, freigegeben
**Datum:** 2026-08-24

## Ziel

Zwei Einstellungen am API-Token, plus eine Anzeige:

1. **Angebotene Override-Namen.** Pro Override-Zeile ist wählbar, ob der
   angefragte Name als Modell angeboten wird und ob das Ziel dafür aus der
   Liste verschwindet.
2. **Umleitung unbekannter Modelle.** Anfragen an Modelle, die für dieses
   Token nicht greifen, landen beim zuletzt erfolgreich genutzten Modell oder
   einer eingestellten Ausweichwahl.
3. **Zuletzt genutztes Modell** wird für jedes Token geführt und angezeigt.

## Ausgangslage

- Ein Token trägt heute `model_override` (Catch-all) und `model_override_map`
  (`angefragter Name → Gateway-Modell`). `resolveModelOverride` in
  `internal/gateway/inference_handlers.go` ist die einzige Stelle, die beides
  auflöst — sowohl die Übersetzungs-Handler als auch der native Passthrough
  gehen über denselben Durchgang (`inferencePreflight`).
- Die Modell-Liste (`/v1/models`, Anthropic, LM Studio) kommt aus
  `Portal.ModelsForFlavor` → `modelFlavorSets`. Sie ist bereits token-abhängig
  und mischt **Modelle und Gruppen in einen Namensraum**; versteckte und
  gesperrte Einträge sind dort schon entfernt.
- Ein Name, der nicht auflöst, endet in `routing.ErrNoModelRoute`.
- `last_used_at` wird bei jeder Authentifizierung geschrieben
  (`sqlite_token.go`), ungedrosselt.

## Entscheidungen

| Frage | Entscheidung |
|---|---|
| Granularität Feature 1 | Pro Override-Zeile |
| Ziel-Modell bei angebotenem Alias | Ebenfalls pro Zeile wählbar (zwei Schalter) |
| Was heißt „zuletzt genutzt" | Die letzte **erfolgreich geroutete** Anfrage |
| Vorrang Catch-all vs. Umleitung | Catch-all gewinnt |
| Auslöser für den Fallback | Nur wenn das gemerkte Ziel nicht mehr angeboten wird (oder es keins gibt); vorübergehende Nichterreichbarkeit bleibt Sache des Routings |
| Gesperrte Modelle umleiten | Am Token umschaltbar |
| Ort der Umleitung | Gateway-Layer, nicht im Routing-Resolver |
| Merker-Speicherung | Spalte am Token, geschrieben nur bei Wertänderung |

Verworfen: die Umleitung im Routing-Resolver (zieht Token-Policy in
`resolveGroupOnce`, die komplexeste Stelle im System — siehe §11.4), und die
Ableitung des Merkers aus den Usage-Events (der Memory-Store hat keinen
Usage-Event-Store, das Feature wäre dort wirkungslos).

## Datenmodell

Neue Felder am Token-Record:

| Feld | Typ | Bedeutung |
|---|---|---|
| `last_used_model` | string | Name des zuletzt erfolgreich gerouteten Modells oder der Gruppe. Für **alle** Tokens geführt, unabhängig von der Umleitung. |
| `unknown_model_redirect` | bool | Schaltet Feature 2 scharf. Default `false`. |
| `unknown_model_redirect_blocked` | bool | `false` = nur gar nicht auflösbare Namen umleiten; `true` = auch Namen, die dieses Token nicht nutzen darf. Nur wirksam bei aktiver Umleitung. |
| `unknown_model_fallback` | string | Modell **oder** Gruppe, wenn kein gültiges „letztes" vorliegt. `""` = keins. |

Die Override-Map wechselt von `map[string]string` auf eine Map auf ein Objekt:

```json
{ "gpt-4o": { "to": "qwen3-32b", "offer": true, "hide_target": false } }
```

Die Migration liest bestehende Werte als `offer=false, hide_target=false`, damit
sich für kein existierendes Token etwas ändert.

## Verhalten

### Modell-Liste

Über das Ergebnis von `modelFlavorSets` legt sich ein Overlay:

- Jede Zeile mit `offer` fügt ihren **Schlüssel** als angebotenen Namen hinzu
  und erbt die API-Flavors ihres Ziels. Ein Anthropic-only-Ziel erscheint damit
  nicht in der OpenAI-Liste.
- Ein Alias wird auch dann angeboten, wenn sein Ziel `visibility != shown` hat.
  Der Alias ist ein anderer Name und verrät den versteckten nicht.
- Jede Zeile mit `hide_target` entfernt den Zielnamen aus der Liste. Zeigen
  mehrere Zeilen auf dasselbe Ziel und verbirgt es nur eine, ist es verborgen:
  ein gesetzter Schalter ist eine Anweisung, ein nicht gesetzter nur deren
  Abwesenheit.
- Der Catch-all bekommt keinen Schalter — er hat keinen Namen, den man
  anbieten könnte.

Die Liste ist eine Anzeige, keine Zugriffsbeschränkung: ein verborgenes Ziel
bleibt über seinen echten Namen aufrufbar, genau wie heute.

### Auflösung beim Request

In `inferencePreflight`, in dieser Reihenfolge:

1. Exakte Override-Zeile → deren Ziel *(unverändert)*
2. Catch-all → dessen Ziel *(unverändert)*
3. Umleitung aktiv und der Name greift nicht:
   a. `last_used_model`, sofern es diesem Token **für diesen Flavor gerade
      angeboten** wird
   b. sonst `unknown_model_fallback`, ebenfalls nur wenn angeboten
4. sonst der heutige Fehler, unverändert

„Greift nicht" heißt bei `unknown_model_redirect_blocked = false` nur: der Name
löst gar nicht auf. Bei `true` zusätzlich: der Name löst auf, ist für dieses
Token aber gesperrt (Service-Allowlist, Ressourcengruppen-Sichtbarkeit).

**Invariante:** Das Umleitungsziel durchläuft sämtliche Zulassungsprüfungen so,
als hätte der Client es selbst genannt — Allowlist, Sichtbarkeit,
Provisionierung. Die Umleitung ändert, welcher Name angefragt wird, niemals was
ein Token erreichen darf. Die Variante „auch Gesperrtes umleiten" ersetzt
deshalb eine Ablehnung durch ein erlaubtes Ziel; sie hebt keine Sperre auf.

### Merker

Nach erfolgreicher Auflösung wird der **effektive** Name geschrieben — also
nach Override und nach Umleitung — und nur, wenn er vom gespeicherten Wert
abweicht. Wiederholte Anfragen auf dasselbe Modell verursachen damit keinen
zusätzlichen Schreibvorgang.

Folge, bewusst so: Greift einmal der Fallback, wird er zum neuen „zuletzt
genutzt" und die nächste unbekannte Anfrage landet wieder dort.

## Oberfläche

- Der geteilte `ModelOverrideEditor` bekommt pro Zeile zwei Checkboxen. Da ihn
  `TokenList` und `ServiceTokensSection` gemeinsam nutzen, gilt Feature 1 für
  persönliche, Chat- und Service-Tokens.
- Das Token-Formular bekommt einen Abschnitt: Schalter für die Umleitung,
  darunter der Umschalter für gesperrte Modelle, der Fallback-Wähler und die
  **Anzeige des zuletzt genutzten Modells** (im Anlegen-Formular ein
  Platzhalter). Die Untereinstellungen sind ausgegraut, solange die Umleitung
  aus ist.
- Der Fallback-Wähler ist **eine** Liste aus Modellen und Gruppen.
- Die Token-Liste bekommt eine Spalte „zuletzt genutztes Modell",
  **standardmäßig ausgeblendet**.
- Feature 2 gilt für alle API-Token-Sorten; eine Sonderregel für eine Sorte
  wäre nur Verwirrung.

## Fehlerverhalten

Der Fallback-Name wird beim Speichern validiert und abgelehnt, wenn er nicht
existiert — dasselbe Muster und derselbe Fehlercode wie beim Override-Ziel
(`ErrTokenModelOverrideInvalid`). Es braucht keinen neuen Fehlercode.

Verschwindet das Modell später, greift die Umleitung schlicht nicht mehr und
der Client bekommt den heutigen Fehler. Das ist kein Sonderfall, sondern
Schritt 4 der Auflösung.

## Tests

- **Auflösungsreihenfolge** als reine Funktionstests über alle Kombinationen
  aus exakter Zeile, Catch-all, Merker, Fallback und beiden Schalterstellungen.
- **Listen-Zusammenstellung**: angebotene Aliasse, deren Flavor-Erbschaft,
  verborgene Ziele und der Zwei-Zeilen-Konflikt.
- **Migration** gegen echtes PostgreSQL über die Store-Konformitätssuite: eine
  Alt-Zeile schreiben, nach der Migration lesen, beide Schalter `false`, die
  Map verlustfrei. Das ist der heikelste Punkt der ganzen Änderung.
- **Merker-Schreibpfad**: kein Schreibvorgang bei gleichbleibendem Modell, ein
  Schreibvorgang bei Wechsel.
- **Frontend**: neue Formularfelder samt Ausgrauen, die neue Spalte und ihr
  Standard-Ausgeblendet-Sein.

## Nicht Teil dieser Änderung

- Die Modell-Liste als Zugriffsbeschränkung. Sie bleibt eine Anzeige.
- Umleitung bei vorübergehender Nichterreichbarkeit — dafür sind
  Warteschlange und Warmstart im Routing zuständig.
- Ein Verlauf zuletzt genutzter Modelle. Gemerkt wird genau eines.
