# Cómo poner en marcha FTW

Esta guía es para ti que nunca has configurado una Raspberry Pi antes. Tranquila — es más fácil de lo que parece. Basta con seguir los pasos, uno a uno.

> **Beta nativa 0.x:** la antigua imagen FTW y el instalador Docker ya no se usan en equipos nuevos. Esta guía cubre la preparación de la Pi. Quienes prueben la beta en un equipo nuevo de 64 bits deben seguir la [instalación nativa para Linux](../native-beta.md#install). ¿Ya usas FTW? Consulta [Coming from an older FTW](../native-beta.md#coming-from-an-older-ftw).

> **¿No tienes Raspberry Pi?** Un equipo nuevo de 64 bits con Debian o Ubuntu puede usar el mismo instalador de la beta nativa. Salta los pasos de la Pi y lee el **Paso 11 — Instalar FTW**.

## Lo que necesitas

- Una **Raspberry Pi**
- Una **tarjeta de memoria** — incluida si la compraste en un pack, si no, consigue una microSD aparte
- Un **adaptador de corriente** — incluido si lo compraste en un pack, si no, consigue uno aparte
- Una **caja** — opcional, salvo si tienes un RAK wireless o una Sourceful gateway (vienen ya con carcasa)
- **Tu ordenador normal** (Windows o Mac)
- Un **lector de tarjetas** en tu ordenador normal (ranura incorporada o un pequeño aparato USB)
- **Internet en casa** (cable de red o Wi-Fi)

---

## Paso 1 — Descargar el programa que prepara la tarjeta de memoria

1. Abre el navegador en tu ordenador normal.
2. Ve a: **https://www.raspberrypi.com/software/**
3. Haz clic en **Download** y elige la versión para tu ordenador (Windows o Mac).
4. Abre el archivo descargado e instala el programa. Haz clic en "OK" o "Siguiente" hasta que termine.

## Paso 2 — Abrir el programa

El programa se llama **Raspberry Pi Imager**. Ábrelo.

## Paso 3 — Elegir qué Raspberry Pi tienes

Verás tres cuadros grandes. Haz clic en el de arriba y elige:

- **Raspberry Pi 4** (casi siempre es lo correcto si tienes un Sourceful gateway o RAK wireless)

![](images/pi-imager-step1.png)

## Paso 4 — Elegir el programa

Haz clic en el cuadro del medio ("Operating System"):

- Elige **Raspberry Pi OS (other)**
- Después elige **Raspberry Pi OS Lite (64-bit)**

Es el programa base que hace funcionar a la Raspberry Pi — un poco como Windows en tu ordenador normal.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Paso 5 — Pon la tarjeta de memoria en tu ordenador

1. Saca la tarjeta de memoria de la Raspberry Pi (si está dentro). Manéjala con cuidado.
2. Deslízala en el lector de tarjetas de tu ordenador normal.
3. Haz clic en el cuadro de abajo ("Storage") en el programa y elige tu tarjeta.

## Paso 6 — Personalizar los ajustes

Haz clic en **Next**. El programa pregunta si quieres personalizar los ajustes — di **Sí** (o "Edit settings").

Rellena:

- **Hostname:** `ftw`
- **Localization:** elige tu país
- **Usuario:** p. ej. `pi` (no `ftw`: FTW crea su propia cuenta con ese nombre)
- **Contraseña:** elige una que recuerdes — pero no "1234"
- **Wi-Fi:** si vas a usar Wi-Fi, escribe el nombre de tu Wi-Fi de casa (SSID) y la contraseña. ¿Usas cable de red? Sáltate este paso.
- **Remote access:** activa **SSH**. Si eres un usuario avanzado — elige inicio de sesión con **clave SSH** (pega tu clave pública). Si no, elige **contraseña + usuario**.
- **Raspberry Pi Connect:** pasa de largo haciendo clic.

## Paso 7 — Escribir en la tarjeta

Haz clic en el botón que escribe todo en la tarjeta de memoria. Espera a que termine. Expulsa la tarjeta cuando esté listo.

## Paso 8 — Mete la tarjeta en la Raspberry Pi

1. El adaptador de corriente debe estar **DESCONECTADO** de la Raspberry Pi.
2. Inserta la tarjeta de memoria.
3. Conecta el adaptador de corriente.
4. Espera unos **10 minutos**. La luz parpadeará y luego quedará fija. Es normal. Prepárate un té.

## Paso 9 — Encontrar la dirección IP de la Raspberry Pi

Tu Raspberry Pi tiene ahora una **dirección IP** en tu red de casa.

Entra en tu router. Mira en "dispositivos conectados". Busca algo que se llame **ftw**.

Apunta la dirección IP. Se ve así: **192.168.1.xxx**.

> ¿No sabes cómo entrar en el router? Pregúntale a alguien que sepa, o prueba la dirección web **192.168.1.1** en el navegador.

## Paso 10 — Hablar con la Raspberry Pi

### En Windows

1. Descarga **PuTTY** desde el sitio oficial: **https://www.chiark.greenend.org.uk/~sgtatham/putty/latest.html**
2. Instala el programa y ábrelo.
3. En la casilla "Host Name": escribe la dirección IP que anotaste.
4. Haz clic en **Open**. Si sale un aviso — haz clic en **Yes / Accept**.
5. Escribe el usuario que elegiste.
6. Escribe la contraseña (no verás puntitos mientras escribes — es a propósito).

### En Mac

1. Abre la **Terminal** (pulsa `Cmd`+espacio, escribe "Terminal", enter).
2. Escribe (sustituye la dirección IP y el usuario por los tuyos):

   ```
   ssh pi@192.168.1.123
   ```

3. Pulsa enter. Si dice "Are you sure?" — escribe **yes** y pulsa enter.
4. Escribe tu contraseña (no verás ningún carácter mientras escribes). Pulsa enter.

Bien hecho — ya estás "dentro" de la Raspberry Pi.

## Paso 11 — Instalar FTW

El instalador antiguo de Docker de una sola línea ya no se usa. Native 0.x
sigue en pruebas; esta guía aún no ofrece una instalación general. Si participas
en la beta y tienes una Pi nueva de 64 bits, sigue los pasos con una versión
exacta en la [guía de instalación](../native-beta.md#install). Si FTW ya se
ejecuta en tu Pi, conserva esa tarjeta tal cual y lee
[Coming from an older FTW](../native-beta.md#coming-from-an-older-ftw).

## Después de la instalación

Abre el navegador en tu ordenador normal y ve a la interfaz web:

```
http://ftw.local:8080/
```

Si esa dirección no funciona — prueba con la dirección IP que apuntaste, p. ej. `http://192.168.1.123:8080/`.

---

## Si algo no funciona

- **La luz no se enciende nada** → revisa que el adaptador de corriente esté bien conectado.
- **No encuentras la dirección IP** → reinicia el router, espera 5 minutos, vuelve a mirar.
- **SSH dice "Connection refused"** → espera un poco más. El primer arranque tarda.
- **La página abre, pero al cambiar ajustes aparece "remote access to protected API routes is disabled"** → FTW solo acepta cambios desde direcciones que reconoce como locales. Abre la interfaz con la dirección IP de la Pi, p. ej. `http://192.168.1.123:8080`. El porqué — incluso en casa — se explica en [operations.md](../operations.md#remote-access-to-protected-api-routes-is-disabled) (en inglés).
- **Nada de esto funciona** → pásate por nuestro Discord y pide ayuda amablemente: **https://discord.gg/25xcBzQaux**
