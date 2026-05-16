// configtool is the CLI companion to ~/.race-engineer/config.json.
//
// Subcommands:
//   import-env [--env-file .env] [--dry-run] [--force]
//                       Read .env, write every value into the JSON config
//                       (preserving keys already present unless --force).
//                       Renames the source to .env.migrated-<unix>.
//   show [KEY] [--reveal-secrets] [--json]
//                       Pretty-print effective config with provenance per
//                       key (file / env / default).  Restrict to one key
//                       by passing KEY.
//   set KEY VALUE       Persist a single key.  Auto-detects type from the
//                       schema (int/bool/string/hex/float).
//   unset KEY           Remove a key (revert to env / default).
//   validate            Schema-check every key.  Fails non-zero on missing
//                       Required keys, unknown keys, or type mismatches.
//
// All commands operate on the file resolved by config.FilePath() —
// ~/.race-engineer/config.json by default.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "import-env":
		err = cmdImportEnv(args)
	case "show":
		err = cmdShow(args)
	case "set":
		err = cmdSet(args)
	case "unset":
		err = cmdUnset(args)
	case "validate":
		err = cmdValidate(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: configtool <command> [args]

commands:
  import-env [--env-file FILE] [--dry-run] [--force]
  show [KEY] [--reveal-secrets] [--json]
  set KEY VALUE
  unset KEY
  validate

The config file lives at ~/.race-engineer/config.json by default.`)
}

// --- import-env ---

func cmdImportEnv(args []string) error {
	fs := flag.NewFlagSet("import-env", flag.ExitOnError)
	envFile := fs.String("env-file", ".env", "Path to .env to import")
	dryRun := fs.Bool("dry-run", false, "Print what would be written without saving")
	force := fs.Bool("force", false, "Overwrite keys that already exist in the JSON config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pairs, err := parseEnvFile(*envFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", *envFile, err)
	}
	if len(pairs) == 0 {
		return fmt.Errorf("no key=value pairs found in %s", *envFile)
	}

	current, err := config.LoadFile()
	if err != nil {
		return err
	}

	patch := map[string]any{}
	skipped := []string{}
	unknown := []string{}
	for _, kv := range pairs {
		field := config.FieldFor(kv.key)
		if field == nil {
			unknown = append(unknown, kv.key)
			continue
		}
		if !*force {
			if _, exists := current[kv.key]; exists {
				skipped = append(skipped, kv.key)
				continue
			}
		}
		coerced, err := coerce(field, kv.value)
		if err != nil {
			return fmt.Errorf("%s=%s: %w", kv.key, kv.value, err)
		}
		patch[kv.key] = coerced
	}

	if *dryRun {
		fmt.Println("DRY RUN — no changes written")
		for k, v := range patch {
			fmt.Printf("  + %s = %v\n", k, v)
		}
		for _, k := range skipped {
			fmt.Printf("  = %s (kept existing)\n", k)
		}
		for _, k := range unknown {
			fmt.Printf("  ? %s (unknown — not in schema)\n", k)
		}
		return nil
	}

	if len(patch) > 0 {
		if err := config.SaveFile(patch); err != nil {
			return err
		}
	}

	// Rename the .env so the next boot doesn't reimport it unintentionally.
	if absEnv, err := filepath.Abs(*envFile); err == nil {
		newName := absEnv + ".migrated-" + strconv.FormatInt(time.Now().Unix(), 10)
		_ = os.Rename(absEnv, newName)
		fmt.Printf("renamed %s → %s\n", absEnv, newName)
	}

	fmt.Printf("imported %d keys", len(patch))
	if len(skipped) > 0 {
		fmt.Printf(", skipped %d existing", len(skipped))
	}
	if len(unknown) > 0 {
		fmt.Printf(", ignored %d unknown (%s)", len(unknown), strings.Join(unknown, ","))
	}
	fmt.Println()
	return nil
}

type envPair struct {
	key   string
	value string
}

// parseEnvFile reads a .env file with KEY=VALUE lines. Comments (#) and
// blank lines are ignored.  Surrounding quotes (single or double) are
// stripped from the value.  Does not handle escape sequences — the
// configtool's brief is "best-effort one-shot migration", not a full
// dotenv parser.
func parseEnvFile(path string) ([]envPair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var out []envPair
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Allow `export KEY=VALUE` for shell-style .env files.
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		// Strip inline trailing comment introduced by `KEY=value # note`.
		if hash := strings.Index(v, " #"); hash >= 0 {
			v = strings.TrimSpace(v[:hash])
		}
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		out = append(out, envPair{key: k, value: v})
	}
	return out, scanner.Err()
}

