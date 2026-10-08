package smoke

import (
	"cmp"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// FileName is the smoke file's name inside diadoche's state directory
// (<project>/.claude/diadoche).
const FileName = "smoke.yaml"

// PathIn returns the smoke file's path inside diadoche's state directory.
func PathIn(diadocheRoot string) string {
	return filepath.Join(diadocheRoot, FileName)
}

// Defaults for the keys a smoke file may omit.
const (
	DefaultUpTimeout       = 5 * time.Minute
	DefaultDownTimeout     = 2 * time.Minute
	DefaultWaitTimeout     = 60 * time.Second
	DefaultWaitInterval    = 1 * time.Second
	DefaultCmdCheckTimeout = 60 * time.Second
	DefaultURLCheckTimeout = 30 * time.Second
	DefaultCheckStatus     = 200
	DefaultBrowserTimeout  = 60 * time.Second
)

// Command is the up or down step: a shell command and its timeout.
type Command struct {
	Cmd     string
	Timeout time.Duration
	Line    int
}

// Wait is the readiness step: exactly one of URL or Cmd, polled every
// Interval until it is ready or Timeout has passed.
type Wait struct {
	URL      string
	Cmd      string
	Timeout  time.Duration
	Interval time.Duration
	Line     int
}

// Check is one item of checks: exactly one of Cmd or URL. Status is the
// expected HTTP status of a URL check (0 for a command check).
type Check struct {
	Cmd     string
	URL     string
	Status  int
	Timeout time.Duration
	Line    int
}

// Label describes the check for reports: "cmd: …" or "url: …".
func (c Check) Label() string {
	if c.URL != "" {
		return "url: " + c.URL
	}
	return "cmd: " + c.Cmd
}

// BrowserURL is one entry of browser.
type BrowserURL struct {
	URL  string
	Line int
}

// Config is a parsed and validated smoke file, defaults filled in. A nil
// step pointer or an empty list means the step is not configured.
type Config struct {
	Up      *Command
	Wait    *Wait
	Checks  []Check
	Browser []BrowserURL
	Down    *Command
}

// Load reads and parses the smoke file at path. A missing file yields an
// error for which errors.Is(err, fs.ErrNotExist) holds.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read smoke file: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates a smoke file. Errors are *Error values naming
// the line and the key.
func Parse(data []byte) (*Config, error) {
	root, err := parseYAML(data)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	for _, e := range root.entries {
		switch e.key {
		case "up":
			cfg.Up, err = decodeCommand(e, DefaultUpTimeout)
		case "down":
			cfg.Down, err = decodeCommand(e, DefaultDownTimeout)
		case "wait":
			cfg.Wait, err = decodeWait(e)
		case "checks":
			cfg.Checks, err = decodeChecks(e)
		case "browser":
			cfg.Browser, err = decodeBrowser(e)
		default:
			err = errAt(e.line, e.key, "unknown key (allowed: up, wait, checks, browser, down)")
		}
		if err != nil {
			return nil, err
		}
	}
	if cfg.Up == nil && cfg.Wait == nil && len(cfg.Checks) == 0 && len(cfg.Browser) == 0 && cfg.Down == nil {
		return nil, &Error{Msg: "no steps defined (need at least one of up, wait, checks, browser, down)"}
	}
	return cfg, nil
}

// scalar returns n's value, or an error when n is a map or a list.
func scalar(n *node, line int, key string) (string, error) {
	if n.kind != scalarNode {
		return "", errAt(line, key, "expected a scalar, got a %s", n.kind)
	}
	return n.value, nil
}

func nonEmpty(n *node, line int, key string) (string, error) {
	v, err := scalar(n, line, key)
	if err == nil && v == "" {
		err = errAt(line, key, "empty value")
	}
	return v, err
}

func duration(n *node, line int, key string) (time.Duration, error) {
	v, err := nonEmpty(n, line, key)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, errAt(line, key, "invalid duration %q: %v", v, err)
	}
	if d <= 0 {
		return 0, errAt(line, key, "duration %q must be positive", v)
	}
	return d, nil
}

func httpURL(n *node, line int, key string) (string, error) {
	v, err := nonEmpty(n, line, key)
	if err != nil {
		return "", err
	}
	u, perr := url.Parse(v)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errAt(line, key, "not an http(s) URL: %q", v)
	}
	return v, nil
}

func decodeCommand(e mapEntry, def time.Duration) (*Command, error) {
	c := &Command{Timeout: def, Line: e.line}
	switch e.value.kind {
	case scalarNode:
		if e.value.value == "" {
			return nil, errAt(e.line, e.key, "empty command")
		}
		c.Cmd = e.value.value
		return c, nil
	case listNode:
		return nil, errAt(e.line, e.key, "expected a command string or {cmd, timeout} map, got a list")
	}
	for _, f := range e.value.entries {
		key := e.key + "." + f.key
		var err error
		switch f.key {
		case "cmd":
			c.Cmd, err = nonEmpty(f.value, f.line, key)
		case "timeout":
			c.Timeout, err = duration(f.value, f.line, key)
		default:
			err = errAt(f.line, key, "unknown key (allowed: cmd, timeout)")
		}
		if err != nil {
			return nil, err
		}
	}
	if c.Cmd == "" {
		return nil, errAt(e.line, e.key+".cmd", "missing command")
	}
	return c, nil
}

