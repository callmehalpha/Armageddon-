package runtimes

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// phpProvider: Composer and plain PHP projects (plan M8.3). PHP itself
// comes from the server (there are no official relocatable PHP builds to
// install per workspace); a version the server cannot satisfy is reported,
// not installed. Composer is the server's, or composer.phar downloaded into
// ~/.armageddon/runtimes/composer and checked against its published
// SHA-256. Laravel is detected by artisan plus laravel/framework and is
// started with `php artisan serve`.
type phpProvider struct{}

func (phpProvider) Name() string { return "php" }

type composerJSON struct {
	Require    map[string]string `json:"require"`
	RequireDev map[string]string `json:"require-dev"`
	Config     struct {
		Platform map[string]string `json:"platform"`
	} `json:"config"`
	Scripts map[string]json.RawMessage `json:"scripts"`
}

func readComposerJSON(dir string) (*composerJSON, error) {
	b, err := os.ReadFile(filepath.Join(dir, "composer.json"))
	if err != nil {
		return nil, err
	}
	var cj composerJSON
	if err := json.Unmarshal(b, &cj); err != nil {
		return nil, fmt.Errorf("composer.json: %w", err)
	}
	return &cj, nil
}

func (phpProvider) Detect(dir string) (*Detection, error) {
	d := &Detection{}
	switch {
	case exists(dir, "composer.json"):
		cj, err := readComposerJSON(dir)
		if err != nil {
			return nil, err
		}
		d.Evidence = append(d.Evidence, "composer.json")
		d.PackageManager = "composer"
		if v := cj.Require["php"]; v != "" {
			d.Version, d.VersionFrom = v, "composer.json require.php"
		} else if v := cj.Config.Platform["php"]; v != "" {
			d.Version, d.VersionFrom = v, "composer.json config.platform.php"
		}
		_, laravel := cj.Require["laravel/framework"]
		if laravel && exists(dir, "artisan") {
			d.Framework = "laravel"
			d.Evidence = append(d.Evidence, "artisan")
		} else if _, ok := cj.Require["symfony/framework-bundle"]; ok {
			d.Framework = "symfony"
		}
	case exists(dir, "index.php"):
		d.Evidence = append(d.Evidence, "index.php")
	case exists(dir, "public/index.php"):
		d.Evidence = append(d.Evidence, "public/index.php")
	default:
		return nil, nil
	}
	return d, nil
}

func (phpProvider) Plan(ctx context.Context, d *Detection, h *Host) (*Plan, error) {
	p := &Plan{Detection: *d, Toolchain: Toolchain{Name: "php", Version: d.Version, Source: "missing"}, Port: 8000}
	if php, err := h.LookPath("php"); err == nil {
		out, err := h.Output(ctx, php, "-r", "echo PHP_VERSION;")
		if v, ok := ParseVersion(out); err == nil && ok {
			p.Toolchain = Toolchain{Name: "php", Version: v.String(), Source: "system", Path: php}
			if d.Version != "" {
				r, err := ParseComposerRange(d.Version)
				if err == nil && !r.Match(v) {
					p.Notes = append(p.Notes, fmt.Sprintf("%s asks for PHP %s but the server has %s; install a matching PHP on the server (per-workspace PHP installs are not supported in v0.1)", d.VersionFrom, d.Version, v))
				}
			}
		}
	}
	if p.Toolchain.Source == "missing" {
		p.Notes = append(p.Notes, "PHP is not installed on the server: install it there (for example `apt install php-cli php-mbstring php-xml php-curl php-zip php-sqlite3`)")
	}
	if d.PackageManager == "composer" {
		p.Install = [][]string{{"composer", "install", "--no-interaction", "--no-progress"}}
	}
	port := fmt.Sprint(p.Port)
	switch {
	case d.Framework == "laravel":
		p.Start = []string{"php", "artisan", "serve", "--host=127.0.0.1", "--port=" + port}
	case exists(d.Dir, "public/index.php"):
		p.Start = []string{"php", "-S", "127.0.0.1:" + port, "-t", "public"}
	default:
		p.Start = []string{"php", "-S", "127.0.0.1:" + port}
	}
	p.Env = []string{"PORT=" + port}
	return p, nil
}

