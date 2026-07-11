// playwright.config.ts — tier-2 container-side home-route test.
//
// One project, Chromium only (the CDP virtual WebAuthn authenticator and the
// container-to-container WebRTC path are Chromium features). The browser must
// resolve the relay's home host to the relay container on the docker bridge
// net, and must NOT use a proxy for it — both are wired via launch args here
// so the test file stays focused on the ceremony.
//
// HOME_ORIGIN (e.g. http://home.fortytwowatts.localhost:7378) is injected by
// the compose service; RELAY_HOST is the docker service name the home host
// resolves to.
import { defineConfig, devices } from "@playwright/test";

const HOME_ORIGIN = process.env.HOME_ORIGIN ?? "http://home.fortytwowatts.localhost:7378";
const HOME_HOST = new URL(HOME_ORIGIN).hostname; // home.fortytwowatts.localhost
const RELAY_HOST = process.env.RELAY_HOST ?? "relay"; // docker service name

export default defineConfig({
  testDir: "./tests",
  // The whole flow (relay register settle + passkey + ICE) fits well under this;
  // generous so a cold image pull / first boot doesn't flake.
  timeout: 120_000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: HOME_ORIGIN,
    // Map the home host to the relay container, and never route *.localhost
    // through a proxy. host-resolver-rules makes Chromium resolve the
    // WebAuthn-significant hostname (home.fortytwowatts.localhost) to the relay
    // service IP without touching /etc/hosts — so clientDataJSON.origin stays
    // the home host (RP-ID match) while the bytes go to the relay.
    launchOptions: {
      args: [
        `--host-resolver-rules=MAP ${HOME_HOST} ${RELAY_HOST}`,
        "--proxy-bypass-list=*",
        // Chromium can't use its setuid sandbox in an unprivileged container.
        "--no-sandbox",
        "--disable-dev-shm-usage",
      ],
    },
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
