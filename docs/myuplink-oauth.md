# MyUplink OAuth setup

`drivers/myuplink.lua` reads heat-pump telemetry from MyUplink. It is
read-only: the driver does not call control endpoints.

## Register the app

1. Open <https://dev.myuplink.com> and create an application.
2. In FTW, add the MyUplink driver under **Settings → Devices**.
3. Copy the callback URL shown by FTW into the portal exactly.
4. Copy the portal's Client Identifier and Client Secret into FTW and save.

The callback must be reachable by the browser after consent. If MyUplink
requires HTTPS, use an operator-managed HTTPS/private-network address and open
FTW through that same origin before starting the connection.

MyUplink may require `WRITESYSTEM READSYSTEM offline_access` for the consent
page even though this driver only reads. A portal that accepts a narrower grant
can use `oauth_scope: "READSYSTEM offline_access"`.

## Connect

Click **Connect to MyUplink**, sign in and grant access. Normally the browser
returns to FTW, which exchanges the code, stores the refresh token and restarts
the driver.

If the browser cannot reach the callback, copy the full redirected URL
containing `code` and `state`, paste it into the fallback field in Settings
and click **Complete connection**. FTW then performs the exchange through its
outbound HTTPS connection. The state is single-use and expires after 15
minutes.

Refresh tokens are stored as masked config secrets. MyUplink rotates them; the
driver persists the replacement through `host.persist_secret`.

## Troubleshooting

| Symptom | Action |
|---|---|
| `invalid_client` | Recheck the saved Client Identifier and Secret. |
| Authorize page says `invalid_request` | Match the callback URL exactly and use the default scope. |
| FTW reports invalid state | Start Connect again and finish within 15 minutes from the same origin. |
| Badge remains disconnected | Reload Settings after completing consent. |
| `awaiting OAuth connect` | Credentials exist but browser consent is incomplete. |
| No heat-pump card | Wait for the first poll, then verify parameter IDs in the driver header. |

Common NIBE defaults and per-model override names live next to the mapping in
`drivers/myuplink.lua`; that file is the reference.
