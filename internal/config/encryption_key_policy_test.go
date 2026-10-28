package config

import (
	"regexp"
	"strings"
	"testing"

	"pad-analyzer/internal/platform/secret"
)

// R-security-7: the keystore key's rules. A database keystore seals every stored provider key and secret column
// whatever the mode, so the strength rule covers local mode with PAD_STORAGE=database as it covers the cloud; and the
// cloud's PAD_AUTH_SECRET fallback is deprecated with a date and an exact instruction rather than refused, because the
// deployments that rely on it today (the local compose stack runs the cloud image without PAD_ENCRYPTION_KEY) would
// otherwise stop booting on upgrade with every stored credential sealed under the auth secret.

// TestValidate_LocalDatabaseRefusesAWeakEncryptionKey is the item's first Done-when: PAD_MODE=local
// PAD_STORAGE=database PAD_ENCRYPTION_KEY=secret fails validation, naming the variable and never the value.
func TestValidate_LocalDatabaseRefusesAWeakEncryptionKey(t *testing.T) {
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_STORAGE", "database")
	t.Setenv("PAD_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("PAD_ENCRYPTION_KEY", "secret")
	cfg, err := LoadFromEnvRaw()
	if err != nil {
		t.Fatal(err)
	}
	err = Validate(cfg)
	if err == nil {
		t.Fatal("a weak PAD_ENCRYPTION_KEY with a database keystore in local mode passed validation")
	}
	if !strings.Contains(err.Error(), "PAD_ENCRYPTION_KEY") || strings.Contains(err.Error(), `"secret"`) {
		t.Errorf("error = %q, want it to name PAD_ENCRYPTION_KEY and not carry the key", err)
	}

	cfg.Auth.EncryptionKey = secret.New(strongEncKey)
	if err := Validate(cfg); err != nil {
		t.Errorf("a strong key in local mode with a database was refused: %v", err)
	}

	// Without a database there is no database keystore to seal with it: the desktop and the filesystem backend
	// keep provider keys in the OS keyring, so the key is not checked there.
	fs := Default()
	fs.Auth.EncryptionKey = secret.New("secret")
	if err := Validate(fs); err != nil {
		t.Errorf("a weak key without a database keystore was refused: %v", err)
	}
}

// TestValidate_AWeakKeyCanBeRotatedAway: the keys PAD_ENCRYPTION_KEY replaced only open what they sealed, so the
// strength rule is the current key's alone. A deployment on a weak key moves off it the documented way, a new strong
// PAD_ENCRYPTION_KEY with the old key in PAD_ENCRYPTION_KEY_PREVIOUS, in local mode with a database as in the cloud;
// the weak key cannot stay current.
func TestValidate_AWeakKeyCanBeRotatedAway(t *testing.T) {
	const weakOld = "secret"
	local := Default()
	local.Storage.Backend = StorageDatabase
	local.Storage.DatabaseURL = secret.New("postgres://localhost/db")
	for name, cfg := range map[string]*Config{"local with a database": local, "cloud": cloudCfg()} {
		cfg.Auth.EncryptionKey = secret.New(strongEncKey)
		cfg.Auth.EncryptionKeyPrevious = []secret.Value{secret.New(weakOld), secret.New("please-change-this-secret-in-production")}
		if err := Validate(cfg); err != nil {
			t.Errorf("%s: moving off a weak key (strong current, weak previous) was refused: %v", name, err)
		}
		cfg.Auth.EncryptionKey = secret.New(weakOld)
		cfg.Auth.EncryptionKeyPrevious = nil
		if err := Validate(cfg); err == nil {
			t.Errorf("%s: a weak current key was accepted", name)
		}
	}
}

// TestEncryptionKeyFallbackDeprecation is the item's second Done-when, decided as a dated deprecation: a cloud
// deployment without PAD_ENCRYPTION_KEY still validates (refusing it would stop it booting), and the boot check names
// the removal date, the exact variables to set and the command that confirms the result, never a key.
func TestEncryptionKeyFallbackDeprecation(t *testing.T) {
	cfg := cloudCfg()
	if err := Validate(cfg); err != nil {
		t.Fatalf("a cloud deployment on the auth-secret fallback was refused before the deprecation date: %v", err)
	}
	msg := EncryptionKeyFallbackDeprecation(cfg)
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(EncryptionKeyFallbackRemoval) {
		t.Fatalf("EncryptionKeyFallbackRemoval = %q, want a date", EncryptionKeyFallbackRemoval)
	}
	for _, want := range []string{
		EncryptionKeyFallbackRemoval,
		"PAD_ENCRYPTION_KEY=",
		"PAD_ENCRYPTION_KEY_PREVIOUS=",
		"PAD_AUTH_SECRET",
		"bakicli keys check",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the deprecation warning lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, cfg.Auth.Secret.Reveal()) {
		t.Error("the deprecation warning carries the auth secret")
	}

	cfg.Auth.EncryptionKey = secret.New(strongEncKey)
	if msg := EncryptionKeyFallbackDeprecation(cfg); msg != "" {
		t.Errorf("with PAD_ENCRYPTION_KEY set the warning is %q, want none", msg)
	}
	local := Default()
	local.Storage.Backend = StorageDatabase
	local.Auth.Secret = secret.New("a-sufficiently-long-random-secret-value-123456")
	if msg := EncryptionKeyFallbackDeprecation(local); msg != "" {
		t.Errorf("local mode got the cloud deprecation: %q", msg)
	}
}

// TestDeploy_EncryptionKeyIsRequired is the item's third Done-when: infra/main.bicep requires the encryptionKey
// parameter (no default, at least 32 characters), as docker-compose.prod.yml requires PAD_ENCRYPTION_KEY, and always
// passes it to the app; the deploy workflow refuses a deploy without the secret before it reaches Azure.
func TestDeploy_EncryptionKeyIsRequired(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	param := regexp.MustCompile(`(?m)^param encryptionKey string(.*)$`).FindStringSubmatch(bicep)
	if param == nil {
		t.Fatal("main.bicep declares no encryptionKey parameter")
	}
	if strings.Contains(param[1], "=") {
		t.Errorf("main.bicep's encryptionKey has a default (%q): it must be required", strings.TrimSpace(param[1]))
	}
	decl := bicep[:strings.Index(bicep, param[0])]
	decl = decl[strings.LastIndex(decl, "@description"):]
	if !strings.Contains(decl, "@secure()") || !strings.Contains(decl, "@minLength(32)") {
		t.Errorf("main.bicep's encryptionKey must be @secure() and @minLength(32):\n%s", decl)
	}
	for _, want := range []string{
		"{ name: 'encryption-key', value: encryptionKey }",
		"{ name: 'PAD_ENCRYPTION_KEY', secretRef: 'encryption-key' }",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks %q", want)
		}
	}
	if strings.Contains(bicep, "empty(encryptionKey)") {
		t.Error("main.bicep still treats encryptionKey as optional (empty(encryptionKey))")
	}

	prod := readDeployFile(t, "docker-compose.prod.yml")
	if !strings.Contains(prod, "PAD_ENCRYPTION_KEY=${PAD_ENCRYPTION_KEY:?") {
		t.Error("docker-compose.prod.yml no longer requires PAD_ENCRYPTION_KEY")
	}

	deploy := readDeployFile(t, ".github/workflows/deploy.yml")
	if !strings.Contains(deploy, `encryptionKey="${{ secrets.PAD_ENCRYPTION_KEY }}"`) {
		t.Error("deploy.yml does not pass the PAD_ENCRYPTION_KEY secret as encryptionKey")
	}
	if !strings.Contains(deploy, "PAD_ENCRYPTION_KEY: ${{ secrets.PAD_ENCRYPTION_KEY }}") || !strings.Contains(deploy, `[ -z "$PAD_ENCRYPTION_KEY" ]`) {
		t.Error("deploy.yml has no step refusing a deploy without the PAD_ENCRYPTION_KEY secret")
	}

	params := readDeployFile(t, "infra/main.parameters.json")
	if !regexp.MustCompile(`"encryptionKey":\s*\{\s*"value":\s*"<openssl rand -hex 32`).MatchString(params) {
		t.Error("main.parameters.json's encryptionKey is not the generate-it placeholder")
	}
}
