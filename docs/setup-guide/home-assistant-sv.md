# FTW på Home Assistant

Appen **FTW (beta)** kör FTW inne i Home Assistant OS och öppnar det där,
bakom Home Assistants inloggning. Varje FTW-beta kommer som en appuppdatering,
och du väljer när du installerar den. En stabil app som heter **FTW** kommer
till butiken med den första stabila versionen. Du kan också köra FTW på en
[separat Linux-dator](install-sv.md#en-linux-dator) och koppla den till Home
Assistant med [MQTT](../ha-integration.md). [English](../home-assistant-app.md)

Har du kört FTW förut, här eller på en annan dator? Läs
[Byt till nya FTW](update-sv.md) först. Bara en FTW får styra ett hem.

## Det här behöver du

- Home Assistant OS (till exempel Home Assistant Green), amd64 eller aarch64.
  Home Assistant Container/Core har inga appar; använd en
  [separat dator](install-sv.md#en-linux-dator) i stället.
- Ha minst 1 GB ledigt för första bygget, plus plats för historik och backup.
- Växelriktare, batteri, elmätare eller laddbox på samma nätverk.

## 1. Lägg till app-repot

[![Lägg till FTW:s arkiv i min Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fsrcfl%2Fhome-assistant-addons)

Knappen öppnar din Home Assistant och frågar om arkivet ska läggas till. Eller
gör det för hand:

1. **Inställningar → Appar → Installera app** för att öppna appbutiken.
2. Menyn (⋮) uppe till höger → **Git-arkiv** (**Repositories** på engelska).
3. Klistra in `https://github.com/srcfl/home-assistant-addons`, välj **Lägg till**.
4. Stäng och ladda om sidan om inget hänt.

## 2. Installera

1. Öppna **FTW (beta)** i butiken. Versionen börjar med `0.`.
2. Välj **Installera**. Home Assistant bygger appen på enheten från FTW:s
   releasepaket och kontrollerar kontrollsumman. Första bygget laddar ned paket
   och kan ta flera minuter. Vänta tills det är klart.
3. Slå på **Watchdog**, så att Home Assistant startar om FTW när dess
   hälsokontroll misslyckas. **Starta vid uppstart** är redan på. Välj sedan
   **Starta**.
4. Fliken **Logg** ska visa `FTW starting`, sedan
   `no config found — starting setup wizard` vid en nyinstallation.
   Efter installationen visar loggen `HTTP API ready`.

## 3. Öppna FTW och ställ in

Välj **Öppna webbgränssnittet** (**Open Web UI**). FTW öppnas inne i Home
Assistant, inloggat med din Home Assistant-inloggning; det kräver en
administratör. Vill du öppna FTW från sidofältet, slå på **Visa i sidofältet**
(**Show in sidebar**) på appens flik **Info**.

En ny installation öppnar installationsguiden där. Den frågar efter ditt hem
och dina enheter; [Ställ in ditt hem](install-sv.md#ställ-in-ditt-hem) visar
vad du kan vänta dig. Home Assistants inloggning skyddar FTW, så guiden frågar
inte efter något huslösenord.

FTW har ingen egen webbport på ditt nätverk; det öppnas bara genom Home
Assistant. Ge Home Assistant en fast adress (en DHCP-reservation i routern),
så att dina enheter fortsätter att hitta den.

## 4. Koppla iPhone-appen

Öppna **Settings → FTW app** i FTW och följ [parkopplingen](../pairing.md).
Använd parkopplingslänken eller QR-koden; telefonen når sedan FTW genom FTW
Cloud. Den korta koden fungerar inte i den här appen, eftersom den kräver FTW:s
egen webbport på ditt nätverk.

## 5. Entiteter i Home Assistant

FTW:s värden syns som entiteter i Home Assistant utan att du skriver något.
Lägg till appen **Mosquitto broker** och integrationen **MQTT** som Home
Assistant då erbjuder. Körde FTW redan när du lade till Mosquitto, starta om
FTW en gång. Appens logg visar då
`Home Assistant's MQTT broker is core-mosquitto:1883`.

Vill du stänga av det, eller använda en annan broker, öppna
**Settings → Integrations** i FTW. Se [Home Assistant](../ha-integration.md)
(på engelska).

## Uppdateringar

Nya betor kommer som vanliga appuppdateringar under **Inställningar →
Uppdateringar**. Räkna med täta uppdateringar under betan. Ta backup först om
du vill kunna gå tillbaka. Home Assistant äger appens uppdateringar;
native-installationens provstart och automatiska återgång finns inte här.
`ftw update` gäller inte i appen.

Den stabila appen **FTW** är en egen app med egna data, så att gå från
**FTW (beta)** till **FTW** är en ny installation. Ta en backup, stoppa
**FTW (beta)** och stäng av dess **Starta vid uppstart** och **Watchdog**.
Installera sedan **FTW** och ställ in den.

## Backup och återställning

- Home Assistants backup tar med FTW:s data (inställningar, historik,
  parkoppling). Appen stoppas kort under backup.
- Återställ FTW-appen via **Inställningar → System → Säkerhetskopior**.
  Då återställer HA både appversionen och dess data. Håll andra FTW stoppade.

## Felsökning

| Det du ser | Gör så här |
|---|---|
| Appen syns inte i butiken | Kontrollera repo-URL:en, ladda om sidan eller välj **⋮ → Sök efter uppdateringar**. |
| Installationen misslyckas | Se **Inställningar → System → Loggar → Supervisor**. Ett fel vid nedladdning eller kontrollsumma: försök igen senare. |
| Webbgränssnittet öppnas inte | Kolla appens **Logg**. Öppna FTW med **Öppna webbgränssnittet** eller från sidofältet; ett bokmärke till port 8080 fungerar inte. Port 18099 på Home Assistant måste vara ledig. |
| Inga FTW-entiteter | Kontrollera att appen Mosquitto broker körs och att MQTT-integrationen är tillagd, och starta sedan om FTW. Appens logg säger vad den hittade. |
| FTW når inte en enhet | Kontrollera enhetens IP-adress och att den är på samma nätverk. |
| Två appar heter **FTW (beta)** | Den med version 3.x är pensionerad. Se [Byt till nya FTW](update-sv.md). |

Fråga i [Discord](https://discord.gg/25xcBzQaux) och bifoga appens logg.
