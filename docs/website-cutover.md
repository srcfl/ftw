# FTW website cutover

This runbook moves the public project website from `fortytwowatts.com` to
`ftw.sourceful.energy`. It does not migrate owner access, WebAuthn credentials,
the relay, or TURN.

## Safety boundary

Only these website hosts are in scope:

- `fortytwowatts.com`
- `www.fortytwowatts.com`
- `ftw.sourceful.energy`

Do not redirect, delete, or change the security identity of:

- `home.fortytwowatts.com`
- `relay.fortytwowatts.com`
- any other `fortytwowatts.com` subdomain

`home.fortytwowatts.com` remains the WebAuthn RP ID until the separate passkey
migration is designed, rolled out, and observed. A redirect rule matching
`*.fortytwowatts.com` is therefore unsafe.

## Observed state on 2026-07-17

- `fortytwowatts.com` and `www.fortytwowatts.com` resolve through Cloudflare.
- The legacy public website returns `200` and is served through AWS
  Amplify/CloudFront.
- `home.fortytwowatts.com` and `relay.fortytwowatts.com` still resolve through
  Cloudflare.
- The public website repository has been transferred to `srcfl/ftw-web`.
- Cloudflare Pages project `ftw` is Git-integrated with `srcfl/ftw-web`; its
  production branch is `main` and the production deployment is active at
  `ftw-613.pages.dev`.
- `ftw.sourceful.energy` has been registered as a Pages custom domain, but is
  pending because the required CNAME record has not yet been created.
- `ftw.sourceful.energy`, `relay.ftw.sourceful.energy`, and
  `turn.ftw.sourceful.energy` do not currently resolve.

Re-run every observation immediately before cutover. DNS and cloud state can
change independently of this repository.

## Phase 1 — publish the canonical site

The repository transfer, product-story merge, Git-integrated Pages project,
and production deployment are complete.

1. In the `sourceful.energy` Cloudflare zone, create this DNS record:

   | Type | Name | Target | Proxy |
   |---|---|---|---|
   | CNAME | `ftw` | `ftw-613.pages.dev` | Proxied |

   The Pages custom-domain association must exist before creating the CNAME;
   it already exists for `ftw.sourceful.energy`.
2. Wait until the Pages custom domain reports `active` and verify the issued
   certificate covers `ftw.sourceful.energy`.
3. Verify the canonical site before creating any redirect from the legacy
   domain.

Cloudflare documents Git-connected Pages projects and custom domains here:

- <https://developers.cloudflare.com/pages/get-started/git-integration/>
- <https://developers.cloudflare.com/pages/configuration/custom-domains/>

## Phase 2 — verify the canonical site

Do not redirect the legacy site until all of these pass:

```bash
curl -fsSIL https://ftw.sourceful.energy/
curl -fsSL https://ftw.sourceful.energy/ | rg \
  'FTW|Apache-2.0|Community support only|Sourceful Energy'
```

Verify in a browser on desktop and mobile:

- canonical URL, title, description, Open Graph, Twitter, and JSON-LD use
  `ftw.sourceful.energy` and `srcfl/ftw`;
- the page contains no active Forty Two Watts, 42W, `frahlg`, or MIT project
  claims;
- installation commands use `srcfl/ftw`;
- community support is explicitly best-effort with no SLA;
- the commercial call to action sells a supported outcome, not permission to
  use Apache-2.0 code;
- the visual language follows <https://design.sourceful.energy/DESIGN.md>;
- internal anchors and the GitHub, Discord, support, license, trademark, and
  Sourceful contact links work;
- no browser console errors or horizontal mobile overflow are present.

## Phase 3 — redirect only the legacy website hosts

Create two Cloudflare Single Redirect rules. Do not use an include-subdomains
option and do not use a wildcard hostname to the left of
`fortytwowatts.com`.

### Apex rule

- Request wildcard: `http*://fortytwowatts.com/*`
- Target wildcard: `https://ftw.sourceful.energy/${2}`
- Status: `301`
- Preserve query string: enabled

### WWW rule

- Request wildcard: `http*://www.fortytwowatts.com/*`
- Target wildcard: `https://ftw.sourceful.energy/${2}`
- Status: `301`
- Preserve query string: enabled

Cloudflare documents this exact path- and query-preserving hostname pattern at
<https://developers.cloudflare.com/rules/url-forwarding/examples/redirect-all-different-hostname/>.

If Cloudflare offers a combined custom-expression rule instead, its match must
still be equivalent to:

```text
http.host in {"fortytwowatts.com" "www.fortytwowatts.com"}
```

It must not match `home.fortytwowatts.com` or `relay.fortytwowatts.com`.

## Phase 4 — cutover verification

```bash
curl -sSI https://fortytwowatts.com/
curl -sSI 'https://fortytwowatts.com/docs/example?from=legacy'
curl -sSI https://www.fortytwowatts.com/
curl -sSI https://home.fortytwowatts.com/
curl -sSI https://relay.fortytwowatts.com/
```

Expected results:

- the first three requests return one permanent redirect to the canonical FTW
  host, preserving path and query;
- `home.fortytwowatts.com` and `relay.fortytwowatts.com` do not redirect to the
  marketing site;
- `ftw.sourceful.energy` returns the canonical page directly without a loop;
- TLS is valid on every retained host.

After DNS caches have settled, submit the new canonical URL and sitemap to the
search consoles in use and update external project profiles that still point at
the legacy apex.

## Rollback

1. Disable the two Cloudflare Single Redirect rules.
2. Confirm `fortytwowatts.com` and `www.fortytwowatts.com` once again return the
   previous Amplify deployment.
3. Leave the `ftw.sourceful.energy` Pages custom domain online while diagnosing
   it unless it is itself causing the incident.
4. Do not alter `home.fortytwowatts.com` as part of website rollback.

Keep the previous Amplify deployment available until the canonical site and
redirects have been stable through at least one DNS TTL and one normal release
cycle. After that observation period, delete the legacy Amplify domain
association and app as a separate cleanup step.
