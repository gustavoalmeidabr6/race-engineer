package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Skill is a markdown playbook the pi agent can lazy-load to scope its
// reasoning to one topic. The frontmatter (name, description, when_to_use,
// tools) is parsed once at load; the body is the LLM-facing instructions.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	WhenToUse   string   `json:"when_to_use"`
	Tools       []string `json:"tools,omitempty"` // optional curated subset
	Body        string   `json:"body"`
	Path        string   `json:"-"`
}

// SkillRegistry loads and serves *.md files from a directory. Re-reads on
// every list call (cheap; keeps hot-reload free during development).
type SkillRegistry struct {
	dir   string
	mu    sync.Mutex
	cache map[string]Skill
	stamp time.Time
}

// NewSkillRegistry returns a registry rooted at dir (e.g. workspace/pi_skills).
// A missing directory is fine — list/get return empty results until files appear.
func NewSkillRegistry(dir string) *SkillRegistry {
	return &SkillRegistry{dir: dir, cache: map[string]Skill{}}
}

// List returns skill metadata (without bodies) ordered by name.
// filter is a substring matched case-insensitively against name+description.
func (r *SkillRegistry) List(filter string) []Skill {
	r.refresh()
	r.mu.Lock()
	defer r.mu.Unlock()
	filter = strings.ToLower(strings.TrimSpace(filter))
	out := make([]Skill, 0, len(r.cache))
	for _, s := range r.cache {
		if filter != "" {
			hay := strings.ToLower(s.Name + " " + s.Description + " " + s.WhenToUse)
			if !strings.Contains(hay, filter) {
				continue
			}
		}
		s.Body = "" // don't ship body in list
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the full skill (with body), or an error if absent.
func (r *SkillRegistry) Get(name string) (Skill, error) {
	r.refresh()
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.cache[strings.ToLower(name)]
	if !ok {
		return Skill{}, fmt.Errorf("skill %q not found", name)
	}
	return s, nil
}

func (r *SkillRegistry) refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Stat(r.dir)
	if err != nil {
		// dir missing — clear cache and return.
		r.cache = map[string]Skill{}
		return
	}
	// Throttle disk re-scans to once per second.
	if time.Since(r.stamp) < time.Second && info.ModTime().Equal(r.stamp) {
		return
	}
	r.stamp = time.Now()
	cache := map[string]Skill{}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s, err := parseSkill(string(data))
		if err != nil {
			continue
		}
		s.Path = path
		if s.Name == "" {
			s.Name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		cache[strings.ToLower(s.Name)] = s
	}
	r.cache = cache
}

// parseSkill reads YAML-ish frontmatter delimited by --- and the markdown body
// after it. Extremely small parser — only string and string-list values, one
// per line. Keeps the package free of a YAML dependency.
func parseSkill(raw string) (Skill, error) {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Skill{Body: raw}, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Skill{Body: raw}, nil
	}
	s := Skill{}
	for _, line := range lines[1:end] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"`)
		switch key {
		case "name":
			s.Name = val
		case "description":
			s.Description = val
		case "when_to_use", "when-to-use":
			s.WhenToUse = val
		case "tools":
			parts := strings.Split(strings.Trim(val, "[]"), ",")
			for _, p := range parts {
				p = strings.TrimSpace(strings.Trim(p, `"`))
				if p != "" {
					s.Tools = append(s.Tools, p)
				}
			}
		}
	}
	body := strings.Join(lines[end+1:], "\n")
	s.Body = strings.TrimLeft(body, "\n")
	return s, nil
}
