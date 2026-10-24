# FTW på Home Assistant (0.x beta, pilot)

[English](../home-assistant-app.md)

> **Beta / pilot.** Appen **FTW (0.x beta)** är avsedd för en första pilot på
> Home Assistant Green. Granskade betaversioner
> kommer som appuppdateringar. Du väljer när du installerar dem. Du kan också köra FTW på en
> [separat Linux-dator](update-sv.md) och koppla den till Home Assistant med
> [MQTT](../ha-integration.md).

## Det här behöver du

- Home Assistant OS (till exempel Home Assistant Green), amd64 eller aarch64.
  Home Assistant Container/Core har inga appar.
- Ha minst 1 GB ledigt för första bygget, plus plats för historik och backup.
- Växelriktare, batteri, elmätare eller laddbox på samma nätverk.

## 1. Stoppa annan FTW först

Bara en FTW får styra anläggningen.

- **Gamla appen FTW (beta), 3.x:** **Inställningar → Appar**, stäng av
  **Starta vid uppstart** och **Watchdog**, sedan **Stoppa**. Ta en backup
  först om du vill spara den. Inställningar och historik följer inte med.
- **FTW på en annan dator:** stoppa den där.

## 2. Lägg till app-repot

1. **Inställningar → Appar → Installera app** för att öppna appbutiken.
2. Menyn (⋮) uppe till höger → **Git-arkiv** (**Repositories** på engelska).
3. Klistra in `https://github.com/srcfl/home-assistant-addons`, välj **Lägg till**.
4. Stäng och ladda om sidan om inget hänt.

## 3. Installera

1. Öppna **FTW (0.x beta)** i butiken.
2. Välj **Installera**. Home Assistant bygger appen på enheten från FTW:s
   releasepaket och kontrollerar kontrollsumman. Första bygget laddar ned paket
   och kan ta flera minuter. Vänta tills det är klart.
3. Slå på **Starta vid uppstart** och **Watchdog**, sedan **Starta**.
4. Fliken **Logg** ska visa `FTW starting`, sedan
   `no config found — starting setup wizard` vid en nyinstallation.
   Efter installationen visar loggen `HTTP API ready`.

## 4. Öppna FTW och ställ in

Välj **Öppna webbgränssnittet** inne i Home Assistant. Appversioner med
ingress använder din HA-inloggning; du behöver inget eget FTW-lösenord.
Du kan slå på **Visa i sidofältet** på appens sida. HA kräver en administratör
för att öppna FTW. Den direkta webbporten på hemnätverket är stängd.

Följ installationsguiden och lägg till dina enheter ([guide](sv.md)).
Det gäller också en befintlig app med ett sparat FTW-lösenord: efter uppdatering
öppnar du FTW via HA. Inställningar och historik finns kvar.

Ge Home Assistant en fast adress (DHCP-reservation i routern).

## 5. Koppla FTW-appen eller webappen

Nya FTW-webappen och iOS-appen kräver nya FTW 0.x, som den här appen ger.
Ingress ger åtkomst till webbgränssnittet genom HA. Direkt LAN-åtkomst från
en separat FTW-klient ingår inte i detta läge. För en separat klient behöver
du aktivera FTW Cloud och följa [parkopplingen](../pairing.md).
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
  Öppnas inte webbgränssnittet: öppna appen genom HA och kolla appens logg. Ingressport 18099 måste vara ledig.
- Fråga i [Discord](https://discord.gg/UK2ygPBu8N) och bifoga appens logg.
