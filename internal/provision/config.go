package provision

import (
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DataDir          string
	AppDir           string
	HTTP             string
	PublicURL        string
	AdminEmail       string
	AdminPassword    string
	EncryptionKey    string
	Dev              bool
	SettingsJSON     string
	OAuthSecretsJSON string
}

// Secret accepts either a value or its Docker/Kubernetes _FILE convention.
// Errors deliberately identify only the setting, never the secret or file path.
func Secret(name string) (string, error) {
	value, set := os.LookupEnv(name)
	path, fileSet := os.LookupEnv(name + "_FILE")
	if set && fileSet {
		return "", fmt.Errorf("set only %s or %s_FILE", name, name)
	}
	if !fileSet {
		return value, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s_FILE", name)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return "", fmt.Errorf("invalid %s_FILE", name)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

func Load() (Config, error) {
	c := Config{DataDir: "./bb_data", AppDir: "./app", HTTP: "127.0.0.1:8090"}
	for key, dest := range map[string]*string{
		"BIRALOBASE_DATA_DIR": &c.DataDir, "BIRALOBASE_APP_DIR": &c.AppDir,
		"BIRALOBASE_HTTP": &c.HTTP, "BIRALOBASE_PUBLIC_URL": &c.PublicURL,
		"BIRALOBASE_ADMIN_EMAIL": &c.AdminEmail, "BIRALOBASE_ADMIN_PASSWORD": &c.AdminPassword,
		"BIRALOBASE_ENCRYPTION_KEY": &c.EncryptionKey, "BIRALOBASE_SETTINGS_JSON": &c.SettingsJSON,
		"BIRALOBASE_OAUTH_SECRETS_JSON": &c.OAuthSecretsJSON,
	} {
		if _, ok := os.LookupEnv(key); !ok {
			if _, ok = os.LookupEnv(key + "_FILE"); !ok {
				continue
			}
		}
		v, err := Secret(key)
		if err != nil {
			return c, err
		}
		*dest = v
	}
	if raw, ok := os.LookupEnv("BIRALOBASE_DEV"); ok {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return c, errors.New("invalid BIRALOBASE_DEV")
		}
		c.Dev = v
	}
	if c.DataDir == "" || c.AppDir == "" || c.HTTP == "" {
		return c, errors.New("data directory, application directory, and HTTP address must not be empty")
	}
	return c, nil
}

func (c Config) ValidateServer() error {
	if c.AdminEmail == "" && c.AdminPassword == "" && c.Dev {
		return nil
	}
	email, err := mail.ParseAddress(c.AdminEmail)
	if err != nil || email.Address != c.AdminEmail {
		return errors.New("BIRALOBASE_ADMIN_EMAIL must be a valid email address")
	}
	if len(c.AdminPassword) < 12 {
		return errors.New("BIRALOBASE_ADMIN_PASSWORD must contain at least 12 characters")
	}
	if !c.Dev {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
			return errors.New("BIRALOBASE_PUBLIC_URL must be an HTTPS origin in production")
		}
		if len(c.EncryptionKey) != 32 {
			return errors.New("BIRALOBASE_ENCRYPTION_KEY must contain exactly 32 bytes in production")
		}
	}
	return nil
}
