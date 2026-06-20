# MyUplink heat-pump setup (NIBE, Bosch, Atlantic, Daikin, …)

The `myuplink` driver reads heat-pump telemetry — compressor power and
hot-water / indoor / outdoor temperatures — from the MyUplink Cloud API. It is
**read-only**: it observes the pump, it cannot control it.

> There is **no `mode` field**. One physical pump = one driver. The driver does
> not split into hot-water / heating instances. (Earlier troubleshooting advice
> that mentioned `mode: hotwater | heating` was wrong — ignore it.)

## Why there's a "Connect" step

MyUplink's developer portal issues **authorization-code** apps: you register a
*Callback URL*, get a *Client Identifier* and a *Client Secret*, and the user
grants access through a browser sign-in. It does **not** support the
`client_credentials` grant — an app of that kind fails at startup with:

```
MyUplink: token request failed: HTTP 400: {"error":"invalid_client", ...}
```

So 42-watts does a one-time browser consent for you and stores the resulting
refresh token. After that the driver refreshes silently; you never paste a
token by hand. (Background: issue #496.)

## Step 1 — register an application

1. Go to the MyUplink **developer web portal**: <https://dev.myuplink.com> →
   **Applications** → create an application. (This is the *web* portal — not the
   iOS/Android MyUplink app, which will not show a 42-watts client.)
2. In 42-watts, open **Settings → Devices**, add the **MyUplink** driver from
   the catalogue, and copy the **Callback URL** it shows you (it looks like
   `http://<your-42w-address>/api/oauth/myuplink/callback`).
3. Paste that exact string into the portal's **Callback Url** field. It must
   match the address you use to reach 42-watts, character for character.
4. Save the portal app and copy its **Client Identifier** and **Client
   Secret**.

> **Callback URL must be reachable by your browser after sign-in.** If you reach
> 42-watts over plain `http://<lan-ip>:8080` and MyUplink rejects a non-HTTPS
> callback, use an HTTPS address instead — e.g. your relay URL
> (`https://…`) — and register *that* as the callback. Whatever address you
> register is the one you must be on when you click Connect.

## Step 2 — enter credentials in 42-watts

In **Settings → Devices**, on the MyUplink driver:

- **Client ID** → paste the *Client Identifier*.
- **Secrets → Client Secret** → paste the *Client Secret*.
- **Save** the settings (the Connect button reads the *saved* Client ID).

## Step 3 — connect

Click **Connect to MyUplink**. A new tab opens MyUplink's sign-in; log in and
grant access. You're redirected back to 42-watts, which exchanges the code for a
refresh token, stores it, and restarts the driver. Return to Settings and
**reload** — the badge flips to **✓ Connected**.

Within a minute the **Heat pump** card appears on the dashboard with live
compressor power and temperatures, plus a 24-hour power sparkline.

## How the token is kept fresh

- The refresh token is stored as a masked `config_secret` (shown as "saved",
  never echoed back to the browser).
- At runtime the driver exchanges it for short-lived access tokens
  (`grant_type=refresh_token`).
- MyUplink (Azure B2C) rotates the refresh token on each refresh; the driver
  persists the rotated value via `host.persist_secret` into the state database,
  so it survives restarts without you reconnecting.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `invalid_client` at startup | Old `client_credentials` build, or wrong Client ID/Secret. Update 42-watts and re-enter the credentials. |
| Connect button: "save the Client ID first" | Save the settings before clicking Connect — `/start` reads the *saved* Client ID. |
| Redirected to a "connection failed — invalid state" page | The consent took longer than 10 minutes, or you started Connect from a different address than you finished on. Start again from the address you registered as the callback. |
| Badge stays "Not connected" after consent | Reload the Settings page; the badge reflects the last config load. |
| "awaiting OAuth connect" in the logs | Credentials saved but consent not completed — click Connect. |
| Card never appears | The pump has not reported `hp_power_w` yet (first poll is ~1 min), or the parameter IDs don't match your model — override `param_power_id` etc. in config (see the driver header). |

## Parameter IDs

The defaults target common NIBE models (`10012` compressor power, `40013` BT6
hot-water top, `40033` BT50 indoor, `40004` BT1 outdoor). If your model differs,
list your device's points via `GET /v2/devices/{deviceId}/points` and override
`param_power_id`, `param_hw_temp_id`, `param_indoor_temp_id`,
`param_outdoor_temp_id` in the driver config.