func (phpProvider) Install(ctx context.Context, p *Plan, h *Host, log io.Writer) error {
	if p.Toolchain.Source == "missing" {
		return fmt.Errorf("%s", strings.Join(p.Notes, "; "))
	}
	fmt.Fprintf(log, "PHP %s (%s)\n", p.Toolchain.Version, p.Toolchain.Source)
	for _, n := range p.Notes {
		fmt.Fprintln(log, "note:", n)
	}
	if p.PackageManager == "composer" {
		if _, err := h.LookPath("composer"); err != nil {
			if err := installComposer(ctx, h, log); err != nil {
				return err
			}
		}
	}
	for _, step := range p.Install {
		if err := h.Run(ctx, p.Dir, log, step...); err != nil {
			return err
		}
	}
	if p.Framework == "laravel" {
		return laravelEnv(ctx, p, h, log)
	}
	return nil
}

// installComposer puts composer.phar in the workspace, checked against the
// SHA-256 published next to it.
func installComposer(ctx context.Context, h *Host, log io.Writer) error {
	url := h.composerURL()
	fmt.Fprintf(log, "Downloading Composer from %s…\n", url)
	sumFile, err := fetch(ctx, h, url+".sha256", 4096)
	if err != nil {
		return err
	}
	f := strings.Fields(string(sumFile))
	if len(f) == 0 || len(f[0]) != 64 {
		return fmt.Errorf("%s.sha256: no checksum", url)
	}
	phar, err := fetch(ctx, h, url, 64<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(phar)
	if got := hex.EncodeToString(sum[:]); got != strings.ToLower(f[0]) {
		return fmt.Errorf("composer.phar: checksum mismatch (got %s, want %s): refused", got, f[0])
	}
	dir := filepath.Join(h.Base(), "runtimes", "composer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, "composer.phar")
	if err := os.WriteFile(dest+".tmp", phar, 0o755); err != nil {
		return err
	}
	if err := os.Rename(dest+".tmp", dest); err != nil {
		return err
	}
	if err := h.link("composer", dest); err != nil {
		return err
	}
	return h.EnsureProfile()
}

// laravelEnv gives a fresh Laravel checkout its .env and application key,
// as `composer create-project` would have.
func laravelEnv(ctx context.Context, p *Plan, h *Host, log io.Writer) error {
	env := filepath.Join(p.Dir, ".env")
	if !exists(p.Dir, ".env") {
		if !exists(p.Dir, ".env.example") {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(p.Dir, ".env.example"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(env, b, 0o600); err != nil {
			return err
		}
		fmt.Fprintln(log, "Created .env from .env.example")
	}
	if envValue(env, "APP_KEY") == "" {
		if err := h.Run(ctx, p.Dir, log, "php", "artisan", "key:generate", "--no-interaction"); err != nil {
			return err
		}
	}
	// The default SQLite database (Laravel 11+): create and migrate it, as
	// `composer create-project` does. Other databases are migrated by the
	// user once they are up (for example after `armageddon compose up`).
	if conn := envValue(env, "DB_CONNECTION"); conn == "sqlite" && envValue(env, "DB_DATABASE") == "" {
		db := filepath.Join(p.Dir, "database", "database.sqlite")
		if !exists(p.Dir, "database/database.sqlite") && exists(p.Dir, "database") {
			if err := os.WriteFile(db, nil, 0o644); err != nil {
				return err
			}
			fmt.Fprintln(log, "Created database/database.sqlite")
			return h.Run(ctx, p.Dir, log, "php", "artisan", "migrate", "--force", "--no-interaction")
		}
	} else if conn != "" && conn != "sqlite" {
		fmt.Fprintf(log, "note: run `php artisan migrate` once the %s database is up\n", conn)
	}
	return nil
}

// envValue reads KEY from a .env file ("" if absent or empty).
func envValue(envFile, key string) string {
	f, err := os.Open(envFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), key+"="); ok {
			return strings.Trim(v, `"' `)
		}
	}
	return ""
}
