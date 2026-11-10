# FTW på Home Assistant (0.x beta, pilot)

[English](../home-assistant-app.md)

> **Beta / pilot.** Appen **FTW (0.x beta)** är avsedd för en första pilot på
> Home Assistant Green. Granskade betaversioner
> kommer som appuppdateringar. Du väljer när du installerar dem. Du kan också köra FTW på en
> [separat Linux-dator](install-sv.md#raspberry-pi-eller-linux) och koppla den till Home Assistant med
> [MQTT](../ha-integration.md).

## Det här behöver du

- Home Assistant OS (till exempel Home Assistant Green), amd64 eller aarch64.
  Home Assistant Container/Core har inga appar.
- Ha minst 1 GB ledigt för första bygget, plus plats för historik och backup.
- Växelriktare, batteri, elmätare eller laddbox på samma nätverk.

## Kör du redan en annan FTW?

Bara en FTW får styra anläggningen. Är du ny på FTW? Gå till steg 1.

- **Gamla appen FTW (beta), 3.x:** **Inställningar → Appar**, stäng av
  **Starta vid uppstart** och **Watchdog**, sedan **Stoppa**. Ta en backup
  först om du vill spara den. Inställningar och historik följer inte med.
- **FTW på en annan dator:** stoppa den där.

## 1. Lägg till app-repot

[![Lägg till FTW:s arkiv i min Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fsrcfl%2Fhome-assistant-addons)

Knappen öppnar din Home Assistant och frågar om arkivet ska läggas till. Eller
gör det för hand:

1. **Inställningar → Appar → Installera app** för att öppna appbutiken.
2. Menyn (⋮) uppe till höger → **Git-arkiv** (**Repositories** på engelska).
3. Klistra in `https://github.com/srcfl/home-assistant-addons`, välj **Lägg till**.
4. Stäng och ladda om sidan om inget hänt.

## 2. Installera

1. Öppna **FTW (0.x beta)** i butiken.
2. Välj **Installera**. Home Assistant bygger appen på enheten från FTW:s
   releasepaket och kontrollerar kontrollsumman. Första bygget laddar ned paket
   och kan ta flera minuter. Vänta tills det är klart.
3. Slå på **Starta vid uppstart** och **Watchdog**, sedan **Starta**.
4. Fliken **Logg** ska visa `FTW starting`, sedan
   `no config found — starting setup wizard` vid en nyinstallation.
   Efter installationen visar loggen `HTTP API ready`.

## 3. Öppna FTW och ställ in

Välj **Öppna webbgränssnittet** på appens sida. Det öppnar FTW på port 8080
på din Home Assistants adress, direkt på hemnätverket. Det går inte genom
Home Assistants inloggning.

En ny installation öppnar installationsguiden. Följ den och lägg till dina
enheter; [Ställ in ditt hem](install-sv.md#ställ-in-ditt-hem) visar vad du kan
vänta dig.

Ge Home Assistant en fast adress (DHCP-reservation i routern).

## 4. Koppla FTW-appen eller webappen

FTW-webappen och iPhone-appen kräver nya FTW 0.x, som den här appen ger.
Öppna **Settings → FTW app** i FTW och följ [parkopplingen](../pairing.md).
Har du använt webappen mot äldre FTW: ta bort den från hemskärmen och
parkoppla igen.

## Uppdateringar, backup och felsökning

- Nya betor kommer som vanliga appuppdateringar under **Inställningar →
  Uppdateringar**. Räkna med täta uppdateringar under betan. Ta backup först.
- Home Assistants backup tar med FTW:s data. Appen stoppas kort under backup.
- Återställ FTW-appen via **Inställningar → System → Säkerhetskopior**.
  Då återställer HA både appversionen och dess data. Håll andra FTW stoppade.
  Native-installationens automatiska återgång finns inte här; HA äger uppdateringen.
- Syns inte appen: kontrollera repo-URL:en och ladda om. Misslyckas
  installationen: se **Inställningar → System → Loggar → Supervisor**.
  Öppnas inte webbgränssnittet: kolla appens logg. Port 8080 måste vara ledig.
- Fråga i [Discord](https://discord.gg/UK2ygPBu8N) och bifoga appens logg.
