# Pair with one iPhone

Install FTW Energy from the
[App Store](https://apps.apple.com/app/ftw-energy/id6822024949) first.

On the home Wi-Fi, open the box's dashboard in Safari and choose
**Settings → FTW app**.

The app reaches the box through FTW Cloud, which keeps a readable copy of
your energy data while it is on. The first code you make accepts the
[FTW Cloud terms](https://ftw.sourceful.energy/privacy), as pairing does in
the app. Until then, a line beside the buttons says so. The box runs
without Cloud. Once Cloud is on, the same page offers **Turn FTW Cloud
off**, below the pairing. The copy has no switch of its own: turning
Cloud off is how you stop it.

Choose one way to connect:

- **Show pairing code → Open in FTW on this phone.** The app shows which
  box the link names before you connect it. If Safari keeps the link in the
  browser, copy the pairing link and paste it under **Enter pairing code**
  in FTW.
- **Use a pairing code.** In FTW, choose **Enter pairing code**. Enter the
  eight characters and the box address shown beside them. **Copy pairing
  code and address** copies both for the app's Paste button. If the box has
  a house password, enter it when the app asks.
- Take a screenshot of the QR and choose **Choose a picture of the QR**
  in FTW. The app reads the code from that image.

The page also shows the box address beside the QR: the address this page
uses.

Inside the Home Assistant app, the page offers the QR and the pairing link
only. Both go through FTW Cloud. The box address and the short code need
the box's own port, which Home Assistant keeps closed.

The pairing buttons work while the app link runs, which it does on a new
box. If the page shows **Let the FTW app connect to this box**, the link is
off: turn it on, choose **Save and restart FTW** and wait for the page to
return. FTW pauses energy control while it restarts. While the link runs,
**Turn off the app link…** below the pairing stops it the same way.

A short code works once for five minutes. Five wrong tries cancel it.
Making a new code cancels the previous code. The QR and link keep their
existing expiry and single-use checks.

A code from this page first sends `POST /api/cloud/grant` with the current
Cloud scopes and terms when `GET /api/cloud` reports them not yet accepted.
The box applies the same checks as for the Cloud panel. If it refuses, the
page shows why and makes no code.