// --- show ---

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	reveal := fs.Bool("reveal-secrets", false, "Print secrets in cleartext")
	asJSON := fs.Bool("json", false, "Emit JSON instead of human-readable text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	file, err := config.LoadFile()
	if err != nil {
		return err
	}

	type row struct {
		Key    string `json:"key"`
		Value  any    `json:"value"`
		Source string `json:"source"` // file | env | default
		Group  string `json:"group"`
		Help   string `json:"help,omitempty"`
		Secret bool   `json:"secret,omitempty"`
	}
	var rows []row

	emitKey := func(key string) bool {
		if len(rest) == 0 {
			return true
		}
		for _, k := range rest {
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}

	for _, f := range config.Schema {
		if !emitKey(f.Key) {
			continue
		}
		r := row{Key: f.Key, Group: f.Group, Help: f.Help, Secret: f.Secret}
		if v, ok := file[f.Key]; ok && v != nil && v != "" {
			r.Value = v
			r.Source = "file"
		} else if v := os.Getenv(f.Key); v != "" {
			r.Value = v
			r.Source = "env"
		} else {
			r.Value = f.Default
			r.Source = "default"
		}
		if f.Secret && !*reveal {
			if s, ok := r.Value.(string); ok && s != "" {
				r.Value = mask(s)
			}
		}
		rows = append(rows, r)
	}

	if *asJSON {
		out, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	// Group by section for readability.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Group != rows[j].Group {
			return rows[i].Group < rows[j].Group
		}
		return rows[i].Key < rows[j].Key
	})
	currentGroup := ""
	for _, r := range rows {
		if r.Group != currentGroup {
			fmt.Printf("\n[%s]\n", r.Group)
			currentGroup = r.Group
		}
		fmt.Printf("  %-38s = %-30v  (%s)\n", r.Key, r.Value, r.Source)
	}
	path, _ := config.FilePath()
	fmt.Printf("\nconfig file: %s\n", path)
	return nil
}

func mask(s string) string {
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

// --- set ---

func cmdSet(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: configtool set KEY VALUE")
	}
	key := args[0]
	value := strings.Join(args[1:], " ")
	field := config.FieldFor(key)
	if field == nil {
		return fmt.Errorf("unknown key: %s (not declared in schema)", key)
	}
	coerced, err := coerce(field, value)
	if err != nil {
		return err
	}
	if err := config.SaveFile(map[string]any{key: coerced}); err != nil {
		return err
	}
	fmt.Printf("set %s = %v\n", key, coerced)
	return nil
}

// --- unset ---

func cmdUnset(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: configtool unset KEY")
	}
	key := args[0]
	if config.FieldFor(key) == nil {
		return fmt.Errorf("unknown key: %s", key)
	}
	if err := config.SaveFile(map[string]any{key: nil}); err != nil {
		return err
	}
	fmt.Printf("unset %s (reverts to env / default)\n", key)
	return nil
}

// --- validate ---

func cmdValidate(args []string) error {
	file, err := config.LoadFile()
	if err != nil {
		return err
	}

	var missing, badType, unknown []string

	// Required check.  When the chosen LLM provider has a registered key,
	// that key is upgraded to Required for this run.
	required := map[string]bool{}
	for _, f := range config.Schema {
		if f.Required {
			required[f.Key] = true
		}
	}
	provider := ""
	if v, ok := file["LLM_PROVIDER"]; ok {
		provider, _ = v.(string)
	} else {
		provider = os.Getenv("LLM_PROVIDER")
	}
	if k := config.SchemaProviderKey(strings.ToLower(provider)); k != "" {
		required[k] = true
	}

	for key := range required {
		v, ok := file[key]
		if (!ok || v == nil || v == "") && os.Getenv(key) == "" {
			missing = append(missing, key)
		}
	}

	// Type check for every known key present in the file.
	for k, v := range file {
		field := config.FieldFor(k)
		if field == nil {
			unknown = append(unknown, k)
			continue
		}
		if _, err := coerce(field, fmt.Sprint(v)); err != nil {
			badType = append(badType, fmt.Sprintf("%s (%v)", k, err))
		}
	}

	if len(missing)+len(badType)+len(unknown) == 0 {
		fmt.Println("OK — schema valid")
		return nil
	}
	sort.Strings(missing)
	sort.Strings(badType)
	sort.Strings(unknown)
	if len(missing) > 0 {
		fmt.Fprintln(os.Stderr, "missing required:")
		for _, k := range missing {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}
	if len(badType) > 0 {
		fmt.Fprintln(os.Stderr, "type errors:")
		for _, k := range badType {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintln(os.Stderr, "unknown keys (not in schema):")
		for _, k := range unknown {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}
	return errors.New("validation failed")
}

// --- helpers ---

// coerce parses a string-form value into the schema-declared type. Returns
// (coerced, nil) on success; the coerced value uses Go-native types so JSON
// encoding produces clean output (no quoted ints).
func coerce(f *config.Field, raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch f.Kind {
	case config.KindString:
		return raw, nil
	case config.KindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("expected int: %w", err)
		}
		return n, nil
	case config.KindFloat:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("expected float: %w", err)
		}
		return n, nil
	case config.KindBool:
		switch strings.ToLower(raw) {
		case "true", "1", "yes", "y":
			return true, nil
		case "false", "0", "no", "n":
			return false, nil
		}
		return nil, fmt.Errorf("expected bool, got %q", raw)
	case config.KindHex:
		n, err := strconv.ParseUint(raw, 0, 32)
		if err != nil {
			return nil, fmt.Errorf("expected hex (0x...): %w", err)
		}
		return "0x" + strings.ToUpper(strconv.FormatUint(n, 16)), nil
	}
	return raw, nil
}

var _ = fs.ErrNotExist // silence unused import when older toolchains complain
