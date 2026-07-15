package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/nova"
	"github.com/srcfl/ftw/go/internal/state"
)

// runNovaClaim is the entry point for `ftw nova-claim`.
//
// The subcommand orchestrates the three one-time actions needed to
// opt this site into Nova Core federation:
//
//  1. Generate (or load) an ES256 keypair at cfg.Nova.KeyPath.
//  2. POST /gateways/claim — register the pubkey under an org, using a
//     human operator's Bearer JWT and a signed proof-of-possession.
//  3. POST /devices/provision — for each ftw driver that
//     has emitted telemetry, create a Device + its DERs, receive the
//     Nova-assigned der_id's, and persist them in state.nova_ders for
//     the telemetry publisher to consult.
//
// Step 2 can be skipped with --reconcile for operators who already
// claimed the gateway and just want to re-provision after adding a
// new driver.
//
// The operator JWT is read from --token or the NOVA_OPERATOR_JWT env
// var. It is NEVER persisted — once claim + provision complete, the
// runtime only needs the gateway's ES256 private key (signed JWT is
// the MQTT password) and the Nova config block in config.yaml.
func runNovaClaim(args []string) {
	fs := flag.NewFlagSet("nova-claim", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to config.yaml")
	url := fs.String("url", "", "Nova core-api base URL (e.g. https://core.sourceful.energy)")
	orgID := fs.String("org", "", "Nova organization ID (org-...)")
	siteID := fs.String("site", "", "Nova site ID (sit-...)")
	claimerID := fs.String("claimer", "", "Human identity ID (idt-...) that will own the claim — must match the identity that issued --token")
	serial := fs.String("serial", "", "Gateway serial (optional — auto-generated as f42w-<hex> if empty)")
	mqttHost := fs.String("mqtt-host", "", "Nova MQTT broker host (defaults to the host portion of --url)")
	mqttPort := fs.Int("mqtt-port", 1883, "Nova MQTT broker port")
	mqttTLS := fs.Bool("mqtt-tls", false, "Use TLS for the MQTT connection")
	reconcile := fs.Bool("reconcile", false, "Skip /gateways/claim (already done) and only (re-)provision devices")
	token := fs.String("token", "", "Operator JWT (fallback: NOVA_OPERATOR_JWT env var)")
	_ = fs.Parse(args)

	if *token == "" {
		*token = os.Getenv("NOVA_OPERATOR_JWT")
	}

	if err := claimAndProvision(*configPath, *url, *mqttHost, *mqttPort, *mqttTLS,
		*orgID, *siteID, *claimerID, *serial, *token, *reconcile); err != nil {
		slog.Error("nova-claim failed", "err", err)
		os.Exit(1)
	}
}

func claimAndProvision(
	configPath, novaURL, mqttHost string, mqttPort int, mqttTLS bool,
	orgID, siteID, claimerID, gatewaySerial, token string, reconcile bool,
) error {
	// --- Validate args ---
	if novaURL == "" {
		return errors.New("--url is required")
	}
	if siteID == "" {
		return errors.New("--site is required")
	}
	if !reconcile {
		if orgID == "" {
			return errors.New("--org is required (unless --reconcile)")
		}
		if claimerID == "" {
			return errors.New("--claimer is required (unless --reconcile)")
		}
		if token == "" {
			return errors.New("operator JWT required — pass --token or set NOVA_OPERATOR_JWT")
		}
	} else if token == "" {
		return errors.New("operator JWT required for --reconcile provisioning")
	}

	// --- Load or bootstrap config ---
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Nova == nil {
		cfg.Nova = &config.Nova{}
	}
	if gatewaySerial == "" {
		if cfg.Nova.GatewaySerial != "" {
			gatewaySerial = cfg.Nova.GatewaySerial
		} else {
			buf := make([]byte, 6)
			_, _ = rand.Read(buf)
			gatewaySerial = "f42w-" + hex.EncodeToString(buf)
		}
	}

	// --- Resolve state + key paths ---
	statePath := "state.db"
	if cfg.State != nil && cfg.State.Path != "" {
		statePath = cfg.State.Path
	}
	keyPath := cfg.Nova.KeyPath
	if keyPath == "" {
		keyPath = filepath.Join(filepath.Dir(statePath), "nova.key")
	}

	id, err := nova.LoadOrCreateIdentity(keyPath)
	if err != nil {
		return fmt.Errorf("key material: %w", err)
	}
	slog.Info("nova identity ready", "pubkey_hex", id.PublicKeyHex(), "key_path", keyPath)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := nova.NewClient(novaURL, token)

	// --- Step 1: claim (skip if --reconcile) ---
	if !reconcile {
		nonce := randNonce()
		msg := nova.BuildClaimMessage(claimerID, nonce, gatewaySerial, time.Now())
		sig, err := id.SignRawHex(msg)
		if err != nil {
			return fmt.Errorf("sign claim: %w", err)
		}
		if err := client.Claim(ctx, nova.ClaimRequest{
			GatewaySerial: gatewaySerial,
			OrgID:         orgID,
			Signature:     sig,
			Message:       msg,
			PublicKey:     id.PublicKeyHex(),
		}); err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		slog.Info("gateway claimed", "serial", gatewaySerial, "org_id", orgID)
	}

	// --- Step 2: provision devices + DERs ---
	st, err := state.Open(statePath)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	defer st.Close()

	devices, err := st.AllDevices()
	if err != nil {
		return fmt.Errorf("list local devices: %w", err)
	}
	if len(devices) == 0 {
		slog.Warn("no devices registered yet — start ftw once so drivers can register, then re-run `ftw nova-claim --reconcile`")
	}

	for _, d := range devices {
		kinds := st.InferDerKinds(d.DriverName)
		if len(kinds) == 0 {
			slog.Warn("skip device (no telemetry emitted yet)",
				"driver", d.DriverName, "device_id", d.DeviceID)
			continue
		}
		ders := make([]nova.DERDefinition, 0, len(kinds))
		for _, k := range kinds {
			ders = append(ders, nova.DERDefinition{
				Name: nova.DerName(d.DriverName, k),
				Type: nova.TranslateDerTypeToLegacy(k),
			})
		}
		resp, err := client.Provision(ctx, nova.ProvisionRequest{
			GatewaySerial: gatewaySerial,
			HardwareID:    d.DeviceID,
			DeviceType:    nova.DeviceTypeFor(kinds),
			SiteID:        siteID,
			Manufacturer:  d.Make,
			DERs:          ders,
		})
		if err != nil {
			slog.Error("provision", "driver", d.DriverName, "err", err)
			continue
		}
		// Persist Nova's der_ids so the publisher can look them up.
		for _, created := range resp.DERs {
			kind := cleanKindFromLegacyName(created.Name, d.DriverName)
			if kind == "" {
				// Fallback: reverse the vocabulary mapping on Type.
				kind = cleanKindFromLegacyType(created.Type)
			}
			if kind == "" {
				slog.Warn("nova returned unrecognised DER name — skipping persist",
					"name", created.Name, "type", created.Type)
				continue
			}
			if err := st.UpsertNovaDER(state.NovaDER{
				DeviceID: d.DeviceID,
				DerType:  kind,
				DerName:  created.Name,
				DerID:    created.ID,
			}); err != nil {
				slog.Warn("persist nova der", "err", err)
			}
		}
		slog.Info("device provisioned",
			"device_id", d.DeviceID, "nova_device_id", resp.DeviceID,
			"der_count", len(resp.DERs))
	}

	// --- Step 3: persist nova config back to config.yaml ---
	cfg.Nova.Enabled = true
	cfg.Nova.URL = novaURL
	cfg.Nova.GatewaySerial = gatewaySerial
	cfg.Nova.OrgID = orgID
	cfg.Nova.SiteID = siteID
	cfg.Nova.KeyPath = keyPath
	if cfg.Nova.SchemaMode == "" {
		cfg.Nova.SchemaMode = "legacy"
	}
	if mqttHost != "" {
		cfg.Nova.MQTTHost = mqttHost
	} else if cfg.Nova.MQTTHost == "" {
		cfg.Nova.MQTTHost = defaultMQTTHostFromURL(novaURL)
	}
	if cfg.Nova.MQTTPort == 0 {
		cfg.Nova.MQTTPort = mqttPort
	}
	if mqttTLS {
		cfg.Nova.MQTTTLS = true
	}
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	slog.Info("nova config saved", "config", configPath, "serial", gatewaySerial)
	return nil
}

func randNonce() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// defaultMQTTHostFromURL extracts "host" from "scheme://host[:port]/..."
// as a convenience — operators running Nova's all-in-one stack have the
// same host for core-api and the MQTT broker.
func defaultMQTTHostFromURL(u string) string {
	// Strip scheme
	if i := indexOf(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	// Strip port/path
	for _, sep := range []string{"/", ":"} {
		if i := indexOf(u, sep); i >= 0 {
			u = u[:i]
		}
	}
	return u
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// cleanKindFromLegacyName reverses nova.DerName's "{driver}-{kind}"
// layout to recover the clean kind from the Nova-stored DER name.
func cleanKindFromLegacyName(derName, driverName string) string {
	prefix := driverName + "-"
	if len(derName) <= len(prefix) || derName[:len(prefix)] != prefix {
		return ""
	}
	return derName[len(prefix):]
}

// cleanKindFromLegacyType reverses Nova's vocabulary (solar/ev_port)
// back to ftw' clean kind (pv/ev/v2x_charger). Called as fallback when
// the DER name doesn't follow the "{driver}-{kind}" shape.
func cleanKindFromLegacyType(legacy string) string {
	switch legacy {
	case "solar":
		return "pv"
	case "ev_port":
		return "ev"
	case "battery", "meter", "v2x_charger":
		return legacy
	}
	return ""
}
