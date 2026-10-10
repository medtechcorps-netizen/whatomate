package stagingkit

// Parse the real template with each production config reader, without opening
// a socket. A misspelled env key otherwise silently falls back to a live host.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/gmailrelay"
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/shridarpatil/whatomate/release/staging/graphstub"
)

func TestTemplateUsesRealConfigurationReaders(t *testing.T) {
	raw, err := os.ReadFile("app-spec.template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Services []struct {
			Name string
			Envs []struct{ Key, Value string }
		}
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"runtime_url": "postgresql://rereply_app:synthetic@staging.invalid:25060/rereply?sslmode=require",
		"redis_url":   "rediss://default:synthetic@staging.invalid:25061",
		"admin_email": "staging-admin@rereply.invalid", "reply_enabled": "false", "allowlist": "",
		"stub_app_id": "900000000000001", "stub_accounts": "[]",
	}
	for _, service := range spec.Services {
		t.Run(service.Name, func(t *testing.T) {
			for _, item := range service.Envs {
				value := item.Value
				if strings.HasPrefix(value, "@@") {
					name := strings.Trim(value, "@")
					var ok bool
					value, ok = values[name]
					if !ok {
						value = name + strings.Repeat("-synthetic", 6)
					}
				}
				t.Setenv(item.Key, value)
			}
			switch service.Name {
			case "omnitech-web":
				cfg, err := config.Load(filepath.Join("..", "..", "config.example.toml"))
				if err != nil {
					t.Fatal(err)
				}
				if cfg.App.Environment != "staging" || cfg.WhatsApp.BaseURL != "http://graph-stub" ||
					cfg.AI.QwenBaseURL != "http://127.0.0.1:9" || cfg.GoogleSearchConsole.AuthURL != "http://127.0.0.1:9" ||
					cfg.MetaMessenger.Enabled || cfg.MetaInstagram.Enabled || cfg.ThreadsManaged.Enabled ||
					cfg.MetaRegistry.Enabled || cfg.LegacyWhatsAppReply.Enabled {
					t.Fatal("web template is not isolated")
				}
			case "meta-relay":
				cfg, err := metarelay.LoadConfig()
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Environment != "staging" || cfg.FacebookGraphBaseURL != "http://graph-stub" ||
					cfg.InstagramGraphBaseURL != "http://graph-stub" || cfg.RegistryEnabled {
					t.Fatal("Meta relay template is not isolated")
				}
			case "gmail-relay":
				cfg, err := gmailrelay.LoadConfig()
				if err != nil {
					t.Fatal(err)
				}
				for _, endpoint := range []string{cfg.GoogleAuthURL, cfg.GoogleTokenURL, cfg.GmailAPIBaseURL} {
					if endpoint != "http://127.0.0.1:9" {
						t.Fatal("Gmail relay endpoint is live")
					}
				}
			case "graph-stub":
				cfg, err := graphstub.ConfigFromEnv(os.Getenv)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Environment != "staging" || cfg.CallbackOrigin != "http://omnitech-web" {
					t.Fatal("stub template is not isolated")
				}
			default:
				t.Fatal("unreviewed service")
			}
		})
	}
}
