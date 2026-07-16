# Getting FTW up and running

This guide is for you if you've never set up a Raspberry Pi before. Relax — it's easier than it sounds. Just follow the steps, one at a time.

> **Alternative manual path:** the recommended installation uses the ready-made FTW image and Raspberry Pi Imager repository in [`../rpi-image.md`](../rpi-image.md). Continue here only when you specifically want generic Raspberry Pi OS + Docker.

> **Don't have a Raspberry Pi?** You can also run FTW on a NUC, an old laptop, or any other hardware you have lying around — as long as it can run Docker. This guide focuses on getting started with a Raspberry Pi; if you're on another box, skim the hardware steps and jump to **Step 11 — Install FTW** (the install script assumes Debian or Ubuntu).

## What you'll need

- A **Raspberry Pi**
- A **memory card** — included if you bought it as a bundle, otherwise grab a separate microSD card
- A **power adapter** — included if you bought it as a bundle, otherwise grab a separate one
- A **case** — optional, unless you bought a RAK wireless or Sourceful gateway (those already come housed)
- **Your regular computer** (Windows or Mac)
- A **card reader** on your regular computer (a built-in slot, or a small USB gadget)
- **Internet at home** (a network cable or Wi-Fi)

---

## Step 1 — Download the program that prepares the memory card

1. Open the web browser on your regular computer.
2. Go to: **https://www.raspberrypi.com/software/**
3. Click **Download** and choose the version for your computer (Windows or Mac).
4. Open the file you downloaded and install the program. Click "OK" or "Next" until it's finished.

## Step 2 — Start the program

The program is called **Raspberry Pi Imager**. Open it.

## Step 3 — Choose which Raspberry Pi you have

You'll see three big boxes. Click the top one and select:

- **Raspberry Pi 4** (this is almost always right if you got a Sourceful gateway or RAK wireless)

![](images/pi-imager-step1.png)

## Step 4 — Choose the software

Click the middle box ("Operating System"):

- Choose **Raspberry Pi OS (other)**
- Then choose **Raspberry Pi OS Lite (64-bit)**

This is the base program that makes the Raspberry Pi work — a bit like Windows on your regular computer.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Step 5 — Put the memory card in your computer

1. Take the memory card out of the Raspberry Pi (if it's in there). Handle it gently.
2. Slide it into the card reader on your regular computer.
3. Click the bottom box ("Storage") in the program and select your card.

## Step 6 — Customize the settings

Click **Next**. The program asks whether you want to customize settings — say **Yes** (or "Edit settings").

Fill in:

- **Hostname:** `ftw`
- **Localization:** choose your country
- **Username:** e.g. `pi`
- **Password:** pick something you'll remember — but not "1234"
- **Wi-Fi:** if you're using wireless, type your home Wi-Fi name (SSID) and password. Using a network cable? Skip this step.
- **Remote access:** turn on **SSH**. If you're an advanced user — choose **SSH key** login (paste your public key). Otherwise choose **password + username**.
- **Raspberry Pi Connect:** just click past it.

## Step 7 — Write to the card

Click the button that writes everything to the memory card. Wait until it says it's done. Eject the card when it's finished.

## Step 8 — Put the card in the Raspberry Pi

1. The power must be **UNPLUGGED** from the Raspberry Pi.
2. Insert the memory card.
3. Plug in the power adapter.
4. Wait about **10 minutes**. The light will blink and then shine steadily. That's normal. Go make a cup of tea.

## Step 9 — Find the Raspberry Pi's IP address

Your Raspberry Pi now has an **IP-address** on your home network.

Log in to your router. Look under "connected devices". Look for something called **ftw**.

Write down the IP address. It looks like **192.168.1.xxx**.

> Not sure how to log in to the router? Ask someone tech-savvy, or try the web address **192.168.1.1** in your browser.

## Step 10 — Talk to the Raspberry Pi

### On Windows

1. Download **PuTTY** from the official site: **https://www.chiark.greenend.org.uk/~sgtatham/putty/latest.html**
2. Install and open the program.
3. In the "Host Name" box: type the IP address you wrote down.
4. Click **Open**. If a warning pops up — click **Yes / Accept**.
5. Type the username you chose.
6. Type the password (you won't see dots as you type — that's on purpose).

### On Mac

1. Open **Terminal** (press `Cmd`+space, type "Terminal", press enter).
2. Type (replace the IP address and username with yours):

   ```
   ssh pi@192.168.1.123
   ```

3. Press enter. If it says "Are you sure?" — type **yes** and press enter.
4. Type your password (you won't see any characters as you type). Press enter.

Well done — you're now "inside" the Raspberry Pi.

## Step 11 — Install FTW

Copy this line EXACTLY as it is:

```
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
```

Paste it into the terminal or PuTTY (right-click usually pastes) and press enter.

Type your password one more time. Everything installs now. It takes a few minutes. **Then you're done.**

## All done

Open the browser on your regular computer and go to the web interface:

```
http://ftw.local:8080/
```

If that address doesn't work — try the IP address you wrote down, e.g. `http://192.168.1.123:8080/`.

---

## If something isn't working

- **The light isn't on at all** → check that the power adapter is plugged in.
- **Can't find the IP address** → restart the router, wait 5 minutes, look again.
- **SSH says "Connection refused"** → wait a little longer. The first boot takes time.
- **None of this helps** → drop by our Discord and kindly ask for help: **https://discord.gg/25xcBzQaux**
