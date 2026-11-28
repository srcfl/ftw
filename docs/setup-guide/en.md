# Getting FTW up and running

This guide is for you if you've never set up a Raspberry Pi before. Relax — it's easier than it sounds. Just follow the steps, one at a time.

> **Have you run FTW before?** [Start here](../before.md) instead.

> **Running Home Assistant OS?** You need no Raspberry Pi: [install the FTW app](../install.md#home-assistant) instead.

> **Another Linux computer?** A 64-bit Debian or Ubuntu computer uses the same line. Skip the Pi steps and go to **Step 11 — Install FTW**.

## What you'll need

- A **Raspberry Pi 3 or newer**
- A **memory card** (microSD)
- A **power adapter** for the Pi
- A **case** — optional
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

- The model you have: **Raspberry Pi 3**, **4** or **5**.

![](images/pi-imager-step1.png)

## Step 4 — Choose the software

Click the middle box ("Operating System"):

- Choose **Raspberry Pi OS (other)**
- Then choose **Raspberry Pi OS Lite (64-bit)**

This is the base program that makes the Raspberry Pi work — a bit like Windows on your regular computer.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Step 5 — Put the memory card in your computer

1. Choose a memory card. Everything on it will be erased.
2. Slide it into the card reader on your regular computer.
3. Click the bottom box ("Storage") in the program and select your card.

## Step 6 — Customize the settings

Click **Next**. The program asks whether you want to customize settings — say **Yes** (or "Edit settings").

Fill in:

- **Hostname:** `ftw`
- **Localization:** choose your country
- **Username:** e.g. `pi` (not `ftw`: FTW creates its own account with that name)
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

Copy this line. Paste it into the window from Step 10 and press enter:

```bash
curl -fsSL https://ftw.energy/install.sh | bash -s -- --fresh-host
```

(In PuTTY, a right click pastes. On a Mac, press `Cmd`+`V`.)

FTW downloads, checks itself and starts. If it asks for a password, type the one you chose in Step 6. Wait until it's done. At the end it shows the address of your FTW, something like this:

```text
  http://192.168.1.123:8080/setup
  http://ftw.local:8080/setup
```

If it shows an error instead, stop there and ask in Discord (the link is at the bottom). Don't try anything else first.

## After installation

Open the browser on your regular computer or your phone and go to one of those addresses. If one doesn't work, try the other.

The setup page asks for three things:

- **Your home:** the country, the price zone and the size of your main fuse, for example 16, 20 or 25 A. Your grid contract says it.
- **Your devices:** FTW looks for them on your network. Pick each one and add it.
- **A house password:** other devices on your Wi-Fi need it to change settings. Pick something you'll remember.

Check the summary and click **Start FTW**. You'll land on the dashboard. Well done!

---

## If something isn't working

- **The light isn't on at all** → check that the power adapter is plugged in.
- **Can't find the IP address** → restart the router, wait 5 minutes, look again.
- **SSH says "Connection refused"** → wait a little longer. The first boot takes time.
- **The page opens, but changing settings shows "remote access to protected API routes is disabled"** → FTW only accepts changes from addresses it recognizes as local. Open the UI through the Pi's IP address, e.g. `http://192.168.1.123:8080`. Why this happens — even at home — is explained in [operations.md](../operations.md#remote-access-to-protected-api-routes-is-disabled).
- **None of this helps** → drop by our Discord and kindly ask for help: **https://discord.gg/UK2ygPBu8N**
