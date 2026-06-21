package runtime

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Configuration and secret management. Facet is configured entirely from the
// environment (FACET_*), but a project commonly keeps local settings in a `.env`
// file; LoadDotEnv folds that file into the process environment (without
// overriding anything already set, so real env vars and CI secrets win). The
// Config type resolves and validates the settings the runtime depends on, and
// `facet config` reports them so an operator can see, at a glance, whether a
// deployment is wired correctly and safely.

// LoadDotEnv reads KEY=VALUE lines from a dotenv file and sets any variable not
// already present in the environment. Missing files are ignored (it is optional).
// Lines may be blank, `# comments`, or `KEY=VALUE` with optional surrounding
// quotes and a leading `export `.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}

// Config is the resolved runtime configuration.
type Config struct {
	DatabaseURL   string
	Secret        string
	SecureCookies bool
	Cluster       bool
	OIDC          bool
	UploadDir     string
	RateLimit     int
}

// ResolveConfig reads the current environment into a Config.
func ResolveConfig() Config {
	return Config{
		DatabaseURL:   os.Getenv("FACET_DATABASE_URL"),
		Secret:        os.Getenv("FACET_SECRET"),
		SecureCookies: os.Getenv("FACET_SECURE_COOKIES") == "1",
		Cluster:       clusterEnabled(),
		OIDC:          os.Getenv("FACET_OIDC_ISSUER") != "",
		UploadDir:     uploadDirFromEnv(),
		RateLimit:     rateLimitFromEnv(),
	}
}

// Warnings returns the production-readiness problems with this configuration:
// the things that are missing or unsafe. An empty slice means it is good to ship.
func (c Config) Warnings() []string {
	var w []string
	if c.DatabaseURL == "" {
		w = append(w, "FACET_DATABASE_URL is not set — `facet run` needs Postgres (only the dev tools work without it).")
	} else if !strings.HasPrefix(c.DatabaseURL, "postgres") {
		w = append(w, "FACET_DATABASE_URL is not a postgres:// URL.")
	}
	if c.Secret == "" {
		w = append(w, "FACET_SECRET is not set — cookies, MFA secrets and encrypted columns ride an ephemeral key that does not survive a restart. Set one with `facet config --gen-secret`.")
	} else if len(c.Secret) < 32 {
		w = append(w, "FACET_SECRET is short (<32 chars) — use at least 32 bytes of randomness.")
	}
	if !c.SecureCookies {
		w = append(w, "FACET_SECURE_COOKIES is off — set it to 1 behind TLS so session cookies are only sent over HTTPS.")
	}
	return w
}

// Report renders the resolved configuration and its warnings for `facet config`.
func (c Config) Report() string {
	var b strings.Builder
	set := func(name, val string) {
		if val == "" {
			fmt.Fprintf(&b, "  %-26s (not set)\n", name)
		} else {
			fmt.Fprintf(&b, "  %-26s %s\n", name, val)
		}
	}
	b.WriteString("configuration (from environment / .env):\n")
	set("FACET_DATABASE_URL", redactURL(c.DatabaseURL))
	set("FACET_SECRET", redact(c.Secret))
	set("FACET_SECURE_COOKIES", boolStr(c.SecureCookies))
	set("FACET_CLUSTER", boolStr(c.Cluster))
	set("FACET_OIDC_ISSUER", os.Getenv("FACET_OIDC_ISSUER"))
	set("FACET_UPLOAD_DIR", c.UploadDir)
	fmt.Fprintf(&b, "  %-26s %d req/s\n", "FACET_RATE_LIMIT", c.RateLimit)
	if signedMedia() {
		fmt.Fprintf(&b, "  %-26s signed, %ds TTL\n", "FACET_MEDIA_TTL", mediaTTL())
	} else {
		set("FACET_MEDIA_TTL", "(public links)")
	}

	w := c.Warnings()
	if len(w) == 0 {
		b.WriteString("\nready: configuration is complete and production-safe.\n")
	} else {
		b.WriteString("\nwarnings:\n")
		for _, line := range w {
			fmt.Fprintf(&b, "  ! %s\n", line)
		}
	}
	return b.String()
}

// GenerateSecret mints a fresh 32-byte hex secret suitable for FACET_SECRET.
func GenerateSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 6 {
		return "******"
	}
	return s[:3] + "…" + "(set)"
}

// redactURL hides the password in a Postgres URL while showing host/db.
func redactURL(u string) string {
	if u == "" {
		return ""
	}
	if at := strings.LastIndex(u, "@"); at >= 0 {
		if scheme := strings.Index(u, "://"); scheme >= 0 && scheme+3 < at {
			return u[:scheme+3] + "***@" + u[at+1:]
		}
	}
	return u
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
