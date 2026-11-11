package provision

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"

	"github.com/biralo-studio/biralobase/core"
)

// ApplySettings keeps environment credentials out of the application artifact.
// PocketBase encrypts persisted settings using EncryptionEnv configured by the
// launcher. Schema settings are merged first, then the secret environment layer.
func ApplySettings(app core.App, values map[string]any, secretJSON, publicURL string) error {
	settings, err := app.Settings().Clone()
	if err != nil {
		return err
	}
	if publicURL != "" {
		settings.Meta.AppURL = publicURL
	}
	if len(values) > 0 {
		raw, marshalErr := json.Marshal(values)
		if marshalErr != nil || jsonv2.Unmarshal(raw, settings) != nil {
			return errors.New("invalid schema settings")
		}
	}
	if secretJSON != "" {
		var values map[string]any
		if json.Unmarshal([]byte(secretJSON), &values) != nil {
			return errors.New("invalid BIRALOBASE_SETTINGS_JSON")
		}
		if jsonv2.Unmarshal([]byte(secretJSON), settings) != nil {
			return errors.New("invalid BIRALOBASE_SETTINGS_JSON settings")
		}
	}
	if len(values) == 0 && secretJSON == "" && publicURL == "" {
		return nil
	}
	if err = app.Save(settings); err != nil {
		return errors.New("configured settings failed validation")
	}
	return nil
}

// Collection secrets are keyed by stable collection ID, then provider name.
func ApplyOAuthSecrets(app core.App, raw string) error {
	if raw == "" {
		return nil
	}
	var values map[string]map[string]string
	if json.Unmarshal([]byte(raw), &values) != nil {
		return errors.New("invalid BIRALOBASE_OAUTH_SECRETS_JSON")
	}
	return app.RunInTransaction(func(tx core.App) error {
		for id, secrets := range values {
			c, err := tx.FindCollectionByNameOrId(id)
			if err != nil || c.Id != id || !c.IsAuth() || c.System {
				return errors.New("OAuth secrets require an application auth collection ID")
			}
			changed := false
			for name, secret := range secrets {
				found := false
				for i := range c.OAuth2.Providers {
					if c.OAuth2.Providers[i].Name == name {
						found = true
						if c.OAuth2.Providers[i].ClientSecret != secret {
							c.OAuth2.Providers[i].ClientSecret = secret
							changed = true
						}
					}
				}
				if !found {
					return errors.New("OAuth secret references an unconfigured provider")
				}
			}
			if changed {
				if err = tx.Save(c); err != nil {
					return errors.New("OAuth provider secret configuration failed validation")
				}
			}
		}
		return nil
	})
}
