package config

import "sync/atomic"

// Holder keeps the active configuration and swaps it on reload. Readers call
// Current and never see a partially loaded configuration.
type Holder struct {
	path string
	cur  atomic.Pointer[Config]
}

// NewHolder loads the file at path. It fails if the file is invalid, since
// there is no previous configuration to fall back to.
func NewHolder(path string) (*Holder, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	h := &Holder{path: path}
	h.cur.Store(cfg)
	return h, nil
}

// Current returns the active configuration. Treat it as read-only.
func (h *Holder) Current() *Config {
	return h.cur.Load()
}

// Reload loads the file again. On error the previous configuration stays
// active and the error is returned for logging (design section 5).
func (h *Holder) Reload() (*Config, error) {
	cfg, err := Load(h.path)
	if err != nil {
		return h.cur.Load(), err
	}
	h.cur.Store(cfg)
	return cfg, nil
}

// Path is the file the configuration is loaded from.
func (h *Holder) Path() string { return h.path }

// ReloadWith validates the file again and, if check accepts the new
// configuration, makes it active. check rejects changes that need a
// restart.
func (h *Holder) ReloadWith(check func(old, new *Config) error) (*Config, error) {
	cfg, err := Load(h.path)
	if err != nil {
		return h.cur.Load(), err
	}
	if err := check(h.cur.Load(), cfg); err != nil {
		return h.cur.Load(), err
	}
	h.cur.Store(cfg)
	return cfg, nil
}
