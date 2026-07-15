# So richtest du FTW ein

Diese Anleitung ist für dich, wenn du noch nie einen Raspberry Pi eingerichtet hast. Keine Sorge — es ist leichter, als es klingt. Folge den Schritten einfach einer nach dem anderen.

> **Kein Raspberry Pi?** Du kannst FTW auch auf einem NUC, einem alten Laptop oder anderer Hardware laufen lassen, die du herumliegen hast — solange sie Docker ausführen kann. Diese Anleitung konzentriert sich auf die Einrichtung mit einem Raspberry Pi; wenn du eine andere Maschine nutzt, überspringe die Hardware-Schritte und gehe direkt zu **Schritt 11 — FTW installieren** (das Installationsskript setzt Debian oder Ubuntu voraus).

## Was du brauchst

- Einen **Raspberry Pi**
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

- **Raspberry Pi 4** (das ist fast immer richtig, wenn du ein Sourceful Gateway oder RAK wireless bekommen hast)

![](images/pi-imager-step1.png)

## Schritt 4 — Die Software wählen

Klicke auf das mittlere Feld ("Operating System"):

- Wähle **Raspberry Pi OS (other)**
- Dann wähle **Raspberry Pi OS Lite (64-bit)**

Das ist das Grundprogramm, damit der Raspberry Pi funktioniert — ähnlich wie Windows auf deinem normalen Computer.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Schritt 5 — Die Speicherkarte in deinen Computer stecken

1. Nimm die Speicherkarte aus dem Raspberry Pi (falls sie drin ist). Sei vorsichtig.
2. Stecke sie in den Kartenleser an deinem normalen Computer.
3. Klicke im Programm auf das untere Feld ("Storage") und wähle deine Karte aus.

## Schritt 6 — Einstellungen anpassen

Klicke auf **Next**. Das Programm fragt, ob du die Einstellungen anpassen möchtest — sag **Ja** (oder "Edit settings").

Trage ein:

- **Hostname:** `fortytwo`
- **Localization:** wähle dein Land
- **Benutzername:** z.B. `pi`
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

Melde dich bei deinem Router an. Schau unter "verbundene Geräte". Such nach etwas namens **fortytwo**.

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

Kopiere diese Zeile GENAU so, wie sie ist:

```
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
```

Füge sie im Terminal oder in PuTTY ein (Rechtsklick fügt meistens ein) und drücke Enter.

Gib dein Passwort noch einmal ein. Jetzt wird alles installiert. Das dauert ein paar Minuten. **Dann bist du fertig.**

## Fertig

Öffne den Browser auf deinem normalen Computer und rufe die Web-Oberfläche auf:

```
http://fortytwo:8080/
```

Wenn diese Adresse nicht funktioniert — probiere die IP-Adresse, die du aufgeschrieben hast, z.B. `http://192.168.1.123:8080/`.

---

## Wenn etwas nicht funktioniert

- **Das Lämpchen leuchtet gar nicht** → prüfe, ob das Netzteil richtig eingesteckt ist.
- **Keine IP-Adresse zu finden** → starte den Router neu, warte 5 Minuten, schau nochmal.
- **SSH sagt "Connection refused"** → warte noch etwas. Der erste Start dauert.
- **Das alles hilft nicht** → schau auf unserem Discord vorbei und frage freundlich nach Hilfe: **https://discord.gg/7Ewr45rd**
