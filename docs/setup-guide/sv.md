# Så här sätter du igång FTW

Den här guiden är skriven för dig som aldrig har pillat med en Raspberry Pi förut. Lugn — det är lättare än det låter. Följ stegen ett i taget, så går det fint.

> **Ingen Raspberry Pi?** Du kan lika gärna köra FTW på en NUC, en gammal bärbar, eller annan hårdvara du har liggande — så länge den kan köra Docker. Den här guiden fokuserar på att komma igång med en Raspberry Pi; är du på en annan burk kan du skumma hårdvarustegen och gå direkt till **Steg 11 — Installera FTW** (installationsskriptet förutsätter Debian eller Ubuntu).

## Vad du behöver

- En **Raspberry Pi**
- Ett **minneskort** — medföljer om du köpte den i en bundle, annars behöver du ett separat microSD-kort
- En **strömadapter** — medföljer om du köpte den i en bundle, annars behöver du en separat
- Ett **chassi** — valfritt, om du inte har en RAK wireless eller Sourceful gateway (de kommer redan i ett chassi)
- **Din vanliga dator** (Windows eller Mac)
- En **kortläsare** till din vanliga dator (inbyggt fack, eller en liten USB-pryl att stoppa i)
- **Internet** hemma (nätverkskabel eller Wi-Fi)

---

## Steg 1 — Hämta programmet som förbereder minneskortet

1. Öppna webbläsaren på din vanliga dator.
2. Gå till: **https://www.raspberrypi.com/software/**
3. Klicka på **Download** och välj versionen som passar din dator (Windows eller Mac).
4. Öppna filen som laddades hem och installera programmet. Klicka "OK" eller "Nästa" tills det är klart.

## Steg 2 — Starta programmet

Programmet heter **Raspberry Pi Imager**. Öppna det.

## Steg 3 — Välj vilken Raspberry Pi du har

Du ser tre stora rutor. Klicka på den översta och välj:

- **Raspberry Pi 4** (det är nästan alltid rätt om du fått en Sourceful gateway eller RAK wireless)

![](images/pi-imager-step1.png)

## Steg 4 — Välj programvara

Klicka på den mellersta rutan ("Operating System"):

- Välj **Raspberry Pi OS (other)**
- Välj sedan **Raspberry Pi OS Lite (64-bit)**

Det här är grundprogrammet som får Raspberry Pin att fungera — ungefär som Windows på din vanliga dator.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Steg 5 — Stoppa in minneskortet i din dator

1. Ta ur minneskortet ur Raspberry Pin (om det sitter i). Var försiktig.
2. Stoppa det i kortläsaren på din vanliga dator.
3. Klicka på den nedersta rutan ("Storage") i programmet och välj ditt kort.

## Steg 6 — Anpassa inställningarna

Klicka på **Next**. Programmet frågar om du vill anpassa inställningar — svara **Ja** (eller "Edit settings").

Fyll i så här:

- **Hostname:** `fortytwo`
- **Localization:** **Sweden / SE**
- **Användarnamn:** t.ex. `pi`
- **Lösenord:** välj något du kommer ihåg, men inte "1234"
- **Wi-Fi:** använder du trådlöst, skriv in namnet på ditt hem-Wi-Fi (SSID) och lösenordet. Använder du nätverkskabel — hoppa över det här steget.
- **Remote access:** slå på **SSH**. Är du avancerad användare — välj **SSH-nyckel** (klistra in din publika nyckel). Annars välj **lösenord + användarnamn**.
- **Raspberry Pi Connect:** klicka bara vidare.

## Steg 7 — Skriv till kortet

Klicka på knappen som skriver allt till minneskortet. Vänta tills det är färdigt. Ejecta kortet (mata ut det) när det är klart.

## Steg 8 — Stoppa kortet i Raspberry Pin

1. **Strömmen ska vara URDRAGEN** ur Raspberry Pin.
2. Stoppa in minneskortet.
3. Anslut strömadaptern.
4. Vänta ungefär **10 minuter**. Lampan blinkar och lyser sen fast. Det är normalt. Sätt på en kopp te under tiden.

## Steg 9 — Hitta Raspberry Pins IP-adress

Din Raspberry Pi har fått en **IP-adress** på ditt hemnätverk.

Logga in på din router. Titta under "anslutna enheter". Leta efter något som heter **fortytwo**.

Skriv ner IP-adressen. Den ser ut som **192.168.1.xxx**.

> Osäker på hur man loggar in på routern? Fråga någon datorkunnig, eller prova webbadressen **192.168.1.1** i webbläsaren.

## Steg 10 — Prata med Raspberry Pin

### Har du Windows?

1. Ladda ner **PuTTY** från den officiella sidan: **https://www.chiark.greenend.org.uk/~sgtatham/putty/latest.html**
2. Installera och starta programmet.
3. I rutan "Host Name": skriv IP-adressen du antecknade.
4. Klicka **Open**. Dyker en varningsruta upp — klicka **Yes / Accept**.
5. Skriv användarnamnet du valde.
6. Skriv lösenordet (du ser inga prickar när du skriver — det är meningen).

### Har du Mac?

1. Öppna **Terminalen** (tryck `Cmd`+mellanslag, skriv "Terminal", enter).
2. Skriv (byt ut IP-adressen och ev. användarnamn mot dina):

   ```
   ssh pi@192.168.1.123
   ```

3. Tryck enter. Säger den "Are you sure?" — skriv **yes** och tryck enter.
4. Skriv lösenordet (du ser inga tecken när du skriver). Tryck enter.

Grattis — du är nu "inne" i Raspberry Pin.

## Steg 11 — Installera FTW

Kopiera denna rad EXAKT som den står:

```
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
```

Klistra in i terminalen eller PuTTY (högerklicka brukar fungera för att klistra in) och tryck enter.

Skriv in ditt lösenord en gång till. Nu installeras allt. Det tar några minuter. **Sen är det klart.**

## Klart

Öppna webbläsaren på din vanliga dator och gå till webb-gränssnittet:

```
http://fortytwo:8080/
```

Fungerar inte den adressen — prova IP-adressen du skrev ner, t.ex. `http://192.168.1.123:8080/`.

---

## Om något inte fungerar

- **Lampan lyser inte alls** → kolla att strömadaptern sitter i.
- **Hittar ingen IP-adress** → starta om routern, vänta 5 minuter, titta igen.
- **SSH säger "Connection refused"** → vänta lite till. Första uppstarten tar tid.
- **Det hjälper inte** → kika in på vår Discord och fråga snällt om hjälp: **https://discord.gg/7Ewr45rd**