func decodeWait(e mapEntry) (*Wait, error) {
	if e.value.kind != mapNode {
		return nil, errAt(e.line, e.key, "expected a map with url or cmd, got a %s", e.value.kind)
	}
	w := &Wait{Timeout: DefaultWaitTimeout, Interval: DefaultWaitInterval, Line: e.line}
	for _, f := range e.value.entries {
		key := "wait." + f.key
		var err error
		switch f.key {
		case "url":
			w.URL, err = httpURL(f.value, f.line, key)
		case "cmd":
			w.Cmd, err = nonEmpty(f.value, f.line, key)
		case "timeout":
			w.Timeout, err = duration(f.value, f.line, key)
		case "interval":
			w.Interval, err = duration(f.value, f.line, key)
		default:
			err = errAt(f.line, key, "unknown key (allowed: url, cmd, timeout, interval)")
		}
		if err != nil {
			return nil, err
		}
	}
	switch {
	case w.URL != "" && w.Cmd != "":
		return nil, errAt(e.line, "wait", "url and cmd are mutually exclusive")
	case w.URL == "" && w.Cmd == "":
		return nil, errAt(e.line, "wait", "one of url or cmd is required")
	}
	return w, nil
}

func decodeChecks(e mapEntry) ([]Check, error) {
	if e.value.kind != listNode {
		return nil, errAt(e.line, e.key, "expected a list of checks, got a %s", e.value.kind)
	}
	var out []Check
	for i, item := range e.value.items {
		path := fmt.Sprintf("checks[%d]", i+1)
		if item.kind != mapNode {
			return nil, errAt(item.line, path, "expected a map with cmd or url, got a %s", item.kind)
		}
		if len(item.entries) == 0 {
			return nil, errAt(item.line, path, "empty item, need cmd or url")
		}
		c := Check{Line: item.line}
		statusLine := 0
		for _, f := range item.entries {
			key := path + "." + f.key
			var err error
			switch f.key {
			case "cmd":
				c.Cmd, err = nonEmpty(f.value, f.line, key)
			case "url":
				c.URL, err = httpURL(f.value, f.line, key)
			case "timeout":
				c.Timeout, err = duration(f.value, f.line, key)
			case "status":
				statusLine = f.line
				c.Status, err = status(f.value, f.line, key)
			default:
				err = errAt(f.line, key, "unknown key (allowed: cmd, url, status, timeout)")
			}
			if err != nil {
				return nil, err
			}
		}
		switch {
		case c.Cmd != "" && c.URL != "":
			return nil, errAt(item.line, path, "cmd and url are mutually exclusive")
		case c.Cmd == "" && c.URL == "":
			return nil, errAt(item.line, path, "one of cmd or url is required")
		case c.Cmd != "" && statusLine > 0:
			return nil, errAt(statusLine, path+".status", "status applies only to a url check")
		}
		if c.URL != "" {
			c.Status = cmp.Or(c.Status, DefaultCheckStatus)
			c.Timeout = cmp.Or(c.Timeout, DefaultURLCheckTimeout)
		} else {
			c.Timeout = cmp.Or(c.Timeout, DefaultCmdCheckTimeout)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errAt(e.line, e.key, "empty list")
	}
	return out, nil
}

func status(n *node, line int, key string) (int, error) {
	v, err := nonEmpty(n, line, key)
	if err != nil {
		return 0, err
	}
	s, err := strconv.Atoi(v)
	if err != nil {
		return 0, errAt(line, key, "non-numeric status %q", v)
	}
	if s < 100 || s > 599 {
		return 0, errAt(line, key, "status %d out of range 100-599", s)
	}
	return s, nil
}

func decodeBrowser(e mapEntry) ([]BrowserURL, error) {
	if e.value.kind != listNode {
		return nil, errAt(e.line, e.key, "expected a list of URLs, got a %s", e.value.kind)
	}
	var out []BrowserURL
	for i, item := range e.value.items {
		path := fmt.Sprintf("browser[%d]", i+1)
		if item.kind != scalarNode {
			return nil, errAt(item.line, path, "a %s where a URL string is expected", item.kind)
		}
		u, err := httpURL(item, item.line, path)
		if err != nil {
			return nil, err
		}
		out = append(out, BrowserURL{URL: u, Line: item.line})
	}
	if len(out) == 0 {
		return nil, errAt(e.line, e.key, "empty list")
	}
	return out, nil
}
