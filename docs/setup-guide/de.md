# So richtest du FTW ein

Diese Anleitung ist für dich, wenn du noch nie einen Raspberry Pi eingerichtet hast. Keine Sorge — es ist leichter, als es klingt. Folge den Schritten einfach einer nach dem anderen.

> **Hast du FTW schon einmal genutzt?** [Fang hier an](../before.md) (auf Englisch).

> **Nutzt du Home Assistant OS?** Dann brauchst du keinen Raspberry Pi: [Installiere stattdessen die FTW-App](../install.md#home-assistant).

> **Ein anderer Linux-Rechner?** Ein 64-Bit-Rechner mit Debian oder Ubuntu nutzt dieselbe Zeile. Überspringe die Pi-Schritte und geh zu **Schritt 11 — FTW installieren**.

## Was du brauchst

- Einen **Raspberry Pi 3 oder neuer**
- Eine **Speicherkarte** — im Bundle enthalten, ansonsten besorge dir eine separate microSD-Karte
- Ein **Netzteil** — im Bundle enthalten, ansonsten besorge dir ein separates
- Ein **Gehäuse** — optional, außer du hast ein RAK-wireless- oder Sourceful-Gateway (die werden bereits im Gehäuse geliefert)
- **Deinen normalen Computer** (Windows oder Mac)
- Einen **Kartenleser** an deinem normalen Computer (eingebauter Schlitz oder ein kleines USB-Gerät)
- **Internet zu Hause** (Netzwerkkabel oder WLAN)

---

## Schritt 1 — Das Programm herunterladen, das die Speicherkarte vorbereitet

1. Öffne den Webbrowser auf deinem normalen Computer.
2. Gehe zu: **https://www.raspberrypi.com/software/**
3. Klicke auf **Download** und wähle die Version für deinen Computer (Windows oder Mac).
4. Öffne die heruntergeladene Datei und installiere das Programm. Klicke "OK" oder "Weiter", bis es fertig ist.

## Schritt 2 — Das Programm starten

Das Programm heißt **Raspberry Pi Imager**. Öffne es.

## Schritt 3 — Wähle, welchen Raspberry Pi du hast

Du siehst drei große Felder. Klicke auf das obere und wähle:

- Das Modell, das du hast: **Raspberry Pi 3**, **4** oder **5**. Hast du ein Sourceful Gateway oder RAK wireless bekommen? Dann ist es fast immer **Raspberry Pi 4**.

![](images/pi-imager-step1.png)

## Schritt 4 — Die Software wählen

Klicke auf das mittlere Feld ("Operating System"):

- Wähle **Raspberry Pi OS (other)**
- Dann wähle **Raspberry Pi OS Lite (64-bit)**

Das ist das Grundprogramm, damit der Raspberry Pi funktioniert — ähnlich wie Windows auf deinem normalen Computer.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Schritt 5 — Die Speicherkarte in deinen Computer stecken

1. Wähle eine Speicherkarte. Alles, was darauf ist, wird gelöscht.
2. Stecke sie in den Kartenleser an deinem normalen Computer.
3. Klicke im Programm auf das untere Feld ("Storage") und wähle deine Karte aus.

## Schritt 6 — Einstellungen anpassen

Klicke auf **Next**. Das Programm fragt, ob du die Einstellungen anpassen möchtest — sag **Ja** (oder "Edit settings").

Trage ein:

- **Hostname:** `ftw`
- **Localization:** wähle dein Land
- **Benutzername:** z.B. `pi` (nicht `ftw`: FTW legt ein eigenes Konto mit diesem Namen an)
- **Passwort:** wähle eins, das du dir merken kannst — aber nicht "1234"
- **WLAN:** wenn du WLAN benutzt, gib den Namen deines Heim-WLANs (SSID) und das Passwort ein. Benutzt du ein Netzwerkkabel? Überspringe diesen Schritt.
- **Remote access:** schalte **SSH** ein. Bist du fortgeschrittener Nutzer — wähle Login mit **SSH-Schlüssel** (füge deinen öffentlichen Schlüssel ein). Sonst wähle **Passwort + Benutzername**.
- **Raspberry Pi Connect:** einfach weiterklicken.

## Schritt 7 — Auf die Karte schreiben

Klicke auf die Schaltfläche, die alles auf die Speicherkarte schreibt. Warte, bis es fertig ist. Wirf die Karte aus, wenn alles erledigt ist.

## Schritt 8 — Die Karte in den Raspberry Pi stecken

1. Das Netzteil muss vom Raspberry Pi **AUSGESTECKT** sein.
2. Stecke die Speicherkarte hinein.
3. Schließe das Netzteil an.
4. Warte etwa **10 Minuten**. Das Lämpchen blinkt und leuchtet dann dauerhaft. Das ist normal. Geh einen Tee machen.

## Schritt 9 — Die IP-Adresse des Raspberry Pi finden

Dein Raspberry Pi hat jetzt eine **IP-Adresse** in deinem Heimnetzwerk.

Melde dich bei deinem Router an. Schau unter "verbundene Geräte". Such nach etwas namens **ftw**.

Schreibe die IP-Adresse auf. Sie sieht so aus: **192.168.1.xxx**.

> Nicht sicher, wie du dich am Router anmeldest? Frag jemanden, der sich auskennt, oder probiere die Webadresse **192.168.1.1** im Browser.

## Schritt 10 — Mit dem Raspberry Pi sprechen

### Unter Windows

1. Lade **PuTTY** von der offiziellen Seite herunter: **https://www.chiark.greenend.org.uk/~sgtatham/putty/latest.html**
2. Installiere das Programm und öffne es.
3. Im Feld "Host Name": gib die IP-Adresse ein, die du notiert hast.
4. Klicke auf **Open**. Erscheint eine Warnmeldung — klicke auf **Yes / Accept**.
5. Gib den Benutzernamen ein, den du gewählt hast.
6. Gib das Passwort ein (du siehst keine Punkte beim Tippen — das ist Absicht).

### Auf dem Mac

1. Öffne das **Terminal** (drücke `Cmd`+Leertaste, tippe "Terminal", Enter).
2. Tippe (ersetze IP-Adresse und Benutzername durch deine):

   ```
   ssh pi@192.168.1.123
   ```

3. Drücke Enter. Wenn es fragt "Are you sure?" — tippe **yes** und drücke Enter.
4. Gib dein Passwort ein (du siehst keine Zeichen beim Tippen). Drücke Enter.

Gut gemacht — du bist jetzt "im" Raspberry Pi.

## Schritt 11 — FTW installieren

Kopiere diese Zeile. Füge sie in das Fenster aus Schritt 10 ein und drücke Enter:

```bash
curl -fsSL https://ftw.energy/install.sh | bash -s -- --fresh-host
```

(In PuTTY fügst du mit einem Rechtsklick ein. Auf dem Mac drückst du `Cmd`+`V`.)

FTW wird heruntergeladen, geprüft und gestartet. Fragt es nach einem Passwort, gib das aus Schritt 6 ein. Warte, bis es fertig ist. Am Ende zeigt es die Adresse deines FTW, etwa so:

```text
  http://192.168.1.123:8080/setup
  http://ftw.local:8080/setup
```

Zeigt es stattdessen einen Fehler: Hör dort auf und frag auf Discord (der Link steht ganz unten). Probiere vorher nichts anderes.

## Nach der Installation

Öffne den Browser auf deinem normalen Computer oder deinem Handy und rufe eine der Adressen auf. Klappt die eine nicht, nimm die andere.

Die Einrichtungsseite fragt nach drei Dingen:

- **Dein Zuhause:** das Land, die Preiszone und die Größe deiner Hauptsicherung, zum Beispiel 16, 20 oder 25 A. Sie steht in deinem Netzvertrag.
- **Deine Geräte:** FTW sucht sie in deinem Netzwerk. Wähle jedes aus und füge es hinzu.
- **Ein Hauspasswort:** Andere Geräte in deinem WLAN brauchen es, um Einstellungen zu ändern. Wähle eines, das du dir merkst.

Prüfe die Zusammenfassung und klicke auf **Start FTW**. Dann landest du auf der Übersicht. Gut gemacht!

---

## Wenn etwas nicht funktioniert

- **Das Lämpchen leuchtet gar nicht** → prüfe, ob das Netzteil richtig eingesteckt ist.
- **Keine IP-Adresse zu finden** → starte den Router neu, warte 5 Minuten, schau nochmal.
- **SSH sagt "Connection refused"** → warte noch etwas. Der erste Start dauert.
- **Die Seite lädt, aber beim Ändern von Einstellungen erscheint "remote access to protected API routes is disabled"** → FTW nimmt Änderungen nur von Adressen an, die es als lokal erkennt. Öffne die Oberfläche über die IP-Adresse des Pi, z. B. `http://192.168.1.123:8080`. Warum das passiert — auch zu Hause — steht in [operations.md](../operations.md#remote-access-to-protected-api-routes-is-disabled) (auf Englisch).
- **Das alles hilft nicht** → schau auf unserem Discord vorbei und frage freundlich nach Hilfe: **https://discord.gg/UK2ygPBu8N**
