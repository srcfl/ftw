# Pair with one iPhone

Install FTW Energy from the
[App Store](https://apps.apple.com/app/ftw-energy/id6822024949) first.

On the home Wi-Fi, open the box's dashboard in Safari and choose
**Settings → FTW app**. Turn on **Let the FTW app connect to this box** if
needed. Choose **Save and restart FTW** and wait for the page to return.
FTW pauses energy control while it restarts.

Choose one way to connect:

- **Show pairing code → Open in FTW on this iPhone.** The app shows which
  box the link names before you connect it. If Safari keeps the link in the
  browser, copy the pairing link and paste it under **Enter pairing code**
  in FTW.
- **Use a pairing code.** In FTW, choose **Enter pairing code**. Enter the
  eight characters and the box address shown beside them. **Copy pairing
  code and address** copies both for the app's Paste button. If the box has
  a house password, enter it when the app asks.
- Take a screenshot of the QR and choose **Choose a picture of the QR**
  in FTW. The app reads the code from that image.

A short code works once for five minutes. Five wrong tries cancel it.
Making a new code cancels the previous code. The QR and link keep their
existing expiry and single-use checks.

The short-code exchange runs directly against the box's local, protected
`POST /api/app-link/pairing` with `{"kind":"claim","code":"…"}`. It keeps
the code's role and expiry and replaces it with a full enrollment token.
The response carries the same v3 link as the QR. No short-code hash or box
secret goes through a cloud lookup. Remote clients, proxies and app sessions
cannot use this exchange. The existing house-password and browser-origin
checks still apply.
