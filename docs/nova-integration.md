# Nova Core federation

FTW can publish telemetry to Sourceful Nova Core. Federation is opt-in and a
strict read-side integration: disabling it or losing its network connection
does not change local telemetry, planning, control or safety.

This bridge is an engineering integration, not a public remote-access path.

## Boundary

`go/internal/nova` snapshots registered DER telemetry, maps it to the selected
Nova schema and publishes over MQTT. An ES256 identity signs short-lived MQTT
JWTs. Claim and provisioning use HTTPS.

FTW keeps its clean snake_case site convention internally. The default
`schema_mode: legacy` adapter translates Nova's deployed field names, DER
vocabulary and battery sign at this one boundary. `schema_mode: unified`
publishes the clean schema when the target Nova deployment supports it.

Only DER telemetry, registered device identity and the bounded driver inventory
are published. Operator credentials and FTW configuration are not payloads.

## Driver inventory

When Nova federation is enabled, FTW also publishes
`sourceful.driver-inventory/v1` on connect, after a loaded-driver change and at
least every 15 minutes. It reports driver ID, version, loaded source or package
hash, package channel, declared control class, instance counts and health.

The report does not contain instance or site names, driver config, connection
details, device IDs, tokens, logs, command inputs or vendor responses. Nova
uses the authenticated MQTT identity for gateway and organization ownership.
Fleet reports must show how many FTW gateways sent a fresh inventory; the first
beta counts do not cover FTW sites that have not enabled Nova federation.

## Claim and provision

Prerequisites are a Nova organization/site, a human operator JWT and identity
ID, and at least one locally registered device with telemetry.

```bash
export NOVA_OPERATOR_JWT=eyJhbGciOi...

./ftw nova-claim \
  --url=https://core.sourceful.energy \
  --org=org-... \
  --site=sit-... \
  --claimer=idt-...
```

The command creates or loads `nova.key`, proves key possession, provisions
the observed device/DER set, caches returned DER IDs and writes the `nova:`
configuration atomically. Restart core after the first claim.

After adding hardware, let it emit telemetry and reconcile:

```bash
./ftw nova-claim --reconcile \
  --url=https://core.sourceful.energy \
  --site=sit-...
```

The operator JWT authorizes the operation and is not persisted.

## Configuration

```yaml
nova:
  enabled: true
  url: https://core.sourceful.energy
  mqtt_host: broker.sourceful.energy
  mqtt_port: 1883
  mqtt_tls: false
  gateway_serial: fftw-abc123
  org_id: org-...
  site_id: sit-...
  key_path: /var/lib/ftw/nova.key
  schema_mode: legacy
  publish_interval_s: 5
```

The config types and adapter tests are the detailed schema reference.

## Troubleshooting

- `DER not provisioned`: run `nova-claim --reconcile` after telemetry exists.
- reconnect loop: verify clock, broker address/TLS and claimed gateway state;
  JWTs are short-lived and time-sensitive.
- published topic but no data: verify the device/DER tuple was provisioned.
- wrong battery sign: match `schema_mode` to the Nova deployment.
