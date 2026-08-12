package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapabilityVectors(t *testing.T) {
	tests := []struct {
		secret string
		want   string
	}{
		{"000102030405060708090a0b0c0d0e0f", "MHLEY5PmW1GWqJkSrlmJpvJUiLhBH_QKy6yKg8a0JPk"},
		{"dd000102030405060708090a0b0c0d0e0f", "IpJrt3e7sKtzPyoXy6w-Zj6GGEvsvclN66JzQEfPYLA"},
	}
	for _, test := range tests {
		secret, err := DecodeSecret(test.secret)
		if err != nil {
			t.Fatal(err)
		}
		got := CapabilityString(DeriveCapability("proxy.example.com", secret))
		if got != test.want {
			t.Fatalf("got %q, want %q", got, test.want)
		}
	}
}

func TestHostnameValidation(t *testing.T) {
	for _, valid := range []string{
		"site.example",
		"xn--bcher-kva.example",
		"a.b.example",
		strings.Repeat("a", 63) + ".example",
	} {
		if err := ValidateHostname(valid); err != nil {
			t.Errorf("%q: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"localhost",
		"127.0.0.1",
		"[::1]",
		"HTTPS://site.example",
		"Site.example",
		"bücher.example",
		"site.example.",
		"site.example:443",
		"site/example",
		"site\\example",
		"site..example",
		"-site.example",
		"site-.example",
		"site_example.com",
		strings.Repeat("a", 64) + ".example",
		strings.Repeat("a.", 127) + "aa",
	} {
		if err := ValidateHostname(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestPlainSecretMayBeginWithEE(t *testing.T) {
	secret, err := DecodeSecret("ee0102030405060708090a0b0c0d0e0f")
	if err != nil || len(secret) != 16 || secret[0] != 0xee {
		t.Fatalf("valid plain secret was rejected: %v", err)
	}
}

func TestLoadAppliesDefaultsAndRelativePaths(t *testing.T) {
	directory := t.TempDir()
	public := filepath.Join(directory, "public")
	if err := os.Mkdir(public, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, "index.html"), []byte("site"), 0600); err != nil {
		t.Fatal(err)
	}
	profiles := `{"profiles":[{"name":"default","secret":"000102030405060708090a0b0c0d0e0f","backend":"127.0.0.1:2398"}]}`
	if err := os.WriteFile(filepath.Join(directory, "profiles.json"), []byte(profiles), 0600); err != nil {
		t.Fatal(err)
	}
	server := `{"public_hostname":"proxy.example.com","public_dir":"public","profiles_file":"profiles.json"}`
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(server), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PublicDir != public || len(loaded.Profiles) != 1 || loaded.Limits.MaxBodyBytes != 2*1024*1024 {
		t.Fatalf("unexpected loaded configuration: %#v", loaded)
	}
	profileLimits := loaded.Profiles[0].Limits
	if profileLimits.MaxSessions != loaded.Limits.MaxSessionsGlobal ||
		profileLimits.MaxStreams != loaded.Limits.MaxStreamsGlobal ||
		profileLimits.MaxBackendDialsInFlight != loaded.Limits.MaxBackendDialsInFlight ||
		profileLimits.NewSessionsPerMinute != loaded.Limits.NewSessionsPerMinute ||
		profileLimits.NewStreamsPerMinute != loaded.Limits.NewStreamsPerMinute {
		t.Fatalf("profile limits did not inherit global values: %#v", profileLimits)
	}
}

func TestProfileStreamDefaultsRespectProfileCeiling(t *testing.T) {
	global := Defaults().Limits
	resolved := (ProfileLimits{MaxStreams: 32}).WithDefaults(global)
	if resolved.MaxStreamsPerSession != 32 ||
		resolved.MaxBackendDialsInFlight != 32 ||
		resolved.MaxSessions != global.MaxSessionsGlobal ||
		resolved.NewStreamsPerMinute != global.NewStreamsPerMinute {
		t.Fatalf("unexpected resolved profile limits: %#v", resolved)
	}
}

func TestProfileLimitsCannotExceedGlobalCeilings(t *testing.T) {
	global := Defaults().Limits
	if err := validateProfileLimits(ProfileLimits{
		MaxStreams: global.MaxStreamsGlobal + 1,
	}, global); err == nil {
		t.Fatal("profile max_streams exceeded the global ceiling")
	}
	if err := validateProfileLimits(ProfileLimits{
		MaxStreams:           16,
		MaxStreamsPerSession: 17,
	}, global); err == nil {
		t.Fatal("per-session stream limit exceeded the profile stream ceiling")
	}
}

func TestLoadAcceptsSystemdCredentialReadPermissions(t *testing.T) {
	directory := t.TempDir()
	public := filepath.Join(directory, "public")
	if err := os.Mkdir(public, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, "index.html"), []byte("site"), 0600); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(directory, "credentials")
	if err := os.Mkdir(credentials, 0700); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(credentials, "profiles.json")
	content := `{"profiles":[{"name":"default","secret":"000102030405060708090a0b0c0d0e0f","backend":"127.0.0.1:2398"}]}`
	if err := os.WriteFile(profiles, []byte(content), 0444); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentials)
	server := `{"public_hostname":"proxy.example.com","public_dir":"public","profiles_file":"credentials/profiles.json"}`
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(server), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := Load(path); err == nil {
		t.Fatal("group/other-readable profiles file outside a credential directory was accepted")
	}
}
