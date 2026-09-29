---
name: switching-ftw-deploy-mode
description: Use when the user wants to switch an FTW host between a release and a development build, or asks how to update an existing host. Identify native release slots, new Docker packaging and retired Docker before choosing a path. Never use old Docker latest as a route to current FTW.
---

# Choose the FTW deployment and update path

Read [Install and update FTW](../../../docs/native-beta.md) before changing a
host. It is the common guide for people and agents. 2.x and 3.x receive no
more updates. All new releases use the new 0.x line, starting with
`v0.131.0-beta.1`; old 0.x through 0.130.x belongs to the retired line too.
Do not install 3.x beta as an intermediate step.

## Identify the host and layout

Use the host authorized for this task. Record its running version, native unit
or Compose project and files, data paths, image or binary, overrides and start
rules. `ftw.service`, a working dashboard and `ftw status` do not identify the
layout by themselves. A diagnosis stays read-only. An authorized update needs
no repeat approval for the same action.

| Installed layout | Action |
|---|---|
| New native with launcher and release slots | Follow the native update section: `ftw status`, `ftw update --channel beta`, then verify. Refresh launcher/CLI/unit only when the release requires it. |
| New Docker from a release package | Keep the project and data, set the exact `FTW_VERSION` in its existing `.env`, then `docker compose up -d --build`. Native update/rollback commands do not apply. |
| Old Docker, Pi image or Home Assistant app | Follow the switch guide. Use a separate new setup and data; guided transfer of old data is not ready. |
| Older direct native or custom development build | Inspect the actual binary and override first. Use the guide's older-native path or a checked recovery plan for that site. Do not infer a safe binary swap from the service name. |

## Returning from a development build

Preserve the current config, data and override before changing the running
process. Check that the chosen release can read those data. Use the update or
recovery steps for the identified layout, within the owner's authorized scope.
If the development build changed the data format, establish its recovery path
before replacing it.

The old container distribution, `ghcr.io/srcfl/ftw` and its compatibility
mirror, is retired. Do not pull `:latest` or restore a saved Compose override
to obtain new 0.x. The
[previous skill](https://github.com/srcfl/ftw/blob/5d811a937b6085df7d5494022c615d1f611198fb/.agents/skills/switching-ftw-deploy-mode/SKILL.md)
is historical context, not a current host recipe.

For local development with separate test data, use
[the development guide](../../../docs/development.md).

## Verify the result

Follow the common guide's checks: expected running release or revision,
healthy devices, advancing measurements, plan, history writes and recovery.
Confirm that only one Core controls the equipment and that old Core/updater
start rules cannot bring another instance back. Preserve required MQTT.
Repeat after a planned reboot; report any checks not done. A successful
Compose command or one visible dashboard does not prove a complete switch.
