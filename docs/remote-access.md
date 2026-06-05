# Reaching your home from anywhere

> A plain-language guide for owners. If you've just set up your
> forty-two-watts box (see [`setup-guide/`](setup-guide/)) and now want to
> open it from your phone on the train, this is for you.

## The short version

You get **one address**: `https://home.fortytwowatts.com`.

Open it from any browser, anywhere. Sign in with your **passkey** (the same
Face ID / Touch ID / fingerprint you already use to unlock your phone). You're
looking at your own home energy system — live.

That's it. No app to install, no port forwarding, no VPN, no account to create,
no password to remember.

---

## The one idea worth understanding

**Your data goes straight from your browser to your box at home. Nobody sits in
the middle.**

It helps to picture it. There is a small public server — we call it the
**relay** — at `home.fortytwowatts.com`. Think of it as a switchboard operator
from an old telephone exchange. When you open the address, the operator's only
job is to help your browser and your home box *find each other* and shake hands.
The moment they're connected, the operator steps away. Your actual data — every
watt, every price, every battery number — travels on a **direct, end-to-end
encrypted line** between your browser and your box.

Three things follow from that, and they're the whole security story:

1. **The relay never sees your data.** It carries the handshake, not the
   conversation. Even we, running the relay, can't read what flows between you
   and your home.
2. **The relay can't reach into your home.** It has no door into your network.
   It can't poll your box, can't read a file, can't ask it anything. An
   anonymous visitor who lands on the address just gets the locked sign-in
   screen, served by the relay itself — your box is never even contacted.
3. **No data moves until you prove it's you.** The passkey is checked on the
   direct line, by your own box. Until that check passes, nothing about your
   home is sent. There is no path around it.

A passkey can't be phished, reused, or leaked in a database breach — it's a
private key that never leaves your device. So the lock on your home is as strong
as the lock on your phone.

---

## First time: do this once, on your home Wi-Fi

The first enrollment happens **at home, on the same network as your box.** That's
deliberate — it means the very first trust is established locally, where no
stranger can be involved.

1. At home, open your box's dashboard the way you normally do (e.g.
   `http://forty-two-watts.local` or its IP address).
2. Find **Sign in / Add passkey** on the landing.
3. Create a passkey. Your browser will offer Face ID / Touch ID / a fingerprint
   / a security key — pick whatever you use. Confirm.

Done. Your phone (or laptop) is now a key to your home.

> **Tip:** enroll on each device you'll actually use — your phone *and* your
> laptop, say. Each gets its own passkey. You can add or remove them any time
> from the same screen, and removing one instantly locks that device out.

---

## From then on: anywhere

1. Open **`https://home.fortytwowatts.com`**.
2. You'll see a clean sign-in card — *"Reaching your home…"* then *"Sign in with
   your passkey."*
3. Tap it, confirm with Face ID / Touch ID, and your dashboard appears.

When you're at home on your own Wi-Fi, it skips the sign-in entirely and just
shows the dashboard — the local network is already trusted.

### What the little status word means

Near the top you'll see one of:

- **Direct** — your browser and box found a straight line to each other. Fastest,
  fully private. This is the normal case.
- **Relayed** — a straight line wasn't possible (some strict networks block it),
  so the encrypted line is tunnelled through the relay. Still end-to-end
  encrypted — the relay still can't read it — just a little slower.
- **Connecting** — the handshake is in progress. It usually clears in a second.

It's just there to tell you what's happening. You don't need to do anything with
it.

---

## "Is this actually safe?" — yes, and here's the honest state

Everything in *The one idea* above is live today: direct peer-to-peer,
end-to-end encryption, a relay that's blind by design, and no data without your
passkey. That's the part that protects you, and it's on.

We're still rolling out one **convenience** layer on top:

- **"Remember this device"** so a returning phone signs you in without the Face
  ID prompt every single time. Until that lands, just sign in with your passkey
  on each visit — a two-second tap. The security is identical either way; this
  only saves taps.

We'd rather ship the protection first and the polish second, and tell you
plainly which is which.

---

## Letting a friend look (optional)

Remote access above is **for you** — your devices, your passkeys. If you want to
let a friend or installer see or help with your system, that's a *separate,
deliberate* flow with its own consent step — see
[`ftw-pair.md`](ftw-pair.md). Granting a friend is a real act of trust (it gives
them broad access on purpose), so it never happens by accident and never by a
stranger.

---

## If something's off

- **"…is offline" page.** Your box isn't reachable right now — usually it's
  powered down, lost internet, or still booting. It reconnects on its own; the
  page has a retry button. Nothing to fix on your end.
- **The sign-in won't take my passkey.** Make sure you're using a device you
  enrolled (step *First time*). Enrolled on your phone but trying from a borrowed
  laptop? That laptop has no key — enroll it from home first, or use your phone.
- **It says "Relayed" and feels slow.** Harmless — it's still encrypted
  end-to-end. Strict mobile networks sometimes force this; it often flips back to
  *Direct* on Wi-Fi.
- **I'm at home but it asked me to sign in.** Your browser may be on a guest
  VLAN or mobile data rather than the home Wi-Fi. Signing in with your passkey
  works regardless.

---

## In one sentence

`home.fortytwowatts.com` + your passkey = your home, from anywhere, on a private
line nobody else can open or listen to.
